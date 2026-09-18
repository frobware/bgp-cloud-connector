# Azure authentication

The operator needs Azure credentials to read the Route Server (Virtual Hub) and reconcile router-node network interfaces (see [cloud-integration.md](cloud-integration.md)). Azure authentication works **differently from [AWS](aws-authentication.md)**: the operator does not provision or request credentials, and there is no Cloud Credential Operator (CCO) path. It relies entirely on an **ambient identity supplied to the pod** — you must set that up out of band.

## How the operator authenticates

Route Server calls and the default network-interface path build the SDK's default credential chain, `azidentity.NewDefaultAzureCredential` (`internal/platform/azure/routeserver.go`, `internal/platform/azure/client.go`); when `spec.azure.networkInterfaceClientID` is configured, network-interface calls instead use `azidentity.NewWorkloadIdentityCredential` (see [Cross-resource-group identity (ARO)](#cross-resource-group-identity-aro) below). The operator reads no auth environment variable itself — the chain resolves whatever the cluster's **Azure Workload Identity** setup has projected into the pod (`AZURE_TENANT_ID`, `AZURE_FEDERATED_TOKEN_FILE`, `AZURE_AUTHORITY_HOST`).

There is no `CredentialsRequest`, no `ROLEARN`, and the CSV declares `features.operators.openshift.io/token-auth-azure: "false"` — so the OperatorHub console does **not** drive an Azure credential flow. Credentials must already be resolvable when the operator starts.

## Setup

1. **Configure Azure Workload Identity for the operator's ServiceAccount** so the workload-identity webhook injects a federated token into the pod. This is the standard OpenShift-on-Azure workload-identity mechanism: the operator's ServiceAccount (`openshift-bgp-cloud-connector-controller-manager`) is associated with a managed identity, and that identity carries a federated credential trusting the ServiceAccount. See the [Azure Workload Identity](https://azure.github.io/azure-workload-identity/docs/) documentation for the exact annotations and federation setup.

2. **Grant the identity the permissions the operator needs** on the resource group holding the Route Server (Virtual Hub) and the router nodes' network interfaces: read the Virtual Hub and its BGP connections, create/delete BGP connections, and enable IP forwarding on the router-node NICs.

## Cross-resource-group identity (ARO)

A router node's network interface and the Route Server can sit in resource groups reachable by **different** identities. On ARO, the interfaces live in a resource group the cluster does not own, and the identity with write access there is the cluster's own machine-api identity, which cannot be granted to this operator.

For that case, set `spec.azure.networkInterfaceClientID` to the client ID of the managed identity that can write the interfaces. The operator then uses `azidentity.NewWorkloadIdentityCredential` with that client ID for **network interface calls only** (`internal/platform/azure/client.go`); Route Server calls continue to use the identity the operator otherwise runs as.

The identity named there must carry a federated credential trusting the operator's ServiceAccount. Leaving `networkInterfaceClientID` unset means one identity reaches both the Route Server and the interfaces — the case wherever the cluster owns the resource group holding its own interfaces.

See [custom-resources.md](custom-resources.md) for the full `spec.azure` field reference.

## Troubleshooting

The operator reports credential state through the `CloudEndpointsDiscovered` condition on `BGPCloudConfiguration`; a client-construction or auth failure surfaces as `CloudDiscoveryFailed`. Because Azure credentials are ambient, a failure here almost always means the workload-identity setup is incomplete or the identity lacks permissions — verify the federated credential trusts the operator's ServiceAccount and that the identity has access to the resource group. Inspect the live conditions:

```bash
oc get bgpcloudconfiguration cluster -o jsonpath='{.status.conditions}' | jq .
```
