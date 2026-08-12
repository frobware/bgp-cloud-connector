package gcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

// --- Fakes ---

type fakeCompute struct {
	topology    *CloudRouterTopology
	topologyErr error

	canIPForwardCalls []string
	nestedVirtCalls   []string

	peersNodes  []RouterNode
	peersASN    int64
	peersCalled bool
	clearCalled bool
}

func (f *fakeCompute) EnsureCanIPForward(_ context.Context, node RouterNode) (bool, error) {
	f.canIPForwardCalls = append(f.canIPForwardCalls, node.Name)
	return true, nil
}

func (f *fakeCompute) EnsureNestedVirtualization(_ context.Context, node RouterNode) (bool, error) {
	f.nestedVirtCalls = append(f.nestedVirtCalls, node.Name)
	return true, nil
}

func (f *fakeCompute) GetRouterTopology(_ context.Context, _ string) (*CloudRouterTopology, error) {
	if f.topologyErr != nil {
		return nil, f.topologyErr
	}
	return f.topology, nil
}

func (f *fakeCompute) ReconcilePeers(_ context.Context, _, _ string, nodes []RouterNode, _ *CloudRouterTopology, localASN int64) (bool, error) {
	f.peersCalled = true
	f.peersNodes = nodes
	f.peersASN = localASN
	return true, nil
}

func (f *fakeCompute) ClearPeers(_ context.Context, _, _ string) (bool, error) {
	f.clearCalled = true
	return true, nil
}

type fakeNCC struct {
	existing []string

	reconciled map[string][]RouterNode
	deleted    []string
}

func (f *fakeNCC) ReconcileSpoke(_ context.Context, spokeID, _ string, nodes []RouterNode, _ bool) (bool, error) {
	if f.reconciled == nil {
		f.reconciled = map[string][]RouterNode{}
	}
	f.reconciled[spokeID] = nodes
	return true, nil
}

func (f *fakeNCC) DeleteSpoke(_ context.Context, spokeID string) (bool, error) {
	f.deleted = append(f.deleted, spokeID)
	return true, nil
}

func (f *fakeNCC) ListSpokesByPrefix(_ context.Context, _, _ string) ([]string, error) {
	return f.existing, nil
}

func testConfig() Config {
	return Config{
		Project:         "proj",
		Region:          "europe-west1",
		CloudRouterName: "router",
		NCCHubName:      "hub",
		NCCSpokePrefix:  "spoke",
		NestedVirt:      true,
		LocalASN:        65003,
		ClusterID:       "cz-demo1",
	}
}

func node(name, ip, zone string) platform.RouterNode {
	return platform.RouterNode{
		Name:       name,
		PrivateIP:  ip,
		AZ:         zone,
		ProviderID: "gce://proj/" + zone + "/" + name,
	}
}

// --- Provider ID ---

func TestParseProviderID(t *testing.T) {
	inst, err := ParseProviderID("gce://my-proj/europe-west1-b/worker-0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if inst.Project != "my-proj" || inst.Zone != "europe-west1-b" || inst.Name != "worker-0" {
		t.Errorf("unexpected instance: %+v", inst)
	}
	want := "https://www.googleapis.com/compute/v1/projects/my-proj/zones/europe-west1-b/instances/worker-0"
	if inst.SelfLink != want {
		t.Errorf("selfLink = %q, want %q", inst.SelfLink, want)
	}
}

func TestParseProviderID_Invalid(t *testing.T) {
	for _, in := range []string{
		"aws:///us-east-1a/i-0abc",
		"gce://proj/zone",
		"gce://",
		"",
	} {
		t.Run(in, func(t *testing.T) {
			if _, err := ParseProviderID(in); err == nil {
				t.Errorf("expected an error for %q", in)
			}
		})
	}
}

// --- Discovery ---

