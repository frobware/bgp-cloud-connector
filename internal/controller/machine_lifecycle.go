package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

const (
	// LifecycleHookName gates instance deletion until the node's BGP peers
	// have been withdrawn from whichever cloud it is on.
	LifecycleHookName  = "networking.openshift.io/bgp-cleanup"
	LifecycleHookOwner = "CUDNBgpConfig"

	// MachineNamespace holds the OpenShift Machine objects. It is a constant
	// rather than a spec field because it is the same on every OpenShift
	// cluster, and where it is not -- a HyperShift guest cluster, whose
	// Machines live in the management cluster -- the Machine API is absent
	// here entirely and isMachineAPIAbsent already makes this a no-op.
	MachineNamespace = "openshift-machine-api"
)

var machineListGVK = schema.GroupVersionKind{
	Group:   "machine.openshift.io",
	Version: "v1beta1",
	Kind:    "MachineList",
}

var machineGVK = schema.GroupVersionKind{
	Group:   "machine.openshift.io",
	Version: "v1beta1",
	Kind:    "Machine",
}

// HoldTerminatingRouterNodes places a preTerminate hook on each router node's
// Machine and reports those already terminating. Held nodes are excluded from
// cloud reconciliation so their peers are withdrawn, and the hook is released
// only once that has happened. Without it, deleting a node races peer
// withdrawal: the instance disappears while the cloud still has a peer
// pointing at it.
//
// Machines that are no longer router nodes have the hook removed, so a node
// dropping out of the router set does not stay blocked forever.
//
// This is an OpenShift concern, not a cloud one. It keys on spec.providerID as
// an opaque string and touches no cloud SDK, which is why it lives here rather
// than behind the platform interface: the GCP and Azure operators this was
// assembled from each wrote their own copy of the same algorithm, and a third
// cloud should not have to write a fourth.
func HoldTerminatingRouterNodes(ctx context.Context, c client.Client, nodes []platform.RouterNode) ([]platform.RouterNode, error) {
	byProviderID := make(map[string]platform.RouterNode, len(nodes))
	for _, n := range nodes {
		byProviderID[n.ProviderID] = n
	}

	var list unstructured.UnstructuredList
	list.SetGroupVersionKind(machineListGVK)
	if err := c.List(ctx, &list, client.InNamespace(MachineNamespace)); err != nil {
		if apierrors.IsNotFound(err) || isMachineAPIAbsent(err) {
			// Not an OpenShift cluster, or the Machine API is not installed.
			// There is nothing to gate deletion on.
			return nil, nil
		}
		return nil, fmt.Errorf("listing machines in %q: %w", MachineNamespace, err)
	}

	var held []platform.RouterNode
	for i := range list.Items {
		m := &list.Items[i]

		providerID, found, err := unstructured.NestedString(m.Object, "spec", "providerID")
		if err != nil || !found || providerID == "" {
			continue
		}
		node, isRouter := byProviderID[providerID]
		hasHook := hasLifecycleHook(m)
		deleting := m.GetDeletionTimestamp() != nil

		switch {
		case isRouter && deleting && hasHook:
			held = append(held, node)
		case isRouter && !deleting && !hasHook:
			if err := setLifecycleHook(ctx, c, m, true); err != nil {
				return nil, fmt.Errorf("adding lifecycle hook to machine %q: %w", m.GetName(), err)
			}
		case !isRouter && hasHook:
			if err := setLifecycleHook(ctx, c, m, false); err != nil {
				return nil, fmt.Errorf("removing lifecycle hook from machine %q: %w", m.GetName(), err)
			}
		}
	}
	return held, nil
}

// ReleaseTerminatingRouterNodes drops the hook, letting the Machine controller
// destroy the instance.
func ReleaseTerminatingRouterNodes(ctx context.Context, c client.Client, held []platform.RouterNode) error {
	for _, node := range held {
		m, err := machineForProviderID(ctx, c, node.ProviderID)
		if err != nil {
			return err
		}
		if m == nil {
			continue
		}
		if err := setLifecycleHook(ctx, c, m, false); err != nil {
			return fmt.Errorf("releasing lifecycle hook on machine %q: %w", m.GetName(), err)
		}
	}
	return nil
}

func machineForProviderID(ctx context.Context, c client.Client, providerID string) (*unstructured.Unstructured, error) {
	var list unstructured.UnstructuredList
	list.SetGroupVersionKind(machineListGVK)
	if err := c.List(ctx, &list, client.InNamespace(MachineNamespace)); err != nil {
		if apierrors.IsNotFound(err) || isMachineAPIAbsent(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing machines in %q: %w", MachineNamespace, err)
	}
	for i := range list.Items {
		m := &list.Items[i]
		got, found, err := unstructured.NestedString(m.Object, "spec", "providerID")
		if err != nil || !found {
			continue
		}
		if got == providerID {
			return m, nil
		}
	}
	return nil, nil
}

func preTerminateHooks(m *unstructured.Unstructured) []interface{} {
	hooks, _, _ := unstructured.NestedMap(m.Object, "spec", "lifecycleHooks")
	if hooks == nil {
		return nil
	}
	pt, _ := hooks["preTerminate"].([]interface{})
	return pt
}

func hasLifecycleHook(m *unstructured.Unstructured) bool {
	for _, h := range preTerminateHooks(m) {
		hm, _ := h.(map[string]interface{})
		if hm["name"] == LifecycleHookName {
			return true
		}
	}
	return false
}

type hookEntry struct {
	Name  string `json:"name"`
	Owner string `json:"owner"`
}

type hookPatch struct {
	Spec struct {
		LifecycleHooks struct {
			PreTerminate []hookEntry `json:"preTerminate"`
		} `json:"lifecycleHooks"`
	} `json:"spec"`
}

// setLifecycleHook adds or removes our hook, leaving any other owner's hooks
// untouched. It is a no-op when the hook is already in the wanted state.
func setLifecycleHook(ctx context.Context, c client.Client, m *unstructured.Unstructured, want bool) error {
	var kept []hookEntry
	present := false
	for _, h := range preTerminateHooks(m) {
		hm, _ := h.(map[string]interface{})
		name, _ := hm["name"].(string)
		owner, _ := hm["owner"].(string)
		if name == LifecycleHookName {
			present = true
			continue
		}
		kept = append(kept, hookEntry{Name: name, Owner: owner})
	}
	if present == want {
		return nil
	}
	if want {
		kept = append(kept, hookEntry{Name: LifecycleHookName, Owner: LifecycleHookOwner})
	}

	var p hookPatch
	p.Spec.LifecycleHooks.PreTerminate = kept
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}

	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(machineGVK)
	target.SetName(m.GetName())
	target.SetNamespace(m.GetNamespace())
	return c.Patch(ctx, target, client.RawPatch(types.MergePatchType, data))
}

// isMachineAPIAbsent reports errors that mean machine.openshift.io is not
// registered, which is the normal case off OpenShift.
func isMachineAPIAbsent(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "no matches for kind") ||
		strings.Contains(msg, "no kind is registered") ||
		strings.Contains(msg, "the server could not find the requested resource")
}
