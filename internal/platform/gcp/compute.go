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

	desired := desiredPeers(clusterID, nodes, topology, localASN)
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

// desiredPeers builds one Cloud Router peer per (node, router interface) pair.
// Nodes are sorted by name so peer names are stable across reconciles.
func desiredPeers(clusterID string, nodes []RouterNode, topology *CloudRouterTopology, localASN int64) []*compute.RouterBgpPeer {
	sorted := append([]RouterNode(nil), nodes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var peers []*compute.RouterBgpPeer
	for idx := range sorted {
		for ifaceIdx, ifaceName := range topology.InterfaceNames {
			if ifaceIdx >= len(topology.InterfaceIPs) {
				break
			}
			peers = append(peers, &compute.RouterBgpPeer{
				Name:                    PeerName(clusterID, idx, ifaceIdx),
				InterfaceName:           ifaceName,
				PeerIpAddress:           sorted[idx].IPAddress,
				IpAddress:               topology.InterfaceIPs[ifaceIdx],
				PeerAsn:                 localASN,
				RouterApplianceInstance: sorted[idx].SelfLink,
			})
		}
	}
	return peers
}

// PeerName is the Cloud Router peer name for a node and router interface.
func PeerName(clusterID string, nodeIdx, ifaceIdx int) string {
	return fmt.Sprintf("%s-bgp-peer-%d-%d", clusterID, nodeIdx, ifaceIdx)
}

func (c *computeClient) ClearPeers(ctx context.Context, routerName string) (bool, error) {
	r, err := c.svc.Routers.Get(c.project, c.region, routerName).Context(ctx).Do()
	if err != nil {
		return false, err
	}
	if len(r.BgpPeers) == 0 {
		return false, nil
	}
	r.BgpPeers = nil
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
