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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gpuv1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1"
	nvidiav1alpha1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1alpha1"
	"github.com/NVIDIA/gpu-operator/internal/consts"
)

func isDefaultNVIDIADriver(driver *nvidiav1alpha1.NVIDIADriver) bool {
	if driver == nil {
		return false
	}
	return isDefaultNVIDIADriverName(driver) &&
		driver.Labels[consts.DefaultNVIDIADriverLabel] == "true"
}

func isDefaultNVIDIADriverName(driver *nvidiav1alpha1.NVIDIADriver) bool {
	return driver != nil && driver.Name == consts.DefaultNVIDIADriverName
}

func nvidiaDriverCRDEnabled(clusterPolicy *gpuv1.ClusterPolicy) bool {
	return clusterPolicy != nil &&
		clusterPolicy.Spec.Driver.IsEnabled() &&
		clusterPolicy.Spec.Driver.UseNvidiaDriverCRDType()
}

func assignNVIDIADriverOwners(ctx context.Context, c client.Client) error {
	drivers := &nvidiav1alpha1.NVIDIADriverList{}
	if err := c.List(ctx, drivers); err != nil {
		return fmt.Errorf("failed to list NVIDIADriver CRs: %w", err)
	}

	var defaultDriver *nvidiav1alpha1.NVIDIADriver
	specificDrivers := make([]nvidiav1alpha1.NVIDIADriver, 0, len(drivers.Items))
	for i := range drivers.Items {
		if isDefaultNVIDIADriver(&drivers.Items[i]) {
			defaultDriver = &drivers.Items[i]
			continue
		}
		specificDrivers = append(specificDrivers, drivers.Items[i])
	}
	if defaultDriver == nil && len(specificDrivers) == 0 {
		return nil
	}

	nodes := &corev1.NodeList{}
	if err := c.List(ctx, nodes, client.MatchingLabels{consts.GPUPresentLabel: "true"}); err != nil {
		return fmt.Errorf("failed to list GPU nodes: %w", err)
	}

	for i := range nodes.Items {
		node := &nodes.Items[i]
		owner := ""
		conflictingOwners := 0
		for _, driver := range specificDrivers {
			if nodeMatchesSelector(node, driver.GetNodeSelector()) {
				owner = driver.Name
				conflictingOwners++
			}
		}
		if conflictingOwners > 1 {
			continue
		}
		if owner == "" && defaultDriver != nil {
			owner = defaultDriver.Name
		}
		if owner == "" {
			continue
		}
		if node.Labels != nil && node.Labels[consts.NVIDIADriverOwnerLabel] == owner {
			continue
		}
		if node.Labels == nil {
			node.Labels = map[string]string{}
		}
		node.Labels[consts.NVIDIADriverOwnerLabel] = owner
		if err := c.Update(ctx, node); err != nil {
			return fmt.Errorf("failed to update NVIDIADriver owner label for node %s: %w", node.Name, err)
		}
	}

	return nil
}

func ensureDefaultNVIDIADriverMetadata(driver *nvidiav1alpha1.NVIDIADriver) bool {
	changed := false
	if driver.Labels == nil {
		driver.Labels = map[string]string{}
		changed = true
	}
	for key, value := range map[string]string{
		consts.DefaultNVIDIADriverLabel: "true",
	} {
		if driver.Labels[key] != value {
			driver.Labels[key] = value
			changed = true
		}
	}
	if driver.GetDeletionTimestamp() == nil && !controllerutil.ContainsFinalizer(driver, consts.DefaultNVIDIADriverFinalizer) {
		controllerutil.AddFinalizer(driver, consts.DefaultNVIDIADriverFinalizer)
		changed = true
	}
	return changed
}

func allowDefaultNVIDIADriverDeletion(clusterPolicies []gpuv1.ClusterPolicy) bool {
	if len(clusterPolicies) == 0 {
		return true
	}
	for i := range clusterPolicies {
		if clusterPolicies[i].GetDeletionTimestamp() == nil &&
			clusterPolicies[i].Spec.Driver.UseNvidiaDriverCRDType() {
			return false
		}
	}
	return true
}
