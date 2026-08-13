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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	networkingv1alpha1 "github.com/openshift/bgp-cloud-connector/api/v1alpha1"
)

// These drive the whole reconcile, where machine_lifecycle_test.go drives the
// hold and release directly. What matters here is the ordering the hooks
// exist for: a node whose Machine is terminating must be out of the set handed
// to the cloud, so its peers are withdrawn before the instance goes away, and
// the hook must come off afterwards or the Machine never finishes deleting.
//
// They used to assert a type assertion on an optional platform interface.
// There is no such interface now: this is Machine API handling and every
// platform gets it.

func lifecycleScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = networkingv1alpha1.AddToScheme(s)
	s.AddKnownTypeWithName(NetworkGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(NetworkGVK.GroupVersion().WithKind("NetworkList"), &unstructured.UnstructuredList{})
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfiguration"), &unstructured.Unstructured{})
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfigurationList"), &unstructured.UnstructuredList{})
	s.AddKnownTypeWithName(machineGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(machineListGVK, &unstructured.UnstructuredList{})
	return s
}

func lifecycleNode(name, ip string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"networking.openshift.io/cudn-bgp-router": "",
				"topology.kubernetes.io/zone":             "us-east-1a",
			},
		},
		Spec:   corev1.NodeSpec{ProviderID: "aws:///us-east-1a/i-" + name},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}}},
	}
}

// lifecycleReconciler wires a config, its nodes and their Machines together.
func lifecycleReconciler(t *testing.T, mock *mockPlatform, withMachines bool, extra ...client.Object) *CUDNBgpConfigReconciler {
	t.Helper()

	config := newTestCUDNBgpConfigWithAWS()
	config.Finalizers = []string{ConfigFinalizerName}
	s := lifecycleScheme()

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
		lifecycleNode("worker-a", "10.0.0.1"),
		lifecycleNode("worker-b", "10.0.0.2"),
	}
	if withMachines {
		objs = append(objs,
			newMachine("worker-a", "aws:///us-east-1a/i-worker-a", false, false),
			newMachine("worker-b", "aws:///us-east-1a/i-worker-b", false, false),
		)
	}
	objs = append(objs, extra...)

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithStatusSubresource(config).Build()
	return &CUDNBgpConfigReconciler{Client: c, Scheme: s, PlatformBuilder: mockPlatformBuilder(mock)}
}

func reconcileOnce(t *testing.T, r *CUDNBgpConfigReconciler) error {
	t.Helper()
	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: SingletonName},
	})
	return err
}

// TestLifecycle_HooksEveryRouterNode pins that the hook goes on regardless of
// cloud. AWS never had this behaviour before the handling moved out of the GCP
// platform; now it does.
func TestLifecycle_HooksEveryRouterNode(t *testing.T) {
	r := lifecycleReconciler(t, &mockPlatform{}, true)
	if err := reconcileOnce(t, r); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	for _, name := range []string{"worker-a", "worker-b"} {
		if !contains(hookNames(t, r.Client, name), LifecycleHookName) {
			t.Errorf("no preTerminate hook on %s", name)
		}
	}
}

// TestLifecycle_TerminatingNodeExcludedThenReleased is the ordering the whole
// mechanism exists for.
func TestLifecycle_TerminatingNodeExcludedThenReleased(t *testing.T) {
	mock := &mockPlatform{}
	dying := newMachine("worker-b", "aws:///us-east-1a/i-worker-b", true, true)
	r := lifecycleReconciler(t, mock, false,
		newMachine("worker-a", "aws:///us-east-1a/i-worker-a", true, false),
		dying,
	)

	if err := reconcileOnce(t, r); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(mock.reconcileNodesArgs) != 1 || mock.reconcileNodesArgs[0].Name != "worker-a" {
		t.Fatalf("the terminating node should be excluded so its peers are withdrawn, got %+v", mock.reconcileNodesArgs)
	}
	if contains(hookNames(t, r.Client, "worker-b"), LifecycleHookName) {
		t.Error("the hook must be released once the peers are gone, or the Machine never finishes deleting")
	}
}

// TestLifecycle_NothingTerminatingKeepsHooks pins that a quiet cluster keeps
// its hooks in place, ready for a deletion that has not happened yet.
func TestLifecycle_NothingTerminatingKeepsHooks(t *testing.T) {
	mock := &mockPlatform{}
	r := lifecycleReconciler(t, mock, true)

	if err := reconcileOnce(t, r); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(mock.reconcileNodesArgs) != 2 {
		t.Errorf("expected both nodes reconciled, got %d", len(mock.reconcileNodesArgs))
	}
	if !contains(hookNames(t, r.Client, "worker-a"), LifecycleHookName) {
		t.Error("hooks should stay on a node that is not going anywhere")
	}
}

// TestLifecycle_NoMachineAPIIsANoOp is what replaced "the platform does not
// implement the optional interface". Off OpenShift, or on a HyperShift guest
// cluster whose Machines live in the management cluster, there is nothing to
// gate deletion on and reconciliation must carry on regardless.
//
// The error is injected rather than produced by leaving Machine out of the
// scheme. The fake client returns an empty list and a nil error for a kind it
// does not know, so a test written that way passes whether or not the guard
// exists -- which is what the first version of this test did.
func TestLifecycle_NoMachineAPIIsANoOp(t *testing.T) {
	mock := &mockPlatform{}
	r := lifecycleReconciler(t, mock, true)

	noMachineAPI := &meta.NoKindMatchError{
		GroupKind: schema.GroupKind{Group: "machine.openshift.io", Kind: "Machine"},
	}
	base, ok := r.Client.(client.WithWatch)
	if !ok {
		t.Fatal("expected the fake client to support Watch, which the interceptor needs")
	}
	r.Client = interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if list.GetObjectKind().GroupVersionKind() == machineListGVK {
				return noMachineAPI
			}
			return c.List(ctx, list, opts...)
		},
	})

	if err := reconcileOnce(t, r); err != nil {
		t.Fatalf("reconcile should survive a cluster with no Machine API: %v", err)
	}
	if !mock.reconcileNodesCalled {
		t.Error("the cloud should still be reconciled with no Machine API present")
	}
}

// TestIsMachineAPIAbsent pins which errors mean the Machine API is not there,
// since the guard above turns on this classification and nothing else.
func TestIsMachineAPIAbsent(t *testing.T) {
	absent := []error{
		&meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "machine.openshift.io", Kind: "Machine"}},
		errors.New("no kind is registered for the type v1beta1.MachineList"),
		errors.New("the server could not find the requested resource"),
	}
	for _, err := range absent {
		if !isMachineAPIAbsent(err) {
			t.Errorf("should be treated as absent: %v", err)
		}
	}

	present := []error{
		nil,
		errors.New("connection refused"),
		errors.New("machines.machine.openshift.io is forbidden"),
	}
	for _, err := range present {
		if isMachineAPIAbsent(err) {
			t.Errorf("should not be treated as absent, or a real failure becomes a silent no-op: %v", err)
		}
	}
}
