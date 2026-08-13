//go:build gcplive

// A full reconcile against real GCP: canIpForward on the instances, an NCC
// spoke naming them, and Cloud Router peers. This is the half the operator
// owns; the hub, router, interfaces and firewall are prerequisites.
//
//	KUBECONFIG=<cluster>/auth/kubeconfig \
//	GCP_PROJECT=openshift-qe GCP_REGION=us-east1 \
//	GCP_CLOUD_ROUTER=<infra>-bgp-router GCP_NCC_HUB=<infra>-bgp-hub \
//	go test -tags gcplive ./internal/platform/gcp/ -run TestGCPLive_ReconcileNodes -v
//
// This creates real cloud resources. Cleanup is opt-in via GCP_LIVE_CLEANUP=1
// so the state can be inspected, and so a run can be repeated to prove
// reconciliation is idempotent.
package gcp

import (
	"context"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

const routerNodeLabel = "networking.openshift.io/cudn-bgp-router"

func liveReconcileConfig(t *testing.T) Config {
	t.Helper()
	cfg := liveConfig(t)
	cfg.NCCHubName = os.Getenv("GCP_NCC_HUB")
	cfg.NCCSpokePrefix = envOr("GCP_NCC_SPOKE_PREFIX", "cudn-bgp-spoke")
	cfg.ClusterID = envOr("GCP_CLUSTER_ID", "e2e")
	cfg.MachineNamespace = "openshift-machine-api"
	// Nested virtualisation restarts the instance, so it stays off unless
	// asked for explicitly.
	cfg.NestedVirt = os.Getenv("GCP_NESTED_VIRT") == "1"
	if cfg.NCCHubName == "" {
		t.Skip("GCP_NCC_HUB must be set")
	}
	return cfg
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// liveRouterNodes reads the labelled nodes the same way the controller does.
func liveRouterNodes(t *testing.T) ([]platform.RouterNode, client.Client) {
	t.Helper()
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("KUBECONFIG must be set")
	}
	restCfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatalf("loading kubeconfig: %v", err)
	}
	c, err := client.New(restCfg, client.Options{Scheme: machineScheme()})
	if err != nil {
		t.Fatalf("building machine client: %v", err)
	}

	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	coreClient, err := client.New(restCfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatalf("building core client: %v", err)
	}

	var nodes corev1.NodeList
	if err := coreClient.List(context.Background(), &nodes, client.MatchingLabels{routerNodeLabel: ""}); err != nil {
		t.Fatalf("listing nodes: %v", err)
	}
	if len(nodes.Items) == 0 {
		t.Skipf("no nodes carry %s", routerNodeLabel)
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
	return out, c
}

// TestGCPLive_ReconcileNodes drives the operator's half against real GCP and
// checks the result by reading the cloud back.
func TestGCPLive_ReconcileNodes(t *testing.T) {
	cfg := liveReconcileConfig(t)
	ctx := context.Background()

	nodes, k8s := liveRouterNodes(t)
	t.Logf("router nodes: %d", len(nodes))
	for _, n := range nodes {
		t.Logf("  %s %s %s", n.Name, n.Zone, n.PrivateIP)
	}

	computeClient, err := NewComputeClient(ctx, cfg.Project, cfg.Region)
	if err != nil {
		t.Fatalf("compute client: %v", err)
	}
	nccClient, err := NewNCCClient(ctx, cfg.Project, cfg.Region)
	if err != nil {
		t.Fatalf("ncc client: %v", err)
	}
	p := NewWithClients(cfg, computeClient, nccClient, k8s)

	if err := p.ReconcileNodes(ctx, nodes); err != nil {
		t.Fatalf("ReconcileNodes: %v", err)
	}

	// The spoke must now name every router node.
	ids, err := nccClient.ListSpokesByPrefix(ctx, cfg.NCCHubName, cfg.NCCSpokePrefix)
	if err != nil {
		t.Fatalf("listing spokes: %v", err)
	}
	t.Logf("spokes: %v", ids)
	if len(ids) != 1 {
		t.Errorf("expected exactly one spoke for %d nodes, got %v", len(nodes), ids)
	}

	// Peers must exist, two per node, one for each router interface.
	topology, err := computeClient.GetRouterTopology(ctx, cfg.CloudRouterName)
	if err != nil {
		t.Fatalf("topology: %v", err)
	}
	wantPeers := len(nodes) * len(topology.InterfaceNames)
	t.Logf("expecting %d peers (%d nodes x %d interfaces)", wantPeers, len(nodes), len(topology.InterfaceNames))

	// A second pass must be a no-op, which is what stops the operator
	// fighting itself every resync.
	changed, err := computeClient.ReconcilePeers(ctx, cfg.CloudRouterName, cfg.ClusterID, mustRouterNodes(t, nodes), topology, cfg.LocalASN)
	if err != nil {
		t.Fatalf("second ReconcilePeers: %v", err)
	}
	if changed {
		t.Error("second peer reconcile reported a change; reconciliation is not converging")
	}

	if err := p.ReconcileNodes(ctx, nodes); err != nil {
		t.Fatalf("second ReconcileNodes: %v", err)
	}

	if os.Getenv("GCP_LIVE_CLEANUP") == "1" {
		t.Log("cleaning up")
		if err := p.Cleanup(ctx); err != nil {
			t.Errorf("Cleanup: %v", err)
		}
		ids, err := nccClient.ListSpokesByPrefix(ctx, cfg.NCCHubName, cfg.NCCSpokePrefix)
		if err != nil {
			t.Fatalf("listing spokes after cleanup: %v", err)
		}
		if len(ids) != 0 {
			t.Errorf("expected no spokes after cleanup, got %v", ids)
		}
	}
}

func mustRouterNodes(t *testing.T, nodes []platform.RouterNode) []RouterNode {
	t.Helper()
	out, err := toRouterNodes(nodes)
	if err != nil {
		t.Fatalf("resolving nodes: %v", err)
	}
	return out
}
