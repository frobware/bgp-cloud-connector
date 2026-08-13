//go:build gcplive

// Machine lifecycle hooks against a real API server. The fake client does not
// implement merge-patch the way the apiserver does, so the patch shape in
// machines.go can only be proven here.
//
//	KUBECONFIG=<cluster>/auth/kubeconfig \
//	go test -tags gcplive ./internal/platform/gcp/ -run TestLiveMachines -v
//
// This mutates real Machine objects: it adds our preTerminate hook and removes
// it again. A preTerminate hook blocks Machine deletion while present, so the
// removal is deferred rather than left to the end of the test body.
package gcp

import (
	"context"
	"os"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

func liveK8s(t *testing.T) client.Client {
	t.Helper()
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("KUBECONFIG must be set")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatalf("loading kubeconfig: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: machineScheme()})
	if err != nil {
		t.Fatalf("building client: %v", err)
	}
	return c
}

func liveMachines(t *testing.T, c client.Client) []unstructured.Unstructured {
	t.Helper()
	var list unstructured.UnstructuredList
	list.SetGroupVersionKind(machineListGVK)
	if err := c.List(context.Background(), &list, client.InNamespace("openshift-machine-api")); err != nil {
		t.Fatalf("listing machines: %v", err)
	}
	return list.Items
}

// TestLiveMachines_ProviderIDsParse feeds every real provider ID on the
// cluster through the parser. A format the parser rejects would take the whole
// GCP platform down, and the format is not something the fakes can vouch for.
func TestLiveMachines_ProviderIDsParse(t *testing.T) {
	c := liveK8s(t)

	items := liveMachines(t, c)
	if len(items) == 0 {
		t.Skip("no machines on this cluster")
	}

	step(t, "parsing the provider ID of all %d Machines in openshift-machine-api", len(items))
	for i := range items {
		m := &items[i]
		providerID, found, err := unstructured.NestedString(m.Object, "spec", "providerID")
		if err != nil || !found || providerID == "" {
			observed(t, "%s has no providerID yet, skipping", m.GetName())
			continue
		}
		inst, err := ParseProviderID(providerID)
		if err != nil {
			t.Errorf("%s: %v", m.GetName(), err)
			continue
		}
		if inst.Name != m.GetName() {
			t.Errorf("%s: parsed instance name %q does not match the Machine name", m.GetName(), inst.Name)
		}
		observed(t, "%s -> zone=%s instance=%s", providerID, inst.Zone, inst.Name)
	}
	step(t, "every provider ID parsed and named its own Machine")
}

// TestLiveMachines_HookRoundTrip proves the merge patch adds and removes only
// our hook against a real apiserver.
func TestLiveMachines_HookRoundTrip(t *testing.T) {
	c := liveK8s(t)
	ctx := context.Background()

	items := liveMachines(t, c)
	var target *unstructured.Unstructured
	var providerID string
	for i := range items {
		m := &items[i]
		if m.GetDeletionTimestamp() != nil {
			continue
		}
		pid, found, _ := unstructured.NestedString(m.Object, "spec", "providerID")
		if found && pid != "" {
			target, providerID = m, pid
			break
		}
	}
	if target == nil {
		t.Skip("no live machine with a providerID")
	}
	name := target.GetName()
	step(t, "using Machine %s", name)

	cfg := testConfig()
	cfg.MachineNamespace = "openshift-machine-api"
	p := NewWithClients(cfg, &fakeCompute{}, &fakeNCC{}, c)

	node := platform.RouterNode{Name: name, PrivateIP: "10.0.0.1", ProviderID: providerID}

	// Always take the hook off again, however this test exits: leaving one
	// behind would block deletion of a real machine.
	defer func() {
		step(t, "releasing the hook, however this test exited")
		if err := p.ReleaseTerminating(context.Background(), []platform.RouterNode{node}); err != nil {
			t.Errorf("cleanup: %v", err)
		}
		if hasHookLive(t, c, name) {
			t.Errorf("cleanup left our hook on %s", name)
		}
		observed(t, "hooks now %v", hookNamesLive(t, c, name))
	}()

	before := hookNamesLive(t, c, name)
	observed(t, "hooks before %v", before)

	step(t, "holding: HoldTerminating should add %s", LifecycleHookName)
	if _, err := p.HoldTerminating(ctx, []platform.RouterNode{node}); err != nil {
		t.Fatalf("HoldTerminating: %v", err)
	}

	after := hookNamesLive(t, c, name)
	observed(t, "hooks after  %v", after)

	step(t, "checking our hook is present")
	if !containsLive(after, LifecycleHookName) {
		t.Fatalf("expected our hook on %s, got %v", name, after)
	}

	step(t, "checking another owner's hooks on the same Machine survived")
	for _, h := range before {
		if !containsLive(after, h) {
			t.Errorf("hook %q owned by someone else was lost", h)
		}
	}

	step(t, "checking a second pass writes nothing, so a resync does not churn the Machine")
	rv := resourceVersionLive(t, c, name)
	if _, err := p.HoldTerminating(ctx, []platform.RouterNode{node}); err != nil {
		t.Fatalf("second HoldTerminating: %v", err)
	}
	got := resourceVersionLive(t, c, name)
	if got != rv {
		t.Errorf("expected no write on an unchanged machine, resourceVersion moved %s -> %s", rv, got)
	}
	observed(t, "resourceVersion unchanged at %s", got)
}

func getMachineLive(t *testing.T, c client.Client, name string) *unstructured.Unstructured {
	t.Helper()
	m := &unstructured.Unstructured{}
	m.SetGroupVersionKind(machineGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: "openshift-machine-api"}, m); err != nil {
		t.Fatalf("get machine %s: %v", name, err)
	}
	return m
}

func hookNamesLive(t *testing.T, c client.Client, name string) []string {
	t.Helper()
	var names []string
	for _, h := range preTerminateHooks(getMachineLive(t, c, name)) {
		hm, _ := h.(map[string]interface{})
		if s, ok := hm["name"].(string); ok {
			names = append(names, s)
		}
	}
	return names
}

func hasHookLive(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	return containsLive(hookNamesLive(t, c, name), LifecycleHookName)
}

func resourceVersionLive(t *testing.T, c client.Client, name string) string {
	t.Helper()
	return getMachineLive(t, c, name).GetResourceVersion()
}

func containsLive(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
