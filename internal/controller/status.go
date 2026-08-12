/*
Copyright 2026.

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

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"

	networkingv1alpha1 "github.com/openshift/bgp-cloud-connector/api/v1alpha1"
)

// configStatusEqual reports whether two Config status values are semantically equal.
func configStatusEqual(a, b networkingv1alpha1.CUDNBgpConfigStatus) bool {
	return apiequality.Semantic.DeepEqual(a, b)
}

// routingStatusEqual reports whether two Routing status values are semantically equal.
func routingStatusEqual(a, b networkingv1alpha1.CUDNBgpRoutingStatus) bool {
	return apiequality.Semantic.DeepEqual(a, b)
}

// patchConfigStatus updates status when desired differs from the etcd baseline.
// baselineStatus must be a DeepCopy of status as read from the API server at reconcile start.
func (r *CUDNBgpConfigReconciler) patchConfigStatus(
	ctx context.Context,
	config *networkingv1alpha1.CUDNBgpConfig,
	baselineStatus networkingv1alpha1.CUDNBgpConfigStatus,
	mutate func(*networkingv1alpha1.CUDNBgpConfig),
) error {
	desired := config.DeepCopy()
	mutate(desired)

	if configStatusEqual(baselineStatus, desired.Status) {
		// Skip Status().Update when desired status matches etcd to avoid hot-loop writes.
		config.Status = desired.Status
		return nil
	}

	emitConditionEvents(r.Recorder, config, baselineStatus.Conditions, desired.Status.Conditions)

	config.Status = desired.Status
	return r.Status().Update(ctx, config)
}

// emitConditionEvents announces conditions that changed.
//
// Conditions say what is true now; events say what changed, and the change is
// what a person wants when they run oc describe. Emitting from the one place
// status is written, and only for conditions whose status or reason actually
// moved, is what keeps the five minute resync from republishing the same set
// forever and burying the transition that mattered.
//
// A nil recorder is tolerated so a unit test that does not care about events
// need not supply one.
func emitConditionEvents(
	recorder record.EventRecorder,
	object runtime.Object,
	before, after []metav1.Condition,
) {
	if recorder == nil {
		return
	}
	for i := range after {
		cond := after[i]
		old := meta.FindStatusCondition(before, cond.Type)
		// The message is compared as well as the status and reason, because
		// the router set growing or shrinking moves neither of those: every
		// condition stays True with the same reason and only the node count
		// in the message changes. That is exactly the transition worth
		// announcing. It is safe to compare because these messages are
		// derived from the reconcile's inputs and carry nothing volatile.
		if old != nil && old.Status == cond.Status && old.Reason == cond.Reason && old.Message == cond.Message {
			continue
		}
		eventType := corev1.EventTypeNormal
		if cond.Status != metav1.ConditionTrue {
			eventType = corev1.EventTypeWarning
		}
		recorder.Eventf(object, eventType, cond.Reason, "%s: %s", cond.Type, cond.Message)
	}
}

// patchRoutingStatus updates status when desired differs from the etcd baseline.
// baselineStatus must be a DeepCopy of status as read from the API server at reconcile start.
func (r *CUDNBgpRoutingReconciler) patchRoutingStatus(
	ctx context.Context,
	routing *networkingv1alpha1.CUDNBgpRouting,
	baselineStatus networkingv1alpha1.CUDNBgpRoutingStatus,
	mutate func(*networkingv1alpha1.CUDNBgpRouting),
) error {
	desired := routing.DeepCopy()
	mutate(desired)

	if routingStatusEqual(baselineStatus, desired.Status) {
		// Skip Status().Update when desired status matches etcd to avoid hot-loop writes.
		routing.Status = desired.Status
		return nil
	}

	emitConditionEvents(r.Recorder, routing, baselineStatus.Conditions, desired.Status.Conditions)

	routing.Status = desired.Status
	return r.Status().Update(ctx, routing)
}

// reportDeletionBlocked sets DeletionBlocked condition when routing CRs block config deletion.
func (r *CUDNBgpConfigReconciler) reportDeletionBlocked(
	ctx context.Context,
	config *networkingv1alpha1.CUDNBgpConfig,
	baselineStatus networkingv1alpha1.CUDNBgpConfigStatus,
	routings []networkingv1alpha1.CUDNBgpRouting,
) error {
	names := make([]string, len(routings))
	for i := range routings {
		names[i] = routings[i].Name
	}
	sort.Strings(names)

	condMessage := fmt.Sprintf("%d CUDNBgpRouting CR(s) must be deleted first: %s",
		len(names), strings.Join(names, ", "))

	return r.patchConfigStatus(ctx, config, baselineStatus, func(c *networkingv1alpha1.CUDNBgpConfig) {
		meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
			Type:               ConditionDeletionBlocked,
			Status:             metav1.ConditionTrue,
			Reason:             "RoutingCRsExist",
			Message:            condMessage,
			ObservedGeneration: c.Generation,
		})
	})
}
