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
	"fmt"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	networkingv1alpha1 "github.com/openshift/bgp-cloud-connector/api/v1alpha1"
	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

func configTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = networkingv1alpha1.AddToScheme(s)

	s.AddKnownTypeWithName(NetworkGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(NetworkGVK.GroupVersion().WithKind("NetworkList"), &unstructured.UnstructuredList{})

	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfiguration"), &unstructured.Unstructured{})
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfigurationList"), &unstructured.UnstructuredList{})
	return s
}

func newTestCUDNBgpConfig() *networkingv1alpha1.CUDNBgpConfig {
	return &networkingv1alpha1.CUDNBgpConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster",
		},
		Spec: networkingv1alpha1.CUDNBgpConfigSpec{
			Platform: networkingv1alpha1.PlatformManual,
			BGP: networkingv1alpha1.BGPConfig{
				LocalASN:          65001,
				LivenessDetection: networkingv1alpha1.LivenessDetectionBGPKeepalive,
				PeerGroups: []networkingv1alpha1.PeerGroup{
					{
						NodeSelector: map[string]string{"topology.kubernetes.io/zone": "us-east-1a"},
						Neighbors: []networkingv1alpha1.BGPNeighbor{
							{Address: "10.0.1.47", RemoteASN: 64512},
							{Address: "10.0.1.183", RemoteASN: 64512},
						},
					},
				},
			},
			RouterNodeSelector: map[string]string{"networking.openshift.io/cudn-bgp-router": ""},
		},
	}
}

func TestConfigReconcile_FullReconcile(t *testing.T) {
	config := newTestCUDNBgpConfig()
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
			Name:      "frr-k8s-pod",
			Namespace: FRRNamespace,
			Labels:    map[string]string{"app": "frr-k8s"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config, network, frrNS, frrPod).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{Client: c, Scheme: s}

	// First reconcile adds finalizer
	_, _ = r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "cluster"},
	})

	// Second reconcile does full 3-phase
	result, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "cluster"},
	})
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}
	if result.RequeueAfter != 5*time.Minute {
		t.Errorf("expected 5m resync requeue, got %v", result.RequeueAfter)
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated); err != nil {
		t.Fatalf("failed to get config: %v", err)
	}
	if !meta.IsStatusConditionTrue(updated.Status.Conditions, networkingv1alpha1.ConditionReady) {
		t.Errorf("expected Ready, got %v", findCondition(updated.Status.Conditions, networkingv1alpha1.ConditionReady))
	}

	// Three from the phases this fixture runs, plus Suspended, which is
	// reported on every pass because absence of a condition reads as Unknown
	// rather than as False, plus Ready, which summarises them.
	if len(updated.Status.Conditions) != 5 {
		t.Errorf("expected 5 conditions, got %d", len(updated.Status.Conditions))
	}
	if ready := findCondition(updated.Status.Conditions, networkingv1alpha1.ConditionReady); ready == nil {
		t.Errorf("missing condition %s", networkingv1alpha1.ConditionReady)
	} else if ready.Status != metav1.ConditionTrue {
		t.Errorf("condition %s = %s, want True", networkingv1alpha1.ConditionReady, ready.Status)
	}

	// Verify FRRConfiguration was created
	frrConfig := &unstructured.Unstructured{}
	frrConfig.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cudn-bgp-1", Namespace: FRRNamespace}, frrConfig); err != nil {
		t.Fatalf("FRRConfiguration not created: %v", err)
	}
}

func TestConfigReconcile_DeleteBlockedByRouting(t *testing.T) {
	now := metav1.Now()
	config := newTestCUDNBgpConfig()
	config.Finalizers = []string{ConfigFinalizerName}
	config.DeletionTimestamp = &now

	routing := &networkingv1alpha1.CUDNBgpRouting{
		ObjectMeta: metav1.ObjectMeta{Name: "prod"},
		Spec: networkingv1alpha1.CUDNBgpRoutingSpec{
			Network: networkingv1alpha1.NetworkConfig{
				Name: "prod", Subnets: []string{"10.100.0.0/16"},
			},
		},
	}

	s := configTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config, routing).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{Client: c, Scheme: s}
	result, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "cluster"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue when routing CRs still exist")
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated)
	found := false
	for _, f := range updated.Finalizers {
		if f == ConfigFinalizerName {
			found = true
		}
	}
	if !found {
		t.Error("finalizer should not be removed while routing CRs exist")
	}
}

// --- Phase 4: Controller AWS Integration (Mocked Platform) ---

