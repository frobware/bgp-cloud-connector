package aws

import (
	"context"
	"fmt"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

func (p *Platform) reconcileRouteServerPeers(ctx context.Context, nodes []platform.RouterNode) error {
	logger := log.FromContext(ctx)

	nodesByAZ := make(map[string]map[string]bool)
	for _, n := range nodes {
		if nodesByAZ[n.Zone] == nil {
			nodesByAZ[n.Zone] = make(map[string]bool)
		}
		nodesByAZ[n.Zone][n.PrivateIP] = true
	}

	for az, endpointIDs := range p.endpointsByAZ {
		desiredIPs := nodesByAZ[az]

		for _, endpointID := range endpointIDs {
			managedPeers, err := p.listManagedPeers(ctx, endpointID)
			if err != nil {
				return fmt.Errorf("listing managed peers for endpoint %s: %w", endpointID, err)
			}

			managedIPs := make(map[string]string, len(managedPeers))
			for _, peer := range managedPeers {
				if peer.PeerAddress != nil && peer.RouteServerPeerId != nil {
					managedIPs[*peer.PeerAddress] = *peer.RouteServerPeerId
				}
			}

			for ip, peerID := range managedIPs {
				if !desiredIPs[ip] {
					logger.Info("deleting stale route server peer", "endpoint", endpointID, "peerIP", ip, "peerID", peerID, "az", az)
					if err := p.deletePeer(ctx, peerID); err != nil {
						return fmt.Errorf("deleting peer %s: %w", peerID, err)
					}
				}
			}

			allPeers, err := p.listAllPeers(ctx, endpointID)
			if err != nil {
				return fmt.Errorf("listing all peers for endpoint %s: %w", endpointID, err)
			}
			unmanagedIPs := make(map[string]string, len(allPeers))
			for _, peer := range allPeers {
				if peer.PeerAddress != nil && peer.RouteServerPeerId != nil {
					ip := *peer.PeerAddress
					if _, managed := managedIPs[ip]; !managed {
						unmanagedIPs[ip] = *peer.RouteServerPeerId
					}
				}
			}

			for ip := range desiredIPs {
				if _, exists := managedIPs[ip]; exists {
					continue
				}
				if peerID, exists := unmanagedIPs[ip]; exists {
					logger.Info("adopting pre-existing route server peer", "endpoint", endpointID, "peerIP", ip, "peerID", peerID, "az", az)
					if err := p.tagPeer(ctx, peerID); err != nil {
						return fmt.Errorf("tagging peer %s: %w", peerID, err)
					}
				} else {
					logger.Info("creating route server peer", "endpoint", endpointID, "peerIP", ip, "az", az)
					if err := p.createPeer(ctx, endpointID, ip); err != nil {
						return fmt.Errorf("creating peer for %s on %s: %w", ip, endpointID, err)
					}
				}
			}
		}
	}

	return nil
}

// ObservePeers reports the route server peers this operator owns as EC2 sees
// them.
//
// AWS splits the answer the same way Azure does, and for the same reason.
// State describes the peer resource: whether it finished being created.
// BgpStatus.Status describes the session running on it. A peer that is
// available with its BGP status down is a resource EC2 built exactly as asked,
// carrying a session that never came up.
//
// Only managed peers, by the same tag reconcile uses. A route server endpoint
// shared with something else keeps its peers out of this operator's status as
// well as out of its reconcile.
func (p *Platform) ObservePeers(ctx context.Context) ([]platform.ObservedPeer, error) {
	var out []platform.ObservedPeer

	for _, endpointIDs := range p.endpointsByAZ {
		for _, endpointID := range endpointIDs {
			peers, err := p.listManagedPeers(ctx, endpointID)
			if err != nil {
				return nil, fmt.Errorf("listing managed peers for endpoint %s: %w", endpointID, err)
			}
			for _, peer := range peers {
				observed := platform.ObservedPeer{
					Name:    aws.ToString(peer.RouteServerPeerId),
					Address: aws.ToString(peer.PeerAddress),
					State:   string(peer.State),
				}
				if peer.BgpOptions != nil && peer.BgpOptions.PeerAsn != nil {
					observed.ASN = *peer.BgpOptions.PeerAsn
				}
				if peer.BgpStatus != nil {
					observed.SessionState = string(peer.BgpStatus.Status)
				}
				out = append(out, observed)
			}
		}
	}

	// endpointsByAZ is a map, so the walk above has no order of its own.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (p *Platform) listManagedPeers(ctx context.Context, endpointID string) ([]ec2types.RouteServerPeer, error) {
	all, err := p.listAllPeers(ctx, endpointID)
	if err != nil {
		return nil, err
	}
	tagKey := managedByTagKey()
	tagVal := p.peerTagValue()
	var filtered []ec2types.RouteServerPeer
	for _, peer := range all {
		if hasTag(peer.Tags, tagKey, tagVal) {
			filtered = append(filtered, peer)
		}
	}
	return filtered, nil
}

func (p *Platform) listAllPeers(ctx context.Context, endpointID string) ([]ec2types.RouteServerPeer, error) {
	output, err := p.ec2Client.DescribeRouteServerPeers(ctx, &ec2.DescribeRouteServerPeersInput{})
	if err != nil {
		return nil, err
	}
	var filtered []ec2types.RouteServerPeer
	for _, peer := range output.RouteServerPeers {
		if aws.ToString(peer.RouteServerEndpointId) == endpointID {
			filtered = append(filtered, peer)
		}
	}
	return filtered, nil
}

func hasTag(tags []ec2types.Tag, key, value string) bool {
	for _, t := range tags {
		if aws.ToString(t.Key) == key && aws.ToString(t.Value) == value {
			return true
		}
	}
	return false
}

func (p *Platform) tagPeer(ctx context.Context, peerID string) error {
	input := &ec2.CreateTagsInput{
		Resources: []string{peerID},
		Tags:      p.peerTags(),
	}
	_, err := p.ec2Client.CreateTags(ctx, input)
	return err
}

func (p *Platform) createPeer(ctx context.Context, endpointID, peerAddress string) error {
	bgpOpts := &ec2types.RouteServerBgpOptionsRequest{
		PeerAsn: aws.Int64(p.localASN),
	}
	if p.livenessDetection == "bfd" {
		bgpOpts.PeerLivenessDetection = ec2types.RouteServerPeerLivenessMode("bfd")
	}
	input := &ec2.CreateRouteServerPeerInput{
		RouteServerEndpointId: aws.String(endpointID),
		PeerAddress:           aws.String(peerAddress),
		BgpOptions:            bgpOpts,
		TagSpecifications: []ec2types.TagSpecification{
			{
				ResourceType: ec2types.ResourceTypeRouteServerPeer,
				Tags:         p.peerTags(),
			},
		},
	}
	_, err := p.ec2Client.CreateRouteServerPeer(ctx, input)
	return err
}

func (p *Platform) deletePeer(ctx context.Context, peerID string) error {
	input := &ec2.DeleteRouteServerPeerInput{
		RouteServerPeerId: aws.String(peerID),
	}
	_, err := p.ec2Client.DeleteRouteServerPeer(ctx, input)
	return err
}

func (p *Platform) deleteAllManagedPeers(ctx context.Context) error {
	for _, endpointIDs := range p.endpointsByAZ {
		for _, endpointID := range endpointIDs {
			peers, err := p.listManagedPeers(ctx, endpointID)
			if err != nil {
				return fmt.Errorf("listing peers for endpoint %s: %w", endpointID, err)
			}
			for _, peer := range peers {
				if peer.RouteServerPeerId != nil {
					if err := p.deletePeer(ctx, *peer.RouteServerPeerId); err != nil {
						return fmt.Errorf("deleting peer %s: %w", *peer.RouteServerPeerId, err)
					}
				}
			}
		}
	}
	return nil
}
