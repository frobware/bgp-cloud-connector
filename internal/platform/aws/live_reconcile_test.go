//go:build awslive

// A full reconcile against real EC2: route server peers for the router nodes,
// and SourceDestCheck cleared on the instances carrying them. This is the half
// the operator owns; the route server and its endpoints are prerequisites.
//
//	KUBECONFIG=<cluster>/auth/kubeconfig \
//	AWS_LIVE_REGION=us-east-2 \
//	AWS_LIVE_ROUTE_SERVER_IDS=rs-0abc \
//	AWS_LIVE_CLUSTER_ID=<infra> \
//	go test -tags awslive ./internal/platform/aws/ -run TestAWSLive_ReconcileNodes -v
//
// This creates real cloud resources. Cleanup is opt-in via AWS_LIVE_CLEANUP=1
// so the state can be inspected, and so a run can be repeated to prove
// reconciliation is idempotent.
package aws

import (
	"context"
	"os"
	"sort"
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
// Zone matters here in a way it does not on the other two clouds: a route
// server endpoint lives in one subnet, so a node peers only with the endpoints
// in its own availability zone.
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

// wantPeerCount is what the AWS shape implies: a node peers with every
// endpoint in its own zone and none in any other, so the total is summed per
// zone rather than multiplied across the cluster. That is the arithmetic GCP
// and Azure do not have, because their nodes all peer with the same addresses.
func wantPeerCount(nodes []platform.RouterNode, endpointsByAZ map[string][]string) int {
	total := 0
	for az, endpoints := range endpointsByAZ {
		for _, n := range nodes {
			if n.Zone == az {
				total += len(endpoints)
			}
		}
	}
	return total
}

// TestAWSLive_ReconcileNodes drives the operator's half against real EC2 and
// checks the result by reading the cloud back.
func TestAWSLive_ReconcileNodes(t *testing.T) {
	cfg := liveConfig(t)
	if cfg.ClusterID == "" {
		t.Skip("AWS_LIVE_CLUSTER_ID must be set: it is the ownership tag reconcile writes")
	}
	ctx := context.Background()
	logToTest(t)

	nodes := liveRouterNodes(t)
	step(t, "router nodes: %d", len(nodes))
	for _, n := range nodes {
		observed(t, "%s %s %s", n.Name, n.Zone, n.PrivateIP)
	}

	p := livePlatform(t, cfg)

	// Discovery populates the endpoint map reconcile peers against, so it is a
	// precondition rather than a separate check here.
	if _, err := p.DiscoverEndpoints(ctx); err != nil {
		t.Fatalf("DiscoverEndpoints: %v", err)
	}

	step(t, "reconciling peers and source/dest check for %d node(s)", len(nodes))
	if err := p.ReconcileNodes(ctx, nodes); err != nil {
		t.Fatalf("ReconcileNodes: %v", err)
	}

	want := wantPeerCount(nodes, p.endpointsByAZ)
	step(t, "checking the route server carries %d managed peer(s), summed per zone", want)
	got := liveManagedPeerIPs(t, p)
	observed(t, "%d managed peer(s) across %d zone(s)", len(got), len(p.endpointsByAZ))
	if len(got) != want {
		t.Errorf("expected %d managed peers, got %d", want, len(got))
	}

	// Naming which node each peer is for, rather than counting them, is what
	// catches a node being peered into the wrong zone -- the count stays right
	// while the session can never establish, because the endpoint is in a
	// subnet the node has no route to.
	step(t, "checking each node is peered with every endpoint in its own zone, and no other")
	for _, n := range nodes {
		for az, endpoints := range p.endpointsByAZ {
			for _, endpointID := range endpoints {
				_, peered := got[peerKey{endpointID, n.PrivateIP}]
				switch {
				case az == n.Zone && !peered:
					t.Errorf("node %s (%s) is not peered with endpoint %s in its own zone", n.Name, n.Zone, endpointID)
				case az != n.Zone && peered:
					t.Errorf("node %s (%s) is peered with endpoint %s in %s, which it cannot reach", n.Name, n.Zone, endpointID, az)
				case az == n.Zone:
					observed(t, "%s -> %s (%s)", n.Name, endpointID, az)
				}
			}
		}
	}

	step(t, "checking SourceDestCheck is off on every router node, without which pods are unreachable")
	for _, n := range nodes {
		instanceID, _, err := ParseProviderID(n.ProviderID)
		if err != nil {
			t.Errorf("%s: %v", n.Name, err)
			continue
		}
		eniID, enabled, err := p.getPrimaryENI(ctx, instanceID)
		if err != nil {
			t.Errorf("%s: reading primary ENI: %v", n.Name, err)
			continue
		}
		if enabled {
			t.Errorf("node %s still has SourceDestCheck enabled on %s", n.Name, eniID)
			continue
		}
		observed(t, "%s %s SourceDestCheck=false", n.Name, eniID)
	}

	// A second pass must not churn the estate, which is what stops the
	// operator fighting itself every resync. ReconcileNodes reports only an
	// error, so convergence is read from the cloud rather than a return value.
	step(t, "checking a second pass creates and deletes nothing")
	if err := p.ReconcileNodes(ctx, nodes); err != nil {
		t.Fatalf("second ReconcileNodes: %v", err)
	}
	after := liveManagedPeerIPs(t, p)
	if len(after) != len(got) {
		t.Errorf("peer count moved across a second reconcile: %d then %d", len(got), len(after))
	}
	for k, id := range got {
		if after[k] != id {
			t.Errorf("peer for %s on %s was replaced: %s then %s", k.peerIP, k.endpointID, id, after[k])
		}
	}
	observed(t, "%d peer(s) unchanged, same ids", len(after))

	if os.Getenv("AWS_LIVE_CLEANUP") == "1" {
		step(t, "cleaning up")
		if err := p.Cleanup(ctx); err != nil {
			t.Errorf("Cleanup: %v", err)
		}
		if remaining := liveManagedPeerIPs(t, p); len(remaining) != 0 {
			t.Errorf("expected no managed peers after cleanup, got %d", len(remaining))
		}
	}
}

// peerKey identifies a peering by the endpoint it sits on and the node address
// it points at, which is the pair reconcile treats as unique.
type peerKey struct {
	endpointID string
	peerIP     string
}

// liveManagedPeerIPs reads back only the peers carrying this operator's tag,
// keyed by endpoint and node address, with the EC2 peer id as the value so a
// second read can tell a surviving peer from a recreated one.
func liveManagedPeerIPs(t *testing.T, p *Platform) map[peerKey]string {
	t.Helper()
	out := map[peerKey]string{}
	azs := make([]string, 0, len(p.endpointsByAZ))
	for az := range p.endpointsByAZ {
		azs = append(azs, az)
	}
	sort.Strings(azs)
	for _, az := range azs {
		for _, endpointID := range p.endpointsByAZ[az] {
			peers, err := p.listManagedPeers(context.Background(), endpointID)
			if err != nil {
				t.Fatalf("listing managed peers on %s: %v", endpointID, err)
			}
			for _, peer := range peers {
				if peer.PeerAddress == nil || peer.RouteServerPeerId == nil {
					continue
				}
				out[peerKey{endpointID, *peer.PeerAddress}] = *peer.RouteServerPeerId
			}
		}
	}
	return out
}