// TestDiscoverEndpoints_SingleGroup pins the decision that GCP needs only one
// peer group: every router node peers with the same Cloud Router interfaces,
// so there is no per-zone split.
func TestDiscoverEndpoints_SingleGroup(t *testing.T) {
	compute := &fakeCompute{topology: &CloudRouterTopology{
		ASN:            64512,
		InterfaceNames: []string{"if-0", "if-1"},
		InterfaceIPs:   []string{"169.254.0.1", "169.254.1.1"},
	}}
	p := NewWithClients(testConfig(), compute, &fakeNCC{}, nil)

	result, err := p.DiscoverEndpoints(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.PeerGroups) != 1 {
		t.Fatalf("expected 1 peer group, got %d", len(result.PeerGroups))
	}

	group := result.PeerGroups[0]
	if len(group.NodeSelector) != 0 {
		t.Errorf("expected no extra node selector, got %v", group.NodeSelector)
	}
	if len(group.Neighbors) != 2 {
		t.Fatalf("expected 2 neighbours, got %d", len(group.Neighbors))
	}
	for _, n := range group.Neighbors {
		if n.ASN != 64512 {
			t.Errorf("neighbour %s: expected ASN 64512, got %d", n.Address, n.ASN)
		}
	}
}

// TestDiscoverEndpoints_RawConfig pins the disable-connected-check directives.
// The worker carries a /32 on br-ex, so FRR rejects the Cloud Router as
// unreachable without them.
func TestDiscoverEndpoints_RawConfig(t *testing.T) {
	compute := &fakeCompute{topology: &CloudRouterTopology{
		ASN:            64512,
		InterfaceNames: []string{"if-0", "if-1"},
		InterfaceIPs:   []string{"169.254.0.1", "169.254.1.1"},
	}}
	p := NewWithClients(testConfig(), compute, &fakeNCC{}, nil)

	result, err := p.DiscoverEndpoints(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	raw := result.PeerGroups[0].RawFRRConfig
	for _, want := range []string{
		"router bgp 65003",
		"neighbor 169.254.0.1 disable-connected-check",
		"neighbor 169.254.1.1 disable-connected-check",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("raw config missing %q:\n%s", want, raw)
		}
	}
}

func TestDiscoverEndpoints_NoInterfaces(t *testing.T) {
	compute := &fakeCompute{topology: &CloudRouterTopology{ASN: 64512}}
	p := NewWithClients(testConfig(), compute, &fakeNCC{}, nil)

	if _, err := p.DiscoverEndpoints(context.Background()); err == nil {
		t.Fatal("expected an error when the Cloud Router has no interfaces")
	}
}

func TestDiscoverEndpoints_TopologyError(t *testing.T) {
	compute := &fakeCompute{topologyErr: errors.New("permission denied")}
	p := NewWithClients(testConfig(), compute, &fakeNCC{}, nil)

	if _, err := p.DiscoverEndpoints(context.Background()); err == nil {
		t.Fatal("expected the topology error to surface")
	}
}

// --- Reconcile ---

func TestReconcileNodes(t *testing.T) {
	compute := &fakeCompute{topology: &CloudRouterTopology{
		ASN:            64512,
		InterfaceNames: []string{"if-0"},
		InterfaceIPs:   []string{"169.254.0.1"},
	}}
	ncc := &fakeNCC{}
	p := NewWithClients(testConfig(), compute, ncc, nil)

	nodes := []platform.RouterNode{
		node("worker-b", "10.0.0.3", "europe-west1-b"),
		node("worker-a", "10.0.0.2", "europe-west1-a"),
	}
	if err := p.ReconcileNodes(context.Background(), nodes); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(compute.canIPForwardCalls) != 2 {
		t.Errorf("expected canIpForward on 2 instances, got %v", compute.canIPForwardCalls)
	}
	if len(compute.nestedVirtCalls) != 2 {
		t.Errorf("expected nested virtualisation on 2 instances, got %v", compute.nestedVirtCalls)
	}
	if !compute.peersCalled {
		t.Error("expected Cloud Router peers to be reconciled")
	}
	if compute.peersASN != 65003 {
		t.Errorf("expected peers to use local ASN 65003, got %d", compute.peersASN)
	}
	if got := ncc.reconciled["spoke-0"]; len(got) != 2 {
		t.Errorf("expected 2 instances on spoke-0, got %+v", got)
	}
	for _, n := range compute.peersNodes {
		if !strings.HasPrefix(n.SelfLink, "https://www.googleapis.com/compute/v1/") {
			t.Errorf("node %q has no resolved selfLink: %q", n.Name, n.SelfLink)
		}
	}
}

