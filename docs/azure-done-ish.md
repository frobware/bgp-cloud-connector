# Azure: done-ish

Azure is implemented, wired into dispatch, and covered by twenty unit tests.
It has never run against an Azure Route Server. Both halves of that sentence
matter, and the second is why this file is not called "Azure is done".

Everything below the line "What is not verified" is the honest inventory of
what we do not know, and it is longer than it looks.

## What is implemented

`internal/platform/azure/` implements all four `CloudPlatform` methods.

| Method | What it does |
| --- | --- |
| `DiscoverEndpoints` | Reads the Virtual Hub's `VirtualRouterIPs` and `VirtualRouterAsn` and returns a single peer group with `EBGPMultiHop` on every neighbour |
| `ReconcileNodes` | Creates, updates and prunes `VirtualHubBgpConnections`, one per router node. No instance attribute to set |
| `Cleanup` | Deletes the peerings this cluster owns |
| `CheckPrerequisites` | Reports a Route Server with no addresses or no ASN |

`spec.azure` carries `subscriptionID`, `resourceGroup` and `routeServerName`.
`Azure` is in the `spec.platform` enum with a CEL rule pairing the block to the
discriminator, and `defaultPlatformBuilder` dispatches to it --
`TestDefaultPlatformBuilder_EveryEnumValueDispatches` walks the enum and fails
if that case is ever removed, which is the trap that put
`no platform implementation for "GCP"` on a live cluster once already.

Credentials come from `azidentity.NewDefaultAzureCredential`, which resolves
workload identity federation in cluster and an `az login` outside it.

## What Azure taught us about the abstraction

Azure arrived after the abstraction had been fitted to AWS and GCP, which is
exactly the test `gcp-integration-design.md` set for it. Three things it
changed, all of which landed before Azure itself did:

**`ebgpMultiHop` on a neighbour.** Azure Route Server is not on the router
node's link, so its working configuration sets `ebgpMultiHop: true` on every
neighbour. Nothing in the peering plan could express that. A group could carry
raw FRR text, but `ebgpMultiHop` is a structured field in frr-k8s, and using
the raw escape hatch to avoid widening a type is the abstraction leaking. So
`DiscoveredNeighbor` and `BGPNeighbor` both gained the field.

**Only AWS has an availability zone axis.** GCP and Azure both peer every
router node with the same regional pair, so both had to declare a single
"availability zone" that is not a zone. `spec.bgp.availabilityZones` became
`spec.bgp.peerGroups`, and the generated objects lost their `-az-`.

**`status.aws` did not generalise.** Azure's own operator reports per-peer
status as a list rather than a cloud-shaped block, which is the answer
`gcp-integration-design.md` had recommended and not taken. `status.peerGroups`
replaced it, and `RouteServers`/`NeighborsByAZ`/`EndpointsByAZ` left the shared
`DiscoveryResult` with it.

A fourth thing Azure confirmed rather than changed: Machine `preTerminate`
handling belongs in the controller. Azure's `internal/reconciler/machines.go`
is structurally the same code as GCP's, down to the three-case switch and the
same test for the Machine API being absent. Three independent implementations
of one algorithm is not a coincidence to file under a cloud, and it has now
moved, deleting `NodeLifecycle` on the way out.

## What was taken, and what was not

The implementation came from
[`rh-mobb/managed-bgp-peer-operator`](https://github.com/rh-mobb/managed-bgp-peer-operator).
Taken close to verbatim and renamed to our vocabulary: the SDK choice and
versions, the client construction, `ListPeers`, the reconcile diffing, and the
peer-set comparison and lookup with their test cases.

Two things are deliberately **not** theirs, both correctness rather than
naming, and both worth knowing if you compare the two side by side:

**Peer naming.** Theirs names a peering after the node. Ours keys on the node
address behind a `<clusterID>-bgp-` prefix, matching GCP. An Azure BGP
connection carries no tags, so the name is the only ownership signal there is.

**Cleanup scope.** Their `DeleteAllPeers` removes every peering on the Route
Server. Ours prunes only what carries our prefix. Two clusters can share a
Route Server, and theirs would disconnect the other one on teardown. This is
the "quietly inherited the weaker half of each side" failure
[`GUIDERAILS.md`](../GUIDERAILS.md) rule 2 records, and there is a test pinning
it.

Not taken at all: their `RequireReadyNodes` default. Azure defaults it on; we
added `spec.requireReadyNodes` defaulting **off**, because turning it on churns
cloud peerings whenever a node reboots and how long nodes stay NotReady is not
something the operator can know.

## What is not verified

**Nothing has run against Azure.** No ARO cluster exists, no Route Server
exists in the subscription, and `internal/platform/azure/` has no live tests --
where AWS and GCP each have a `live_test.go` that talks to the real API. The
twenty unit tests all drive a fake.

Concretely, these are assumptions until something runs:

- That `VirtualHubsClient.Get` returns `VirtualRouterIPs` and `VirtualRouterAsn`
  populated for a real Route Server. The rh-mobb operator never reads them --
  it takes the addresses from Terraform output -- so this path exists in no
  working implementation, ours or theirs.
- That `BeginCreateOrUpdate` accepts the body we send. The call shape matches
  theirs, which has run, and we dropped a redundant `Name` field to match, but
  we have not seen Azure accept it.
- That `ebgpMultiHop` plus our generated FRRConfiguration actually establishes a
  session. The value comes from `aro-bgp`'s working script, but our generated
  object is not their hand-written one.
- That `CheckPrerequisites` checks anything useful. No Azure equivalent of AWS's
  propagation trap is known -- the rh-mobb operator checks nothing and nothing
  in `aro-bgp` documents one -- so ours reports only what the Route Server can
  tell us about itself. That is honest but probably incomplete.
- That the credential works in cluster. `azidentity` resolving an `az login`
  locally says nothing about workload identity federation on ARO, which
  `aro-bgp` sets up as a federated identity under the `openshift-machine-api`
  managed identity.

## To finish it

1. Build a Route Server. `aro-bgp`'s Terraform is the Azure equivalent of
   `gcp-create-cloud-router --prerequisites-only`; check whether it can be run
   for the prerequisites alone, because building the peerings too would leave
   an estate that makes the reconcile tests pass against an operator that did
   nothing.
2. Add `internal/platform/azure/live_test.go` behind an `azurelive` build tag,
   mirroring the AWS and GCP ones: read-only, narrated, and falsified by
   pointing it at the wrong Route Server.
3. Add `make test-azure-live` beside `test-aws-live` and `test-gcp-live`.
4. Deploy the operator on ARO and run the full reconcile. That is the only
   thing that exercises the credential, the dispatch and the FRR interaction
   together.

## Questions for Thatcher

1. Does anything enable NIC IP forwarding on the router nodes? AWS clears
   `SourceDestCheck` and GCP sets `canIpForward`; the Azure operator does
   neither and nothing in `aro-bgp` appeared to. If Azure genuinely needs none,
   that is a fact worth writing down, because it makes `ReconcileNodes` do two
   things on two clouds and one on the third.
2. `oc-cudn-rsconfig.sh` sets `toAdvertise: allowed: mode: all` on each
   neighbour. We set `toReceive`, and AWS and GCP independently chose the same.
   Your script also enables `routeAdvertisements`, so OVN-Kubernetes should be
   generating the advertise side itself -- does the `toAdvertise` block
   still do something, or is it left over?
3. Is there an Azure failure that is silent in the way AWS's missing route
   server propagation is -- everything reporting healthy while nothing reaches
   a pod? That is what `CheckPrerequisites` is for and we currently have
   nothing to put in it.
