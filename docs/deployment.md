# Development and deployment

## Prerequisites

- Go 1.24+ installed locally. Some of the build happens outside of a container.
- operator-sdk v1.42+
- `oc` CLI logged into an OCP 4.21+ cluster
- Podman (for image builds only)

## Clone the repo

```bash
git clone https://github.com/openshift/bgp-cloud-connector.git
cd bgp-cloud-connector
```

## Deploy and test

The operator can be tested on any OCP 4.21+ cluster with an external BGP router — with or without cloud integration.

1. Ensure the internal image registry is enabled and exposed.

   Check whether it is currently enabled (by default, it is enabled on ROSA):

```bash
oc get configs.imageregistry.operator.openshift.io cluster -o jsonpath='{.spec.managementState}'
```

   If it returns `Removed` (common on compact/SNO deployments), enable it:

```bash
oc patch configs.imageregistry.operator.openshift.io cluster --type merge -p '{"spec":{"managementState":"Managed","storage":{"emptyDir":{}}}}'
oc wait --for=condition=Available configs.imageregistry.operator.openshift.io cluster --timeout=120s
```

   > **Note:** `storage.emptyDir` is **ephemeral** — the registry's image layers live only in the registry pod and are lost whenever that pod is replaced (restart, reschedule, node drain). This is fine for a throwaway build-and-test loop, but if you replace the registry pod you must rebuild and push the image again. For a registry you intend to reuse, configure persistent storage (e.g. a PVC) instead of `emptyDir`.

   Check whether it currently is exposing a default route to outside of the cluster (by default, it is not on ROSA):

```bash
oc patch configs.imageregistry.operator.openshift.io cluster --type merge -p '{"spec":{"defaultRoute":true}}'
oc wait route/default-route -n openshift-image-registry --for=jsonpath='{.status.ingress[0].conditions[0].status}'=True --timeout=120s
```

2. Build the image:

```bash
REGISTRY=$(oc get route default-route -n openshift-image-registry -o jsonpath='{.spec.host}')
IMG=$REGISTRY/openshift-bgp-cloud-connector/operator:dev
make image-build CONTAINER_TOOL=podman IMG=$IMG
```

3. Push the image and deploy:

```bash
oc create namespace openshift-bgp-cloud-connector --dry-run=client -o yaml | oc apply -f -
oc create sa registry-push -n openshift-bgp-cloud-connector 2>/dev/null
oc adm policy add-role-to-user registry-editor -z registry-push -n openshift-bgp-cloud-connector
# Trust the registry's serving certificate so TLS verification stays enabled. The default
# route is served by the cluster ingress cert; export its CA into Podman's per-registry
# trust dir (rootless path shown). If you use a custom ingress cert, trust that CA instead.
mkdir -p ~/.config/containers/certs.d/$REGISTRY
oc get secret router-ca -n openshift-ingress-operator -o jsonpath='{.data.tls\.crt}' \
  | base64 -d > ~/.config/containers/certs.d/$REGISTRY/ca.crt
# Log in with the token piped via stdin so it never appears in the process arguments.
oc create token registry-push -n openshift-bgp-cloud-connector \
  | podman login $REGISTRY -u unused --password-stdin
make image-push CONTAINER_TOOL=podman IMG=$IMG
make deploy IMG=image-registry.openshift-image-registry.svc:5000/openshift-bgp-cloud-connector/operator:dev
```

4. Re-deploy after code changes (rebuild, push, and restart):

```bash
make image-build CONTAINER_TOOL=podman IMG=$IMG
make image-push CONTAINER_TOOL=podman IMG=$IMG
oc patch deployment openshift-bgp-cloud-connector-controller-manager -n openshift-bgp-cloud-connector \
  -p '{"spec":{"template":{"spec":{"containers":[{"name":"manager","imagePullPolicy":"Always"}]}}}}'
oc rollout restart deployment/openshift-bgp-cloud-connector-controller-manager -n openshift-bgp-cloud-connector
```

