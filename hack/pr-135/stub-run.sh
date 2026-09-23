#!/bin/bash
# stub-run.sh <manager-binary> <mode> <port> <secs>
#
# Runs the manager against fakeapi.py in <mode> (see fakeapi.py), or against
# a closed port for mode "refused", for at most <secs>. Polls /healthz once a
# second (the probe binds on <port>+1000) and prints the exit code, wall-clock
# time and when /healthz first answered. Logs go to $OUT (default: a fresh
# temporary directory, printed on exit).
set -u
H=$(cd "$(dirname "$0")" && pwd)
bin=$1; mode=$2; port=$3; secs=$4
OUT=${OUT:-$(mktemp -d)}
kc=$(mktemp); trap 'rm -f "$kc"; [ -n "${spid:-}" ] && kill $spid 2>/dev/null' EXIT
if [ "$mode" = refused ]; then server=https://127.0.0.1:1; else server=http://127.0.0.1:$port; fi
cat > "$kc" <<KC
apiVersion: v1
kind: Config
clusters: [{cluster: {server: '$server', insecure-skip-tls-verify: true}, name: stub}]
contexts: [{context: {cluster: stub, user: stub}, name: stub}]
current-context: stub
users: [{name: stub, user: {token: stub}}]
KC
if [ "$mode" != refused ]; then
  MODE=$mode python3 "$H/fakeapi.py" "$port" 2> "$OUT/$mode.req" & spid=$!; sleep 1
fi
start=$(date +%s)
KUBECONFIG=$kc timeout "$secs" "$bin" --metrics-bind-address=0 \
  --health-probe-bind-address=127.0.0.1:$((port+1000)) > "$OUT/$mode.log" 2>&1 &
mpid=$!
probe=never
while kill -0 $mpid 2>/dev/null; do
  sleep 1
  if [ $probe = never ] && curl -s -o /dev/null http://127.0.0.1:$((port+1000))/healthz; then
    probe="$(( $(date +%s)-start ))s"
  fi
done
wait $mpid; rc=$?
echo "mode=$mode rc=$rc elapsed=$(( $(date +%s)-start ))s healthz_first_up=$probe log=$OUT/$mode.log"
