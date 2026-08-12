package gcp

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
)

// pollInterval is how often a long-running GCE operation is re-read.
const pollInterval = 500 * time.Millisecond

// NewComputeClient builds a ComputeClient using Application Default
// Credentials, which on a cluster come from the mounted Workload Identity
// Federation credential.
func NewComputeClient(ctx context.Context, project, region string) (ComputeClient, error) {
	svc, err := compute.NewService(ctx, option.WithScopes(compute.CloudPlatformScope))
	if err != nil {
		return nil, err
	}
	return &computeClient{svc: svc, project: project, region: region}, nil
}

type computeClient struct {
	svc     *compute.Service
	project string
	region  string
}

func (c *computeClient) EnsureCanIPForward(ctx context.Context, node RouterNode) (bool, error) {
	zone := shortZone(node.Zone)
	inst, err := c.svc.Instances.Get(c.project, zone, node.Name).Context(ctx).Do()
	if err != nil {
		return false, err
	}
	if inst.CanIpForward {
		return false, nil
	}
	inst.CanIpForward = true
	op, err := c.svc.Instances.Update(c.project, zone, node.Name, inst).
		MostDisruptiveAllowedAction("REFRESH").
		Context(ctx).
		Do()
	if err != nil {
		return false, err
	}
	if err := c.waitZoneOp(ctx, zone, op); err != nil {
		return false, err
	}
	return true, nil
}

func (c *computeClient) EnsureNestedVirtualization(ctx context.Context, node RouterNode) (bool, error) {
	zone := shortZone(node.Zone)
	inst, err := c.svc.Instances.Get(c.project, zone, node.Name).Context(ctx).Do()
	if err != nil {
		return false, err
	}
	if inst.AdvancedMachineFeatures != nil && inst.AdvancedMachineFeatures.EnableNestedVirtualization {
		return false, nil
	}
	if inst.AdvancedMachineFeatures == nil {
		inst.AdvancedMachineFeatures = &compute.AdvancedMachineFeatures{}
	}
	inst.AdvancedMachineFeatures.EnableNestedVirtualization = true
	// GCE rejects REFRESH for this field and returns 400 demanding RESTART.
	op, err := c.svc.Instances.Update(c.project, zone, node.Name, inst).
		MostDisruptiveAllowedAction("RESTART").
		Context(ctx).
		Do()
	if err != nil {
		return false, err
	}
	if err := c.waitZoneOp(ctx, zone, op); err != nil {
		return false, err
	}
	return true, nil
}

func (c *computeClient) GetRouterTopology(ctx context.Context, routerName string) (*CloudRouterTopology, error) {
	r, err := c.svc.Routers.Get(c.project, c.region, routerName).Context(ctx).Do()
	if err != nil {
		return nil, err
	}

	topology := &CloudRouterTopology{}
	for _, iface := range r.Interfaces {
		ip := iface.IpRange
		if idx := strings.Index(ip, "/"); idx >= 0 {
			ip = ip[:idx]
		}
		topology.InterfaceNames = append(topology.InterfaceNames, iface.Name)
		topology.InterfaceIPs = append(topology.InterfaceIPs, ip)
	}
	if r.Bgp != nil {
		topology.ASN = r.Bgp.Asn
	}
	return topology, nil
}

func (c *computeClient) ReconcilePeers(ctx context.Context, routerName, clusterID string, nodes []RouterNode, topology *CloudRouterTopology, localASN int64) (bool, error) {
	r, err := c.svc.Routers.Get(c.project, c.region, routerName).Context(ctx).Do()
	if err != nil {
		return false, err
	}

	desired := mergePeers(r.BgpPeers, desiredPeers(clusterID, nodes, topology, localASN), clusterID)
	if buildPeerSet(r.BgpPeers).equal(buildPeerSet(desired)) {
		return false, nil
	}

	op, err := c.svc.Routers.Patch(c.project, c.region, routerName, &compute.Router{BgpPeers: desired}).Context(ctx).Do()
	if err != nil {
		return false, err
	}
	if err := c.waitRegionOp(ctx, op); err != nil {
		return false, err
	}
	return true, nil
}

// desiredPeers builds one Cloud Router peer per (node, router interface)
// pair, ordered by name so an unchanged node set produces an identical list.
func desiredPeers(clusterID string, nodes []RouterNode, topology *CloudRouterTopology, localASN int64) []*compute.RouterBgpPeer {
	var peers []*compute.RouterBgpPeer
	for _, node := range nodes {
		for ifaceIdx, ifaceName := range topology.InterfaceNames {
			if ifaceIdx >= len(topology.InterfaceIPs) {
				break
			}
			peers = append(peers, &compute.RouterBgpPeer{
				Name:                    PeerName(clusterID, node.IPAddress, ifaceIdx),
				InterfaceName:           ifaceName,
				PeerIpAddress:           node.IPAddress,
				IpAddress:               topology.InterfaceIPs[ifaceIdx],
				PeerAsn:                 localASN,
				RouterApplianceInstance: node.SelfLink,
			})
		}
	}
	sortPeers(peers)
	return peers
}

