# Integrating GCP into bgp-cloud-connector

## Scope

The operator supports AWS, GCP and Manual. Azure is to follow. This records how
the GCP CUDN BGP operator in
[`rh-mobb/osd-gcp-cudn-routing`](https://github.com/rh-mobb/osd-gcp-cudn-routing)
became the second platform here, what the abstraction it landed behind commits
us to, and what is still open.

The test of every decision below was not "does this fit AWS and GCP" but "does
this still hold when a third cloud arrives" -- an abstraction fitted to exactly
two clouds usually encodes the differences between those two rather than the
shape of the problem.

The API is `v1alpha1` with no consumers to protect, so breaking changes that
leave it in a better place were taken rather than deferred.

Stages 1 to 4 of the plan below are implemented and have run against a live
GCP cluster. Stage 5, e2e, is not started; [docs/e2e-plan.md](e2e-plan.md) is
the plan for it and supersedes the sketch here.

## Direction

GCP moved into this repository rather than the reverse. This repository is the
productisation path: `openshift.io` API domain, `networking` group, Konflux
build pipeline, operator bundle, `.ci-operator.yaml`, Tekton. The rh-mobb
repository describes itself as experimental and explicitly not a supported
product baseline.

The work was therefore an absorption, not a merge of equals. This repository
already had the platform abstraction and a cloud-neutral controller half; the
GCP operator had neither, calling `internal/gcp/` directly from
`internal/reconciler/`. GCP was the side that reshaped.

[`GUIDERAILS.md`](../GUIDERAILS.md) records the rules that merge produced,
each with the mistake that earned it. Read it before generalising anything
here.

## What mapped

The two operators solved the same problem and, independently, arrived at
substantially the same decomposition.

| Concern | Here | GCP operator |
| --- | --- | --- |
| Patch the Network operator | `internal/controller/network_operator.go` | equivalent inline |
| CUDN and RouteAdvertisements | `internal/controller/{cudn,routeadvertisements}.go` | `configure-routing.sh` (out of operator) |
| FRRConfiguration generation | `internal/controller/frr.go` | `internal/frr/builder.go` |
| Router node discovery | `listRouterNodes` | `internal/reconciler/nodes.go` |
| Router node labelling | `internal/controller/router_labels.go` | `SyncRouterLabels` |
| Machine `preTerminate` hooks | `internal/platform/gcp/machines.go` | `internal/reconciler/machines.go` |
| Per-node cloud attribute | source/dest check | `canIpForward` |
| Cloud peering reconcile | Route Server peers | NCC spokes plus Cloud Router peers |

## What was decided

### The peering plan replaced the AWS vocabulary

`DiscoveryResult` was expressed in `RouteServers`, `NeighborsByAZ` and
`EndpointsByAZ`. GCP has no route servers and no availability-zone axis for
BGP: it has an NCC hub, per-instance spokes, and a Cloud Router whose interface
addresses are the neighbours. Every router node peers with the same set.

A platform now returns `[]PeerGroup` -- a set of nodes and the neighbours they
share -- and the controller renders one `FRRConfiguration` per group. AWS emits
one group per AZ, selecting on `topology.kubernetes.io/zone`. GCP emits one
group covering every router node. The old fields survive as AWS-private detail
feeding `status.aws`; no other cloud populates them.

**One peer group on GCP, not one per node.** Driving GCP through the explicit
`spec.bgp.peerGroups` path by hand produced exactly one
`FRRConfiguration` and the sessions came up. A GCP subnet is regional, there is
one Cloud Router for the region, and every node peers with the same interface
addresses, so the zone axis had nothing to partition. The per-node
configurations the rh-mobb operator wrote were defensive rather than required.

GCP also emits a `spec.raw` block at priority 20 carrying `neighbor <ip>
disable-connected-check`, needed because the GCP worker carries a `/32` on
`br-ex`. AWS needs no such block. It rides on `PeerGroup.RawFRRConfig`: it is
cloud-specific, but it is FRR configuration, so it belongs in the peering plan
rather than behind a platform method.

### An explicit discriminator, not a field per cloud

`spec.platform` is a required enum (`AWS`, `GCP`, `Manual`) and the cloud block
sits beside it, with one CEL rule per cloud tying the two together and two more
governing `spec.bgp.peerGroups`. The choice is legible in the object and
in `kubectl get`; adding a cloud is one enum value plus one block.

Enum values are added only alongside a working implementation. Accepting
`platform: Azure` before an Azure platform exists would turn an admission
rejection into a runtime error, which is strictly worse.

The conditions are `CloudEndpointsDiscovered` and `CloudResourcesReconciled`.
Renaming them without changing the selection mechanism would have left the API
half cloud-neutral, which is worse than either end state.

`status.aws` remains a cloud-specific status block with no GCP sibling; GCP
currently writes no platform status at all. Three optional status blocks would
be a smell, and the question of how much of that detail is genuinely
cloud-shaped is still open.

### The operator maintains the router node labels, opt-in

Two ownership models met here: AWS took whatever matched
`spec.routerNodeSelector`, GCP chose candidates itself and applied a label to
the winners. They are different models, not different spellings of one.

`spec.autoLabelRouterNodes` settles it without forcing either side. Set it, and
the operator maintains the labels: `eligible` selects, `exclude` drops nodes
carrying a key whatever its value, and labels are pruned from nodes that fall
out. Leave it unset, and the operator only ever reads Node objects, which is
right where the labels are already managed deliberately. It runs as Phase 3,
before discovery, because everything downstream selects on those labels.

The reason it matters is churn: a node added by scaling a MachineSet is
silently not a router, because the MachineSet template carries no such label.

### Cloud prerequisites are reported, not created

`CheckPrerequisites` is the fourth `CloudPlatform` method. It reports, read-only
and one line per unmet requirement, the cloud configuration the operator relies
on and deliberately does not create. The operator keeps reconciling either way,
so that fixing one is enough, but will not claim Ready while the path cannot
work.

It exists because the sharpest failure observed here was silent: every Route
Server peer available, every BGP session established, FRR advertising the CUDN
prefix, and no route table with propagation enabled, so nothing in the VPC
could reach a pod while every signal the operator produced said healthy. AWS
checks propagation; GCP checks that the Cloud Router has interfaces and an ASN,
and that an ingress rule allows TCP 179.

### `NodeLifecycle` is optional and type-asserted

GCP holds a `preTerminate` hook on each router node's Machine until the node's
BGP peers have been withdrawn, then releases it. Without it, deleting a node
races peer withdrawal: the instance disappears while the Cloud Router still has
a peer pointing at it.

`CloudPlatform` did not grow a method for this. `NodeLifecycle`
(`HoldTerminating` / `ReleaseTerminating`) is a separate interface the
controller type-asserts, so a platform whose cloud tolerates a peer outliving
its instance simply does not implement it and the controller skips the hold.
AWS does not implement it.

Whether AWS needs it is unestablished. It depends on how Route Server handles a
peer whose ENI has gone, which is worth measuring rather than assuming.

## Shape

```
internal/platform/
    platform.go          RouterNode, DiscoveredNeighbor, PeerGroup,
                         DiscoveryResult, CloudPlatform, NodeLifecycle,
                         CredentialError
    aws/                 one PeerGroup per AZ, Route Server peers,
                         source/dest check, propagation prerequisite
    gcp/                 Cloud Router topology, NCC spokes, canIpForward,
                         nested virtualisation, Machine lifecycle hooks,
                         firewall and router prerequisites
internal/controller/     cloud-neutral; frr.go consumes PeerGroup,
                         router_labels.go maintains the node labels
```

## Still open

**`preTerminate` sits in the wrong layer.** `internal/platform/gcp/machines.go`
imports no Google SDK -- only apimachinery, controller-runtime and
`machine.openshift.io/v1beta1` -- and keys on `spec.providerID` as an opaque
string. It is an OpenShift concern filed under a cloud. By
[`GUIDERAILS.md`](../GUIDERAILS.md) rule 6 it belongs in the controller, where
every platform would get it; as it stands Azure would have to reimplement
Machine handling inside its own cloud package. Node labelling made that move;
this has not. Rule 6 also states that `NodeLifecycle` was deleted, which is not
the case.

**Per-node state has no home.** Here `CUDNBgpRouting` is user-facing input
declaring a CUDN to advertise; on GCP `BGPRouter` was operator-authored
per-node status: instance link, IP, `canIpForward`, nested virtualisation,
conditions. Neither subsumes the other. Nothing carries the per-node detail
today. Folding it into `CUDNBgpConfig.status` as a list avoids a third kind but
makes a cluster-scoped singleton's status grow with the node count; a per-node
CRD is easy to add later and hard to remove. Unresolved, and currently costing
nothing.

**Suspend.** GCP had `spec.suspended`, which cleaned up everything the operator
manages and paused reconciliation while keeping the configuration. There is no
equivalent here and none was added. It is cheap and genuinely useful for a
stack that reconciles cloud state, but it is new behaviour on the AWS side and
wants an explicit yes or no.

**Spoke sharding may conflict with Google's ASN rule.** An NCC spoke takes at
most 8 router appliance instances, so `chunkRouterNodes` shards across
`{prefix}-0`, `{prefix}-1` and so on. Google's ASN requirements state that
different spokes must use different ASNs, while every router node here
advertises one cluster-wide ASN from `spec.bgp.localASN`. Two readings: the
rule is scoped to site-to-site data transfer and does not bind, or it binds and
nothing has yet crossed 8 router nodes to find out. Nothing has run with more
than four. Treat 8 as the tested ceiling.

**Spoke naming carries no cluster identity.** NCC spokes are named
`<spokePrefix>-<N>` and carry only a free-text description. Spoke names are
unique per project and region, so two clusters in one project using the default
`cudn-bgp-spoke` collide. AWS Route Server peers carry
`managed-by: cudn-bgp-routing-operator/<clusterID>` and Cloud Router peers are
named with the cluster ID; the spokes were never given the same treatment.

**Questions for the GCP authors.** Two of the original five were answered by
measurement, which establishes what the code does but not what its authors
know. Outstanding:

1. Does anything outside the operator depend on the router node label being
   applied by the operator rather than by Terraform or a MachineSet?
2. What breaks in practice without the `preTerminate` hook? Was it added in
   response to an observed failure, and if so what was the symptom?
3. Is `spec.suspended` used operationally, or was it built for development?

## Known limits

**The operator must not be pointed at the installer's Cloud Router.** Every
OpenShift GCP cluster has an `<infra>-network-router` created to anchor Cloud
NAT, since a NAT gateway cannot exist without a router to hang off. It has no
ASN and no interfaces. Writing BGP peers onto it would entangle the operator
with the resource the cluster's egress depends on, and `Cleanup` issues a full
update against it. `DiscoverEndpoints` rejects a router with no interfaces,
which is exactly the shape of the NAT router, so the realistic misconfiguration
fails at discovery instead. That check is deliberate.

## References

- `internal/platform/platform.go`, `internal/platform/{aws,gcp}/`
- `internal/controller/cudnbgpconfig_controller.go`, `internal/controller/frr.go`
- `api/v1alpha1/cudnbgpconfig_types.go`
- [`GUIDERAILS.md`](../GUIDERAILS.md), [`docs/e2e-plan.md`](e2e-plan.md)
- rh-mobb `ROSA_KNOWLEDGE.md` for the GCP and AWS behavioural comparison
