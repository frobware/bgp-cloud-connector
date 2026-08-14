package azure

import (
	"context"
	"errors"
	"testing"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

type fakeNICs struct {
	nics      []NIC
	listErr   error
	enabled   []string
	enableErr error
}

func (f *fakeNICs) ListNICs(_ context.Context, resourceGroup string) ([]NIC, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []NIC
	for _, n := range f.nics {
		if n.ResourceGroup == resourceGroup {
			out = append(out, n)
		}
	}
	return out, nil
}

func (f *fakeNICs) EnableIPForwarding(_ context.Context, _, name string) error {
	if f.enableErr != nil {
		return f.enableErr
	}
	f.enabled = append(f.enabled, name)
	return nil
}

const (
	vmID   = "/subscriptions/sub-1/resourceGroups/rg-1/providers/Microsoft.Compute/virtualMachines/worker-a"
	nodeID = "azure://" + vmID
)

// TestVMResourceID covers the one piece of string handling here. A providerID
// that does not parse must be an error rather than an empty resource group,
// because an empty group would list the wrong NICs and silently enable
// forwarding on nothing.
func TestVMResourceID(t *testing.T) {
	for _, tc := range []struct {
		name       string
		providerID string
		wantRG     string
		wantID     string
		wantErr    bool
	}{
		{"azure providerID", nodeID, "rg-1", vmID, false},
		{"already a bare resource id", vmID, "rg-1", vmID, false},
		{"mixed case group, as ARM returns", "azure:///subscriptions/s/resourceGroups/RG-One/providers/Microsoft.Compute/virtualMachines/vm", "RG-One", "/subscriptions/s/resourceGroups/RG-One/providers/Microsoft.Compute/virtualMachines/vm", false},
		{"empty", "", "", "", true},
		{"another cloud", "aws:///us-east-1a/i-0abc", "", "", true},
		{"no resource group", "azure:///subscriptions/s/providers/Microsoft.Compute/virtualMachines/vm", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rg, id, err := vmResourceID(tc.providerID)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got rg=%q id=%q", rg, id)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if rg != tc.wantRG || id != tc.wantID {
				t.Errorf("got rg=%q id=%q, want rg=%q id=%q", rg, id, tc.wantRG, tc.wantID)
			}
		})
	}
}

