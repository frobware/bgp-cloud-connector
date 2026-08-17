package platform

import "context"

type RouterNode struct {
	Name      string
	PrivateIP string
	// Zone is the node's failure domain. Only AWS partitions on it, because
	// its endpoints are per subnet; it is spelled out rather than abbreviated
	// to AZ because that abbreviation is one cloud's.
	Zone       string
	ProviderID string
}

type DiscoveredNeighbor struct {
	Address string
	ASN     int64
}

type DiscoveredEndpoint struct {
	EndpointID string
	AZ         string
	Address    string
}

type DiscoveredRouteServer struct {
	RouteServerID string
	RemoteASN     int64
	Endpoints     []DiscoveredEndpoint
}

type DiscoveryResult struct {
	RouteServers  []DiscoveredRouteServer
	NeighborsByAZ map[string][]DiscoveredNeighbor
	EndpointsByAZ map[string][]string
}

type CloudPlatform interface {
	DiscoverEndpoints(ctx context.Context) (*DiscoveryResult, error)
	ReconcileNodes(ctx context.Context, nodes []RouterNode) error
	Cleanup(ctx context.Context) error
}
