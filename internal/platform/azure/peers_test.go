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
	"strings"
	"testing"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

func routerNode(name, ip string) platform.RouterNode {
	return platform.RouterNode{
		Name:       name,
		PrivateIP:  ip,
		ProviderID: "azure:///subscriptions/sub-1/resourceGroups/rg-1/providers/Microsoft.Compute/virtualMachines/" + name,
	}
}

// TestPeeringSetEqual covers the comparison that decides whether to write at
// all. Carried over from the rh-mobb implementation, which compares on name,
// address and ASN together.
func TestPeeringSetEqual(t *testing.T) {
	a := buildPeeringSet([]Peering{
		{Name: "c-bgp-10-0-0-1", PeerIP: "10.0.0.1", PeerASN: 65001},
		{Name: "c-bgp-10-0-0-2", PeerIP: "10.0.0.2", PeerASN: 65001},
	})

	for _, tc := range []struct {
		name  string
		other []Peering
		equal bool
	}{
		{"identical, order reversed", []Peering{
			{Name: "c-bgp-10-0-0-2", PeerIP: "10.0.0.2", PeerASN: 65001},
			{Name: "c-bgp-10-0-0-1", PeerIP: "10.0.0.1", PeerASN: 65001},
		}, true},
		{"one missing", []Peering{
			{Name: "c-bgp-10-0-0-1", PeerIP: "10.0.0.1", PeerASN: 65001},
		}, false},
		{"address changed", []Peering{
			{Name: "c-bgp-10-0-0-1", PeerIP: "10.0.0.9", PeerASN: 65001},
			{Name: "c-bgp-10-0-0-2", PeerIP: "10.0.0.2", PeerASN: 65001},
		}, false},
		{"ASN changed underneath us", []Peering{
			{Name: "c-bgp-10-0-0-1", PeerIP: "10.0.0.1", PeerASN: 64512},
			{Name: "c-bgp-10-0-0-2", PeerIP: "10.0.0.2", PeerASN: 65001},
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := a.Equal(buildPeeringSet(tc.other)); got != tc.equal {
				t.Errorf("Equal = %v, want %v", got, tc.equal)
			}
		})
	}
}

// TestFindPeering covers the lookup that decides create versus leave alone.
func TestFindPeering(t *testing.T) {
	current := []Peering{
		{Name: "c-bgp-10-0-0-1", PeerIP: "10.0.0.1", PeerASN: 65001},
		{Name: "c-bgp-10-0-0-2", PeerIP: "10.0.0.2", PeerASN: 65001},
	}

	got, ok := findPeering(current, "c-bgp-10-0-0-2")
	if !ok {
		t.Fatal("expected to find the peering")
	}
	if got.PeerIP != "10.0.0.2" {
		t.Errorf("found the wrong peering: %+v", got)
	}
	if _, ok := findPeering(current, "nope"); ok {
		t.Error("expected no match for an absent name")
	}
}

// TestPeeringName_EncodesAddressNotPosition pins the property that the GCP bug
// was about: adding a node that sorts first must not renumber the others.
func TestPeeringName_EncodesAddressNotPosition(t *testing.T) {
	before := desiredPeerings("cluster", 65001, []platform.RouterNode{
		routerNode("worker-b", "10.0.0.2"),
		routerNode("worker-c", "10.0.0.3"),
	})

	after := desiredPeerings("cluster", 65001, []platform.RouterNode{
		routerNode("worker-a", "10.0.0.1"),
		routerNode("worker-b", "10.0.0.2"),
		routerNode("worker-c", "10.0.0.3"),
	})

	existing := map[string]string{}
	for _, p := range before {
		existing[p.PeerIP] = p.Name
	}
	for _, p := range after {
		if was, ok := existing[p.PeerIP]; ok && was != p.Name {
			t.Errorf("peering for %s was renamed %q -> %q by a scale up", p.PeerIP, was, p.Name)
		}
	}
	if len(after) != 3 {
		t.Errorf("expected 3 peerings after scale up, got %d", len(after))
	}
}

func TestPeeringName_TruncatesLongClusterID(t *testing.T) {
	name := peeringName(strings.Repeat("x", 120), "10.0.0.1")
	if len(name) > maxPeeringNameLength {
		t.Errorf("name is %d characters, over Azure's limit of %d: %q", len(name), maxPeeringNameLength, name)
	}
	if !strings.HasSuffix(name, "-10-0-0-1") {
		t.Errorf("truncation ate the address, which is the part that identifies the node: %q", name)
	}
}

func TestIsOurPeering(t *testing.T) {
	if !isOurPeering("cluster-bgp-10-0-0-1", "cluster") {
		t.Error("expected our own peering to be recognised")
	}
	if isOurPeering("someone-else", "cluster") {
		t.Error("a foreign peering must not be claimed")
	}
	// Without the trailing separator, "cluster" would claim "cluster-two"'s.
	if isOurPeering("cluster-two-bgp-10-0-0-1", "cluster") {
		t.Error("a peering belonging to a similarly named cluster must not be claimed")
	}
}

func TestReconcileNodes_CreatesPeerings(t *testing.T) {
	f := &fakeRouteServer{}
	p := testPlatform(f)

	err := p.ReconcileNodes(context.Background(), []platform.RouterNode{
		routerNode("worker-a", "10.0.0.1"),
		routerNode("worker-b", "10.0.0.2"),
	})
	if err != nil {
		t.Fatalf("ReconcileNodes: %v", err)
	}
	if len(f.created) != 2 {
		t.Fatalf("expected 2 peerings created, got %d", len(f.created))
	}
	for _, c := range f.created {
		if c.PeerASN != 65001 {
			t.Errorf("peering %s created at ASN %d, want the cluster's 65001", c.Name, c.PeerASN)
		}
	}
}

