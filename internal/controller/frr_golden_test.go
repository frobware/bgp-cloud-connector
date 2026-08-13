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

package controller

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	networkingv1alpha1 "github.com/openshift/bgp-cloud-connector/api/v1alpha1"
	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

// updateGolden rewrites the expected files instead of asserting against them.
// Run with: go test ./internal/controller -run Golden -update-golden
var updateGolden = flag.Bool("update-golden", false, "rewrite golden files")

// These tests pin the exact FRRConfiguration objects the controller generates.
// They exist so that reshaping the platform abstraction for a second cloud can
// be shown not to change what AWS produces.

func frrTestClient(t *testing.T) client.Client {
	t.Helper()
	s := testScheme()
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfiguration"), &unstructured.Unstructured{})
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfigurationList"), &unstructured.UnstructuredList{})

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: FRRNamespace}}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(ns).Build()
}

// dumpFRRConfigurations returns every managed FRRConfiguration as stable,
// indented JSON with server-populated fields removed.
func dumpFRRConfigurations(t *testing.T, ctx context.Context, c client.Client) string {
	t.Helper()

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.List(ctx, list,
		client.InNamespace(FRRNamespace),
		client.MatchingLabels{LabelManagedBy: LabelManagedByVal},
	); err != nil {
		t.Fatalf("listing FRRConfigurations: %v", err)
	}

	items := make([]map[string]any, 0, len(list.Items))
	for i := range list.Items {
		obj := list.Items[i].DeepCopy()
		meta, _, _ := unstructured.NestedMap(obj.Object, "metadata")
		for _, volatile := range []string{"resourceVersion", "creationTimestamp", "managedFields", "uid", "generation"} {
			delete(meta, volatile)
		}
		obj.Object["metadata"] = meta
		items = append(items, obj.Object)
	}
	sort.Slice(items, func(i, j int) bool {
		mi := items[i]["metadata"].(map[string]any)
		mj := items[j]["metadata"].(map[string]any)
		return mi["name"].(string) < mj["name"].(string)
	})

	out, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	return string(out) + "\n"
}

func assertGolden(t *testing.T, name, got string) {
	t.Helper()

	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("creating testdata: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("writing golden: %v", err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden %s (run with -update-golden to create): %v", path, err)
	}
	if got != string(want) {
		t.Errorf("FRRConfiguration output differs from %s\n--- got ---\n%s\n--- want ---\n%s", path, got, string(want))
	}
}

func goldenConfig(liveness networkingv1alpha1.LivenessDetectionType) *networkingv1alpha1.CUDNBgpConfig {
	return &networkingv1alpha1.CUDNBgpConfig{
		ObjectMeta: metav1.ObjectMeta{Name: SingletonName},
		Spec: networkingv1alpha1.CUDNBgpConfigSpec{
			Platform:           networkingv1alpha1.PlatformManual,
			RouterNodeSelector: map[string]string{"bgp-router": "true"},
			BGP: networkingv1alpha1.BGPConfig{
				LocalASN:          65001,
				LivenessDetection: liveness,
			},
		},
	}
}

// TestGoldenFRRFromSpec pins the statically configured path, where the
// availability zones and their neighbours come from the CR.
func TestGoldenFRRFromSpec(t *testing.T) {
	ctx := context.Background()
	c := frrTestClient(t)

	config := goldenConfig(networkingv1alpha1.LivenessDetectionBGPKeepalive)
	config.Spec.BGP.PeerGroups = []networkingv1alpha1.PeerGroup{
		{
			NodeSelector: map[string]string{"topology.kubernetes.io/zone": "eu-central-1a"},
			Neighbors: []networkingv1alpha1.BGPNeighbor{
				{Address: "10.0.1.10", RemoteASN: 65000},
				{Address: "10.0.1.11", RemoteASN: 65000},
			},
		},
		{
			NodeSelector: map[string]string{"topology.kubernetes.io/zone": "eu-central-1b"},
			Neighbors: []networkingv1alpha1.BGPNeighbor{
				{Address: "10.0.2.10", RemoteASN: 65000},
			},
		},
	}

	if err := EnsureFRRConfigurations(ctx, c, config); err != nil {
		t.Fatalf("EnsureFRRConfigurations: %v", err)
	}
	assertGolden(t, "frr-from-spec.json", dumpFRRConfigurations(t, ctx, c))
}

