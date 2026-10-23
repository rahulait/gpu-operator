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
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	gpuv1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1"
	gpuv1alpha1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1alpha1"
	"github.com/NVIDIA/gpu-operator/internal/consts"
)

func newSharedServicesTest(t *testing.T, hooks interceptor.Funcs, objects ...client.Object) *SharedServices {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, gpuv1.AddToScheme(scheme))
	require.NoError(t, gpuv1alpha1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithInterceptorFuncs(hooks).Build()
	return &SharedServices{Client: c, Reader: c, Scheme: scheme, Namespace: "gpu-operator", ManifestDir: "../manifests/shared"}
}

func sharedServicesTestCRs() (*gpuv1.ClusterPolicy, *gpuv1alpha1.GPUCluster) {
	cp := &gpuv1.ClusterPolicy{ObjectMeta: metav1.ObjectMeta{Name: "cluster-policy", UID: "cp"}}
	cp.Spec.DCGM.Enabled = ptr.To(true)
	gc := &gpuv1alpha1.GPUCluster{ObjectMeta: metav1.ObjectMeta{Name: "gpu-cluster", UID: "gc"}}
	gc.Spec.DCGM = &gpuv1.DCGMSpec{Enabled: ptr.To(true)}
	gc.Spec.DCGMExporter = &gpuv1.DCGMExporterSpec{}
	return cp, gc
}

func TestSharedServicesLifecycle(t *testing.T) {
	for name, stacks := range map[string]int{"ClusterPolicy": 1, "GPUCluster": 2, "both": 3, "neither": 0} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			cp, gc := sharedServicesTestCRs()
			var objects []client.Object
			if stacks&1 != 0 {
				objects = append(objects, cp)
			}
			if stacks&2 != 0 {
				objects = append(objects, gc)
			}
			s := newSharedServicesTest(t, interceptor.Funcs{}, objects...)
			require.NoError(t, s.reconcile(ctx))
			for _, name := range sharedServiceNames {
				svc := &corev1.Service{}
				key := client.ObjectKey{Namespace: s.Namespace, Name: name}
				err := s.Client.Get(ctx, key, svc)
				if stacks == 0 {
					assert.True(t, apierrors.IsNotFound(err))
					continue
				}
				require.NoError(t, err)
				assert.Equal(t, map[string]string{AppComponentLabelKey: name}, svc.Spec.Selector)
				assert.Equal(t, name, svc.Labels[AppComponentLabelKey])
				count := 1
				if stacks == 3 {
					count = 2
				}
				require.Len(t, svc.OwnerReferences, count)
				for _, owner := range svc.OwnerReferences {
					assert.False(t, ptr.Deref(owner.Controller, false))
				}
				if name == "nvidia-dcgm" {
					assert.Equal(t, corev1.ServiceInternalTrafficPolicyLocal, *svc.Spec.InternalTrafficPolicy)
				}
				rv := svc.ResourceVersion
				require.NoError(t, s.reconcile(ctx))
				require.NoError(t, s.Client.Get(ctx, key, svc))
				assert.Equal(t, rv, svc.ResourceVersion)
			}
		})
	}
}

