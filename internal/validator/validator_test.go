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

package validator

import (
	"context"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nvidiav1alpha1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1alpha1"
	"github.com/NVIDIA/gpu-operator/internal/consts"
)

const (
	testDriverName = "my-nvidia-driver"
	testNodeName   = "my-test-node"
)

type driverOptions func(*nvidiav1alpha1.NVIDIADriver)

func makeTestDriver(opts ...driverOptions) *nvidiav1alpha1.NVIDIADriver {
	c := &nvidiav1alpha1.NVIDIADriver{
		ObjectMeta: metav1.ObjectMeta{
			Name: testDriverName,
		},
		Spec: nvidiav1alpha1.NVIDIADriverSpec{
			Image:   "",
			Version: "",
		},
	}

	c.Kind = reflect.TypeOf(nvidiav1alpha1.NVIDIADriver{}).Name()

	gvk := nvidiav1alpha1.SchemeGroupVersion.WithKind(c.Kind)

	c.APIVersion = gvk.GroupVersion().String()

	for _, o := range opts {
		o(c)
	}
	return c
}

func named(name string) driverOptions {
	return func(c *nvidiav1alpha1.NVIDIADriver) {
		c.Name = name
	}
}

func nodeSelector(labels map[string]string) driverOptions {
	return func(c *nvidiav1alpha1.NVIDIADriver) {
		c.Spec.NodeSelector = labels
	}
}

func defaultDriver() driverOptions {
	return func(c *nvidiav1alpha1.NVIDIADriver) {
		c.Spec.Default = true
	}
}

func labelled(labels map[string]string) nodeOptions {
	return func(n *corev1.Node) {
		n.Labels = labels
	}
}

type nodeOptions func(*corev1.Node)

func makeTestNode(opts ...nodeOptions) *corev1.Node {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNodeName,
		},
	}
	for _, o := range opts {
		o(n)
	}
	return n
}

func TestCheckNodeSelector(t *testing.T) {
	node := makeTestNode(labelled(map[string]string{"os-version": "ubuntu20.04"}))
	driver := makeTestDriver(nodeSelector(node.Labels))
	conflictingDriver := makeTestDriver(named("conflictingDriver"), nodeSelector(node.Labels))
	nonconflictingDriver := makeTestDriver(named("nonconflictingDriver"))

	tests := []struct {
		node            *corev1.Node
		existingDriver  *nvidiav1alpha1.NVIDIADriver
		requestedDriver *nvidiav1alpha1.NVIDIADriver
		shouldError     bool
	}{
		{node: node, existingDriver: driver, requestedDriver: conflictingDriver, shouldError: true},
		{node: node, existingDriver: driver, requestedDriver: nonconflictingDriver, shouldError: false},
	}

	for _, tc := range tests {
		s := scheme.Scheme
		err := nvidiav1alpha1.AddToScheme(s)
		require.NoError(t, err)
		c := fake.
			NewClientBuilder().
			WithScheme(s).
			WithObjects(tc.node, tc.existingDriver, tc.requestedDriver).
			Build()
		nsv := NewNodeSelectorValidator(c)

		err = nsv.Validate(context.Background(), tc.requestedDriver)
		if tc.shouldError {
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "conflicting NVIDIADriver NodeSelectors found for node my-test-node")
			assert.Contains(t, err.Error(), "conflictingDriver")
			assert.Contains(t, err.Error(), testDriverName)
		} else {
			assert.NoError(t, err)
		}
	}
}

func TestCheckNodeSelectorIgnoresDefaultDriver(t *testing.T) {
	node := makeTestNode(labelled(map[string]string{
		"nvidia.com/gpu.present": "true",
		"nodepool":               "a",
	}))
	defaultDriver := makeTestDriver(defaultDriver())
	requestedDriver := makeTestDriver(named("specificDriver"), nodeSelector(map[string]string{"nodepool": "a"}))

	s := scheme.Scheme
	err := nvidiav1alpha1.AddToScheme(s)
	require.NoError(t, err)
	c := fake.
		NewClientBuilder().
		WithScheme(s).
		WithObjects(node, defaultDriver, requestedDriver).
		Build()
	nsv := NewNodeSelectorValidator(c)

	err = nsv.Validate(context.Background(), requestedDriver)
	assert.NoError(t, err)
}

func TestCheckNodeSelectorRejectsReservedOwnerLabel(t *testing.T) {
	driver := makeTestDriver(nodeSelector(map[string]string{consts.NVIDIADriverOwnerLabel: "other-driver"}))

	s := scheme.Scheme
	err := nvidiav1alpha1.AddToScheme(s)
	require.NoError(t, err)
	c := fake.
		NewClientBuilder().
		WithScheme(s).
		WithObjects(driver).
		Build()
	nsv := NewNodeSelectorValidator(c)

	err = nsv.Validate(context.Background(), driver)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "reserved label")
	assert.Contains(t, err.Error(), consts.NVIDIADriverOwnerLabel)
}

func TestCheckNodeSelectorRejectsDefaultDriverNodeSelector(t *testing.T) {
	driver := makeTestDriver(defaultDriver(), nodeSelector(map[string]string{"nodepool": "default"}))

	s := scheme.Scheme
	err := nvidiav1alpha1.AddToScheme(s)
	require.NoError(t, err)
	c := fake.
		NewClientBuilder().
		WithScheme(s).
		WithObjects(driver).
		Build()
	nsv := NewNodeSelectorValidator(c)

	err = nsv.Validate(context.Background(), driver)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "default NVIDIADriver")
	assert.Contains(t, err.Error(), "cannot use nodeSelector")
}

func TestCheckNodeSelectorDoesNotIgnoreDefaultNameWithoutDefaultField(t *testing.T) {
	node := makeTestNode(labelled(map[string]string{
		"nvidia.com/gpu.present": "true",
		"nodepool":               "a",
	}))
	nonDefaultNameDriver := makeTestDriver(named(consts.DefaultNVIDIADriverName), nodeSelector(map[string]string{"nodepool": "a"}))
	requestedDriver := makeTestDriver(named("specificDriver"), nodeSelector(map[string]string{"nodepool": "a"}))

	s := scheme.Scheme
	err := nvidiav1alpha1.AddToScheme(s)
	require.NoError(t, err)
	c := fake.
		NewClientBuilder().
		WithScheme(s).
		WithObjects(node, nonDefaultNameDriver, requestedDriver).
		Build()
	nsv := NewNodeSelectorValidator(c)

	err = nsv.Validate(context.Background(), requestedDriver)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "conflicting NVIDIADriver NodeSelectors found for node my-test-node")
	assert.Contains(t, err.Error(), consts.DefaultNVIDIADriverName)
	assert.Contains(t, err.Error(), "specificDriver")
}
