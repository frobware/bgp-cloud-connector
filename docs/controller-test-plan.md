# Controller Test Plan

Tests for the CUDN BGP Routing Operator controllers and helpers. Unit tests are split into two sections:

- **Basic (platform-independent)** -- tests the controller reconciliation logic with `platform: Manual` and explicit `spec.bgp.peerGroups`, no cloud provider involved.
- **Platform interface** -- tests the controller's interaction with the generic `CloudPlatform` interface (Phases 4 and 6: discovery + cloud resource reconciliation). The interface is provider-agnostic; the tests inject a mock through `PlatformBuilder` and never reach a real cloud package.

The reconcile is six phases: 1 patch the Network operator, 2 wait for FRR, 3 settle which nodes are BGP routers, 4 discover cloud endpoints, 5 apply FRRConfigurations, 6 reconcile cloud resources. Phases 4 and 6 are skipped under `platform: Manual`.

- [Test Configuration](#test-configuration)
- [Unit Tests](#unit-tests)
  - [Basic (platform-independent)](#basic-platform-independent)
  - [Platform interface (AWS as provider)](#platform-interface-aws-as-provider)
- [E2E Tests](#e2e-tests)
- [How to Run](#how-to-run)

---

## Test Configuration

**Basic unit tests** use `platform: Manual` with explicit `spec.bgp.peerGroups`:

| Field | Value |
|:---|:---|
| Local BGP ASN | 65001 |
| Remote BGP ASN | 64512 |
| Router node selector | `networking.openshift.io/cudn-bgp-router: ""` |
| Availability Zones | 1 (minimal for unit tests) |
| Neighbor addresses | `10.0.1.47`, `10.0.1.183` |
| CUDN subnets | `10.100.0.0/16` |

**Platform interface tests** use `platform: AWS` with `spec.aws`, and inject a mocked `CloudPlatform` through the reconciler's `PlatformBuilder`. The mock returns a `DiscoveryResult` with 1 Route Server, 1 endpoint (`rse-001` in `us-east-1a`, address `10.0.1.47`, remote ASN `64512`). No real AWS credentials required, and no real cloud package is reached.

AWS is only the mock's costume here. Nothing in these tests is AWS-specific beyond the spec block needed to satisfy the CEL discriminator rule, and they would read identically against `platform: GCP`. Because they all bypass the dispatch switch, they cannot catch a platform that is implemented but never wired into it -- `platform_dispatch_test.go` exists for that alone.

**E2E tests** read CR manifests from a profile directory (`test/e2e/manifests/<profile>/`). Each profile contains a `cudnbgpconfig.yaml` and `cudnbgprouting.yaml` matching the target cluster. Shared E2E tests require `platform: Manual` with explicit `spec.bgp.peerGroups`. See the `ocp-or18` profile for an example.

---

## Unit Tests

All unit tests use a fake Kubernetes client. They never invoke real provider code.

---

### Basic (platform-independent)

Tests the controller reconciliation logic without any cloud provider configured.

#### Config Controller

| ID | Test Case | Setup | Expected Result | Test |
|:---|:---|:---|:---|:---|
| UT-01 | Full reconcile (Phases 1, 2, 5) | Network CR exists, FRR namespace + pod running, `platform: Manual` with explicit `bgp.peerGroups` | Network patched, FRRConfigurations created from explicit neighbours, phase=Ready with 3 conditions: `NetworkOperatorPatched`, `FRRNamespaceReady`, `FRRConfigurationApplied` | `TestConfigReconcile_FullReconcile` |
| UT-02 | Delete blocked by routing CRs | CUDNBgpRouting CR exists | Finalizer retained, requeues every 10s | `TestConfigReconcile_DeleteBlockedByRouting` |
| UT-02b | Manual skips the platform entirely | `platform: Manual` | No platform built, no discovery, no cloud conditions set | `TestConfigReconcile_ManualSkipsPlatform` |
| UT-02c | Every enum value dispatches | Each value of the `spec.platform` enum | A builder exists for each; no value compiles to a runtime "no implementation" error | `TestDefaultPlatformBuilder_EveryEnumValueDispatches` |
| UT-02d | Unknown platform | A platform value outside the enum | Error, not a silent no-op | `TestDefaultPlatformBuilder_UnknownPlatform` |

#### Router node labelling (Phase 3)

Runs only when `spec.autoLabelRouterNodes` is set; without it the operator only ever reads Node objects.

| ID | Test Case | Setup | Expected Result | Test |
|:---|:---|:---|:---|:---|
| UT-02e | Disabled by default | No `autoLabelRouterNodes` | No node is written | `TestSyncRouterLabels_Disabled` |
| UT-02f | Labels eligible nodes | Nodes matching `eligible` | Router label applied | `TestSyncRouterLabels_LabelsEligible` |
| UT-02g | Honours exclusions | Node carries an `exclude` key | Not labelled, whatever the value | `TestSyncRouterLabels_SkipsExcluded` |
| UT-02h | Prunes on ineligibility | Labelled node stops matching | Label removed | `TestSyncRouterLabels_RemovesFromIneligible` |
| UT-02i | Idempotent | Labels already correct | No write | `TestSyncRouterLabels_Idempotent` |
| UT-02j | Cleanup removes all | Config deleted | Every operator-applied label removed | `TestRemoveAllRouterLabels` |

#### Routing Controller

| ID | Test Case | Setup | Expected Result | Test |
|:---|:---|:---|:---|:---|
| UT-03 | Duplicate network name | Another CUDNBgpRouting claims same spec.network.name | phase=Degraded, reason=DuplicateNetwork | `TestRoutingReconcile_DuplicateNetworkName` |
| UT-04 | Full reconcile | Config Ready, labelled namespace pre-created | CUDN + RouteAdvertisements created, phase=Ready with 2 conditions | `TestRoutingReconcile_FullReconcile` |
| UT-04b | No labelled namespace | Config Ready, no namespace with required labels | phase=Degraded, reason=NamespaceNotReady | `TestRoutingReconcile_NoNamespace` |
| UT-05 | Delete last removes RA | No other CUDNBgpRouting CRs | CUDN deleted, RouteAdvertisements deleted, finalizer removed | `TestRoutingReconcile_DeleteLastRemovesRA` |
| UT-06 | Delete keeps RA when others exist | Another CUDNBgpRouting CR exists | CUDN deleted, RouteAdvertisements retained | `TestRoutingReconcile_DeleteKeepsRAWhenOthersExist` |

#### Watch Map Functions

| ID | Test Case | Setup | Expected Result | Test |
|:---|:---|:---|:---|:---|
| UT-07 | CUDN watch maps to owning routing CR | Managed CUDN `cluster-udn-prod`, routing CR `prod` exists | Reconcile request for routing CR `prod` | `TestMapCUDNToRouting_ManagedCUDN` |
| UT-08 | Unmanaged CUDN ignored | CUDN without `managed-by` label | No reconcile requests | `TestMapCUDNToRouting_UnmanagedCUDN` |
| UT-09 | RA watch maps to all routing CRs | Managed RouteAdvertisements, two routing CRs exist | Reconcile requests for both routing CRs | `TestMapRAToRouting_ManagedRA` |
| UT-10 | Unmanaged RA ignored | RouteAdvertisements without `managed-by` label | No reconcile requests | `TestMapRAToRouting_UnmanagedRA` |

#### Helpers

Non-trivial helper logic tested in isolation. Simple CRUD helpers (create, delete) are covered implicitly by the controller tests above.

| ID | Test Case | Verifies | Test |
|:---|:---|:---|:---|
| UT-11 | ValidateNamespaceLabels found | Returns nil when namespace with required labels exists | `TestValidateNamespaceLabels_Found` |
| UT-11b | ValidateNamespaceLabels not found | Returns error when no namespace has required labels | `TestValidateNamespaceLabels_NotFound` |
| UT-12 | EnsureFRRConfigurations BFD | BFD profile added when livenessDetection=bfd | `TestEnsureFRRConfigurations_BFDProfile` |
| UT-13 | EnsureFRRConfigurations prunes stale | Stale managed configs deleted when the peer group count drops | `TestEnsureFRRConfigurations_PrunesStale` |
| UT-14 | EnsureFRRConfigurations keeps unmanaged | User-owned FRRConfigurations not pruned | `TestEnsureFRRConfigurations_KeepsUnmanagedResources` |

#### Generated FRRConfiguration, pinned

Golden tests over the rendered output. They exist so that a refactor of the peering plan cannot quietly change what FRR is asked to do, which is the one thing no unit test asserting on Go structs would notice.

| ID | Test Case | Verifies | Test |
|:---|:---|:---|:---|
| UT-14a | From explicit spec | `platform: Manual` renders byte-identical output | `TestGoldenFRRFromSpec` |
| UT-14b | From explicit spec, BFD | BFD profile in the rendered output | `TestGoldenFRRFromSpecBFD` |
| UT-14c | From discovery | Peer groups render the same as the equivalent explicit spec | `TestGoldenFRRFromDiscovery` |
| UT-14d | Raw config block | A group's `RawFRRConfig` is emitted as `spec.raw` at the expected priority | `TestGoldenFRRRawConfig` |
| UT-14e | Pruning | Stale generated configurations removed | `TestGoldenFRRPrunesStale` |

#### Status, events and requeue behaviour

| ID | Test Case | Verifies | Test |
|:---|:---|:---|:---|
| UT-14f | Status write skipped when unchanged | A periodic resync that changes nothing writes nothing | `TestPatchConfigStatus_SkipsUnchangedStatus` |
| UT-14g | Status written when phase changes | A real transition is not suppressed | `TestPatchConfigStatus_WritesWhenPhaseChanges` |
| UT-14h | Watched objects written only when different | The regression that produced roughly two reconciles a second: both controllers watch what they write | `TestCreateOrUpdate_NoWriteWhenUnchanged`, `TestCreateOrUpdate_WritesWhenChanged` |
| UT-14i | Events on condition change | Event emitted when a condition flips, and on message-only changes | `TestEvents_EmittedOnFirstReconcile`, `TestEvents_EmittedWhenOnlyMessageChanges` |
| UT-14j | Events silent otherwise | Nothing emitted when nothing changed | `TestEvents_SilentWhenNothingChanges`, `TestEvents_SilentWhenIdentical` |
| UT-14k | Warning on degrade | Degrade emits a Warning | `TestEvents_WarningOnDegrade` |
| UT-14l | System faults back the queue off | Degrading on a system fault returns an error so the workqueue backs off, rather than swallowing it | `TestDegrade_ReturnsErrorSoTheQueueBacksOff` |
| UT-14m | Configured resync interval honoured | `--resync-interval` reaches the requeue | `TestRoutingReconcile_ConfiguredResyncInterval` |

---

### Platform interface (AWS as provider)

Tests the config controller's interaction with the generic `CloudPlatform` interface.

#### Reconciliation

| ID | Test Case | Setup | Expected Result | Test |
|:---|:---|:---|:---|:---|
| UT-15 | Full reconcile with cloud (Phases 1-6) | `platform: AWS`, spec.aws with routeServerIDs, mock platform with discovery | `CloudEndpointsDiscovered`=True, FRRConfigurations created from discovered neighbours, ReconcileNodes called, `CloudResourcesReconciled`=True, phase=Ready with 6 conditions: the 3 above plus `PrerequisitesSatisfied`, `CloudEndpointsDiscovered`, `CloudResourcesReconciled`; `status.aws` populated | `TestConfigReconcile_AWSFullReconcile` |
| UT-16 | Credential failure | Mock platform builder returns CredentialError | `CloudEndpointsDiscovered`=False, reason=`CloudCredentialsInvalid`, phase=Degraded | `TestConfigReconcile_AWSCredentialFailure` |
| UT-17 | Phase 4 discovery failure | Mock platform discovery returns error | `CloudEndpointsDiscovered`=False, reason=`CloudDiscoveryFailed`, phase=Degraded, requeue 30s | `TestConfigReconcile_AWSDiscoveryFailure` |
| UT-18 | Phase 6 failure | Mock platform ReconcileNodes returns error | `CloudResourcesReconciled`=False, reason=`CloudReconcileFailed`, phase=Degraded, requeue 30s | `TestConfigReconcile_AWSReconcileFailure` |
| UT-19 | Node filtering | 5 nodes: 3 complete, 1 missing IP, 1 missing AZ | Only 3 RouterNodes passed to ReconcileNodes | `TestConfigReconcile_AWSNodeFiltering` |

#### Prerequisites

Checked before discovery, on every cloud. The operator keeps reconciling when one is unmet, so that fixing it is enough and no second action is needed, but withholds Ready.

| ID | Test Case | Setup | Expected Result | Test |
|:---|:---|:---|:---|:---|
| UT-19a | Prerequisites satisfied | Mock returns an empty slice | `PrerequisitesSatisfied`=True, reason=`Satisfied`, reconcile continues | covered by UT-15 |
| UT-19b | Prerequisite unmet | Mock returns one or more lines | `PrerequisitesSatisfied`=False, reason=`Unmet`, message carries the remedy, phase not Ready, reconcile still continues | **not covered** |
| UT-19c | Check itself fails | Mock returns an error | `PrerequisitesSatisfied`=False, reason=`CheckFailed`, phase=Degraded | **not covered** |

#### Node lifecycle (optional `NodeLifecycle`)

| ID | Test Case | Setup | Expected Result | Test |
|:---|:---|:---|:---|:---|
| UT-19d | Terminating node held then released | Mock implements NodeLifecycle, one node terminating | Held node excluded from ReconcileNodes so its peers are withdrawn, then released | `TestLifecycle_TerminatingNodeExcludedThenReleased` |
| UT-19e | Nothing terminating | No node terminating | No release call | `TestLifecycle_NothingTerminatingSkipsRelease` |
| UT-19f | Platform without the interface | Mock does not implement NodeLifecycle | Hold and release skipped entirely | `TestLifecycle_NotImplementedIsSkipped` |
| UT-19g | Hold failure | HoldTerminating returns error | phase=Degraded | `TestLifecycle_HoldFailureDegrades` |

#### Deletion

| ID | Test Case | Setup | Expected Result | Test |
|:---|:---|:---|:---|:---|
| UT-20 | Delete succeeds with credential failure | Cloud platform returns CredentialError | Finalizer removed (deletion not blocked by stale credentials) | `TestConfigReconcile_DeleteSucceedsWithCredentialFailure` |
| UT-21 | Delete with cloud cleanup | Mock platform, FRRConfiguration exists | Cloud cleanup called, FRRConfigurations deleted, finalizer removed | `TestConfigReconcile_DeleteSuccessful` |

Cleanup releases the finalizer without confirming the deletes landed. On AWS peer deletion is asynchronous, so a delete that fails afterwards has nobody left to retry or report it. That is a known open behaviour, recorded in `GUIDERAILS.md`, and nothing here pins it either way.

---

## E2E Tests

Platform-independent end-to-end tests that validate behavior only a real cluster with a live BGP peer can exercise — BGP session establishment, route advertisement, drift recovery, and cleanup. Unit tests cover the reconciliation logic; E2E tests verify the downstream effect on actual BGP sessions.

Tests read CR manifests from a profile directory (`test/e2e/manifests/<profile>/`). The `CUDNBgpConfig` CR must use `platform: Manual` with explicit `spec.bgp.peerGroups`.

These are still Ginkgo. [docs/e2e-plan.md](e2e-plan.md) is the plan to convert them to standard library `testing` and to fix what the table below does not yet cover; read it before adding to them.

| Component | How discovered |
|:---|:---|
| BGP neighbors, ASN, node selectors | From `CUDNBgpConfig` CR in the profile (`spec.bgp.peerGroups`) |
| Router nodes | Listed from cluster using CR's `routerNodeSelector` |
| BGP session state | `BGPSessionState` CRD (`frrk8s.metallb.io/v1beta1`) |
| FRR running config | `FRRNodeState` CRD (`frrk8s.metallb.io/v1beta1`) |

### Full Stack Reconcile

| ID | Test Case | Action | Verification |
|:---|:---|:---|:---|
| E2E-01 | Full stack reconcile with BGP session verification | Apply `CUDNBgpConfig` CR, create labelled namespace, apply `CUDNBgpRouting` CR | Config phase=`Ready`; one FRRConfiguration per peer group, which under `platform: Manual` is one per entry in `spec.bgp.peerGroups`; routing phase=`Ready` with CUDN + RouteAdvertisements; `BGPSessionState` resources show `Established` for all router nodes; CUDN subnets appear in FRR advertised routes |

### Drift Recovery

| ID | Test Case | Action | Verification |
|:---|:---|:---|:---|
| E2E-02 | FRRConfiguration deleted | Delete the operator-managed FRRConfiguration(s) | Operator recreates FRRConfiguration(s) immediately (watch-triggered); BGP sessions return to `Established` |
| E2E-03 | FRR pod restart | Delete all FRR pods (`-l app=frr-k8s`) | DaemonSet restarts pods; BGP sessions return to `Established` within the hold-timer window |

### Deletion

| ID | Test Case | Action | Verification |
|:---|:---|:---|:---|
| E2E-04 | Deletion cleanup | Delete config CR (blocked by finalizer), then delete routing CR(s) | Config CR finalizer blocks while routing CRs exist. Routing CR deletion: CUDN and shared RouteAdvertisements removed. After routing CRs gone: config CR finalizer removed, FRRConfigurations deleted. BGP sessions drop |

---

## How to Run

```bash
# Unit tests (no cluster required)
make test

# E2E tests (requires cluster with external BGP peer)
# Prerequisites:
# - oc login to OCP 4.21+ cluster with an external BGP peer
# - Operator deployed to the cluster
# - A profile with CUDNBgpConfig using platform: Manual and explicit peerGroups
make test-e2e <profile>
```

Profiles are directories under `test/e2e/manifests/` containing `cudnbgpconfig.yaml` and `cudnbgprouting.yaml`. To test your own cluster, create a profile directory with CRs pointing to your BGP peer and run `make test-e2e <profile-name>`.