func TestSharedServicesDisableReleaseAndRecovery(t *testing.T) {
	ctx := context.Background()
	cp, gc := sharedServicesTestCRs()
	s := newSharedServicesTest(t, interceptor.Funcs{}, cp, gc)
	require.NoError(t, s.reconcile(ctx))
	require.NoError(t, s.Client.Get(ctx, client.ObjectKeyFromObject(cp), cp))
	cp.Spec.DCGM.Enabled = ptr.To(false)
	cp.Spec.DCGMExporter.Enabled = ptr.To(false)
	require.NoError(t, s.Client.Update(ctx, cp))
	// A releasing stack must retain endpoints needed by the other stack.
	require.NoError(t, s.ReleaseSharedService(ctx, "nvidia-dcgm"))
	require.NoError(t, s.reconcile(ctx))
	key := client.ObjectKey{Namespace: s.Namespace, Name: "nvidia-dcgm"}
	svc := &corev1.Service{}
	require.NoError(t, s.Client.Get(ctx, key, svc))
	require.Len(t, svc.OwnerReferences, 1)
	assert.Equal(t, gc.UID, svc.OwnerReferences[0].UID)
	require.NoError(t, s.Client.Delete(ctx, svc))
	require.NoError(t, s.reconcile(ctx))
	require.NoError(t, s.Client.Get(ctx, key, svc))
	require.NoError(t, s.Client.Get(ctx, client.ObjectKeyFromObject(gc), gc))
	gc.Spec.DCGM.Enabled = ptr.To(false)
	gc.Spec.DCGMExporter.Enabled = ptr.To(false)
	require.NoError(t, s.Client.Update(ctx, gc))
	require.NoError(t, s.reconcile(ctx))
	for _, name := range sharedServiceNames {
		key.Name = name
		assert.True(t, apierrors.IsNotFound(s.Client.Get(ctx, key, &corev1.Service{})))
	}
}

func TestSharedServicesConfigurationConflict(t *testing.T) {
	ctx := context.Background()
	cp, gc := sharedServicesTestCRs()
	gc.Spec.DCGMExporter.ServiceSpec = &gpuv1.DCGMExporterServiceConfig{Type: corev1.ServiceTypeClusterIP, InternalTrafficPolicy: ptr.To(corev1.ServiceInternalTrafficPolicyCluster)}
	s := newSharedServicesTest(t, interceptor.Funcs{}, cp, gc)
	require.NoError(t, s.reconcile(ctx))
	key := client.ObjectKey{Namespace: s.Namespace, Name: "nvidia-dcgm-exporter"}
	before := &corev1.Service{}
	require.NoError(t, s.Client.Get(ctx, key, before))
	require.NoError(t, s.Client.Get(ctx, client.ObjectKeyFromObject(gc), gc))
	gc.Spec.DCGMExporter.ServiceSpec.Type = corev1.ServiceTypeNodePort
	require.NoError(t, s.Client.Update(ctx, gc))
	require.ErrorContains(t, s.reconcile(ctx), "conflicting")
	after := &corev1.Service{}
	require.NoError(t, s.Client.Get(ctx, key, after))
	assert.Equal(t, before, after)
}

func TestSharedServicesMigration(t *testing.T) {
	ctx := context.Background()
	cp, _ := sharedServicesTestCRs()
	cp.Spec.DCGMExporter.ServiceSpec = &gpuv1.DCGMExporterServiceConfig{Type: corev1.ServiceTypeNodePort}
	old := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "nvidia-dcgm-exporter", Namespace: "gpu-operator", Labels: map[string]string{consts.StateLabel: "state-dcgm-exporter", "custom": "value"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: gpuv1.SchemeGroupVersion.String(), Kind: "ClusterPolicy", Name: cp.Name, UID: cp.UID, Controller: ptr.To(true)}}}, Spec: corev1.ServiceSpec{ClusterIP: "10.0.0.5", ClusterIPs: []string{"10.0.0.5"}, IPFamilies: []corev1.IPFamily{corev1.IPv4Protocol}, IPFamilyPolicy: ptr.To(corev1.IPFamilyPolicySingleStack), Type: corev1.ServiceTypeNodePort, Ports: []corev1.ServicePort{{Name: "gpu-metrics", Port: 9400, Protocol: corev1.ProtocolTCP, NodePort: 30400}}}}
	legacy := old.DeepCopy()
	legacy.Name += "-dra"
	s := newSharedServicesTest(t, interceptor.Funcs{}, cp, old, legacy)
	require.NoError(t, s.reconcile(ctx))
	svc := &corev1.Service{}
	require.NoError(t, s.Client.Get(ctx, client.ObjectKeyFromObject(old), svc))
	assert.Equal(t, old.Spec.ClusterIP, svc.Spec.ClusterIP)
	assert.Equal(t, old.Spec.ClusterIPs, svc.Spec.ClusterIPs)
	assert.Equal(t, old.Spec.IPFamilies, svc.Spec.IPFamilies)
	assert.Equal(t, old.Spec.IPFamilyPolicy, svc.Spec.IPFamilyPolicy)
	assert.Equal(t, int32(30400), svc.Spec.Ports[0].NodePort)
	assert.NotContains(t, svc.Labels, consts.StateLabel)
	assert.Equal(t, "value", svc.Labels["custom"])
	assert.False(t, ptr.Deref(svc.OwnerReferences[0].Controller, false))
	assert.True(t, apierrors.IsNotFound(s.Client.Get(ctx, client.ObjectKeyFromObject(legacy), &corev1.Service{})))
}

