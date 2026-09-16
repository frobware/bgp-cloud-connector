# What the operator still needs for Azure, and why

9 September 2026. Untracked scratch file. Keep it out of pull requests.

Companion to `AZURE-E2E-PLAN.md`. That one covers the shell;
openshift/bgp-cloud-connector#122 is the shell. This one is the Go.

Everything below is either read out of the tree or measured on
`amcdermo-2609090810-kxq9j`. Where something is neither, it says so.

## Short version

The Azure reconcile path is mature and was proven working today: six
of six BGP sessions Established, driven by the operator built from
#122's branch. Two things are missing, and only one of them is hard.

1. The operator cannot obtain Azure credentials inside a cluster.
   Blocking: `e2e-azure-operator` cannot pass without it.
2. `test/e2e/azure` does not exist. Blocking for the same reason, but
   mechanical.

A third is cosmetic but ships with the first: the CSV says Azure is
unsupported.

## 1. Credentials

Moved out. This is now its own design document,
`AZURE-CREDENTIALS-DESIGN.md`, which carries the measurements, the
survey of what the rest of the organisation does, and a proposal.

The short version: the operator has no way to authenticate inside a
cluster, and that blocks the job whatever the shell does.

## Why this operator creates its own request, when others do not

Worth writing down, because the two families look contradictory until
you see which one you are in.

**In-payload operators** ship a static `CredentialsRequest` and never
create one at runtime. `cluster-ingress-operator/manifests/00-ingress-credentials-request.yaml`
holds five of them, one per platform, all naming the same secret
(`cloud-credentials` in `openshift-ingress-operator`). CVO applies the
manifests, CCO reconciles whichever matches the cluster's platform, and
the operator only ever reads the secret
(`pkg/operator/controller/dns/controller.go:687`).

**OLM-installed operators** cannot do that: nothing applies their
`manifests/` directory, so a static request would never exist on a
cluster. They build it from Go instead.
`external-dns-operator/pkg/operator/controller/externaldns/credentials_request.go`
is the precedent -- `ensureExternalCredentialsRequest`,
`desiredCredentialsRequest`, per-platform provider specs including
`AzureProviderSpec`, all applied at reconcile time. That is what
`internal/platform/aws/credentials.go` does here, so the design is
ordinary for what we are; only the Azure half is missing.

One deliberate divergence: external-dns-operator imports the typed
cloud-credential-operator API package, and ours addresses
`CredentialsRequest` as unstructured because the typed package drags in
a dependency tree out of proportion to one object. Keep ours.

For contrast, `cluster-dns-operator` has no `CredentialsRequest` at all.
It manages CoreDNS in the cluster and never calls a cloud API.

## 2. `test/e2e/azure`

Does not exist. The shared suite cannot stand in: its `BeforeSuite`
asserts `spec.aws` is nil and `spec.bgp.peerGroups` is non-empty
(`test/e2e/e2e_suite_test.go:88`), so it serves `platform: Manual`
only.

Port `test/e2e/aws`, whose five specs are: full stack reconcile, node
lifecycle, a peer deleted behind the operator's back, an attribute
tampered with, and the cleanup ordering. What differs on Azure:

- one peer group keyed on the Route Server name, not one per
  availability zone
- peerings identified by name, `<clusterID>-bgp-<address with dots as
  dashes>`, because an Azure BGP connection carries no tags. That is
  the operator's own naming, in `azure.go:66-78`
- `enableIPForwarding` on the router nodes' NICs replaces
  SourceDestCheck
- neighbours carry `ebgpMultiHop`
- Azure does not populate `connectionState`, per the runbook, so
  session state has to come from `bgpsessionstates` rather than from
  Azure

Timing matters for how the specs are written. Peering create measured
about 3m43s each and they are created serially, so a spec that changes
the node set and waits for reconciliation waits minutes, not seconds.

## 3. The CSV says Azure is unsupported

`bundle/manifests/bgp-cloud-connector.clusterserviceversion.yaml:58`
and `config/manifests/bases/...:49` carry
`features.operators.openshift.io/token-auth-azure: "false"` next to
`token-auth-aws: "true"`. That annotation is what makes the console
offer the workload identity fields and what makes OLM set them on the
Subscription, so it flips with the credentials work, not separately.

## Order, and a suggestion

Credentials first, because the suite cannot go green without it and
because writing the suite against an operator that cannot authenticate
means debugging two things at once.

Worth considering: the ROSA job in the release config proves its
credentials path with a throwaway `BGPCloudConfiguration` naming a
route server id that cannot exist, then reads the condition to see
whether the failure moved past credentials. The same trick works on
Azure with a `routeServerName` nobody created, and it is much cheaper
than standing up an estate to find out.

## What is not blocking, and should be left alone

The Azure reconcile path. Proven on 14 and 21 August by hand, proven
again today from #122's branch, and #120 has just added SDK-boundary
seams and fixed the empty-node-list peering loss. The gap is
authentication, not reconciliation.

GCP has the same credentials gap, for what it is worth: no
`credentials.go`, and `buildGCPPlatform` constructs directly too. Doing
Azure first should leave a shape GCP can follow.
