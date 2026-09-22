# PR #139: what we reproduced, and why each commit exists

Every commit below is justified by a failure observed on a running cluster,
not by reading the code. Where we could not demonstrate a failure, the commit
was dropped rather than kept on argument.

## The cluster

AWS IPI, `us-east-2`, three workers.

```
OCP 4.22.13
OpenShift Virtualization (CNV) 4.22.9
frr-k8s 6/6 ready
```

## What we applied to it

Starting from a cluster with nothing of ours on it:

**1. FRR and route advertisements on the Network operator.** This is the same
patch the operator applies in its own Phase 1, but it has to happen first:
both controllers watch `FRRConfiguration` and `RouteAdvertisements` from
`SetupWithManager`, and CNO creates those CRDs only in response to this patch.
Without them the manager's caches never sync and it exits after two minutes.

```bash
oc patch network.operator.openshift.io cluster --type=merge -p \
  '{"spec":{"additionalRoutingCapabilities":{"providers":["FRR"]},
    "defaultNetwork":{"ovnKubernetesConfig":{"routeAdvertisements":"Enabled"}}}}'
```

**2. OpenShift Virtualization**, with software emulation, because no non-metal
AWS instance type exposes `/dev/kvm`:

```yaml
apiVersion: hco.kubevirt.io/v1beta1
kind: HyperConverged
metadata:
  name: kubevirt-hyperconverged
  namespace: openshift-cnv
  annotations:
    kubevirt.kubevirt.io/jsonpatch: |-
      [{"op": "add", "path": "/spec/configuration/developerConfiguration/useEmulation", "value": true}]
spec: {}
```

Emulation is sufficient for all of this. `status.phase` and
`status.interfaces[].ipAddresses` are filled by virt-handler before the guest
boots, so the fields the operator reads do not depend on the guest running.

**3. The operator CRDs**, `make install`.

**4. Router nodes, a namespace, and the two CRs.** The namespace label matters:
`k8s.ovn.org/primary-user-defined-network` is enforced by a
ValidatingAdmissionPolicy and cannot be added after the namespace exists, so it
goes on at creation.

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: prod-test
  labels:
    cluster-udn: prod
    k8s.ovn.org/primary-user-defined-network: ""
---
apiVersion: networking.openshift.io/v1beta1
kind: BGPCloudConfiguration
metadata:
  name: cluster
spec:
  platform: Manual
  bgp:
    localASN: 65001
    livenessDetection: bgp-keepalive
    peerGroups:
      - neighbors: [{address: 192.0.2.1, remoteASN: 64512}]
        nodeSelector: {}
  routerNodeSelector: {bgp_router: "true"}
---
apiVersion: networking.openshift.io/v1beta1
kind: BGPRouting
metadata:
  name: cudn1
spec:
  network:
    name: prod
    subnets: ["10.100.0.0/16"]
```

The peer address is TEST-NET-1 and no session ever establishes. That is
deliberate: every observation below is about what the operator writes, and a
real peer would add nothing.

**5. A VM.** `l2bridge` binding, on the primary UDN:

```yaml
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata: {name: bgp-vm, namespace: prod-test}
spec:
  runStrategy: Always
  template:
    spec:
      nodeSelector: {bgp_router: "true"}
      terminationGracePeriodSeconds: 0
      domain:
        devices:
          interfaces: [{name: default, binding: {name: l2bridge}}]
          disks: [{name: rootdisk, disk: {bus: virtio}}]
        resources: {requests: {memory: 512Mi}}
      networks: [{name: default, pod: {}}]
      volumes:
        - name: rootdisk
          containerDisk: {image: quay.io/kubevirt/cirros-container-disk-demo}
```

With that in place the PR works as advertised: `Phase=Ready`,
`VMHostRoutesConfigured=True`, one FRRConfiguration pinning the guest address
as a `/32` to the node hosting it, and OVN-Kubernetes generating its own
per-node configurations from `bgp-cc-1`.

## Method

One operator binary was built at each commit and at each commit's parent. For
every finding the cluster is driven into a state, the **parent** binary is run
and its behaviour recorded, then the **commit** binary is run against the same
state. Only the binary changes between the two observations.

---

## `e1db12cb` Skip a VMI whose node has been deleted

**Starting condition.** Operator at `5e50fada`, the PR as written. VM running,
route pinned.

```
phase=Ready
VMHostRoutesConfigured=True Reconciled: Configured 1 VM host routes
object bgp-cc-vm-d11e75282d29917b pins ["10.100.0.3/32"] to ip-10-0-52-81
```

**Action.** Stop the kubelet on that node so it cannot re-register, then delete
the Node object. This is a node dying abruptly.

**Observed.** The VMI still names the node that is gone:

```
VMI: phase=Running node=ip-10-0-52-81   (Node object deleted)

