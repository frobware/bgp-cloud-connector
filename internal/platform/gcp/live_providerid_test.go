//go:build gcplive

// Provider ID parsing against a real cluster's Machines.
//
//	KUBECONFIG=<cluster>/auth/kubeconfig \
//	GCP_PROJECT=openshift-qe GCP_REGION=us-east1 \
//	GCP_CLOUD_ROUTER=<infra>-bgp-router \
//	go test -tags gcplive ./internal/platform/gcp/ -run TestGCPLive_ProviderIDsParse -v
//
// Read-only: it lists Machines and parses what they carry.
package gcp

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// machineListGVK is declared here rather than shared with the controller,
// which keeps its own copy unexported. Machine handling there keys on
// spec.providerID as an opaque string and never parses it; the parsing is
// GCP's, so the test that exercises it belongs on this side of the interface.
var machineListGVK = schema.GroupVersionKind{
	Group:   "machine.openshift.io",
	Version: "v1beta1",
	Kind:    "MachineList",
}

// TestGCPLive_ProviderIDsParse feeds every real provider ID on the cluster
// through the parser. A format the parser rejects would take the whole GCP
// platform down, and the format is not something the fakes can vouch for.
func TestGCPLive_ProviderIDsParse(t *testing.T) {
	restCfg := liveRESTConfig(t)

	s := runtime.NewScheme()
	s.AddKnownTypeWithName(machineListGVK, &unstructured.UnstructuredList{})
	c, err := client.New(restCfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatalf("building machine client: %v", err)
	}

	var list unstructured.UnstructuredList
	list.SetGroupVersionKind(machineListGVK)
	if err := c.List(context.Background(), &list, client.InNamespace("openshift-machine-api")); err != nil {
		t.Fatalf("listing machines: %v", err)
	}
	if len(list.Items) == 0 {
		t.Skip("no machines on this cluster")
	}

	step(t, "parsing the provider ID of all %d Machines in openshift-machine-api", len(list.Items))
	for i := range list.Items {
		m := &list.Items[i]
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
