# e2e plan

What to test, in what order, and why each item earns its place. The
governing constraint is that the operator now has to be right on AWS, GCP
and Azure, and the half of its behaviour that has never been automated is
the half where the bugs were.

## Decisions

**Standard library `testing`, no framework.** The only argument that could
have overridden this was `openshift-tests` spec registration, which
discovers specs through Ginkgo. These tests run in our own CI and this
operator will not be registering specs into `openshift-tests`, so it does
not apply and will not start applying later.

What we use Ginkgo for, measured across the 1141 lines of the two existing
suites: nine test cases, exactly one `It` per `Context`, ten distinct
matchers of which four appear once or twice, and 116 of 139 `Expect` calls
are error checks. No `AfterEach`, `AfterAll`, `AfterSuite` or
`DeferCleanup` anywhere -- the suites cannot clean up, which is why the
shared one refuses to start against leftover state. No parallelism, no
labels, no focus, no JUnit reporter. It is an assertion library with
decorative nesting. `Eventually` and `Consistently`, 21 blocks, are the
only thing worth replacing rather than deleting. Everything Gomega was doing for us is a short helper; see
"Helpers" below. Failure output is the reason to prefer stdlib: when a
suite fails at 2am its only job is to say what broke, and a file, a line
and the message you wrote beats a spec tree with the message somewhere
inside it.

**Convert before porting, not after.** `test/e2e` is roughly 700 lines
today and is about to take on ~1,300 lines of ported material. Converting
700 lines is a day; converting 2,000 is not.

**Land PR #13 first.** It contains "Let the AWS e2e suite be run more than
once", which touches `test/e2e`. Converting before it merges means
conflicting with ourselves in exactly the way the two cherry-picked
commits on this branch now will. Order: land #13, rebase this branch,
convert, port.

## What exists, and what is wrong with it

`test/e2e` has a generic suite (E2E-01 to E2E-04) and an AWS suite
(E2E-AWS-01 to E2E-AWS-05), in Ginkgo. Between them they cover full-stack
reconcile, FRRConfiguration deletion, FRR pod restart, manual peer
deletion, `SourceDestCheck` re-enablement, and deletion cleanup. That is a
reasonable spread and most of it survives the port unchanged.

Two things do not survive review.

**E2E-AWS-02 "Node lifecycle" does not test node lifecycle.** Its `BeforeAll`
records `initialNodes` and `initialPeerCount`; the body prints them and
then asserts that every *current* router node has a peer and that
`SourceDestCheck` is off. It adds no node, removes no node, and never
compares against the counts it captured. It is a steady-state consistency
check under a name that claims lifecycle coverage, and it would pass on a
cluster where lifecycle handling was completely broken. Rewrite it, do not
port it.

**The resync comment is stale.** It says the operator re-reconciles every
five minutes. That is now `--resync-interval`, and the tests should set it
low so convergence assertions take seconds rather than minutes.

## What to take from ocp-bgp-test-suite

