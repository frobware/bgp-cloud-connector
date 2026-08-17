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

package gcp_e2e

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	compute "google.golang.org/api/compute/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkingv1alpha1 "github.com/openshift/bgp-cloud-connector/api/v1alpha1"
)

var _ = Describe("GCP E2E", Ordered, func() {

	BeforeAll(func(ctx context.Context) {
		By("applying CUDNBgpConfig CR")
		configCR := bgpConfig.DeepCopy()
		configCR.ResourceVersion = ""
		Expect(k8sClient.Create(ctx, configCR)).To(Succeed())

		By("waiting for config phase=Ready")
		Eventually(func(g Gomega) {
			cfg := &networkingv1alpha1.CUDNBgpConfig{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: configCR.Name}, cfg)).To(Succeed())
			g.Expect(cfg.Status.Phase).To(Equal(networkingv1alpha1.PhaseReady))
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
	// E2E-GCP-01: self-healing, Cloud Router peer deleted
	// ---------------------------------------------------------------
	Context("E2E-GCP-01: Cloud Router peer manually deleted", func() {
		It("should recreate the deleted peer within the reconcile window", func(ctx context.Context) {
			By("finding a peer this cluster owns")
			peers, err := ourPeers(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(peers).NotTo(BeEmpty(), "expected peers to exist before breaking one")

			victim := peers[0].Name
			victimIP := peers[0].PeerIpAddress
			GinkgoWriter.Printf("deleting Cloud Router peer %s (peerIp %s)\n", victim, victimIP)

			// A peer is a field inside the router rather than a resource of
			// its own, so removing one means patching the router with the set
			// that should remain.
			By("removing it from the router, which is where a GCP peer lives")
			router, err := computeSvc.Routers.Get(project, region, cloudRouter).Context(ctx).Do()
			Expect(err).NotTo(HaveOccurred())
			kept := make([]*compute.RouterBgpPeer, 0, len(router.BgpPeers))
			for _, p := range router.BgpPeers {
				if p.Name != victim {
					kept = append(kept, p)
				}
			}
			Expect(kept).To(HaveLen(len(router.BgpPeers)-1), "exactly one peer should have been removed")

			// ForceSendFields matters when the last peer is removed: without
			// it an empty slice is omitted and the patch becomes a no-op.
			patch := &compute.Router{BgpPeers: kept}
			patch.ForceSendFields = []string{"BgpPeers"}
			op, err := computeSvc.Routers.Patch(project, region, cloudRouter, patch).Context(ctx).Do()
			Expect(err).NotTo(HaveOccurred())
			Expect(waitForOperation(ctx, op, "")).To(Succeed())

			// Without this the recovery assertion could pass having never
			// observed the peer absent, which is the shape of a test that
			// proves nothing.
			By("confirming it is really gone")
			gone, err := ourPeers(ctx)
			Expect(err).NotTo(HaveOccurred())
			for _, p := range gone {
				Expect(p.Name).NotTo(Equal(victim), "peer %s should have been deleted", victim)
			}

			By("waiting for the operator to recreate it")
			Eventually(func(g Gomega) {
				current, err := ourPeers(ctx)
				g.Expect(err).NotTo(HaveOccurred())
				found := false
				for _, p := range current {
					if p.Name == victim {
						found = true
						g.Expect(p.PeerIpAddress).To(Equal(victimIP),
							"recreated peer should point at the same node")
					}
				}
				g.Expect(found).To(BeTrue(), "peer %s should be recreated", victim)
			}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())
		})
	})

	// ---------------------------------------------------------------
	// E2E-GCP-02: self-healing, canIpForward cleared
	// ---------------------------------------------------------------
	Context("E2E-GCP-02: canIpForward manually cleared", func() {
		It("should set canIpForward again within the reconcile window", func(ctx context.Context) {
			By("finding a router node to tamper with")
			nodes, err := routerNodes(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(nodes).NotTo(BeEmpty())

			node := nodes[0]
			inst, ref, err := instanceOf(ctx, &node)
			Expect(err).NotTo(HaveOccurred())
			Expect(inst.CanIpForward).To(BeTrue(), "expected forwarding on before clearing it")

			// The GCP counterpart of re-enabling SourceDestCheck, and the same
			// silent failure: with forwarding off the session stays up, the
			// Cloud Router still advertises the pod prefix with this node as a
			// next hop, and every packet to a pod is discarded.
			//
			// REFRESH is what makes this possible on a running node. The
			// obvious check says otherwise, since gcloud has no
			// --can-ip-forward flag on update, but Google lists the field
			// among those needing only a refresh.
			By("clearing canIpForward on " + node.Name)
			inst.CanIpForward = false
			op, err := computeSvc.Instances.Update(project, ref.Zone, ref.Name, inst).
				MostDisruptiveAllowedAction("REFRESH").
				Context(ctx).Do()
			Expect(err).NotTo(HaveOccurred())
			Expect(waitForOperation(ctx, op, ref.Zone)).To(Succeed())

			By("confirming it is really off")
			cleared, _, err := instanceOf(ctx, &node)
			Expect(err).NotTo(HaveOccurred())
			Expect(cleared.CanIpForward).To(BeFalse(), "canIpForward should be off before the operator repairs it")

			By("waiting for the operator to set it back")
			Eventually(func(g Gomega) {
				cur, _, err := instanceOf(ctx, &node)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cur.CanIpForward).To(BeTrue(),
					"canIpForward should be restored on %s", node.Name)
			}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())
		})
	})
})
