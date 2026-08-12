package gcp

import (
	"testing"

	"google.golang.org/api/compute/v1"
)

func testTopology() *CloudRouterTopology {
	return &CloudRouterTopology{
		ASN:            65000,
		InterfaceNames: []string{"cudn-bgp-if-0", "cudn-bgp-if-1"},
		InterfaceIPs:   []string{"10.0.128.5", "10.0.128.6"},
	}
}

func peerIPsByName(peers []*compute.RouterBgpPeer) map[string]string {
	m := make(map[string]string, len(peers))
	for _, p := range peers {
		m[p.Name] = p.PeerIpAddress
	}
	return m
}

// TestDesiredPeers_NamesAreStableAcrossScaleUp is the defect this replaces.
// Naming peers by their position in a sorted list means a node joining in the
// middle renumbers everything after it, so the router is rewritten with every
// existing peer pointing at a different node and every session drops.
func TestDesiredPeers_NamesAreStableAcrossScaleUp(t *testing.T) {
	top := testTopology()
	before := []RouterNode{
		{Name: "worker-b", IPAddress: "10.0.128.3"},
		{Name: "worker-c", IPAddress: "10.0.128.4"},
		{Name: "worker-d", IPAddress: "10.0.128.2"},
	}
	// Sorts ahead of every existing node, the worst case for positional names.
	after := append([]RouterNode{{Name: "worker-a", IPAddress: "10.0.128.9"}}, before...)

	was := peerIPsByName(desiredPeers("cluster", before, top, 65001))
	now := peerIPsByName(desiredPeers("cluster", after, top, 65001))

	for name, ip := range was {
		got, still := now[name]
		if !still {
			t.Errorf("peer %q disappeared when a node was added", name)
			continue
		}
		if got != ip {
			t.Errorf("peer %q moved from %s to %s; the session would drop", name, ip, got)
		}
	}
	if len(now) != len(was)+len(top.InterfaceNames) {
		t.Errorf("expected %d peers after adding one node, got %d", len(was)+len(top.InterfaceNames), len(now))
	}
}

func TestDesiredPeers_NameEncodesAddress(t *testing.T) {
	top := testTopology()
	peers := desiredPeers("cz-demo1", []RouterNode{{Name: "worker-b", IPAddress: "10.0.128.3"}}, top, 65001)

	if len(peers) != 2 {
		t.Fatalf("expected one peer per interface, got %d", len(peers))
	}
	want := "cz-demo1-bgp-10-0-128-3-0"
	if peers[0].Name != want {
		t.Errorf("peer name = %q, want %q", peers[0].Name, want)
	}
	for _, p := range peers {
		if len(p.Name) > 63 {
			t.Errorf("peer name %q exceeds the 63 character limit", p.Name)
		}
	}
}

// TestPeerName_TruncatesLongClusterID keeps the address intact when the
// cluster ID is long, because the address is what makes the name unique.
func TestPeerName_TruncatesLongClusterID(t *testing.T) {
	long := "a-very-long-openshift-infrastructure-name-that-goes-on-and-on"
	name := PeerName(long, "10.128.255.254", 1)

	if len(name) > 63 {
		t.Errorf("peer name %q is %d characters, over the limit", name, len(name))
	}
	if got := name[len(name)-len("-10-128-255-254-1"):]; got != "-10-128-255-254-1" {
		t.Errorf("address suffix was truncated: %q", name)
	}
}

// TestMergePeers_PreservesForeignPeers matters because GCP BGP peers are
// fields inside the router and cannot carry labels, so a name prefix is the
// only ownership signal there is. Another cluster sharing the router, or a
// hand-made peer, must survive our reconcile.
func TestMergePeers_PreservesForeignPeers(t *testing.T) {
	foreign := []*compute.RouterBgpPeer{
		{Name: "someone-else-peer-0", PeerIpAddress: "10.0.200.1", PeerAsn: 64999},
		{Name: "manual-debug-peer", PeerIpAddress: "10.0.200.2", PeerAsn: 64998},
	}
	ours := desiredPeers("cluster", []RouterNode{{Name: "worker-b", IPAddress: "10.0.128.3"}}, testTopology(), 65001)

	merged := mergePeers(append(foreign, ours...), ours, "cluster")

	names := peerIPsByName(merged)
	for _, f := range foreign {
		if _, kept := names[f.Name]; !kept {
			t.Errorf("foreign peer %q was removed", f.Name)
		}
	}
	if len(merged) != len(foreign)+len(ours) {
		t.Errorf("expected %d peers, got %d", len(foreign)+len(ours), len(merged))
	}
}

// TestMergePeers_DropsOurStalePeers covers scale-down: a peer of ours whose
// node has gone must not survive.
func TestMergePeers_DropsOurStalePeers(t *testing.T) {
	top := testTopology()
	gone := desiredPeers("cluster", []RouterNode{{Name: "worker-z", IPAddress: "10.0.128.99"}}, top, 65001)
	ours := desiredPeers("cluster", []RouterNode{{Name: "worker-b", IPAddress: "10.0.128.3"}}, top, 65001)

	merged := mergePeers(append(gone, ours...), ours, "cluster")

	names := peerIPsByName(merged)
	for _, g := range gone {
		if _, still := names[g.Name]; still {
			t.Errorf("stale peer %q survived", g.Name)
		}
	}
	if len(merged) != len(ours) {
		t.Errorf("expected only our current peers, got %d", len(merged))
	}
}

// TestMergePeers_Deterministic stops a map iteration ordering difference
// looking like a change and provoking a write every reconcile.
func TestMergePeers_Deterministic(t *testing.T) {
	top := testTopology()
	ours := desiredPeers("cluster", []RouterNode{
		{Name: "worker-b", IPAddress: "10.0.128.3"},
		{Name: "worker-c", IPAddress: "10.0.128.4"},
	}, top, 65001)
	foreign := []*compute.RouterBgpPeer{{Name: "zz-other", PeerIpAddress: "10.0.200.1", PeerAsn: 64999}}

	first := mergePeers(append(foreign, ours...), ours, "cluster")
	second := mergePeers(append(ours, foreign...), ours, "cluster")

	if len(first) != len(second) {
		t.Fatalf("lengths differ: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Name != second[i].Name {
			t.Errorf("order differs at %d: %q vs %q", i, first[i].Name, second[i].Name)
		}
	}
}

// TestOurPeers_OnlyMatchesOurPrefix guards the ownership test itself: a peer
// belonging to a cluster whose ID merely starts the same must not be claimed.
func TestOurPeers_OnlyMatchesOurPrefix(t *testing.T) {
	cases := map[string]bool{
		"cluster-bgp-10-0-0-1-0":     true,
		"cluster-two-bgp-10-0-0-1-0": false,
		"other-cluster-bgp-10-0-0-1": false,
		"cluster-bgp-peer-0-0":       true,
		"clusterbgp-10-0-0-1-0":      false,
	}
	for name, want := range cases {
		if got := isOurPeer(name, "cluster"); got != want {
			t.Errorf("isOurPeer(%q) = %v, want %v", name, got, want)
		}
	}
}
