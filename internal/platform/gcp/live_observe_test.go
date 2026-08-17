//go:build gcplive

// What GCP says about the peerings this operator owns.
//
//	GCP_PROJECT=openshift-qe GCP_REGION=us-east1 \
//	GCP_CLOUD_ROUTER=<infra>-cudn-cr GCP_CLUSTER_ID=<infra> \
//	go test -tags gcplive ./internal/platform/gcp/ -run TestGCPLive_ObservePeers -v
//
// Read-only. It needs peers to already exist, which TestGCPLive_ReconcileNodes
// creates.
package gcp

import (
	"context"
	"strings"
	"testing"
)

func liveObserveConfig(t *testing.T) Config {
	t.Helper()
	cfg := liveConfig(t)
	cfg.ClusterID = envOr("GCP_CLUSTER_ID", "e2e")
	return cfg
}

// TestGCPLive_ObservePeers checks what reaches status.peers. Everything else
// the operator reports is its own intent quoted back; this is the only part
// that comes from the cloud, and nothing has ever exercised it against one.
func TestGCPLive_ObservePeers(t *testing.T) {
	cfg := liveObserveConfig(t)
	ctx := context.Background()

	c, err := NewComputeClient(ctx, cfg.Project, cfg.Region)
	if err != nil {
		t.Fatalf("building compute client: %v", err)
	}
	p := NewWithClients(cfg, c, nil)

	step(t, "reading what GCP says about the peerings owned by %q", cfg.ClusterID)
	peers, err := p.ObservePeers(ctx)
	if err != nil {
		t.Fatalf("ObservePeers: %v", err)
	}

	// A missing fixture is a failure, not a skip: a green run against a Cloud
	// Router with no peers on it would say this works when it has not run.
	if len(peers) == 0 {
		t.Fatalf("no peers owned by %q on Cloud Router %q; run TestGCPLive_ReconcileNodes first",
			cfg.ClusterID, cfg.CloudRouterName)
	}
	observed(t, "%d peer(s)", len(peers))
	for _, pr := range peers {
		observed(t, "%-45s addr=%-12s asn=%d state=%q session=%q",
			pr.Name, pr.Address, pr.ASN, pr.State, pr.SessionState)
	}

	step(t, "checking only peers this cluster owns are reported, since a shared router carries others")
	for _, pr := range peers {
		if !isOurPeer(pr.Name, cfg.ClusterID) {
			t.Errorf("peer %q is not ours and should have been filtered out", pr.Name)
		}
	}

	step(t, "checking every peer carries the address and ASN status.peers is read for")
	for _, pr := range peers {
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

	// The two halves of ObservedPeer that differ by cloud, pinned here because
	// the API documents them as a promise to whoever reads status.peers. GCP is
	// the cloud with no resource state to report: a Cloud Router peer is a
	// field inside the router, not a resource with a lifecycle of its own.
	step(t, "checking GCP reports a session state and no resource state")
	for _, pr := range peers {
		if pr.SessionState == "" {
			t.Errorf("peer %s reports no session state, which is the half GCP does have", pr.Name)
		}
		if pr.State != "" {
			t.Errorf("peer %s reports resource state %q; a Cloud Router peer has no lifecycle to report",
				pr.Name, pr.State)
		}
	}
}
