package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

func machineScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(machineGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(machineListGVK, &unstructured.UnstructuredList{})
	return s
}

// machineRouterNode builds a router node whose provider ID is a GCE one. The
// scheme is arbitrary: this handling keys on spec.providerID as an opaque
// string and never parses it, which is the property that let it move out of a
// cloud package. TestHoldTerminating_ProviderIDSchemeIsOpaque pins that.
func machineRouterNode(name, ip, zone string) platform.RouterNode {
	return platform.RouterNode{
		Name:       name,
		PrivateIP:  ip,
		Zone:       zone,
		ProviderID: "gce://proj/" + zone + "/" + name,
	}
}

// newMachine builds a Machine, optionally already carrying our hook and
// optionally already being deleted.
func newMachine(name, providerID string, withHook, deleting bool) *unstructured.Unstructured {
	m := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "machine.openshift.io/v1beta1",
		"kind":       "Machine",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": "openshift-machine-api",
		},
		"spec": map[string]interface{}{
			"providerID": providerID,
		},
	}}
	var hooks []interface{}
	// An unrelated owner's hook, to prove we never disturb it.
	hooks = append(hooks, map[string]interface{}{"name": "someone-else", "owner": "OtherOperator"})
	if withHook {
		hooks = append(hooks, map[string]interface{}{"name": LifecycleHookName, "owner": LifecycleHookOwner})
	}
	_ = unstructured.SetNestedSlice(m.Object, hooks, "spec", "lifecycleHooks", "preTerminate")
	if deleting {
		now := metav1.Now()
		m.SetDeletionTimestamp(&now)
		m.SetFinalizers([]string{"machine.openshift.io/machine"})
	}
	return m
}

// machineClient is a fake client carrying the given Machines. There is no
// platform involved any more: this handling is Machine API, not cloud.
func machineClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(machineScheme()).WithObjects(objs...).Build()
}

func hookNames(t *testing.T, c client.Client, name string) []string {
	t.Helper()
	m := &unstructured.Unstructured{}
	m.SetGroupVersionKind(machineGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: "openshift-machine-api"}, m); err != nil {
		t.Fatalf("get machine %s: %v", name, err)
	}
	var names []string
	for _, h := range preTerminateHooks(m) {
		hm, _ := h.(map[string]interface{})
		if s, ok := hm["name"].(string); ok {
			names = append(names, s)
		}
	}
	return names
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestHoldTerminating_AddsHookToRouterMachine covers the steady state: a live
// router node gets the hook so a later deletion is gated.
func TestHoldTerminating_AddsHookToRouterMachine(t *testing.T) {
	m := newMachine("worker-a", "gce://proj/europe-west1-a/worker-a", false, false)
	c := machineClient(t, m)

	held, err := HoldTerminatingRouterNodes(context.Background(), c, []platform.RouterNode{machineRouterNode("worker-a", "10.0.0.2", "europe-west1-a")})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(held) != 0 {
		t.Errorf("nothing is terminating, expected no held nodes, got %+v", held)
	}

	names := hookNames(t, c, "worker-a")
	if !contains(names, LifecycleHookName) {
		t.Errorf("expected our hook to be added, got %v", names)
	}
	if !contains(names, "someone-else") {
		t.Errorf("another owner's hook must survive, got %v", names)
	}
}

