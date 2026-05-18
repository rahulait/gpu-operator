/*
Copyright NVIDIA CORPORATION & AFFILIATES

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gpuv1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1"
	nvidiav1alpha1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1alpha1"
	"github.com/NVIDIA/gpu-operator/internal/consts"
)

func TestNvidiaDriverCRDEnabled(t *testing.T) {
	driverEnabled := true
	driverDisabled := false
	crdEnabled := true
	crdDisabled := false

	tests := []struct {
		name          string
		driverEnabled *bool
		crdEnabled    *bool
		expected      bool
	}{
		{
			name:       "driver enabled by default and CRD enabled",
			crdEnabled: &crdEnabled,
			expected:   true,
		},
		{
			name:       "CRD disabled",
			crdEnabled: &crdDisabled,
			expected:   false,
		},
		{
			name:          "driver disabled",
			driverEnabled: &driverDisabled,
			crdEnabled:    &crdEnabled,
			expected:      false,
		},
		{
			name:          "driver explicitly enabled and CRD enabled",
			driverEnabled: &driverEnabled,
			crdEnabled:    &crdEnabled,
			expected:      true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clusterPolicy := &gpuv1.ClusterPolicy{
				Spec: gpuv1.ClusterPolicySpec{
					Driver: gpuv1.DriverSpec{
						Enabled:            tc.driverEnabled,
						UseNvidiaDriverCRD: tc.crdEnabled,
					},
				},
			}

			require.Equal(t, tc.expected, nvidiaDriverCRDEnabled(clusterPolicy))
		})
	}
}

func TestEnsureDefaultNVIDIADriverMetadataDoesNotOverwriteExistingSpec(t *testing.T) {
	driver := &nvidiav1alpha1.NVIDIADriver{
		ObjectMeta: metav1.ObjectMeta{Name: consts.DefaultNVIDIADriverName},
		Spec: nvidiav1alpha1.NVIDIADriverSpec{
			DriverType: nvidiav1alpha1.GPU,
			Repository: "user-edited-repo",
			Image:      "driver",
			Version:    "555.42.02",
		},
	}

	require.True(t, ensureDefaultNVIDIADriverMetadata(driver))

	require.Equal(t, "user-edited-repo", driver.Spec.Repository)
	require.Equal(t, "555.42.02", driver.Spec.Version)
	require.Equal(t, "true", driver.Labels[consts.DefaultNVIDIADriverLabel])
	require.Contains(t, driver.Finalizers, consts.DefaultNVIDIADriverFinalizer)
}

func TestEnsureDefaultNVIDIADriverMetadataDoesNotOverwriteAnnotations(t *testing.T) {
	driver := &nvidiav1alpha1.NVIDIADriver{
		ObjectMeta: metav1.ObjectMeta{
			Name: consts.DefaultNVIDIADriverName,
			Annotations: map[string]string{
				"meta.helm.sh/release-name": "gpu-operator",
			},
		},
		Spec: nvidiav1alpha1.NVIDIADriverSpec{
			DriverType: nvidiav1alpha1.GPU,
			Version:    "550.54.14",
		},
	}

	require.True(t, ensureDefaultNVIDIADriverMetadata(driver))

	require.Equal(t, "550.54.14", driver.Spec.Version)
	require.Equal(t, "gpu-operator", driver.Annotations["meta.helm.sh/release-name"])
}

func TestAllowDefaultNVIDIADriverDeletion(t *testing.T) {
	crdEnabled := true
	crdDisabled := false

	tests := []struct {
		name            string
		clusterPolicies []gpuv1.ClusterPolicy
		expected        bool
	}{
		{
			name:     "allowed when no ClusterPolicy exists",
			expected: true,
		},
		{
			name: "blocked when live ClusterPolicy has NVIDIADriver enabled",
			clusterPolicies: []gpuv1.ClusterPolicy{
				{
					Spec: gpuv1.ClusterPolicySpec{
						Driver: gpuv1.DriverSpec{
							UseNvidiaDriverCRD: &crdEnabled,
						},
					},
				},
			},
			expected: false,
		},
		{
			name: "allowed when live ClusterPolicy falls back to legacy driver",
			clusterPolicies: []gpuv1.ClusterPolicy{
				{
					Spec: gpuv1.ClusterPolicySpec{
						Driver: gpuv1.DriverSpec{
							UseNvidiaDriverCRD: &crdDisabled,
						},
					},
				},
			},
			expected: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, allowDefaultNVIDIADriverDeletion(tc.clusterPolicies))
		})
	}
}

func TestAssignNVIDIADriverOwnersGivesSpecificDriversPrecedence(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, nvidiav1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	defaultDriver := &nvidiav1alpha1.NVIDIADriver{
		ObjectMeta: metav1.ObjectMeta{
			Name:   consts.DefaultNVIDIADriverName,
			Labels: map[string]string{consts.DefaultNVIDIADriverLabel: "true"},
		},
	}
	specificDriver := &nvidiav1alpha1.NVIDIADriver{
		ObjectMeta: metav1.ObjectMeta{Name: "h100-driver"},
		Spec: nvidiav1alpha1.NVIDIADriverSpec{
			NodeSelector: map[string]string{"nodepool": "h100"},
		},
	}
	defaultNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "default-node",
		Labels: map[string]string{consts.GPUPresentLabel: "true"},
	}}
	specificNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "specific-node",
		Labels: map[string]string{consts.GPUPresentLabel: "true", "nodepool": "h100"},
	}}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(defaultDriver, specificDriver, defaultNode, specificNode).Build()

	require.NoError(t, assignNVIDIADriverOwners(context.Background(), k8sClient))

	require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKey{Name: "default-node"}, defaultNode))
	require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKey{Name: "specific-node"}, specificNode))
	require.Equal(t, consts.DefaultNVIDIADriverName, defaultNode.Labels[consts.NVIDIADriverOwnerLabel])
	require.Equal(t, "h100-driver", specificNode.Labels[consts.NVIDIADriverOwnerLabel])
}