phase=Degraded
VMHostRoutesConfigured=False VMHostRoutesFailed: failed to ensure VM host routes:
  getting node "ip-10-0-52-81" hosting VMI prod-test/bgp-vm: Node "ip-10-0-52-81" not found
host-route objects: 1
```

Still `Degraded` 30 seconds later.

**Why this is wrong.** Three harms from one absent Node. The CR degrades. The
loop is abandoned before it reaches any other VMI, so no other VM on the network
gets a route. And `pruneVMHostRouteConfigurations` is never called, so the stale
object survives and keeps advertising a `/32` pointing at a node that no longer
exists.

The premise is not theoretical: KubeVirt does not fail the VMI while a
virt-launcher pod on that node still counts as alive, and a pod on a node with
no kubelet keeps its container statuses until pod garbage collection removes it.
The output above is that state.

**Corrected.** Identical action, operator at `e1db12cb`, captured in the same
window with the VMI still naming the gone node:

```
VMI: phase=Running node=ip-10-0-13-252   (Node object deleted)

phase=Configuring
VMHostRoutesConfigured=Unknown WaitingForVMAddresses: Configured 0 VM host routes
host-route objects: 0
node-not-found errors: 0
```

The prune ran, so the stale `/32` is withdrawn rather than left advertising.
`Configuring` here rather than `Ready` because this commit precedes `55b22c2f`.

---

## `55b22c2f` Keep a BGPRouting Ready while VM addresses are pending

**Starting condition.** Operator at `e1db12cb` (parent). One healthy VM, route
written.

```
phase=Ready
VMHostRoutesConfigured=True Reconciled: Configured 1 VM host routes
host-route objects: 1
vmi: bgp-vm phase=Running node=ip-10-0-52-81
```

**Action.** Add a second VM whose `nodeSelector` matches no node, so it can
never be scheduled.

**Observed.**

```
phase=Configuring
VMHostRoutesConfigured=Unknown WaitingForVMAddresses: Configured 1 VM host routes; waiting for VM addresses
host-route objects: 1
vmi: stuck-vm phase=Scheduling node=
```

Still `Configuring` 60 seconds later, with 30 reconcile log lines in that
window. It does not recover, because the VM never schedules.

**Why this is wrong.** The ClusterUserDefinedNetwork and the
RouteAdvertisements are configured. The route that can be written has been
written and is correct. One unschedulable VM makes the whole network report
not-ready for as long as it exists, and anything waiting on `Phase=Ready`
waits forever.

**Corrected.** Same cluster state, operator at `55b22c2f`:

```
phase=Ready
VMHostRoutesConfigured=Unknown WaitingForVMAddresses: Configured 1 VM host routes; waiting for VM addresses
```

Both binaries agree the VM is unserved. They disagree about whether that makes
the network not ready. `Phase` now tracks the network; the condition carries
the VM.

---

## `06025769` Report VMs that cannot be given a host route

**Starting condition.** Operator at `55b22c2f` (parent). One VM on
`ip-10-0-52-81`, route written, `Ready`.

**Action.** Remove `bgp_router` from that node, so the VM is on a node with no
BGP peers.

**Observed.**

```
phase=Degraded
VMHostRoutesConfigured=False VMHostRoutesFailed: failed to ensure VM host routes:
  VMs are running on nodes without matching BGP peers: ip-10-0-52-81
host-route objects: 0
NetworkCreated=True Created
RouteAdvertisementsCreated=True Created
```

Still `Degraded` 60 seconds later.

**Why this is wrong.** `NetworkCreated` and `RouteAdvertisementsCreated` are
both True on the same object: the network is healthy and the operator says so.
One VM that cannot be served takes the whole CR to `Degraded` with no recovery
path. Every profile in `test/e2e/manifests` sets `routerNodeSelector` to a
dedicated pool and nothing steers VMs onto it, so a VM landing on an ordinary
worker is the default outcome rather than an error case.

**Corrected.** Same cluster state, operator at `06025769`:

```
phase=Ready
VMHostRoutesConfigured=False VMHostRoutesIncomplete: Configured 0 VM host routes;
  VMs are running on nodes without matching BGP peers: ip-10-0-52-81
