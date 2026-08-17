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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	networkingv1alpha1 "github.com/openshift/bgp-cloud-connector/api/v1alpha1"
	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

// The FRRConfiguration objects this controller writes are its actual product:
// everything else it does is in service of telling FRR what to peer with. The
// tests around them check properties one at a time, which is the right shape
// for a property but leaves the object as a whole unpinned -- a change to a
// field nobody thought to assert on passes.
//
// These record the whole object instead, so any change to what FRR is told
// shows up as a diff rather than as silence. That makes the fixtures the
// review artefact for anything touching generation: an intended change is
// regenerated and read, and an unintended one fails.
//
//	go test ./internal/controller/ -run TestFRRGolden -update
//
// Regenerating is not the same as approving. The diff is the thing to read.
var updateGolden = flag.Bool("update", false, "rewrite the golden files from current behaviour")

// goldenDir holds one file per case, named for it.
const goldenDir = "testdata"

// assertGolden compares every managed FRRConfiguration in the cluster against
// the recorded fixture.
//
// The objects are read back from the client rather than captured on the way
// in, so what is pinned is what a cluster would hold, and they are sorted by
// name because List has no order of its own.
func assertGolden(t *testing.T, ctx context.Context, c client.Client, name string) {
	t.Helper()

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.List(ctx, list, client.InNamespace(FRRNamespace)); err != nil {
		t.Fatalf("listing FRRConfigurations: %v", err)
	}

	items := make([]map[string]interface{}, 0, len(list.Items))
	for i := range list.Items {
		obj := list.Items[i].DeepCopy().Object
		// resourceVersion is the fake client's bookkeeping, not the
		// operator's output, and it would churn the fixture on every run.
		if md, ok := obj["metadata"].(map[string]interface{}); ok {
			delete(md, "resourceVersion")
		}
		items = append(items, obj)
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i]["metadata"].(map[string]interface{})["name"].(string) <
			items[j]["metadata"].(map[string]interface{})["name"].(string)
	})

	got, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	got = append(got, '\n')

	path := filepath.Join(goldenDir, name+".json")
	if *updateGolden {
		if err := os.MkdirAll(goldenDir, 0o755); err != nil {
			t.Fatalf("creating %s: %v", goldenDir, err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s (run with -update to create it): %v", path, err)
	}
	if string(got) != string(want) {
		t.Errorf("generated FRRConfigurations differ from %s.\n--- want ---\n%s\n--- got ---\n%s",
			path, want, got)
	}
}

func goldenConfig(liveness networkingv1alpha1.LivenessDetectionType) *networkingv1alpha1.CUDNBgpConfig {
	return &networkingv1alpha1.CUDNBgpConfig{
		ObjectMeta: metav1.ObjectMeta{Name: SingletonName},
		Spec: networkingv1alpha1.CUDNBgpConfigSpec{
			BGP: networkingv1alpha1.BGPConfig{
				LocalASN:          65001,
				LivenessDetection: liveness,
				PeerGroups: []networkingv1alpha1.PeerGroup{
					{
						NodeSelector: map[string]string{"topology.kubernetes.io/zone": "us-east-1a"},
						Neighbors: []networkingv1alpha1.BGPNeighbor{
							{Address: "10.0.1.47", RemoteASN: 64512},
							{Address: "10.0.1.183", RemoteASN: 64512},
						},
					},
					{
						NodeSelector: map[string]string{"topology.kubernetes.io/zone": "us-east-1b"},
						Neighbors: []networkingv1alpha1.BGPNeighbor{
							{Address: "10.0.2.47", RemoteASN: 64512},
						},
					},
				},
			},
			RouterNodeSelector: map[string]string{"networking.openshift.io/cudn-bgp-router": ""},
		},
	}
}

func goldenClient(objs ...client.Object) client.Client {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: FRRNamespace}}
	return fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(append([]client.Object{ns}, objs...)...).
		Build()
}

