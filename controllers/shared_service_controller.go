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
	"fmt"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	gpuv1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1"
	gpuv1alpha1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1alpha1"
	"github.com/NVIDIA/gpu-operator/internal/consts"
	"github.com/NVIDIA/gpu-operator/internal/render"
)

const sharedServiceLabel = "nvidia.com/shared-service"

// SharedServiceReconciler keeps consumer endpoints independent of the allocation stack.
// Workloads and their supporting resources remain owned by the stack controllers.
type SharedServiceReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	Namespace   string
	ManifestDir string
}

type sharedServiceData struct {
	Namespace                    string
	ServiceType                  corev1.ServiceType
	ServiceInternalTrafficPolicy corev1.ServiceInternalTrafficPolicy
}

type sharedServiceState struct {
	name   string
	owners []client.Object
	data   sharedServiceData
}

// Existing operator RBAC already grants these permissions.
//+kubebuilder:rbac:groups=nvidia.com,resources=clusterpolicies;gpuclusters,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete

func (r *SharedServiceReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	cp, gc, err := resolveActiveConfig(ctx, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	states := []sharedServiceState{
		{name: "nvidia-dcgm", data: sharedServiceData{Namespace: r.Namespace, ServiceType: corev1.ServiceTypeClusterIP, ServiceInternalTrafficPolicy: corev1.ServiceInternalTrafficPolicyLocal}},
		{name: "nvidia-dcgm-exporter", data: sharedServiceData{Namespace: r.Namespace, ServiceType: corev1.ServiceTypeClusterIP, ServiceInternalTrafficPolicy: corev1.ServiceInternalTrafficPolicyCluster}},
	}
	var exporterSettings []sharedServiceData
	if cp != nil && cp.DeletionTimestamp.IsZero() {
		if cp.Spec.DCGM.IsEnabled() {
			states[0].owners = append(states[0].owners, cp)
		}
		if cp.Spec.DCGMExporter.IsEnabled() {
			states[1].owners = append(states[1].owners, cp)
			exporterSettings = append(exporterSettings, normalizedExporterService(r.Namespace, cp.Spec.DCGMExporter.ServiceSpec))
		}
	}
	if gc != nil && gc.DeletionTimestamp.IsZero() {
		// GPUCluster defaults standalone DCGM to disabled, unlike DCGMSpec.IsEnabled.
		if gc.Spec.DCGM != nil && gc.Spec.DCGM.Enabled != nil && *gc.Spec.DCGM.Enabled {
			states[0].owners = append(states[0].owners, gc)
		}
		if gc.Spec.DCGMExporter != nil && gc.Spec.DCGMExporter.IsEnabled() {
			states[1].owners = append(states[1].owners, gc)
			exporterSettings = append(exporterSettings, normalizedExporterService(r.Namespace, gc.Spec.DCGMExporter.ServiceSpec))
		}
	}
	// Reconcile DCGM even if the independently configured exporter is in conflict.
	var reconcileErr error
	for _, state := range states {
		if state.name == "nvidia-dcgm-exporter" && len(exporterSettings) > 0 {
			state.data = exporterSettings[0]
			if len(exporterSettings) > 1 && exporterSettings[0] != exporterSettings[1] {
				reconcileErr = fmt.Errorf("conflicting DCGM Exporter Service settings in ClusterPolicy and GPUCluster")
				continue
			}
		}
		if err := r.syncService(ctx, state); err != nil {
			reconcileErr = fmt.Errorf("failed to reconcile shared Service %s: %w", state.name, err)
		}
	}
	// Periodic recovery also covers ownership handoff and desired-state changes
	// between reading CRs and writing Services, which cannot be made atomic.
	return ctrl.Result{RequeueAfter: time.Minute}, reconcileErr
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

func stackServiceOwner(ref metav1.OwnerReference) bool {
	return (ref.APIVersion == gpuv1.SchemeGroupVersion.String() && ref.Kind == "ClusterPolicy") ||
		(ref.APIVersion == gpuv1alpha1.SchemeGroupVersion.String() && ref.Kind == "GPUCluster")
}

func managedDCGMService(svc *corev1.Service) bool {
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

func (r *SharedServiceReconciler) deleteService(ctx context.Context, name string) error {
	svc := &corev1.Service{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: name}, svc); err != nil {
		return client.IgnoreNotFound(err)
	}
	// Never remove a same-named Service not managed by GPU Operator.
	if !managedDCGMService(svc) {
		return nil
	}
	if !svc.DeletionTimestamp.IsZero() {
		return fmt.Errorf("Service %s is still terminating", name)
	}
	return client.IgnoreNotFound(r.Delete(ctx, svc, client.Preconditions{UID: &svc.UID, ResourceVersion: &svc.ResourceVersion}))
}

func (r *SharedServiceReconciler) syncService(ctx context.Context, state sharedServiceState) error {
	if len(state.owners) == 0 {
		if err := r.deleteService(ctx, state.name); err != nil {
			return err
		}
		return r.deleteService(ctx, state.name+"-dra")
	}
	component := "state-dcgm"
	if state.name == "nvidia-dcgm-exporter" {
		component = "state-dcgm-exporter"
	}
	objects, err := render.NewRenderer([]string{filepath.Join(r.ManifestDir, component, "service.yaml")}).RenderObjects(&render.TemplatingData{Data: state.data})
	if err != nil {
		return err
	}
	if len(objects) != 1 || objects[0].GetKind() != "Service" {
		return fmt.Errorf("expected one Service in shared template")
	}
	desired := &corev1.Service{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(objects[0].Object, desired); err != nil {
		return err
	}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: state.name, Namespace: r.Namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		if svc.ResourceVersion != "" && !managedDCGMService(svc) {
			return fmt.Errorf("refusing to adopt unmanaged Service %s", svc.Name)
		}
		if !svc.DeletionTimestamp.IsZero() {
			return fmt.Errorf("Service %s is still terminating", svc.Name)
		}
		if svc.Labels == nil {
			svc.Labels = map[string]string{}
		}
		for key, value := range desired.Labels {
			svc.Labels[key] = value
		}
		svc.Labels[sharedServiceLabel] = "true"
		// Stack state cleanup must never include shared Services.
		delete(svc.Labels, consts.StateLabel)
		if svc.Annotations == nil {
			svc.Annotations = map[string]string{}
		}
		for key, value := range desired.Annotations {
			svc.Annotations[key] = value
		}
		refs := svc.OwnerReferences[:0]
		for _, ref := range svc.OwnerReferences {
			if !stackServiceOwner(ref) {
				if ptr.Deref(ref.Controller, false) {
					return fmt.Errorf("Service %s has another controller owner", svc.Name)
				}
				refs = append(refs, ref)
			}
		}
		svc.OwnerReferences = refs
		for _, owner := range state.owners {
			if err := controllerutil.SetOwnerReference(owner, svc, r.Scheme); err != nil {
				return err
			}
		}
		svc.Spec.Selector = desired.Spec.Selector
		svc.Spec.Type = desired.Spec.Type
		svc.Spec.InternalTrafficPolicy = desired.Spec.InternalTrafficPolicy
		ports := desired.Spec.Ports
		if desired.Spec.Type == corev1.ServiceTypeNodePort || desired.Spec.Type == corev1.ServiceTypeLoadBalancer {
			for i := range ports {
				for _, existing := range svc.Spec.Ports {
					if existing.Name == ports[i].Name && existing.Protocol == ports[i].Protocol {
						ports[i].NodePort = existing.NodePort
					}
				}
			}
		}
		svc.Spec.Ports = ports
		if desired.Spec.Type != corev1.ServiceTypeLoadBalancer {
			svc.Spec.HealthCheckNodePort = 0
			svc.Spec.AllocateLoadBalancerNodePorts = nil
		}
		if desired.Spec.Type == corev1.ServiceTypeClusterIP {
			svc.Spec.ExternalTrafficPolicy = ""
		}
		return nil
	})
	if err != nil {
		return err
	}
	// The common endpoint replaces operator-owned DRA aliases. Monitoring must
	// not discover the same pods through both Service identities.
	return r.deleteService(ctx, state.name+"-dra")
}

func (r *SharedServiceReconciler) SetupWithManager(_ context.Context, mgr ctrl.Manager) error {
	if r.ManifestDir == "" {
		r.ManifestDir = "/opt/gpu-operator/manifests/shared"
	}
	c, err := controller.New("shared-service-controller", mgr, controller.Options{Reconciler: r, MaxConcurrentReconciles: 1})
	if err != nil {
		return err
	}
	requests := func() []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: r.Namespace, Name: "shared-services"}}}
	}
	// Watch CR metadata updates too: deletionTimestamp can change without generation.
	if err := c.Watch(source.Kind(mgr.GetCache(), &gpuv1.ClusterPolicy{}, handler.TypedEnqueueRequestsFromMapFunc(func(context.Context, *gpuv1.ClusterPolicy) []reconcile.Request { return requests() }))); err != nil {
		return err
	}
	if err := c.Watch(source.Kind(mgr.GetCache(), &gpuv1alpha1.GPUCluster{}, handler.TypedEnqueueRequestsFromMapFunc(func(context.Context, *gpuv1alpha1.GPUCluster) []reconcile.Request { return requests() }))); err != nil {
		return err
	}
	return c.Watch(source.Kind(mgr.GetCache(), &corev1.Service{}, handler.TypedEnqueueRequestsFromMapFunc(func(_ context.Context, svc *corev1.Service) []reconcile.Request {
		if svc.Namespace == r.Namespace && (svc.Name == "nvidia-dcgm" || svc.Name == "nvidia-dcgm-exporter" || svc.Name == "nvidia-dcgm-dra" || svc.Name == "nvidia-dcgm-exporter-dra") {
			return requests()
		}
		return nil
	})))
}
