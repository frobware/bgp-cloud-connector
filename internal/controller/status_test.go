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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	networkingv1alpha1 "github.com/openshift/bgp-cloud-connector/api/v1alpha1"
)

func TestConfigStatusEqual(t *testing.T) {
	base := networkingv1alpha1.CUDNBgpConfigStatus{
		Phase:              networkingv1alpha1.PhaseConfiguring,
		ObservedGeneration: 1,
		Conditions: []metav1.Condition{
			{Type: networkingv1alpha1.ConditionFRRNamespaceReady, Status: metav1.ConditionFalse, Reason: "WaitingForFRR"},
		},
	}
	same := base.DeepCopy()
	if !configStatusEqual(base, *same) {
		t.Fatal("expected DeepCopy status to be equal")
	}
	diff := base.DeepCopy()
	diff.Phase = networkingv1alpha1.PhaseReady
	if configStatusEqual(base, *diff) {
		t.Fatal("expected different phase to be unequal")
	}
}

func TestRoutingStatusEqual_NilVsEmptyConditions(t *testing.T) {
	withNil := networkingv1alpha1.CUDNBgpRoutingStatus{
		Phase:              networkingv1alpha1.PhasePending,
		ObservedGeneration: 1,
	}
	withEmpty := networkingv1alpha1.CUDNBgpRoutingStatus{
		Phase:              networkingv1alpha1.PhasePending,
		ObservedGeneration: 1,
		Conditions:         []metav1.Condition{},
	}
	if !routingStatusEqual(withNil, withEmpty) {
		t.Fatal("expected nil and empty conditions to compare equal with Semantic equality")
	}
}

func TestPatchConfigStatus_SkipsUnchangedStatus(t *testing.T) {
	ctx := context.Background()

	config := &networkingv1alpha1.CUDNBgpConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:       SingletonName,
			Generation: 1,
		},
		Status: networkingv1alpha1.CUDNBgpConfigStatus{
			Phase:              networkingv1alpha1.PhaseConfiguring,
			ObservedGeneration: 1,
		},
	}
	baseline := config.Status.DeepCopy()

	c := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(config).WithObjects(config).Build()
	r := &CUDNBgpConfigReconciler{Client: c, Scheme: testScheme()}

	before := config.DeepCopy()
	if err := r.patchConfigStatus(ctx, config, *baseline, func(c *networkingv1alpha1.CUDNBgpConfig) {
		c.Status.Phase = networkingv1alpha1.PhaseConfiguring
		c.Status.ObservedGeneration = c.Generation
	}); err != nil {
		t.Fatalf("patchConfigStatus: %v", err)
	}

	after := &networkingv1alpha1.CUDNBgpConfig{}
	if err := c.Get(ctx, types.NamespacedName{Name: SingletonName}, after); err != nil {
		t.Fatalf("get config: %v", err)
	}
	if after.ResourceVersion != before.ResourceVersion {
		t.Fatalf("expected no status update, resourceVersion changed from %q to %q", before.ResourceVersion, after.ResourceVersion)
	}
}

func TestPatchConfigStatus_WritesWhenPhaseChanges(t *testing.T) {
	ctx := context.Background()

	config := &networkingv1alpha1.CUDNBgpConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:       SingletonName,
			Generation: 1,
		},
		Status: networkingv1alpha1.CUDNBgpConfigStatus{
			Phase:              networkingv1alpha1.PhaseConfiguring,
			ObservedGeneration: 1,
		},
	}
	baseline := config.Status.DeepCopy()

	c := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(config).WithObjects(config).Build()
	r := &CUDNBgpConfigReconciler{Client: c, Scheme: testScheme()}

	if err := r.patchConfigStatus(ctx, config, *baseline, func(c *networkingv1alpha1.CUDNBgpConfig) {
		c.Status.Phase = networkingv1alpha1.PhaseReady
		c.Status.ObservedGeneration = c.Generation
	}); err != nil {
		t.Fatalf("patchConfigStatus: %v", err)
	}

	after := &networkingv1alpha1.CUDNBgpConfig{}
	if err := c.Get(ctx, types.NamespacedName{Name: SingletonName}, after); err != nil {
		t.Fatalf("get config: %v", err)
	}
	if after.Status.Phase != networkingv1alpha1.PhaseReady {
		t.Fatalf("expected phase Ready, got %q", after.Status.Phase)
	}
}

func TestPatchRoutingStatus_SkipsUnchangedPending(t *testing.T) {
	ctx := context.Background()

	routing := &networkingv1alpha1.CUDNBgpRouting{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "prod",
			Generation: 1,
		},
		Status: networkingv1alpha1.CUDNBgpRoutingStatus{
			Phase:              networkingv1alpha1.PhasePending,
			ObservedGeneration: 1,
		},
	}
	baseline := routing.Status.DeepCopy()

	c := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(routing).WithObjects(routing).Build()
	r := &CUDNBgpRoutingReconciler{Client: c, Scheme: testScheme()}

	before := routing.DeepCopy()
	if err := r.patchRoutingStatus(ctx, routing, *baseline, func(rt *networkingv1alpha1.CUDNBgpRouting) {
		rt.Status.Phase = networkingv1alpha1.PhasePending
		rt.Status.Conditions = nil
	}); err != nil {
		t.Fatalf("patchRoutingStatus: %v", err)
	}

	after := &networkingv1alpha1.CUDNBgpRouting{}
	if err := c.Get(ctx, types.NamespacedName{Name: "prod"}, after); err != nil {
		t.Fatalf("get routing: %v", err)
	}
	if after.ResourceVersion != before.ResourceVersion {
		t.Fatalf("expected no status update, resourceVersion changed from %q to %q", before.ResourceVersion, after.ResourceVersion)
	}
}

// TestDeletionBlocked_MessageSaysHowToUnblock covers the message a user
// actually sees. Naming the offending CRs says what is wrong; it does not say
// what to do about it, and the person reading it is by definition looking at a
// delete that appears to have hung.
func TestDeletionBlocked_MessageSaysHowToUnblock(t *testing.T) {
	msg := deletionBlockedMessage([]string{"prod2", "prod"})

	// The command has to be pasteable, so the names are space separated
	// inside it and appear nowhere else.
	if !strings.Contains(msg, "oc delete cudnbgprouting prod prod2") {
		t.Errorf("message %q does not carry a runnable command", msg)
	}
	if strings.Contains(msg, ",") {
		t.Errorf("message %q joins names with a comma; that cannot be pasted", msg)
	}
	// Sorted, so the message does not churn between reconciles and re-emit
	// an event every pass on a List-ordered slice.
	if deletionBlockedMessage([]string{"b", "a"}) != deletionBlockedMessage([]string{"a", "b"}) {
		t.Error("message depends on input order; it will churn between reconciles")
	}
}
