# Azure e2e: the ask, and what it turned into

9 September 2026. Untracked scratch file, like `AZURE-HANDOFF.md` in
the release clone. Keep it out of pull requests.

## The ask, as you gave it

Two new worktrees exist at
`~/src/github.com/openshift/bgp-cloud-connector/worktrees/{gcp,azure}-e2e`.
In `frobware/bgp-cloud-connector-hacks` you spiked scripts that stand
up route servers and cloud routers for the three clouds.
openshift/bgp-cloud-connector#84 made the AWS e2e story work end to
end. Azure and GCP should get the same. Azure first.

The requirements you set, in your words as far as they go:

- Port the scripts from the hacks repo onto a new branch in the Azure
  worktree.
- Expect rework: structural changes and robustness, as #84 did.
- Compose them, because the same scripts serve both PROW-based CI jobs
  and local development against test clusters.
- Note the effort #84 went to over capturing errors and using
  higher-order functions, and carry that across.
- Make them idempotent, because that is what you use when iteratively
  debugging and writing new e2e tests.
- Always favour debuggability.
- Shellcheck clean.
- Verify against the live Azure cluster at
  `clusters/azure-2609090810-42212`.

## Decisions taken since, and who took them

- **Per-run creation of the estate, not a persistent one.** A
  persistent Route Server would need a BYO-vnet install workflow,
  because `ipi-azure` builds a fresh vnet per run and a Route Server
  only peers inside its own. It would also make the CI path and the
  developer path different code, which is the property #84 does not
  have. Your description of the workflow settled it.
- **`az`, not the ARM REST API.** Raised as an option because the
  build root carries no `az`; you rejected rewriting the scripts
  around `curl`, and the answer was to find `az` in a CI image
  instead.
- **Prerequisites only.** The scripts create the subnet, the public IP
  and the Route Server, and never the peerings. The peerings are the
  operator's work and the suite asserts on them, so building them here
  would let a broken operator adopt them and look identical to a
  working one. This drops `--prerequisites-only` as a flag: it is the
  only behaviour.
- **No `az_retry`.** The AWS scripts retry on `IncorrectState` because
  the AWS CLI returns immediately. `az` blocks on its long-running
  operations, and nothing observed has needed a retry. Speculative
  until something does.

## What exists now, on branch `azure-e2e`

Three commits, each with its tests:

- `133a958f` split the CI bootstrap from the cloud it bootstraps
- `c4a54675` moved `print_fields` into the common library
- `71e6faac` added the Azure helpers, where a failed read stays failed

Written and verified but not yet committed:

| file | what it does |
|:---|:---|
| `hack/azure/create-route-server.sh` | prefix, subnet, public IP, Route Server |
| `hack/azure/delete-route-server.sh` | the reverse, re-derived each run |
| `hack/azure/write-e2e-profile.sh` | the generated profile, and the ASN checks |
| `hack/azure/ci.sh` | the prow bootstrap: service principal, `AZURE_CONFIG_DIR` |
| `hack/ci-e2e-azure.sh` | the job: run the test, then tear down, always |
| `hack/ci-e2e-azure-run.sh` | creates and never removes |
| `hack/ci-e2e-azure-teardown.sh` | removes and never creates |

121 assertions in `hack/lib-test.sh`, shellcheck clean.

## Measurements, taken on the live cluster

`amcdermo-2609090810-kxq9j`, centralus, three masters and three
workers, 4.22.12. One run each, in a subscription nobody else was
using, so read them as floors rather than as worst cases.

| step | measured |
|:---|:---|
| enable FRR | 139s |
| Route Server create | 910s (15m10s) |
| create, second run, everything adopted | 11s |
| Azure peering create, each | about 3m43s, three serially |
| apply to six conditions True | 11m53s |
| operator cleanup, three peerings | 4m39s, about 1m33s each |
| estate delete | 417s (6m57s) |
| delete, second run | 11s |

The peering create is materially slower than the 2m20s your runbook
recorded in August, on the same region and cluster shape. n=1 both
times.

Two things worth keeping:

- `provisioningState` reports `Succeeded` with `virtualRouterIps` empty
  while a create is still running, so readiness is the addresses and
  nothing else. An interrupted create leaves a Route Server that exists,
  claims Succeeded, and has none.
