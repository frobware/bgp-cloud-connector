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

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

// DiscoverEndpoints returns a single peer group covering every router node.
//
// An Azure Route Server is regional and every router node peers with the same
// redundant pair of addresses, so there is nothing for a zone axis to
// partition. That matches GCP; AWS, whose endpoints are per subnet and
// therefore per zone, is the one that emits a group per zone.
//
// Every neighbour asks for eBGP multihop because the Route Server is not on
// the node's link. Unlike GCP's disable-connected-check this is a structured
// field in frr-k8s, so it rides on the neighbour rather than in a raw block.
func (p *Platform) DiscoverEndpoints(ctx context.Context) (*platform.DiscoveryResult, error) {
	topology, err := p.rs.GetTopology(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading Route Server %q: %w", p.cfg.RouteServerName, err)
	}
	if len(topology.Addresses) == 0 {
		return nil, fmt.Errorf("Route Server %q has no addresses to peer with", p.cfg.RouteServerName)
	}
	if topology.ASN == 0 {
		return nil, fmt.Errorf("Route Server %q has no ASN and cannot peer", p.cfg.RouteServerName)
	}

	group := platform.PeerGroup{Key: p.cfg.RouteServerName}
	for _, addr := range topology.Addresses {
		group.Neighbors = append(group.Neighbors, platform.DiscoveredNeighbor{
			Address:      addr,
			ASN:          topology.ASN,
			EBGPMultiHop: true,
		})
	}

	return &platform.DiscoveryResult{PeerGroups: []platform.PeerGroup{group}}, nil
}