5. Create the CRs for your environment:

   **For AWS (e.g. ROSA HCP):** provision AWS infrastructure first with [rosa-bgp Terraform](https://github.com/msemanrh/rosa-bgp), set up AWS credentials (see [AWS authentication](aws-authentication.md) — the recommended CCO path is `ROLEARN`, or nothing on mint-mode IPI), then create the CR with Route Server IDs and BGP ASN from `terraform output`. The operator auto-discovers all Route Server endpoints, neighbor IPs, and remote ASN:

   ```bash
   $EDITOR config/samples/networking_v1beta1_bgpcloudconfiguration.yaml # Add needed Terraform outputs
   oc apply -f config/samples/networking_v1beta1_bgpcloudconfiguration.yaml
   ```

   **For Azure or GCP:** set `spec.platform` to `Azure` or `GCP` with the matching `spec.azure` / `spec.gcp` block (see [Custom resources](custom-resources.md)) and set up credentials — [Azure authentication](azure-authentication.md) / [GCP authentication](gcp-authentication.md). The operator auto-discovers neighbours and remote ASN the same way; peering is a single region-wide group rather than per-AZ.

   **Without cloud integration:** create the CRs with your BGP router's ASN, neighbor addresses, and node selectors. Set `platform: Manual`, omit the `spec.aws` section and provide explicit `spec.bgp.peerGroups`. See the commented-out section in `config/samples/networking_v1beta1_bgpcloudconfiguration.yaml` for an example:

   ```bash
   oc apply -f your-bgpcloudconfiguration.yaml  # platform: Manual, explicit peerGroups
   ```

   Then create a labeled namespace:

```bash
cat <<EOF | oc apply -f -
apiVersion: v1
kind: Namespace
metadata:
  name: app1
  labels:
    k8s.ovn.org/primary-user-defined-network: ""
    cluster-udn: prod
EOF
```
   Finally apply the routing CR:

```bash
oc apply -f config/samples/networking_v1beta1_bgprouting.yaml
```

6. Verify:

```bash
oc get bgpcloudconfiguration cluster -o yaml   # phase: Ready
oc get bgprouting cudn1 -o yaml                # phase: Ready
oc get frrconfiguration -n openshift-frr-k8s
oc get clusteruserdefinednetwork
oc get routeadvertisements
```

7. Clean up (delete CRs first so finalizers can clean up cloud resources and FRRConfigurations):

```bash
oc delete bgprouting --all
oc delete bgpcloudconfiguration cluster
make undeploy
```

## Deploy via OLM bundle

Use this workflow to test the operator as it would be installed from OperatorHub, using a bundle image.
This requires an external image registry (e.g. `quay.io`) that the cluster can pull from, and a kubeconfig pointing at the target cluster.

```bash
make images bundle-deploy \
  IMG=<registry>/<repository>/bgp-cloud-connector:v<x.y.z> \
  BUNDLE_IMG=<registry>/<repository>/bgp-cloud-connector-bundle:v<x.y.z>
```

To remove the operator and all OLM resources it created:

> **Note:** Delete all CRs before, and wait for them to be fully removed to ensure any external resources managed by the operator are cleaned up.

```bash
make bundle-clean
```

## OLM / OperatorHub packaging

| Field | Value |
|:---|:---|
| Package name | `bgp-cloud-connector` |
| Default channel | `alpha` (moves to `stable` at GA) |
| Install modes | OwnNamespace, SingleNamespace |
| Target namespace | `openshift-bgp-cloud-connector` |
| Min OCP version | 4.21 (frr-k8s + CUDN + RouteAdvertisements) |
| Categories | Networking |
| Provider | Red Hat |

**Required APIs:** `FRRConfiguration` (frrk8s.metallb.io/v1beta1), `ClusterUserDefinedNetwork` (k8s.ovn.org/v1), `RouteAdvertisements` (k8s.ovn.org/v1)
**Owned APIs:** `BGPCloudConfiguration`, `BGPRouting` (networking.openshift.io/v1beta1)

> The operator constructs `ClusterUserDefinedNetwork`, `RouteAdvertisements`, and `FRRConfiguration` objects using `unstructured.Unstructured` rather than importing typed Go structs. The typed structs for CUDN and RouteAdvertisements live inside the monolithic `github.com/ovn-kubernetes/ovn-kubernetes/go-controller` module, which carries 100+ transitive dependencies (CNI plugins, netlink, kubevirt, the full `k8s.io/kubernetes` repo, etc.). Even `openshift/cluster-network-operator` avoids importing it for the same reason.

## Testing

See [test-strategy.md](test-strategy.md) for the full test strategy and the per-area test plans, and [kubevirt-testing.md](kubevirt-testing.md) for KubeVirt VM testing.
