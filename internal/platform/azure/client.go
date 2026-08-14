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

// nicClient is the network interface half of the Azure API, kept separate from
// routeServerClient because it works on a different resource in a possibly
// different resource group: the Route Server lives where spec.azure says, and
// a node's interface lives wherever that node does.
type nicClient struct {
	interfaces *armnetwork.InterfacesClient
}

func newNICClient(cfg Config) (*nicClient, error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("resolving Azure credentials: %w", err)
	}
	factory, err := armnetwork.NewClientFactory(cfg.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("building the Azure network client: %w", err)
	}
	return &nicClient{interfaces: factory.NewInterfacesClient()}, nil
}

// ListNICs returns every network interface in a resource group.
//
// By resource group rather than by virtual machine, because Azure offers no
// way to ask a VM for its interfaces without the compute API, and reaching for
// armcompute to read one field is a dependency for nothing. The interfaces
// carry the VM they are attached to, so the join happens here.
func (c *nicClient) ListNICs(ctx context.Context, resourceGroup string) ([]NIC, error) {
	var out []NIC
	pager := c.interfaces.NewListPager(resourceGroup, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, iface := range page.Value {
			if iface == nil || iface.Name == nil {
				continue
			}
			nic := NIC{Name: *iface.Name, ResourceGroup: resourceGroup}
			if iface.Properties != nil {
				if iface.Properties.VirtualMachine != nil && iface.Properties.VirtualMachine.ID != nil {
					nic.VMID = *iface.Properties.VirtualMachine.ID
				}
				if iface.Properties.EnableIPForwarding != nil {
					nic.IPForwarding = *iface.Properties.EnableIPForwarding
				}
			}
			out = append(out, nic)
		}
	}
	return out, nil
}

// EnableIPForwarding sets enableIPForwarding on one interface.
//
// Read, modify, write the whole object, because Azure's create-or-update has
// no patch semantics: what the body omits is what the interface loses,
// including its IP configurations. Reading first is not a courtesy, it is the
// only safe way to change one field. This is the same shape as the GCP
// platform's whole-instance PUT for canIpForward, and for the same reason.
func (c *nicClient) EnableIPForwarding(ctx context.Context, resourceGroup, name string) error {
	current, err := c.interfaces.Get(ctx, resourceGroup, name, nil)
	if err != nil {
		return err
	}
	if current.Properties == nil {
		return fmt.Errorf("network interface %q has no properties to update", name)
	}

	enabled := true
	current.Properties.EnableIPForwarding = &enabled

	poller, err := c.interfaces.BeginCreateOrUpdate(ctx, resourceGroup, name, current.Interface, nil)
	if err != nil {
		return err
	}
	_, err = poller.PollUntilDone(ctx, nil)
	return err
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
