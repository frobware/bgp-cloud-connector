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

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
)

// routeServerClient is the only code here that touches the Azure SDK.
// Everything above it works in terms of RouteServerTopology and Peering, so
// the reconcile logic is testable without a subscription.
type routeServerClient struct {
	resourceGroup   string
	routeServerName string
	hubs            *armnetwork.VirtualHubsClient
	connections     *armnetwork.VirtualHubBgpConnectionsClient
	connection      *armnetwork.VirtualHubBgpConnectionClient
}

func newRouteServerClient(cfg Config) (*routeServerClient, error) {
	// The default chain resolves the workload identity federation the
	// operator's ServiceAccount is set up with in cluster, and a developer's
	// az login outside it.
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("resolving Azure credentials: %w", err)
	}
	factory, err := armnetwork.NewClientFactory(cfg.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("building the Azure network client: %w", err)
	}
	return &routeServerClient{
		resourceGroup:   cfg.ResourceGroup,
		routeServerName: cfg.RouteServerName,
		hubs:            factory.NewVirtualHubsClient(),
		connections:     factory.NewVirtualHubBgpConnectionsClient(),
		connection:      factory.NewVirtualHubBgpConnectionClient(),
	}, nil
}

// GetTopology reads the addresses the Route Server peers from and the ASN it
// peers as. Azure calls the Route Server a Virtual Hub and puts both on the
// hub resource rather than on the connections.
func (c *routeServerClient) GetTopology(ctx context.Context) (*RouteServerTopology, error) {
	resp, err := c.hubs.Get(ctx, c.resourceGroup, c.routeServerName, nil)
	if err != nil {
		return nil, err
	}

	topology := &RouteServerTopology{}
	if resp.Properties == nil {
		return topology, nil
	}
	if resp.Properties.VirtualRouterAsn != nil {
		topology.ASN = *resp.Properties.VirtualRouterAsn
	}
	for _, ip := range resp.Properties.VirtualRouterIPs {
		if ip != nil && *ip != "" {
			topology.Addresses = append(topology.Addresses, *ip)
		}
	}
	return topology, nil
}

func (c *routeServerClient) ListPeerings(ctx context.Context) ([]Peering, error) {
	var out []Peering
	pager := c.connections.NewListPager(c.resourceGroup, c.routeServerName, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, conn := range page.Value {
			if conn == nil || conn.Name == nil {
				continue
			}
			p := Peering{Name: *conn.Name}
			if conn.Properties != nil {
				if conn.Properties.PeerIP != nil {
					p.PeerIP = *conn.Properties.PeerIP
				}
				if conn.Properties.PeerAsn != nil {
					p.PeerASN = *conn.Properties.PeerAsn
				}
				if conn.Properties.ProvisioningState != nil {
					p.ProvisioningState = string(*conn.Properties.ProvisioningState)
				}
				if conn.Properties.ConnectionState != nil {
					p.ConnectionState = string(*conn.Properties.ConnectionState)
				}
			}
			out = append(out, p)
		}
	}
	return out, nil
}

func (c *routeServerClient) CreatePeering(ctx context.Context, name, peerIP string, peerASN int64) error {
	// The name is the path parameter; the body carries properties only. The
	// rh-mobb implementation that has run against a real Route Server omits
	// it here, and an unexercised field is not worth keeping on a guess.
	conn := armnetwork.BgpConnection{
		Properties: &armnetwork.BgpConnectionProperties{
			PeerIP:  &peerIP,
			PeerAsn: &peerASN,
		},
	}
	poller, err := c.connection.BeginCreateOrUpdate(ctx, c.resourceGroup, c.routeServerName, name, conn, nil)
	if err != nil {
		return err
	}
	_, err = poller.PollUntilDone(ctx, nil)
	return err
}

func (c *routeServerClient) DeletePeering(ctx context.Context, name string) error {
	poller, err := c.connection.BeginDelete(ctx, c.resourceGroup, c.routeServerName, name, nil)
	if err != nil {
		return err
	}
	_, err = poller.PollUntilDone(ctx, nil)
	return err
}
