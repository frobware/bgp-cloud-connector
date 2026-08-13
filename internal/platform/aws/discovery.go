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

func (p *Platform) DiscoverEndpoints(ctx context.Context) (*platform.DiscoveryResult, error) {
	logger := log.FromContext(ctx)

	result := &platform.DiscoveryResult{}

	// Per zone, and private to this package: AWS Route Server endpoints are
	// per subnet and therefore per zone, which is why AWS is the only cloud
	// emitting more than one peer group. The endpoint map is kept because
	// peer reconciliation needs to know which endpoints belong to which zone.
	neighborsByZone := make(map[string][]platform.DiscoveredNeighbor)
	endpointsByZone := make(map[string][]string)

	for _, rsID := range p.routeServerIDs {
		rs, err := p.describeRouteServer(ctx, rsID)
		if err != nil {
			return nil, fmt.Errorf("describing route server %s: %w", rsID, err)
		}

		remoteASN := aws.ToInt64(rs.AmazonSideAsn)
		logger.Info("discovered route server", "routeServerID", rsID, "remoteASN", remoteASN)

		endpoints, err := p.describeRouteServerEndpoints(ctx, rsID)
		if err != nil {
			return nil, fmt.Errorf("listing endpoints for route server %s: %w", rsID, err)
		}

		subnetIDs := make([]string, 0, len(endpoints))
		for _, ep := range endpoints {
			if ep.SubnetId != nil {
				subnetIDs = append(subnetIDs, *ep.SubnetId)
			}
		}
		subnetAZMap, err := p.resolveSubnetAZs(ctx, subnetIDs)
		if err != nil {
			return nil, fmt.Errorf("resolving subnet AZs for route server %s: %w", rsID, err)
		}

		for _, ep := range endpoints {
			epID := aws.ToString(ep.RouteServerEndpointId)
			address := aws.ToString(ep.EniAddress)
			subnetID := aws.ToString(ep.SubnetId)
			az := subnetAZMap[subnetID]

			logger.Info("discovered endpoint", "endpointID", epID, "az", az, "address", address)

			neighborsByZone[az] = append(neighborsByZone[az], platform.DiscoveredNeighbor{
				Address: address,
				ASN:     remoteASN,
			})
			endpointsByZone[az] = append(endpointsByZone[az], epID)
		}

	}

	result.PeerGroups = peerGroupsByAZ(neighborsByZone)

	p.endpointsByAZ = endpointsByZone
	return result, nil
}

// peerGroupsByAZ renders discovered neighbours as one peer group per
// availability zone, ordered by AZ name so that generated object names are
// stable across reconciles.
func peerGroupsByAZ(neighborsByAZ map[string][]platform.DiscoveredNeighbor) []platform.PeerGroup {
	azNames := make([]string, 0, len(neighborsByAZ))
	for az := range neighborsByAZ {
		azNames = append(azNames, az)
	}
	sort.Strings(azNames)

	groups := make([]platform.PeerGroup, 0, len(azNames))
	for _, az := range azNames {
		groups = append(groups, platform.PeerGroup{
			Key:          az,
			NodeSelector: map[string]string{"topology.kubernetes.io/zone": az},
			Neighbors:    neighborsByAZ[az],
		})
	}
	return groups
}

func (p *Platform) describeRouteServer(ctx context.Context, routeServerID string) (*ec2types.RouteServer, error) {
	output, err := p.ec2Client.DescribeRouteServers(ctx, &ec2.DescribeRouteServersInput{
		RouteServerIds: []string{routeServerID},
	})
	if err != nil {
		return nil, err
	}
	if len(output.RouteServers) == 0 {
		return nil, fmt.Errorf("route server %s not found", routeServerID)
	}
	return &output.RouteServers[0], nil
}

func (p *Platform) describeRouteServerEndpoints(ctx context.Context, routeServerID string) ([]ec2types.RouteServerEndpoint, error) {
	output, err := p.ec2Client.DescribeRouteServerEndpoints(ctx, &ec2.DescribeRouteServerEndpointsInput{})
	if err != nil {
		return nil, err
	}
	var filtered []ec2types.RouteServerEndpoint
	for _, ep := range output.RouteServerEndpoints {
		if aws.ToString(ep.RouteServerId) == routeServerID {
			filtered = append(filtered, ep)
		}
	}
	return filtered, nil
}

func (p *Platform) resolveSubnetAZs(ctx context.Context, subnetIDs []string) (map[string]string, error) {
	if len(subnetIDs) == 0 {
		return nil, nil
	}
	output, err := p.ec2Client.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		SubnetIds: subnetIDs,
	})
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(output.Subnets))
	for _, s := range output.Subnets {
		result[aws.ToString(s.SubnetId)] = aws.ToString(s.AvailabilityZone)
	}
	return result, nil
}
