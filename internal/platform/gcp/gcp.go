package gcp

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

// Config is everything the GCP platform needs that the controller cannot work
// out for itself.
type Config struct {
	Project         string
	Region          string
	CloudRouterName string
	NCCHubName      string
	NCCSpokePrefix  string
	SiteToSite      bool
	NestedVirt      bool
	// MachineNamespace holds the OpenShift Machine objects whose preTerminate
	// hooks gate instance deletion.
	MachineNamespace string
	// LocalASN is the ASN FRR runs with on the router nodes, which is what
	// Cloud Router peers are configured to expect.
	LocalASN int64
	// ClusterID names Cloud Router peers so two clusters sharing a router do
	// not collide.
	ClusterID string
}

// Platform reconciles GCP Cloud Router peering and NCC spokes for the CUDN
// BGP router nodes.
type Platform struct {
	cfg     Config
	compute ComputeClient
	ncc     NCCClient
	// k8s reads and patches Machine objects for the preTerminate hooks.
	k8s client.Client
}

// New builds a Platform against the live Google APIs.
func New(ctx context.Context, cfg Config, k8s client.Client) (*Platform, error) {
	computeClient, err := NewComputeClient(ctx, cfg.Project, cfg.Region)
	if err != nil {
		return nil, &platform.CredentialError{Msg: fmt.Sprintf("GCP compute client: %v", err)}
	}
	nccClient, err := NewNCCClient(ctx, cfg.Project, cfg.Region)
	if err != nil {
		return nil, &platform.CredentialError{Msg: fmt.Sprintf("GCP network connectivity client: %v", err)}
	}
	return NewWithClients(cfg, computeClient, nccClient, k8s), nil
}

// NewWithClients builds a Platform against supplied clients.
func NewWithClients(cfg Config, computeClient ComputeClient, nccClient NCCClient, k8s client.Client) *Platform {
	return &Platform{cfg: cfg, compute: computeClient, ncc: nccClient, k8s: k8s}
}

// DiscoverEndpoints reads the Cloud Router and returns a single peer group.
//
// Every router node peers with the same Cloud Router interface addresses, so
// unlike AWS there is no per-zone split and one FRRConfiguration covers the
// whole router set. The raw block carries disable-connected-check, which FRR
// needs because the worker carries a /32 on br-ex and would otherwise reject
// the neighbour as unreachable.
func (p *Platform) DiscoverEndpoints(ctx context.Context) (*platform.DiscoveryResult, error) {
	topology, err := p.compute.GetRouterTopology(ctx, p.cfg.CloudRouterName)
	if err != nil {
		return nil, fmt.Errorf("reading Cloud Router %q topology: %w", p.cfg.CloudRouterName, err)
	}
	if len(topology.InterfaceIPs) == 0 {
		return nil, fmt.Errorf("Cloud Router %q has no interfaces", p.cfg.CloudRouterName)
	}

	group := platform.PeerGroup{
		Key:          p.cfg.CloudRouterName,
		RawFRRConfig: rawFRRConfig(p.cfg.LocalASN, topology.InterfaceIPs),
	}
	for _, ip := range topology.InterfaceIPs {
		group.Neighbors = append(group.Neighbors, platform.DiscoveredNeighbor{
			Address: ip,
			ASN:     topology.ASN,
		})
	}

	return &platform.DiscoveryResult{PeerGroups: []platform.PeerGroup{group}}, nil
}

// rawFRRConfig renders the FRR directives the structured neighbour API cannot
// express.
func rawFRRConfig(localASN int64, interfaceIPs []string) string {
	lines := []string{"      router bgp " + strconv.FormatInt(localASN, 10)}
	for _, ip := range interfaceIPs {
		lines = append(lines, "       neighbor "+ip+" disable-connected-check")
	}
	return strings.Join(lines, "\n") + "\n"
}

// ReconcileNodes brings GCE instance attributes, NCC spokes and Cloud Router
// peers into line with the given router nodes.
func (p *Platform) ReconcileNodes(ctx context.Context, nodes []platform.RouterNode) error {
	logger := log.FromContext(ctx)

	routerNodes, err := toRouterNodes(nodes)
	if err != nil {
		return err
	}

	for _, node := range routerNodes {
		changed, err := p.compute.EnsureCanIPForward(ctx, node)
		if err != nil {
			return fmt.Errorf("enabling IP forwarding on %q: %w", node.Name, err)
		}
		if changed {
			logger.Info("enabled canIpForward", "instance", node.Name)
		}

		if p.cfg.NestedVirt {
			changed, err := p.compute.EnsureNestedVirtualization(ctx, node)
			if err != nil {
				return fmt.Errorf("enabling nested virtualisation on %q: %w", node.Name, err)
			}
			if changed {
				logger.Info("enabled nested virtualisation", "instance", node.Name)
			}
		}
	}

	spokeChanges, err := p.reconcileSpokes(ctx, routerNodes)
	if err != nil {
		return fmt.Errorf("reconciling NCC spokes: %w", err)
	}
	if spokeChanges > 0 {
		logger.Info("NCC spokes updated", "changes", spokeChanges)
	}

	topology, err := p.compute.GetRouterTopology(ctx, p.cfg.CloudRouterName)
	if err != nil {
		return fmt.Errorf("reading Cloud Router %q topology: %w", p.cfg.CloudRouterName, err)
	}
	changed, err := p.compute.ReconcilePeers(ctx, p.cfg.CloudRouterName, p.cfg.ClusterID, routerNodes, topology, p.cfg.LocalASN)
	if err != nil {
		return fmt.Errorf("reconciling Cloud Router peers: %w", err)
	}
	if changed {
		logger.Info("Cloud Router peers updated", "router", p.cfg.CloudRouterName, "nodes", len(routerNodes))
	}
	return nil
}

// Cleanup removes every resource this platform manages.
func (p *Platform) Cleanup(ctx context.Context) error {
	if _, err := p.compute.ClearPeers(ctx, p.cfg.CloudRouterName); err != nil {
		return fmt.Errorf("clearing Cloud Router peers: %w", err)
	}
	ids, err := p.ncc.ListSpokesByPrefix(ctx, p.cfg.NCCHubName, p.cfg.NCCSpokePrefix)
	if err != nil {
		return fmt.Errorf("listing NCC spokes: %w", err)
	}
	for _, id := range ids {
		if _, err := p.ncc.DeleteSpoke(ctx, id); err != nil {
			return fmt.Errorf("deleting NCC spoke %q: %w", id, err)
		}
	}
	return nil
}

// toRouterNodes resolves the cloud-neutral node list into GCE identities.
func toRouterNodes(nodes []platform.RouterNode) ([]RouterNode, error) {
	out := make([]RouterNode, 0, len(nodes))
	for _, n := range nodes {
		inst, err := ParseProviderID(n.ProviderID)
		if err != nil {
			return nil, fmt.Errorf("node %q: %w", n.Name, err)
		}
		out = append(out, RouterNode{
			Name:      inst.Name,
			SelfLink:  inst.SelfLink,
			Zone:      inst.Zone,
			IPAddress: n.PrivateIP,
		})
	}
	return out, nil
}
