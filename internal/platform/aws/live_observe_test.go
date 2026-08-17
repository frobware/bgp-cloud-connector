//go:build awslive

// What EC2 says about the peerings this operator owns.
//
//	AWS_LIVE_REGION=us-east-2 \
//	AWS_LIVE_ROUTE_SERVER_IDS=rs-0abc \
//	AWS_LIVE_CLUSTER_ID=<infra> \
//	go test -tags awslive ./internal/platform/aws/ -run TestAWSLive_ObservePeers -v
//
// Read-only. It needs peers to already exist, which TestAWSLive_ReconcileNodes
// creates.
package aws

import (
	"context"
	"sort"
	"strings"
	"testing"
)

// TestAWSLive_ObservePeers checks what reaches status.peers. Everything else
// the operator reports is its own intent quoted back; this is the only part
// that comes from the cloud.
func TestAWSLive_ObservePeers(t *testing.T) {
	cfg := liveConfig(t)
	if cfg.ClusterID == "" {
		t.Skip("AWS_LIVE_CLUSTER_ID must be set: it is the ownership tag observation filters on")
	}
	ctx := context.Background()
	p := livePlatform(t, cfg)

	// Observation walks the endpoint map discovery builds, so discovery is a
	// precondition rather than a separate check here.
	if _, err := p.DiscoverEndpoints(ctx); err != nil {
		t.Fatalf("DiscoverEndpoints: %v", err)
	}

	step(t, "reading what EC2 says about the peerings owned by %q", cfg.ClusterID)
	peers, err := p.ObservePeers(ctx)
	if err != nil {
		t.Fatalf("ObservePeers: %v", err)
	}

	// A missing fixture is a failure, not a skip: a green run against a route
	// server with no peers on it would say this works when it has not run.
	if len(peers) == 0 {
		t.Fatalf("no peers owned by %q on %v; run TestAWSLive_ReconcileNodes first",
			cfg.ClusterID, cfg.RouteServerIDs)
	}
	observed(t, "%d peer(s)", len(peers))
	for _, pr := range peers {
		observed(t, "%-24s addr=%-14s asn=%d state=%q session=%q",
			pr.Name, pr.Address, pr.ASN, pr.State, pr.SessionState)
	}

	step(t, "checking every peer carries the address and ASN status.peers is read for")
	for _, pr := range peers {
		if pr.Name == "" {
			t.Errorf("peer with address %q has no id", pr.Address)
		}
		if pr.Address == "" {
			t.Errorf("peer %s has no address", pr.Name)
		}
		if strings.Contains(pr.Address, "/") {
			t.Errorf("peer %s carries a mask on %q", pr.Name, pr.Address)
		}
		if pr.ASN != cfg.LocalASN {
			t.Errorf("peer %s reports ASN %d, want this cluster's %d", pr.Name, pr.ASN, cfg.LocalASN)
		}
	}

	// The two halves of ObservedPeer that differ by cloud, and AWS is the one
	// that fills both in. A peer available with its BGP status down is a
	// resource EC2 built exactly as asked, carrying a session that never came
	// up -- which is the distinction status.peers exists to show, and the one
	// GCP cannot draw because its peers have no lifecycle of their own.
	step(t, "checking AWS reports both a resource state and a session state")
	for _, pr := range peers {
		if pr.State == "" {
			t.Errorf("peer %s reports no resource state, which AWS does have", pr.Name)
		}
		if pr.SessionState == "" {
			t.Errorf("peer %s reports no session state, which AWS does have", pr.Name)
		}
	}

	step(t, "checking the report is ordered, since the endpoint map it walks is not")
	if !sort.SliceIsSorted(peers, func(i, j int) bool { return peers[i].Name < peers[j].Name }) {
		t.Error("peers came back unordered; status would churn between otherwise identical reconciles")
	}
}
