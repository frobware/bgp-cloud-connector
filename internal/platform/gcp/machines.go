package gcp

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
	// LifecycleHookName gates GCE instance deletion until the node's Cloud
	// Router peers have been withdrawn.
	LifecycleHookName  = "networking.openshift.io/bgp-cleanup"
	LifecycleHookOwner = "CUDNBgpConfig"
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

// HoldTerminating places a preTerminate hook on each router node's Machine and
// reports those already terminating. Held nodes are excluded from
// reconciliation so their peers are withdrawn, and the hook is released only
// once that has happened.
//
// Machines that are no longer router nodes have the hook removed, so a node
// dropping out of the router set does not stay blocked forever.
func (p *Platform) HoldTerminating(ctx context.Context, nodes []platform.RouterNode) ([]platform.RouterNode, error) {
	byProviderID := make(map[string]platform.RouterNode, len(nodes))
	for _, n := range nodes {
		byProviderID[n.ProviderID] = n
	}

	var list unstructured.UnstructuredList
	list.SetGroupVersionKind(machineListGVK)
	if err := p.k8s.List(ctx, &list, client.InNamespace(p.cfg.MachineNamespace)); err != nil {
		if apierrors.IsNotFound(err) || isMachineAPIAbsent(err) {
			// Not an OpenShift cluster, or the Machine API is not installed.
			// There is nothing to gate deletion on.
			return nil, nil
		}
		return nil, fmt.Errorf("listing machines in %q: %w", p.cfg.MachineNamespace, err)
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
			if err := setLifecycleHook(ctx, p.k8s, m, true); err != nil {
				return nil, fmt.Errorf("adding lifecycle hook to machine %q: %w", m.GetName(), err)
			}
		case !isRouter && hasHook:
			if err := setLifecycleHook(ctx, p.k8s, m, false); err != nil {
				return nil, fmt.Errorf("removing lifecycle hook from machine %q: %w", m.GetName(), err)
			}
		}
	}
	return held, nil
}

// ReleaseTerminating drops the hook, letting the Machine controller destroy
// the instance.
func (p *Platform) ReleaseTerminating(ctx context.Context, held []platform.RouterNode) error {
	for _, node := range held {
		m, err := p.machineForProviderID(ctx, node.ProviderID)
		if err != nil {
			return err
		}
		if m == nil {
			continue
		}
		if err := setLifecycleHook(ctx, p.k8s, m, false); err != nil {
			return fmt.Errorf("releasing lifecycle hook on machine %q: %w", m.GetName(), err)
		}
	}
	return nil
}

func (p *Platform) machineForProviderID(ctx context.Context, providerID string) (*unstructured.Unstructured, error) {
	var list unstructured.UnstructuredList
	list.SetGroupVersionKind(machineListGVK)
	if err := p.k8s.List(ctx, &list, client.InNamespace(p.cfg.MachineNamespace)); err != nil {
		if apierrors.IsNotFound(err) || isMachineAPIAbsent(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing machines in %q: %w", p.cfg.MachineNamespace, err)
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
