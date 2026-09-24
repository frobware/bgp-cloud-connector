#!/usr/bin/env bash
# Finding 2 of the 88fed70d re-review reached without an operator restart,
# through finding 1's no-watch mode, and the no-watch mode persisting in one
# process after KubeVirt is installed.
#
# Needs: the cluster set up as in rereview-88fed70d.sh (lab_setup), the
# OpenShift Virtualization subscription present in openshift-cnv, and
# $BIN/operator-88fed70d. Run: bash rereview-88fed70d-path2.sh
. "$(dirname "$0")/rereview-88fed70d.sh"

hco_apply() {
  oc apply -f - >/dev/null <<'EOF'
apiVersion: hco.kubevirt.io/v1beta1
kind: HyperConverged
metadata:
  name: kubevirt-hyperconverged
  namespace: openshift-cnv
  annotations:
    kubevirt.kubevirt.io/jsonpatch: |-
      [{"op": "add", "path": "/spec/configuration/developerConfiguration/useEmulation", "value": true}]
spec: {}
EOF
}
hco_present() { [ -n "$(oc -n openshift-cnv get hyperconverged -o name 2>/dev/null)" ]; }
vmi_api() { oc api-resources --api-group=kubevirt.io 2>/dev/null | grep -qw VirtualMachineInstance; }
op_pid() { pgrep -g "$(cat "$LOGS/operator.pgid")" -f "operator-88fed70d" | head -1; }
sources() { grep -c 'Starting EventSource' "$LOGS/path2.log"; }
phase3() { grep 'Phase 3: ensuring VM host routes' "$LOGS/path2.log" | awk '{print $1}' | tail -"${1:-3}" | tr '\n' ' '; echo; }

echo "== A. remove KubeVirt, start the operator without it"
op_stop
oc -n openshift-cnv delete hyperconverged kubevirt-hyperconverged --ignore-not-found --wait=false >/dev/null
wait_for "HCO object gone" 900 '! hco_present' || exit 1
wait_for "VMI API gone" 900 '! vmi_api' || exit 1
op_start 88fed70d path2
PID=$(op_pid); echo "operator pid=$PID eventsources=$(sources)"

echo "== B. install KubeVirt with the operator running"
hco_apply
wait_for "VMI API served" 900 'vmi_api' || exit 1
wait_for "HCO Available" 1200 '[ "$(oc -n openshift-cnv get hyperconverged kubevirt-hyperconverged -o jsonpath="{.status.conditions[?(@.type==\"Available\")].status}" 2>/dev/null)" = True ]' || exit 1
T_INSTALL=$SECONDS

for cycle in 1 2; do
  echo "== C$cycle. create and delete a VM, $(( (SECONDS - T_INSTALL) / 60 ))m after install"
  lab_vm vm-p$cycle
  wait_for "VMI has IP" 600 '[ -n "$(oc -n prod-test get vmi vm-p$cycle -o jsonpath="{.status.interfaces[0].ipAddress}" 2>/dev/null)" ]'
  wait_for "route appears" 420 '[ "$(hostroutes | grep -c .)" -gt 0 ]'
  lab_state
  oc -n prod-test delete vm vm-p$cycle --wait=false >/dev/null; echo "$(ts) VM deleted"
  wait_for "VMI gone" 180 '! oc -n prod-test get vmi vm-p$cycle >/dev/null 2>&1'
  wait_for "route withdrawn" 420 '[ "$(hostroutes | grep -c .)" -eq 0 ]'
  echo "  phase 3 reconciles (latest): $(phase3 4)"
  echo "  operator pid now=$(op_pid) (start $PID) eventsources=$(sources)"
done

echo "== D. uninstall KubeVirt as an admin would, with the operator running"
lab_vm vm-p3
wait_for "VMI has IP" 600 '[ -n "$(oc -n prod-test get vmi vm-p3 -o jsonpath="{.status.interfaces[0].ipAddress}" 2>/dev/null)" ]'
wait_for "route appears" 420 '[ "$(hostroutes | grep -c .)" -gt 0 ]'
lab_state
# The default uninstallStrategy, BlockUninstallIfWorkloadsExist, makes the
# HCO admission webhook reject this while a VMI exists.
oc -n openshift-cnv delete hyperconverged kubevirt-hyperconverged --wait=false 2>&1 | sed 's/^/  /'
echo "$(ts) HCO present after delete attempt: $(hco_present && echo yes || echo no)"
oc -n prod-test delete vm vm-p3 --wait=false >/dev/null; echo "$(ts) VM deleted"
wait_for "VMI gone" 180 '! oc -n prod-test get vmi vm-p3 >/dev/null 2>&1'
oc -n openshift-cnv delete hyperconverged kubevirt-hyperconverged --wait=false >/dev/null; echo "$(ts) HCO delete requested"
wait_for "VMI API gone" 600 '! vmi_api'
lab_state
wait_for "routing leaves Ready" 420 '[ "$(oc get bgprouting cudn1 -o jsonpath={.status.phase})" != Ready ]'
lab_state
oc get bgprouting cudn1 -o jsonpath='{range .status.conditions[*]}  {.type}={.status} {.reason}{"\n"}{end}'
echo "  phase 3 reconciles (latest): $(phase3 4)"
echo "  operator pid now=$(op_pid) (start $PID) eventsources=$(sources)"
echo "== done"
