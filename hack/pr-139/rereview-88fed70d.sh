# Lab harness for the re-review of 88fed70d. Source with: . rereview-88fed70d.sh
#
# Binaries are built one per commit as $BIN/operator-<sha>, e.g.
#   go build -o "$BIN/operator-88fed70d" ./cmd/
# and run under that name, so the kube-apiserver audit log's userAgent
# identifies which commit made a request.
: "${KUBECONFIG:?set KUBECONFIG to the cluster under test}"
export KUBECONFIG
BIN=${BIN:-$PWD/bin}
LOGS=${LOGS:-$PWD/logs}
mkdir -p "$LOGS"

ts() { date -u +%H:%M:%S; }

op_stop() {
  if [ -f "$LOGS/operator.pgid" ]; then
    kill -TERM -"$(cat "$LOGS/operator.pgid")" 2>/dev/null
    rm -f "$LOGS/operator.pgid"
  fi
  for i in $(seq 1 40); do ss -ltn 2>/dev/null | grep -q ':8081 ' || return 0; sleep 0.5; done
  echo "port 8081 still held" >&2; return 1
}

op_start() { # op_start <sha> <logname>
  op_stop || return 1
  setsid "$BIN/operator-$1" --zap-devel > "$LOGS/$2.log" 2>&1 &
  echo "$(ps -o pgid= -p $! | tr -d ' ')" > "$LOGS/operator.pgid"
  sleep 3
  echo "$(ts) operator-$1 pgid=$(cat "$LOGS/operator.pgid") log=$LOGS/$2.log"
}

lab_setup() {
  for n in $(oc get nodes -l node-role.kubernetes.io/worker -o name); do
    oc label "$n" bgp_router=true --overwrite >/dev/null
  done
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

lab_vm() { # lab_vm <name>
  oc apply -f - >/dev/null <<EOF
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata: {name: $1, namespace: prod-test}
spec:
  runStrategy: Always
  template:
    metadata: {labels: {kubevirt.io/domain: $1}}
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
EOF
}

hostroutes() { # names and advertised prefixes of the VM host-route objects
  oc -n openshift-frr-k8s get frrconfiguration -l app.kubernetes.io/managed-by=bgp-cloud-connector-vm-host-routes \
    -o jsonpath='{range .items[*]}{.metadata.name} {.spec.bgp.routers[0].prefixes}{"\n"}{end}' 2>/dev/null
}

lab_state() {
  echo "$(ts) routing: phase=$(oc get bgprouting cudn1 -o jsonpath='{.status.phase}' 2>/dev/null)"
  oc get bgprouting cudn1 -o jsonpath='{range .status.conditions[?(@.type=="VMHostRoutesConfigured")]}  VMHostRoutesConfigured={.status} {.reason}: {.message}{"\n"}{end}' 2>/dev/null
  echo "  host-route objects: $(hostroutes | grep -c .)"
  hostroutes | sed 's/^/    /'
  oc -n prod-test get vmi --no-headers 2>/dev/null | awk '{print "  vmi:", $1, "phase="$3, "ip="$4, "node="$5}'
}

# wait_for <description> <timeout-secs> <shell condition>; prints elapsed seconds
wait_for() {
  local what="$1" limit="$2" cond="$3" start=$SECONDS
  while [ $((SECONDS - start)) -lt "$limit" ]; do
    if eval "$cond"; then echo "$(ts) $what after $((SECONDS - start))s"; return 0; fi
    sleep 1
  done
  echo "$(ts) $what: NOT within ${limit}s"; return 1
}
