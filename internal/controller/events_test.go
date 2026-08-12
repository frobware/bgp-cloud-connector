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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	networkingv1alpha1 "github.com/openshift/bgp-cloud-connector/api/v1alpha1"
	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

// Conditions say what is true now. Events say what changed, which is what
// oc describe surfaces and what a human looks at first.

// drain empties the recorder without blocking, so a test can assert on what
// was emitted during one reconcile.
func drain(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func eventTestEnv(t *testing.T, config *networkingv1alpha1.CUDNBgpConfig) (client.Client, *record.FakeRecorder) {
	t.Helper()
	s := configTestScheme()

	network := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "operator.openshift.io/v1",
			"kind":       "Network",
			"metadata":   map[string]interface{}{"name": "cluster"},
			"spec":       map[string]interface{}{},
		},
	}
	frrNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: FRRNamespace}}
	frrPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "frr-k8s-pod", Namespace: FRRNamespace, Labels: map[string]string{"app": "frr-k8s"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config, network, frrNS, frrPod).
		WithStatusSubresource(config).
		Build()

	return c, record.NewFakeRecorder(32)
}

// TestEvents_EmittedOnFirstReconcile: reaching a state for the first time is
// a transition, so the conditions that got there should be announced.
func TestEvents_EmittedOnFirstReconcile(t *testing.T) {
	config := newTestCUDNBgpConfig()
	c, rec := eventTestEnv(t, config)

	r := &CUDNBgpConfigReconciler{Client: c, Scheme: configTestScheme(), Recorder: rec}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	events := drain(rec)
	if len(events) == 0 {
		t.Fatal("expected events for the conditions reached on the first pass")
	}
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, networkingv1alpha1.ConditionFRRConfigurationApplied) {
		t.Errorf("expected an event naming %s, got:\n%s", networkingv1alpha1.ConditionFRRConfigurationApplied, joined)
	}
}

// TestEvents_SilentWhenNothingChanges is the one that matters. The controller
// resyncs every five minutes; emitting the same events each time would bury
// the transition that mattered under identical noise.
func TestEvents_SilentWhenNothingChanges(t *testing.T) {
	config := newTestCUDNBgpConfig()
	c, rec := eventTestEnv(t, config)

	r := &CUDNBgpConfigReconciler{Client: c, Scheme: configTestScheme(), Recorder: rec}
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	drain(rec)

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if events := drain(rec); len(events) != 0 {
		t.Errorf("a settled cluster must emit nothing, got:\n%s", strings.Join(events, "\n"))
	}
}

// TestEvents_WarningOnDegrade covers the path every failure funnels through.
func TestEvents_WarningOnDegrade(t *testing.T) {
	config := newTestCUDNBgpConfigWithAWS()
	c, rec := eventTestEnv(t, config)

	r := &CUDNBgpConfigReconciler{
		Client: c, Scheme: configTestScheme(), Recorder: rec,
		PlatformBuilder: func(_ context.Context, _ client.Client, _ *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error) {
			return nil, &platform.CredentialError{Msg: "invalid credentials"}
		},
	}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	joined := strings.Join(drain(rec), "\n")
	if !strings.Contains(joined, "Warning") {
		t.Errorf("a degraded condition should be a Warning, got:\n%s", joined)
	}
	if !strings.Contains(joined, "CloudCredentialsInvalid") {
		t.Errorf("expected the reason in the event, got:\n%s", joined)
	}
}

// TestEvents_NilRecorderIsSafe: the recorder comes from the manager, and unit
// tests that do not care should not have to supply one.
func TestEvents_NilRecorderIsSafe(t *testing.T) {
	config := newTestCUDNBgpConfig()
	c, _ := eventTestEnv(t, config)

	r := &CUDNBgpConfigReconciler{Client: c, Scheme: configTestScheme()}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}}); err != nil {
		t.Fatalf("reconcile with no recorder: %v", err)
	}
}

// TestEvents_EmittedWhenOnlyMessageChanges is the router set growing or
// shrinking. Scaling a MachineSet leaves every condition True with the same
// reason and changes only the count in the message, which is precisely the
// transition someone wants told about.
func TestEvents_EmittedWhenOnlyMessageChanges(t *testing.T) {
	rec := record.NewFakeRecorder(8)

	before := []metav1.Condition{{
		Type:    networkingv1alpha1.ConditionCloudResourcesReconciled,
		Status:  metav1.ConditionTrue,
		Reason:  "Reconciled",
		Message: "Reconciled cloud resources for 3 router node(s)",
	}}
	after := []metav1.Condition{{
		Type:    networkingv1alpha1.ConditionCloudResourcesReconciled,
		Status:  metav1.ConditionTrue,
		Reason:  "Reconciled",
		Message: "Reconciled cloud resources for 4 router node(s)",
	}}

	emitConditionEvents(rec, &networkingv1alpha1.CUDNBgpConfig{}, before, after)

	events := drain(rec)
	if len(events) != 1 {
		t.Fatalf("expected one event for the changed count, got %v", events)
	}
	if !strings.Contains(events[0], "4 router node(s)") {
		t.Errorf("expected the new count in the event, got %q", events[0])
	}
}

// TestEvents_SilentWhenIdentical guards the other side: an unchanged
// condition must stay quiet however often it is reconciled.
func TestEvents_SilentWhenIdentical(t *testing.T) {
	rec := record.NewFakeRecorder(8)
	conds := []metav1.Condition{{
		Type:    networkingv1alpha1.ConditionCloudResourcesReconciled,
		Status:  metav1.ConditionTrue,
		Reason:  "Reconciled",
		Message: "Reconciled cloud resources for 3 router node(s)",
	}}

	emitConditionEvents(rec, &networkingv1alpha1.CUDNBgpConfig{}, conds, conds)

	if events := drain(rec); len(events) != 0 {
		t.Errorf("expected silence, got %v", events)
	}
}