func TestSharedServicesConcurrentCallers(t *testing.T) {
	cp, gc := sharedServicesTestCRs()
	s := newSharedServicesTest(t, interceptor.Funcs{}, cp, gc)
	// Distinct helper instances model the two independent controllers.
	other := *s
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, caller := range []*SharedServices{s, &other} {
		wg.Add(1)
		go func(caller *SharedServices) { defer wg.Done(); errs <- caller.reconcile(context.Background()) }(caller)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	svc := &corev1.Service{}
	require.NoError(t, s.Client.Get(context.Background(), client.ObjectKey{Namespace: s.Namespace, Name: "nvidia-dcgm"}, svc))
	require.Len(t, svc.OwnerReferences, 2)
}

func TestSharedServicesConflictRetriesFreshState(t *testing.T) {
	ctx := context.Background()
	cp, gc := sharedServicesTestCRs()
	attempts := 0
	hooks := interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if obj.GetName() == "nvidia-dcgm" {
			attempts++
			if attempts == 1 {
				// Enablement changes after the initial read, concurrent with a write.
				current := &gpuv1.ClusterPolicy{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(cp), current); err != nil {
					return err
				}
				current.Spec.DCGM.Enabled = ptr.To(false)
				if err := c.Update(ctx, current); err != nil {
					return err
				}
				return apierrors.NewConflict(schema.GroupResource{Resource: "services"}, obj.GetName(), fmt.Errorf("injected conflict"))
			}
		}
		return c.Update(ctx, obj, opts...)
	}}
	s := newSharedServicesTest(t, hooks, cp, gc)
	old := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "nvidia-dcgm", Namespace: s.Namespace, Labels: map[string]string{sharedServiceLabel: "true"}}}
	require.NoError(t, s.Client.Create(ctx, old))
	require.NoError(t, s.SyncSharedService(ctx, old.Name))
	assert.Equal(t, 2, attempts)
	svc := &corev1.Service{}
	require.NoError(t, s.Client.Get(ctx, client.ObjectKeyFromObject(old), svc))
	require.Len(t, svc.OwnerReferences, 1)
	assert.Equal(t, gc.UID, svc.OwnerReferences[0].UID)
}

