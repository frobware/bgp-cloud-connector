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
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	networkingv1alpha1 "github.com/openshift/bgp-cloud-connector/api/v1alpha1"
	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

var errStub = errors.New("stub failure")

// --- NodeLifecycle ---

// mockLifecyclePlatform is a platform that also holds terminating nodes. It
// records the order of calls so the controller's sequencing can be asserted.
type mockLifecyclePlatform struct {
	mockPlatform

	terminating  []string // node names to report as terminating
	calls        []string
	heldOnHold   []platform.RouterNode
	releasedWith []platform.RouterNode
	holdErr      error
}

func (m *mockLifecyclePlatform) HoldTerminating(_ context.Context, nodes []platform.RouterNode) ([]platform.RouterNode, error) {
	m.calls = append(m.calls, "hold")
	if m.holdErr != nil {
		return nil, m.holdErr
	}
	names := make(map[string]struct{}, len(m.terminating))
	for _, n := range m.terminating {
		names[n] = struct{}{}
	}
	var held []platform.RouterNode
	for _, n := range nodes {
		if _, ok := names[n.Name]; ok {
			held = append(held, n)
		}
	}
	m.heldOnHold = held
	return held, nil
}

func (m *mockLifecyclePlatform) ReleaseTerminating(_ context.Context, held []platform.RouterNode) error {
	m.calls = append(m.calls, "release")
	m.releasedWith = held
	return nil
}

func (m *mockLifecyclePlatform) ReconcileNodes(ctx context.Context, nodes []platform.RouterNode) error {
	m.calls = append(m.calls, "reconcile")
	return m.mockPlatform.ReconcileNodes(ctx, nodes)
}

func runLifecycleReconcile(t *testing.T, mock platform.CloudPlatform, nodes ...*corev1.Node) *networkingv1alpha1.CUDNBgpConfig {
	t.Helper()

	config := newTestCUDNBgpConfigWithAWS()
	s := configTestScheme()

	network := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "operator.openshift.io/v1",
			"kind":       "Network",
			"metadata":   map[string]interface{}{"name": "cluster"},
			"spec":       map[string]interface{}{},
		},
	}
	frrNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: FRRNamespace}}
	frrPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "frr-k8s-pod", Namespace: FRRNamespace, Labels: map[string]string{"app": "frr-k8s"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}

	objs := []client.Object{config, network, frrNS, frrPod}
	for _, n := range nodes {
		objs = append(objs, n)
	}

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{
		Client: c, Scheme: s,
		PlatformBuilder: func(_ context.Context, _ client.Client, _ *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error) {
			return mock, nil
		},
	}

	// A single pass adds the finalizer and runs every phase, so the recorded
	// call sequence is exactly one reconciliation's worth.
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}}); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated); err != nil {
		t.Fatalf("failed to get config: %v", err)
	}
	return updated
}

// TestLifecycle_TerminatingNodeExcludedThenReleased is the reason the
// interface exists: a node whose Machine is going away must be dropped from
// cloud reconciliation so its peers are torn down, and only released once
// that has happened.
func TestLifecycle_TerminatingNodeExcludedThenReleased(t *testing.T) {
	mock := &mockLifecyclePlatform{terminating: []string{"node-2"}}

	runLifecycleReconcile(t, mock,
		newRouterNode("node-1", "10.0.1.10", "us-east-1a", "aws:///us-east-1a/i-001"),
		newRouterNode("node-2", "10.0.2.10", "us-east-1b", "aws:///us-east-1b/i-002"),
	)

	want := []string{"hold", "reconcile", "release"}
	if len(mock.calls) != len(want) {
		t.Fatalf("expected calls %v, got %v", want, mock.calls)
	}
	for i, w := range want {
		if mock.calls[i] != w {
			t.Fatalf("expected calls %v, got %v", want, mock.calls)
		}
	}

	if len(mock.reconcileNodesArgs) != 1 || mock.reconcileNodesArgs[0].Name != "node-1" {
		t.Errorf("expected only node-1 to reach ReconcileNodes, got %+v", mock.reconcileNodesArgs)
	}
	if len(mock.releasedWith) != 1 || mock.releasedWith[0].Name != "node-2" {
		t.Errorf("expected node-2 to be released, got %+v", mock.releasedWith)
	}
}

// TestLifecycle_NothingTerminatingSkipsRelease pins that a quiet cluster does
// no release work at all.
func TestLifecycle_NothingTerminatingSkipsRelease(t *testing.T) {
	mock := &mockLifecyclePlatform{}

	runLifecycleReconcile(t, mock,
		newRouterNode("node-1", "10.0.1.10", "us-east-1a", "aws:///us-east-1a/i-001"),
	)

	for _, call := range mock.calls {
		if call == "release" {
			t.Error("release must not be called when nothing is terminating")
		}
	}
	if len(mock.reconcileNodesArgs) != 1 {
		t.Errorf("expected 1 node to reach ReconcileNodes, got %d", len(mock.reconcileNodesArgs))
	}
}

// TestLifecycle_NotImplementedIsSkipped proves a platform without the optional
// interface reconciles exactly as before.
func TestLifecycle_NotImplementedIsSkipped(t *testing.T) {
	mock := &mockPlatform{}

	updated := runLifecycleReconcile(t, mock,
		newRouterNode("node-1", "10.0.1.10", "us-east-1a", "aws:///us-east-1a/i-001"),
	)

	if !mock.reconcileNodesCalled {
		t.Fatal("expected ReconcileNodes to be called")
	}
	if len(mock.reconcileNodesArgs) != 1 {
		t.Errorf("expected 1 node, got %d", len(mock.reconcileNodesArgs))
	}
	if updated.Status.Phase != networkingv1alpha1.PhaseReady {
		t.Errorf("expected Ready, got %s", updated.Status.Phase)
	}
}

// TestLifecycle_HoldFailureDegrades pins that a failure to hold is surfaced
// rather than silently reconciling a node that is going away.
func TestLifecycle_HoldFailureDegrades(t *testing.T) {
	mock := &mockLifecyclePlatform{holdErr: errStub}

	updated := runLifecycleReconcile(t, mock,
		newRouterNode("node-1", "10.0.1.10", "us-east-1a", "aws:///us-east-1a/i-001"),
	)

	if updated.Status.Phase != networkingv1alpha1.PhaseDegraded {
		t.Errorf("expected Degraded, got %s", updated.Status.Phase)
	}
	if mock.reconcileNodesCalled {
		t.Error("ReconcileNodes must not run when the hold failed")
	}
	for _, cond := range updated.Status.Conditions {
		if cond.Type == networkingv1alpha1.ConditionCloudResourcesReconciled {
			if cond.Status != metav1.ConditionFalse {
				t.Errorf("expected %s to be False", cond.Type)
			}
			return
		}
	}
	t.Errorf("expected a %s condition", networkingv1alpha1.ConditionCloudResourcesReconciled)
}
