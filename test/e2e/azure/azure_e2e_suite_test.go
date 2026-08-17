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

// Package azure_e2e covers what only Azure can be asked: whether the operator
// repairs its own cloud state after something outside it breaks the state.
//
// The shared suite covers everything cloud-neutral, and the azurelive tests
// prove the API calls are right. Neither covers drift.
//
// Everything here is slow. A Route Server peering takes about two minutes to
// create or delete, and the operator does them one at a time inside a single
// reconcile, so budget generously.
package azure_e2e

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
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
)

const (
	// Generous because an Azure peering create is roughly two minutes and the
	// operator may be part way through a serial pass over the other nodes when
	// the drift is noticed.
	reconcileTimeout = 20 * time.Minute
	pollInterval     = 20 * time.Second
)

var (
	k8sClient   client.Client
	connections *armnetwork.VirtualHubBgpConnectionsClient
	connection  *armnetwork.VirtualHubBgpConnectionClient
	interfaces  *armnetwork.InterfacesClient

	bgpConfig *networkingv1alpha1.CUDNBgpConfig

	clusterID     string
	resourceGroup string
	routeServer   string
	peeringPrefix string
)

func TestAzureE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Azure E2E Suite")
}

var _ = BeforeSuite(func() {
	profile := os.Getenv("E2E_PROFILE")
	Expect(profile).NotTo(BeEmpty(), "E2E_PROFILE must be set (e.g. make test-e2e-azure my-cluster)")
	manifestDir := filepath.Join("..", "..", "..", "test", "e2e", "manifests", profile)

	By("loading CUDNBgpConfig manifest from profile " + profile)
	bgpConfig = &networkingv1alpha1.CUDNBgpConfig{}
	loadManifest(filepath.Join(manifestDir, "cudnbgpconfig.yaml"), bgpConfig)
	Expect(bgpConfig.Spec.Azure).NotTo(BeNil(), "profile CUDNBgpConfig must have spec.azure")
	resourceGroup = bgpConfig.Spec.Azure.ResourceGroup
	routeServer = bgpConfig.Spec.Azure.RouteServerName

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

	By("building Azure clients from the default credential chain")
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	Expect(err).NotTo(HaveOccurred())
	factory, err := armnetwork.NewClientFactory(bgpConfig.Spec.Azure.SubscriptionID, cred, nil)
	Expect(err).NotTo(HaveOccurred())
	connections = factory.NewVirtualHubBgpConnectionsClient()
	connection = factory.NewVirtualHubBgpConnectionClient()
	interfaces = factory.NewInterfacesClient()

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

// ourPeerings returns the Route Server BGP connections this cluster owns. An
// Azure BGP connection carries no tags, so the name prefix is the only
// ownership signal there is.
func ourPeerings(ctx context.Context) ([]*armnetwork.BgpConnection, error) {
	var out []*armnetwork.BgpConnection
	pager := connections.NewListPager(resourceGroup, routeServer, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, c := range page.Value {
			if c.Name != nil && strings.HasPrefix(*c.Name, peeringPrefix) {
				out = append(out, c)
			}
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

// nicFor finds the network interface attached to a node's virtual machine.
// The interface is matched by the VM it is attached to rather than by name,
// because nothing guarantees a naming convention between the two.
func nicFor(ctx context.Context, node *corev1.Node) (*armnetwork.Interface, error) {
	vmID := strings.TrimPrefix(node.Spec.ProviderID, "azure://")
	pager := interfaces.NewListPager(resourceGroup, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, nic := range page.Value {
			if nic.Properties == nil || nic.Properties.VirtualMachine == nil ||
				nic.Properties.VirtualMachine.ID == nil {
				continue
			}
			// ARM returns ids with whatever casing was used to create them.
			if strings.EqualFold(*nic.Properties.VirtualMachine.ID, vmID) {
				return nic, nil
			}
		}
	}
	return nil, nil
}
