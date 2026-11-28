# CLAUDE.md - Cortex

**cortex** gives agent workloads zero-trust token management with no application code
changes: SPIFFE/SPIRE workload identity, inbound JWT validation, and outbound OAuth 2.0
token exchange (RFC 8693). It runs as a sidecar that
[rossoctl/operator](https://github.com/rossoctl/operator) injects, and as a laptop
service that `scripts/install.sh` installs. **The laptop service is the priority.**

Start with [`core/README.md`](core/README.md) (packages), [`cmd/README.md`](cmd/README.md)
(binaries, modes, ports), [`docs/README.md`](docs/README.md) (reference index) and
[`CONTRIBUTING.md`](CONTRIBUTING.md) (build, test, lint, PR titles).

## Work in your own git worktree

Several Claude Code sessions run against this repo at once. A shared working tree lets one
session's `git checkout` rewrite another's files. If this session did not start in its own
worktree, run this from the top-level checkout before you start:

```sh
git fetch https://github.com/rossoctl/cortex.git main:refs/base/<topic>
git worktree add .claude/worktrees/<topic> -b <branch> refs/base/<topic>
git diff --no-index CLAUDE.md .claude/worktrees/<topic>/CLAUDE.md
```

- Put the worktree under `.claude/worktrees/`. The session then loads only one copy of
  this file: the top-level copy. Where the `diff` shows a difference, follow the worktree's
  lines.
- Fetch into `refs/base/<topic>`. Do not branch from `FETCH_HEAD`, because other sessions
  overwrite it. Do not use `refs/worktree/`, because git reserves that namespace.
- Stay in the worktree. Do not change the top-level checkout.
- If `worktree add -b` fails, run `git worktree list`. A live worktree on that branch
  means another session holds it: pick another name. No worktree means a leftover branch.
- Teardown, from the top-level checkout: `git worktree remove .claude/worktrees/<topic>`,
  then `git branch -d <branch>`, then `git update-ref -d refs/base/<topic>`. If `-d` says
  *not fully merged*, update `main` first. Use `-D` only to discard work.

**Worktrees do not isolate the running Cortex.** All sessions share one
`~/.cortex/config.yaml` and one proxy on `:47600`, and every session's `HTTPS_PROXY`
points at that proxy. `agentop service restart` and `make dev-install` restart the proxy and
cut every attached session. `agentop setup` and `agentop service install` can restart it
when they have something to change. Coordinate before you run any of them.

## Commits and PRs

- Sign off every commit (`git commit -s`). CI fails a PR without `Signed-off-by`.
- End commit messages and PR bodies with
  `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. Never add
  `Co-Authored-By`, `Made-with` or `Generated with Claude Code` lines.
- PR title: `<Prefix>: <Subject starting with uppercase>`. Prefixes are in
  [CONTRIBUTING.md](CONTRIBUTING.md#pr-title-convention).

## Naming: Cortex is the product, AuthBridge is the sidecar

The renames Kagenti → Rossoctl and abctl → agentop are complete. A new `kagenti` or `abctl`
string outside `docs/superpowers/` (a frozen archive) is a mistake, except in code that
removes a pre-rename binary.

AuthBridge → Cortex is partial on purpose. Say **Cortex** for the product and its binaries
(`cortex`, `cortex-envoy`, `cortex-cpex`). Say **AuthBridge** for the injected sidecar
component and for these artifacts, which other systems address by name. **Do not rename
them:**

| Frozen name | Why |
|---|---|
| `authbridge`, `authbridge-envoy`, `authbridge-lite`, `authbridge-cpex` | Image names. The operator selects images by name |
| `authbridge-proxy` (container name) | The operator's `AuthBridgeProxyContainerName`. The binary inside is `cortex` |
| `x-authbridge-direction`, `x-authbridge-secret` | Wire protocol |
| `AUTHBRIDGE_*` | Env vars that users can have set |
| `authbridge-config{,-<agent>}`, `authbridge-runtime{,-config,-mtls}`, `authproxy-routes` | ConfigMaps that the operator creates and mounts |
| `/etc/authbridge/config.yaml` | The operator's volume spec |
| `abph_` | Credential-handle prefix, on the wire |
| `Spec.AuthBridgeMode`, `rossoctl.io/authbridge-mode` | The operator's CRD field and annotation. A rename needs coordinated PRs in two repos |

`authbridge-proxy` also survives where code stops, removes or adopts a v0.7.0 install.

## Nested CLAUDE.md files

They load when you read a file in their directory, but not when you only `grep` there.
Read the file before you change code in its directory:

| File | Covers |
|---|---|
| [`core/plugins/CLAUDE.md`](core/plugins/CLAUDE.md) | Inbound JWT validation, outbound token exchange, route resolver, credential files |
| [`core/listener/vmcontrol/CLAUDE.md`](core/listener/vmcontrol/CLAUDE.md) | The MOCA SBX VM control channel |
| [`deploy/proxy-init/CLAUDE.md`](deploy/proxy-init/CLAUDE.md) | `init-iptables.sh` |
| [`.github/CLAUDE.md`](.github/CLAUDE.md) | CI workflows, container images, release binaries |

## Rules that break silently

- **Plugins are opt-in build tags.** Each plugin is in `core/plugins/all/plugins_<name>.go`
  behind `//go:build include_plugin_<name>`. A build with no `-tags` rejects every config.
  Tag sets come from [`scripts/profile-tags`](scripts/profile-tags/).
- **Plugin contracts** are in [`docs/plugin-reference.md`](docs/plugin-reference.md). Read it
  before you add or change a plugin: body capabilities (`WritesResponseBody` controls SSE
  streaming), `MetricsProvider` forwarding through `configuredPlugin`, and `IsAction`
  classification.
- **`core/` is consumed outside this repo.** Removing exported API there is a cross-repo
  change.
- **The session API (`:9094`, `:47601` on a laptop) has no authentication**, and its
  payloads hold raw prompts and completions. Before you change `core/sessionapi/`,
  `core/session/` or what a listener records, read [`docs/session-api.md`](docs/session-api.md).
  `DELETE` does not clear accountant or context-guru state (see that doc).
- **`listener.egress_allow_hosts` covers the forward proxy only.** The transparent listener
  and extproc do not consult it.
- **Memory changes:** measure both `cortex` and `agentop`, and remember that RSS is not heap.
- **Config hot-reload** does not apply every field. `validateReloadable` in
  `core/reloader/reloader.go` lists the fields that need a restart.

## Checks before you push

- Go: from each module that you touched, run `GOWORK=off go vet ./...` and `gofmt -l .`.
  Give `gofmt` a path: a bare `gofmt -l` reads stdin and exits 0. Only `vet` is a CI gate.
- If a change deletes a package or the last import of a dependency, run
  `go mod tidy -diff` in every module. CI gates on it.
- `cmd/agentop` tests are slow. While you work, run
  `scripts/dev/go-test-shards.sh -short cmd/agentop`. Before you push, run the full
  commands in [CONTRIBUTING.md](CONTRIBUTING.md).
- Tests wait on events, not on the clock. Read
  [Tests and time](CONTRIBUTING.md#tests-and-time) before you write a test that waits.
  Mutation-check each fix: put the bug back and see the test fail.
- Log with `log/slog`.
- `deploy/proxy-init/init-iptables.sh` is `set -e` only. Do not add `pipefail`.
