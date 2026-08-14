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
	"errors"
	"strings"
	"testing"
)

type fakeRouteServer struct {
	topology    *RouteServerTopology
	topologyErr error
	peerings    []Peering
	created     []Peering
	deleted     []string
}

func (f *fakeRouteServer) GetTopology(context.Context) (*RouteServerTopology, error) {
	if f.topologyErr != nil {
		return nil, f.topologyErr
	}
	return f.topology, nil
}

func (f *fakeRouteServer) ListPeerings(context.Context) ([]Peering, error) {
	return f.peerings, nil
}

func (f *fakeRouteServer) CreatePeering(_ context.Context, name, peerIP string, peerASN int64) error {
	f.created = append(f.created, Peering{Name: name, PeerIP: peerIP, PeerASN: peerASN})
	return nil
}

func (f *fakeRouteServer) DeletePeering(_ context.Context, name string) error {
	f.deleted = append(f.deleted, name)
	return nil
}

func testConfig() Config {
	return Config{
		SubscriptionID:  "sub-1",
		ResourceGroup:   "rg-1",
		RouteServerName: "rs-1",
		LocalASN:        65001,
		ClusterID:       "cluster-abc",
	}
}

// testPlatform builds a Platform with both fakes. The router nodes' interfaces
// already permit forwarding, so a peering test exercises the peering path and
// nothing else; nodes_test.go drives the per-node path on its own.
func testPlatform(rs routeServerAPI) *Platform {
	return &Platform{cfg: testConfig(), rs: rs, nics: testNICs()}
}

func testNICs() *fakeNICs {
	var nics []NIC
	for _, name := range []string{"worker-a", "worker-b", "worker-c"} {
		nics = append(nics, NIC{
			Name:          name + "-nic",
			ResourceGroup: "rg-1",
			VMID:          "/subscriptions/sub-1/resourceGroups/rg-1/providers/Microsoft.Compute/virtualMachines/" + name,
			IPForwarding:  true,
		})
	}
	return &fakeNICs{nics: nics}
}

// TestDiscoverEndpoints_SingleGroup pins that Azure yields one peer group for
// every router node rather than one per zone. The Route Server is regional and
// every node peers with the same pair of addresses.
func TestDiscoverEndpoints_SingleGroup(t *testing.T) {
	p := testPlatform(&fakeRouteServer{
		topology: &RouteServerTopology{ASN: 65515, Addresses: []string{"10.0.1.4", "10.0.1.5"}},
	})

	result, err := p.DiscoverEndpoints(context.Background())
	if err != nil {
		t.Fatalf("DiscoverEndpoints: %v", err)
	}
	if len(result.PeerGroups) != 1 {
		t.Fatalf("expected one peer group, got %d", len(result.PeerGroups))
	}
	group := result.PeerGroups[0]
	if group.NodeSelector != nil {
		t.Errorf("expected no node selector, so every router node is covered, got %v", group.NodeSelector)
	}
	if len(group.Neighbors) != 2 {
		t.Fatalf("expected the redundant pair, got %d", len(group.Neighbors))
	}
	for _, n := range group.Neighbors {
		if n.ASN != 65515 {
			t.Errorf("neighbour %s has ASN %d, want the Route Server's 65515", n.Address, n.ASN)
		}
	}
}

// TestDiscoverEndpoints_RequestsMultiHop pins the one thing Azure needs that
// neither other cloud does. The Route Server is not on the node's link, so
// without ebgpMultiHop the session never establishes.
func TestDiscoverEndpoints_RequestsMultiHop(t *testing.T) {
	p := testPlatform(&fakeRouteServer{
		topology: &RouteServerTopology{ASN: 65515, Addresses: []string{"10.0.1.4", "10.0.1.5"}},
	})

	result, err := p.DiscoverEndpoints(context.Background())
	if err != nil {
		t.Fatalf("DiscoverEndpoints: %v", err)
	}
	for _, n := range result.PeerGroups[0].Neighbors {
		if !n.EBGPMultiHop {
			t.Errorf("neighbour %s did not ask for eBGP multihop", n.Address)
		}
	}
}

// TestDiscoverEndpoints_NoRawConfig pins that Azure needs no raw FRR block.
// The raw escape hatch is for directives frr-k8s cannot express, and Azure's
// requirement is a structured field.
func TestDiscoverEndpoints_NoRawConfig(t *testing.T) {
	p := testPlatform(&fakeRouteServer{
		topology: &RouteServerTopology{ASN: 65515, Addresses: []string{"10.0.1.4"}},
	})

	result, err := p.DiscoverEndpoints(context.Background())
	if err != nil {
		t.Fatalf("DiscoverEndpoints: %v", err)
	}
	if raw := result.PeerGroups[0].RawFRRConfig; raw != "" {
		t.Errorf("expected no raw block, got:\n%s", raw)
	}
}

// TestDiscoverEndpoints_NoAddresses rejects a Route Server with nothing to
// peer with, rather than returning an empty plan that would silently generate
// an FRRConfiguration with no neighbours.
func TestDiscoverEndpoints_NoAddresses(t *testing.T) {
	p := testPlatform(&fakeRouteServer{
		topology: &RouteServerTopology{ASN: 65515},
	})

	_, err := p.DiscoverEndpoints(context.Background())
	if err == nil {
		t.Fatal("expected a Route Server with no addresses to be rejected")
	}
	if !strings.Contains(err.Error(), "no addresses") {
		t.Errorf("error should say what is wrong, got: %v", err)
	}
}

// TestDiscoverEndpoints_NoASN rejects a Route Server that cannot peer. This is
// the Azure counterpart of GCP's guard against the installer's Cloud NAT
// router.
func TestDiscoverEndpoints_NoASN(t *testing.T) {
	p := testPlatform(&fakeRouteServer{
		topology: &RouteServerTopology{Addresses: []string{"10.0.1.4"}},
	})

	_, err := p.DiscoverEndpoints(context.Background())
	if err == nil {
		t.Fatal("expected a Route Server with no ASN to be rejected")
	}
	if !strings.Contains(err.Error(), "no ASN") {
		t.Errorf("error should say what is wrong, got: %v", err)
	}
}

func TestDiscoverEndpoints_TopologyError(t *testing.T) {
	p := testPlatform(&fakeRouteServer{
		topologyErr: errors.New("boom"),
	})

	if _, err := p.DiscoverEndpoints(context.Background()); err == nil {
		t.Fatal("expected the read failure to propagate")
	}
}
