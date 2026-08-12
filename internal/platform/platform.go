package platform

import "context"

type RouterNode struct {
	Name       string
	PrivateIP  string
	AZ         string
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

// PeerGroup is a set of router nodes sharing a BGP neighbour set. Each group
// becomes one FRRConfiguration. Clouds group differently: AWS peers per
// availability zone, so it emits one group per AZ; a cloud whose router
// addresses are identical for every node needs only a single group.
type PeerGroup struct {
	// Key identifies the group in cloud-meaningful terms (an AZ name, a node
	// name) and appears in diagnostics. Generated object names come from the
	// group's position in DiscoveryResult.PeerGroups, so a platform must emit
	// groups in a stable order.
	Key string
	// NodeSelector narrows spec.routerNodeSelector to this group's nodes. It
	// is merged over the base selector.
	NodeSelector map[string]string
	Neighbors    []DiscoveredNeighbor
	// RawFRRConfig, when non-empty, becomes spec.raw.rawConfig on the
	// generated FRRConfiguration. Some clouds need FRR directives the
	// structured neighbour API does not express.
	RawFRRConfig string
}

type DiscoveryResult struct {
	// PeerGroups is what the controller renders into FRRConfigurations.
	PeerGroups []PeerGroup

	// The fields below are AWS-shaped and feed status reporting only.
	RouteServers  []DiscoveredRouteServer
	NeighborsByAZ map[string][]DiscoveredNeighbor
	EndpointsByAZ map[string][]string
}

type CloudPlatform interface {
	DiscoverEndpoints(ctx context.Context) (*DiscoveryResult, error)
	ReconcileNodes(ctx context.Context, nodes []RouterNode) error
	Cleanup(ctx context.Context) error
	// CheckPrerequisites reports cloud configuration this operator relies on
	// but deliberately does not create, returning one human-readable line per
	// unmet requirement and an empty slice when everything is in place.
	//
	// The operator discovers the route server or Cloud Router; it never
	// provisions them, and that boundary is what keeps it an operator rather
	// than a replacement for the Terraform that builds the estate. The cost
	// of the boundary is that a missing prerequisite used to surface only as
	// sessions that never came up, or worse as BGP that worked perfectly
	// while nothing could reach a pod. Checking is read-only and lets the
	// operator say so instead.
	CheckPrerequisites(ctx context.Context) ([]string, error)
}

// NodeLifecycle is implemented by platforms that must withdraw cloud state
// before a router node's Machine is allowed to terminate. Platforms whose
// cloud tolerates a peer outliving its instance do not implement it, and the
// controller then skips the hold and release entirely.
type NodeLifecycle interface {
	// HoldTerminating reports which of nodes are terminating and have been
	// held. Held nodes are excluded from cloud reconciliation so that their
	// peers are torn down, and must later be passed to ReleaseTerminating.
	HoldTerminating(ctx context.Context, nodes []RouterNode) ([]RouterNode, error)
	// ReleaseTerminating releases the hold placed by HoldTerminating,
	// unblocking Machine deletion.
	ReleaseTerminating(ctx context.Context, held []RouterNode) error
}

// CredentialError reports that a platform could not authenticate against its
// cloud. The controller surfaces it as a distinct condition reason so that a
// missing or expired credential is not reported as a discovery failure.
type CredentialError struct {
	Msg string
}

func (e *CredentialError) Error() string {
	return e.Msg
}
