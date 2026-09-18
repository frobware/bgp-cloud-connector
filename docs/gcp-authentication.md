# GCP authentication

The operator needs GCP credentials to read the Cloud Router and reconcile Network Connectivity Center (NCC) spokes and router-node forwarding (see [cloud-integration.md](cloud-integration.md)). GCP authentication works **differently from [AWS](aws-authentication.md)**: the operator does not provision or request credentials, and there is no Cloud Credential Operator (CCO) path. It relies entirely on an **ambient identity supplied to the pod** — you must set that up out of band.

## How the operator authenticates

Both GCP clients — Compute and Network Connectivity Center — are built from **Application Default Credentials (ADC)** with the `cloud-platform` scope (`compute.NewService` in `internal/platform/gcp/compute.go`, `networkconnectivity.NewService` in `internal/platform/gcp/ncc.go`). The operator reads no auth environment variable itself; ADC resolves whatever the cluster has provided to the pod — a Workload Identity Federation token, or a credentials file referenced by `GOOGLE_APPLICATION_CREDENTIALS`.

There is no `CredentialsRequest`, no `ROLEARN`, no workload-identity branch in operator code, and the CSV declares `features.operators.openshift.io/token-auth-gcp: "false"` — so the OperatorHub console does **not** drive a GCP credential flow. Credentials must already be resolvable when the operator starts.

## Setup

1. **Give the operator's ServiceAccount an ADC-resolvable identity.** The standard mechanism on OpenShift-on-GCP is **Workload Identity Federation**: bind the operator's ServiceAccount (`openshift-bgp-cloud-connector-controller-manager`) to a GCP service account so the pod receives a federated token ADC can use. See the [GCP Workload Identity Federation](https://cloud.google.com/iam/docs/workload-identity-federation) documentation for the binding setup. Alternatively, mount a service-account key and set `GOOGLE_APPLICATION_CREDENTIALS`.

2. **Grant that GCP service account the permissions the operator needs:**
   - **Compute** — read the Cloud Router (its interface addresses become the BGP neighbors), read router-node instances, and set `canIpForward` on them.
   - **Network Connectivity Center** — manage the router-appliance spokes under the configured hub (a node cannot peer with a Cloud Router until it belongs to an NCC spoke).

See [custom-resources.md](custom-resources.md) for the full `spec.gcp` field reference (project, region, Cloud Router, NCC hub/spoke, nested virtualization).

## Troubleshooting

The operator reports credential state through the `CloudEndpointsDiscovered` condition on `BGPCloudConfiguration`; a client-construction or auth failure surfaces as `CloudDiscoveryFailed`. Because GCP credentials are ambient, a failure here almost always means ADC could not resolve an identity or the identity lacks permissions — verify the Workload Identity Federation binding (or the mounted key) and the IAM roles on the GCP service account. Inspect the live conditions:

```bash
oc get bgpcloudconfiguration cluster -o jsonpath='{.status.conditions}' | jq .
```