func TestSharedServicesReleaseRace(t *testing.T) {
	ctx := context.Background()
	cp, gc := sharedServicesTestCRs()
	cp.Spec.DCGM.Enabled = ptr.To(false)
	cp.Spec.DCGMExporter.Enabled = ptr.To(false)
	gc.Spec.DCGM.Enabled = ptr.To(false)
	gc.Spec.DCGMExporter.Enabled = ptr.To(false)
	deletes := 0
	hooks := interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		if obj.GetName() == "nvidia-dcgm" {
			deletes++
			options := &client.DeleteOptions{}
			for _, opt := range opts {
				opt.ApplyToDelete(options)
			}
			require.NotNil(t, options.Preconditions)
			require.NotNil(t, options.Preconditions.UID)
			require.NotNil(t, options.Preconditions.ResourceVersion)
			assert.Equal(t, obj.GetUID(), *options.Preconditions.UID)
			assert.Equal(t, obj.GetResourceVersion(), *options.Preconditions.ResourceVersion)
			current := &gpuv1alpha1.GPUCluster{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(gc), current); err != nil {
				return err
			}
			current.Spec.DCGM.Enabled = ptr.To(true)
			if err := c.Update(ctx, current); err != nil {
				return err
			}
			return apierrors.NewConflict(schema.GroupResource{Resource: "services"}, obj.GetName(), fmt.Errorf("concurrent sync"))
		}
		return c.Delete(ctx, obj, opts...)
	}}
	old := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "nvidia-dcgm", Namespace: "gpu-operator", Labels: map[string]string{sharedServiceLabel: "true"}}}
	s := newSharedServicesTest(t, hooks, cp, gc, old)
	require.NoError(t, s.ReleaseSharedService(ctx, old.Name))
	assert.Equal(t, 1, deletes)
	require.NoError(t, s.SyncSharedService(ctx, old.Name))
	svc := &corev1.Service{}
	require.NoError(t, s.Client.Get(ctx, client.ObjectKeyFromObject(old), svc))
	require.Len(t, svc.OwnerReferences, 1)
	assert.Equal(t, gc.UID, svc.OwnerReferences[0].UID)
}

func TestSharedServiceDeletionPredicate(t *testing.T) {
	cp, _ := sharedServicesTestCRs()
	deleted := cp.DeepCopy()
	deleted.DeletionTimestamp = ptr.To(metav1.Now())
	predicate := sharedServiceCRPredicate[*gpuv1.ClusterPolicy]()
	assert.True(t, predicate.Update(event.TypedUpdateEvent[*gpuv1.ClusterPolicy]{ObjectOld: cp, ObjectNew: deleted}))
	assert.False(t, predicate.Update(event.TypedUpdateEvent[*gpuv1.ClusterPolicy]{ObjectOld: cp, ObjectNew: cp.DeepCopy()}))
	assert.True(t, isSharedService(&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "nvidia-dcgm", Namespace: "gpu-operator"}}, "gpu-operator"))
	assert.False(t, isSharedService(&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "nvidia-dcgm", Namespace: "other"}}, "gpu-operator"))
}

func TestSharedServicesIndependentDefaultsAndUnmanagedObjects(t *testing.T) {
	ctx := context.Background()
	cp, gc := sharedServicesTestCRs()
	cp.Spec.DCGM.Enabled = ptr.To(false)
	cp.Spec.DCGMExporter.Enabled = nil
	s := newSharedServicesTest(t, interceptor.Funcs{}, cp)
	require.NoError(t, s.reconcile(ctx))
	require.NoError(t, s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: "nvidia-dcgm-exporter"}, &corev1.Service{}))
	assert.True(t, apierrors.IsNotFound(s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: "nvidia-dcgm"}, &corev1.Service{})))
	gc.Spec.DCGM = &gpuv1.DCGMSpec{}
	gc.Spec.DCGMExporter = nil
	s = newSharedServicesTest(t, interceptor.Funcs{}, gc)
	require.NoError(t, s.reconcile(ctx))
	services := &corev1.ServiceList{}
	require.NoError(t, s.Client.List(ctx, services))
	assert.Empty(t, services.Items)
	userService := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "nvidia-dcgm", Namespace: s.Namespace}}
	s = newSharedServicesTest(t, interceptor.Funcs{}, userService)
	require.NoError(t, s.ReleaseSharedService(ctx, userService.Name))
	require.NoError(t, s.Client.Get(ctx, client.ObjectKeyFromObject(userService), &corev1.Service{}))
	cp.Spec.DCGM.Enabled = ptr.To(true)
	cp.ResourceVersion = ""
	require.NoError(t, s.Client.Create(ctx, cp))
	require.ErrorContains(t, s.SyncSharedService(ctx, userService.Name), "unmanaged")
}
