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

package azure_e2e

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkingv1alpha1 "github.com/openshift/bgp-cloud-connector/api/v1alpha1"
)

var _ = Describe("Azure E2E", Ordered, func() {

	BeforeAll(func(ctx context.Context) {
		By("applying CUDNBgpConfig CR")
		configCR := bgpConfig.DeepCopy()
		configCR.ResourceVersion = ""
		Expect(k8sClient.Create(ctx, configCR)).To(Succeed())

		By("waiting for the config to report Ready")
		Eventually(func(g Gomega) {
			cfg := &networkingv1alpha1.CUDNBgpConfig{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: configCR.Name}, cfg)).To(Succeed())
			g.Expect(meta.IsStatusConditionTrue(cfg.Status.Conditions, networkingv1alpha1.ConditionReady)).
				To(BeTrue(), "config should report Ready")
		}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())
	})

	AfterAll(func(ctx context.Context) {
		By("deleting CUDNBgpConfig CR")
		cfg := &networkingv1alpha1.CUDNBgpConfig{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: bgpConfig.Name}, cfg); err == nil {
			Expect(k8sClient.Delete(ctx, cfg)).To(Succeed())
			Eventually(func(g Gomega) {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: bgpConfig.Name}, cfg)
				g.Expect(client.IgnoreNotFound(err)).To(Succeed())
				g.Expect(err).To(HaveOccurred(), "config CR should be gone")
			}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())
		}
	})

	// ---------------------------------------------------------------
	// E2E-AZURE-01: self-healing, Route Server peering deleted
	// ---------------------------------------------------------------
	Context("E2E-AZURE-01: Route Server peering manually deleted", func() {
		It("should recreate the deleted peering within the reconcile window", func(ctx context.Context) {
			By("finding a peering this cluster owns")
			peerings, err := ourPeerings(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(peerings).NotTo(BeEmpty(), "expected peerings to exist before breaking one")

			victim := *peerings[0].Name
			var victimIP string
			if peerings[0].Properties != nil && peerings[0].Properties.PeerIP != nil {
				victimIP = *peerings[0].Properties.PeerIP
			}
			Expect(victimIP).NotTo(BeEmpty())
			GinkgoWriter.Printf("deleting Route Server peering %s (peerIp %s)\n", victim, victimIP)

			By("deleting it, which Azure takes about two minutes to do")
			poller, err := connection.BeginDelete(ctx, resourceGroup, routeServer, victim, nil)
			Expect(err).NotTo(HaveOccurred())
			_, err = poller.PollUntilDone(ctx, nil)
			Expect(err).NotTo(HaveOccurred())

			// Without this the recovery assertion could pass having never
			// observed the peering absent, which is the shape of a test that
			// proves nothing.
			By("confirming it is really gone")
			gone, err := ourPeerings(ctx)
			Expect(err).NotTo(HaveOccurred())
			for _, p := range gone {
				Expect(*p.Name).NotTo(Equal(victim), "peering %s should have been deleted", victim)
			}

			By("waiting for the operator to recreate it")
			Eventually(func(g Gomega) {
				current, err := ourPeerings(ctx)
				g.Expect(err).NotTo(HaveOccurred())
				found := false
				for _, p := range current {
					if p.Name != nil && *p.Name == victim {
						found = true
						g.Expect(p.Properties).NotTo(BeNil())
						g.Expect(p.Properties.PeerIP).NotTo(BeNil())
						g.Expect(*p.Properties.PeerIP).To(Equal(victimIP),
							"recreated peering should point at the same node")
					}
				}
				g.Expect(found).To(BeTrue(), "peering %s should be recreated", victim)
			}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())
		})
	})

	// ---------------------------------------------------------------
	// E2E-AZURE-02: self-healing, enableIPForwarding cleared
	// ---------------------------------------------------------------
	Context("E2E-AZURE-02: enableIPForwarding manually cleared", func() {
		It("should enable IP forwarding again within the reconcile window", func(ctx context.Context) {
			By("finding a router node to tamper with")
			nodes, err := routerNodes(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(nodes).NotTo(BeEmpty())

			node := nodes[0]
			nic, err := nicFor(ctx, &node)
			Expect(err).NotTo(HaveOccurred())
			Expect(nic).NotTo(BeNil(), "no interface is attached to %s", node.Name)
			Expect(nic.Properties.EnableIPForwarding).NotTo(BeNil())
			Expect(*nic.Properties.EnableIPForwarding).To(BeTrue(),
				"expected forwarding on before clearing it")
			nicName := *nic.Name

			// This is the failure measured on this very cluster: with
			// forwarding off every BGP session stays Established, the Route
			// Server still reports the pod prefix learned via eBGP, every
			// interface in the vnet still carries a route to the router nodes,
			// and every packet addressed to a pod is discarded.
			By("clearing enableIPForwarding on " + nicName)
			off := false
			nic.Properties.EnableIPForwarding = &off
			poller, err := interfaces.BeginCreateOrUpdate(ctx, resourceGroup, nicName, *nic, nil)
			Expect(err).NotTo(HaveOccurred())
			_, err = poller.PollUntilDone(ctx, nil)
			Expect(err).NotTo(HaveOccurred())

			By("confirming it is really off")
			cleared, err := nicFor(ctx, &node)
			Expect(err).NotTo(HaveOccurred())
			Expect(cleared).NotTo(BeNil())
			Expect(*cleared.Properties.EnableIPForwarding).To(BeFalse(),
				"forwarding should be off before the operator repairs it")

			By("waiting for the operator to enable it again")
			Eventually(func(g Gomega) {
				cur, err := nicFor(ctx, &node)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cur).NotTo(BeNil())
				g.Expect(cur.Properties.EnableIPForwarding).NotTo(BeNil())
				g.Expect(*cur.Properties.EnableIPForwarding).To(BeTrue(),
					"enableIPForwarding should be restored on %s", nicName)
			}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())
		})
	})
})
