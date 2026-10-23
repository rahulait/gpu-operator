/**
# Copyright (c) NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
**/

package controllers

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	gpuv1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1"
	gpuv1alpha1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1alpha1"
	"github.com/NVIDIA/gpu-operator/internal/consts"
	"github.com/NVIDIA/gpu-operator/internal/render"
)

const sharedServiceLabel = "nvidia.com/shared-service"

var sharedServiceNames = []string{"nvidia-dcgm", "nvidia-dcgm-exporter"}

// SharedServices is used by both stack controllers so templates, configuration
// decisions, and lifecycle rules cannot drift between reconciliation paths.
type SharedServices struct {
	Client      client.Client
	Reader      client.Reader
	Scheme      *runtime.Scheme
	Namespace   string
	ManifestDir string
}

type sharedServiceData struct {
	Namespace                    string
	ServiceType                  corev1.ServiceType
	ServiceInternalTrafficPolicy corev1.ServiceInternalTrafficPolicy
}

func newSharedServices(c client.Client, scheme *runtime.Scheme, namespace string) *SharedServices {
	return &SharedServices{Client: c, Reader: c, Scheme: scheme, Namespace: namespace, ManifestDir: "/opt/gpu-operator/manifests/shared"}
}

func (s *SharedServices) reader() client.Reader {
	if s.Reader != nil {
		return s.Reader
	}
	return s.Client
}

