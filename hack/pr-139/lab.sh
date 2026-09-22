# Lab harness. Source with: . lab.sh
export KUBECONFIG=/home/aim/src/github.com/frobware/bgp-cloud-connector-hacks/clusters/aws-2609220800-42213/auth/kubeconfig
BIN=/tmp/claude-1000/-home-aim-src-github-com-openshift-bgp-cloud-connector-worktrees-general/6861a923-5759-4e01-9a69-ec521072b105/scratchpad/bin
LOGS=/tmp/claude-1000/-home-aim-src-github-com-openshift-bgp-cloud-connector-worktrees-general/6861a923-5759-4e01-9a69-ec521072b105/scratchpad/logs
mkdir -p "$LOGS"

op_stop() {
  ps -eo pid,pgid,comm | awk '$3=="operator"{print $2}' | sort -u | while read g; do kill -TERM -"$g" 2>/dev/null; done
  for i in $(seq 1 20); do ss -ltn 2>/dev/null | grep -q ':8081' || break; sleep 0.5; done
}

op_start() { # op_start <sha> <logname>
  op_stop
  cp "$BIN/operator-$1" "$LOGS/operator"
  setsid "$LOGS/operator" --zap-devel > "$LOGS/$2.log" 2>&1 &
  sleep 3
  echo "operator $1 running, log=$LOGS/$2.log"
}

lab_reset() {
  op_stop
  oc -n prod-test delete vm --all --wait=true >/dev/null 2>&1
  oc -n prod-test delete vmi --all --wait=true >/dev/null 2>&1
  for kind in bgprouting bgpcloudconfiguration; do
    for n in $(oc get $kind -o name 2>/dev/null); do
      oc patch "$n" --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1
      oc delete "$n" --wait=false >/dev/null 2>&1
    done
  done
  oc delete ns prod-test --ignore-not-found --wait=true >/dev/null 2>&1
  oc delete clusteruserdefinednetwork --all >/dev/null 2>&1
  oc delete routeadvertisements --all >/dev/null 2>&1
  oc -n openshift-frr-k8s delete frrconfiguration --all >/dev/null 2>&1
  for n in $(oc get nodes -l bgp_router=true -o name 2>/dev/null); do oc label "$n" bgp_router- >/dev/null 2>&1; done
}

lab_setup() { # label nodes, namespace, CRs. Operator must be started separately.
  for n in $(oc get nodes -l node-role.kubernetes.io/worker -o name); do
    oc label "$n" bgp_router=true --overwrite >/dev/null
  done
  # The primary-UDN label is enforced by a ValidatingAdmissionPolicy and cannot
  # be added after the namespace exists, so it goes on at creation.
  oc apply -f - >/dev/null <<'NS'
apiVersion: v1
kind: Namespace
metadata:
  name: prod-test
  labels:
    cluster-udn: prod
    k8s.ovn.org/primary-user-defined-network: ""
NS
  oc apply -f - >/dev/null <<EOF
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
EOF
}

lab_vm() { # lab_vm <name> [nodeSelectorKey=value]
  local name="$1" sel="${2:-bgp_router: \"true\"}"
  oc apply -f - >/dev/null <<EOF
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata: {name: $name, namespace: prod-test}
spec:
  runStrategy: Always
  template:
    metadata: {labels: {kubevirt.io/domain: $name}}
    spec:
      nodeSelector: {$sel}
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
EOF
}

lab_state() {
  echo "  routing: phase=$(oc get bgprouting cudn1 -o jsonpath='{.status.phase}' 2>/dev/null)"
  oc get bgprouting cudn1 -o jsonpath='{range .status.conditions[?(@.type=="VMHostRoutesConfigured")]}  VMHostRoutesConfigured={.status} {.reason}: {.message}{"\n"}{end}' 2>/dev/null
  echo "  host-route objects: $(oc -n openshift-frr-k8s get frrconfiguration -l app.kubernetes.io/managed-by=bgp-cloud-connector-vm-host-routes -o name 2>/dev/null | wc -l)"
  oc -n prod-test get vmi --no-headers 2>/dev/null | awk '{print "  vmi:", $1, "phase="$3, "node="$5}'
}
