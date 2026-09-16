# Design: Azure credentials for bgp-cloud-connector

9 September 2026. Untracked working document, not committed and not for
a pull request. Supersedes AZURE-CREDENTIALS.md.

Status: proposed. Nothing here is implemented.

## Problem

The operator cannot authenticate to Azure when it runs inside a
cluster. `e2e-azure-operator` therefore cannot pass, whatever the shell
side of openshift/bgp-cloud-connector#122 does.

Measured on `amcdermo-2609090810-kxq9j` (IPI, 4.22.12, centralus) on
9 September:

- an ordinary pod cannot reach IMDS. `curl` to
  `169.254.169.254/metadata/instance` returns rc 7, connection failed.
  The same node on the host network returns HTTP 200 and a token, so
  OVN is blocking pod egress to the link-local address rather than IMDS
  being absent.
- the cluster has no `spec.credentialsMode` and no
  `serviceAccountIssuer`, so nothing injects `AZURE_CLIENT_ID` or a
  federated token path either.
- `azidentity.NewDefaultAzureCredential` returns a credential and a nil
  error with nothing available. The failure appears later, at
  `GetToken`, listing all six chain attempts including
  `ManagedIdentityCredential: managed identity timed out`.

So today the operator builds successfully, fails during discovery, and
reports a discovery problem. It also pays the IMDS timeout on every
reconcile.

`buildAzurePlatform` calls `azureplatform.New` directly, with no
counterpart to `awsplatform.ResolveCredentials`, and there is no
`CredentialsRequest` for Azure anywhere in the tree.

## Goals

1. The operator authenticates in a cluster that mints (IPI, our prow
   job, QE clusters).
2. It authenticates in a cluster that federates, without a second code
   path.
3. Running a manager from a desk against a cluster keeps working
   unchanged, on an ordinary `az login`.
4. A credentials failure is reported as a credentials failure.

## Non-goals

- ARO and ARO HCP. Sketched at the end so the shape is not a surprise,
  but not built.
- Changing anything about how the operator reconciles. That path is
  proven: six of six BGP sessions Established on 9 September, driven by
  the operator built from #122's branch.
- Changing `spec.azure.networkInterfaceClientID`. It solves a different
  problem, which is interfaces in a resource group the operator's own
  identity cannot write to.

## Where the e2e has to just work

These are the acceptance criteria, not background. A design that serves
three of the four is not done.

**1. A minted cluster you stand up, operator installed from its
bundle.** The QE account, an ordinary `openshift-install` cluster in
CCO's default mode. CCO serves our `CredentialsRequest` by minting a
service principal and writing a secret carrying
`azure_client_secret`. This is the case that is completely broken
today, and the one everything else follows from.

**2. A minted cluster you stand up, operator run from your desk.**
Same cluster, manager built locally and pointed at it with
`--kubeconfig`. Credentials come from your own `az login` through
azidentity's `AzureCLICredential`. **This works today** and the design
must not break it: it is how Azure was proven on 14 and 21 August and
again on 9 September, and it is the fastest loop for developing the
suite. The resolver therefore asks the SDK first and only falls back to
the cluster.

**3. Prow, IPI.** `e2e-azure-operator`, which openshift/release#84758
already merged, on the `ipi-azure` workflow. Identical to case 1 in
every respect that matters: `ipi-azure` leaves CCO in its default mode,
so the same minting path serves it. 40 distinct non-`openshift-priv`
repositories use that workflow, so we are following the herd rather
than inventing.

**4. ARO, later, via openshift/release.** Not built here. ARO HCP does
not use `CredentialsRequest` at all: the service supplies a user
assigned identity credentials file, read with
`dataplane.NewUserAssignedIdentityCredential`, which sits outside
azidentity's chain and needs an explicit branch. There is no ARO
workflow in the step registry we can consume either -- every one
belongs to a product team -- so it is a cluster profile and a
conversation as much as it is code. The design below is additive with
respect to it: the branch goes beside the chain rather than through it,
which is exactly how `cluster-ingress-operator` is arranged.

A fifth exists and is worth naming so it is not confused with 4: a
self-managed cluster installed with `credentialsMode: Manual` and an
OIDC issuer, where `ccoctl` registers a federated credential and the
secret carries `azure_federated_token_file` instead of a client
secret. That is federation without ARO, it is what the
`ipi-azure-pre-manual-oidc-workload-identity` chain builds, and
eighteen repositories already run a job on it. The design serves it
with no extra code, which is the main argument for the approach in
section 3.

## Prior art, and what it argues for

Counted on 9 September against clones fast-forwarded to 4-9 September.

**We are an OLM operator, not in the payload.** Seven operators ship a
static `CredentialsRequest` under `manifests/` and rely on CVO to apply
it: `cluster-capi-operator`,
`cluster-cloud-controller-manager-operator`,
`cluster-image-registry-operator`, `cluster-ingress-operator`,
`cluster-network-operator`, `cluster-storage-operator` and
`machine-api-operator`. That route is not open to us: nothing applies
an OLM operator's `manifests/`.