// TestReconcileNodes_NoWriteWhenUnchanged is the no-churn property. Both
// controllers watch what they write, so a pass that rewrites an unchanged
// peering is how this stack reconciled twice a second.
func TestReconcileNodes_NoWriteWhenUnchanged(t *testing.T) {
	nodes := []platform.RouterNode{routerNode("worker-a", "10.0.0.1")}
	f := &fakeRouteServer{
		peerings: desiredPeerings(testConfig().ClusterID, 65001, nodes),
	}
	p := testPlatform(f)

	if err := p.ReconcileNodes(context.Background(), nodes); err != nil {
		t.Fatalf("ReconcileNodes: %v", err)
	}
	if len(f.created) != 0 || len(f.deleted) != 0 {
		t.Errorf("expected no writes on an unchanged Route Server, created=%v deleted=%v", f.created, f.deleted)
	}
}

func TestReconcileNodes_PrunesStale(t *testing.T) {
	f := &fakeRouteServer{
		peerings: []Peering{
			{Name: peeringName("cluster-abc", "10.0.0.9"), PeerIP: "10.0.0.9", PeerASN: 65001},
		},
	}
	p := testPlatform(f)

	if err := p.ReconcileNodes(context.Background(), []platform.RouterNode{routerNode("worker-a", "10.0.0.1")}); err != nil {
		t.Fatalf("ReconcileNodes: %v", err)
	}
	if len(f.deleted) != 1 || f.deleted[0] != peeringName("cluster-abc", "10.0.0.9") {
		t.Errorf("expected the stale peering to be pruned, deleted=%v", f.deleted)
	}
}

// TestReconcileNodes_LeavesForeignPeeringsAlone pins the difference from the
// implementation this was taken from, which deletes every peering on the Route
// Server. Two clusters can share one, and tearing down either must not
// disconnect the other.
func TestReconcileNodes_LeavesForeignPeeringsAlone(t *testing.T) {
	f := &fakeRouteServer{
		peerings: []Peering{
			{Name: "other-cluster-bgp-10-9-9-9", PeerIP: "10.9.9.9", PeerASN: 65002},
		},
	}
	p := testPlatform(f)

	if err := p.ReconcileNodes(context.Background(), []platform.RouterNode{routerNode("worker-a", "10.0.0.1")}); err != nil {
		t.Fatalf("ReconcileNodes: %v", err)
	}
	for _, d := range f.deleted {
		if d == "other-cluster-bgp-10-9-9-9" {
			t.Error("deleted a peering belonging to another cluster")
		}
	}
}

func TestReconcileNodes_SkipsNodeWithNoAddress(t *testing.T) {
	f := &fakeRouteServer{}
	p := testPlatform(f)

	if err := p.ReconcileNodes(context.Background(), []platform.RouterNode{
		routerNode("worker-a", "10.0.0.1"),
		{Name: "worker-b", ProviderID: "azure:///x"},
	}); err != nil {
		t.Fatalf("ReconcileNodes: %v", err)
	}
	if len(f.created) != 1 {
		t.Errorf("expected the addressless node to be skipped, created=%v", f.created)
	}
}

func TestCleanup_DeletesOnlyOurs(t *testing.T) {
	f := &fakeRouteServer{
		peerings: []Peering{
			{Name: peeringName("cluster-abc", "10.0.0.1"), PeerIP: "10.0.0.1", PeerASN: 65001},
			{Name: "other-cluster-bgp-10-9-9-9", PeerIP: "10.9.9.9", PeerASN: 65002},
		},
	}
	p := testPlatform(f)

	if err := p.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if len(f.deleted) != 1 || f.deleted[0] != peeringName("cluster-abc", "10.0.0.1") {
		t.Errorf("Cleanup should remove ours and only ours, deleted=%v", f.deleted)
	}
}

func TestCheckPrerequisites_Satisfied(t *testing.T) {
	p := testPlatform(&fakeRouteServer{
		topology: &RouteServerTopology{ASN: 65515, Addresses: []string{"10.0.1.4", "10.0.1.5"}},
	})

	unmet, err := p.CheckPrerequisites(context.Background())
	if err != nil {
		t.Fatalf("CheckPrerequisites: %v", err)
	}
	if len(unmet) != 0 {
		t.Errorf("expected nothing unmet, got %v", unmet)
	}
}

func TestCheckPrerequisites_ReportsWithRemedy(t *testing.T) {
	p := testPlatform(&fakeRouteServer{
		topology: &RouteServerTopology{},
	})

	unmet, err := p.CheckPrerequisites(context.Background())
	if err != nil {
		t.Fatalf("CheckPrerequisites: %v", err)
	}
	if len(unmet) != 2 {
		t.Fatalf("expected both the address and ASN problems, got %v", unmet)
	}
	for _, u := range unmet {
		if !strings.Contains(u, "rs-1") {
			t.Errorf("unmet line does not name the Route Server: %q", u)
		}
		if !strings.Contains(u, "az network") {
			t.Errorf("unmet line carries no remedy: %q", u)
		}
	}
}

func TestPlatformSatisfiesInterface(t *testing.T) {
	var _ platform.CloudPlatform = (*Platform)(nil)
}
