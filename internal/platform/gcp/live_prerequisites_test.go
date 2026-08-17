//go:build gcplive

// The GCP configuration the operator relies on and does not create.
//
//	GCP_PROJECT=openshift-qe GCP_REGION=us-east1 \
//	GCP_CLOUD_ROUTER=<infra>-cudn-cr \
//	go test -tags gcplive ./internal/platform/gcp/ -run TestGCPLive_CheckPrerequisites -v
//
// Read-only. It expects the estate gcp-create-cloud-router builds.
package gcp

import (
	"context"
	"strings"
	"testing"
)

// TestGCPLive_CheckPrerequisites is the check whose absence is silent: with no
// firewall rule for TCP 179, FRR sits in Active and the Cloud Router in
// Connect, and neither says why.
//
// Unlike the AWS counterpart this asserts a verdict rather than a shape. The
// firewall match is real work against real rules -- source ranges, direction,
// port and whether the rule is enabled -- so an estate built correctly must
// come back satisfied, and anything else is either a broken estate or a broken
// matcher.
func TestGCPLive_CheckPrerequisites(t *testing.T) {
	cfg := liveConfig(t)
	ctx := context.Background()

	c, err := NewComputeClient(ctx, cfg.Project, cfg.Region)
	if err != nil {
		t.Fatalf("building compute client: %v", err)
	}
	p := NewWithClients(cfg, c, nil)

	step(t, "checking prerequisites for Cloud Router %q", cfg.CloudRouterName)
	unmet, err := p.CheckPrerequisites(ctx)
	if err != nil {
		t.Fatalf("CheckPrerequisites: %v", err)
	}

	for _, u := range unmet {
		observed(t, "unmet: %s", u)
	}
	if len(unmet) != 0 {
		t.Errorf("expected a Cloud Router built by gcp-create-cloud-router to satisfy every prerequisite, got %d unmet", len(unmet))
	} else {
		observed(t, "all prerequisites satisfied")
	}

	step(t, "checking each unmet line names the router, since this is what reaches status")
	for _, u := range unmet {
		if !strings.Contains(u, cfg.CloudRouterName) {
			t.Errorf("unmet prerequisite names no Cloud Router, so nobody can act on it: %q", u)
		}
	}

	// Asserting the firewall rule was found is worth little on its own: a
	// matcher that returned true for anything would pass it. So it is asked
	// the same question about an address no rule admits. Both answers together
	// are what show it is matching rather than agreeing.
	topology, err := c.GetRouterTopology(ctx, cfg.CloudRouterName)
	if err != nil {
		t.Fatalf("GetRouterTopology: %v", err)
	}

	step(t, "checking TCP 179 is admitted from the router interfaces %v", topology.InterfaceIPs)
	allowed, err := c.HasBGPFirewallRule(ctx, topology.InterfaceIPs)
	if err != nil {
		t.Fatalf("HasBGPFirewallRule: %v", err)
	}
	if !allowed {
		t.Error("no rule admits TCP 179 from the Cloud Router interfaces; sessions would sit in Connect")
	}

	// TEST-NET-3, reserved by RFC 5737 for documentation, so no real estate
	// has a rule admitting it.
	const unroutable = "203.0.113.1"
	step(t, "checking the same question about %s, which no rule should admit", unroutable)
	allowed, err = c.HasBGPFirewallRule(ctx, []string{unroutable})
	if err != nil {
		t.Fatalf("HasBGPFirewallRule for %s: %v", unroutable, err)
	}
	if allowed {
		t.Errorf("a rule was reported admitting TCP 179 from %s; the matcher is not discriminating on source range", unroutable)
	}
	observed(t, "admitted from the interfaces, refused from %s", unroutable)
}