Seven apply one themselves at runtime: `loki`,
`aws-load-balancer-operator`, `grafana-tempo-operator`,
`kedacore-keda`, `external-dns-operator`, `managed-velero-operator` and
`sandboxed-containers-operator`. This is our family, and our AWS path
already sits in it.

**How the herd turns a secret into a credential.** Across all
non-vendor Go in the organisation:

```
262  azidentity.NewDefaultAzureCredential
 81  azidentity.NewClientSecretCredential
 62  azidentity.NewClientCertificateCredential
 47  azidentity.NewWorkloadIdentityCredential
 46  azidentity.NewManagedIdentityCredential
```

`loki` is the closest match to us: OLM, Azure, applies its own request,
and rather than parsing the secret it sets `AZURE_CLIENT_ID`,
`AZURE_TENANT_ID`, `AZURE_SUBSCRIPTION_ID` and
`AZURE_FEDERATED_TOKEN_FILE` and lets the chain decide
(`operator/internal/manifests/storage/configure.go:236-238`).

`cluster-ingress-operator` builds credentials explicitly, but it has a
case the chain cannot express: ARO HCP, where it uses
`dataplane.NewUserAssignedIdentityCredential` against a file the
service provides. That is a real reason and it is not ours yet.

## Design

### 1. Ask the cluster for credentials

A new `internal/platform/azure/credentials.go`, mirroring the AWS one
in structure:

- ambient first. Build the default chain and retrieve a token. If that
  works, use it and leave the cluster alone. This is what keeps the
  out-of-cluster case working and what would pick up a future ARO
  injection.
- otherwise reconcile a `CredentialsRequest` named
  `bgp-cloud-connector-azure` in `openshift-cloud-credential-operator`,
  with `secretRef` pointing at the operator's own namespace and
  `serviceAccountNames` naming the manager's service account.
- read the secret. Absent means `platform.ErrCredentialsPending`, which
  the controller already turns into `WaitingForCloudCredentials` with a
  requeue.

Reconcile rather than create-once, for the same reason the AWS one
does: on a federating cluster the request needs `cloudTokenPath`, and
that arrives after the operator first asked.

### 2. The permissions to request

`AzureProviderSpec` takes a `permissions` list. The operator's calls,
and nothing else:

| permission | call |
|:---|:---|
| `Microsoft.Network/virtualHubs/read` | `hubs.Get`, `client.go:96` |
| `Microsoft.Network/virtualHubs/bgpConnections/read` | `routeserver.go:93` |
| `Microsoft.Network/virtualHubs/bgpConnections/write` | `routeserver.go:47` |
| `Microsoft.Network/virtualHubs/bgpConnections/delete` | `routeserver.go:52` |
| `Microsoft.Network/networkInterfaces/read` | `client.go:152,183` |
| `Microsoft.Network/networkInterfaces/write` | `client.go:33` |

No compute permission. The operator never calls a compute API: it
parses the VM out of `providerID` with a regex (`providerid.go:9`) and
matches interfaces on `Properties.VirtualMachine.ID`.
`openshift-cloud-network-config-controller-azure` does ask for
`Microsoft.Compute/virtualMachines/read`, and copying its list would
over-grant.

**Open question, worth settling in review.** Assemble the request in Go,
as our AWS path does, or ship it as YAML in the image and apply it, as
`sandboxed-containers-operator` does from
`config/peerpods/credentials-requests/`? The YAML keeps the permissions
diffable in a pull request rather than buried in a Go literal, which
for a permission list is worth something. Against it: it would sit
differently from the AWS path in the same repository, and it needs the
file to be present in the image. My inclination is Go for consistency,
but I do not feel strongly.

### 3. Turning the secret into a credential

Set environment variables from the secret and let azidentity choose,
rather than choosing ourselves.

| secret key | variable |
|:---|:---|
| `azure_client_id` | `AZURE_CLIENT_ID` |
| `azure_tenant_id` | `AZURE_TENANT_ID` |
| `azure_client_secret` | `AZURE_CLIENT_SECRET` |
| `azure_federated_token_file` | `AZURE_FEDERATED_TOKEN_FILE` |

A minting cluster supplies the first three and the chain resolves
`EnvironmentCredential`. A federating cluster supplies the first two
and the fourth and the chain resolves `WorkloadIdentityCredential`.
Nothing set at all, and it falls through to `AzureCLICredential`, which
is the desk case. One mechanism, three environments, no mode detection
of our own.

Verified rather than assumed: `ccoctl`'s `secretManifestTemplate`
(`cloud-credential-operator/pkg/cmd/provisioning/azure/create_managed_identities.go:35-45`)
writes `azure_client_id`, `azure_tenant_id`, `azure_region`,
`azure_subscription_id` and `azure_federated_token_file`, and no
`azure_client_secret`. That absence is exactly what the chain keys on.

