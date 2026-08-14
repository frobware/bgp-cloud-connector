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
// negativePolarityConditions are the conditions this controller reports whose
// normal state is False. Every other condition it reports is normal when True.
//
// The polarity cannot be derived from the condition itself, and the API
// conventions say so: for some conditions True represents normal operation and
// for some False does, and without further knowledge of the conditions it is
// not possible to compute a generic summary. That knowledge is here because
// there is nowhere else it can be.
var negativePolarityConditions = map[string]struct{}{
	networkingv1alpha1.ConditionSuspended: {},
	ConditionDeletionBlocked:              {},
}

// deletionBlockedMessage explains why a delete is not proceeding, and what to
// do about it.
//
// The person reading this is by definition looking at a delete that appears to
// have hung with no other explanation, so the message carries the command that
// unblocks it rather than only the names.
//
// The names appear once, space separated inside that command, so the whole
// thing can be pasted into a shell. Listing them again comma separated would
// be the same information in a form nobody can use.
//
// Sorted, because the list comes from a List call whose order is not
// guaranteed: an unsorted message would differ between reconciles and re-emit
// an event every ten seconds for a condition that has not changed.
func deletionBlockedMessage(names []string) string {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	return fmt.Sprintf(
		"%d CUDNBgpRouting CR(s) must be deleted first. To proceed: oc delete cudnbgprouting %s",
		len(sorted), strings.Join(sorted, " "))
}

// conditionEventType decides whether a condition's new state is news or a
// problem.
//
// Unknown is neither. It says the controller is not currently observing the
// thing, which is what suspending deliberately leaves behind, and a screenful
// of warnings is the wrong answer to somebody asking for the operator to stop.
// A genuine failure sets the condition False with a reason, and that still
// warns.
func conditionEventType(cond metav1.Condition) string {
	if cond.Status == metav1.ConditionUnknown {
		return corev1.EventTypeNormal
	}
	normal := metav1.ConditionTrue
	if _, negative := negativePolarityConditions[cond.Type]; negative {
		normal = metav1.ConditionFalse
	}
	if cond.Status == normal {
		return corev1.EventTypeNormal
	}
	return corev1.EventTypeWarning
}

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
		recorder.Eventf(object, conditionEventType(cond), cond.Reason, "%s: %s", cond.Type, cond.Message)
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
	condMessage := deletionBlockedMessage(names)

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
