package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	networkingv1alpha1 "github.com/openshift/bgp-cloud-connector/api/v1alpha1"
)

// operationalConditions are the conditions this controller reports about work
// it is actually doing, as distinct from Suspended, which reports whether it
// is doing any.
var operationalConditions = []string{
	networkingv1alpha1.ConditionNetworkOperatorPatched,
	networkingv1alpha1.ConditionFRRNamespaceReady,
	networkingv1alpha1.ConditionPrerequisitesSatisfied,
	networkingv1alpha1.ConditionCloudEndpointsDiscovered,
	networkingv1alpha1.ConditionFRRConfigurationApplied,
	networkingv1alpha1.ConditionCloudResourcesReconciled,
}

func setSuspended(t *testing.T, r *CUDNBgpConfigReconciler, suspended bool) {
	t.Helper()
	config := reloadConfig(t, r)
	config.Spec.Suspended = suspended
	if err := r.Update(context.Background(), config); err != nil {
		t.Fatalf("setting suspended=%v: %v", suspended, err)
	}
	if err := reconcileOnce(t, r); err != nil {
		t.Fatalf("reconcile with suspended=%v: %v", suspended, err)
	}
}

// TestResume_ClearsTheSuspendedCondition covers the state a resumed config is
// left in.
//
// The API conventions are explicit that absence of a condition means Unknown
// rather than False, so a controller cannot signal "not suspended" by removing
// the condition: it has to say so. Leaving a stale Suspended=True behind means
// the resource claims to be paused while it reconciles, and anything reading
// that condition believes it.
func TestResume_ClearsTheSuspendedCondition(t *testing.T) {
	r, _ := suspendFixture(t, &mockPlatform{})

	setSuspended(t, r, true)
	if cond := findCondition(reloadConfig(t, r).Status.Conditions, networkingv1alpha1.ConditionSuspended); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("precondition: expected Suspended=True while suspended, got %+v", cond)
	}

	setSuspended(t, r, false)

	updated := reloadConfig(t, r)
	cond := findCondition(updated.Status.Conditions, networkingv1alpha1.ConditionSuspended)
	if cond == nil {
		t.Fatal("Suspended is absent after resuming; absence reads as Unknown, not False")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("Suspended=%s after resuming, want False", cond.Status)
	}
	if cond.ObservedGeneration != updated.Generation {
		t.Errorf("Suspended observedGeneration = %d, want %d: a stale generation is how this went unnoticed",
			cond.ObservedGeneration, updated.Generation)
	}
}

// TestSuspend_ReportsOperationalConditionsUnknown covers what happens to the
// rest of the conditions while suspended.
//
// Deleting them loses information other components were told they could rely
// on -- a controller applies its conditions on first visit precisely so that
// consumers know they exist. While suspended the controller is not observing
// FRR or the cloud, so True would be a claim it cannot make and False would be
// a different one. Unknown is what that state is for.
func TestSuspend_ReportsOperationalConditionsUnknown(t *testing.T) {
	r, _ := suspendFixture(t, &mockPlatform{})

	setSuspended(t, r, true)

	updated := reloadConfig(t, r)
	for _, condType := range operationalConditions {
		cond := findCondition(updated.Status.Conditions, condType)
		if cond == nil {
			t.Errorf("%s was deleted while suspended; it should report Unknown", condType)
			continue
		}
		if cond.Status != metav1.ConditionUnknown {
			t.Errorf("%s=%s while suspended, want Unknown", condType, cond.Status)
		}
	}
}

// TestResume_RestoresOperationalConditions is the other half: once running
// again, the conditions have to report what is actually observed rather than
// staying Unknown.
func TestResume_RestoresOperationalConditions(t *testing.T) {
	r, _ := suspendFixture(t, &mockPlatform{})

	setSuspended(t, r, true)
	setSuspended(t, r, false)

	updated := reloadConfig(t, r)
	for _, condType := range operationalConditions {
		cond := findCondition(updated.Status.Conditions, condType)
		if cond == nil {
			t.Errorf("%s is absent after resuming", condType)
			continue
		}
		if cond.Status == metav1.ConditionUnknown {
			t.Errorf("%s is still Unknown after resuming", condType)
		}
		if cond.ObservedGeneration != updated.Generation {
			t.Errorf("%s observedGeneration = %d, want %d", condType, cond.ObservedGeneration, updated.Generation)
		}
	}
}
