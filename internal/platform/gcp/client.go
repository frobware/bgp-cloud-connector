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
	// HasBGPFirewallRule reports whether an enabled ingress rule admits TCP
	// 179 from the given Cloud Router interface addresses.
	HasBGPFirewallRule(ctx context.Context, interfaceIPs []string) (bool, error)
	// GetPeerStatus reads the live BGP session state for a Cloud Router.
	GetPeerStatus(ctx context.Context, routerName string) ([]PeerStatus, error)
}

// PeerStatus is one Cloud Router BGP session as GCP reports it.
//
// There is no resource state here to go with the session state, and that is
// GCP's shape rather than an omission: a Cloud Router peer is a field inside
// the router resource, not a resource with a lifecycle of its own, so the
// only thing to observe is the session.
type PeerStatus struct {
	Name string
	// PeerIP is the node address the session is with.
	PeerIP string
	// State is the BGP state machine's state -- Established, Connect, Active
	// and so on. GCP also reports a Status of UP or DOWN, which is derived
	// from this and adds nothing, so it is not carried separately.
	State string
}

// NCCClient abstracts Network Connectivity Center spoke operations.
type NCCClient interface {
	ReconcileSpoke(ctx context.Context, spokeID, hubName string, nodes []RouterNode, siteToSite bool) (changed bool, err error)
	DeleteSpoke(ctx context.Context, spokeID string) (deleted bool, err error)
	ListSpokesByPrefix(ctx context.Context, hubName, prefix string) ([]string, error)
}
