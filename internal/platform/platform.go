package platform

import "context"

// RouterNode is a node the cloud will peer with.
type RouterNode struct {
	Name      string
	PrivateIP string
	// Zone is the node's failure domain. Only AWS partitions on it, because
	// its endpoints are per subnet; it is spelled out rather than abbreviated
	// to AZ because that abbreviation is one cloud's.
	Zone       string
	ProviderID string
}

// DiscoveredNeighbor is one address the router nodes peer with.
type DiscoveredNeighbor struct {
	Address string
	ASN     int64
}

// PeerGroup is a set of router nodes sharing a neighbour set. Each becomes one
// FRRConfiguration.
type PeerGroup struct {
	// Key identifies the group in the cloud's own terms, for diagnostics: an
	// availability zone on AWS, and whatever names the single regional
	// endpoint on a cloud that has one.
	Key string
	// NodeSelector narrows spec.routerNodeSelector to this group's nodes.
	// Empty means every router node, which is what a cloud with one group
	// emits.
	NodeSelector map[string]string
	Neighbors    []DiscoveredNeighbor
}

// DiscoveryResult is the peering plan a cloud arrived at.
//
// It carries nothing cloud-specific. It used to carry the route servers, their
// endpoints and two maps keyed by availability zone, which described AWS and
// nothing else: a cloud whose BGP endpoints are regional has no zones to key
// on and no route servers to list, and would have left all three empty while
// still having a perfectly good peering plan.
//
// The number of groups is where clouds differ. AWS emits one per zone, because
// its endpoints are per subnet and a node peers with the ones in its own.
type DiscoveryResult struct {
	PeerGroups []PeerGroup
}

// CloudPlatform is everything the controller needs from a cloud.
type CloudPlatform interface {
	DiscoverEndpoints(ctx context.Context) (*DiscoveryResult, error)
	ReconcileNodes(ctx context.Context, nodes []RouterNode) error
	Cleanup(ctx context.Context) error
}