// TestReconcileNodes_NestedVirtDisabled proves the flag is honoured, since
// enabling it restarts the instance.
func TestReconcileNodes_NestedVirtDisabled(t *testing.T) {
	compute := &fakeCompute{topology: &CloudRouterTopology{
		ASN: 64512, InterfaceNames: []string{"if-0"}, InterfaceIPs: []string{"169.254.0.1"},
	}}
	cfg := testConfig()
	cfg.NestedVirt = false
	p := NewWithClients(cfg, compute, &fakeNCC{}, nil)

	if err := p.ReconcileNodes(context.Background(), []platform.RouterNode{node("worker-a", "10.0.0.2", "europe-west1-a")}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(compute.nestedVirtCalls) != 0 {
		t.Errorf("nested virtualisation must not be touched when disabled, got %v", compute.nestedVirtCalls)
	}
}

func TestReconcileNodes_RejectsNonGCEProviderID(t *testing.T) {
	p := NewWithClients(testConfig(), &fakeCompute{}, &fakeNCC{}, nil)

	nodes := []platform.RouterNode{{Name: "worker-a", PrivateIP: "10.0.0.2", ProviderID: "aws:///us-east-1a/i-0abc"}}
	if err := p.ReconcileNodes(context.Background(), nodes); err == nil {
		t.Fatal("expected an error for a non-GCE provider ID")
	}
}

// TestReconcileSpokes_ChunksAndPrunes covers the NCC instance limit: a router
// set larger than one spoke is split, and spokes the set no longer justifies
// are removed.
func TestReconcileSpokes_ChunksAndPrunes(t *testing.T) {
	compute := &fakeCompute{topology: &CloudRouterTopology{
		ASN: 64512, InterfaceNames: []string{"if-0"}, InterfaceIPs: []string{"169.254.0.1"},
	}}
	ncc := &fakeNCC{existing: []string{"spoke-0", "spoke-1", "spoke-2"}}
	p := NewWithClients(testConfig(), compute, ncc, nil)

	var nodes []platform.RouterNode
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"} {
		nodes = append(nodes, node("worker-"+n, "10.0.0."+n, "europe-west1-a"))
	}

	if err := p.ReconcileNodes(context.Background(), nodes); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 9 nodes at 8 per spoke is two spokes, so spoke-2 must go.
	if len(ncc.reconciled["spoke-0"]) != MaxInstancesPerSpoke {
		t.Errorf("expected spoke-0 full, got %d", len(ncc.reconciled["spoke-0"]))
	}
	if len(ncc.reconciled["spoke-1"]) != 1 {
		t.Errorf("expected spoke-1 to hold the remainder, got %d", len(ncc.reconciled["spoke-1"]))
	}
	if len(ncc.deleted) != 1 || ncc.deleted[0] != "spoke-2" {
		t.Errorf("expected spoke-2 to be pruned, got %v", ncc.deleted)
	}
}

func TestCleanup(t *testing.T) {
	compute := &fakeCompute{}
	ncc := &fakeNCC{existing: []string{"spoke-0", "spoke-1"}}
	p := NewWithClients(testConfig(), compute, ncc, nil)

	if err := p.Cleanup(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !compute.clearCalled {
		t.Error("expected Cloud Router peers to be cleared")
	}
	if len(ncc.deleted) != 2 {
		t.Errorf("expected both spokes deleted, got %v", ncc.deleted)
	}
}

// TestPlatformSatisfiesInterfaces is a compile-time check that the GCP
// platform is usable by the controller and opts into node lifecycle handling.
func TestPlatformSatisfiesInterfaces(t *testing.T) {
	var p any = &Platform{}
	if _, ok := p.(platform.CloudPlatform); !ok {
		t.Error("Platform does not implement platform.CloudPlatform")
	}
	if _, ok := p.(platform.NodeLifecycle); !ok {
		t.Error("Platform does not implement platform.NodeLifecycle")
	}
}
