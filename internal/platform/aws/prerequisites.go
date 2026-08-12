package aws

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// CheckPrerequisites reports the AWS configuration the operator relies on and
// does not create.
//
// Route server propagation is the one that matters and the one that hides.
// Without it every peer reaches available, every BGP session establishes, FRR
// advertises the CUDN prefix, and the routes go nowhere: they stay in the
// route server and never reach a VPC route table, so nothing in the VPC can
// reach a pod. Every signal the operator produces looks healthy.
func (p *Platform) CheckPrerequisites(ctx context.Context) ([]string, error) {
	var unmet []string

	for _, rsID := range p.routeServerIDs {
		out, err := p.ec2Client.GetRouteServerPropagations(ctx, &ec2.GetRouteServerPropagationsInput{
			RouteServerId: aws.String(rsID),
		})
		if err != nil {
			return nil, fmt.Errorf("reading propagations for route server %s: %w", rsID, err)
		}

		live := 0
		for _, prop := range out.RouteServerPropagations {
			switch prop.State {
			case ec2types.RouteServerPropagationStateAvailable, ec2types.RouteServerPropagationStatePending:
				live++
			}
		}
		if live == 0 {
			unmet = append(unmet, fmt.Sprintf(
				"route server %s propagates to no route table; BGP will establish and advertise, but the learned routes never reach a VPC route table, so nothing in the VPC can reach a CUDN pod (aws ec2 enable-route-server-propagation)",
				rsID))
		}
	}

	return unmet, nil
}
