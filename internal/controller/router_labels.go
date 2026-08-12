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

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkingv1alpha1 "github.com/openshift/bgp-cloud-connector/api/v1alpha1"
)

// Which nodes are BGP routers is a Kubernetes question rather than a cloud
// one, so it is answered here once for every platform rather than in each
// platform implementation.

// SyncRouterLabels brings spec.routerNodeSelector into agreement with
// spec.autoLabelRouterNodes: eligible nodes gain the labels, nodes that carry
// them but are no longer eligible lose them. It reports how many nodes it
// labelled and unlabelled.
//
// With auto-labelling unset it does nothing at all, so an installation that
// manages the labels itself never sees a write to a Node.
func SyncRouterLabels(ctx context.Context, c client.Client, config *networkingv1alpha1.CUDNBgpConfig) (added, removed int, err error) {
	auto := config.Spec.AutoLabelRouterNodes
	if auto == nil {
		return 0, 0, nil
	}

	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes); err != nil {
		return 0, 0, fmt.Errorf("listing nodes: %w", err)
	}

	for i := range nodes.Items {
		node := &nodes.Items[i]
		switch {
		case isEligibleRouterNode(node, auto):
			changed, err := setRouterLabels(ctx, c, node, config.Spec.RouterNodeSelector, true)
			if err != nil {
				return added, removed, fmt.Errorf("labelling node %q: %w", node.Name, err)
			}
			if changed {
				added++
			}
		case hasRouterLabels(node, config.Spec.RouterNodeSelector):
			changed, err := setRouterLabels(ctx, c, node, config.Spec.RouterNodeSelector, false)
			if err != nil {
				return added, removed, fmt.Errorf("unlabelling node %q: %w", node.Name, err)
			}
			if changed {
				removed++
			}
		}
	}
	return added, removed, nil
}

// RemoveAllRouterLabels strips the router labels from every node carrying
// them. Only labels the operator applied are its to remove, so this is a
// no-op unless auto-labelling is enabled.
func RemoveAllRouterLabels(ctx context.Context, c client.Client, config *networkingv1alpha1.CUDNBgpConfig) (int, error) {
	if config.Spec.AutoLabelRouterNodes == nil {
		return 0, nil
	}

	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes, client.MatchingLabelsSelector{
		Selector: labels.SelectorFromSet(config.Spec.RouterNodeSelector),
	}); err != nil {
		return 0, fmt.Errorf("listing router nodes: %w", err)
	}

	removed := 0
	for i := range nodes.Items {
		changed, err := setRouterLabels(ctx, c, &nodes.Items[i], config.Spec.RouterNodeSelector, false)
		if err != nil {
			return removed, fmt.Errorf("unlabelling node %q: %w", nodes.Items[i].Name, err)
		}
		if changed {
			removed++
		}
	}
	return removed, nil
}

// isEligibleRouterNode reports whether a node should carry the router labels:
// it matches every eligibility label and none of the exclusions. Exclusions
// match on key alone, because node roles are conventionally valueless labels.
func isEligibleRouterNode(node *corev1.Node, auto *networkingv1alpha1.AutoLabelRouterNodesSpec) bool {
	for k, v := range auto.Eligible {
		got, present := node.Labels[k]
		if !present || got != v {
			return false
		}
	}
	for k := range auto.Exclude {
		if _, present := node.Labels[k]; present {
			return false
		}
	}
	return true
}

func hasRouterLabels(node *corev1.Node, want map[string]string) bool {
	for k, v := range want {
		if got, present := node.Labels[k]; !present || got != v {
			return false
		}
	}
	return len(want) > 0
}

// setRouterLabels adds or removes the router labels, leaving every other
// label alone, and reports whether it wrote anything. Returning early on an
// already-correct node is what keeps a resync from churning every Node in the
// cluster.
func setRouterLabels(ctx context.Context, c client.Client, node *corev1.Node, routerLabels map[string]string, want bool) (bool, error) {
	if hasRouterLabels(node, routerLabels) == want {
		return false, nil
	}

	patch := client.MergeFrom(node.DeepCopy())
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	for k, v := range routerLabels {
		if want {
			node.Labels[k] = v
		} else {
			delete(node.Labels, k)
		}
	}
	return true, c.Patch(ctx, node, patch)
}