// TestGoldenFRRFromSpecBFD pins the same path with BFD liveness detection,
// which adds a bfdProfile to each neighbour and a bfdProfiles block.
func TestGoldenFRRFromSpecBFD(t *testing.T) {
	ctx := context.Background()
	c := frrTestClient(t)

	config := goldenConfig(networkingv1alpha1.LivenessDetectionBFD)
	config.Spec.BGP.PeerGroups = []networkingv1alpha1.PeerGroup{
		{
			NodeSelector: map[string]string{"topology.kubernetes.io/zone": "eu-central-1a"},
			Neighbors: []networkingv1alpha1.BGPNeighbor{
				{Address: "10.0.1.10", RemoteASN: 65000},
			},
		},
	}

	if err := EnsureFRRConfigurations(ctx, c, config); err != nil {
		t.Fatalf("EnsureFRRConfigurations: %v", err)
	}
	assertGolden(t, "frr-from-spec-bfd.json", dumpFRRConfigurations(t, ctx, c))
}

// TestGoldenFRRFromDiscovery pins the AWS discovery path, where the zones and
// neighbours are derived from Route Server endpoints.
func TestGoldenFRRFromDiscovery(t *testing.T) {
	ctx := context.Background()
	c := frrTestClient(t)

	config := goldenConfig(networkingv1alpha1.LivenessDetectionBGPKeepalive)
	groups := []platform.PeerGroup{
		{
			Key:          "eu-central-1a",
			NodeSelector: map[string]string{"topology.kubernetes.io/zone": "eu-central-1a"},
			Neighbors: []platform.DiscoveredNeighbor{
				{Address: "10.0.1.10", ASN: 65000},
				{Address: "10.0.1.11", ASN: 65000},
			},
		},
		{
			Key:          "eu-central-1b",
			NodeSelector: map[string]string{"topology.kubernetes.io/zone": "eu-central-1b"},
			Neighbors: []platform.DiscoveredNeighbor{
				{Address: "10.0.2.10", ASN: 65000},
			},
		},
	}

	count, err := EnsureFRRConfigurationsFromGroups(ctx, c, config, groups)
	if err != nil {
		t.Fatalf("EnsureFRRConfigurationsFromGroups: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 configurations, got %d", count)
	}
	assertGolden(t, "frr-from-discovery.json", dumpFRRConfigurations(t, ctx, c))
}

// TestGoldenFRRRawConfig pins that a peer group carrying raw FRR directives
// emits them as spec.raw. GCP needs disable-connected-check, which the
// structured neighbour API cannot express.
func TestGoldenFRRRawConfig(t *testing.T) {
	ctx := context.Background()
	c := frrTestClient(t)

	config := goldenConfig(networkingv1alpha1.LivenessDetectionBGPKeepalive)
	groups := []platform.PeerGroup{
		{
			Key: "cloud-router",
			Neighbors: []platform.DiscoveredNeighbor{
				{Address: "10.0.1.2", ASN: 64512},
				{Address: "10.0.1.3", ASN: 64512},
			},
			RawFRRConfig: "      router bgp 65001\n" +
				"       neighbor 10.0.1.2 disable-connected-check\n" +
				"       neighbor 10.0.1.3 disable-connected-check\n",
		},
	}

	if _, err := EnsureFRRConfigurationsFromGroups(ctx, c, config, groups); err != nil {
		t.Fatalf("EnsureFRRConfigurationsFromGroups: %v", err)
	}
	assertGolden(t, "frr-raw-config.json", dumpFRRConfigurations(t, ctx, c))
}

