package gcp

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/api/googleapi"
	"google.golang.org/api/networkconnectivity/v1"
	"google.golang.org/api/option"
)

// MaxInstancesPerSpoke is the GCP limit on router appliance instances attached
// to a single NCC spoke, so a larger router set is split across numbered
// spokes.
const MaxInstancesPerSpoke = 8

// NewNCCClient builds an NCCClient using Application Default Credentials.
func NewNCCClient(ctx context.Context, project, region string) (NCCClient, error) {
	svc, err := networkconnectivity.NewService(ctx, option.WithScopes(networkconnectivity.CloudPlatformScope))
	if err != nil {
		return nil, err
	}
	return &nccClient{svc: svc, project: project, region: region}, nil
}

type nccClient struct {
	svc     *networkconnectivity.Service
	project string
	region  string
}

func (n *nccClient) parent() string {
	return fmt.Sprintf("projects/%s/locations/%s", n.project, n.region)
}

func (n *nccClient) spokePath(spokeID string) string {
	return fmt.Sprintf("%s/spokes/%s", n.parent(), spokeID)
}

// hubPath accepts either a bare hub name or an already qualified resource path.
func hubPath(project, hubName string) string {
	if strings.HasPrefix(hubName, "projects/") {
		return hubName
	}
	return fmt.Sprintf("projects/%s/locations/global/hubs/%s", project, hubName)
}

func (n *nccClient) ReconcileSpoke(ctx context.Context, spokeID, hubName string, nodes []RouterNode, siteToSite bool) (bool, error) {
	name := n.spokePath(spokeID)
	hub := hubPath(n.project, hubName)

	spoke, err := n.svc.Projects.Locations.Spokes.Get(name).Context(ctx).Do()
	if err != nil {
		if isNotFound(err) {
			return n.createSpoke(ctx, spokeID, hub, nodes, siteToSite)
		}
		return false, err
	}

	if spokeMatches(spoke, nodes) {
		return false, nil
	}

	spoke.LinkedRouterApplianceInstances = &networkconnectivity.LinkedRouterApplianceInstances{
		SiteToSiteDataTransfer: siteToSite,
		Instances:              applianceInstances(nodes),
	}
	op, err := n.svc.Projects.Locations.Spokes.Patch(name, spoke).
		UpdateMask("linkedRouterApplianceInstances.instances").
		Context(ctx).
		Do()
	if err != nil {
		return false, err
	}
	if err := n.waitLRO(ctx, op); err != nil {
		return false, err
	}
	return true, nil
}

// spokeMatches reports whether the spoke already links exactly this instance
// set, so an unchanged reconcile makes no write.
func spokeMatches(spoke *networkconnectivity.Spoke, nodes []RouterNode) bool {
	current := map[string]struct{}{}
	if spoke.LinkedRouterApplianceInstances != nil {
		for _, inst := range spoke.LinkedRouterApplianceInstances.Instances {
			if inst != nil && inst.VirtualMachine != "" {
				current[inst.VirtualMachine] = struct{}{}
			}
		}
	}
	if len(current) != len(nodes) {
		return false
	}
	for _, node := range nodes {
		if _, ok := current[node.SelfLink]; !ok {
			return false
		}
	}
	return true
}

func (n *nccClient) createSpoke(ctx context.Context, spokeID, hub string, nodes []RouterNode, siteToSite bool) (bool, error) {
	spoke := &networkconnectivity.Spoke{
		Hub:         hub,
		Description: "Router appliance spoke for CUDN BGP routing (managed by bgp-cloud-connector)",
		LinkedRouterApplianceInstances: &networkconnectivity.LinkedRouterApplianceInstances{
			SiteToSiteDataTransfer: siteToSite,
			Instances:              applianceInstances(nodes),
		},
	}
	op, err := n.svc.Projects.Locations.Spokes.Create(n.parent(), spoke).
		SpokeId(spokeID).
		Context(ctx).
		Do()
	if err != nil {
		return false, err
	}
	if err := n.waitLRO(ctx, op); err != nil {
		return false, err
	}
	return true, nil
}

func applianceInstances(nodes []RouterNode) []*networkconnectivity.RouterApplianceInstance {
	sorted := append([]RouterNode(nil), nodes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	out := make([]*networkconnectivity.RouterApplianceInstance, 0, len(sorted))
	for i := range sorted {
		out = append(out, &networkconnectivity.RouterApplianceInstance{
			VirtualMachine: sorted[i].SelfLink,
			IpAddress:      sorted[i].IPAddress,
		})
	}
	return out
}

func (n *nccClient) DeleteSpoke(ctx context.Context, spokeID string) (bool, error) {
	op, err := n.svc.Projects.Locations.Spokes.Delete(n.spokePath(spokeID)).Context(ctx).Do()
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if err := n.waitLRO(ctx, op); err != nil {
		return false, err
	}
	return true, nil
}

// ListSpokesByPrefix returns the numbered spokes this operator manages on the
// given hub, ignoring anything whose suffix is not an index.
func (n *nccClient) ListSpokesByPrefix(ctx context.Context, hubName, prefix string) ([]string, error) {
	wantHub := hubPath(n.project, hubName)
	prefixDash := prefix + "-"

	var ids []string
	pageToken := ""
	for {
		call := n.svc.Projects.Locations.Spokes.List(n.parent()).Context(ctx)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		resp, err := call.Do()
		if err != nil {
			return nil, err
		}
		for _, s := range resp.Spokes {
			if s == nil || s.Hub != wantHub {
				continue
			}
			spokeID := s.Name[strings.LastIndex(s.Name, "/")+1:]
			if !strings.HasPrefix(spokeID, prefixDash) {
				continue
			}
			if _, err := strconv.Atoi(spokeID[len(prefixDash):]); err == nil {
				ids = append(ids, spokeID)
			}
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	sort.Strings(ids)
	return ids, nil
}

func (n *nccClient) waitLRO(ctx context.Context, op *networkconnectivity.GoogleLongrunningOperation) error {
	if op == nil || op.Name == "" {
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
		cur, err := n.svc.Projects.Locations.Operations.Get(op.Name).Context(ctx).Do()
		if err != nil {
			return err
		}
		if cur.Done {
			if cur.Error != nil {
				return fmt.Errorf("spoke operation %s failed: code=%d message=%s", op.Name, cur.Error.Code, cur.Error.Message)
			}
			return nil
		}
	}
}

func isNotFound(err error) bool {
	ge, ok := err.(*googleapi.Error)
	return ok && ge.Code == 404
}
