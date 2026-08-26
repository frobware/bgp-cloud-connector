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
	"fmt"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

// CredentialsRequestGVK is the cloud credential operator's API. It is
// addressed as unstructured for the same reason as everything else
// downstream of this operator: the typed package drags in a dependency
// tree out of all proportion to one object.
var CredentialsRequestGVK = schema.GroupVersionKind{
	Group: "cloudcredential.openshift.io", Version: "v1", Kind: "CredentialsRequest",
}

const (
	// CredentialsRequestName is the request the operator makes for itself.
	CredentialsRequestName = "cudn-bgp-routing-aws"

	// CredentialsRequestNamespace is where the cloud credential operator
	// looks for requests. It is not where the secret lands.
	CredentialsRequestNamespace = "openshift-cloud-credential-operator"

	// CredentialsSecretName is the secret CCO mints, in the operator's own
	// namespace.
	CredentialsSecretName = "cudn-bgp-routing-aws-credentials"

	// ServiceAccountName is the operator's ServiceAccount, which CCO needs
	// on STS clusters to scope the role it hands back.
	ServiceAccountName = "openshift-cudn-bgp-routing-controller-manager"

	accessKeyIDKey     = "aws_access_key_id"
	secretAccessKeyKey = "aws_secret_access_key"
)

// policyActions is what the operator needs to do its job, and no more:
// discovery of the route server estate, the peers it manages, the tags
// that mark them as its own, and source/destination checking on the
// router nodes' interfaces.
var policyActions = []string{
	"ec2:DescribeRouteServers",
	"ec2:DescribeRouteServerEndpoints",
	"ec2:DescribeRouteServerPeers",
	"ec2:DescribeSubnets",
	"ec2:DescribeInstances",
	"ec2:CreateRouteServerPeer",
	"ec2:DeleteRouteServerPeer",
	"ec2:CreateTags",
	"ec2:ModifyNetworkInterfaceAttribute",
}

// ambientCredentials asks the AWS SDK whether the pod already has
// credentials. On ROSA it does: the pod identity webhook injects a web
// identity token and a role ARN when the ServiceAccount is annotated, and
// the default chain resolves them. Overridden in tests.
var ambientCredentials = func(ctx context.Context, region string) (awssdk.CredentialsProvider, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, err
	}
	// LoadDefaultConfig assembles a chain without consulting it, so the
	// question of whether anything is actually there is only answered by
	// retrieving.
	if _, err := cfg.Credentials.Retrieve(ctx); err != nil {
		return nil, err
	}
	return cfg.Credentials, nil
}

// ResolveCredentials returns the credentials the operator should use to
// talk to EC2, asking the cluster for them if the pod has none.
//
// The two cases are the two ways OpenShift hands out cloud credentials.
// Where the cluster has an OIDC provider -- ROSA, and any STS install --
// the ServiceAccount annotation and the pod identity webhook mean the
// credentials are already in the pod, and the right thing is to use them
// and leave the cluster alone. Where it does not -- ordinary IPI -- the
// cloud credential operator mints on request, so the operator makes the
// request and reads the secret.
//
// Deciding between them by asking the SDK, rather than by inspecting the
// Infrastructure CR or sniffing environment variables, keeps the decision
// on the thing that actually matters: whether credentials can be
// retrieved.
func ResolveCredentials(ctx context.Context, c client.Client, namespace, region string) (awssdk.CredentialsProvider, error) {
	logger := log.FromContext(ctx)

	if provider, err := ambientCredentials(ctx, region); err == nil {
		logger.V(1).Info("using the credentials already available to the pod")
		return provider, nil
	}

	if err := ensureCredentialsRequest(ctx, c, namespace); err != nil {
		return nil, err
	}

	secret := &corev1.Secret{}
	err := c.Get(ctx, types.NamespacedName{Name: CredentialsSecretName, Namespace: namespace}, secret)
	switch {
	case apierrors.IsNotFound(err):
		return nil, fmt.Errorf("%w: secret %s/%s has not been minted", platform.ErrCredentialsPending, namespace, CredentialsSecretName)
	case err != nil:
		return nil, fmt.Errorf("reading secret %s/%s: %w", namespace, CredentialsSecretName, err)
	}

	id := string(secret.Data[accessKeyIDKey])
	key := string(secret.Data[secretAccessKeyKey])
	if id == "" || key == "" {
		return nil, fmt.Errorf("secret %s/%s has no %s and %s; if this cluster uses STS, annotate the %s ServiceAccount with its role ARN instead",
			namespace, CredentialsSecretName, accessKeyIDKey, secretAccessKeyKey, ServiceAccountName)
	}

	logger.V(1).Info("using credentials minted by the cloud credential operator", "secret", CredentialsSecretName)
	return credentials.NewStaticCredentialsProvider(id, key, ""), nil
}

// ensureCredentialsRequest creates the request if it is absent and
// otherwise leaves it alone. An existing request belongs to CCO once
// made -- it writes status onto it, and on STS clusters an administrator
// may have adjusted it -- so rewriting the spec on every reconcile would
// be a fight rather than a reconciliation.
func ensureCredentialsRequest(ctx context.Context, c client.Client, namespace string) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(CredentialsRequestGVK)
	err := c.Get(ctx, types.NamespacedName{
		Name:      CredentialsRequestName,
		Namespace: CredentialsRequestNamespace,
	}, existing)
	switch {
	case err == nil:
		return nil
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("reading CredentialsRequest %s: %w", CredentialsRequestName, err)
	}

	if err := c.Create(ctx, desiredCredentialsRequest(namespace)); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating CredentialsRequest %s: %w", CredentialsRequestName, err)
	}

	log.FromContext(ctx).Info("asked the cloud credential operator for AWS credentials",
		"credentialsRequest", CredentialsRequestName, "secret", CredentialsSecretName)
	return nil
}

func desiredCredentialsRequest(namespace string) *unstructured.Unstructured {
	actions := make([]interface{}, 0, len(policyActions))
	for _, action := range policyActions {
		actions = append(actions, action)
	}

	cr := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"spec": map[string]interface{}{
				"secretRef": map[string]interface{}{
					"name":      CredentialsSecretName,
					"namespace": namespace,
				},
				"serviceAccountNames": []interface{}{ServiceAccountName},
				"providerSpec": map[string]interface{}{
					"apiVersion": "cloudcredential.openshift.io/v1",
					"kind":       "AWSProviderSpec",
					"statementEntries": []interface{}{
						map[string]interface{}{
							"effect":   "Allow",
							"action":   actions,
							"resource": "*",
						},
					},
				},
			},
		},
	}
	cr.SetGroupVersionKind(CredentialsRequestGVK)
	cr.SetName(CredentialsRequestName)
	cr.SetNamespace(CredentialsRequestNamespace)
	return cr
}
