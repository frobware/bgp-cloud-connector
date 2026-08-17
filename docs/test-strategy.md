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
                   ┌──────────────────────────────┐  ┌──────────────────────────────────────────┐
                   │                              │  │                                          │
  Unit tests       │  internal/controller/*_test  │  │  internal/platform/<provider>/*_test     │
                   │  • Config controller         │  │  • Cloud credential verification         │
                   │    Phases 1-5 (mocked        │  │  • Provider ID → instance ID + AZ        │
                   │    CloudPlatform)            │  │  • Endpoint discovery (mocked API)       │
                   │                              │  │  • Peer reconciliation (mocked API)      │
                   │  • Helpers (NS, CUDN, FRR,   │  │  • Forwarding fix (mocked API)           │
                   │    RouteAdvertisements)      │  │                                          │
                   │  • Routing controller        │  │                                          │
                   │                              │  │                                          │
  E2E tests        │  test/e2e/ (shared tests)    │  │  test/e2e/<provider>/ tests              │
                   │  • Full stack reconcile +    │  │  • Full stack reconcile on cluster       │
                   │    BGP session verification  │  │  • Node-to-peer consistency              │
                   │  • FRR config drift recovery │  │  • Cloud drift recovery                  │
                   │  • FRR pod restart recovery  │  │  • Deletion cleanup (cloud resources)    │
                   │  • Deletion dependency check │  │  Skip when cloud creds not configured    │
                   │  Requires external BGP peer  │  │                                          │
                   └──────────────────────────────┘  └──────────────────────────────────────────┘
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
    cudnbgpconfig_controller_test.go   ← config controller (Phases 1-5, mocked CloudPlatform)
    cudnbgprouting_controller_test.go  ← routing controller
    helpers_test.go                    ← helpers (NS, CUDN, FRR, RouteAdvertisements)
  platform/                            ← mocked at cloud SDK client level
    aws/
      aws_test.go                      ← AWS platform (mocked EC2/STS clients)
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

E2E tests read CR manifests from a profile directory under `test/e2e/manifests/<profile>/`. Shared E2E tests use CRs under `platform: Manual` (explicit `peerGroups`); provider-specific tests require `spec.aws` (or equivalent).

**Shared E2E tests** validate behavior that only a real cluster with a live BGP peer can exercise — BGP session establishment, route advertisement, FRR config drift recovery, and cleanup. Unit tests cover the reconciliation logic itself; shared E2E tests verify the downstream effect on actual BGP sessions.

**Provider-specific E2E tests** additionally verify cloud resource reconciliation (e.g., Route Server peers, SourceDestCheck for AWS).

### Make targets

| Target | What it runs | Credentials needed |
|:---|:---|:---|
| `make test` | Platform-independent unit tests (`internal/controller/`) | No |
| `make test-aws` | AWS unit tests, mocked (`internal/platform/aws/`) | No |
| `make test-e2e <profile>` | Shared E2E (BGP session + drift recovery) | No (cluster + external BGP peer) |
| `make test-e2e-aws <profile>` | AWS E2E tests (`test/e2e/aws/`), profile required | Yes (cluster + IRSA configured) |

## Platform Interface

The `CloudPlatform` interface (`internal/platform/platform.go`) defines three methods:

```go
type CloudPlatform interface {
    DiscoverEndpoints(ctx context.Context) (*DiscoveryResult, error)
    ReconcileNodes(ctx context.Context, nodes []RouterNode) error
    Cleanup(ctx context.Context) error
}
```

`DiscoverEndpoints` returns the discovered Route Server endpoints, their BGP neighbor addresses, AZs, and remote ASN. This data drives FRR configuration generation (Phase 4) and is written to CR status. Every cloud provider implements this interface. Each provider's test plan maps to the same set of concerns:

| Test category | Interface concept | AWS | GCP (future) | Azure (future) |
|:---|:---|:---|:---|:---|
| Platform initialization | `New()` constructor | IRSA (default credential chain) + `sts:GetCallerIdentity` validation | Workload Identity | Workload Identity |
| Provider ID → instance ID + AZ | `RouterNode.ProviderID` | `aws:///zone/instance` | `gce:///project/zone/instance` | `azure:///...` |
| Endpoint discovery | `DiscoverEndpoints` | DescribeRouteServers + DescribeRouteServerEndpoints + DescribeSubnets | Cloud Router interface listing | Azure Route Server IP config |
| Peer reconciliation | `ReconcileNodes` — peering | VPC Route Server peers | Cloud Router peers | Azure Route Server peers |
| Forwarding fix | `ReconcileNodes` — forwarding | SourceDestCheck=false | canIpForward=true | IP forwarding=enabled |

## Test Plans

| Scope | Test plan | Status |
|:---|:---|:---|
| Platform-independent (controllers + helpers + E2E) | [docs/controller-test-plan.md](controller-test-plan.md) | Active |
| AWS | [docs/aws-integration-test-plan.md](aws-integration-test-plan.md) | Active |
| GCP | `docs/gcp-integration-test-plan.md` | Future |
| Azure | `docs/azure-integration-test-plan.md` | Future |

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

The shared E2E job requires a cluster with an external BGP peer and a profile with explicit `peerGroups`. The AWS E2E job additionally requires IRSA configured for the operator's ServiceAccount.
