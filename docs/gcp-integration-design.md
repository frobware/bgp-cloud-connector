# Integrating GCP into bgp-cloud-connector

## Scope

The operator is to support AWS, Azure and GCP. Today it supports AWS. This
describes how the GCP CUDN BGP operator in
[`rh-mobb/osd-gcp-cudn-routing`](https://github.com/rh-mobb/osd-gcp-cudn-routing)
(under `operator/`) becomes the second platform here, and how the abstraction
it lands behind stays honest for the third.

GCP goes first because the code exists and has run in production. Azure
follows the same shape. The test of every decision below is not "does this fit
AWS and GCP" but "does this still hold when a third cloud arrives" -- an
abstraction fitted to exactly two clouds usually encodes the differences
between those two rather than the shape of the problem.

There are no backwards compatibility constraints. The API is `v1alpha1` with
no consumers to protect, so breaking changes that leave the API in a better
place should be taken now rather than deferred.

It is a design proposal, not a record of agreed decisions. Every numbered
decision below needs an answer before the code lands, and several of them are
only answerable by the people who ran the GCP deployment.

## Direction

GCP moves into this repository rather than the reverse. This repository is the
productisation path: `openshift.io` API domain, `networking` group, Konflux
build pipeline, operator bundle, `.ci-operator.yaml`, Tekton. The rh-mobb
repository describes itself as experimental and explicitly not a supported
product baseline. Nothing else about the two codebases needs weighing against
that.

The work is therefore an absorption, not a merge of equals. This repository
already has the platform abstraction (`internal/platform/platform.go`) and a
cloud-neutral controller half; the GCP operator has neither, calling
`internal/gcp/` directly from `internal/reconciler/`. GCP is the side that
reshapes.

## What already maps

The two operators solve the same problem and, independently, arrived at
substantially the same decomposition. The cloud-neutral work exists on both
sides:

| Concern | Here | GCP operator |
| --- | --- | --- |
| Patch the Network operator | `internal/controller/network_operator.go` | equivalent inline |
| CUDN and RouteAdvertisements | `internal/controller/{cudn,routeadvertisements}.go` | `configure-routing.sh` (out of operator) |
| FRRConfiguration generation | `internal/controller/frr.go` | `internal/frr/builder.go` |
| Router node discovery | `listRouterNodes` | `internal/reconciler/nodes.go` |
| Per-node cloud attribute | source/dest check | `canIpForward` |
| Cloud peering reconcile | Route Server peers | NCC spokes plus Cloud Router peers |

The three-method `CloudPlatform` contract survives the merge. GCP's
`GetRouterTopology` is its `DiscoverEndpoints`; its combination of
`EnsureCanIPForward`, `EnsureNestedVirtualization`, `ReconcileNCCSpokes` and
`ReconcilePeers` is its `ReconcileNodes`; its `ClearPeers` plus spoke deletion
is its `Cleanup`. The shape is right. The data flowing through it is not.

## Decisions

### 1. `DiscoveryResult` is AWS-shaped

`platform.DiscoveryResult` is expressed in terms of `RouteServers`,
`NeighborsByAZ` and `EndpointsByAZ`. GCP has no route servers and no
availability-zone axis for BGP: it has an NCC hub, a per-instance spoke, and a
Cloud Router whose interface addresses are the neighbours. Every router node
peers with the same set.

Proposal: replace the AWS vocabulary with a peering plan that both clouds can
express. A platform returns groups of nodes and the neighbours each group peers
with; the controller turns each group into one `FRRConfiguration`.

```go
// PeerGroup is a set of router nodes that share a BGP neighbour set.
type PeerGroup struct {
    // Key is stable across reconciles and names the generated FRRConfiguration.
    Key string
    // NodeSelector narrows spec.routerNodeSelector to this group's nodes.
    NodeSelector map[string]string
    Neighbors    []DiscoveredNeighbor
    // RawFRRConfig, when non-empty, is emitted as spec.raw.
    RawFRRConfig string
}
```

AWS emits one group per AZ, selecting on `topology.kubernetes.io/zone`. GCP
emits either one group for all router nodes or one per node, depending on
decision 2. The existing `DiscoveryResult` fields become AWS-private detail
used only to populate `status`.

### 2. FRR granularity, and whether GCP needs per-node configurations

Here, one `FRRConfiguration` is generated per AZ. On GCP one is generated per
node, selecting on `kubernetes.io/hostname`.

Reading `BuildFRRConfiguration` in the GCP operator, the neighbour list comes
from `topology.InterfaceIPs`, which does not depend on the node. Every
generated configuration is therefore identical apart from its node selector,
which suggests a single configuration selecting all router nodes would produce
the same FRR state. If that is right, GCP collapses to one group and this
decision disappears. It is an inference from the code path and has not been
tested against a live cluster, so it needs confirming by someone who can watch
FRR converge.

GCP also emits a `spec.raw` block at priority 20 carrying `neighbor <ip>
disable-connected-check`, needed because the GCP worker carries a `/32` on
`br-ex`. AWS needs no such block. The abstraction has to carry it, hence
`RawFRRConfig` above. It is cloud-specific but it is FRR configuration, so it
belongs in the peering plan rather than behind a platform method.

### 3. The API is not cloud-neutral

`CUDNBgpConfigSpec` embeds `AWS *AWSConfig` directly, `CUDNBgpConfigStatus`
embeds `AWS *AWSStatus`, a CEL rule couples `spec.aws` to
`spec.bgp.availabilityZones`, and two condition types are named
`AWSEndpointsDiscovered` and `AWSResourcesReconciled`. Platform selection in
`cudnbgpconfig_controller.go` is `if config.Spec.AWS != nil`.

This is the largest decision and the one most likely to be reopened later if
taken quietly now. The options:

- Add `spec.gcp` and `spec.azure` beside `spec.aws`, with a CEL rule making
  them mutually exclusive. Smallest diff, but the spec grows a field per
  cloud, the "exactly one of" rule becomes an N-way condition, and nothing in
  the object says which cloud it is without inspecting which block is
  populated.
- Introduce an explicit discriminator (`spec.platform`) with the cloud block
  beside it. The choice is legible in the object and in `kubectl get`, and
  adding a cloud is one enum value plus one block.
- Move the whole cloud block behind a discriminated union with a shared
  `type` field.

Recommendation: the second, and more clearly so at three clouds than at two.
It keeps illegal states out of reach at admission time and does not pretend
Go's type system is enforcing something CEL is.

Enum values should be added only alongside a working implementation.
Accepting `platform: Azure` before an Azure platform exists turns an admission
rejection into a runtime error, which is strictly worse.

The condition names are API surface and appear in `status.conditions`.
`AWSEndpointsDiscovered` and `AWSResourcesReconciled` become
`CloudEndpointsDiscovered` and `CloudResourcesReconciled`. This belongs with
the discriminator: renaming the conditions without changing the selection
mechanism leaves the API half cloud-neutral, which is worse than either end
state.

`status.aws` is left as a cloud-specific block parallel to `spec.aws`. That
generalises to `status.gcp` and `status.azure`, but three sibling optional
status blocks is a smell worth revisiting once the second one exists and it
is clear how much of the detail is genuinely cloud-shaped.

### 4. Router node selection: selector versus operator-applied label

Here, the user supplies `spec.routerNodeSelector` and the operator selects
whatever matches. On GCP the operator itself chooses candidates, sorts them,
and applies `routing.osd.redhat.com/bgp-router` to the winners
(`SyncRouterLabels` in `internal/reconciler/nodes.go`), then removes it from
nodes that fall out of selection.

These are different ownership models, not different spellings of one model.
The GCP behaviour matters when the node set churns: it is what keeps the label,
the Cloud Router peers, and the FRR configurations agreeing with each other.
The AWS behaviour is simpler and puts the operator out of the business of
mutating nodes.

This needs an explicit answer. If the merged operator keeps the AWS model, the
GCP deployment needs something else to apply that label, and the rh-mobb
Terraform currently does not. If it keeps the GCP model, the AWS side gains
node-mutating behaviour and the RBAC to go with it.

### 5. Machine `preTerminate` lifecycle hooks

The GCP operator registers a `preTerminate` hook on each router node's Machine
and holds it until the node's BGP peers have been withdrawn, then releases it
(`internal/reconciler/machines.go`, steps A to C in `Reconcile`). This
repository has no Machine handling at all -- no `preTerminate`, no
`lifecycleHook`, no reference to `openshift-machine-api`.

Without it, deleting a node races peer withdrawal: the instance disappears
while the Cloud Router still has a peer pointing at it. Whether that matters on
AWS depends on how Route Server handles a peer whose ENI has gone, which is
worth establishing rather than assuming.

`CloudPlatform` has no slot for this and should not grow one for every
platform. Proposal: an optional interface, type-asserted by the controller, so
AWS is unaffected:

```go
// NodeLifecycle is implemented by platforms that must withdraw cloud state
// before a node's Machine is allowed to terminate.
type NodeLifecycle interface {
    HoldTerminating(ctx context.Context, nodes []RouterNode) ([]RouterNode, error)
    ReleaseTerminating(ctx context.Context, held []RouterNode) error
}
```

### 6. The second CRD is on a different axis

Here, `CUDNBgpRouting` is user-facing input declaring a CUDN to advertise. On
GCP, `BGPRouter` is operator-authored per-node status: instance link, IP,
`canIpForward`, nested virtualisation, conditions.

Neither subsumes the other. A merged operator plausibly wants both concepts,
giving three kinds. The alternative is to fold the per-node detail into
`CUDNBgpConfig.status` as a list, which avoids a new kind but makes a
cluster-scoped singleton's status grow with the node count.

Recommendation: fold into status for now and revisit if the object gets
unwieldy. A per-node CRD is easy to add later and hard to remove.

### 7. Suspend

GCP has `spec.suspended`, which triggers cleanup of everything the operator
manages and pauses reconciliation while keeping the configuration. There is no
equivalent here. It is genuinely useful for a stack that reconciles cloud
state, and it is cheap to add, but it is new behaviour on the AWS side and
should be an explicit yes or no rather than something inherited by accident.

### 8. Dependencies

A GCP platform brings `cloud.google.com/go/compute` and
`cloud.google.com/go/networkconnectivity`, plus a credentials story. The GCP
operator authenticates through Workload Identity Federation using a mounted
`credential-config.json` produced by Terraform, which is a different shape from
the AWS credential path here (`sts:GetCallerIdentity` against the default chain
at platform construction).

Both the SDK addition and the credential mechanism need agreeing before code
lands, since this repository vendors its dependencies and the diff will be
large.

## Proposed target shape

```
internal/platform/
    platform.go          RouterNode, DiscoveredNeighbor, PeerGroup,
                         DiscoveryResult, CloudPlatform, NodeLifecycle
    aws/                 unchanged behaviour, emits PeerGroup per AZ
    gcp/                 new: Cloud Router topology, NCC spokes,
                         canIpForward, nested virtualisation,
                         Machine lifecycle hooks
internal/controller/     unchanged responsibilities; frr.go consumes
                         PeerGroup instead of AvailabilityZone
```

## Plan

Each stage is independently reviewable and leaves the tree green. Stages 1 to
3 are implemented on this branch; 4 and 5 are not started.

1. Generalise `platform.DiscoveryResult` to the peering plan; port the AWS
   implementation to emit `PeerGroup`; rewrite `frr.go` to consume it.
   Verify: `go build ./...` and the existing unit tests pass unchanged, and the
   `FRRConfiguration` objects generated for a given AWS discovery result are
   byte-identical to those generated before. A golden-output test on
   `ensureSingleFRRConfiguration` pins this before the refactor starts.
2. Replace `if config.Spec.AWS != nil` with real platform dispatch behind the
   decision taken in 3. Verify: a config naming an unknown platform is rejected
   at admission, and the AWS path is unchanged.
3. Add the optional `NodeLifecycle` interface with no implementations.
   Verify: build is unchanged, AWS behaviour is unchanged.
4. Add `internal/platform/gcp/` implementing `CloudPlatform` and
   `NodeLifecycle`, with fake clients mirroring `aws_test.go`.
   Verify: unit tests against fakes; no live cluster needed.
5. GCP e2e, modelled on `test/e2e/aws`.
   Verify: against a live OSD/GCP cluster.

Azure repeats stages 4 and 5 only. If it turns out to need changes to stages 1
to 3 as well, that is the signal the abstraction was fitted to AWS and GCP
rather than to the problem, and it should be reshaped then rather than grown
a third special case.

Stages 1 to 3 are mechanical and need no GCP knowledge. Stage 4 is where the
decisions above start to matter, so they need answering before it starts.

## Questions for the GCP authors

These cannot be answered from the code:

1. Is the per-node `FRRConfiguration` on GCP deliberate, or an artefact of the
   Python controller it was ported from? Would a single configuration
   selecting all router nodes converge identically? (Decision 2.)
2. Does anything outside the operator depend on the
   `routing.osd.redhat.com/bgp-router` label being applied by the operator
   rather than by Terraform or a MachineSet? (Decision 4.)
3. What breaks in practice without the `preTerminate` hook? Was it added in
   response to an observed failure, and if so what was the symptom?
   (Decision 5.)
4. Is `spec.suspended` used operationally, or was it built for development?
   (Decision 7.)
5. Is the `disable-connected-check` raw block still required on current OCP, or
   was it a workaround for a version that has since changed? (Decision 2.)

## References

- `internal/platform/platform.go`, `internal/platform/aws/`
- `internal/controller/cudnbgpconfig_controller.go`, `internal/controller/frr.go`
- `api/v1alpha1/cudnbgpconfig_types.go`
- rh-mobb `operator/internal/reconciler/reconciler.go`, `operator/internal/gcp/interfaces.go`,
  `operator/internal/frr/builder.go`, `operator/api/v1alpha1/bgproutingconfig_types.go`
- rh-mobb `ROSA_KNOWLEDGE.md` for the GCP and AWS behavioural comparison
