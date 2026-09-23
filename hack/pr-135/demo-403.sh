#!/bin/bash
# demo-403.sh <manager-binary> <deny|grant> [secs]
#
# Runs the manager locally as a ServiceAccount bound to the operator's own
# ClusterRole (config/rbac/role.yaml). "deny" drops the apiservers rule;
# "grant" keeps it and is the control.
#
# A BGPRouting CR is created first. With no BGPCloudConfiguration the
# controller only adds a finalizer and sets status.phase=Pending, so the phase
# shows whether reconciliation ever ran without the operator writing anything
# else. Each second it prints the wall-clock time, /healthz, /readyz, how many
# controllers have started workers, and the CR phase.
#
# KUBECONFIG must name a cluster-admin kubeconfig, and the CRDs must be
# installed (make install). The impersonating kubeconfig is written to a
# private temporary directory and removed on exit, together with the CR and
# the RBAC objects. The manager log goes to $OUT (default: a fresh temporary
# directory).
set -u
H=$(cd "$(dirname "$0")" && pwd)
bin=${1:?manager binary}; mode=${2:?deny or grant}; secs=${3:-150}
: "${KUBECONFIG:?}"
OUT=${OUT:-$(mktemp -d)}
priv=$(mktemp -d); chmod 700 "$priv"
port=18740; ns=pr135-demo; sa=probe; name=pr135-demo; cr=pr135-demo

cleanup() {
  [ -n "${mpid:-}" ] && kill "$mpid" 2>/dev/null && wait "$mpid" 2>/dev/null
  oc patch bgprouting $cr --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1
  oc delete bgprouting $cr --ignore-not-found >/dev/null
  [ -f "$priv/rbac.yaml" ] && oc delete -f "$priv/rbac.yaml" --ignore-not-found >/dev/null
  rm -rf "$priv"
}
trap cleanup EXIT

python3 - "$H/../../config/rbac/role.yaml" "$priv" "$mode" "$ns" "$sa" "$name" <<'EOF'
import os, subprocess, sys, yaml
role, priv, mode, ns, sa, name = sys.argv[1:]
r = [d for d in yaml.safe_load_all(open(role)) if d and d["kind"] == "ClusterRole"][0]
if mode == "deny":
    r["rules"] = [x for x in r["rules"] if "apiservers" not in x.get("resources", [])]
r["metadata"] = {"name": name}
docs = [{"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": ns}},
        {"apiVersion": "v1", "kind": "ServiceAccount", "metadata": {"name": sa, "namespace": ns}}, r,
        {"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding", "metadata": {"name": name},
         "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": name},
         "subjects": [{"kind": "ServiceAccount", "name": sa, "namespace": ns}]}]
yaml.safe_dump_all(docs, open(os.path.join(priv, "rbac.yaml"), "w"))
k = yaml.safe_load(subprocess.check_output(["oc", "config", "view", "--raw", "--minify"]))
k["users"][0]["user"]["as"] = "system:serviceaccount:%s:%s" % (ns, sa)
fd = os.open(os.path.join(priv, "kubeconfig"), os.O_WRONLY | os.O_CREAT, 0o600)
yaml.safe_dump(k, os.fdopen(fd, "w"))
EOF

oc apply -f "$priv/rbac.yaml" >/dev/null
echo "can-i list apiservers as $ns:$sa: $(oc auth can-i list apiservers.config.openshift.io --as=system:serviceaccount:$ns:$sa 2>/dev/null)"
oc apply -f - >/dev/null <<EOF
apiVersion: networking.openshift.io/v1beta1
kind: BGPRouting
metadata:
  name: $cr
spec:
  network:
    name: $cr
    subnets:
      - 10.199.0.0/16
EOF
sleep 2

start=$(date +%s)
KUBECONFIG=$priv/kubeconfig "$bin" --metrics-bind-address=0 \
  --health-probe-bind-address=127.0.0.1:$port > "$OUT/demo-$mode.log" 2>&1 &
mpid=$!

printf '%5s %8s %7s %8s %s\n' t healthz readyz workers phase
last=; lastt=0
while :; do
  sleep 1
  t=$(( $(date +%s)-start ))
  if ! kill -0 $mpid 2>/dev/null; then
    wait $mpid; rc=$?; mpid=
    printf '%4ds manager exited rc=%d\n' "$t" "$rc"; break
  fi
  [ "$t" -ge "$secs" ] && { printf '%4ds stopping after %ss\n' "$t" "$secs"; break; }
  hz=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:$port/healthz)
  rz=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:$port/readyz)
  wk=$(grep -c '"msg":"Starting workers"' "$OUT/demo-$mode.log")
  ph=$(oc get bgprouting $cr -o jsonpath='{.status.phase}' 2>/dev/null); ph=${ph:-<none>}
  cur="$hz $rz $wk $ph"
  if [ "$cur" != "$last" ] || [ $((t - lastt)) -ge 15 ]; then
    printf '%4ds %8s %7s %6s/3 %s\n' "$t" "$hz" "$rz" "$wk" "$ph"; last=$cur; lastt=$t
  fi
done
grep -o '"msg":"problem running manager","error":"[^"]*' "$OUT/demo-$mode.log" | cut -c1-220
echo "forbidden APIServer list errors: $(grep -c 'failed to list \*v1.APIServer' "$OUT/demo-$mode.log")"
echo "log: $OUT/demo-$mode.log"
