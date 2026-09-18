# MTV Migration Testing on Layer2 CUDN Namespace

**Environment:** OCP 4.21, MTV 2.12.3, OpenStack (CBIS)

## Summary

Testing VM migration from OpenStack to OCP using MTV (Forklift) into a namespace with a Layer2 CUDN as primary network.

- Namespace `app1` with labels `cluster-udn=prod`, `k8s.ovn.org/primary-user-defined-network=""`
- `ClusterUserDefinedNetwork` `cluster-udn-prod`: Layer2, `10.100.0.0/16`, persistent IPAM
- `BGPRouting` advertising `10.100.0.0/16` via BGP

## Prerequisites

### 1. Create a StorageClass

```bash
oc create -f - <<EOF
apiVersion: hostpathprovisioner.kubevirt.io/v1beta1
kind: HostPathProvisioner
metadata:
  name: hostpath-provisioner
spec:
  imagePullPolicy: IfNotPresent
  storagePools:
    - name: local
      path: /var/hpvolumes
---
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: hostpath-csi
provisioner: kubevirt.io.hostpath-provisioner
reclaimPolicy: Delete
volumeBindingMode: WaitForFirstConsumer
parameters:
  storagePool: local
EOF
```

### 2. Create the ForkliftController (if not deployed)

```bash
oc create -f - <<EOF
apiVersion: forklift.konveyor.io/v1beta1
kind: ForkliftController
metadata:
  name: forklift-controller
  namespace: openshift-mtv
spec: {}
EOF
```

## Migration Steps By Example

Source VM name is fedora.

### Step 1 — OpenStack credentials Secret

```bash
oc create -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: openstack-creds
  namespace: openshift-mtv
stringData:
  authType: password
  username: <your-user-name>
  password: <your-password> 
  insecureSkipVerify: "true"
  domainName: <your-domain>
  projectName: <your-project>
  regionName: <your-region> 
EOF
```

### Step 2 — Source Provider (OpenStack)

```bash
oc create -f - <<EOF
apiVersion: forklift.konveyor.io/v1beta1
kind: Provider
metadata:
  name: openstack-or18
  namespace: openshift-mtv
spec:
  type: openstack
  url: <your-openstack-auth-url>
  secret:
    name: openstack-creds
    namespace: openshift-mtv
EOF
```

### Step 3 — Target Provider (if not auto-created by MTV)

```yaml
apiVersion: forklift.konveyor.io/v1beta1
kind: Provider
metadata:
  name: host
  namespace: openshift-mtv
spec:
  type: openshift
  url: https://kubernetes.default.svc
  secret: {}
```

### Step 4 — NetworkMap

Maps the OpenStack `demo-net` (VXLAN, `10.100.0.0/16`) to the default pod network on the target namespace. In the CUDN namespace `app1`, the default pod network is the CUDN `cluster-udn-prod` (`10.100.0.0/16`).

```bash
oc create -f - <<EOF
apiVersion: forklift.konveyor.io/v1beta1
kind: NetworkMap
metadata:
  name: demo-netmap
  namespace: openshift-mtv
spec:
  provider:
    source:
      name: openstack-or18
      namespace: openshift-mtv
    destination:
      name: host
      namespace: openshift-mtv
  map:
    - source:
        id: <your-network-id> 
      destination:
        type: pod
EOF
```

### Step 5 — StorageMap

Maps OpenStack storage to the StorageClass:

```bash
oc create -f - <<EOF
apiVersion: forklift.konveyor.io/v1beta1
kind: StorageMap
metadata:
  name: demo-storagemap
  namespace: openshift-mtv
spec:
  provider:
    source:
      name: openstack-or18
      namespace: openshift-mtv
    destination:
      name: host
      namespace: openshift-mtv
  map:
    - source:
        name: glance
      destination:
        storageClass: hostpath-csi
EOF
```

### Step 6 — Migration Plan

```bash
oc create -f - <<EOF
apiVersion: forklift.konveyor.io/v1beta1
kind: Plan
metadata:
  name: migrate-fedora
  namespace: openshift-mtv
spec:
  provider:
    source:
      name: openstack-or18
      namespace: openshift-mtv
    destination:
      name: host
      namespace: openshift-mtv
  targetNamespace: app1
  map:
    network:
      name: demo-netmap
      namespace: openshift-mtv
    storage:
      name: demo-storagemap
      namespace: openshift-mtv
  vms:
    - id: <your-vm-id> 
      name: fedora
EOF
```

### Step 7 — Execute the Migration

```bash
oc create -f - <<EOF
apiVersion: forklift.konveyor.io/v1beta1
kind: Migration
metadata:
  name: migrate-fedora
  namespace: openshift-mtv
spec:
  plan:
    name: migrate-fedora
    namespace: openshift-mtv
EOF
```

### Step 8 — Monitor

```bash
kubectl get migration -n openshift-mtv -w
kubectl get plan -n openshift-mtv migrate-fedora -o yaml
kubectl get vmi -n app1
```

### After Migration Observations

The `l2bridge` binding is used by the migrated VM in the CUDN namespace which is equivalent to `bridge` binding.

The IP and MAC addresses are preserved after the migration. MTV preserves the MAC in the VM spec (`macAddress: fa:16:3e:3b:b2:a4`) and the IP via the pod template annotation `network.kubevirt.io/addresses: '{"net-0":["10.100.0.107"]}'`.

```
[stack@undercloud (overcloudrc) ~]$ os port list | grep fa:16:3e:3b:b2:a4
| 70e0d0c3-866c-445f-bfa1-3eb208cd0a75 |                                 | fa:16:3e:3b:b2:a4 | ip_address='10.100.0.107', subnet_id='bdb2e0fe-96b4-405e-82b1-32761a36aa5c'        | ACTIVE |
```

Migrated VM spec (relevant sections):

```yaml
spec:
  template:
    metadata:
      annotations:
        network.kubevirt.io/addresses: '{"net-0":["10.100.0.107"]}'
    spec:
      domain:
        devices:
          interfaces:
            - name: net-0
              binding:
                name: l2bridge
              macAddress: fa:16:3e:3b:b2:a4
      networks:
        - name: net-0
          pod: {}
```

VM guest networking after migration:
```
[fedora@fedora ~]$ ip a
1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue state UNKNOWN group default qlen 1000
    link/loopback 00:00:00:00:00:00 brd 00:00:00:00:00:00
    inet 127.0.0.1/8 scope host lo
       valid_lft forever preferred_lft forever
    inet6 ::1/128 scope host noprefixroute
       valid_lft forever preferred_lft forever
2: enp1s0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1400 qdisc fq_codel state UP group default qlen 1000
    link/ether fa:16:3e:3b:b2:a4 brd ff:ff:ff:ff:ff:ff
    altname enxfa163e3bb2a4
    inet 10.100.0.107/16 brd 10.100.255.255 scope global dynamic noprefixroute enp1s0
       valid_lft 2310sec preferred_lft 2310sec
    inet6 fe80::f816:3eff:fe3b:b2a4/64 scope link noprefixroute
       valid_lft forever preferred_lft forever
```

```
[fedora@fedora ~]$ ip r
default via 10.100.0.1 dev enp1s0 proto dhcp src 10.100.0.107 metric 100
10.100.0.0/16 dev enp1s0 proto kernel scope link src 10.100.0.107 metric 100
```
