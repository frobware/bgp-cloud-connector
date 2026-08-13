//go:build awslive

// These exercise the real EC2 and STS APIs against a cluster that already has
// its VPC Route Server estate standing. They are excluded from the default
// build because they need credentials and live route servers.
//
//	AWS_LIVE_REGION=us-east-2 \
//	AWS_LIVE_ROUTE_SERVER_IDS=rs-0abc,rs-0def \
//	AWS_LIVE_CLUSTER_ID=<infra> \
//	go test -tags awslive ./internal/platform/aws/ -run TestAWSLive -v
//
// Everything here is read-only. Nothing creates, tags or deletes a peer, and
// nothing touches SourceDestCheck: those are what the operator reconciles, and
// building them here would leave an estate that makes the reconcile tests pass
// against an operator that did nothing.
package aws

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

// liveConfig reads the estate from the environment. The variables are prefixed
// AWS_LIVE_ rather than reusing AWS_REGION, which the SDK's own credential
// chain consumes: sharing them would mean a test could not name a region the
// credentials were not already pointed at.
func liveConfig(t *testing.T) Config {
	t.Helper()
	cfg := Config{
		Region:            os.Getenv("AWS_LIVE_REGION"),
		LocalASN:          65001,
		LivenessDetection: "bgp-keepalive",
		ClusterID:         os.Getenv("AWS_LIVE_CLUSTER_ID"),
	}
	if ids := os.Getenv("AWS_LIVE_ROUTE_SERVER_IDS"); ids != "" {
		for _, id := range strings.Split(ids, ",") {
			if id = strings.TrimSpace(id); id != "" {
				cfg.RouteServerIDs = append(cfg.RouteServerIDs, id)
			}
		}
	}
	if cfg.Region == "" || len(cfg.RouteServerIDs) == 0 {
		t.Skip("AWS_LIVE_REGION and AWS_LIVE_ROUTE_SERVER_IDS must be set")
	}
	step(t, "live AWS: region=%s routeServers=%v clusterID=%q localASN=%d",
		cfg.Region, cfg.RouteServerIDs, cfg.ClusterID, cfg.LocalASN)
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

// TestAWSLive_NewVerifiesCredentials proves the default credential chain
// resolves and that New's sts:GetCallerIdentity probe passes against it. On a
// cluster the operator reaches this through IRSA; from a test runner it is
// whatever the chain finds. Either way a failure here is a credential problem
// and not a route server one, which is the distinction the CredentialError
// type exists to preserve.
func TestAWSLive_NewVerifiesCredentials(t *testing.T) {
	cfg := liveConfig(t)

	step(t, "building the platform, which verifies credentials via sts:GetCallerIdentity")
	p, err := New(context.Background(), cfg)
	if err != nil {
		var credErr *platform.CredentialError
		if errors.As(err, &credErr) {
			t.Fatalf("credentials rejected: %v", credErr)
		}
		t.Fatalf("New: %v", err)
	}
	if p == nil {
		t.Fatal("expected a platform")
	}
	observed(t, "credentials accepted for region %s", cfg.Region)
}

// TestAWSLive_DiscoverEndpoints is the AWS counterpart of the GCP test of the
// same name, and asserts the same contract from the other side: the peering
// plan the controller renders into FRRConfigurations. Where GCP yields one
// group for the whole cluster, AWS yields one per availability zone.
func TestAWSLive_DiscoverEndpoints(t *testing.T) {
	cfg := liveConfig(t)
	p := livePlatform(t, cfg)
	ctx := context.Background()

	step(t, "discovering route server endpoints for %v", cfg.RouteServerIDs)
	result, err := p.DiscoverEndpoints(ctx)
	if err != nil {
		t.Fatalf("DiscoverEndpoints: %v", err)
	}

	step(t, "checking at least one peer group came back")
	if len(result.PeerGroups) == 0 {
		t.Fatal("expected at least one peer group")
	}
	observed(t, "%d peer group(s)", len(result.PeerGroups))

	for _, g := range result.PeerGroups {
		observed(t, "group %-16s selector=%v", g.Key, g.NodeSelector)
		for _, n := range g.Neighbors {
			observed(t, "    neighbour %s remoteASN=%d", n.Address, n.ASN)
		}
	}

	step(t, "checking each group selects its zone, which is how one FRRConfiguration per AZ is targeted")
	for _, g := range result.PeerGroups {
		zone, ok := g.NodeSelector["topology.kubernetes.io/zone"]
		if !ok {
			t.Errorf("group %q has no topology.kubernetes.io/zone selector, got %v", g.Key, g.NodeSelector)
			continue
		}
		if zone != g.Key {
			t.Errorf("group %q selects zone %q; key and selector should agree", g.Key, zone)
		}
	}

	step(t, "checking every neighbour has an address and a remote ASN")
	for _, g := range result.PeerGroups {
		if len(g.Neighbors) == 0 {
			t.Errorf("group %q has no neighbours", g.Key)
		}
		for _, n := range g.Neighbors {
			if n.Address == "" {
				t.Errorf("group %q has a neighbour with no address", g.Key)
			}
			if strings.Contains(n.Address, "/") {
				t.Errorf("neighbour %q carries a mask; FRR needs a bare address", n.Address)
			}
			if n.ASN == 0 {
				t.Errorf("neighbour %s has no remote ASN", n.Address)
			}
		}
	}

	step(t, "checking AWS emits no raw FRR block, unlike GCP's disable-connected-check")
	for _, g := range result.PeerGroups {
		if g.RawFRRConfig != "" {
			t.Errorf("group %q emitted a raw block, which AWS should not need:\n%s", g.Key, g.RawFRRConfig)
		}
	}

	step(t, "checking the AWS-shaped status fields are populated for status.aws")
	if len(result.RouteServers) == 0 {
		t.Error("expected status.aws.routeServers to be populated")
	}
	for _, rs := range result.RouteServers {
		observed(t, "route server %s remoteASN=%d endpoints=%d", rs.RouteServerID, rs.RemoteASN, len(rs.Endpoints))
		if rs.RemoteASN == 0 {
			t.Errorf("route server %s has no remote ASN", rs.RouteServerID)
		}
	}

	step(t, "checking discovery is stable, since group order names the generated FRRConfigurations")
	second, err := p.DiscoverEndpoints(ctx)
	if err != nil {
		t.Fatalf("second DiscoverEndpoints: %v", err)
	}
	if len(second.PeerGroups) != len(result.PeerGroups) {
		t.Fatalf("group count moved between calls: %d then %d", len(result.PeerGroups), len(second.PeerGroups))
	}
	for i := range result.PeerGroups {
		if second.PeerGroups[i].Key != result.PeerGroups[i].Key {
			t.Errorf("group %d changed key between calls: %q then %q",
				i, result.PeerGroups[i].Key, second.PeerGroups[i].Key)
		}
	}
	observed(t, "two calls agreed on %d group(s) in the same order", len(result.PeerGroups))
}

// TestAWSLive_CheckPrerequisites reads the propagation state of the real route
// servers. This is the check whose absence is silent: without a propagation
// every peer reaches available, every session establishes, FRR advertises the
// CUDN prefix, and nothing in the VPC can reach a pod.
//
// It asserts the shape rather than a verdict, because both answers are
// legitimate depending on what Terraform built. What it will not accept is an
// error, which means the call itself did not work.
func TestAWSLive_CheckPrerequisites(t *testing.T) {
	cfg := liveConfig(t)
	p := livePlatform(t, cfg)

	step(t, "reading route server propagations")
	unmet, err := p.CheckPrerequisites(context.Background())
	if err != nil {
		t.Fatalf("CheckPrerequisites: %v", err)
	}

	if len(unmet) == 0 {
		observed(t, "all prerequisites satisfied")
	} else {
		observed(t, "%d unmet:", len(unmet))
		for _, u := range unmet {
			observed(t, "    %s", u)
		}
	}

	step(t, "checking each unmet line names its route server and a remedy, since this is what reaches status")
	for _, u := range unmet {
		named := false
		for _, id := range cfg.RouteServerIDs {
			if strings.Contains(u, id) {
				named = true
				break
			}
		}
		if !named {
			t.Errorf("unmet prerequisite names no route server, so nobody can act on it: %q", u)
		}
		if !strings.Contains(u, "aws ec2 ") {
			t.Errorf("unmet prerequisite carries no remedy command: %q", u)
		}
	}
}

// TestAWSLive_RouteServerNotFound pins the failure mode for a route server ID
// that does not exist, which is the realistic misconfiguration: a stale ID
// left in a profile after the estate was rebuilt. It must fail at discovery
// with the ID named, not succeed with an empty plan.
func TestAWSLive_RouteServerNotFound(t *testing.T) {
	cfg := liveConfig(t)
	const bogus = "rs-00000000000000000"
	cfg.RouteServerIDs = []string{bogus}
	p := livePlatform(t, cfg)

	step(t, "pointing discovery at a route server that does not exist: %s", bogus)
	_, err := p.DiscoverEndpoints(context.Background())

	step(t, "checking it fails rather than returning an empty peering plan")
	if err == nil {
		t.Fatal("expected discovery to fail for an unknown route server")
	}

	step(t, "checking the error names the offending ID")
	if !strings.Contains(err.Error(), bogus) {
		t.Errorf("error does not name the route server, so it is hard to act on: %v", err)
	}
	observed(t, "%v", err)
}
