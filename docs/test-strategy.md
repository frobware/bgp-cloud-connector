# Test Strategy

Test strategy for the CUDN BGP Routing Operator. The operator has two layers of functionality — core BGP/CUDN reconciliation (platform-independent) and cloud platform integration (per-provider). This document describes the shared test structure that all platform-specific test plans follow.

- [Coverage by layer and location](#coverage-by-layer-and-location)
- [Test Layers](#test-layers)
- [Platform Interface](#platform-interface)
- [Test Plans](#test-plans)
- [OpenShift CI Pipeline](#openshift-ci-pipeline)

## Coverage by layer and location

```
                      Platform-independent              Provider-specific (AWS/GCP/Azure)
                   +------------------------------+  +------------------------------------------+
                   |                              |  |                                          |
  Unit tests       |  internal/controller/*_test  |  |  internal/platform/<provider>/*_test     |
                   |  - Config controller         |  |  - Cloud credential verification         |
                   |    Phases 1-6 (mocked        |  |  - Provider ID -> instance identity      |
                   |    CloudPlatform)            |  |  - Endpoint discovery (mocked API)       |
                   |  - Router node labelling     |  |  - Peer reconciliation (mocked API)      |
                   |  - Helpers (NS, CUDN, FRR,   |  |  - Forwarding fix (mocked API)           |
                   |    RouteAdvertisements)      |  |  - Prerequisite checks (mocked API)      |
                   |  - Routing controller        |  |  - Machine lifecycle hooks               |
                   |                              |  |                                          |
  E2E tests        |  test/e2e/ (shared tests)    |  |  test/e2e/<provider>/ tests              |
                   |  - Full stack reconcile +    |  |  - Full stack reconcile on cluster       |
                   |    BGP session verification  |  |  - Node-to-peer consistency              |
                   |  - FRR config drift recovery |  |  - Cloud drift recovery                  |
                   |  - FRR pod restart recovery  |  |  - Deletion cleanup (cloud resources)    |
                   |  - Deletion dependency check |  |  Fails, never skips, on a missing fixture|
                   |  Requires external BGP peer  |  |                                          |
                   +------------------------------+  +------------------------------------------+
```

## Test Layers

Ordered by infrastructure cost:

| Layer | What it validates | Infrastructure |
|:---|:---|:---|
| Unit | Reconciliation logic, error handling, edge cases | None (fake client + mocks) |
| Unit (provider) | Cloud API integration (mocked API clients) | None (mocked API clients) |
| E2E | Operator lifecycle + BGP session verification on a real cluster | Cluster + external BGP peer |
| E2E (provider) | Full operator lifecycle including cloud resource reconciliation | Cluster + cloud infra (Terraform) |

### Unit test structure

```
internal/
  controller/
    cudnbgpconfig_controller_test.go   config controller (Phases 1-6, mocked CloudPlatform)
    cudnbgprouting_controller_test.go  routing controller
    helpers_test.go                    helpers (NS, CUDN, FRR, RouteAdvertisements)
    router_labels_test.go              autoLabelRouterNodes eligibility and pruning
    node_lifecycle_test.go             NodeLifecycle hold and release dispatch
    platform_dispatch_test.go          spec.platform to platform builder
    frr_golden_test.go                 generated FRRConfiguration, pinned output
    status_test.go                     condition and phase transitions
    events_test.go                     events emitted on condition change
    backoff_test.go                    system faults returned to the workqueue
  platform/                            mocked at cloud SDK client level
    aws/
      aws_test.go                      AWS platform (mocked EC2/STS clients)
    gcp/
      gcp_test.go                      discovery, provider ID, prerequisites
      peers_test.go                    Cloud Router peer reconciliation
      machines_test.go                 preTerminate hold and release
      live_*_test.go                   live GCP, skipped unless GCP_PROJECT,
                                       GCP_REGION and GCP_CLOUD_ROUTER are set
```

### E2E test structure

```
test/e2e/
  e2e_suite_test.go                    ← shared suite setup (k8s client, profile loading)
  e2e_test.go                          ← shared E2E tests (BGP session verification, drift recovery)
  aws/
    aws_e2e_suite_test.go              ← AWS suite setup (k8s client + EC2 client + discovery)
    aws_e2e_test.go                    ← AWS E2E (requires AWS credential configured)
  manifests/
    <profile>/                         ← per-cluster profile (CUDNBgpConfig + CUDNBgpRouting)
      cudnbgpconfig.yaml
      cudnbgprouting.yaml
```

E2E tests read CR manifests from a profile directory under `test/e2e/manifests/<profile>/`. Every `CUDNBgpConfig` must set `spec.platform`, which is the discriminator for the cloud block beside it. Shared E2E tests use `platform: Manual`, where CEL requires `spec.bgp.peerGroups` and forbids any cloud block; provider-specific tests use `platform: AWS` with `spec.aws`, or `platform: GCP` with `spec.gcp`, where CEL forbids `peerGroups`.

**Shared E2E tests** validate behavior that only a real cluster with a live BGP peer can exercise — BGP session establishment, route advertisement, FRR config drift recovery, and cleanup. Unit tests cover the reconciliation logic itself; shared E2E tests verify the downstream effect on actual BGP sessions.

**Provider-specific E2E tests** additionally verify cloud resource reconciliation (e.g., Route Server peers, SourceDestCheck for AWS).

### Make targets

| Target | What it runs | Credentials needed |
|:---|:---|:---|
| `make test` | Platform-independent unit tests (`internal/controller/`, `api/`, `cmd/`) | No |
| `make test-aws` | AWS unit tests, mocked (`internal/platform/aws/`) | No |
| `make test-e2e <profile>` | Shared E2E (BGP session + drift recovery) | No (cluster + external BGP peer) |
| `make test-e2e-aws <profile>` | AWS E2E tests (`test/e2e/aws/`), profile required | Yes (cluster + IRSA configured) |

Two gaps in that table are real rather than editorial. There is no `test-gcp`
target, and `make test` covers only `./internal/controller/... ./api/...
./cmd/...`, so **nothing runs the GCP unit tests**. There is no `test-e2e-gcp`
target and no `test/e2e/gcp` directory, so the GCP platform has no e2e coverage
at all. Both are tracked in [docs/e2e-plan.md](e2e-plan.md).

## Platform Interface

The `CloudPlatform` interface (`internal/platform/platform.go`) defines four methods, and every cloud implements all four:

```go
type CloudPlatform interface {
    DiscoverEndpoints(ctx context.Context) (*DiscoveryResult, error)
    ReconcileNodes(ctx context.Context, nodes []RouterNode) error
    Cleanup(ctx context.Context) error
    CheckPrerequisites(ctx context.Context) ([]string, error)
}
```

`DiscoverEndpoints` returns a cloud-neutral peering plan: a list of `PeerGroup`, each a set of router nodes sharing a BGP neighbour set, which the controller renders one-for-one into `FRRConfiguration` objects. How a cloud groups is its own business. AWS emits one group per availability zone, selecting on `topology.kubernetes.io/zone`; GCP emits a single group covering every router node, because one regional Cloud Router's interface addresses are the neighbours for all of them. A group may also carry `RawFRRConfig`, emitted as `spec.raw`, for directives the structured neighbour API cannot express.

`DiscoveryResult` carries nothing else. It used to also carry `RouteServers`, `NeighborsByAZ` and `EndpointsByAZ`, which only AWS populated and only `status.aws` consumed; both are gone, and the discovered plan is reported cloud-neutrally as `status.peerGroups`.

`CheckPrerequisites` reports cloud configuration the operator relies on but deliberately does not create, one human-readable line per unmet requirement. It is read-only. It exists because the sharpest failures here are silent: on AWS a route server propagating to no route table leaves every peer available and every session established while nothing in the VPC can reach a pod.

A platform may additionally implement the optional `NodeLifecycle` interface (`HoldTerminating` / `ReleaseTerminating`), which the controller type-asserts. GCP implements it to hold a Machine `preTerminate` hook until the node's BGP peers are withdrawn; AWS does not implement it and the controller then skips the hold entirely.

Each provider's test plan maps to the same set of concerns:

| Test category | Interface concept | AWS | GCP | Azure (future) |
|:---|:---|:---|:---|:---|
| Platform initialization | `New()` constructor | IRSA (default credential chain) + `sts:GetCallerIdentity` validation | Workload Identity Federation | Workload Identity |
| Provider ID -> instance identity | `RouterNode.ProviderID` | `aws:///zone/instance` | `gce://project/zone/name` | `azure:///...` |
| Endpoint discovery | `DiscoverEndpoints` | DescribeRouteServers + DescribeRouteServerEndpoints + DescribeSubnets, one peer group per AZ | Cloud Router interface listing, one peer group | Azure Route Server IP config |
| Peer reconciliation | `ReconcileNodes` -- peering | VPC Route Server peers, keyed on node address and tagged `managed-by` | NCC spokes plus Cloud Router peers, keyed on node address | Azure Route Server peers |
| Forwarding fix | `ReconcileNodes` -- forwarding | SourceDestCheck=false | canIpForward=true | IP forwarding=enabled |
| Prerequisite reporting | `CheckPrerequisites` | route server propagation to a route table | Cloud Router has interfaces and an ASN; ingress rule for TCP 179 | to be determined |
| Termination ordering | `NodeLifecycle` (optional) | not implemented | Machine `preTerminate` hook | to be determined |

## Test Plans

| Scope | Test plan | Status |
|:---|:---|:---|
| Platform-independent (controllers + helpers + E2E) | [docs/controller-test-plan.md](controller-test-plan.md) | Active |
| AWS | [docs/aws-integration-test-plan.md](aws-integration-test-plan.md) | Active |
| GCP | `docs/gcp-integration-test-plan.md` | Not written; the platform is implemented and its unit tests exist, so this doc is the gap, not the coverage |
| Azure | `docs/azure-integration-test-plan.md` | Future; no platform implementation |

What to test next, and in what order, is [docs/e2e-plan.md](e2e-plan.md). It supersedes the priorities implied by the per-provider plans below.

When adding a new provider, clone the AWS test plan and replace:

1. **Unit tests** — swap AWS API mocks for the new provider's API mocks. Peer reconciliation, forwarding fix, and provider ID parsing test cases map 1:1. Controller tests (mocked `CloudPlatform`) do not need to be duplicated — they are provider-agnostic.
2. **E2E tests** — same test categories (initial deployment, node lifecycle, self-healing, deletion). Swap AWS-specific verifications (Route Server peers, SourceDestCheck) for provider equivalents.

## OpenShift CI Pipeline

When the project moves to an OpenShift CI-managed repository, the following pipeline structure can be used:

| Layer | ci-operator type | Trigger |
|:---|:---|:---|
| Unit | Container test | Every PR (presubmit) |
| E2E (shared) | Container test + cluster with BGP peer | On demand (`/test e2e <profile>`) |
| E2E (AWS) | Container test + cluster + cloud credentials | On demand (`/test e2e-aws <profile>`) |
| E2E (GCP) | Container test + cluster + cloud credentials | On demand (`/test e2e-gcp <profile>`) |

The shared E2E job requires a cluster with an external BGP peer and a profile using `platform: Manual`. The AWS E2E job additionally requires IRSA configured for the operator's ServiceAccount; the GCP job requires Workload Identity Federation.