The valuable artefact in
[`rh-mobb/ocp-bgp-test-suite`](https://github.com/rh-mobb/ocp-bgp-test-suite)
is `test-plan-rosa.md`, not the bash. Eighty-six lines written by people
who ran this in anger, and it is a requirements list. The ~1,300 lines of
BATS automate one half of it -- data-plane connectivity from VMs -- and
nothing else. No `@test` touches MachinePools, and none asserts on route
server peers or BGP state.

Take the plan as the specification. Port the matrix test by test so each
one is read on the way through rather than inherited; daxelrod has
explicitly invited us to adapt it, and frobware has flagged that some of
it is unfinished. Forking it wholesale takes the slop with it.

Two habits must not come across:

- **A missing fixture fails; it does not skip.** The BATS suite reads
  `$(get_vm_ip ... 2>/dev/null)`, finds it empty and calls `skip`. An
  unreachable API server therefore produces a green run. Skip only for
  "this cloud is not the target of this run".
- **A test's name must match what it asserts.** E2E-AWS-02 is the same
  defect from the other direction: not a false green from skipping, but a
  false green from asserting something trivially true under a name that
  claims more. Grep for the pattern during the port.

One field note from that plan is worth carrying into our own docs: the
flapping chased on 4.20.17 turned out to be traffic egressing through
nodes that were not router nodes and dying at the ENI on a combination of
source/destination checks and security groups. The expected behaviour is
that traffic egresses from the node the VM is on.

## Priority order

### 1. Node lifecycle

This is first because it is where the real bug was and because nothing,
in either suite, covers it. GCP named Cloud Router peers by a node's
position in a sorted list, so adding a node that sorted first renumbered
every existing peer and dropped every session. A manual MachineSet
scale-up is what caught it: six existing peers untouched, two added.

Three cases, in increasing difficulty:

- **Scale up.** Add a machine, watch the node join, get labelled by
  `autoLabelRouterNodes`, and gain peers. Assert the *existing* peers are
  untouched -- that is the assertion that catches renumbering, and the one
  a naive test omits.
- **Scale down the current next hop.** Remove the node that traffic is
  actually using and assert connectivity survives and the stale peer is
  pruned. `test-plan-rosa.md` lists this with no PASS marker, so it
  appears never to have been run. It is also the ECMP and conntrack
  question this project opened with.
- **Node replaced with a new IP.** An upgrade replaces a node and its
  address changes. Peers are keyed `PeerName(clusterID, ipAddress,
  ifaceIdx)`, so this must create a peer and prune the old one. Also
  listed in the ROSA plan with no PASS marker.

Note that the ROSA plan's PASS results for scale-up were obtained against
an `eni-srcdst-disable` DaemonSet, not against this operator. Different
mechanism, different failure modes; their green says nothing about ours.

### 2. Teardown and finalizer release

Exercised for the first time on 12 August, on both clouds, and it produced
two findings. Deleting `CUDNBgpRouting` then `CUDNBgpConfig` released both
finalizers cleanly, cleared the cloud peers and deleted the NCC spokes,
but:

- `Cleanup` releases the finalizer without confirming the deletes landed.
  On AWS the six route server peers were still reporting `deleting` when
  the CR had gone. Peer deletion there is asynchronous, so a delete that
  failed afterwards has nobody left to retry or report it. GCP masks this
  because its peer removal is a synchronous router update.
- Neither cloud reverts the Network operator patch.
  `additionalRoutingCapabilities` and `routeAdvertisements: Enabled`
  survive teardown. That may be the right call, but it is currently a
  silent one.

Both want tests that state the intended behaviour, whichever way we decide
it.

### 3. Repair after tampering

These were run by hand on GCP on 12 August and have known-good timings, so
they convert directly into assertions:

| Break | Expected |
|---|---|
| Delete the managed `FRRConfiguration` | recreated; BGP re-establishes |
| Delete cloud peers directly | recreated; sessions return |
| Strip the router node label | re-added by `autoLabelRouterNodes` |
| Delete a prerequisite (GCP BGP firewall rule) | condition goes False with a remedy in the message, phase Degraded; restoring it returns to Ready with no restart |

The prerequisite case is the one worth being careful with, because it is
the only one where the operator must *not* self-heal. It must report and
wait.

Worth encoding from the same session: deleting the single managed
`FRRConfiguration` dropped every BGP session, not just one node's, because
every router node loses its configuration at once. Repair is bounded by
the resync interval, so at the five minute default that deletion is a
multi-minute outage. Whether that blast radius is acceptable is a design
question; the test should at least pin the behaviour.

### 4. No-churn assertions

The regression that motivates this: `createOrUpdate` rewrote its object on
every reconcile, both controllers watch what they write, and the routing
controller settled into roughly two reconciles a second indefinitely. The
fix compares spec and labels before writing.

The assertion is the inverse of the usual one -- something must *not*
happen over a window. Measured after the fix: four reconciles in
ninety-five seconds at the default interval, and at `--resync-interval=1s`
the CUDN's `resourceVersion` did not move at all across an entire session
of deliberate breakage. Pin both: reconcile count bounded, and
`resourceVersion` stable.

## Helpers

Three functions cover everything Gomega was providing:

```go
// eventually fails unless cond becomes true within timeout.
func eventually(t *testing.T, timeout, interval time.Duration, desc string, cond func() (bool, string))

// consistently fails if cond stops holding at any point during the window.
func consistently(t *testing.T, window, interval time.Duration, desc string, cond func() (bool, string))

// requireFixture fails, rather than skipping, when a precondition is absent.
func requireFixture(t *testing.T, name string, get func() (bool, error))
```

`cond` returns a message alongside the boolean so the failure can report
the last observed state rather than only that it timed out. Setup and
teardown is `TestMain` plus `t.Cleanup`, which registers at the point of
use and so keeps the cleanup next to the thing it cleans up. JUnit for CI
comes from `gotestsum --junitfile`. Parallelism is deliberately not used:
the cloud resources are shared, there is one route server, and the tests
mutate route tables.

## Practicalities

Cluster provisioning is the long pole, so start it before anything else;
both clusters were destroyed on 12 August and nothing is billing.

Set `--resync-interval` low in the test harness. At one second the routing
controller reconciles once a second and repair is observable in seconds
rather than minutes, which is the difference between a suite you run and
one you avoid. Note that the config controller is slower than its interval
on GCP because each pass makes real Compute and NCC calls, so assertions
should wait on state rather than on a number of passes.

## Related fixes that want a cluster anyway

Not e2e work, but they need the same cluster and pair naturally with it:

- **BGP timers.** No `holdTime` or `keepaliveTime` is set, so FRR's 180
  second default applies and a dead router node stays in the ECMP set for
  up to three minutes. `bfd` is already supported in the CR and detects in
  under a second. This is a live data-plane defect and it is what case 1.2
  above will measure.
- **Spoke naming.** NCC spokes are named `<spokePrefix>-<N>` from a prefix
  with no cluster identity, and carry no labels -- only a free-text
  description. Spoke names are unique per project and region, so two
  clusters in one project using the default `cudn-bgp-spoke` collide. AWS
  route server peers carry `managed-by:
  cudn-bgp-routing-operator/<clusterID>` and Cloud Router peers are named
  with the cluster ID; the spokes were never given the same treatment.