- An ordinary pod cannot reach IMDS: `curl` rc 7. The same node on the
  host network gets HTTP 200 and a real `management.azure.com` token.
  This cluster has no `spec.credentialsMode` and no
  `serviceAccountIssuer`.

## The manual e2e, run today from this branch

The runbook's procedure at `RUNBOOK.md:874`, with the operator built
from this branch and run out of cluster, against the estate these
scripts built:

- six conditions True, `status.peerGroups` holding one group keyed on
  the Route Server with both addresses, `ebgpMultiHop` true, ASN 65515
- three Azure peerings at ASN 65001, one per router node
- one `FRRConfiguration`, `bgp-cc-1`
- **six of six BGP sessions Established**

So the estate is one the operator can discover and peer against. What
does not exist is anything that asserts that automatically.

## What blocks the job going green

Both are operator-side and neither is in this pull request.

1. **Azure credentials in a cluster.** `buildAzurePlatform` calls
   `azureplatform.New` directly, with no counterpart to
   `awsplatform.ResolveCredentials`, and there is no
   `CredentialsRequest` anywhere for Azure. With IMDS unreachable and
   no service account issuer, azidentity's default chain has nothing
   to find. It is more work than the AWS one: CCO hands AWS a
   shared-credentials ini file the SDK parses in either mode, whereas
   Azure gets discrete fields that differ between minting and
   federating, so the operator has to choose between
   `ClientSecretCredential` and `WorkloadIdentityCredential` itself.
   `azureplatform.Config` needs a credential field, and `New`,
   `NewTopologyReader` and `NewNICClient` need to take one.
   `features.operators.openshift.io/token-auth-azure` is `"false"` in
   the CSV and has to flip with it.
2. **`test/e2e/azure` does not exist.** The shared suite asserts
   `spec.aws` is nil and `spec.bgp.peerGroups` is non-empty, so it
   serves platform Manual only.

Until both land, `hack/ci-e2e-azure-run.sh` stands the estate up and
then exits non-zero saying so. A job that goes green having tested
nothing is worse than one that is honestly red, and it is already
non-gating.

## Still to do here

- One openshift/release pull request: a `base_images` entry for
  `ocp:upi-installer`, an `images` entry building `azure-e2e-runner`
  `FROM src` with `az` copied in at its original path, and the `test`
  step's `from:` changed to it. Verified locally with podman: az 2.61
  runs on the build root's Python 3.9.25 and has every flag the scripts
  use. **Raise `grace_period` from 30m0s to 60m0s in the same change**:
  with a 1200s finalizer wait and a Route Server delete that measured
  6m57s, a cancelled job no longer fits 30 minutes.
- Makefile targets: `ci-e2e-azure`, `ci-e2e-azure-teardown`.
- **An AWS bug found while fixing the Azure one, deliberately left
  alone.** `aws_cluster_facts` in `hack/aws/lib.sh` folds stderr into
  `infra` and `region` with `2>&1`, exactly as `azure_cluster_facts`
  did before commit `62b99800`. oc writes server warnings and
  deprecation notices to stderr on calls that return 0, so the warning
  text ends up inside the value, passes the `[[ -n ... ]]` guard, and
  goes on to build names such as `${infra}-rs`. It predates this branch
  and does not belong in an Azure pull request, so it wants either its
  own small commit against main or a tracked issue. Not raised yet.
- Whether an Azure resource group delete cascades through a Route
  Server. `AZURE-HANDOFF.md` says a leaked one blocks the vnet delete
  and takes the resource group with it; `azure-create-route-server`
  says the cluster teardown collects it. Both are yours and they
  disagree. Still free to settle, by standing an estate up and leaving
  it when this cluster is destroyed.

## State of the live cluster

Left behind by today's work, all reversible:

- the operator's CRDs are installed (`oc delete -f config/crd/bases/`)
- FRR is enabled (`hack/disable-frr.sh`)
- three workers carry `bgp_router=true`
  (`hack/label-router-nodes.sh --remove`)
- three workers have `enableIPForwarding` true on their NICs, which
  the operator sets and nothing reverts, on Azure as on AWS and GCP
- no Route Server, no public IP, no RouteServerSubnet, and the vnet is
  back to `10.0.0.0/16`
