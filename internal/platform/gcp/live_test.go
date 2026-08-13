//go:build gcplive

// These exercise the real Google APIs against a cluster that already has the
// BGP infrastructure standing. They are excluded from the default build
// because they need credentials and a live Cloud Router.
//
//	GCP_PROJECT=openshift-qe GCP_REGION=us-east1 \
//	GCP_CLOUD_ROUTER=<infra>-bgp-router \
//	go test -tags gcplive ./internal/platform/gcp/ -v
package gcp

import (
	"context"
	"os"
	"strings"
	"testing"
)

func liveConfig(t *testing.T) Config {
	t.Helper()
	cfg := Config{
		Project:         os.Getenv("GCP_PROJECT"),
		Region:          os.Getenv("GCP_REGION"),
		CloudRouterName: os.Getenv("GCP_CLOUD_ROUTER"),
		LocalASN:        65001,
	}
	if cfg.Project == "" || cfg.Region == "" || cfg.CloudRouterName == "" {
		t.Skip("GCP_PROJECT, GCP_REGION and GCP_CLOUD_ROUTER must be set")
	}
	return cfg
}

// TestLive_GetRouterTopology proves the vendored client, the credentials and
// the topology parsing work against a real Cloud Router. The interface
// addresses come back carrying a mask that the BGP neighbour must not have.
func TestLive_GetRouterTopology(t *testing.T) {
	cfg := liveConfig(t)
	ctx := context.Background()

	c, err := NewComputeClient(ctx, cfg.Project, cfg.Region)
	if err != nil {
		t.Fatalf("building compute client: %v", err)
	}

	topology, err := c.GetRouterTopology(ctx, cfg.CloudRouterName)
	if err != nil {
		t.Fatalf("GetRouterTopology: %v", err)
	}

	t.Logf("ASN=%d interfaces=%v ips=%v", topology.ASN, topology.InterfaceNames, topology.InterfaceIPs)

	if topology.ASN == 0 {
		t.Error("expected the Cloud Router to have an ASN")
	}
	if len(topology.InterfaceIPs) == 0 {
		t.Fatal("expected at least one interface")
	}
	if len(topology.InterfaceNames) != len(topology.InterfaceIPs) {
		t.Errorf("names and addresses disagree: %d vs %d", len(topology.InterfaceNames), len(topology.InterfaceIPs))
	}
	for _, ip := range topology.InterfaceIPs {
		if strings.Contains(ip, "/") {
			t.Errorf("interface address %q still carries a mask; FRR needs a bare address", ip)
		}
	}
}

// TestLive_DiscoverEndpoints checks the peer group the controller renders,
// including the raw block that defeats FRR's connected check.
func TestLive_DiscoverEndpoints(t *testing.T) {
	cfg := liveConfig(t)
	ctx := context.Background()

	c, err := NewComputeClient(ctx, cfg.Project, cfg.Region)
	if err != nil {
		t.Fatalf("building compute client: %v", err)
	}
	p := NewWithClients(cfg, c, nil, nil)

	result, err := p.DiscoverEndpoints(ctx)
	if err != nil {
		t.Fatalf("DiscoverEndpoints: %v", err)
	}
	if len(result.PeerGroups) != 1 {
		t.Fatalf("expected exactly one peer group for GCP, got %d", len(result.PeerGroups))
	}

	group := result.PeerGroups[0]
	t.Logf("neighbours=%+v", group.Neighbors)
	t.Logf("raw:\n%s", group.RawFRRConfig)

	if len(group.Neighbors) < 2 {
		t.Errorf("expected the redundant interface pair, got %d neighbours", len(group.Neighbors))
	}
	for _, n := range group.Neighbors {
		if n.ASN == 0 {
			t.Errorf("neighbour %s has no remote ASN", n.Address)
		}
	}
	if group.RawFRRConfig == "" {
		t.Error("expected disable-connected-check directives")
	}
}

// TestLive_NATRouterIsRejected pins the guard that stops the operator being
// pointed at the installer's Cloud NAT router, which has no interfaces and
// carries the cluster's egress.
func TestLive_NATRouterIsRejected(t *testing.T) {
	cfg := liveConfig(t)
	natRouter := os.Getenv("GCP_NAT_ROUTER")
	if natRouter == "" {
		t.Skip("GCP_NAT_ROUTER must be set")
	}
	ctx := context.Background()

	c, err := NewComputeClient(ctx, cfg.Project, cfg.Region)
	if err != nil {
		t.Fatalf("building compute client: %v", err)
	}
	cfg.CloudRouterName = natRouter
	p := NewWithClients(cfg, c, nil, nil)

	_, err = p.DiscoverEndpoints(ctx)
	if err == nil {
		t.Fatal("expected a router with no interfaces to be rejected")
	}
	// Assert the reason, not merely that something failed: an auth or
	// network error would otherwise pass this test without exercising the
	// guard at all.
	if !strings.Contains(err.Error(), "has no interfaces") {
		t.Fatalf("expected rejection for having no interfaces, got: %v", err)
	}
	t.Logf("rejected as expected: %v", err)
}