// TestEnsureNodesCanForward is the substance: Azure discards a packet whose
// destination is not the NIC's own address unless the NIC says it forwards, so
// a router node without it drops every packet addressed to a pod while every
// other signal stays healthy.
func TestEnsureNodesCanForward(t *testing.T) {
	node := platform.RouterNode{Name: "worker-a", PrivateIP: "10.0.128.4", ProviderID: nodeID}

	t.Run("enables a NIC that is not forwarding", func(t *testing.T) {
		nics := &fakeNICs{nics: []NIC{
			{Name: "worker-a-nic", ResourceGroup: "rg-1", VMID: vmID, IPForwarding: false},
		}}
		p := &Platform{cfg: testConfig(), nics: nics}
		if err := p.ensureNodesCanForward(context.Background(), []platform.RouterNode{node}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(nics.enabled) != 1 || nics.enabled[0] != "worker-a-nic" {
			t.Errorf("enabled = %v, want [worker-a-nic]", nics.enabled)
		}
	})

	// Both controllers watch what they write, so rewriting a NIC that is
	// already correct would feed a reconcile loop.
	t.Run("leaves a NIC that already forwards alone", func(t *testing.T) {
		nics := &fakeNICs{nics: []NIC{
			{Name: "worker-a-nic", ResourceGroup: "rg-1", VMID: vmID, IPForwarding: true},
		}}
		p := &Platform{cfg: testConfig(), nics: nics}
		if err := p.ensureNodesCanForward(context.Background(), []platform.RouterNode{node}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(nics.enabled) != 0 {
			t.Errorf("enabled = %v, want nothing rewritten", nics.enabled)
		}
	})

	// A shared resource group holds every node's NIC and plenty besides.
	// Matching on the VM the NIC is attached to is the only thing that keeps
	// this from enabling forwarding across the cluster.
	t.Run("touches only NICs attached to the node's VM", func(t *testing.T) {
		other := "/subscriptions/sub-1/resourceGroups/rg-1/providers/Microsoft.Compute/virtualMachines/worker-b"
		nics := &fakeNICs{nics: []NIC{
			{Name: "worker-a-nic", ResourceGroup: "rg-1", VMID: vmID},
			{Name: "worker-b-nic", ResourceGroup: "rg-1", VMID: other},
			{Name: "detached-nic", ResourceGroup: "rg-1", VMID: ""},
		}}
		p := &Platform{cfg: testConfig(), nics: nics}
		if err := p.ensureNodesCanForward(context.Background(), []platform.RouterNode{node}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(nics.enabled) != 1 || nics.enabled[0] != "worker-a-nic" {
			t.Errorf("enabled = %v, want only [worker-a-nic]", nics.enabled)
		}
	})

	// ARM returns resource ids with whatever casing was used to create them,
	// and a node's providerID is not guaranteed to match a NIC's reference
	// character for character.
	t.Run("matches the VM id case-insensitively", func(t *testing.T) {
		nics := &fakeNICs{nics: []NIC{
			{Name: "worker-a-nic", ResourceGroup: "rg-1",
				VMID: "/subscriptions/SUB-1/resourceGroups/RG-1/providers/Microsoft.Compute/virtualMachines/worker-a"},
		}}
		p := &Platform{cfg: testConfig(), nics: nics}
		if err := p.ensureNodesCanForward(context.Background(), []platform.RouterNode{node}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(nics.enabled) != 1 {
			t.Errorf("enabled = %v, want the NIC matched despite casing", nics.enabled)
		}
	})

	// A node whose VM has no NIC we can see is worth reporting rather than
	// passing over: it is the state that produces a silently broken datapath.
	t.Run("reports a node with no matching NIC", func(t *testing.T) {
		nics := &fakeNICs{nics: []NIC{
			{Name: "somebody-elses-nic", ResourceGroup: "rg-1", VMID: "/subscriptions/s/resourceGroups/rg-1/providers/Microsoft.Compute/virtualMachines/other"},
		}}
		p := &Platform{cfg: testConfig(), nics: nics}
		err := p.ensureNodesCanForward(context.Background(), []platform.RouterNode{node})
		if err == nil {
			t.Fatal("expected an error naming the node with no NIC")
		}
	})

	// The same node set desiredPeerings acts on. A node part way through
	// registering has no address, will not become a peer, and so has nothing
	// routed through it -- and failing on one would block the reconcile for
	// every other node in the cluster.
	t.Run("skips a node with no address, whatever its providerID", func(t *testing.T) {
		nics := &fakeNICs{}
		p := &Platform{cfg: testConfig(), nics: nics}
		half := platform.RouterNode{Name: "worker-b", ProviderID: "azure:///x"}
		if err := p.ensureNodesCanForward(context.Background(), []platform.RouterNode{half}); err != nil {
			t.Fatalf("a node still coming up must not fail the reconcile: %v", err)
		}
		if len(nics.enabled) != 0 {
			t.Errorf("enabled = %v, want nothing", nics.enabled)
		}
	})

	t.Run("a bad providerID is an error, not a silent skip", func(t *testing.T) {
		nics := &fakeNICs{}
		p := &Platform{cfg: testConfig(), nics: nics}
		// With an address, so it gets past the still-coming-up skip above and
		// the providerID is actually parsed.
		bad := platform.RouterNode{Name: "worker-a", PrivateIP: "10.0.128.4", ProviderID: "gce://project/zone/instance"}
		if err := p.ensureNodesCanForward(context.Background(), []platform.RouterNode{bad}); err == nil {
			t.Fatal("expected an error for a non-Azure providerID")
		}
	})

	t.Run("propagates a list failure", func(t *testing.T) {
		nics := &fakeNICs{listErr: errors.New("boom")}
		p := &Platform{cfg: testConfig(), nics: nics}
		if err := p.ensureNodesCanForward(context.Background(), []platform.RouterNode{node}); err == nil {
			t.Fatal("expected the list error to surface")
		}
	})
}
