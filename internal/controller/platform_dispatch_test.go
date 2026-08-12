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
	"strings"
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

// --- Platform dispatch ---

// TestDefaultPlatformBuilder_EveryEnumValueDispatches walks the platform enum
// and proves each implemented value reaches its builder. Without it a value
// can be added to the API and to the CRD while the switch never grows a case:
// the orphaned builder is still a legal Go function, so nothing fails to
// compile and every other test passes.
//
// Construction is expected to fail here, because these builders read
// Infrastructure/cluster first and the fake client has none. What must not
// happen is falling through to the default arm.
func TestDefaultPlatformBuilder_EveryEnumValueDispatches(t *testing.T) {
	implemented := []networkingv1alpha1.PlatformType{
		networkingv1alpha1.PlatformAWS,
		networkingv1alpha1.PlatformGCP,
	}

	for _, p := range implemented {
		t.Run(string(p), func(t *testing.T) {
			config := newTestCUDNBgpConfigWithAWS()
			config.Spec.Platform = p
			config.Spec.GCP = &networkingv1alpha1.GCPConfig{
				Project:         "proj",
				Region:          "europe-west1",
				CloudRouterName: "router",
				NCC:             networkingv1alpha1.NCCConfig{HubName: "hub", SpokePrefix: "spoke"},
			}
			c := fake.NewClientBuilder().WithScheme(configTestScheme()).Build()

			_, err := defaultPlatformBuilder(context.Background(), c, config)
			if err != nil && strings.Contains(err.Error(), "no platform implementation") {
				t.Fatalf("%s is in the enum but the builder has no case for it: %v", p, err)
			}
		})
	}
}

func TestDefaultPlatformBuilder_UnknownPlatform(t *testing.T) {
	config := newTestCUDNBgpConfigWithAWS()
	config.Spec.Platform = networkingv1alpha1.PlatformType("Azure")

	c := fake.NewClientBuilder().WithScheme(configTestScheme()).Build()

	_, err := defaultPlatformBuilder(context.Background(), c, config)
	if err == nil {
		t.Fatal("expected an error for a platform with no implementation")
	}
}

// TestConfigReconcile_ManualSkipsPlatform proves that a Manual configuration
// never constructs a cloud platform, so no cloud credentials are required.
func TestConfigReconcile_ManualSkipsPlatform(t *testing.T) {
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
		ObjectMeta: metav1.ObjectMeta{Name: "frr-k8s-pod", Namespace: FRRNamespace, Labels: map[string]string{"app": "frr-k8s"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config, network, frrNS, frrPod).
		WithStatusSubresource(config).
		Build()

	builderCalled := false
	r := &CUDNBgpConfigReconciler{
		Client: c, Scheme: s,
		PlatformBuilder: func(_ context.Context, _ client.Client, _ *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error) {
			builderCalled = true
			return &mockPlatform{}, nil
		},
	}

	_, _ = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}})
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}}); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	if builderCalled {
		t.Error("Manual platform must not construct a cloud platform")
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated)
	for _, cond := range updated.Status.Conditions {
		if cond.Type == networkingv1alpha1.ConditionCloudEndpointsDiscovered ||
			cond.Type == networkingv1alpha1.ConditionCloudResourcesReconciled {
			t.Errorf("Manual platform must not report %s", cond.Type)
		}
	}
}
