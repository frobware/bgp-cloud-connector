//go:build azurelive

// A full reconcile against real Azure: enableIPForwarding on the router nodes'
// network interfaces, and BGP connections on the Route Server. This is the
// half the operator owns; the Route Server itself is a prerequisite.
//
//	KUBECONFIG=<cluster>/auth/kubeconfig \
//	AZURE_LIVE_SUBSCRIPTION_ID=... AZURE_LIVE_RESOURCE_GROUP=<infra>-rg \
//	AZURE_LIVE_ROUTE_SERVER=<infra>-rs AZURE_LIVE_CLUSTER_ID=<infra> \
//	go test -tags azurelive ./internal/platform/azure/ -run TestAzureLive_ReconcileNodes -v
//
// This creates real cloud resources, and slowly: a Route Server peering takes
// about two minutes to create or delete, and they are done one at a time.
// Budget several minutes per node. Cleanup is opt-in via AZURE_LIVE_CLEANUP=1
// so the state can be inspected, and so a run can be repeated to prove
// reconciliation is idempotent.
package azure

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

const routerNodeLabel = "networking.openshift.io/cudn-bgp-router"

// logToTest sends what the platform logs into the test's own output. Without a
// logger set, controller-runtime prints a stack trace saying so, in the middle
// of the narration these tests exist to produce.
func logToTest(t *testing.T) {
	t.Helper()
	logf.SetLogger(funcr.New(func(prefix, args string) {
		t.Logf("      %s %s", prefix, args)
	}, funcr.Options{}))
}

// liveRouterNodes reads the labelled nodes the same way the controller does.
func liveRouterNodes(t *testing.T) []platform.RouterNode {
	t.Helper()
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("KUBECONFIG must be set")
	}
	restCfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatalf("loading kubeconfig: %v", err)
	}
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	c, err := client.New(restCfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatalf("building core client: %v", err)
	}

	var nodes corev1.NodeList
	if err := c.List(context.Background(), &nodes, client.MatchingLabels{routerNodeLabel: ""}); err != nil {
		t.Fatalf("listing nodes: %v", err)
	}
	if len(nodes.Items) == 0 {
		t.Fatalf("no nodes carry %s; label the router nodes first", routerNodeLabel)
	}

	out := make([]platform.RouterNode, 0, len(nodes.Items))
	for i := range nodes.Items {
		n := &nodes.Items[i]
		rn := platform.RouterNode{
			Name:       n.Name,
			ProviderID: n.Spec.ProviderID,
			Zone:       n.Labels["topology.kubernetes.io/zone"],
		}
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeInternalIP {
				rn.PrivateIP = a.Address
				break
			}
		}
		out = append(out, rn)
	}
	return out
}

