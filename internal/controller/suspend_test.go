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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	networkingv1alpha1 "github.com/openshift/bgp-cloud-connector/api/v1alpha1"
	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

// suspendFixture is a config that has already reconciled once, so there is
// something for suspension to tear down.
func suspendFixture(t *testing.T, mock *mockPlatform) (*CUDNBgpConfigReconciler, *networkingv1alpha1.CUDNBgpConfig) {
	t.Helper()

	config := newTestCUDNBgpConfigWithAWS()
	config.Finalizers = []string{ConfigFinalizerName}
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
		ObjectMeta: metav1.ObjectMeta{
			Name: "frr-k8s-pod", Namespace: FRRNamespace,
			Labels: map[string]string{"app": "frr-k8s"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config, network, frrNS, frrPod).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{
		Client: c, Scheme: s,
		PlatformBuilder: mockPlatformBuilder(mock),
	}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: SingletonName},
	}); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	return r, config
}

func reloadConfig(t *testing.T, r *CUDNBgpConfigReconciler) *networkingv1alpha1.CUDNBgpConfig {
	t.Helper()
	out := &networkingv1alpha1.CUDNBgpConfig{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: SingletonName}, out); err != nil {
		t.Fatalf("reloading config: %v", err)
	}
	return out
}

func frrConfigurationCount(t *testing.T, r *CUDNBgpConfigReconciler) int {
	t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(FRRConfigurationGVK)
	if err := r.List(context.Background(), list); err != nil {
		t.Fatalf("listing FRRConfigurations: %v", err)
	}
	return len(list.Items)
}

// TestSuspend_TearsDownAndStops is the point of the field: suspension gives
// back the cloud state and stops reconciling, without deleting the
// configuration. Deleting the CR would also tear down, but you would have to
// write it again to resume.
func TestSuspend_TearsDownAndStops(t *testing.T) {
	mock := &mockPlatform{}
	r, _ := suspendFixture(t, mock)

	if frrConfigurationCount(t, r) == 0 {
		t.Fatal("fixture should have produced an FRRConfiguration to tear down")
	}

	config := reloadConfig(t, r)
	config.Spec.Suspended = true
	if err := r.Update(context.Background(), config); err != nil {
		t.Fatalf("suspending: %v", err)
	}

	mock.cleanupCalled = false
	mock.reconcileNodesCalled = false
	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: SingletonName},
	}); err != nil {
		t.Fatalf("reconcile while suspended: %v", err)
	}

	if !mock.cleanupCalled {
		t.Error("suspending should release the cloud state it created")
	}
	if mock.reconcileNodesCalled {
		t.Error("suspending should stop reconciling, not reconcile once more")
	}
	if n := frrConfigurationCount(t, r); n != 0 {
		t.Errorf("expected the generated FRRConfigurations to be removed, %d remain", n)
	}

	updated := reloadConfig(t, r)
	if meta.IsStatusConditionTrue(updated.Status.Conditions, networkingv1alpha1.ConditionReady) {
		t.Errorf("expected not Ready, got %v", findCondition(updated.Status.Conditions, networkingv1alpha1.ConditionReady))
	}
	cond := findCondition(updated.Status.Conditions, networkingv1alpha1.ConditionSuspended)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("expected Suspended=True, got %+v", cond)
	}
}

// TestSuspend_KeepsTheConfiguration distinguishes suspension from deletion:
// the finalizer stays, so resuming needs no reapply.
func TestSuspend_KeepsTheConfiguration(t *testing.T) {
	r, _ := suspendFixture(t, &mockPlatform{})

	config := reloadConfig(t, r)
	config.Spec.Suspended = true
	if err := r.Update(context.Background(), config); err != nil {
		t.Fatalf("suspending: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: SingletonName},
	}); err != nil {
		t.Fatalf("reconcile while suspended: %v", err)
	}

	updated := reloadConfig(t, r)
	if len(updated.Finalizers) == 0 {
		t.Error("suspension must not release the finalizer; it is not a deletion")
	}
	if updated.Spec.BGP.LocalASN == 0 {
		t.Error("suspension must keep the configuration")
	}
}

// TestSuspend_IsIdempotent pins that a suspended config does not tear down
// again on every resync, which would mean repeated cloud calls forever.
func TestSuspend_IsIdempotent(t *testing.T) {
	mock := &mockPlatform{}
	r, _ := suspendFixture(t, mock)

	config := reloadConfig(t, r)
	config.Spec.Suspended = true
	if err := r.Update(context.Background(), config); err != nil {
		t.Fatalf("suspending: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(context.Background(), reconcile.Request{
			NamespacedName: types.NamespacedName{Name: SingletonName},
		}); err != nil {
			t.Fatalf("reconcile %d while suspended: %v", i, err)
		}
	}

	updated := reloadConfig(t, r)
	if meta.IsStatusConditionTrue(updated.Status.Conditions, networkingv1alpha1.ConditionReady) {
		t.Errorf("expected not Ready, got %v", findCondition(updated.Status.Conditions, networkingv1alpha1.ConditionReady))
	}
}

