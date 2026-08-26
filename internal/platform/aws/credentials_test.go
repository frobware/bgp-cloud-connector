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

package aws

import (
	"context"
	"errors"
	"slices"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

func credentialsTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	s.AddKnownTypeWithName(CredentialsRequestGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(CredentialsRequestGVK.GroupVersion().WithKind("CredentialsRequestList"), &unstructured.UnstructuredList{})
	return s
}

// withAmbient replaces the ambient credential probe for the duration of a
// test. A nil provider means the cluster has no credentials of its own.
func withAmbient(t *testing.T, provider awssdk.CredentialsProvider) {
	t.Helper()
	previous := ambientCredentials
	ambientCredentials = func(_ context.Context, _ string) (awssdk.CredentialsProvider, error) {
		if provider == nil {
			return nil, errors.New("no ambient credentials")
		}
		return provider, nil
	}
	t.Cleanup(func() { ambientCredentials = previous })
}

func mintedSecret(namespace string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: CredentialsSecretName, Namespace: namespace},
		Data:       data,
	}
}

// On ROSA the pod identity webhook injects a web identity token, so the
// SDK's own chain answers and nothing should be asked of the cluster.
func TestResolveCredentials_AmbientChainWins(t *testing.T) {
	ambient := credentials.NewStaticCredentialsProvider("ambient", "secret", "")
	withAmbient(t, ambient)

	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme()).Build()

	got, err := ResolveCredentials(context.Background(), c, "openshift-cudn-bgp-routing", "us-east-2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	creds, err := got.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("retrieving credentials: %v", err)
	}
	if creds.AccessKeyID != "ambient" {
		t.Errorf("expected the ambient credentials, got AccessKeyID %q", creds.AccessKeyID)
	}

	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(CredentialsRequestGVK)
	err = c.Get(context.Background(), types.NamespacedName{
		Name: CredentialsRequestName, Namespace: CredentialsRequestNamespace,
	}, cr)
	if err == nil {
		t.Error("a CredentialsRequest was created even though the SDK already had credentials")
	}
}

// On IPI there is nothing in the pod, so the operator asks CCO and waits
// for the secret rather than reporting a fault.
func TestResolveCredentials_CreatesRequestAndWaits(t *testing.T) {
	withAmbient(t, nil)

	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme()).Build()

	_, err := ResolveCredentials(context.Background(), c, "openshift-cudn-bgp-routing", "us-east-2")
	if !errors.Is(err, platform.ErrCredentialsPending) {
		t.Fatalf("expected platform.ErrCredentialsPending, got %v", err)
	}

	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(CredentialsRequestGVK)
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: CredentialsRequestName, Namespace: CredentialsRequestNamespace,
	}, cr); err != nil {
		t.Fatalf("expected a CredentialsRequest to have been created: %v", err)
	}

	name, _, _ := unstructured.NestedString(cr.Object, "spec", "secretRef", "name")
	namespace, _, _ := unstructured.NestedString(cr.Object, "spec", "secretRef", "namespace")
	if name != CredentialsSecretName || namespace != "openshift-cudn-bgp-routing" {
		t.Errorf("secretRef points at %s/%s, want openshift-cudn-bgp-routing/%s", namespace, name, CredentialsSecretName)
	}

	entries, found, err := unstructured.NestedSlice(cr.Object, "spec", "providerSpec", "statementEntries")
	if err != nil || !found || len(entries) != 1 {
		t.Fatalf("expected one statement entry in the providerSpec, got %v (found=%v, err=%v)", entries, found, err)
	}
	entry, ok := entries[0].(map[string]interface{})
	if !ok {
		t.Fatalf("statement entry is %T, want a map", entries[0])
	}
	actions, _, err := unstructured.NestedStringSlice(entry, "action")
	if err != nil {
		t.Fatalf("reading the actions: %v", err)
	}
	// The peer calls are the ones without which the operator can do
	// nothing at all; if the list is ever trimmed, it fails here.
	for _, want := range []string{"ec2:CreateRouteServerPeer", "ec2:DeleteRouteServerPeer", "ec2:ModifyNetworkInterfaceAttribute"} {
		if !slices.Contains(actions, want) {
			t.Errorf("the policy does not grant %s: %v", want, actions)
		}
	}
}

// Once CCO has minted, the keys in the secret are what the SDK should use.
func TestResolveCredentials_UsesMintedSecret(t *testing.T) {
	withAmbient(t, nil)

	secret := mintedSecret("openshift-cudn-bgp-routing", map[string][]byte{
		"aws_access_key_id":     []byte("AKIAMINTED"),
		"aws_secret_access_key": []byte("mintedsecret"),
	})
	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme()).WithObjects(secret).Build()

	provider, err := ResolveCredentials(context.Background(), c, "openshift-cudn-bgp-routing", "us-east-2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	creds, err := provider.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("retrieving credentials: %v", err)
	}
	if creds.AccessKeyID != "AKIAMINTED" || creds.SecretAccessKey != "mintedsecret" {
		t.Errorf("got %q/%q, want AKIAMINTED/mintedsecret", creds.AccessKeyID, creds.SecretAccessKey)
	}
}

// A secret that exists but carries nothing usable is a fault, not
// something to wait out: CCO has answered, and the answer is no good.
func TestResolveCredentials_SecretWithoutKeys(t *testing.T) {
	withAmbient(t, nil)

	secret := mintedSecret("openshift-cudn-bgp-routing", map[string][]byte{
		"something_else": []byte("nonsense"),
	})
	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme()).WithObjects(secret).Build()

	_, err := ResolveCredentials(context.Background(), c, "openshift-cudn-bgp-routing", "us-east-2")
	if err == nil {
		t.Fatal("expected an error for a secret with no usable keys")
	}
	if errors.Is(err, platform.ErrCredentialsPending) {
		t.Error("a malformed secret should not read as still pending")
	}
}

// Re-running against an existing request must not fail: reconcile is
// called repeatedly and the request is ours to own, not to duplicate.
func TestResolveCredentials_ExistingRequestIsNotAnError(t *testing.T) {
	withAmbient(t, nil)

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(CredentialsRequestGVK)
	existing.SetName(CredentialsRequestName)
	existing.SetNamespace(CredentialsRequestNamespace)

	c := fake.NewClientBuilder().
		WithScheme(credentialsTestScheme()).
		WithObjects(existing).
		Build()

	_, err := ResolveCredentials(context.Background(), c, "openshift-cudn-bgp-routing", "us-east-2")
	if !errors.Is(err, platform.ErrCredentialsPending) {
		t.Fatalf("expected platform.ErrCredentialsPending, got %v", err)
	}

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(CredentialsRequestGVK.GroupVersion().WithKind("CredentialsRequestList"))
	if err := c.List(context.Background(), list, client.InNamespace(CredentialsRequestNamespace)); err != nil {
		t.Fatalf("listing credentials requests: %v", err)
	}
	if len(list.Items) != 1 {
		t.Errorf("expected exactly one CredentialsRequest, got %d", len(list.Items))
	}
}