type mockPlatform struct {
	discoverResult       *platform.DiscoveryResult
	discoverErr          error
	discoverCalled       bool
	reconcileNodesCalled bool
	reconcileNodesArgs   []platform.RouterNode
	reconcileNodesErr    error
	cleanupCalled        bool
	cleanupErr           error
	prerequisitesCalled  bool
	unmetPrerequisites   []string
	prerequisitesErr     error
	observePeersCalled   bool
	observedPeers        []platform.ObservedPeer
	observePeersErr      error
}

func (m *mockPlatform) DiscoverEndpoints(_ context.Context) (*platform.DiscoveryResult, error) {
	m.discoverCalled = true
	if m.discoverErr != nil {
		return nil, m.discoverErr
	}
	if m.discoverResult != nil {
		return m.discoverResult, nil
	}
	return &platform.DiscoveryResult{
		PeerGroups: []platform.PeerGroup{
			{
				Key:          "us-east-1a",
				NodeSelector: map[string]string{"topology.kubernetes.io/zone": "us-east-1a"},
				Neighbors:    []platform.DiscoveredNeighbor{{Address: "10.0.1.47", ASN: 64512}},
			},
		},
	}, nil
}

func (m *mockPlatform) ReconcileNodes(_ context.Context, nodes []platform.RouterNode) error {
	m.reconcileNodesCalled = true
	m.reconcileNodesArgs = nodes
	return m.reconcileNodesErr
}

func (m *mockPlatform) CheckPrerequisites(_ context.Context) ([]string, error) {
	m.prerequisitesCalled = true
	return m.unmetPrerequisites, m.prerequisitesErr
}

func (m *mockPlatform) ObservePeers(_ context.Context) ([]platform.ObservedPeer, error) {
	m.observePeersCalled = true
	return m.observedPeers, m.observePeersErr
}

func (m *mockPlatform) Cleanup(_ context.Context) error {
	m.cleanupCalled = true
	return m.cleanupErr
}

func newTestCUDNBgpConfigWithAWS() *networkingv1alpha1.CUDNBgpConfig {
	return &networkingv1alpha1.CUDNBgpConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster",
		},
		Spec: networkingv1alpha1.CUDNBgpConfigSpec{
			Platform: networkingv1alpha1.PlatformAWS,
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
}

func newRouterNode(name, ip, az, providerID string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"networking.openshift.io/cudn-bgp-router": "", "topology.kubernetes.io/zone": az},
		},
		Spec: corev1.NodeSpec{ProviderID: providerID},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}},
		},
	}
}

