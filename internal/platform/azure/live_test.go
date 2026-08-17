//go:build azurelive

// These exercise the real Azure network API against a cluster that already has
// its Route Server standing. They are excluded from the default build because
// they need credentials and a live Route Server.
//
//	AZURE_LIVE_SUBSCRIPTION_ID=... \
//	AZURE_LIVE_RESOURCE_GROUP=<infra>-rg \
//	AZURE_LIVE_ROUTE_SERVER=<infra>-rs \
//	AZURE_LIVE_CLUSTER_ID=<infra> \
//	go test -tags azurelive ./internal/platform/azure/ -run TestAzureLive -v
//
// Everything in this file is read-only. Creating a peering or writing
// enableIPForwarding is what the operator reconciles, and building it here
// would leave an estate that makes the reconcile test pass against an operator
// that did nothing.
package azure

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
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

// azureRouteServerASN is fixed by Azure. Unlike AWS and GCP there is nothing to
// choose: `az network routeserver create` has no flag to set it.
const azureRouteServerASN = 65515

// liveConfig reads the estate from the environment. The variables carry an
// AZURE_LIVE_ prefix rather than the conventional AZURE_ ones, so that a test
// can name a subscription the ambient credential is not already pointed at,
// and so nothing here collides with what azidentity consumes.
func liveConfig(t *testing.T) Config {
	t.Helper()
	cfg := Config{
		SubscriptionID:  os.Getenv("AZURE_LIVE_SUBSCRIPTION_ID"),
		ResourceGroup:   os.Getenv("AZURE_LIVE_RESOURCE_GROUP"),
		RouteServerName: os.Getenv("AZURE_LIVE_ROUTE_SERVER"),
		ClusterID:       os.Getenv("AZURE_LIVE_CLUSTER_ID"),
		LocalASN:        65001,
	}
	if cfg.SubscriptionID == "" || cfg.ResourceGroup == "" || cfg.RouteServerName == "" {
		t.Skip("AZURE_LIVE_SUBSCRIPTION_ID, AZURE_LIVE_RESOURCE_GROUP and AZURE_LIVE_ROUTE_SERVER must be set")
	}
	step(t, "live Azure: subscription=%s resourceGroup=%s routeServer=%s clusterID=%q localASN=%d",
		cfg.SubscriptionID, cfg.ResourceGroup, cfg.RouteServerName, cfg.ClusterID, cfg.LocalASN)
	return cfg
}