// TestGoldenFRRMultiHop pins the per-neighbour eBGP multihop option.
//
// Azure Route Server peers are not on the node's link, so its working
// configuration carries ebgpMultiHop on every neighbour. Unlike GCP's
// disable-connected-check, frr-k8s expresses this as a structured field, so it
// belongs on the neighbour rather than in a raw block.
func TestGoldenFRRMultiHop(t *testing.T) {
	ctx := context.Background()
	c := frrTestClient(t)

	config := goldenConfig(networkingv1alpha1.LivenessDetectionBGPKeepalive)
	groups := []platform.PeerGroup{
		{
			Key: "route-server",
			Neighbors: []platform.DiscoveredNeighbor{
				{Address: "10.0.1.4", ASN: 65515, EBGPMultiHop: true},
				{Address: "10.0.1.5", ASN: 65515, EBGPMultiHop: true},
			},
		},
	}

	if _, err := EnsureFRRConfigurationsFromGroups(ctx, c, config, groups); err != nil {
		t.Fatalf("EnsureFRRConfigurationsFromGroups: %v", err)
	}
	assertGolden(t, "frr-multihop.json", dumpFRRConfigurations(t, ctx, c))
}

// TestGoldenFRRMultiHopOmittedWhenUnset pins that a neighbour not asking for
// multihop emits no ebgpMultiHop key at all. frr-k8s defaults it to false, and
// writing it explicitly would churn every FRRConfiguration on both existing
// clouds for no behavioural change.
func TestGoldenFRRMultiHopOmittedWhenUnset(t *testing.T) {
	ctx := context.Background()
	c := frrTestClient(t)

	config := goldenConfig(networkingv1alpha1.LivenessDetectionBGPKeepalive)
	groups := []platform.PeerGroup{
		{
			Key:       "route-server",
			Neighbors: []platform.DiscoveredNeighbor{{Address: "10.0.1.6", ASN: 64512}},
		},
	}

	if _, err := EnsureFRRConfigurationsFromGroups(ctx, c, config, groups); err != nil {
		t.Fatalf("EnsureFRRConfigurationsFromGroups: %v", err)
	}
	if got := dumpFRRConfigurations(t, ctx, c); strings.Contains(got, "ebgpMultiHop") {
		t.Errorf("expected no ebgpMultiHop key for a neighbour that did not ask for it:\n%s", got)
	}
}

// TestGoldenFRRPrunesStale pins the pruning behaviour: a configuration that is
// no longer expected must be deleted on the next pass.
func TestGoldenFRRPrunesStale(t *testing.T) {
	ctx := context.Background()
	c := frrTestClient(t)

	config := goldenConfig(networkingv1alpha1.LivenessDetectionBGPKeepalive)
	config.Spec.BGP.PeerGroups = []networkingv1alpha1.PeerGroup{
		{
			NodeSelector: map[string]string{"topology.kubernetes.io/zone": "eu-central-1a"},
			Neighbors:    []networkingv1alpha1.BGPNeighbor{{Address: "10.0.1.10", RemoteASN: 65000}},
		},
		{
			NodeSelector: map[string]string{"topology.kubernetes.io/zone": "eu-central-1b"},
			Neighbors:    []networkingv1alpha1.BGPNeighbor{{Address: "10.0.2.10", RemoteASN: 65000}},
		},
	}
	if err := EnsureFRRConfigurations(ctx, c, config); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	config.Spec.BGP.PeerGroups = config.Spec.BGP.PeerGroups[:1]
	if err := EnsureFRRConfigurations(ctx, c, config); err != nil {
		t.Fatalf("second pass: %v", err)
	}

	assertGolden(t, "frr-pruned.json", dumpFRRConfigurations(t, ctx, c))
}
