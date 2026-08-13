package platform

import "context"

type RouterNode struct {
	Name       string
	PrivateIP  string
	Zone       string
	ProviderID string
}

type DiscoveredNeighbor struct {
	Address string
	ASN     int64
	// EBGPMultiHop allows the session to be established with a peer that is
	// not on the node's link. Azure Route Server needs it; AWS and GCP do
	// not. It is a structured field in frr-k8s, so it belongs on the
	// neighbour rather than in a peer group's RawFRRConfig -- that escape
	// hatch is for directives frr-k8s cannot express at all, such as GCP's
	// disable-connected-check.
	EBGPMultiHop bool
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

// DiscoveryResult is the peering plan a cloud reports. It carries nothing
// cloud-specific: it used to also carry RouteServers, NeighborsByAZ and
// EndpointsByAZ, which only AWS populated and only status.aws consumed, so a
// shared type described one cloud's shape and the other two left it empty.
type DiscoveryResult struct {
	// PeerGroups is what the controller renders into FRRConfigurations, and
	// what it reports in status.
	PeerGroups []PeerGroup
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

// CredentialError reports that a platform could not authenticate against its
// cloud. The controller surfaces it as a distinct condition reason so that a
// missing or expired credential is not reported as a discovery failure.
type CredentialError struct {
	Msg string
}

func (e *CredentialError) Error() string {
	return e.Msg
}
