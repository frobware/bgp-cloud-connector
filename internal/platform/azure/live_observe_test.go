//go:build azurelive

// What Azure says about the peerings this operator owns.
//
//	AZURE_LIVE_SUBSCRIPTION_ID=... AZURE_LIVE_RESOURCE_GROUP=<infra>-rg \
//	AZURE_LIVE_ROUTE_SERVER=<infra>-rs AZURE_LIVE_CLUSTER_ID=<infra> \
//	go test -tags azurelive ./internal/platform/azure/ -run TestAzureLive_ObservePeers -v
//
// Read-only. It needs peerings to already exist, which
// TestAzureLive_ReconcileNodes creates.
package azure

import (
	"context"
	"sort"
	"strings"
	"testing"
)

// TestAzureLive_ObservePeers checks what reaches status.peers, and completes
// the three-way split across the clouds. AWS fills in both a resource state
// and a session state. GCP fills in only the session, because a Cloud Router
// peer is a field inside the router with no lifecycle of its own. Azure is the
// other way round: a BGP connection has a provisioning state, and its
// connection state comes back absent for a live session and a dead one alike,
// so nothing is inferred from it and sessionState simply does not appear.
//
// Asserting that emptiness is the point. It is a documented promise to whoever
// reads status.peers, and the only way it stays true is if something notices
// when Azure starts filling it in.
func TestAzureLive_ObservePeers(t *testing.T) {
	cfg := liveConfig(t)
	if cfg.ClusterID == "" {
		t.Skip("AZURE_LIVE_CLUSTER_ID must be set: it is the ownership prefix observation filters on")
	}
	ctx := context.Background()
	p := livePlatform(t, cfg)

	step(t, "reading what Azure says about the peerings owned by %q", cfg.ClusterID)
	peers, err := p.ObservePeers(ctx)
	if err != nil {
		t.Fatalf("ObservePeers: %v", err)
	}

	// A missing fixture is a failure, not a skip: a green run against a Route
	// Server with no peerings on it would say this works when it has not run.
	if len(peers) == 0 {
		t.Fatalf("no peerings owned by %q on Route Server %q; run TestAzureLive_ReconcileNodes first",
			cfg.ClusterID, cfg.RouteServerName)
	}
	observed(t, "%d peering(s)", len(peers))
	for _, pr := range peers {
		observed(t, "%-40s addr=%-14s asn=%d state=%q session=%q",
			pr.Name, pr.Address, pr.ASN, pr.State, pr.SessionState)
	}

	step(t, "checking only peerings this cluster owns are reported, since a shared Route Server carries others")
	for _, pr := range peers {
		if !isOurPeering(pr.Name, cfg.ClusterID) {
			t.Errorf("peering %q is not ours and should have been filtered out", pr.Name)
		}
	}

	step(t, "checking every peering carries the address and ASN status.peers is read for")
	for _, pr := range peers {
		if pr.Address == "" {
			t.Errorf("peering %s has no address", pr.Name)
		}
		if strings.Contains(pr.Address, "/") {
			t.Errorf("peering %s carries a mask on %q", pr.Name, pr.Address)
		}
		if pr.ASN != cfg.LocalASN {
			t.Errorf("peering %s reports ASN %d, want this cluster's %d", pr.Name, pr.ASN, cfg.LocalASN)
		}
	}

	step(t, "checking Azure reports a provisioning state and no session state")
	for _, pr := range peers {
		if pr.State == "" {
			t.Errorf("peering %s reports no provisioning state, which is the half Azure does have", pr.Name)
		}
		if pr.SessionState != "" {
			t.Errorf("peering %s reports session state %q; Azure declares connectionState and never fills it in, "+
				"so if this is now populated the omitempty on status.peers is understating what Azure knows",
				pr.Name, pr.SessionState)
		}
	}

	step(t, "checking the report is ordered, since status would otherwise churn between identical reconciles")
	if !sort.SliceIsSorted(peers, func(i, j int) bool { return peers[i].Name < peers[j].Name }) {
		t.Error("peerings came back unordered")
	}
}