Setting process environment from a reconcile is not free of hazards. It
is process-global and the manager is concurrent, so it must be done
once, before any client is built, and not rewritten per reconcile
unless the values changed. The alternative, projecting the secret into
the deployment with `secretKeyRef` as loki does, is rejected: the
kubelet resolves those when the pod starts, and CCO writes the secret
after the operator is already running, so the operator would have to
restart itself to see it.

### 4. Report the failure at the right layer

`NewDefaultAzureCredential` succeeds with nothing available, so the
resolver must call `GetToken` once, during construction, and classify:

- token retrieved: proceed.
- no secret yet: `platform.ErrCredentialsPending`.
- secret present but the token is refused:
  `platform.CredentialError`, which the controller reports as
  `CloudCredentialsInvalid`.

The controller needs no change. It already handles both
(`bgpcloudconfiguration_controller.go:177-202`).

### 5. The CSV

`features.operators.openshift.io/token-auth-azure` is `"false"` in
`bundle/manifests/...clusterserviceversion.yaml:58` and
`config/manifests/bases/...:49`. It flips with this work, not
separately: it is what makes the console offer the workload identity
fields and OLM set them on the Subscription.

The deployment already projects a service account token with
`audience: openshift` for the AWS federated path
(`config/manager/manager.yaml:127-132`). `ccoctl azure` registers the
federated credential with audience `openshift`
(`create_managed_identities.go:550-557`), so the existing volume is
already what Azure federation wants.

## How this gets tested

1. Unit, with a fake client: request created when the pod has nothing;
   not created when it has something; pending when the secret is
   absent; environment set correctly for each of the two secret shapes.
   The AWS `credentials_test.go` is the template.
2. A cheap probe on a real cluster, before any estate exists. Apply a
   `BGPCloudConfiguration` naming a `routeServerName` nobody created
   and read the condition: `CloudCredentialsInvalid` means the identity
   is wrong, anything else means Azure accepted us and discovery is
   what failed, which is the expected answer for a name that does not
   exist. This is what the ROSA job does for AWS, and it is much
   cheaper than standing up a Route Server to find out.
3. Case 2 by hand, which is the cheapest regression test that the
   ambient path still works: build the manager, run it against the QE
   cluster with a kubeconfig and an `az login`, and confirm it still
   reconciles without asking the cluster for anything.
4. Case 1 on a QE cluster: install from the bundle, and watch the
   `CredentialsRequest` appear and a secret follow.
5. Case 3, `e2e-azure-operator` on `ipi-azure`, which is case 1 in CI.
6. The federated case afterwards, as a second job on the
   `ipi-azure-pre-manual-oidc-workload-identity` chain.

## Order

Credentials before the e2e suite. The suite cannot go green without
them, and writing it against an operator that cannot authenticate means
debugging two things at once.

## Settled by experiment, 2026-09-10

**CCO serves a runtime-created Azure CredentialsRequest.** This was the
assumption everything else rested on, and I could find no precedent for
it anywhere in the organisation, every static Azure example being
in-payload. Tested on `amcdermo-2609100810-mknz5`, a fresh IPI cluster:
created the namespace, applied a `CredentialsRequest` naming
`AzureProviderSpec` with the six permissions above, and CCO reported
`provisioned=true` within twenty seconds and wrote the secret.

The keys are exactly what was predicted from reading
`cloud-network-config-controller`'s:

```
azure_client_id  azure_client_secret  azure_region
azure_resource_prefix  azure_resourcegroup
azure_subscription_id  azure_tenant_id
```

**And the credential works.** Logged in with it as a service principal
in an isolated AZURE_CONFIG_DIR, it listed route servers and the six
network interfaces in the cluster's resource group -- the reads
discovery and forwarding actually make.

**The mode matters and is worth recording.** This cluster resolves to
`passthrough`, which the root secret's
`cloudcredential.openshift.io/mode` annotation states. Passthrough
copies the installer's own credentials rather than minting a scoped
one, so the `permissions` list is not what grants access here: the
credential has whatever the installer had. That is fine for CI and for
a developer cluster, and it means the permission list is doing
documentation and validation work rather than restriction. On a cluster
that mints or federates it would restrict, so the list still has to be
right.

## Still unverified
- The federating secret shape is confirmed from `ccoctl`'s generator
  rather than from a live cluster. Confirming it properly needs a
  `credentialsMode: Manual` cluster built with `ccoctl`, about 45
  minutes.

## ARO, when it comes

ARO HCP does not use `CredentialsRequest`. The service provides a user
assigned identity credentials file and the operator reads it with
`dataplane.NewUserAssignedIdentityCredential`, which sits outside
azidentity's default chain and so needs an explicit branch, exactly as
`cluster-ingress-operator` has. Additive to everything above, and it is
where `networkInterfaceClientID` finally earns its keep.