```

and when the node rejoins the pool it returns to `True` with the route
rewritten.

---

## `145215e5` Own VM host route configurations from the BGPRouting

**Starting condition.** Operator at `06025769` (parent). One VM, route written.

```
object: bgp-cc-vm-d11e75282d29917b
ownerReferences:            <empty>
prefixes: ["10.100.0.3/32"]
```

**Action.** Stop the operator, remove the BGPRouting finalizer by hand, delete
the BGPRouting. Removing the finalizer is the escape hatch the operator's own
`DeletionBlocked` message tells an admin to use.

**Observed.**

```
bgprouting: 0 remaining
host-route objects still present: 1
orphan bgp-cc-vm-d11e75282d29917b still advertising ["10.100.0.3/32"]
```

and on the node itself, with no BGPRouting and no operator running:

```
show bgp ipv4 unicast
 *>  10.100.0.0/16    0.0.0.0    0    32768 i
 *>  10.100.0.3/32    0.0.0.0    0    32768 i
```

**Why this is wrong.** The object has no owner, so nothing collects it. It goes
on originating the `/32` for a VM that may have moved or gone, and frr-k8s
renders `no bgp network import-check` on every router, so FRR never rechecks
whether a route for that prefix exists. Nothing withdraws it.

**Corrected.** Same action, operator at `145215e5`:

```
ownerReferences: BGPRouting/cudn1 uid=fa1e0252-adda-41bd-b784-66362c6495e0
...
garbage collector removed the orphan
host-route objects remaining: 0
```

BGPRouting is cluster-scoped and FRRConfiguration is namespaced, which is a
legal owner relationship and one the collector honours.

---

## `8ea89b4f` Stop counting VM host routes as another FRR consumer

**Starting condition.** Operator at `145215e5` (parent). No BGPRouting, one VM
host-route object left behind, and `status.frrProviderOwnership: Owned`.

That last part is a precondition, not a result. The foreign-consumer check runs
only when the operator owns the Network patch. This lab pre-enabled FRR, so the
operator recorded the patch as `External` and never reaches the check. That
ordering is forced, and the CI job does the same, so the flag was set directly.

**Action.** Delete the BGPCloudConfiguration.

**Observed.** Deletion blocked, indefinitely:

```
ExternalFRRConfigsExist: 1 FRRConfiguration(s) not owned by this BGPCloudConfiguration
still consume the Network/cluster FRR patch; deletion is blocked until they are
removed: openshift-frr-k8s/bgp-cc-vm-0123456789abcdef. To finish deletion without
reverting the patch, remove the networking.openshift.io/bgpcloudconfiguration finalizer.
```

**Why this is wrong.** The object it names is one the operator wrote. It is
telling an administrator to go and delete the operator's own work, and holding
itself in `Terminating` until they do. `ownedFRRConfiguration` asks whether the
*BGPCloudConfiguration* owns the object, and these are owned by a *BGPRouting*
and carry their own managed-by value, so neither term matches.

**Corrected.** Same state, operator at `8ea89b4f`: deletion completes, and the
object it used to complain about is left untouched.

---

## `f04a1e65` Watch nodes and the cloud configuration from the routing controller

**Starting condition.** Operator at `8ea89b4f` (parent). VM running, route
written, `Ready`, and the operator verified quiet: no BGPRouting reconcile for
a full 20-second sample.

**Action.** Remove `bgp_router` from the node hosting the VM. No annotation, no
nudge, nothing else touched.

**Observed.** First reconcile after **261 seconds**, which is the five-minute
resync. Until then the operator believes a route it wrote to a node that is no
longer a BGP router is still correct.

**Why this is wrong.** `EnsureVMHostRoutes` reads node labels to decide whether
a node is a router and which peer group it is in, and reads the
BGPCloudConfiguration for `routerNodeSelector`, the BGP settings and the peer
groups a cloud discovers. It watches neither. Meanwhile `bgp-cc-N` is rewritten
immediately by the controller that does watch Nodes, so the two disagree for
the whole window.

**Corrected.** Identical action, operator at `f04a1e65`: first reconcile after
**3 seconds**. Establishing the starting condition also needed no nudge on this
binary, where the parent required one.