func TestConfigReconcile_AWSFullReconcile(t *testing.T) {
	mock := &mockPlatform{}
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
	node := newRouterNode("node-1", "10.0.1.10", "us-east-1a", "aws:///us-east-1a/i-001")

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config, network, frrNS, frrPod, node).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{
		Client: c, Scheme: s,
		PlatformBuilder: func(_ context.Context, _ client.Client, _ *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error) {
			return mock, nil
		},
	}

	// First reconcile adds finalizer
	_, _ = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}})
	// Second reconcile does full 5-phase
	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}})
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}
	if result.RequeueAfter != 5*time.Minute {
		t.Errorf("expected 5m resync, got %v", result.RequeueAfter)
	}
	if !mock.discoverCalled {
		t.Fatal("expected DiscoverEndpoints to be called")
	}
	if !mock.reconcileNodesCalled {
		t.Fatal("expected ReconcileNodes to be called")
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated)
	if !meta.IsStatusConditionTrue(updated.Status.Conditions, networkingv1alpha1.ConditionReady) {
		t.Errorf("expected Ready, got %v", findCondition(updated.Status.Conditions, networkingv1alpha1.ConditionReady))
	}
	// Assert the conditions by name rather than by count, so adding one is a
	// deliberate edit to this list instead of a number that needs bumping.
	wantConditions := []string{
		networkingv1alpha1.ConditionNetworkOperatorPatched,
		networkingv1alpha1.ConditionFRRNamespaceReady,
		networkingv1alpha1.ConditionPrerequisitesSatisfied,
		networkingv1alpha1.ConditionCloudEndpointsDiscovered,
		networkingv1alpha1.ConditionFRRConfigurationApplied,
		networkingv1alpha1.ConditionCloudResourcesReconciled,
	}
	gotConditions := map[string]metav1.ConditionStatus{}
	for _, cond := range updated.Status.Conditions {
		gotConditions[cond.Type] = cond.Status
	}
	for _, want := range wantConditions {
		if status, present := gotConditions[want]; !present {
			t.Errorf("missing condition %s", want)
		} else if status != metav1.ConditionTrue {
			t.Errorf("condition %s = %s, want True", want, status)
		}
	}
	// Suspended is reported alongside them, False while running. It is not in
	// wantConditions because those are asserted True; this one is the opposite
	// polarity and has to be said rather than left out, since an absent
	// condition reads as Unknown.
	if status, present := gotConditions[networkingv1alpha1.ConditionSuspended]; !present {
		t.Errorf("missing condition %s", networkingv1alpha1.ConditionSuspended)
	} else if status != metav1.ConditionFalse {
		t.Errorf("condition %s = %s, want False", networkingv1alpha1.ConditionSuspended, status)
	}
	// Ready summarises the rest, so it is reported alongside them and True
	// once every step above is satisfied.
	if status, present := gotConditions[networkingv1alpha1.ConditionReady]; !present {
		t.Errorf("missing condition %s", networkingv1alpha1.ConditionReady)
	} else if status != metav1.ConditionTrue {
		t.Errorf("condition %s = %s, want True", networkingv1alpha1.ConditionReady, status)
	}
	if len(gotConditions) != len(wantConditions)+2 {
		t.Errorf("expected conditions %v plus Suspended and Ready, got %v", wantConditions, gotConditions)
	}
	// The discovered peering plan is reported for every cloud, not just the
	// one whose vocabulary the status block used to be written in.
	if len(updated.Status.PeerGroups) != 1 {
		t.Fatalf("expected 1 peer group in status, got %d", len(updated.Status.PeerGroups))
	}
	pg := updated.Status.PeerGroups[0]
	if pg.Key != "us-east-1a" {
		t.Errorf("peer group key = %q, want the discovered group's key", pg.Key)
	}
	if len(pg.Neighbors) != 1 || pg.Neighbors[0].Address != "10.0.1.47" {
		t.Errorf("expected the discovered neighbour in status, got %+v", pg.Neighbors)
	}
	if pg.Neighbors[0].RemoteASN != 64512 {
		t.Errorf("expected the discovered remote ASN in status, got %d", pg.Neighbors[0].RemoteASN)
	}

	for _, cond := range updated.Status.Conditions {
		if cond.Type == networkingv1alpha1.ConditionCloudEndpointsDiscovered {
			if cond.Status != metav1.ConditionTrue {
				t.Errorf("expected AWSEndpointsDiscovered=True, got %s", cond.Status)
			}
		}
		if cond.Type == networkingv1alpha1.ConditionCloudResourcesReconciled {
			if cond.Status != metav1.ConditionTrue {
				t.Errorf("expected AWSResourcesReconciled=True, got %s", cond.Status)
			}
		}
	}

	// Verify FRR was created from discovery
	frrConfig := &unstructured.Unstructured{}
	frrConfig.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cudn-bgp-1", Namespace: FRRNamespace}, frrConfig); err != nil {
		t.Fatalf("FRRConfiguration not created from discovery: %v", err)
	}
}

func TestConfigReconcile_AWSCredentialFailure(t *testing.T) {
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
		ObjectMeta: metav1.ObjectMeta{Name: "frr-k8s-pod", Namespace: FRRNamespace, Labels: map[string]string{"app": "frr-k8s"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config, network, frrNS, frrPod).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{
		Client: c, Scheme: s,
		PlatformBuilder: func(_ context.Context, _ client.Client, _ *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error) {
			return nil, &platform.CredentialError{Msg: "invalid credentials"}
		},
	}

	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}})
	// A system fault is returned so the workqueue backs off, rather than
	// pinned to a fixed poll of whatever is failing.
	if err == nil {
		t.Error("expected the failure to surface as an error")
	}
	if result.RequeueAfter != 0 {
		t.Errorf("an error already requeues; RequeueAfter should be unset, got %v", result.RequeueAfter)
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated)
	if meta.IsStatusConditionTrue(updated.Status.Conditions, networkingv1alpha1.ConditionReady) {
		t.Errorf("expected not Ready, got %v", findCondition(updated.Status.Conditions, networkingv1alpha1.ConditionReady))
	}
	for _, cond := range updated.Status.Conditions {
		if cond.Type == networkingv1alpha1.ConditionCloudEndpointsDiscovered {
			if cond.Reason != "CloudCredentialsInvalid" {
				t.Errorf("expected reason CloudCredentialsInvalid, got %s", cond.Reason)
			}
			return
		}
	}
	t.Error("AWSEndpointsDiscovered condition not found")
}

