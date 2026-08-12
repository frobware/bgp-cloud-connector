package gcp

import (
	"context"
	"fmt"
	"strings"
)

// CheckPrerequisites reports the GCP configuration the operator relies on and
// does not create.
//
// Both checks here exist because their absence is silent. A Cloud Router with
// no interfaces is what the installer's Cloud NAT router looks like, and
// pointing the operator at that one would write BGP peers onto the resource
// the cluster's egress depends on. A missing firewall rule for TCP 179 leaves
// FRR in Active and the Cloud Router in Connect with neither saying why; GCP
// denies ingress by default and the installer opens 6443, etcd, node ports and
// geneve, never 179.
func (p *Platform) CheckPrerequisites(ctx context.Context) ([]string, error) {
	var unmet []string

	topology, err := p.compute.GetRouterTopology(ctx, p.cfg.CloudRouterName)
	if err != nil {
		return nil, fmt.Errorf("reading Cloud Router %q: %w", p.cfg.CloudRouterName, err)
	}
	if len(topology.InterfaceIPs) == 0 {
		unmet = append(unmet, fmt.Sprintf(
			"Cloud Router %q has no interfaces; a BGP router needs an interface per redundant path, and a router with none is typically the installer's Cloud NAT router rather than one built for CUDN",
			p.cfg.CloudRouterName))
	}
	if topology.ASN == 0 {
		unmet = append(unmet, fmt.Sprintf("Cloud Router %q has no ASN and cannot peer", p.cfg.CloudRouterName))
	}

	allowed, err := p.compute.HasBGPFirewallRule(ctx, topology.InterfaceIPs)
	if err != nil {
		return nil, fmt.Errorf("checking BGP firewall rules: %w", err)
	}
	if !allowed && len(topology.InterfaceIPs) > 0 {
		unmet = append(unmet, fmt.Sprintf(
			"no ingress firewall rule allows TCP 179 from the Cloud Router interfaces (%s); GCP denies ingress by default and the installer opens no rule for BGP, so sessions will sit in Connect",
			strings.Join(topology.InterfaceIPs, ", ")))
	}

	return unmet, nil
}
