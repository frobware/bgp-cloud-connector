# PR 135 startup reproductions

Scripts behind the review of the TLS profile startup path in
https://github.com/openshift/bgp-cloud-connector/pull/135. Every script
takes the manager binary as its first argument, so you can run the same
scenario against different builds:

```
direnv exec . go build -o /tmp/pr135/manager ./cmd/
```

## Stub API server: stub-run.sh

No cluster needed. `fakeapi.py` serves discovery for `config.openshift.io/v1`
and fails in the way `MODE` selects; `stub-run.sh` runs the manager against it,
polls `/healthz`, and reports the exit code, wall-clock time and when the
health probe first answered.

```
hack/pr-135/stub-run.sh /tmp/pr135/manager hang 19000 70
```

| mode | API server behaviour |
| --- | --- |
| `refused` | nothing listening (no stub started) |
| `dis503` | 503 on discovery of `config.openshift.io/v1` |
| `dischang` | discovery of `config.openshift.io/v1` never answers |
| `503` | 503 on GET `apiservers/cluster` |
| `hang` | GET `apiservers/cluster` never answers |

Use distinct ports to run modes in parallel; the health probe binds on
port + 1000. `rc=124` means `timeout` stopped a manager that was still
running.

## Forbidden apiservers: demo-403.sh

Needs a cluster-admin `KUBECONFIG` and the CRDs installed (`make install`).
Runs the manager locally as a ServiceAccount bound to the operator's own
ClusterRole, with the `apiservers` rule removed (`deny`) or kept (`grant`,
the control), and prints `/healthz`, `/readyz`, controllers with workers
started, and the phase of a BGPRouting CR each second.

```
KUBECONFIG=... hack/pr-135/demo-403.sh /tmp/pr135/manager deny 150
KUBECONFIG=... hack/pr-135/demo-403.sh /tmp/pr135/manager grant 20
```

With no BGPCloudConfiguration present, reconciling the CR only adds a
finalizer and sets `status.phase: Pending`. The script deletes the CR, the
RBAC objects and the impersonating kubeconfig on exit; the kubeconfig is only
ever written to a private temporary directory. Logs from both scripts go to
`$OUT`, a fresh temporary directory by default.