func livePlatform(t *testing.T, cfg Config) *Platform {
	t.Helper()
	p, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// TestAzureLive_NewVerifiesCredentials proves the default credential chain
// resolves. Azure has no cheap identity probe equivalent to
// sts:GetCallerIdentity, so New reads the Route Server and that first real
// call doubles as one -- which means a failure here is either a credential
// problem or a missing Route Server, and the error says which.
func TestAzureLive_NewVerifiesCredentials(t *testing.T) {
	cfg := liveConfig(t)

	step(t, "building the platform, which verifies the credential by reading the Route Server")
	p, err := New(context.Background(), cfg)
	if err != nil {
		var credErr *platform.CredentialError
		if errors.As(err, &credErr) {
			t.Fatalf("credential or Route Server unreadable: %v", credErr)
		}
		t.Fatalf("New: %v", err)
	}
	if p == nil {
		t.Fatal("expected a platform")
	}
	observed(t, "credential accepted, Route Server %q readable", cfg.RouteServerName)
}

// TestAzureLive_DiscoverEndpoints checks the peering plan the controller
// renders. This is where Azure's shape differs from both the others, so the
// assertions are about which cloud this behaves like and where it behaves like
// neither.
func TestAzureLive_DiscoverEndpoints(t *testing.T) {
	cfg := liveConfig(t)
	p := livePlatform(t, cfg)
	ctx := context.Background()

	step(t, "discovering the peering plan the controller would render")
	result, err := p.DiscoverEndpoints(ctx)
	if err != nil {
		t.Fatalf("DiscoverEndpoints: %v", err)
	}

	step(t, "checking Azure yields exactly one peer group, not one per zone as AWS does")
	if len(result.PeerGroups) != 1 {
		t.Fatalf("expected exactly one peer group for Azure, got %d", len(result.PeerGroups))
	}
	group := result.PeerGroups[0]
	observed(t, "group key  %s", group.Key)
	observed(t, "selector   %v (empty means every router node)", group.NodeSelector)
	for _, n := range group.Neighbors {
		observed(t, "neighbour  %s remoteASN=%d ebgpMultiHop=%t", n.Address, n.ASN, n.EBGPMultiHop)
	}

	step(t, "checking the group is keyed by the Route Server, which is how it is named in status")
	if group.Key != cfg.RouteServerName {
		t.Errorf("group key is %q, want the Route Server name %q", group.Key, cfg.RouteServerName)
	}

	step(t, "checking the selector is empty, since one Route Server serves every node in the vnet")
	if len(group.NodeSelector) != 0 {
		t.Errorf("expected no node selector, got %v", group.NodeSelector)
	}

	step(t, "checking both halves of the redundant address pair are present")
	if len(group.Neighbors) != 2 {
		t.Errorf("expected the redundant pair, got %d neighbour(s)", len(group.Neighbors))
	}

	step(t, "checking every neighbour asks for eBGP multihop, which only Azure needs")
	for _, n := range group.Neighbors {
		if !n.EBGPMultiHop {
			t.Errorf("neighbour %s does not ask for multihop; the Route Server is not on the node's link and the session will never leave Active", n.Address)
		}
	}

	step(t, "checking every neighbour has a bare address and Azure's fixed ASN")
	for _, n := range group.Neighbors {
		if n.Address == "" {
			t.Error("a neighbour has no address")
		}
		if strings.Contains(n.Address, "/") {
			t.Errorf("neighbour %q carries a mask; FRR needs a bare address", n.Address)
		}
		if n.ASN != azureRouteServerASN {
			t.Errorf("neighbour %s reports ASN %d; Azure fixes it at %d and offers no flag to change it",
				n.Address, n.ASN, azureRouteServerASN)
		}
	}

	step(t, "checking Azure emits no raw FRR block, unlike GCP's disable-connected-check")
	if group.RawFRRConfig != "" {
		t.Errorf("group emitted a raw block, which Azure should not need:\n%s", group.RawFRRConfig)
	}

	step(t, "checking discovery is stable, since the group names the generated FRRConfiguration")
	second, err := p.DiscoverEndpoints(ctx)
	if err != nil {
		t.Fatalf("second DiscoverEndpoints: %v", err)
	}
	if len(second.PeerGroups) != 1 || second.PeerGroups[0].Key != group.Key {
		t.Errorf("discovery moved between calls: %v then %v", result.PeerGroups, second.PeerGroups)
	}
	if len(second.PeerGroups[0].Neighbors) != len(group.Neighbors) {
		t.Errorf("neighbour count moved between calls: %d then %d",
			len(group.Neighbors), len(second.PeerGroups[0].Neighbors))
	}
	observed(t, "two calls agreed on %d neighbour(s)", len(group.Neighbors))
}

// TestAzureLive_CheckPrerequisites reads what can be established from the
// Route Server itself. Azure deliberately checks less than the other two: no
// silent failure of the AWS propagation or GCP firewall kind is known here, and
// inventing a check that cannot be demonstrated would be worse than reporting
// only what can.
func TestAzureLive_CheckPrerequisites(t *testing.T) {
	cfg := liveConfig(t)
	p := livePlatform(t, cfg)

	step(t, "reading the Route Server's addresses and ASN")
	unmet, err := p.CheckPrerequisites(context.Background())
	if err != nil {
		t.Fatalf("CheckPrerequisites: %v", err)
	}

	for _, u := range unmet {
		observed(t, "unmet: %s", u)
	}
	if len(unmet) != 0 {
		t.Errorf("expected a provisioned Route Server to satisfy every prerequisite, got %d unmet", len(unmet))
	} else {
		observed(t, "all prerequisites satisfied")
	}

	step(t, "checking each unmet line names the Route Server and a remedy, since this is what reaches status")
	for _, u := range unmet {
		if !strings.Contains(u, cfg.RouteServerName) {
			t.Errorf("unmet prerequisite names no Route Server, so nobody can act on it: %q", u)
		}
		if !strings.Contains(u, "az network routeserver ") {
			t.Errorf("unmet prerequisite carries no remedy command: %q", u)
		}
	}
}