func TestConfigReconcile_AWSDiscoveryFailure(t *testing.T) {
	mock := &mockPlatform{discoverErr: fmt.Errorf("DescribeRouteServers: InvalidRouteServerID")}
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
		ObjectMeta: metav1.ObjectMeta{Name: "frr-k8s-pod", Namespace: FRRNamespace, Labels: map[string]string{"app": "frr-k8s"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config, network, frrNS, frrPod).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{
		Client: c, Scheme: s,
		PlatformBuilder: func(_ context.Context, _ client.Client, _ *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error) {
			return mock, nil
		},
	}

	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}})
	// A system fault is returned so the workqueue backs off, rather than
	// pinned to a fixed poll of whatever is failing.
	if err == nil {
		t.Error("expected the failure to surface as an error")
	}
	if result.RequeueAfter != 0 {
		t.Errorf("an error already requeues; RequeueAfter should be unset, got %v", result.RequeueAfter)
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated)
	if meta.IsStatusConditionTrue(updated.Status.Conditions, networkingv1alpha1.ConditionReady) {
		t.Errorf("expected not Ready, got %v", findCondition(updated.Status.Conditions, networkingv1alpha1.ConditionReady))
	}
	for _, cond := range updated.Status.Conditions {
		if cond.Type == networkingv1alpha1.ConditionCloudEndpointsDiscovered {
			if cond.Reason != "CloudDiscoveryFailed" {
				t.Errorf("expected reason CloudDiscoveryFailed, got %s", cond.Reason)
			}
			return
		}
	}
	t.Error("AWSEndpointsDiscovered condition not found")
}

func TestConfigReconcile_AWSReconcileFailure(t *testing.T) {
	mock := &mockPlatform{reconcileNodesErr: fmt.Errorf("ec2 API timeout")}
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
		ObjectMeta: metav1.ObjectMeta{Name: "frr-k8s-pod", Namespace: FRRNamespace, Labels: map[string]string{"app": "frr-k8s"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	node := newRouterNode("node-1", "10.0.1.10", "us-east-1a", "aws:///us-east-1a/i-001")

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config, network, frrNS, frrPod, node).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{
		Client: c, Scheme: s,
		PlatformBuilder: func(_ context.Context, _ client.Client, _ *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error) {
			return mock, nil
		},
	}

	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}})
	// A system fault is returned so the workqueue backs off, rather than
	// pinned to a fixed poll of whatever is failing.
	if err == nil {
		t.Error("expected the failure to surface as an error")
	}
	if result.RequeueAfter != 0 {
		t.Errorf("an error already requeues; RequeueAfter should be unset, got %v", result.RequeueAfter)
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated)
	if meta.IsStatusConditionTrue(updated.Status.Conditions, networkingv1alpha1.ConditionReady) {
		t.Errorf("expected not Ready, got %v", findCondition(updated.Status.Conditions, networkingv1alpha1.ConditionReady))
	}
	for _, cond := range updated.Status.Conditions {
		if cond.Type == networkingv1alpha1.ConditionCloudResourcesReconciled {
			if cond.Reason != "CloudReconcileFailed" {
				t.Errorf("expected reason CloudReconcileFailed, got %s", cond.Reason)
			}
			return
		}
	}
	t.Error("AWSResourcesReconciled condition not found")
}

func TestConfigReconcile_AWSNodeFiltering(t *testing.T) {
	mock := &mockPlatform{}
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
		ObjectMeta: metav1.ObjectMeta{Name: "frr-k8s-pod", Namespace: FRRNamespace, Labels: map[string]string{"app": "frr-k8s"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}

	completeNodes := []*corev1.Node{
		newRouterNode("node-1", "10.0.1.10", "us-east-1a", "aws:///us-east-1a/i-001"),
		newRouterNode("node-2", "10.0.2.10", "us-east-1b", "aws:///us-east-1b/i-002"),
		newRouterNode("node-3", "10.0.3.10", "us-east-1c", "aws:///us-east-1c/i-003"),
	}
	missingIP := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-no-ip",
			Labels: map[string]string{"networking.openshift.io/cudn-bgp-router": "", "topology.kubernetes.io/zone": "us-east-1a"},
		},
		Spec: corev1.NodeSpec{ProviderID: "aws:///us-east-1a/i-004"},
	}
	missingAZ := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-no-az",
			Labels: map[string]string{"networking.openshift.io/cudn-bgp-router": ""},
		},
		Spec: corev1.NodeSpec{ProviderID: "aws:///us-east-1a/i-005"},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.4.10"}},
		},
	}

	objs := []client.Object{config, network, frrNS, frrPod, missingIP, missingAZ}
	for _, n := range completeNodes {
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

	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}})
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}
	if !mock.reconcileNodesCalled {
		t.Fatal("expected ReconcileNodes to be called")
	}
	if len(mock.reconcileNodesArgs) != 3 {
		t.Errorf("expected 3 nodes passed to ReconcileNodes, got %d", len(mock.reconcileNodesArgs))
	}
}

