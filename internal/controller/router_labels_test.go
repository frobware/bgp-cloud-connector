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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	networkingv1alpha1 "github.com/openshift/bgp-cloud-connector/api/v1alpha1"
)

// Router node labelling is a Kubernetes concern, not a cloud one, so it lives
// in the controller and applies to every platform.

func labelledNode(name string, labels map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func nodeLabels(t *testing.T, c client.Client, name string) map[string]string {
	t.Helper()
	n := &corev1.Node{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, n); err != nil {
		t.Fatalf("get node %s: %v", name, err)
	}
	return n.Labels
}

func autoLabelConfig() *networkingv1alpha1.CUDNBgpConfig {
	config := newTestCUDNBgpConfig()
	config.Spec.RouterNodeSelector = map[string]string{"networking.openshift.io/cudn-bgp-router": ""}
	config.Spec.AutoLabelRouterNodes = &networkingv1alpha1.AutoLabelRouterNodesSpec{
		Eligible: map[string]string{"node-role.kubernetes.io/worker": ""},
		Exclude:  map[string]string{"node-role.kubernetes.io/infra": ""},
	}
	return config
}

// TestSyncRouterLabels_Disabled is the guarantee for anyone already labelling
// nodes themselves: with the feature off, the operator never writes to a Node.
func TestSyncRouterLabels_Disabled(t *testing.T) {
	config := newTestCUDNBgpConfig()
	config.Spec.RouterNodeSelector = map[string]string{"networking.openshift.io/cudn-bgp-router": ""}
	config.Spec.AutoLabelRouterNodes = nil

	worker := labelledNode("worker-a", map[string]string{"node-role.kubernetes.io/worker": ""})
	c := fake.NewClientBuilder().WithScheme(configTestScheme()).WithObjects(worker).Build()

	added, removed, err := SyncRouterLabels(context.Background(), c, config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if added != 0 || removed != 0 {
		t.Errorf("expected no changes, got added=%d removed=%d", added, removed)
	}
	if _, has := nodeLabels(t, c, "worker-a")["networking.openshift.io/cudn-bgp-router"]; has {
		t.Error("operator must not label nodes when auto-labelling is off")
	}
}

// TestSyncRouterLabels_LabelsEligible is the MachineSet case: a node that
// nobody labelled becomes a router because it is eligible.
func TestSyncRouterLabels_LabelsEligible(t *testing.T) {
	config := autoLabelConfig()

	worker := labelledNode("worker-a", map[string]string{
		"node-role.kubernetes.io/worker": "",
		"kubernetes.io/hostname":         "worker-a",
	})
	c := fake.NewClientBuilder().WithScheme(configTestScheme()).WithObjects(worker).Build()

	added, _, err := SyncRouterLabels(context.Background(), c, config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if added != 1 {
		t.Errorf("expected 1 node labelled, got %d", added)
	}

	got := nodeLabels(t, c, "worker-a")
	if _, labelled := got["networking.openshift.io/cudn-bgp-router"]; !labelled {
		t.Errorf("expected the node to carry networking.openshift.io/cudn-bgp-router, got %v", got)
	}
	if _, has := got["kubernetes.io/hostname"]; !has {
		t.Error("unrelated labels must be preserved")
	}
}

func TestSyncRouterLabels_SkipsExcluded(t *testing.T) {
	config := autoLabelConfig()

	infra := labelledNode("infra-a", map[string]string{
		"node-role.kubernetes.io/worker": "",
		"node-role.kubernetes.io/infra":  "",
	})
	master := labelledNode("master-a", map[string]string{"node-role.kubernetes.io/master": ""})
	c := fake.NewClientBuilder().WithScheme(configTestScheme()).WithObjects(infra, master).Build()

	added, _, err := SyncRouterLabels(context.Background(), c, config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if added != 0 {
		t.Errorf("expected nothing labelled, got %d", added)
	}
	for _, n := range []string{"infra-a", "master-a"} {
		if _, has := nodeLabels(t, c, n)["networking.openshift.io/cudn-bgp-router"]; has {
			t.Errorf("%s must not be labelled", n)
		}
	}
}

// TestSyncRouterLabels_RemovesFromIneligible covers a node leaving the router
// set, for example by being relabelled as infra.
func TestSyncRouterLabels_RemovesFromIneligible(t *testing.T) {
	config := autoLabelConfig()

	stale := labelledNode("was-router", map[string]string{
		"node-role.kubernetes.io/worker":          "",
		"node-role.kubernetes.io/infra":           "",
		"networking.openshift.io/cudn-bgp-router": "",
		"kubernetes.io/hostname":                  "was-router",
	})
	c := fake.NewClientBuilder().WithScheme(configTestScheme()).WithObjects(stale).Build()

	_, removed, err := SyncRouterLabels(context.Background(), c, config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if removed != 1 {
		t.Errorf("expected 1 node unlabelled, got %d", removed)
	}

	got := nodeLabels(t, c, "was-router")
	if _, has := got["networking.openshift.io/cudn-bgp-router"]; has {
		t.Error("expected the router label to be removed")
	}
	if _, has := got["kubernetes.io/hostname"]; !has {
		t.Error("unrelated labels must be preserved")
	}
}

// TestSyncRouterLabels_Idempotent stops the operator rewriting nodes on every
// resync, which would churn resourceVersion and wake every other watcher.
func TestSyncRouterLabels_Idempotent(t *testing.T) {
	config := autoLabelConfig()

	worker := labelledNode("worker-a", map[string]string{
		"node-role.kubernetes.io/worker":          "",
		"networking.openshift.io/cudn-bgp-router": "",
	})
	c := fake.NewClientBuilder().WithScheme(configTestScheme()).WithObjects(worker).Build()

	before := &corev1.Node{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "worker-a"}, before)
	rv := before.ResourceVersion

	added, removed, err := SyncRouterLabels(context.Background(), c, config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if added != 0 || removed != 0 {
		t.Errorf("expected no changes on a settled cluster, got added=%d removed=%d", added, removed)
	}

	after := &corev1.Node{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "worker-a"}, after)
	if after.ResourceVersion != rv {
		t.Errorf("expected no write, resourceVersion moved %s -> %s", rv, after.ResourceVersion)
	}
}

// TestRemoveAllRouterLabels covers teardown: labels the operator applied are
// its to clean up.
func TestRemoveAllRouterLabels(t *testing.T) {
	config := autoLabelConfig()

	a := labelledNode("worker-a", map[string]string{"node-role.kubernetes.io/worker": "", "networking.openshift.io/cudn-bgp-router": ""})
	b := labelledNode("worker-b", map[string]string{"node-role.kubernetes.io/worker": "", "networking.openshift.io/cudn-bgp-router": ""})
	c := fake.NewClientBuilder().WithScheme(configTestScheme()).WithObjects(a, b).Build()

	removed, err := RemoveAllRouterLabels(context.Background(), c, config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if removed != 2 {
		t.Errorf("expected 2 nodes unlabelled, got %d", removed)
	}
	for _, n := range []string{"worker-a", "worker-b"} {
		if _, has := nodeLabels(t, c, n)["networking.openshift.io/cudn-bgp-router"]; has {
			t.Errorf("%s still carries the router label", n)
		}
	}
}

// TestRemoveAllRouterLabels_Disabled: labels the operator never applied are
// not its to remove.
func TestRemoveAllRouterLabels_Disabled(t *testing.T) {
	config := newTestCUDNBgpConfig()
	config.Spec.RouterNodeSelector = map[string]string{"networking.openshift.io/cudn-bgp-router": ""}
	config.Spec.AutoLabelRouterNodes = nil

	a := labelledNode("worker-a", map[string]string{"networking.openshift.io/cudn-bgp-router": ""})
	c := fake.NewClientBuilder().WithScheme(configTestScheme()).WithObjects(a).Build()

	removed, err := RemoveAllRouterLabels(context.Background(), c, config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if removed != 0 {
		t.Errorf("expected nothing removed, got %d", removed)
	}
	if _, has := nodeLabels(t, c, "worker-a")["networking.openshift.io/cudn-bgp-router"]; !has {
		t.Error("a label the operator did not apply must not be removed")
	}
}