func (s *SharedServices) reconcile(ctx context.Context) error {
	var errs []error
	for _, name := range sharedServiceNames {
		if err := s.SyncSharedService(ctx, name); err != nil {
			errs = append(errs, fmt.Errorf("shared Service %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func normalizedExporterService(namespace string, spec *gpuv1.DCGMExporterServiceConfig) sharedServiceData {
	data := sharedServiceData{Namespace: namespace, ServiceType: corev1.ServiceTypeClusterIP, ServiceInternalTrafficPolicy: corev1.ServiceInternalTrafficPolicyCluster}
	if spec != nil {
		if spec.Type != "" {
			data.ServiceType = spec.Type
		}
		if spec.InternalTrafficPolicy != nil {
			data.ServiceInternalTrafficPolicy = *spec.InternalTrafficPolicy
		}
	}
	return data
}

func (s *SharedServices) desiredState(ctx context.Context, name string) (sharedServiceData, []client.Object, error) {
	data := normalizedExporterService(s.Namespace, nil)
	if name != "nvidia-dcgm" && name != "nvidia-dcgm-exporter" {
		return data, nil, fmt.Errorf("unsupported shared Service %q", name)
	}
	cp, gc, err := resolveActiveConfig(ctx, s.reader())
	if err != nil {
		return data, nil, err
	}
	var owners []client.Object
	var configs []sharedServiceData
	if name == "nvidia-dcgm" {
		data.ServiceInternalTrafficPolicy = corev1.ServiceInternalTrafficPolicyLocal
		if cp != nil && cp.DeletionTimestamp.IsZero() && cp.Spec.DCGM.IsEnabled() {
			owners = append(owners, cp)
		}
		// Standalone DCGM defaults to disabled in GPUCluster.
		if gc != nil && gc.DeletionTimestamp.IsZero() && gc.Spec.DCGM != nil && gc.Spec.DCGM.Enabled != nil && *gc.Spec.DCGM.Enabled {
			owners = append(owners, gc)
		}
	} else {
		if cp != nil && cp.DeletionTimestamp.IsZero() && cp.Spec.DCGMExporter.IsEnabled() {
			owners = append(owners, cp)
			configs = append(configs, normalizedExporterService(s.Namespace, cp.Spec.DCGMExporter.ServiceSpec))
		}
		if gc != nil && gc.DeletionTimestamp.IsZero() && gc.Spec.DCGMExporter != nil && gc.Spec.DCGMExporter.IsEnabled() {
			owners = append(owners, gc)
			configs = append(configs, normalizedExporterService(s.Namespace, gc.Spec.DCGMExporter.ServiceSpec))
		}
		if len(configs) > 1 && configs[0] != configs[1] {
			return data, nil, fmt.Errorf("conflicting exporter Service settings in ClusterPolicy and GPUCluster")
		}
		if len(configs) > 0 {
			data = configs[0]
		}
	}
	return data, owners, nil
}

func sharedServiceRetry(err error) bool {
	return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err)
}

// SyncSharedService recomputes state from both CRs on every retry. Fresh API reads
// prevent a conflict retry from repeatedly merging into a stale cached Service.
func (s *SharedServices) SyncSharedService(ctx context.Context, name string) error {
	return retry.OnError(retry.DefaultRetry, sharedServiceRetry, func() error {
		data, owners, err := s.desiredState(ctx, name)
		if err != nil {
			return err
		}
		if len(owners) == 0 {
			return s.ReleaseSharedService(ctx, name)
		}
		component := "state-dcgm"
		if name == "nvidia-dcgm-exporter" {
			component = "state-dcgm-exporter"
		}
		objects, err := render.NewRenderer([]string{filepath.Join(s.ManifestDir, component, "service.yaml")}).RenderObjects(&render.TemplatingData{Data: data})
		if err != nil {
			return err
		}
		if len(objects) != 1 || objects[0].GetKind() != "Service" {
			return fmt.Errorf("expected one shared Service template")
		}
		desired := &corev1.Service{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(objects[0].Object, desired); err != nil {
			return err
		}
		current := &corev1.Service{}
		err = s.reader().Get(ctx, client.ObjectKeyFromObject(desired), current)
		if apierrors.IsNotFound(err) {
			desired.Labels[sharedServiceLabel] = "true"
			if err := s.setOwners(desired, owners); err != nil {
				return err
			}
			if err := s.Client.Create(ctx, desired); err != nil {
				return err
			}
			return s.retireLegacyService(ctx, name)
		}
		if err != nil {
			return err
		}
		if !managedSharedService(current) {
			return fmt.Errorf("refusing to adopt unmanaged Service %s", name)
		}
		if !current.DeletionTimestamp.IsZero() {
			return fmt.Errorf("Service %s is terminating", name)
		}
		updated := current.DeepCopy()
		if updated.Labels == nil {
			updated.Labels = map[string]string{}
		}
		for key, value := range desired.Labels {
			updated.Labels[key] = value
		}
		updated.Labels[sharedServiceLabel] = "true"
		delete(updated.Labels, consts.StateLabel)
		if len(desired.Annotations) > 0 && updated.Annotations == nil {
			updated.Annotations = map[string]string{}
		}
		for key, value := range desired.Annotations {
			updated.Annotations[key] = value
		}
		if err := s.setOwners(updated, owners); err != nil {
			return err
		}
		updated.Spec.Selector = desired.Spec.Selector
		updated.Spec.Type = desired.Spec.Type
		updated.Spec.InternalTrafficPolicy = desired.Spec.InternalTrafficPolicy
		ports := desired.Spec.Ports
		if desired.Spec.Type == corev1.ServiceTypeNodePort || desired.Spec.Type == corev1.ServiceTypeLoadBalancer {
			for i := range ports {
				for _, port := range current.Spec.Ports {
					if port.Name == ports[i].Name && port.Protocol == ports[i].Protocol {
						ports[i].NodePort = port.NodePort
					}
				}
			}
		}
		updated.Spec.Ports = ports
		if desired.Spec.Type != corev1.ServiceTypeLoadBalancer {
			updated.Spec.HealthCheckNodePort = 0
			updated.Spec.AllocateLoadBalancerNodePorts = nil
		}
		if desired.Spec.Type == corev1.ServiceTypeClusterIP {
			updated.Spec.ExternalTrafficPolicy = ""
		}
		// Assigned IPs, IP families, unrelated metadata and Service fields are
		// retained by mutating the fetched object rather than replacing it.
		if !equality.Semantic.DeepEqual(current, updated) {
			if err := s.Client.Update(ctx, updated); err != nil {
				return err
			}
		}
		return s.retireLegacyService(ctx, name)
	})
}

// ReleaseSharedService rechecks both CRs before deleting. A stack being disabled
// is not permission to delete an endpoint that another stack still needs.
func (s *SharedServices) ReleaseSharedService(ctx context.Context, name string) error {
	return retry.OnError(retry.DefaultRetry, sharedServiceRetry, func() error {
		_, owners, err := s.desiredState(ctx, name)
		if err != nil || len(owners) != 0 {
			return err
		}
		svc := &corev1.Service{}
		if err := s.reader().Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: name}, svc); err != nil {
			if apierrors.IsNotFound(err) {
				return s.retireLegacyService(ctx, name)
			}
			return err
		}
		if !managedSharedService(svc) {
			return nil
		}
		if !svc.DeletionTimestamp.IsZero() {
			return fmt.Errorf("Service %s is terminating", name)
		}
		if err := client.IgnoreNotFound(s.Client.Delete(ctx, svc, client.Preconditions{UID: &svc.UID, ResourceVersion: &svc.ResourceVersion})); err != nil {
			return err
		}
		return s.retireLegacyService(ctx, name)
	})
}

func stackServiceOwner(ref metav1.OwnerReference) bool {
	return (ref.APIVersion == gpuv1.SchemeGroupVersion.String() && ref.Kind == "ClusterPolicy") ||
		(ref.APIVersion == gpuv1alpha1.SchemeGroupVersion.String() && ref.Kind == "GPUCluster")
}

func managedSharedService(svc *corev1.Service) bool {
	if svc.Labels[sharedServiceLabel] == "true" {
		return true
	}
	for _, ref := range svc.OwnerReferences {
		if stackServiceOwner(ref) {
			return true
		}
	}
	return false
}

func (s *SharedServices) setOwners(svc *corev1.Service, owners []client.Object) error {
	refs := make([]metav1.OwnerReference, 0, len(svc.OwnerReferences)+len(owners))
	for _, ref := range svc.OwnerReferences {
		if !stackServiceOwner(ref) {
			if ptr.Deref(ref.Controller, false) {
				return fmt.Errorf("Service %s has another controller owner", svc.Name)
			}
			refs = append(refs, ref)
		}
	}
	for _, owner := range owners {
		gvk, err := apiutil.GVKForObject(owner, s.Scheme)
		if err != nil {
			return err
		}
		refs = append(refs, metav1.OwnerReference{APIVersion: gvk.GroupVersion().String(), Kind: gvk.Kind, Name: owner.GetName(), UID: owner.GetUID()})
	}
	svc.OwnerReferences = refs
	return nil
}

// Retire only operator-owned aliases after the common endpoint is available.
func (s *SharedServices) retireLegacyService(ctx context.Context, name string) error {
	legacy := &corev1.Service{}
	if err := s.reader().Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: name + "-dra"}, legacy); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !managedSharedService(legacy) {
		return nil
	}
	if !legacy.DeletionTimestamp.IsZero() {
		return nil
	}
	return client.IgnoreNotFound(s.Client.Delete(ctx, legacy, client.Preconditions{UID: &legacy.UID, ResourceVersion: &legacy.ResourceVersion}))
}
