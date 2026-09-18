# Manual platform (bring your own BGP)

`platform: Manual` reconciles no cloud. Instead of auto-discovering the peering from a cloud Route Server / Cloud Router, you declare it explicitly in `spec.bgp.peerGroups`. It is the on-ramp for anything that is not one of the supported clouds ([AWS](cloud-integration.md#aws-platform), [Azure](cloud-integration.md#azure-platform), [GCP](cloud-integration.md#gcp-platform)).

## When to use it

- **You run your own BGP router or route reflector** — on-prem, a lab, or a cloud this operator does not yet integrate with.
- **You want to exercise the in-cluster behaviour** (FRR enablement, CUDN, RouteAdvertisements, BGP session establishment) **without cloud credentials** — this is the mode the shared E2E tests use. See [deployment.md](deployment.md) and [test-strategy.md](test-strategy.md).
- It mirrors the manual in-cluster steps of the [rosa-bgp PoC](https://github.com/msemanrh/rosa-bgp).

## What you supply

There are no credentials and no cloud API calls. You provide everything the cloud path would otherwise discover:

- `spec.routerNodeSelector` — labels selecting the BGP-enabled worker nodes.
- `spec.bgp.localASN` — the AS number of your OCP FRR routers.
- `spec.bgp.peerGroups[]` — one group per set of nodes that share a neighbour set. Each group has a `nodeSelector` and an explicit `neighbors[]` list (`address`, `remoteASN`, and optional `ebgpMultiHop`).

```yaml
apiVersion: networking.openshift.io/v1beta1
kind: BGPCloudConfiguration
metadata:
  name: cluster
spec:
  platform: Manual                   # no cloud reconciliation; peering declared below
  routerNodeSelector:
    bgp_router: "true"               # selects the BGP-enabled worker nodes
  bgp:
    localASN: 65001                  # your OCP FRR routers' ASN
    livenessDetection: bgp-keepalive # bfd | bgp-keepalive (default)
    peerGroups:
      - nodeSelector:                # narrows routerNodeSelector to this group's nodes
          topology.kubernetes.io/zone: us-east-1a
        neighbors:
          - address: 10.0.1.10       # your BGP router / route reflector
            remoteASN: 64512
          - address: 10.0.1.11
            remoteASN: 64512
            ebgpMultiHop: true       # set when the neighbour is not on the node's link
```

See [custom-resources.md](custom-resources.md#bgpcloudconfiguration-singleton--without-cloud-integration) for a multi-group example and the full field reference.

## How it maps

Each `peerGroup` becomes **one `FRRConfiguration`**, whose `nodeSelector` is `routerNodeSelector` merged with the group's `nodeSelector`. There is no discovery and no cloud reconciliation — Phases 3 and 5 are skipped entirely (see [reconciliation.md](reconciliation.md#cloud-platform-abstraction)). The generated `FRRConfiguration`s are identical to those produced on a cloud; only the source of the input data differs (explicit `peerGroups` here vs discovery there).

## Notes

- **`ebgpMultiHop`** — set this per neighbour when the peer is not on the node's link (e.g. a route reflector reachable over multiple hops). On the cloud platforms the operator sets it automatically where the Route Server / Cloud Router is off-link (Azure and GCP); under `Manual` you set it yourself when needed.
- **`livenessDetection`** (`bfd` | `bgp-keepalive`, default `bgp-keepalive`) applies to all neighbours, the same as on a cloud.
- `BGPRouting` CRs and the resources they generate (CUDN, RouteAdvertisements) are identical regardless of platform — nothing changes on the routing side under `Manual`.
