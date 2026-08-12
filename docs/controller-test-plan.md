# Controller Test Plan

Tests for the CUDN BGP Routing Operator controllers and helpers. Unit tests are split into two sections:

- **Basic (platform-independent)** — tests the controller reconciliation logic with explicit `spec.bgp.availabilityZones`, no cloud provider involved.
- **Platform interface** — tests the controller's interaction with the generic `CloudPlatform` interface (Phases 3 and 5: discovery + cloud resource reconciliation). The interface is provider-agnostic; AWS is used as the first concrete mock implementation.

- [Test Configuration](#test-configuration)
- [Unit Tests](#unit-tests)
  - [Basic (platform-independent)](#basic-platform-independent)
  - [Platform interface (AWS as provider)](#platform-interface-aws-as-provider)
- [E2E Tests](#e2e-tests)
- [How to Run](#how-to-run)

---

## Test Configuration

**Basic unit tests** use hardcoded values with explicit `spec.bgp.availabilityZones` (no `spec.aws`):

| Field | Value |
|:---|:---|
| Local BGP ASN | 65001 |
| Remote BGP ASN | 64512 |
| Router node selector | `networking.openshift.io/cudn-bgp-router: ""` |
| Availability Zones | 1 (minimal for unit tests) |
| Neighbor addresses | `10.0.1.47`, `10.0.1.183` |
| CUDN subnets | `10.100.0.0/16` |

**Platform interface tests** use `spec.aws` set with a mocked `CloudPlatform` interface. The mock returns a `DiscoveryResult` with 1 Route Server, 1 endpoint (`rse-001` in `us-east-1a`, address `10.0.1.47`, remote ASN `64512`). No real AWS credentials required.

**E2E tests** read CR manifests from a profile directory (`test/e2e/manifests/<profile>/`). Each profile contains a `cudnbgpconfig.yaml` and `cudnbgprouting.yaml` matching the target cluster. Shared E2E tests require explicit `spec.bgp.availabilityZones` (no `spec.aws`). See the `ocp-or18` profile for an example.

---

## Unit Tests

All unit tests use a fake Kubernetes client. They never invoke real provider code.

---

### Basic (platform-independent)

Tests the controller reconciliation logic without any cloud provider configured.

#### Config Controller

| ID | Test Case | Setup | Expected Result |
|:---|:---|:---|:---|
| UT-01 | Full reconcile (Phases 1-2, 4) | Network CR exists, FRR namespace + pod running, explicit `bgp.availabilityZones` | Network patched, FRRConfigurations created from explicit neighbors, phase=Ready with 3 conditions |
| UT-02 | Delete blocked by routing CRs | CUDNBgpRouting CR exists | Finalizer retained, requeues every 10s |

#### Routing Controller

| ID | Test Case | Setup | Expected Result |
|:---|:---|:---|:---|
| UT-03 | Duplicate network name | Another CUDNBgpRouting claims same spec.network.name | phase=Degraded, reason=DuplicateNetwork |
| UT-04 | Full reconcile | Config Ready, labeled namespace pre-created | CUDN + RouteAdvertisements created, phase=Ready with 2 conditions |
| UT-04b | No labeled namespace | Config Ready, no namespace with required labels | phase=Degraded, reason=NamespaceNotReady |
| UT-05 | Delete last removes RA | No other CUDNBgpRouting CRs | CUDN deleted, RouteAdvertisements deleted, finalizer removed |
| UT-06 | Delete keeps RA when others exist | Another CUDNBgpRouting CR exists | CUDN deleted, RouteAdvertisements retained |

#### Watch Map Functions

| ID | Test Case | Setup | Expected Result |
|:---|:---|:---|:---|
| UT-07 | CUDN watch maps to owning routing CR | Managed CUDN `cluster-udn-prod`, routing CR `prod` exists | Reconcile request for routing CR `prod` |
| UT-08 | Unmanaged CUDN ignored | CUDN without `managed-by` label | No reconcile requests |
| UT-09 | RA watch maps to all routing CRs | Managed RouteAdvertisements, two routing CRs exist | Reconcile requests for both routing CRs |
| UT-10 | Unmanaged RA ignored | RouteAdvertisements without `managed-by` label | No reconcile requests |

#### Helpers

Non-trivial helper logic tested in isolation. Simple CRUD helpers (create, delete) are covered implicitly by the controller tests above.

| ID | Test Case | Verifies |
|:---|:---|:---|
| UT-11 | ValidateNamespaceLabels found | Returns nil when namespace with required labels exists |
| UT-11b | ValidateNamespaceLabels not found | Returns error when no namespace has required labels |
| UT-12 | EnsureFRRConfigurations BFD | BFD profile added when livenessDetection=bfd |
| UT-13 | EnsureFRRConfigurations prunes stale | Stale managed configs deleted when AZ count reduced |
| UT-14 | EnsureFRRConfigurations keeps unmanaged | User-owned FRRConfigurations not pruned |

---

### Platform interface (AWS as provider)

Tests the config controller's interaction with the generic `CloudPlatform` interface.

#### Reconciliation

| ID | Test Case | Setup | Expected Result |
|:---|:---|:---|:---|
| UT-15 | Full reconcile with cloud (Phases 1-5) | spec.aws set with routeServerIDs, mock platform with discovery | AWSEndpointsDiscovered=True, FRRConfigurations created from discovered neighbors, ReconcileNodes called, AWSResourcesReconciled=True, phase=Ready with 5 conditions |
| UT-16 | Phase 3 credential failure | Mock platform builder returns CredentialError | AWSEndpointsDiscovered=False, reason=AWSCredentialsInvalid, phase=Degraded |
| UT-17 | Phase 3 discovery failure | Mock platform discovery returns error | AWSEndpointsDiscovered=False, reason=AWSDiscoveryFailed, phase=Degraded, requeue 30s |
| UT-18 | Phase 5 failure | Mock platform ReconcileNodes returns error | AWSResourcesReconciled=False, reason=AWSReconcileFailed, phase=Degraded, requeue 30s |
| UT-19 | Node filtering | 5 nodes: 3 complete, 1 missing IP, 1 missing AZ | Only 3 RouterNodes passed to ReconcileNodes |

#### Deletion

| ID | Test Case | Setup | Expected Result |
|:---|:---|:---|:---|
| UT-20 | Delete succeeds with credential failure | spec.aws set, mock platform returns CredentialError | Finalizer removed (deletion not blocked by stale credentials) |
| UT-21 | Delete with cloud cleanup | spec.aws set, mock platform, FRRConfiguration exists | AWS cleanup called, FRRConfigurations deleted, finalizer removed |

---

## E2E Tests

Platform-independent end-to-end tests that validate behavior only a real cluster with a live BGP peer can exercise — BGP session establishment, route advertisement, drift recovery, and cleanup. Unit tests cover the reconciliation logic; E2E tests verify the downstream effect on actual BGP sessions.

Tests read CR manifests from a profile directory (`test/e2e/manifests/<profile>/`). The `CUDNBgpConfig` CR must use explicit `spec.bgp.availabilityZones` (no `spec.aws`).

| Component | How discovered |
|:---|:---|
| BGP neighbors, ASN, node selectors | From `CUDNBgpConfig` CR in the profile (`spec.bgp.availabilityZones`) |
| Router nodes | Listed from cluster using CR's `routerNodeSelector` |
| BGP session state | `BGPSessionState` CRD (`frrk8s.metallb.io/v1beta1`) |
| FRR running config | `FRRNodeState` CRD (`frrk8s.metallb.io/v1beta1`) |

### Full Stack Reconcile

| ID | Test Case | Action | Verification |
|:---|:---|:---|:---|
| E2E-01 | Full stack reconcile with BGP session verification | Apply `CUDNBgpConfig` CR, create labeled namespace, apply `CUDNBgpRouting` CR | Config phase=`Ready`; FRRConfigurations created per AZ; routing phase=`Ready` with CUDN + RouteAdvertisements; `BGPSessionState` resources show `Established` for all router nodes; CUDN subnets appear in FRR advertised routes |

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
# - A profile with CUDNBgpConfig using explicit availabilityZones (no spec.aws)
make test-e2e <profile>
```

Profiles are directories under `test/e2e/manifests/` containing `cudnbgpconfig.yaml` and `cudnbgprouting.yaml`. To test your own cluster, create a profile directory with CRs pointing to your BGP peer and run `make test-e2e <profile-name>`.
