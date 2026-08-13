/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package azure

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

// maxPeeringNameLength is Azure's limit for a BGP connection name.
const maxPeeringNameLength = 80

// peeringName keys a peering on the node's address rather than its position
// in any list. Naming by position renumbers every peering when a node sorts
// first, which rewrites the whole set and drops every session -- the bug that
// was found and fixed on GCP.
func peeringName(clusterID, address string) string {
	suffix := "-" + strings.ReplaceAll(address, ".", "-")
	prefix := peeringPrefix(clusterID)
	if len(prefix)+len(suffix) > maxPeeringNameLength {
		prefix = prefix[:maxPeeringNameLength-len(suffix)]
	}
	return prefix + suffix
}

// peeringPrefix is the ownership signal. An Azure BGP connection carries no
// tags, so as on GCP the name is the only marker available, and it is why
// nothing here touches a peering that does not carry it.
func peeringPrefix(clusterID string) string {
	return clusterID + "-bgp"
}

// isOurPeering reports whether a peering was created by this operator for this
// cluster. The trailing separator matters: without it a cluster named
// "cluster" would claim the peerings of one named "cluster-two".
func isOurPeering(name, clusterID string) bool {
	return strings.HasPrefix(name, peeringPrefix(clusterID)+"-")
}

// peeringKey is the identity a peering is compared on. Name alone is not
// enough: a node whose address changed keeps its name only if the name does
// not encode the address, and the ASN can be edited underneath us.
type peeringKey struct {
	name    string
	peerIP  string
	peerASN int64
}

type peeringSet map[peeringKey]struct{}

func (s peeringSet) Equal(other peeringSet) bool {
	if len(s) != len(other) {
		return false
	}
	for k := range s {
		if _, ok := other[k]; !ok {
			return false
		}
	}
	return true
}

func buildPeeringSet(peerings []Peering) peeringSet {
	s := make(peeringSet, len(peerings))
	for _, p := range peerings {
		s[peeringKey{p.Name, p.PeerIP, p.PeerASN}] = struct{}{}
	}
	return s
}

func findPeering(current []Peering, name string) (Peering, bool) {
	for _, p := range current {
		if p.Name == name {
			return p, true
		}
	}
	return Peering{}, false
}

// desiredPeerings is one peering per router node, at the cluster's local ASN.
func desiredPeerings(clusterID string, localASN int64, nodes []platform.RouterNode) []Peering {
	out := make([]Peering, 0, len(nodes))
	for _, n := range nodes {
		if n.PrivateIP == "" {
			continue
		}
		out = append(out, Peering{
			Name:    peeringName(clusterID, n.PrivateIP),
			PeerIP:  n.PrivateIP,
			PeerASN: localASN,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ReconcileNodes brings the Route Server's peerings in line with the router
// nodes.
//
// Unlike AWS and GCP there is no per-node instance attribute to set: AWS
// clears SourceDestCheck and GCP sets canIpForward, where Azure needs neither.
// So this reconciles peerings and nothing else.
func (p *Platform) ReconcileNodes(ctx context.Context, nodes []platform.RouterNode) error {
	logger := log.FromContext(ctx)

	current, err := p.rs.ListPeerings(ctx)
	if err != nil {
		return fmt.Errorf("listing Route Server peerings: %w", err)
	}

	desired := desiredPeerings(p.cfg.ClusterID, p.cfg.LocalASN, nodes)

	// Compare before writing. Both controllers watch what they write, and a
	// reconcile that rewrites an unchanged peering on every pass is how this
	// stack ended up reconciling twice a second.
	var ours []Peering
	for _, c := range current {
		if isOurPeering(c.Name, p.cfg.ClusterID) {
			ours = append(ours, c)
		}
	}
	if buildPeeringSet(ours).Equal(buildPeeringSet(desired)) {
		return nil
	}

	desiredByName := make(map[string]Peering, len(desired))
	for _, d := range desired {
		desiredByName[d.Name] = d
	}

	for _, want := range desired {
		cur, found := findPeering(current, want.Name)
		if found && cur.PeerIP == want.PeerIP && cur.PeerASN == want.PeerASN {
			continue
		}
		if err := p.rs.CreatePeering(ctx, want.Name, want.PeerIP, want.PeerASN); err != nil {
			return fmt.Errorf("creating peering %q: %w", want.Name, err)
		}
		logger.Info("Route Server peering written", "peering", want.Name, "peerIP", want.PeerIP)
	}

	// Prune only what we own. A Route Server shared with another cluster keeps
	// its peerings, which is why this filters rather than deleting everything
	// the list returned.
	for _, cur := range current {
		if !isOurPeering(cur.Name, p.cfg.ClusterID) {
			continue
		}
		if _, keep := desiredByName[cur.Name]; keep {
			continue
		}
		if err := p.rs.DeletePeering(ctx, cur.Name); err != nil {
			return fmt.Errorf("deleting stale peering %q: %w", cur.Name, err)
		}
		logger.Info("stale Route Server peering removed", "peering", cur.Name)
	}

	return nil
}

// Cleanup removes the peerings this operator created and leaves every other
// peering alone, so tearing down one cluster does not disconnect another that
// shares the Route Server.
func (p *Platform) Cleanup(ctx context.Context) error {
	current, err := p.rs.ListPeerings(ctx)
	if err != nil {
		return fmt.Errorf("listing Route Server peerings: %w", err)
	}
	for _, c := range current {
		if !isOurPeering(c.Name, p.cfg.ClusterID) {
			continue
		}
		if err := p.rs.DeletePeering(ctx, c.Name); err != nil {
			return fmt.Errorf("deleting peering %q: %w", c.Name, err)
		}
	}
	return nil
}
