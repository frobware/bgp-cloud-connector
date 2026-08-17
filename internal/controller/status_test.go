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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	networkingv1alpha1 "github.com/openshift/bgp-cloud-connector/api/v1alpha1"
)

func TestConfigStatusEqual(t *testing.T) {
	base := networkingv1alpha1.CUDNBgpConfigStatus{
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
	diff.Conditions[0].Status = metav1.ConditionTrue
	if configStatusEqual(base, *diff) {
		t.Fatal("expected a changed condition to be unequal")
	}
}

func TestRoutingStatusEqual_NilVsEmptyConditions(t *testing.T) {
	withNil := networkingv1alpha1.CUDNBgpRoutingStatus{
		ObservedGeneration: 1,
	}
	withEmpty := networkingv1alpha1.CUDNBgpRoutingStatus{
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
			ObservedGeneration: 1,
		},
	}
	// A steady state carries the conditions a reconcile leaves behind,
	// including the derived Ready. A status with none is not something this
	// controller ever writes, so testing the no-op path against one would be
	// testing a state that cannot occur.
	steady := []metav1.Condition{
		cond(networkingv1alpha1.ConditionFRRNamespaceReady, metav1.ConditionTrue),
	}
	config.Status.Conditions = append(steady, readyCondition(steady, config.Generation))
	baseline := config.Status.DeepCopy()

	c := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(config).WithObjects(config).Build()
	r := &CUDNBgpConfigReconciler{Client: c, Scheme: testScheme()}

	before := config.DeepCopy()
	if err := r.patchConfigStatus(ctx, config, *baseline, func(c *networkingv1alpha1.CUDNBgpConfig) {
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

func TestPatchConfigStatus_WritesWhenStatusChanges(t *testing.T) {
	ctx := context.Background()

	config := &networkingv1alpha1.CUDNBgpConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:       SingletonName,
			Generation: 1,
		},
		Status: networkingv1alpha1.CUDNBgpConfigStatus{
			ObservedGeneration: 1,
		},
	}
	baseline := config.Status.DeepCopy()

	c := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(config).WithObjects(config).Build()
	r := &CUDNBgpConfigReconciler{Client: c, Scheme: testScheme()}

	if err := r.patchConfigStatus(ctx, config, *baseline, func(c *networkingv1alpha1.CUDNBgpConfig) {
		c.Status.ObservedGeneration = c.Generation
		meta.SetStatusCondition(&c.Status.Conditions,
			cond(networkingv1alpha1.ConditionFRRNamespaceReady, metav1.ConditionTrue))
	}); err != nil {
		t.Fatalf("patchConfigStatus: %v", err)
	}

	after := &networkingv1alpha1.CUDNBgpConfig{}
	if err := c.Get(ctx, types.NamespacedName{Name: SingletonName}, after); err != nil {
		t.Fatalf("get config: %v", err)
	}
	if !meta.IsStatusConditionTrue(after.Status.Conditions, networkingv1alpha1.ConditionReady) {
		t.Fatalf("expected Ready, got %v", findCondition(after.Status.Conditions, networkingv1alpha1.ConditionReady))
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
			ObservedGeneration: 1,
		},
	}
	// The steady state this path settles into: both steps Unknown while it
	// waits on the config, and the summary that follows from them. Repeating
	// the same mutation must not write, or the ten second requeue writes
	// status every time round.
	awaitingConfig(routing)
	routing.Status.Conditions = append(routing.Status.Conditions,
		readyCondition(routing.Status.Conditions, routing.Generation))
	baseline := routing.Status.DeepCopy()

	c := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(routing).WithObjects(routing).Build()
	r := &CUDNBgpRoutingReconciler{Client: c, Scheme: testScheme()}

	before := routing.DeepCopy()
	if err := r.patchRoutingStatus(ctx, routing, *baseline, func(rt *networkingv1alpha1.CUDNBgpRouting) {
		awaitingConfig(rt)
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

// cond is shorthand for building the conditions these tests summarise.
func cond(t string, s metav1.ConditionStatus) metav1.Condition {
	return metav1.Condition{Type: t, Status: s, Reason: "Test"}
}

// TestReadyCondition summarises the step conditions into the single answer the
// API conventions ask for in place of a phase.
//
// The cases that matter are the ones a fixed list of required conditions would
// get wrong: Manual reports fewer conditions than a cloud does, and suspension
// reports Unknown rather than removing them.
func TestReadyCondition(t *testing.T) {
	cloudSteps := []metav1.Condition{
		cond(networkingv1alpha1.ConditionNetworkOperatorPatched, metav1.ConditionTrue),
		cond(networkingv1alpha1.ConditionFRRNamespaceReady, metav1.ConditionTrue),
		cond(networkingv1alpha1.ConditionRouterNodesLabelled, metav1.ConditionTrue),
		cond(networkingv1alpha1.ConditionCloudEndpointsDiscovered, metav1.ConditionTrue),
		cond(networkingv1alpha1.ConditionFRRConfigurationApplied, metav1.ConditionTrue),
		cond(networkingv1alpha1.ConditionCloudResourcesReconciled, metav1.ConditionTrue),
		cond(networkingv1alpha1.ConditionPrerequisitesSatisfied, metav1.ConditionTrue),
	}

	tests := []struct {
		name       string
		conditions []metav1.Condition
		want       metav1.ConditionStatus
		wantReason string
	}{
		{
			name:       "every step satisfied on a cloud",
			conditions: cloudSteps,
			want:       metav1.ConditionTrue,
			wantReason: ReasonAllConditionsSatisfied,
		},
		{
			// Manual builds no platform, so it never reports the three cloud
			// conditions. A fixed list of required conditions would hold it
			// permanently not ready.
			name: "every step satisfied on Manual, which reports fewer",
			conditions: []metav1.Condition{
				cond(networkingv1alpha1.ConditionNetworkOperatorPatched, metav1.ConditionTrue),
				cond(networkingv1alpha1.ConditionFRRNamespaceReady, metav1.ConditionTrue),
				cond(networkingv1alpha1.ConditionRouterNodesLabelled, metav1.ConditionTrue),
				cond(networkingv1alpha1.ConditionFRRConfigurationApplied, metav1.ConditionTrue),
			},
			want:       metav1.ConditionTrue,
			wantReason: ReasonAllConditionsSatisfied,
		},
		{
			name: "a step failed",
			conditions: append(append([]metav1.Condition(nil), cloudSteps[:6]...),
				cond(networkingv1alpha1.ConditionPrerequisitesSatisfied, metav1.ConditionFalse)),
			want:       metav1.ConditionFalse,
			wantReason: ReasonConditionsNotSatisfied,
		},
		{
			// Unknown says the controller is not observing the thing, which is
			// not the same as it being fine.
			name: "a step is not being observed",
			conditions: append(append([]metav1.Condition(nil), cloudSteps[:6]...),
				cond(networkingv1alpha1.ConditionPrerequisitesSatisfied, metav1.ConditionUnknown)),
			want:       metav1.ConditionFalse,
			wantReason: ReasonConditionsNotSatisfied,
		},
		{
			// Suspended is negative polarity: True is the abnormal state, and
			// a suspended configuration is deliberately not running.
			name:       "suspended",
			conditions: append(append([]metav1.Condition(nil), cloudSteps...), cond(networkingv1alpha1.ConditionSuspended, metav1.ConditionTrue)),
			want:       metav1.ConditionFalse,
			wantReason: ReasonSuspended,
		},
		{
			// Not suspended is the normal state for a negative condition, and
			// must not be read as something being wrong.
			name:       "explicitly not suspended",
			conditions: append(append([]metav1.Condition(nil), cloudSteps...), cond(networkingv1alpha1.ConditionSuspended, metav1.ConditionFalse)),
			want:       metav1.ConditionTrue,
			wantReason: ReasonAllConditionsSatisfied,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := readyCondition(tc.conditions, 7)
			if got.Type != networkingv1alpha1.ConditionReady {
				t.Errorf("type: got %q, want %q", got.Type, networkingv1alpha1.ConditionReady)
			}
			if got.Status != tc.want {
				t.Errorf("status: got %s, want %s (message %q)", got.Status, tc.want, got.Message)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("reason: got %q, want %q", got.Reason, tc.wantReason)
			}
			if got.ObservedGeneration != 7 {
				t.Errorf("observedGeneration: got %d, want 7", got.ObservedGeneration)
			}
		})
	}
}

// TestReadyCondition_NamesWhatIsUnsatisfied checks the message is actionable
// and stable, since an unstable one re-emits an event on every resync.
func TestReadyCondition_NamesWhatIsUnsatisfied(t *testing.T) {
	got := readyCondition([]metav1.Condition{
		cond(networkingv1alpha1.ConditionFRRConfigurationApplied, metav1.ConditionFalse),
		cond(networkingv1alpha1.ConditionNetworkOperatorPatched, metav1.ConditionTrue),
		cond(networkingv1alpha1.ConditionCloudEndpointsDiscovered, metav1.ConditionFalse),
	}, 1)

	if !strings.Contains(got.Message, networkingv1alpha1.ConditionCloudEndpointsDiscovered) ||
		!strings.Contains(got.Message, networkingv1alpha1.ConditionFRRConfigurationApplied) {
		t.Errorf("message should name every unsatisfied condition, got %q", got.Message)
	}
	// Sorted, so two reconciles that find the same problems say the same thing.
	if i, j := strings.Index(got.Message, networkingv1alpha1.ConditionCloudEndpointsDiscovered),
		strings.Index(got.Message, networkingv1alpha1.ConditionFRRConfigurationApplied); i > j {
		t.Errorf("unsatisfied conditions should be sorted, got %q", got.Message)
	}
}

// TestReadyCondition_NothingReportedYet pins the vacuous case. A configuration
// that has reported no conditions has not done anything, and summarising an
// empty set as satisfied would report Ready for work that has not happened.
func TestReadyCondition_NothingReportedYet(t *testing.T) {
	got := readyCondition(nil, 3)
	if got.Status != metav1.ConditionUnknown {
		t.Errorf("status: got %s, want Unknown (message %q)", got.Status, got.Message)
	}
	if got.Reason != ReasonReconciling {
		t.Errorf("reason: got %q, want %q", got.Reason, ReasonReconciling)
	}
}
