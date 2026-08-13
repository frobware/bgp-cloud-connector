# GUIDERAILS.md

This operator is being assembled from two implementations that grew up
apart: the AWS one in this repository, and the GCP one in
[`rh-mobb/osd-gcp-cudn-routing`](https://github.com/rh-mobb/osd-gcp-cudn-routing).
Both worked. Both were exercised by hand, on different clouds, by different
people, against different reference stacks.

That is what makes the merge dangerous. Two working implementations disagreeing
is not one of them being wrong; it is usually two different sets of facts. And
a change that looks obviously correct in one context can be inert, or harmful,
in the other.

These are the rules that have actually caught mistakes here, each with the case
that earned it. Read them when you are about to generalise something, or when
you are confident you are making the code better. Those are the two moments
this document exists for.

## 1. Agreement between the two is evidence, not proof

Where both implementations do the same thing, treat it as a signal that the
thing is load bearing, and find out why before changing it.

Both emitted `toReceive.allowed.mode: all` on every BGP neighbour, byte for
byte, written independently. Accepting every route a peer offers looks like an
obvious thing to tighten. It was convergence, not a shared oversight: OVN
Kubernetes generates its own `FRRConfiguration` for the same neighbours and
asks for `mode: all` too. See rule 3 for why that matters.

## 2. Disagreement is a question that must be answered before you pick a side

Where they differ, understand the difference before choosing. The better half
is often obvious afterwards and never obvious before.

AWS keyed Route Server peers on the node address and reconciled them as a set,
with adopt, create and prune. GCP named Cloud Router peers by the node's
position in a sorted list. Adding a node that sorted first renumbered every
existing peer, so the whole set was rewritten and every session dropped. Taking
the AWS discipline into GCP was right, but only because the difference was
understood first. The merge had, at that point, quietly inherited the weaker
half of each side: AWS's model of never labelling nodes, plus GCP's positional
peer names.

## 3. Check the change can take effect at all

Before improving something, establish that the improvement reaches the world.

`frr-k8s` merges every `FRRConfiguration` targeting a node into one rendered
config. Filtering `toReceive` in our CR would have changed our generated
output, passed its tests, and read in review as a security improvement, while
OVN Kubernetes's unfiltered request for the same neighbours won the merge. The
data plane would not have moved.

## 4. Nothing is validated until it has run on a cluster

Everything that has been most wrong here passed its unit tests first.

`buildGCPPlatform` was written and never wired into the dispatch switch. An
orphaned function is legal Go, so the tree compiled, every test passed, CEL
rules were verified against a live API server, and the operator was reviewed.
It failed the moment it ran: `no platform implementation for "GCP"`. Every test
injected a fake platform through `PlatformBuilder` and bypassed the switch, so
the gap was structural rather than an oversight in any one test.

## 5. Prefer the change you can prove now to the change that is merely better

If you cannot demonstrate the difference, do not land it during a merge.

`CheckPrerequisites` earned its place because propagation could be disabled and
the condition watched to flip to Degraded with a remedy in the message, then
re-enabled and watched to recover. The route filter could not be shown to do
anything at all, so it was withdrawn.

## 6. Generic means different in kind, not different in vocabulary

Something belongs behind the platform interface only if two clouds implement it
differently in kind. If the algorithm is the same and only the API call
differs, the algorithm goes in the controller and only the call goes down.

Machine `preTerminate` handling was written into the GCP platform. It keys on
`spec.providerID` as an opaque string and touches no Google SDK: it is an
OpenShift concern that had been filed under a cloud. Node labelling is the
same. Both belong in the controller, where every platform gets them. Applying
this honestly should make `CloudPlatform` smaller, not larger.

Only half of that has been done. Node labelling moved, and is now
`internal/controller/router_labels.go` behind `spec.autoLabelRouterNodes`.
`preTerminate` did not: it is still `internal/platform/gcp/machines.go`, and
the `NodeLifecycle` interface the controller type-asserts to reach it is still
in `internal/platform/platform.go`. The rule names the case it has not yet been
applied to, which is the more useful thing for it to do than to claim a tidy
ending.

Things that genuinely belong below the line: which API to call and what its
resources look like; ordering the cloud imposes, such as `canIpForward` having
to precede NCC spoke creation; the ownership mechanism, since AWS peers carry
tags and GCP peers are fields inside a router and cannot; and spoke sharding,
because the eight instance limit is GCP's alone.

## 7. Surface a problem where the person can act on it

Admission beats status, status beats logs, and a condition that nobody reads is
not a report.

`CheckPrerequisites` exists because of the sharpest case: every Route Server
peer available, every BGP session established, FRR advertising the CUDN prefix,
and no route table with propagation enabled, so nothing in the VPC could reach
a pod while every signal the operator produced said healthy. Reporting that as
a condition, and withholding Ready, moved the failure to somewhere a person
would see it.

The rule has a limit, and it is worth knowing where. Applying it to the
singleton name looked equally obvious: any name other than `cluster` is
accepted by the API server, exits 0 from `oc apply`, and is only refused later
in a status message. But no OpenShift singleton enforces its name at
admission. `networks.operator.openshift.io`, `infrastructures`, `dnses` and
`proxies` all carry zero root CEL rules and rely on the operator ignoring
anything misnamed, exactly as ours does. Enforcing it would have made this CRD
behave unlike every core singleton an administrator has met. Rule 1 applies to
the platform's conventions, not only to our own two implementations.

## 8. Do not weaken a test to make it pass

If a test fails after your change, the first question is whether the test is
right.

Making a shared lifecycle helper ignore reconcile errors turned one failing
test green and quietly stopped three sibling tests noticing a broken reconcile.
Returning an error from every failure path made two routing tests fail, and
they were correct to: a missing namespace and a duplicated network name are
things a person must fix, not faults to back off from.

## 9. Bulk edits are changes, not conveniences

A regex across many files, or a formatter run to check formatting, is a change
and deserves the same scrutiny as one you typed.

Renaming a label with a regex across fifteen files renamed the keys and left
the values that belonged to them, so three assertions compared a valueless
label against `"true"` and would have passed vacuously or matched nothing.
Separately, running `make fmt` to verify a Makefile edit reformatted nineteen
scripts that were never shfmt clean, producing a 340 line diff nobody asked
for.

## Before you land it

- Do both implementations already do this, and if so, why?
- If they differ, do you understand the reason, or only the diff?
- Can the change actually reach the data plane, or does something downstream
  override it?
- Has it run on a cluster, and what did you observe rather than infer?
- Is the check in the right layer: admission, controller, or platform?
- Did any test change to accommodate the code, and was that test wrong?
- Is anything here provable only by argument? That is the part to cut.

## Still open

Recorded here because they sit inside code that has already shipped, and
because forgetting them is exactly what this document is meant to prevent.

- **`preTerminate` is still filed under a cloud.** Rule 6 above says why it
  belongs in the controller and rule 6 is the rule it breaks. Azure would have
  to reimplement OpenShift Machine handling inside its own package to get the
  behaviour GCP already has.
- **Spoke sharding against Google's ASN rule.** `chunkRouterNodes` shards above
  eight router nodes while every node advertises one cluster-wide ASN, and
  Google states that different spokes require different ASNs. Nothing has run
  with more than four router nodes, so it is untested either way.
- **The five questions for the GCP authors** in
  [`docs/gcp-integration-design.md`](docs/gcp-integration-design.md). Two have
  since been answered by measurement rather than by asking, which establishes
  what the code does but not what its authors know.
- **BGP timers.** No `holdTime` or `keepaliveTime` is set, so FRR's 180 second
  default applies and a dead router node stays in the ECMP set for up to three
  minutes. `bfd` is available and detects in under a second.
- **`status.phase` duplicates the conditions** and cannot express two things
  being true at once. It predates the merge, and the conditions carry the
  truth.
- **Cleanup releases the finalizer without confirming the deletes landed.** On
  AWS the six route server peers were still reporting `deleting` when the
  `CUDNBgpConfig` had already gone; they did finish, but nothing was watching.
  Peer deletion there is asynchronous, so a delete that failed after the
  finalizer was released has nobody left to retry it or report it. GCP hides
  this because its peer removal is a synchronous router update.
- **Teardown does not revert the Network operator patch.** Deleting the
  `CUDNBgpConfig` releases its finalizer, clears the Cloud Router peers and
  deletes the NCC spokes, but leaves
  `additionalRoutingCapabilities: {"providers":["FRR"]}` and
  `routeAdvertisements: Enabled` on `network.operator.openshift.io/cluster`.
  Observed on teardown of a live GCP cluster. Reverting it would restart OVN
  Kubernetes across every node, so leaving it may well be the right call, but
  it is currently a silent one: uninstalling the operator leaves the cluster's
  networking reconfigured and says nothing about it.
