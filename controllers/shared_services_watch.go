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

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// Status writes must not create a reconcile loop, but deletionTimestamp changes
// need to release ownership even when the CR generation is unchanged.
func sharedServiceCRPredicate[T client.Object]() predicate.TypedFuncs[T] {
	return predicate.TypedFuncs[T]{UpdateFunc: func(e event.TypedUpdateEvent[T]) bool {
		return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() ||
			!e.ObjectOld.GetDeletionTimestamp().Equal(e.ObjectNew.GetDeletionTimestamp())
	}}
}

func isSharedService(svc *corev1.Service, namespace string) bool {
	if svc.Namespace != namespace {
		return false
	}
	return svc.Name == "nvidia-dcgm" || svc.Name == "nvidia-dcgm-exporter" || svc.Name == "nvidia-dcgm-dra" || svc.Name == "nvidia-dcgm-exporter-dra"
}

func watchSharedServices(mgr ctrl.Manager, c controller.Controller, namespace string, enqueue func(context.Context) []reconcile.Request) error {
	// Map by identity, not controller owner: shared Services have multiple
	// non-controller references, and manual deletion must wake either stack.
	return c.Watch(source.Kind(mgr.GetCache(), &corev1.Service{}, handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, svc *corev1.Service) []reconcile.Request {
		if isSharedService(svc, namespace) {
			return enqueue(ctx)
		}
		return nil
	})))
}
