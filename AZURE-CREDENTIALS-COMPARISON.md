# Do we do what other operators do for Azure credentials?

10 September 2026. Untracked working document.

Short answer: no, in three ways, and two of them we should change.

## Method

Compared against seven OpenShift operators that authenticate to Azure
themselves, all fast-forwarded to their upstream default branch today
except `azure-disk-csi-driver-operator` (2024) and
`cluster-api-provider-azure` (July): `cluster-ingress-operator`,
`cloud-network-config-controller`, `cluster-image-registry-operator`,
`machine-api-provider-azure`, `cluster-capi-operator`, `csi-operator`,
`cluster-storage-operator`. Searches excluded nested `vendor/` and
`_test.go`.

## What the herd does

**Names the credential type explicitly, from the secret's shape.** Four
of four that authenticate in a reconcile path do this, and three of the
four handle the same three cases in the same order:

| operator | constructors used |
|:---|:---|
| cluster-ingress-operator | ClientSecret, WorkloadIdentity, dataplane UserAssignedIdentity |
| cloud-network-config-controller | ClientSecret, WorkloadIdentity, dataplane UserAssignedIdentity |
| cluster-image-registry-operator | ClientSecret, WorkloadIdentity, dataplane UserAssignedIdentity |
| machine-api-provider-azure | ClientSecret, WorkloadIdentity |

The decision is a plain if/else on the secret's contents: a user
assigned identity credentials file wins if present, else workload
identity when the client secret is empty and a token file is set, else
client secret. `cloud-network-config-controller/pkg/cloudprovider/azure.go:595-645`
and `cluster-ingress-operator/pkg/dns/azure/client/auth.go:56-95` are
the same shape.

**Nobody uses the DefaultAzureCredential chain in a reconcile path.**
The single hit is `cluster-image-registry-operator/cmd/move-blobs`, a
standalone one-shot migration CLI, where a chain is entirely
reasonable.

**Nobody sets AZURE_* process environment.** Zero hits for
`Setenv("AZURE_` across all seven.

**Nobody asks for a token to validate.** Zero hits for `GetToken(ctx`.

**Rotation is handled, and ingress does it best.** It watches the
Secret with a predicate that fires only when `Data` changes, caches the
provider alongside the credentials it was built from, and rebuilds only
on a `reflect.DeepEqual` difference
(`pkg/operator/controller/dns/controller.go:105-118, 232-288`).
`cloud-network-config-controller` reads its secret from a mounted
volume with `os.ReadFile` at start-up instead, so it needs a restart to
notice a change.

## Where we diverge, and whether we should

### 1. The chain rather than a named type. Pre-existing; should change.

`NewTopologyReader`, `NewRouteServerBackend` and `NewNICClient` all
call `azidentity.NewDefaultAzureCredential`. That predates this work.
It has three costs.

It is opaque: reading the code does not tell you which credential is in
use, only that azidentity will decide.

It is slow in the one place it matters. The chain reaches
`ManagedIdentityCredential`, which probes IMDS, and IMDS is
unreachable from an OpenShift pod -- measured 9 September, `curl` rc 7
from a pod against HTTP 200 on the node. So every chain walk that gets
that far pays a timeout.

And it sits badly with a feature we already have.
`spec.azure.networkInterfaceClientID` exists so network interface calls
can use a *different* managed identity from the Route Server calls.
That is two identities in one process, which is exactly what an
explicit per-client credential expresses and what a process-wide chain
does not.

### 2. Setting process environment. Mine, from yesterday. Should change.

I argued for it from `loki`, and I misread `loki`. Its
`envVarFromSecret` builds a `corev1.EnvVar` with a `SecretKeyRef`
(`operator/internal/manifests/storage/configure.go:320-330`): it is
composing the environment of the Loki workload it manages, for the
kubelet to resolve. It never calls `os.Setenv` on itself. That is a
different thing from mutating your own process, and it is not
precedent for what I did.

The cost is not theoretical. It produced a real bug within an hour:
`ResolveCredentials` probed ambient first, the probe read back the
environment the previous reconcile had written, succeeded, and returned
early -- so the secret was never re-read and a rotated credential would
be ignored for the life of the process. Caught by
`TestResolveCredentials_SecondResolveSeesARotatedSecret` and patched
with a process-global flag, which is more global state layered on
global state.

It also cannot express two identities, per the point above.

### 3. Validating with GetToken. Ours alone. Should keep.

Nobody else does this, and I still think it is right here, so it is a
divergence to defend rather than remove.

`azidentity.NewDefaultAzureCredential` returns a credential and a nil
error when nothing in its chain can produce a token -- measured. So
does `NewClientSecretCredential` with a wrong secret: construction does
not authenticate. Without asking for a token, a bad or absent
credential surfaces later from `DiscoverEndpoints` and is reported as
`CloudDiscoveryFailed`, sending whoever reads it to look at discovery.
With it, the controller reports `CloudCredentialsInvalid` or
`WaitingForCloudCredentials`, which is what those conditions are for.

The runbook is the argument: this operator's failures have repeatedly
cost hours because the symptom named the wrong subsystem.

### 4. Ambient-first. Ours alone, and it has a reason the others lack.

The herd is in-payload and never runs anywhere but a pod, so the
question does not arise. We run from a desk against an `az login`
routinely, and that is the fastest development loop we have. The AWS
path probes ambient first for the equivalent ROSA case.

But ambient-first is what forces the process-global flag: without it,
the probe would walk the chain every reconcile and pay the IMDS timeout
each time. Reading the secret first and falling back to the chain when
there is none is stateless and gets the same result, at the cost that a
desk run against a cluster that already has the secret would use that
rather than your login.

### 5. Rotation. Ours is adequate; ingress is better.

Re-reading the secret every reconcile is fresher than ingress's cache
comparison and needs no watch. It costs one cached read per reconcile,
which is nothing. Keep it, but drop the global flag that currently
guards it.

### 6. ARO HCP. Not done, and three of four do it.

They all handle `dataplane.NewUserAssignedIdentityCredential` against a
credentials file the service supplies. Out of scope until ARO is a
target, but the explicit-credential shape is what makes adding it a
new branch rather than a redesign.

## Recommendation

Rewrite `ResolveCredentials` to return an `azcore.TokenCredential`
rather than mutate the environment:

- read the secret; build `ClientSecretCredential` or
  `WorkloadIdentityCredential` from its shape, as the herd does
- fall back to `NewDefaultAzureCredential` only when there is no
  secret, which is the desk case
- keep the token retrieval, and classify the failure
- thread the credential through `azureplatform.Config` into
  `NewTopologyReader`, `NewRouteServerBackend` and `NewNICClient`,
  leaving `NICClientID`'s separate identity exactly as it is
- delete `applyEnvironment` and the `fromCluster` flag

That removes two pieces of global state, makes the credential in use
readable from the code, avoids the IMDS timeout, and leaves us
diverging from the herd in exactly one place we can defend.
