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
)

// CheckPrerequisites reports the Azure configuration the operator relies on
// and does not create.
//
// AWS has a sharp case here -- a route server propagating to no route table
// leaves every session established while nothing in the VPC can reach a pod --
// and GCP has two, the missing BGP firewall rule and the installer's Cloud NAT
// router. No equivalent silent failure is known for Azure yet: the rh-mobb
// implementation this was taken from checks nothing, and nothing in aro-bgp
// documents a trap of that shape.
//
// So this checks what can be established from the Route Server itself and no
// more. Inventing a check we cannot demonstrate would be worse than reporting
// the two we can.
func (p *Platform) CheckPrerequisites(ctx context.Context) ([]string, error) {
	topology, err := p.rs.GetTopology(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading Route Server %q: %w", p.cfg.RouteServerName, err)
	}

	var unmet []string
	if len(topology.Addresses) == 0 {
		unmet = append(unmet, fmt.Sprintf(
			"Route Server %q reports no addresses; a Route Server peers from a redundant pair and one with none is not finished provisioning (az network routeserver show -g %s -n %s)",
			p.cfg.RouteServerName, p.cfg.ResourceGroup, p.cfg.RouteServerName))
	}
	if topology.ASN == 0 {
		unmet = append(unmet, fmt.Sprintf(
			"Route Server %q has no ASN and cannot peer (az network routeserver show -g %s -n %s)",
			p.cfg.RouteServerName, p.cfg.ResourceGroup, p.cfg.RouteServerName))
	}

	return unmet, nil
}
