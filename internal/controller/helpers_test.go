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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	networkingv1alpha1 "github.com/openshift/bgp-cloud-connector/api/v1alpha1"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = networkingv1alpha1.AddToScheme(s)
	return s
}

func TestValidateNamespaceLabels_Found(t *testing.T) {
	existing := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "app1",
			Labels: map[string]string{
				LabelPrimaryUDN: "",
				LabelCUDN:       "prod",
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(existing).Build()

	if err := ValidateNamespaceLabels(context.Background(), c, "prod"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateNamespaceLabels_NotFound(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	err := ValidateNamespaceLabels(context.Background(), c, "prod")
	if err == nil {
		t.Fatal("expected error when no namespace has required labels")
	}
}

func TestEnsureFRRConfigurations_BFDProfile(t *testing.T) {
	s := testScheme()
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfiguration"), &unstructured.Unstructured{})
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfigurationList"), &unstructured.UnstructuredList{})

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: FRRNamespace}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(ns).Build()
	ctx := context.Background()

	config := &networkingv1alpha1.CUDNBgpConfig{
		Spec: networkingv1alpha1.CUDNBgpConfigSpec{
			BGP: networkingv1alpha1.BGPConfig{
				LocalASN:          65001,
				LivenessDetection: networkingv1alpha1.LivenessDetectionBFD,
				PeerGroups: []networkingv1alpha1.PeerGroup{
					{
						NodeSelector: map[string]string{"bgp_router_subnet": "1"},
						Neighbors: []networkingv1alpha1.BGPNeighbor{
							{Address: "10.0.1.47", RemoteASN: 64512},
						},
					},
				},
			},
			RouterNodeSelector: map[string]string{"bgp_router": "true"},
		},
	}

	if err := EnsureFRRConfigurations(ctx, c, config); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: "cudn-bgp-1", Namespace: FRRNamespace}, obj); err != nil {
		t.Fatalf("FRRConfiguration not created: %v", err)
	}

	bfdProfiles, _, _ := unstructured.NestedSlice(obj.Object, "spec", "bgp", "bfdProfiles")
	if len(bfdProfiles) == 0 {
		t.Fatal("expected bfdProfiles when livenessDetection is bfd")
	}

	profile := bfdProfiles[0].(map[string]interface{})
	if profile["name"] != "default" {
		t.Errorf("expected bfd profile name 'default', got %v", profile["name"])
	}
}

func TestEnsureFRRConfigurations_PrunesStale(t *testing.T) {
	s := testScheme()
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfiguration"), &unstructured.Unstructured{})
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfigurationList"), &unstructured.UnstructuredList{})

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: FRRNamespace}}

	stale := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "frrk8s.metallb.io/v1beta1",
			"kind":       "FRRConfiguration",
			"metadata": map[string]interface{}{
				"name":      "cudn-bgp-3",
				"namespace": FRRNamespace,
				"labels":    map[string]interface{}{LabelManagedBy: LabelManagedByVal},
			},
			"spec": map[string]interface{}{},
		},
	}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(ns, stale).Build()
	ctx := context.Background()

	config := &networkingv1alpha1.CUDNBgpConfig{
		Spec: networkingv1alpha1.CUDNBgpConfigSpec{
			BGP: networkingv1alpha1.BGPConfig{
				LocalASN:          65001,
				LivenessDetection: networkingv1alpha1.LivenessDetectionBGPKeepalive,
				PeerGroups: []networkingv1alpha1.PeerGroup{
					{
						NodeSelector: map[string]string{"bgp_router_subnet": "1"},
						Neighbors:    []networkingv1alpha1.BGPNeighbor{{Address: "10.0.1.47", RemoteASN: 64512}},
					},
				},
			},
			RouterNodeSelector: map[string]string{"bgp_router": "true"},
		},
	}

	if err := EnsureFRRConfigurations(ctx, c, config); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: "cudn-bgp-1", Namespace: FRRNamespace}, obj); err != nil {
		t.Error("cudn-bgp-1 should exist")
	}

	staleObj := &unstructured.Unstructured{}
	staleObj.SetGroupVersionKind(FRRConfigurationGVK)
	err := c.Get(ctx, types.NamespacedName{Name: "cudn-bgp-3", Namespace: FRRNamespace}, staleObj)
	if err == nil {
		t.Error("cudn-bgp-3 should have been pruned")
	}
}

func TestEnsureFRRConfigurations_KeepsUnmanagedResources(t *testing.T) {
	s := testScheme()
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfiguration"), &unstructured.Unstructured{})
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfigurationList"), &unstructured.UnstructuredList{})

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: FRRNamespace}}

	userOwned := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "frrk8s.metallb.io/v1beta1",
			"kind":       "FRRConfiguration",
			"metadata": map[string]interface{}{
				"name":      "user-custom-frr",
				"namespace": FRRNamespace,
				"labels":    map[string]interface{}{"owner": "user"},
			},
			"spec": map[string]interface{}{},
		},
	}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(ns, userOwned).Build()
	ctx := context.Background()

	config := &networkingv1alpha1.CUDNBgpConfig{
		Spec: networkingv1alpha1.CUDNBgpConfigSpec{
			BGP: networkingv1alpha1.BGPConfig{
				LocalASN:          65001,
				LivenessDetection: networkingv1alpha1.LivenessDetectionBGPKeepalive,
				PeerGroups: []networkingv1alpha1.PeerGroup{
					{
						NodeSelector: map[string]string{"bgp_router_subnet": "1"},
						Neighbors:    []networkingv1alpha1.BGPNeighbor{{Address: "10.0.1.47", RemoteASN: 64512}},
					},
				},
			},
			RouterNodeSelector: map[string]string{"bgp_router": "true"},
		},
	}

	if err := EnsureFRRConfigurations(ctx, c, config); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: "user-custom-frr", Namespace: FRRNamespace}, obj); err != nil {
		t.Error("user-owned FRRConfiguration should not be pruned")
	}
}
