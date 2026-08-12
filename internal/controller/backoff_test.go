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

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	networkingv1alpha1 "github.com/openshift/bgp-cloud-connector/api/v1alpha1"
	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

// A failing reconcile must return its error rather than a fixed requeue.
//
// Returning (Result{RequeueAfter: 30s}, nil) tells controller-runtime the pass
// succeeded, which discards its exponential backoff and pins a broken operator
// to a constant poll of whatever is failing. That matters against a cloud API:
// CreateRouteServerPeer and DeleteRouteServerPeer refill at 5/s for an entire
// AWS account, so a stuck operator is a permanent draw on a shared bucket.
func TestDegrade_ReturnsErrorSoTheQueueBacksOff(t *testing.T) {
	config := newTestCUDNBgpConfigWithAWS()
	c, rec := eventTestEnv(t, config)

	r := &CUDNBgpConfigReconciler{
		Client: c, Scheme: configTestScheme(), Recorder: rec,
		PlatformBuilder: func(_ context.Context, _ client.Client, _ *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error) {
			return nil, &platform.CredentialError{Msg: "invalid credentials"}
		},
	}

	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}})
	if err == nil {
		t.Fatal("expected the failure to be returned as an error so the workqueue backs off")
	}
	if result.RequeueAfter != 0 {
		t.Errorf("an error already requeues with backoff; RequeueAfter should be unset, got %v", result.RequeueAfter)
	}

	// The status must still have been written, so the failure is visible
	// rather than only logged.
	updated := &networkingv1alpha1.CUDNBgpConfig{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated); err != nil {
		t.Fatalf("get: %v", err)
	}
	if updated.Status.Phase != networkingv1alpha1.PhaseDegraded {
		t.Errorf("expected Degraded to be recorded before returning, got %s", updated.Status.Phase)
	}
}