func TestConfigReconcile_DeleteSucceedsWithCredentialFailure(t *testing.T) {
	now := metav1.Now()
	config := newTestCUDNBgpConfigWithAWS()
	config.Finalizers = []string{ConfigFinalizerName}
	config.DeletionTimestamp = &now

	s := configTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{
		Client: c, Scheme: s,
		PlatformBuilder: func(_ context.Context, _ client.Client, _ *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error) {
			return nil, &platform.CredentialError{Msg: "invalid credentials"}
		},
	}

	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}})
	if err != nil {
		t.Fatalf("deletion should succeed even with credential failure, got: %v", err)
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated)
	for _, f := range updated.Finalizers {
		if f == ConfigFinalizerName {
			t.Error("finalizer should be removed even when AWS credentials are invalid")
		}
	}
}

func TestConfigReconcile_DeleteSuccessful(t *testing.T) {
	mock := &mockPlatform{}
	now := metav1.Now()
	config := newTestCUDNBgpConfigWithAWS()
	config.Finalizers = []string{ConfigFinalizerName}
	config.DeletionTimestamp = &now

	frrObj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "frrk8s.metallb.io/v1beta1",
			"kind":       "FRRConfiguration",
			"metadata": map[string]interface{}{
				"name":      "cudn-bgp-1",
				"namespace": FRRNamespace,
				"labels":    map[string]interface{}{LabelManagedBy: LabelManagedByVal},
			},
		},
	}

	s := configTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config, frrObj).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{
		Client: c, Scheme: s,
		PlatformBuilder: func(_ context.Context, _ client.Client, _ *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error) {
			return mock, nil
		},
	}

	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !mock.discoverCalled {
		t.Error("expected DiscoverEndpoints to be called before cleanup")
	}
	if !mock.cleanupCalled {
		t.Error("expected Cleanup to be called during deletion")
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cudn-bgp-1", Namespace: FRRNamespace}, obj); err == nil {
		t.Error("FRRConfiguration should be deleted during cleanup")
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated)
	for _, f := range updated.Finalizers {
		if f == ConfigFinalizerName {
			t.Error("finalizer should be removed after successful deletion")
		}
	}
}

// TestConfigReconcile_ManualReportsPeerGroups proves a Manual configuration
// reports the plan it rendered, as every cloud does.
//
// Manual gets its plan from the spec rather than from discovery, but it still
// renders it into FRRConfigurations, and status.peerGroups exists to say what
// FRR was told to peer with. Reporting nothing leaves the one platform that
// needs no credentials as the only one whose work is invisible.
func TestConfigReconcile_ManualReportsPeerGroups(t *testing.T) {
	config := newTestCUDNBgpConfig() // Platform: Manual
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
			Name:      "frr-k8s-pod",
			Namespace: FRRNamespace,
			Labels:    map[string]string{"app": "frr-k8s"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config, network, frrNS, frrPod).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{Client: c, Scheme: s}
	ctx := context.Background()
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}}

	_, _ = r.Reconcile(ctx, req)
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	if err := c.Get(ctx, types.NamespacedName{Name: "cluster"}, updated); err != nil {
		t.Fatalf("failed to get config: %v", err)
	}

	if len(updated.Status.PeerGroups) != len(config.Spec.BGP.PeerGroups) {
		t.Fatalf("expected %d peer group(s) in status, got %d",
			len(config.Spec.BGP.PeerGroups), len(updated.Status.PeerGroups))
	}

	got := updated.Status.PeerGroups[0]
	// The key is the group's position, which is what names the generated
	// FRRConfiguration: group "1" produced cudn-bgp-1.
	if got.Key != "1" {
		t.Errorf("expected the first group to be keyed %q, got %q", "1", got.Key)
	}
	want := config.Spec.BGP.PeerGroups[0]
	if !reflect.DeepEqual(got.NodeSelector, want.NodeSelector) {
		t.Errorf("node selector: got %v, want %v", got.NodeSelector, want.NodeSelector)
	}
	if !reflect.DeepEqual(got.Neighbors, want.Neighbors) {
		t.Errorf("neighbours: got %v, want %v", got.Neighbors, want.Neighbors)
	}
}
