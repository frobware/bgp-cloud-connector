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

package azure

import (
	"context"
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

// A router node forwards packets addressed to pods, and every cloud's software
// defined network refuses that by default. Each one calls it something
// different, and AWS names its flag after the check while the other two name
// theirs after the capability, so the polarity reads backwards on exactly one
// of the three:
//
//	AWS    SourceDestCheck      default true   -> must be CLEARED to forward
//	GCP    canIpForward         default false  -> must be SET to forward
//	Azure  enableIPForwarding   default false  -> must be SET to forward
//
// The names above are each cloud's own and belong at each cloud's client
// boundary. Nothing above that boundary should use them: a shared layer that
// speaks of "SourceDestCheck" or "IP forwarding" invites somebody to wire a
// cloud up with the polarity inverted, and the result is a cluster where every
// signal is green and no packet reaches a pod. Hence ensureNodesCanForward,
// which says what it is for.
//
// Azure's own words for what the flag does, from the network interface
// documentation, are worth keeping to hand because they say exactly why a
// router node needs it:
//
//	IP forwarding enables a NIC attached to a VM to:
//	  - Receive network traffic not destined for any of the IP addresses
//	    assigned in any of the NIC's IP configurations.
//	  - Send network traffic with a different source IP address than is
//	    assigned in any of the NIC's IP configurations.
//
// A packet for a pod is addressed to the pod, not to the node's NIC, so
// without this Azure discards it on the host and the VM never sees it.
//
// "IP forwarding" is Azure's name and is kept at the client boundary so it
// greps back to their documentation, but it is not the guest's
// net.ipv4.ip_forward and must not be read as it. Enforcement is in Azure's
// network, upstream of the VM: the packet is dropped before it reaches the
// vNIC, nothing inside the guest can observe it, and no sysctl changes it. On
// an OVN-Kubernetes node that sysctl is 0 and irrelevant either way, because
// OVN forwards in OVS rather than through the kernel IP stack.
//
// AWS's SourceDestCheck is the most honest of the three names: what all of
// them configure is whether the cloud enforces that a packet's source or
// destination is the interface's own address.
//
// Without it the BGP side is entirely unaffected -- sessions establish, the
// Route Server learns the pod prefix, and every NIC in the vnet is programmed
// with a route to the router nodes -- and every packet addressed to a pod is
// discarded. There is no signal anywhere that says so.

// NIC is one Azure network interface, reduced to what the per-node work needs.
type NIC struct {
	Name          string
	ResourceGroup string
	// VMID is the ARM resource id of the virtual machine this interface is
	// attached to, empty when it is attached to nothing.
	VMID string
	// IPForwarding is Azure's enableIPForwarding: true means this interface
	// may carry traffic that is not addressed to or from it.
	IPForwarding bool
}

// nicAPI is the slice of Azure the per-node work uses, kept separate from
// routeServerAPI so the tests can drive one without the other.
type nicAPI interface {
	ListNICs(ctx context.Context, resourceGroup string) ([]NIC, error)
	EnableIPForwarding(ctx context.Context, resourceGroup, name string) error
}

// vmResourceID turns a node's providerID into the resource group it lives in
// and the ARM resource id of its virtual machine.
//
// A Kubernetes providerID on Azure is the ARM id with a scheme bolted on:
//
//	azure:///subscriptions/S/resourceGroups/G/providers/Microsoft.Compute/virtualMachines/N
//
// Anything that does not parse is an error rather than an empty resource
// group. An empty group would list the NICs of nothing, find no match, and
// leave forwarding off on a node that needs it -- which is the failure this
// whole file exists to prevent, arrived at by a different route.
func vmResourceID(providerID string) (resourceGroup, id string, err error) {
	id = strings.TrimPrefix(providerID, "azure://")
	if id == providerID && !strings.HasPrefix(id, "/subscriptions/") {
		return "", "", fmt.Errorf("providerID %q is not an Azure one", providerID)
	}
	if !strings.HasPrefix(id, "/subscriptions/") {
		return "", "", fmt.Errorf("providerID %q has no subscription", providerID)
	}

	// Case-insensitively, because ARM returns ids with whatever casing was
	// used to create them and nothing guarantees the two agree.
	parts := strings.Split(id, "/")
	for i := 0; i+1 < len(parts); i++ {
		if strings.EqualFold(parts[i], "resourceGroups") {
			resourceGroup = parts[i+1]
			break
		}
	}
	if resourceGroup == "" {
		return "", "", fmt.Errorf("providerID %q names no resource group", providerID)
	}
	return resourceGroup, id, nil
}

// ensureNodesCanForward makes every router node able to forward packets
// addressed to pods.
//
// Acts on the same node set as desiredPeerings, which means skipping a node
// with no address. Such a node is part way through registering: it will not
// become a peer, so nothing will be routed through it and it has nothing to
// forward. Erroring on one instead would block the reconcile for every other
// node in the cluster, which is a far worse failure than waiting a few seconds
// for the node to finish coming up.
//
// Grouped by resource group so a cluster whose nodes share one -- which is
// every cluster openshift-install builds -- lists NICs once rather than once
// per node.
func (p *Platform) ensureNodesCanForward(ctx context.Context, nodes []platform.RouterNode) error {
	if len(nodes) == 0 {
		return nil
	}
	logger := log.FromContext(ctx)

	type target struct {
		node platform.RouterNode
		vmID string
	}
	byGroup := make(map[string][]target)
	for _, n := range nodes {
		if n.PrivateIP == "" {
			continue
		}
		rg, vmID, err := vmResourceID(n.ProviderID)
		if err != nil {
			return fmt.Errorf("node %q: %w", n.Name, err)
		}
		byGroup[rg] = append(byGroup[rg], target{node: n, vmID: vmID})
	}

	for rg, targets := range byGroup {
		nics, err := p.nics.ListNICs(ctx, rg)
		if err != nil {
			return fmt.Errorf("listing network interfaces in %q: %w", rg, err)
		}

		for _, t := range targets {
			var found bool
			for _, nic := range nics {
				if nic.VMID == "" || !strings.EqualFold(nic.VMID, t.vmID) {
					continue
				}
				found = true
				if nic.IPForwarding {
					continue
				}
				// Only written when it is wrong. Both controllers watch what
				// they write, so rewriting a correct NIC on every pass would
				// feed a reconcile loop.
				if err := p.nics.EnableIPForwarding(ctx, nic.ResourceGroup, nic.Name); err != nil {
					return fmt.Errorf("enabling IP forwarding on %q: %w", nic.Name, err)
				}
				logger.Info("enabled IP forwarding on router node interface",
					"node", t.node.Name, "nic", nic.Name)
			}

			// Reported rather than passed over. A router node whose NIC we
			// cannot find will drop every packet addressed to a pod while
			// reporting a healthy BGP session, and nothing else in the stack
			// says so.
			if !found {
				return fmt.Errorf("no network interface in %q is attached to node %q (%s)",
					rg, t.node.Name, t.vmID)
			}
		}
	}

	return nil
}
