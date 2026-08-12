package gcp

import "context"

// RouterNode is a GCE instance participating in BGP routing, resolved from a
// Kubernetes node.
type RouterNode struct {
	Name      string
	SelfLink  string
	Zone      string
	IPAddress string
}

// CloudRouterTopology is the Cloud Router's BGP identity and the interface
// addresses every router node peers with. Unlike AWS Route Server endpoints,
// these do not vary by zone, so all router nodes share one neighbour set.
type CloudRouterTopology struct {
	ASN            int64
	InterfaceNames []string
	InterfaceIPs   []string
}

// ComputeClient abstracts the GCE instance and Cloud Router calls the platform
// makes, so reconciliation can be tested without reaching Google.
type ComputeClient interface {
	EnsureCanIPForward(ctx context.Context, node RouterNode) (changed bool, err error)
	EnsureNestedVirtualization(ctx context.Context, node RouterNode) (changed bool, err error)
	GetRouterTopology(ctx context.Context, routerName string) (*CloudRouterTopology, error)
	ReconcilePeers(ctx context.Context, routerName, clusterID string, nodes []RouterNode, topology *CloudRouterTopology, localASN int64) (changed bool, err error)
	ClearPeers(ctx context.Context, routerName, clusterID string) (changed bool, err error)
}

// NCCClient abstracts Network Connectivity Center spoke operations.
type NCCClient interface {
	ReconcileSpoke(ctx context.Context, spokeID, hubName string, nodes []RouterNode, siteToSite bool) (changed bool, err error)
	DeleteSpoke(ctx context.Context, spokeID string) (deleted bool, err error)
	ListSpokesByPrefix(ctx context.Context, hubName, prefix string) ([]string, error)
}