// TestAzureLive_ReconcileNodes drives the operator's half against real Azure
// and checks the result by reading the cloud back.
func TestAzureLive_ReconcileNodes(t *testing.T) {
	cfg := liveConfig(t)
	if cfg.ClusterID == "" {
		t.Skip("AZURE_LIVE_CLUSTER_ID must be set: it is the ownership prefix reconcile writes")
	}
	ctx := context.Background()
	logToTest(t)

	nodes := liveRouterNodes(t)
	step(t, "router nodes: %d", len(nodes))
	for _, n := range nodes {
		observed(t, "%s %s %s", n.Name, n.Zone, n.PrivateIP)
	}

	p := livePlatform(t, cfg)

	step(t, "reconciling forwarding and peerings for %d node(s); Azure peerings take about two minutes each", len(nodes))
	if err := p.ReconcileNodes(ctx, nodes); err != nil {
		t.Fatalf("ReconcileNodes: %v", err)
	}

	step(t, "checking a peering exists for every node, named for its address")
	peerings, err := p.rs.ListPeerings(ctx)
	if err != nil {
		t.Fatalf("ListPeerings: %v", err)
	}
	byName := make(map[string]Peering, len(peerings))
	ours := 0
	for _, pr := range peerings {
		byName[pr.Name] = pr
		if isOurPeering(pr.Name, cfg.ClusterID) {
			ours++
		}
	}
	observed(t, "%d of the Route Server's %d peering(s) carry our prefix", ours, len(peerings))
	if ours != len(nodes) {
		t.Errorf("expected %d peerings, one per node, got %d", len(nodes), ours)
	}

	// Naming which node each peering is for, rather than counting them, is
	// what catches a peering pointing at the wrong address. Azure keys on the
	// node address precisely because a BGP connection carries no tags and the
	// name is the only ownership signal there is.
	for _, n := range nodes {
		want := peeringName(cfg.ClusterID, n.PrivateIP)
		pr, found := byName[want]
		if !found {
			t.Errorf("no peering named %s, for node %s", want, n.Name)
			continue
		}
		if pr.PeerIP != n.PrivateIP {
			t.Errorf("peering %s points at %s, not node %s at %s", want, pr.PeerIP, n.Name, n.PrivateIP)
		}
		if pr.PeerASN != cfg.LocalASN {
			t.Errorf("peering %s expects ASN %d, want this cluster's %d", want, pr.PeerASN, cfg.LocalASN)
		}
		observed(t, "%s -> %s (%s)", n.Name, want, pr.ProvisioningState)
	}

	// The failure this exists to prevent: with forwarding off every session
	// still reaches Established, the Route Server still reports the pod prefix
	// learned via eBGP, every interface in the vnet still carries a route to
	// the router nodes, and every packet addressed to a pod is discarded.
	// Nothing anywhere says so.
	step(t, "checking enableIPForwarding is on for every router node's interface")
	for _, n := range nodes {
		rg, vmID, err := vmResourceID(n.ProviderID)
		if err != nil {
			t.Errorf("%s: %v", n.Name, err)
			continue
		}
		nics, err := p.nics.ListNICs(ctx, rg)
		if err != nil {
			t.Errorf("%s: listing NICs in %s: %v", n.Name, rg, err)
			continue
		}
		var found bool
		for _, nic := range nics {
			if nic.VMID == "" || !strings.EqualFold(nic.VMID, vmID) {
				continue
			}
			found = true
			if !nic.IPForwarding {
				t.Errorf("node %s interface %s has forwarding off; BGP would be healthy and pods unreachable",
					n.Name, nic.Name)
				continue
			}
			observed(t, "%s %s enableIPForwarding=true", n.Name, nic.Name)
		}
		if !found {
			t.Errorf("node %s: no interface in %s is attached to %s", n.Name, rg, vmID)
		}
	}

	// A second pass must write nothing. ReconcileNodes compares the peering
	// set before writing and returns early when it matches, so on Azure this
	// is the difference between a reconcile that costs nothing and one that
	// spends two minutes per node rewriting what was already right.
	step(t, "checking a second pass writes nothing")
	before := buildPeeringSet(ourPeerings(peerings, cfg.ClusterID))
	if err := p.ReconcileNodes(ctx, nodes); err != nil {
		t.Fatalf("second ReconcileNodes: %v", err)
	}
	after, err := p.rs.ListPeerings(ctx)
	if err != nil {
		t.Fatalf("second ListPeerings: %v", err)
	}
	if !before.Equal(buildPeeringSet(ourPeerings(after, cfg.ClusterID))) {
		t.Error("the peering set moved across a second reconcile; reconciliation is not converging")
	}
	observed(t, "%d peering(s) unchanged", ours)

	if os.Getenv("AZURE_LIVE_CLEANUP") == "1" {
		step(t, "cleaning up; this is about two minutes per peering")
		if err := p.Cleanup(ctx); err != nil {
			t.Errorf("Cleanup: %v", err)
		}
		remaining, err := p.rs.ListPeerings(ctx)
		if err != nil {
			t.Fatalf("listing peerings after cleanup: %v", err)
		}
		if n := len(ourPeerings(remaining, cfg.ClusterID)); n != 0 {
			t.Errorf("expected no peerings of ours after cleanup, got %d", n)
		}
	}
}

func ourPeerings(all []Peering, clusterID string) []Peering {
	var out []Peering
	for _, pr := range all {
		if isOurPeering(pr.Name, clusterID) {
			out = append(out, pr)
		}
	}
	return out
}
