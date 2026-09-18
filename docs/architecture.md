# Architecture

The overall solution (e.g. AWS) has two layers. The operator manages the in-cluster layer and, when platform integration is configured, also discovers cloud BGP infrastructure and reconciles cloud-side networking resources.

```text
┌──────────────────────────────────────────────────────────────────────┐
│  Cloud Infrastructure (Terraform provisions once)                    │
│                                                                      │
│  VPC / subnets / route server / TGW / etc.                           │
│  Machine pools labeled bgp_router: "true" (Terraform input)          │
│  Terraform outputs: Route Server IDs, local BGP ASN, AWS region      │
└──────────────────────────────────┬───────────────────────────────────┘
                                   │ user copies terraform outputs
                                   │ (RS IDs, ASN) into CR spec
                                   ▼
┌──────────────────────────────────────────────────────────────────────┐
│  In-Cluster (Operator)                                               │
│                                                                      │
│  BGPCloudConfiguration CR (singleton — BGP infra)                    │
│  ├── Patch Network.operator.openshift.io (enable FRR)                │
│  ├── [if platform configured] Discover RS endpoints, neighbor IPs,   │
│  │   remote ASN, and AZ mapping via cloud API                        │
│  ├── FRRConfiguration per peer group (BGP sessions to its peers)     │
│  └── [if platform configured] Reconcile cloud networking on          │
│       node changes (Route Server peers, SourceDestCheck, etc.)       │
│                                                                      │
│  BGPRouting CR (one per network)                                     │
│  ├── ClusterUserDefinedNetwork (targets user-labeled namespaces)     │
│  └── Shared RouteAdvertisements (all CUDNs with advertise=true)      │
└──────────────────────────────────────────────────────────────────────┘
```

## Separation of concerns

Two CRDs with a clear ownership split:

- **`BGPCloudConfiguration`** (singleton, cluster-scoped, **must be named `cluster`**) — shared BGP infrastructure. Owned by the cluster admin.
- **`BGPRouting`** (one per network, cluster-scoped) — declares a single network to advertise via BGP. Owned by application teams.

See [custom-resources.md](custom-resources.md) for the full CRD reference and [reconciliation.md](reconciliation.md) for how the two controllers drive them.

## Related docs

- [Cloud platform integration](cloud-integration.md) — how the operator peers with a cloud BGP service and keeps it current for node changes.
- [AWS authentication](aws-authentication.md) — how the operator obtains its AWS credentials.