// TestHoldTerminating_ReportsDeletingMachine is the case the interface exists
// for: a router node being destroyed is reported so its peers come out first.
func TestHoldTerminating_ReportsDeletingMachine(t *testing.T) {
	live := newMachine("worker-a", "gce://proj/europe-west1-a/worker-a", true, false)
	dying := newMachine("worker-b", "gce://proj/europe-west1-b/worker-b", true, true)
	c := machineClient(t, live, dying)

	held, err := HoldTerminatingRouterNodes(context.Background(), c, []platform.RouterNode{
		machineRouterNode("worker-a", "10.0.0.2", "europe-west1-a"),
		machineRouterNode("worker-b", "10.0.0.3", "europe-west1-b"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(held) != 1 || held[0].Name != "worker-b" {
		t.Fatalf("expected only worker-b held, got %+v", held)
	}
}

// TestHoldTerminating_RemovesHookFromNonRouter stops a node that has left the
// router set from being blocked from deletion forever.
func TestHoldTerminating_RemovesHookFromNonRouter(t *testing.T) {
	m := newMachine("worker-z", "gce://proj/europe-west1-c/worker-z", true, false)
	c := machineClient(t, m)

	if _, err := HoldTerminatingRouterNodes(context.Background(), c, []platform.RouterNode{machineRouterNode("worker-a", "10.0.0.2", "europe-west1-a")}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	names := hookNames(t, c, "worker-z")
	if contains(names, LifecycleHookName) {
		t.Errorf("expected our hook removed from a non-router machine, got %v", names)
	}
	if !contains(names, "someone-else") {
		t.Errorf("another owner's hook must survive, got %v", names)
	}
}

func TestReleaseTerminating_RemovesHook(t *testing.T) {
	m := newMachine("worker-b", "gce://proj/europe-west1-b/worker-b", true, true)
	c := machineClient(t, m)

	if err := ReleaseTerminatingRouterNodes(context.Background(), c, []platform.RouterNode{machineRouterNode("worker-b", "10.0.0.3", "europe-west1-b")}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	names := hookNames(t, c, "worker-b")
	if contains(names, LifecycleHookName) {
		t.Errorf("expected our hook released, got %v", names)
	}
	if !contains(names, "someone-else") {
		t.Errorf("another owner's hook must survive, got %v", names)
	}
}

// TestReleaseTerminating_MissingMachine covers a Machine that finished
// deleting between the hold and the release.
func TestReleaseTerminating_MissingMachine(t *testing.T) {
	c := machineClient(t)

	if err := ReleaseTerminatingRouterNodes(context.Background(), c, []platform.RouterNode{machineRouterNode("gone", "10.0.0.9", "europe-west1-a")}); err != nil {
		t.Fatalf("a machine that has already gone is not an error: %v", err)
	}
}

// TestHoldTerminating_IsIdempotent proves a second pass over an already hooked
// machine issues no further change.
func TestHoldTerminating_IsIdempotent(t *testing.T) {
	m := newMachine("worker-a", "gce://proj/europe-west1-a/worker-a", true, false)
	c := machineClient(t, m)

	nodes := []platform.RouterNode{machineRouterNode("worker-a", "10.0.0.2", "europe-west1-a")}
	if _, err := HoldTerminatingRouterNodes(context.Background(), c, nodes); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	before := &unstructured.Unstructured{}
	before.SetGroupVersionKind(machineGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Name: "worker-a", Namespace: "openshift-machine-api"}, before); err != nil {
		t.Fatalf("get: %v", err)
	}
	rv := before.GetResourceVersion()

	if _, err := HoldTerminatingRouterNodes(context.Background(), c, nodes); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	after := &unstructured.Unstructured{}
	after.SetGroupVersionKind(machineGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Name: "worker-a", Namespace: "openshift-machine-api"}, after); err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.GetResourceVersion() != rv {
		t.Errorf("expected no write on an unchanged machine, resourceVersion moved %s -> %s", rv, after.GetResourceVersion())
	}
}

// TestHoldTerminating_ProviderIDSchemeIsOpaque is why this handling belongs in
// the controller rather than in a cloud package. The same code must gate an
// AWS, Azure or GCP Machine, because all it does is match spec.providerID as a
// string and patch a hook.
func TestHoldTerminating_ProviderIDSchemeIsOpaque(t *testing.T) {
	for _, tc := range []struct {
		cloud      string
		providerID string
	}{
		{"aws", "aws:///us-east-2b/i-0abc123"},
		{"azure", "azure:///subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/worker"},
		{"gce", "gce://proj/us-east1-b/worker"},
	} {
		t.Run(tc.cloud, func(t *testing.T) {
			c := machineClient(t, newMachine("worker", tc.providerID, false, false))

			node := platform.RouterNode{Name: "worker", PrivateIP: "10.0.0.2", ProviderID: tc.providerID}
			if _, err := HoldTerminatingRouterNodes(context.Background(), c, []platform.RouterNode{node}); err != nil {
				t.Fatalf("HoldTerminatingRouterNodes: %v", err)
			}
			if !contains(hookNames(t, c, "worker"), LifecycleHookName) {
				t.Errorf("no hook placed on a %s Machine", tc.cloud)
			}

			if err := ReleaseTerminatingRouterNodes(context.Background(), c, []platform.RouterNode{node}); err != nil {
				t.Fatalf("ReleaseTerminatingRouterNodes: %v", err)
			}
			if contains(hookNames(t, c, "worker"), LifecycleHookName) {
				t.Errorf("hook left on a %s Machine", tc.cloud)
			}
		})
	}
}
