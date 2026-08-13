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

// step narrates what is about to be checked, and observed reports what came
// back. These tests talk to real infrastructure and take seconds rather than
// microseconds, so which check is running -- and that it ran at all rather
// than being compiled out or skipped -- is worth seeing under -v. A live test
// that silently does nothing looks exactly like one that passed.
func step(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Logf("--> "+format, args...)
}

func observed(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Logf("      "+format, args...)
}

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
	step(t, "live GCP: project=%s region=%s router=%s localASN=%d",
		cfg.Project, cfg.Region, cfg.CloudRouterName, cfg.LocalASN)
	return cfg
}

// TestGCPLive_GetRouterTopology proves the vendored client, the credentials and
// the topology parsing work against a real Cloud Router. The interface
// addresses come back carrying a mask that the BGP neighbour must not have.
func TestGCPLive_GetRouterTopology(t *testing.T) {
	cfg := liveConfig(t)
	ctx := context.Background()

	c, err := NewComputeClient(ctx, cfg.Project, cfg.Region)
	if err != nil {
		t.Fatalf("building compute client: %v", err)
	}

	step(t, "reading the topology of Cloud Router %q", cfg.CloudRouterName)
	topology, err := c.GetRouterTopology(ctx, cfg.CloudRouterName)
	if err != nil {
		t.Fatalf("GetRouterTopology: %v", err)
	}
	observed(t, "ASN        %d", topology.ASN)
	observed(t, "interfaces %v", topology.InterfaceNames)
	observed(t, "addresses  %v", topology.InterfaceIPs)

	step(t, "checking the router has an ASN, without which it cannot peer")
	if topology.ASN == 0 {
		t.Error("expected the Cloud Router to have an ASN")
	}

	step(t, "checking it has at least one interface")
	if len(topology.InterfaceIPs) == 0 {
		t.Fatal("expected at least one interface")
	}

	step(t, "checking every interface name has an address")
	if len(topology.InterfaceNames) != len(topology.InterfaceIPs) {
		t.Errorf("names and addresses disagree: %d vs %d", len(topology.InterfaceNames), len(topology.InterfaceIPs))
	}

	step(t, "checking addresses carry no mask, since FRR needs a bare neighbour")
	for _, ip := range topology.InterfaceIPs {
		if strings.Contains(ip, "/") {
			t.Errorf("interface address %q still carries a mask; FRR needs a bare address", ip)
			continue
		}
		observed(t, "%s is bare", ip)
	}
}

// TestGCPLive_DiscoverEndpoints checks the peer group the controller renders,
// including the raw block that defeats FRR's connected check.
func TestGCPLive_DiscoverEndpoints(t *testing.T) {
	cfg := liveConfig(t)
	ctx := context.Background()

	c, err := NewComputeClient(ctx, cfg.Project, cfg.Region)
	if err != nil {
		t.Fatalf("building compute client: %v", err)
	}
	p := NewWithClients(cfg, c, nil)

	step(t, "discovering the peering plan the controller would render")
	result, err := p.DiscoverEndpoints(ctx)
	if err != nil {
		t.Fatalf("DiscoverEndpoints: %v", err)
	}

	step(t, "checking GCP yields exactly one peer group, not one per zone")
	if len(result.PeerGroups) != 1 {
		t.Fatalf("expected exactly one peer group for GCP, got %d", len(result.PeerGroups))
	}

	group := result.PeerGroups[0]
	observed(t, "group key  %s", group.Key)
	observed(t, "selector   %v (empty means every router node)", group.NodeSelector)
	for _, n := range group.Neighbors {
		observed(t, "neighbour  %s remoteASN=%d", n.Address, n.ASN)
	}

	step(t, "checking both halves of the redundant interface pair are present")
	if len(group.Neighbors) < 2 {
		t.Errorf("expected the redundant interface pair, got %d neighbours", len(group.Neighbors))
	}

	step(t, "checking every neighbour carries a remote ASN")
	for _, n := range group.Neighbors {
		if n.ASN == 0 {
			t.Errorf("neighbour %s has no remote ASN", n.Address)
		}
	}

	step(t, "checking the raw block that defeats FRR's connected check is emitted")
	if group.RawFRRConfig == "" {
		t.Error("expected disable-connected-check directives")
	}
	for _, line := range strings.Split(strings.TrimRight(group.RawFRRConfig, "\n"), "\n") {
		observed(t, "%s", line)
	}
}

// TestGCPLive_NATRouterIsRejected pins the guard that stops the operator being
// pointed at the installer's Cloud NAT router, which has no interfaces and
// carries the cluster's egress.
func TestGCPLive_NATRouterIsRejected(t *testing.T) {
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
	p := NewWithClients(cfg, c, nil)

	step(t, "pointing discovery at the Cloud NAT router %q, which carries the cluster's egress", natRouter)
	_, err = p.DiscoverEndpoints(ctx)

	step(t, "checking it is refused rather than written to")
	if err == nil {
		t.Fatal("expected a router with no interfaces to be rejected")
	}

	// Assert the reason, not merely that something failed: an auth or
	// network error would otherwise pass this test without exercising the
	// guard at all.
	step(t, "checking the refusal is for having no interfaces, not an auth or network error")
	if !strings.Contains(err.Error(), "has no interfaces") {
		t.Fatalf("expected rejection for having no interfaces, got: %v", err)
	}
	observed(t, "%v", err)
}
