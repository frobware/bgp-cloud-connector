/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A worker can come up without the br-ex flows OVN-Kubernetes installs
// for an advertised network while still advertising the network over
// BGP, and then it drops traffic for pods on that network. Restarting
// the worker's ovnkube-node pod installs them. This looks like an
// upstream OVN-Kubernetes race, not something the operator controls, so
// the data-plane specs restart ovnkube-node once when their probe fails
// and report that they did.

const (
	ovnNamespace       = "openshift-ovn-kubernetes"
	ovnkubeNodeApp     = "ovnkube-node"
	ovnkubeNodeTimeout = 10 * time.Minute
)

// ProbeAllowingOVNKubeRestart runs check until it passes or timeout
// expires. If it never passes, it records each worker's br-ex flows,
// restarts ovnkube-node on every worker, and runs check again for up to
// timeout; only a failure then fails the spec. A restart that was needed
// is added as a report entry, which Ginkgo prints whether or not the
// spec passes.
func ProbeAllowingOVNKubeRestart(ctx context.Context, c client.Client, workers []string,
	timeout, polling time.Duration, check func(gomega.Gomega)) {
	first := gomega.InterceptGomegaFailure(func() {
		gomega.Eventually(check).WithTimeout(timeout).WithPolling(polling).Should(gomega.Succeed())
	})
	if first == nil {
		return
	}
	ginkgo.GinkgoWriter.Printf("probe failed before any ovnkube-node restart:\n%v\n", first)

	flows := dumpBrExFlows(ctx, c, workers, "br-ex-flows-before-ovnkube-restart")

	ginkgo.By("restarting ovnkube-node on every worker, then probing again")
	gomega.Expect(restartOVNKubeNode(ctx, c, workers)).To(gomega.Succeed())
	gomega.Eventually(check).WithTimeout(timeout).WithPolling(polling).Should(gomega.Succeed())

	ginkgo.AddReportEntry("ovnkube-node restart needed",
		fmt.Sprintf("the probe failed for %s and passed after ovnkube-node was restarted "+
			"on every worker; br-ex flows from before the restart are in %s", timeout, flows))
}

// restartOVNKubeNode deletes the ovnkube-node pod on each worker and
// waits until every worker has a ready replacement.
func restartOVNKubeNode(ctx context.Context, c client.Client, workers []string) error {
	old, err := ovnkubeNodePods(ctx, c)
	if err != nil {
		return err
	}
	for _, node := range workers {
		pod, ok := old[node]
		if !ok {
			return fmt.Errorf("no ovnkube-node pod on worker %s", node)
		}
		if err := c.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting %s on %s: %w", pod.Name, node, err)
		}
		ginkgo.GinkgoWriter.Printf("deleted %s on %s\n", pod.Name, node)
	}

	var waiting string
	err = wait.PollUntilContextTimeout(ctx, 10*time.Second, ovnkubeNodeTimeout, true,
		func(ctx context.Context) (bool, error) {
			current, err := ovnkubeNodePods(ctx, c)
			if err != nil {
				return false, err
			}
			for _, node := range workers {
				pod, ok := current[node]
				if !ok || pod.UID == old[node].UID || !podReady(pod) {
					waiting = node
					return false, nil
				}
			}
			return true, nil
		})
	if err != nil {
		return fmt.Errorf("waiting for a ready ovnkube-node pod on %s: %w", waiting, err)
	}
	return nil
}

// ovnkubeNodePods maps each node to its ovnkube-node pod, leaving out
// pods that are terminating.
func ovnkubeNodePods(ctx context.Context, c client.Client) (map[string]*corev1.Pod, error) {
	pods := &corev1.PodList{}
	if err := c.List(ctx, pods, client.InNamespace(ovnNamespace),
		client.MatchingLabels{"app": ovnkubeNodeApp}); err != nil {
		return nil, err
	}
	byNode := map[string]*corev1.Pod{}
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp != nil {
			continue
		}
		byNode[pods.Items[i].Spec.NodeName] = &pods.Items[i]
	}
	return byNode, nil
}

// dumpBrExFlows writes `ovs-ofctl dump-flows br-ex` for each worker to
// <dir>/<node>.txt, under ${ARTIFACT_DIR} when it is set and a temporary
// directory otherwise, and returns the directory. It goes through oc,
// as hack/report-br-ex-flows.sh does, and needs oc on PATH. It is
// diagnostics only: a worker that cannot be read is logged and skipped.
func dumpBrExFlows(ctx context.Context, c client.Client, workers []string, name string) string {
	dir := filepath.Join(os.Getenv("ARTIFACT_DIR"), name)
	if os.Getenv("ARTIFACT_DIR") == "" {
		tmp, err := os.MkdirTemp("", name+"-")
		if err != nil {
			ginkgo.GinkgoWriter.Printf("br-ex flows not recorded: %v\n", err)
			return ""
		}
		dir = tmp
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		ginkgo.GinkgoWriter.Printf("br-ex flows not recorded: %v\n", err)
		return ""
	}
	pods, err := ovnkubeNodePods(ctx, c)
	if err != nil {
		ginkgo.GinkgoWriter.Printf("br-ex flows not recorded: %v\n", err)
		return dir
	}
	for _, node := range workers {
		pod, ok := pods[node]
		if !ok {
			ginkgo.GinkgoWriter.Printf("br-ex flows on %s not recorded: no ovnkube-node pod\n", node)
			continue
		}
		out, err := exec.CommandContext(ctx, "oc", "-n", ovnNamespace, "exec", pod.Name,
			"-c", "ovn-controller", "--", "ovs-ofctl", "dump-flows", "br-ex").Output()
		if err != nil {
			ginkgo.GinkgoWriter.Printf("br-ex flows on %s not recorded: %v\n", node, err)
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, node+".txt"), out, 0o644); err != nil {
			ginkgo.GinkgoWriter.Printf("br-ex flows on %s not recorded: %v\n", node, err)
		}
	}
	ginkgo.GinkgoWriter.Printf("br-ex flows before the restart written to %s\n", dir)
	return dir
}