// maxPeerNameLength is the GCE limit on a Cloud Router peer name.
const maxPeerNameLength = 63

// PeerName is the Cloud Router peer name for a node address and router
// interface.
//
// The address is the key, not the node's position in any list. Naming peers
// positionally means a node joining renumbers every peer after it, so the
// router is rewritten with each name pointing at a different node and every
// session drops. The address is unique per node and short, where node names
// are long enough to threaten the 63 character limit on their own.
func PeerName(clusterID, ipAddress string, ifaceIdx int) string {
	suffix := fmt.Sprintf("-%s-%d", strings.ReplaceAll(ipAddress, ".", "-"), ifaceIdx)
	prefix := peerPrefix(clusterID)
	if len(prefix)+len(suffix) > maxPeerNameLength {
		prefix = prefix[:maxPeerNameLength-len(suffix)]
	}
	return prefix + suffix
}

// peerPrefix is the ownership signal. GCP BGP peers are fields inside the
// router resource and cannot carry labels, so unlike the AWS tag this is the
// only marker available, and it is why nothing here touches a peer whose name
// does not carry it.
func peerPrefix(clusterID string) string {
	return clusterID + "-bgp"
}

// isOurPeer reports whether a peer name was generated by this operator for
// this cluster. The trailing separator matters: without it a cluster named
// "cluster" would claim the peers of one named "cluster-two".
func isOurPeer(name, clusterID string) bool {
	return strings.HasPrefix(name, peerPrefix(clusterID)+"-")
}

// mergePeers returns the peer list to write: every peer that is not ours,
// left exactly as found, plus our desired set. A Cloud Router patch replaces
// the whole list, so anything omitted here is deleted, including peers
// belonging to another cluster sharing the router.
func mergePeers(existing, desired []*compute.RouterBgpPeer, clusterID string) []*compute.RouterBgpPeer {
	out := make([]*compute.RouterBgpPeer, 0, len(existing)+len(desired))
	for _, p := range existing {
		if p != nil && !isOurPeer(p.Name, clusterID) {
			out = append(out, p)
		}
	}
	out = append(out, desired...)
	sortPeers(out)
	return out
}

func sortPeers(peers []*compute.RouterBgpPeer) {
	sort.Slice(peers, func(i, j int) bool { return peers[i].Name < peers[j].Name })
}

// ClearPeers removes the peers this operator created and leaves every other
// peer on the router alone, so tearing down one cluster does not disconnect
// another that shares the router.
func (c *computeClient) ClearPeers(ctx context.Context, routerName, clusterID string) (bool, error) {
	r, err := c.svc.Routers.Get(c.project, c.region, routerName).Context(ctx).Do()
	if err != nil {
		return false, err
	}
	kept := mergePeers(r.BgpPeers, nil, clusterID)
	if len(kept) == len(r.BgpPeers) {
		return false, nil
	}
	r.BgpPeers = kept
	op, err := c.svc.Routers.Update(c.project, c.region, routerName, r).Context(ctx).Do()
	if err != nil {
		return false, err
	}
	if err := c.waitRegionOp(ctx, op); err != nil {
		return false, err
	}
	return true, nil
}

func (c *computeClient) waitZoneOp(ctx context.Context, zone string, op *compute.Operation) error {
	if op == nil {
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
		cur, err := c.svc.ZoneOperations.Get(c.project, zone, op.Name).Context(ctx).Do()
		if err != nil {
			return err
		}
		if cur.Status == "DONE" {
			if cur.Error != nil {
				return fmt.Errorf("zone operation %s failed: %v", op.Name, cur.Error)
			}
			return nil
		}
	}
}

func (c *computeClient) waitRegionOp(ctx context.Context, op *compute.Operation) error {
	if op == nil {
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
		cur, err := c.svc.RegionOperations.Get(c.project, c.region, op.Name).Context(ctx).Do()
		if err != nil {
			return err
		}
		if cur.Status == "DONE" {
			if cur.Error != nil {
				return fmt.Errorf("region operation %s failed: %v", op.Name, cur.Error)
			}
			return nil
		}
	}
}

type peerKey struct {
	name   string
	peerIP string
	asn    int64
}

type peerSet map[peerKey]struct{}

func buildPeerSet(peers []*compute.RouterBgpPeer) peerSet {
	s := make(peerSet, len(peers))
	for _, p := range peers {
		if p == nil {
			continue
		}
		s[peerKey{name: p.Name, peerIP: p.PeerIpAddress, asn: p.PeerAsn}] = struct{}{}
	}
	return s
}

func (a peerSet) equal(b peerSet) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}
