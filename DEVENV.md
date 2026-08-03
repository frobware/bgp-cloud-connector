# devenv

Local development scaffolding for bgp-cloud-connector. It lives on
its own branch so it never reaches upstream, and sibling worktrees
pick it up by symlink.

Not called `README.md` because that name is taken by the project's
own README, which this branch inherits.

## Setting up a worktree

```
bin/worktree new my-feature
cd worktrees/my-feature
../devenv/link-worktree     # symlinks .envrc, the dev Containerfile and Makefile.local-dev
direnv allow
```

`link-worktree` deliberately does not link `flake.nix`. `.envrc`
resolves the flake through `../devenv`, so every worktree shares one
flake and one lock file.

## What is here

| | |
|:---|:---|
| `flake.nix` | Shell with Go, a C compiler, make, git, kubectl and oc. Not the code generators: the Makefile pins its own versions, and nixpkgs has newer ones that would produce different manifests. |
| `.envrc` | `use flake "path:$PWD/../devenv"` |
| `link-worktree` | Symlinks the above into a sibling worktree |
| `Makefile.local-dev` | `build`, `image`, `push`, `run`, `clean`, `help` |
| `Containerfile.bgp-cloud-connector.dev` | Copies a host-built binary instead of compiling in the container |
| `install-aws-saml` | Creates `~/.venvs/aws-saml` and installs Red Hat's `aws-saml.py` |
| `aws-login` | Mints 12-hour AWS credentials into the `saml` profile, skips if still valid |
| `enable-frr` | Turns on FRR and route advertisements, waits for the rollout |
| `create-route-servers` | Creates the AWS Route Servers the operator expects to discover |
| `list-route-servers` | Shows every route server in a region, flagging orphans |
| `delete-route-servers` | Tears them down again |

## Working on the operator

```
make -f Makefile.local-dev image           # host build, baked into a local image
make -f Makefile.local-dev push            # and pushed to quay
make -f Makefile.local-dev help            # shows the resulting image reference
```

The upstream `Makefile` is untouched and works as normal in the same
shell.

## Running against a cluster

The operator will not start unless `frrk8s.metallb.io` and
`RouteAdvertisements` exist. It creates that precondition itself
during reconcile, which is no help, because it cannot reach reconcile
without them. So:

```
../devenv/enable-frr        # once per cluster; rolls out OVN-Kubernetes
make install                # CRDs
make run                    # manager on your host, against the cluster
```

## AWS

```
export AWS_ACCOUNT=<12-digit-account-id>
../devenv/install-aws-saml  # once per machine
../devenv/aws-login         # every 12 hours
export AWS_PROFILE=saml
```

A `CUDNBgpConfig` needs Route Servers to discover; the operator only
peers against them. On a cluster the rosa-bgp Terraform did not
build, there are none:

```
../devenv/create-route-servers --dry-run
../devenv/create-route-servers
```

Endpoints bill hourly and belong to the VPC, not the cluster, so they
survive the cluster being reaped:

```
../devenv/list-route-servers
../devenv/delete-route-servers --yes
```

All of this duplicates what the rosa-bgp Terraform does, and should
not outlive getting access to it.