// TestFRRGolden_FromSpec pins what a configuration that declares its own
// neighbours produces.
func TestFRRGolden_FromSpec(t *testing.T) {
	ctx := context.Background()
	c := goldenClient()

	if err := EnsureFRRConfigurations(ctx, c, goldenConfig(networkingv1alpha1.LivenessDetectionBGPKeepalive)); err != nil {
		t.Fatalf("EnsureFRRConfigurations: %v", err)
	}
	assertGolden(t, ctx, c, "frr-from-spec")
}

// TestFRRGolden_FromSpecBFD pins the difference BFD makes, which is a
// separate profile and a per-neighbour reference to it.
func TestFRRGolden_FromSpecBFD(t *testing.T) {
	ctx := context.Background()
	c := goldenClient()

	if err := EnsureFRRConfigurations(ctx, c, goldenConfig(networkingv1alpha1.LivenessDetectionBFD)); err != nil {
		t.Fatalf("EnsureFRRConfigurations: %v", err)
	}
	assertGolden(t, ctx, c, "frr-from-spec-bfd")
}

// TestFRRGolden_FromDiscovery pins the other entry point, where the neighbours
// come from the cloud rather than from the spec. The two paths must agree on
// the object they produce, and comparing the two fixtures is how that is
// checked.
func TestFRRGolden_FromDiscovery(t *testing.T) {
	ctx := context.Background()
	c := goldenClient()

	config := &networkingv1alpha1.CUDNBgpConfig{
		ObjectMeta: metav1.ObjectMeta{Name: SingletonName},
		Spec: networkingv1alpha1.CUDNBgpConfigSpec{
			BGP: networkingv1alpha1.BGPConfig{
				LocalASN:          65001,
				LivenessDetection: networkingv1alpha1.LivenessDetectionBGPKeepalive,
			},
			RouterNodeSelector: map[string]string{"networking.openshift.io/cudn-bgp-router": ""},
			AWS: &networkingv1alpha1.AWSConfig{
				Region:         "us-east-1",
				RouteServerIDs: []string{"rs-1"},
			},
		},
	}

	groups := []platform.PeerGroup{
		{
			Key:          "us-east-1a",
			NodeSelector: map[string]string{"topology.kubernetes.io/zone": "us-east-1a"},
			Neighbors: []platform.DiscoveredNeighbor{
				{Address: "10.0.1.47", ASN: 64512},
				{Address: "10.0.1.183", ASN: 64512},
			},
		},
		{
			Key:          "us-east-1b",
			NodeSelector: map[string]string{"topology.kubernetes.io/zone": "us-east-1b"},
			Neighbors: []platform.DiscoveredNeighbor{
				{Address: "10.0.2.47", ASN: 64512},
			},
		},
	}

	if _, err := EnsureFRRConfigurationsFromGroups(ctx, c, config, groups); err != nil {
		t.Fatalf("EnsureFRRConfigurationsFromGroups: %v", err)
	}
	assertGolden(t, ctx, c, "frr-from-discovery")
}

// TestFRRGolden_Pruned pins what is left after the peering plan shrinks. The
// stale configuration must go and the surviving one must be untouched, which
// is the half a count-based assertion misses.
func TestFRRGolden_Pruned(t *testing.T) {
	ctx := context.Background()

	stale := &unstructured.Unstructured{}
	stale.SetGroupVersionKind(FRRConfigurationGVK)
	stale.SetName(FRRConfigNamePrefix + "2")
	stale.SetNamespace(FRRNamespace)
	stale.SetLabels(map[string]string{LabelManagedBy: LabelManagedByVal})
	c := goldenClient(stale)

	config := goldenConfig(networkingv1alpha1.LivenessDetectionBGPKeepalive)
	config.Spec.BGP.PeerGroups = config.Spec.BGP.PeerGroups[:1]

	if err := EnsureFRRConfigurations(ctx, c, config); err != nil {
		t.Fatalf("EnsureFRRConfigurations: %v", err)
	}
	assertGolden(t, ctx, c, "frr-pruned")
}
