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

// Package gcp_e2e covers what only GCP can be asked: whether the operator
// repairs its own cloud state after something outside it breaks the state.
//
// The shared suite already covers everything cloud-neutral, and the gcplive
// tests already prove the API calls are right. Neither covers drift: gcplive
// calls ReconcileNodes twice and checks the second is a no-op, which is
// convergence, not recovery.
package gcp_e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	compute "google.golang.org/api/compute/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkingv1alpha1 "github.com/openshift/bgp-cloud-connector/api/v1alpha1"
	gcpplatform "github.com/openshift/bgp-cloud-connector/internal/platform/gcp"
)

const (
	// The operator resyncs on its own schedule, and drift is repaired on the
	// next pass rather than on a watch event: nothing in GCP notifies the
	// operator that a peer vanished.
	//
	// Run this against the default resync interval, not a shortened one. Each
	// spec breaks something and then checks it is really broken before waiting
	// for the repair, and with a short resync the operator wins that race, so
	// the check fails on an operator that is working perfectly.
	reconcileTimeout = 8 * time.Minute
	pollInterval     = 15 * time.Second
)

var (
	k8sClient     client.Client
	computeSvc    *compute.Service
	bgpConfig     *networkingv1alpha1.CUDNBgpConfig
	clusterID     string
	project       string
	region        string
	cloudRouter   string
	peeringPrefix string
)

func TestGCPE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "GCP E2E Suite")
}

var _ = BeforeSuite(func() {
	profile := os.Getenv("E2E_PROFILE")
	Expect(profile).NotTo(BeEmpty(), "E2E_PROFILE must be set (e.g. make test-e2e-gcp my-cluster)")
	manifestDir := filepath.Join("..", "..", "..", "test", "e2e", "manifests", profile)

	By("loading CUDNBgpConfig manifest from profile " + profile)
	bgpConfig = &networkingv1alpha1.CUDNBgpConfig{}
	loadManifest(filepath.Join(manifestDir, "cudnbgpconfig.yaml"), bgpConfig)
	Expect(bgpConfig.Spec.GCP).NotTo(BeNil(), "profile CUDNBgpConfig must have spec.gcp")
	project = bgpConfig.Spec.GCP.Project
	region = bgpConfig.Spec.GCP.Region
	cloudRouter = bgpConfig.Spec.GCP.CloudRouterName

	By("building kubernetes client")
	scheme := runtime.NewScheme()
	Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
	Expect(networkingv1alpha1.AddToScheme(scheme)).To(Succeed())
	addUnstructuredTypes(scheme)

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	restCfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{},
	).ClientConfig()
	Expect(err).NotTo(HaveOccurred())
	k8sClient, err = client.New(restCfg, client.Options{Scheme: scheme})
	Expect(err).NotTo(HaveOccurred())

	By("building GCP compute client from application default credentials")
	computeSvc, err = compute.NewService(context.Background())
	Expect(err).NotTo(HaveOccurred())

	By("reading cluster infrastructure name, which is the ownership prefix")
	infra := &unstructured.Unstructured{}
	infra.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "config.openshift.io", Version: "v1", Kind: "Infrastructure",
	})
	Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: "cluster"}, infra)).To(Succeed())
	name, found, err := unstructured.NestedString(infra.Object, "status", "infrastructureName")
	Expect(err).NotTo(HaveOccurred())
	Expect(found).To(BeTrue())
	clusterID = name
	peeringPrefix = clusterID + "-bgp-"
})

func loadManifest(path string, obj runtime.Object) {
	data, err := os.ReadFile(path)
	Expect(err).NotTo(HaveOccurred(), "reading manifest %s", path)
	Expect(yaml.NewYAMLOrJSONDecoder(
		bytes.NewReader(data), 4096,
	).Decode(obj)).To(Succeed(), "decoding manifest %s", path)
}

func addUnstructuredTypes(s *runtime.Scheme) {
	for _, gvk := range []schema.GroupVersionKind{
		{Group: "frrk8s.metallb.io", Version: "v1beta1", Kind: "FRRConfiguration"},
		{Group: "k8s.ovn.org", Version: "v1", Kind: "ClusterUserDefinedNetwork"},
		{Group: "k8s.ovn.org", Version: "v1", Kind: "RouteAdvertisements"},
	} {
		s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
}

// ourPeers returns the Cloud Router BGP peers this cluster owns. A Cloud Router
// peer is a field inside the router rather than a resource of its own, so the
// name prefix is the only ownership signal there is.
func ourPeers(ctx context.Context) ([]*compute.RouterBgpPeer, error) {
	router, err := computeSvc.Routers.Get(project, region, cloudRouter).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	var out []*compute.RouterBgpPeer
	for _, p := range router.BgpPeers {
		if strings.HasPrefix(p.Name, peeringPrefix) {
			out = append(out, p)
		}
	}
	return out, nil
}

func routerNodes(ctx context.Context) ([]corev1.Node, error) {
	nodeList := &corev1.NodeList{}
	if err := k8sClient.List(ctx, nodeList, client.MatchingLabels(bgpConfig.Spec.RouterNodeSelector)); err != nil {
		return nil, err
	}
	var out []corev1.Node
	for _, n := range nodeList.Items {
		if n.Spec.ProviderID != "" {
			out = append(out, n)
		}
	}
	return out, nil
}

func instanceOf(ctx context.Context, node *corev1.Node) (*compute.Instance, gcpplatform.Instance, error) {
	ref, err := gcpplatform.ParseProviderID(node.Spec.ProviderID)
	if err != nil {
		return nil, ref, err
	}
	inst, err := computeSvc.Instances.Get(project, ref.Zone, ref.Name).Context(ctx).Do()
	return inst, ref, err
}

func waitForOperation(ctx context.Context, op *compute.Operation, zone string) error {
	for i := 0; i < 120; i++ {
		var cur *compute.Operation
		var err error
		if zone == "" {
			cur, err = computeSvc.RegionOperations.Get(project, region, op.Name).Context(ctx).Do()
		} else {
			cur, err = computeSvc.ZoneOperations.Get(project, zone, op.Name).Context(ctx).Do()
		}
		if err != nil {
			return err
		}
		if cur.Status == "DONE" {
			if cur.Error != nil && len(cur.Error.Errors) > 0 {
				return fmt.Errorf("operation failed: %s", cur.Error.Errors[0].Message)
			}
			return nil
		}
	}
	return fmt.Errorf("operation %s did not finish", op.Name)
}
