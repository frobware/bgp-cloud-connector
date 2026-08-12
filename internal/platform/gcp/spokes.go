package gcp

import (
	"context"
	"sort"
	"strconv"
)

// chunkRouterNodes splits nodes into groups of at most maxPer, sorted by name
// so a node keeps the same spoke across reconciles.
func chunkRouterNodes(nodes []RouterNode, maxPer int) [][]RouterNode {
	if maxPer <= 0 {
		maxPer = 1
	}
	sorted := append([]RouterNode(nil), nodes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var out [][]RouterNode
	for i := 0; i < len(sorted); i += maxPer {
		end := i + maxPer
		if end > len(sorted) {
			end = len(sorted)
		}
		out = append(out, sorted[i:end])
	}
	return out
}

// reconcileSpokes creates or updates the numbered spokes needed to hold the
// router nodes and deletes any the node set no longer justifies.
func (p *Platform) reconcileSpokes(ctx context.Context, nodes []RouterNode) (int, error) {
	chunks := chunkRouterNodes(nodes, MaxInstancesPerSpoke)

	desired := make(map[string]struct{}, len(chunks))
	changes := 0
	for i, chunk := range chunks {
		id := p.cfg.NCCSpokePrefix + "-" + strconv.Itoa(i)
		desired[id] = struct{}{}
		changed, err := p.ncc.ReconcileSpoke(ctx, id, p.cfg.NCCHubName, chunk, p.cfg.SiteToSite)
		if err != nil {
			return changes, err
		}
		if changed {
			changes++
		}
	}

	existing, err := p.ncc.ListSpokesByPrefix(ctx, p.cfg.NCCHubName, p.cfg.NCCSpokePrefix)
	if err != nil {
		return changes, err
	}
	for _, id := range existing {
		if _, keep := desired[id]; keep {
			continue
		}
		deleted, err := p.ncc.DeleteSpoke(ctx, id)
		if err != nil {
			return changes, err
		}
		if deleted {
			changes++
		}
	}
	return changes, nil
}