// TestSuspend_ResumeReconciles pins that clearing the field brings everything
// back, which is the half that makes suspension useful rather than a
// one-way door.
func TestSuspend_ResumeReconciles(t *testing.T) {
	mock := &mockPlatform{}
	r, _ := suspendFixture(t, mock)

	config := reloadConfig(t, r)
	config.Spec.Suspended = true
	if err := r.Update(context.Background(), config); err != nil {
		t.Fatalf("suspending: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: SingletonName},
	}); err != nil {
		t.Fatalf("reconcile while suspended: %v", err)
	}

	config = reloadConfig(t, r)
	config.Spec.Suspended = false
	if err := r.Update(context.Background(), config); err != nil {
		t.Fatalf("resuming: %v", err)
	}
	mock.reconcileNodesCalled = false
	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: SingletonName},
	}); err != nil {
		t.Fatalf("reconcile after resume: %v", err)
	}

	if !mock.reconcileNodesCalled {
		t.Error("resuming should reconcile the cloud again")
	}
	if n := frrConfigurationCount(t, r); n == 0 {
		t.Error("resuming should recreate the FRRConfigurations")
	}
	updated := reloadConfig(t, r)
	if ready := findCondition(updated.Status.Conditions, networkingv1alpha1.ConditionReady); ready == nil {
		t.Error("missing Ready condition after resuming")
	} else if ready.Reason == ReasonSuspended {
		t.Error("Ready should stop reporting Suspended once the field is cleared")
	}
}

// TestRequireReadyNodes_SkipsNotReady pins the opt-in filter. Without it a
// node that has been NotReady for hours keeps its cloud peering, so the cloud
// keeps sending it traffic it cannot forward.
func TestRequireReadyNodes_SkipsNotReady(t *testing.T) {
	mock := &mockPlatform{}
	config := newTestCUDNBgpConfigWithAWS()
	config.Spec.RequireReadyNodes = true

	r := routerNodeReconciler(t, config, mock,
		routerNodeObj("worker-a", "10.0.0.1", corev1.ConditionTrue),
		routerNodeObj("worker-b", "10.0.0.2", corev1.ConditionFalse),
	)
	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: SingletonName},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(mock.reconcileNodesArgs) != 1 {
		t.Fatalf("expected only the Ready node, got %d: %+v", len(mock.reconcileNodesArgs), mock.reconcileNodesArgs)
	}
	if mock.reconcileNodesArgs[0].Name != "worker-a" {
		t.Errorf("kept the wrong node: %s", mock.reconcileNodesArgs[0].Name)
	}
}

// TestRequireReadyNodes_OffByDefault pins that leaving it unset preserves what
// the operator did before the field existed. Turning it on churns peerings
// whenever a node reboots, so it is the operator's choice and not ours.
func TestRequireReadyNodes_OffByDefault(t *testing.T) {
	mock := &mockPlatform{}
	config := newTestCUDNBgpConfigWithAWS()

	r := routerNodeReconciler(t, config, mock,
		routerNodeObj("worker-a", "10.0.0.1", corev1.ConditionTrue),
		routerNodeObj("worker-b", "10.0.0.2", corev1.ConditionFalse),
	)
	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: SingletonName},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(mock.reconcileNodesArgs) != 2 {
		t.Errorf("expected both nodes when the filter is off, got %d", len(mock.reconcileNodesArgs))
	}
}

// TestRequireReadyNodes_NodeWithNoReadyCondition treats absence as not ready.
// A Node that has never reported readiness is not one to send traffic to.
func TestRequireReadyNodes_NodeWithNoReadyCondition(t *testing.T) {
	mock := &mockPlatform{}
	config := newTestCUDNBgpConfigWithAWS()
	config.Spec.RequireReadyNodes = true

	bare := routerNodeObj("worker-c", "10.0.0.3", corev1.ConditionTrue)
	bare.Status.Conditions = nil

	r := routerNodeReconciler(t, config, mock, bare)
	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: SingletonName},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(mock.reconcileNodesArgs) != 0 {
		t.Errorf("a node with no Ready condition should be skipped, got %+v", mock.reconcileNodesArgs)
	}
}

func routerNodeObj(name, ip string, ready corev1.ConditionStatus) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"networking.openshift.io/cudn-bgp-router": "",
				"topology.kubernetes.io/zone":             "us-east-1a",
			},
		},
		Spec: corev1.NodeSpec{ProviderID: "aws:///us-east-1a/i-" + name},
		Status: corev1.NodeStatus{
			Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: ready}},
		},
	}
}

func routerNodeReconciler(t *testing.T, config *networkingv1alpha1.CUDNBgpConfig, mock *mockPlatform, nodes ...*corev1.Node) *CUDNBgpConfigReconciler {
	t.Helper()
	config.Finalizers = []string{ConfigFinalizerName}
	s := configTestScheme()

	objs := []client.Object{
		config,
		&unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "operator.openshift.io/v1", "kind": "Network",
			"metadata": map[string]interface{}{"name": "cluster"}, "spec": map[string]interface{}{},
		}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: FRRNamespace}},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "frr-k8s-pod", Namespace: FRRNamespace,
				Labels: map[string]string{"app": "frr-k8s"},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
	}
	for _, n := range nodes {
		objs = append(objs, n)
	}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithStatusSubresource(config).Build()
	return &CUDNBgpConfigReconciler{Client: c, Scheme: s, PlatformBuilder: mockPlatformBuilder(mock)}
}

func mockPlatformBuilder(mock *mockPlatform) func(context.Context, client.Client, *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error) {
	return func(context.Context, client.Client, *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error) {
		return mock, nil
	}
}

func findCondition(conds []metav1.Condition, condType string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}
