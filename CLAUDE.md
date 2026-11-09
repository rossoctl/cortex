# CLAUDE.md - Rossoctl Extensions

This file provides context for Claude (AI assistant) when working with the `cortex` monorepo.

## AI Assistant Instructions

- **Always work in your own git worktree — never in the shared top-level checkout.**
  Several Claude Code sessions run against this repo at once. They share one object
  store, which is fine, but a shared *working tree* is not: `git checkout` in one
  session rewrites files under another, and two sessions' uncommitted edits land in one
  index. If this session did not start in its own worktree, then before you start work,
  run these from the top-level checkout:

  ```sh
  # Fetch into a ref only you write, so nothing can move it underneath you.
  git fetch https://github.com/rossoctl/cortex.git main:refs/base/<topic>
  git worktree add .claude/worktrees/<topic> -b <branch> refs/base/<topic>
  ```

  **Put the worktree under `.claude/worktrees/`.** Claude Code does not load the root
  `CLAUDE.md` of a worktree there when you read its files, so the request carries one
  copy of this file, not two. The worktree's nested `CLAUDE.md` files still load, and
  its skills are added to the skill list under a `.claude/worktrees/<topic>:` prefix. A worktree anywhere else in the checkout, such as an older
  `.worktrees/<topic>/`, loads its copy beside this one. A worktree outside the checkout
  does not, but a session cannot `cd` there: Claude Code resets the shell to the
  session's start directory. The cost of `.claude/worktrees/` is that the session loads
  the top-level checkout's copy of this file, which can be behind the worktree's. **After
  you create the worktree, run**
  `git diff --no-index CLAUDE.md .claude/worktrees/<topic>/CLAUDE.md` and follow the
  worktree's lines where the two differ. It prints nothing when they are
  the same, so it does not add a second copy of this file to the context.

  Then stay in the worktree. Leave the top-level checkout alone — treat it as a
  reference copy someone else may be using. When the branch is merged or abandoned, tear
  down all three things you created, from the top-level checkout:

  ```sh
  git worktree remove .claude/worktrees/<topic>   # --force if untracked files remain
  git branch -d <branch>
  git update-ref -d refs/base/<topic>
  ```

  `remove` cleans up its own bookkeeping, so `git worktree prune` is not needed here —
  that is for a worktree directory someone deleted by hand. What `remove` does leave is
  the branch, and a leftover branch is enough to make the next `worktree add -b` of that
  name fail. Deleting the worktree directory instead of removing it is worse: the branch
  stays checked out indefinitely.

  When `branch -d` answers *not fully merged*, that is usually not what happened. It
  judges reachability from the HEAD of whichever tree you run it in, which is normally
  the top-level checkout you were told not to touch — so it is simply behind. PRs land
  here as merge commits, so bring `main` up to date and `-d` will accept the branch.
  Save `-D` for work you really are discarding: it drops unpushed commits silently.

  Three things learned the hard way:
  - **Fetch into your own ref; never branch from `FETCH_HEAD`.** `FETCH_HEAD` is
    per-worktree, but this bootstrap has to run in the shared top-level checkout, so
    every session fetching there writes that one file — and a fetch landing between your
    reading it and your using it hands you a different commit than the one you checked.
    Nothing but you writes `refs/base/<topic>`, so there is no window to lose and no verification
    step to remember. Do not use `refs/worktree/` for this — git reserves that namespace
    for per-worktree refs.
  - **A refused `worktree add` does not always mean another session holds the branch.**
    `-b <branch>` also fails when the branch merely exists with nothing checking it out —
    which is what a `worktree remove` without the matching `branch -d` leaves behind.
    `git worktree list` says who actually holds what; the error text does not. When it
    does show a live worktree on that branch, the refusal is a feature: pick another
    name, do not force past it.
  - **Worktrees do not isolate the running Cortex.** One `~/.cortex/config.yaml`, one
    launchd label, one proxy on `:47600`, and every session's `HTTPS_PROXY` points at
    it. `agentop service restart` always replaces that instance and cuts every attached
    session, and so does `make dev-install`, which runs `agentop setup --restart`.
    `agentop setup` without `--restart` (what `install.sh` hands off to) only does so when
    it has something to change: with nothing to do it prints `cortex vX is installed and
    healthy.` and leaves the proxy alone. `agentop service install` restarts it only when
    it has something to change or a running proxy to adopt, and otherwise prints
    `Already current`. `--ref=main` is an
    `install.sh` flag, not an `agentop` one; it picks which installer script runs, so
    whether it interrupts anything depends on what that install then finds. Coordinate
    before any of it.

- **Use `Assisted-By` for attribution** — never add `Co-Authored-By`, `Generated with Claude Code`, or similar trailers. See [Commit Attribution Policy](#commit-attribution-policy) below.

## Nested CLAUDE.md files

Claude Code loads these when you `Read`, `Edit`, `cat`, `sed -n` or `head` a file in
that directory, and only when the directory is below the session's start directory.
It does not load them for `grep` or `sed -i`. **Before you change code in one of these
directories, read its `CLAUDE.md`** if it has not loaded:

| File | Covers |
|---|---|
| [`core/plugins/CLAUDE.md`](core/plugins/CLAUDE.md) | Inbound JWT validation and outbound token exchange: route resolver, config loading, credential files |
| [`core/listener/vmcontrol/CLAUDE.md`](core/listener/vmcontrol/CLAUDE.md) | The MOCA SBX VM control channel |
| [`deploy/proxy-init/CLAUDE.md`](deploy/proxy-init/CLAUDE.md) | `init-iptables.sh` and its gotchas |
| [`demos/CLAUDE.md`](demos/CLAUDE.md) | Kubernetes deployment: pod shape, shared volumes, ConfigMaps, Envoy, Keycloak, Kind builds, cluster gotchas |
| [`.github/CLAUDE.md`](.github/CLAUDE.md) | CI workflows, container images, adding an image to CI, release binaries |

## Repository Overview

**cortex** provides zero-trust, transparent token management for agent workloads:
SPIFFE/SPIRE workload identity, inbound JWT validation, and outbound OAuth 2.0 token
exchange (RFC 8693), with no application code changes. It runs as a sidecar that the
[rossoctl/operator](https://github.com/rossoctl/operator) injects, and as a laptop
service that `scripts/install.sh` installs. The operator also registers each workload
as a Keycloak client.

**GitHub:** `github.com/rossoctl/cortex` · **Registry:** `ghcr.io/rossoctl/cortex/<image-name>` · **License:** Apache 2.0

### Naming: Cortex is the product, AuthBridge is the sidecar

Three renames have happened and two of them finished. **Kagenti → Rossoctl is
complete** — apart from this rule, every remaining `kagenti` string is inside
`docs/superpowers/`, a frozen archive, or names v0.4's
`kagenti-extensions_<ver>_checksums.txt` (install-stats). Treat a new one as a mistake.

**abctl → agentop is complete too**, and was a clean break: no alias, no reading of
the old on-disk names (see `docs/superpowers/specs/2026-09-30-abctl-to-agentop-rename-design.md`).
Outside `docs/superpowers/`, `abctl` survives only in the code that deletes a
pre-rename binary — setup's cleanup step (`cmd/agentop/setup_step_cleanup.go`) and
its tests — in comments that explain it (the Makefile's `dev-install`, `agentop
uninstall`'s ownership rule, the release smoke test), in the release-notes line
announcing the rename, and in this rule. Treat any other new one as a mistake.

**AuthBridge → Cortex is deliberately partial, and the boundary is the point.**
`Cortex` is the product: this repo, the registry namespace, the laptop service,
`~/.cortex/`, and the sidecar binaries — `cortex`, `cortex-envoy` and `cortex-cpex`, renamed from `authbridge-*` as the same kind of clean break (see
`docs/superpowers/specs/2026-09-30-authbridge-binaries-to-cortex-rename-design.md`).
`AuthBridge` is the name of the **injected sidecar component**, and it survives inside
artifact identifiers that other things address by name:

| Frozen — do not rename | Where it is defined |
|---|---|
| `authbridge`, `authbridge-envoy`, `authbridge-lite`, `authbridge-cpex` | published image names; the operator selects images **by name** |
| `authbridge-proxy` as a **container** name | **another repo's API** — the operator's `AuthBridgeProxyContainerName`. `kubectl … -c authbridge-proxy`, agentop's pod picker and the demos' sidecar detection address it. The binary inside is `cortex`; the operator sets only `Args`, so the image's entrypoint picks it |
| `x-authbridge-{direction,secret}`, `x-authbridge-unmapped-<name>` | wire protocol |
| `AUTHBRIDGE_*` | env vars users may already have set |
| `authbridge-config{,-<agent>}`, `authbridge-runtime{,-config,-mtls}`, `authproxy-routes` | ConfigMaps the operator creates and mounts |
| `/etc/authbridge/config.yaml` | the operator's volume spec; also this repo's entrypoint and manifests |
| `abph_` | credential-handle prefix, on the wire |
| `Spec.AuthBridgeMode`, `rossoctl.io/authbridge-mode` | **another repo's API** — the operator's AgentRuntime CRD field and its annotation |

That last row is why "just rename it everywhere" is not on the table: two of these are
a CRD field and a Kubernetes annotation owned by `rossoctl/operator`. Retiring them
needs a deprecation window and coordinated PRs in at least two repositories.

Because image and container names share the binaries' old spelling, `authbridge-envoy`
or `authbridge-cpex` in a sentence is usually the **image**, and `authbridge-proxy` the
**container** — not a leftover. Outside those two meanings, `authbridge-proxy` survives
where the pre-rename binary is stopped or deleted (setup's cleanup and service steps,
`install.sh --stop`), where a v0.7.0 install is adopted or its port holder recognised
(`setup_probe.go`, `cmd/cortex/portconflict.go`, agentop's container picker), and in
the release notes; the other three old names only in this rule and the spec.

**The rule for prose and comments:** say **Cortex** when the sentence is about the
product — what it is, what it does, what a demo demonstrates — or names one of its
binaries. Keep **AuthBridge** when the phrase names one of the artifacts above: a
sidecar, an image, a container, that container's logs, a mode field, a ConfigMap, a
literal UI label. Both of these are correct: "Cortex provides zero-trust token
management", "the AuthBridge sidecar validates the JWT".

`install.sh` is the reference implementation — zero prose "AuthBridge".

## Layout

What used to live under `authbridge/` is the repo root. Only `authbridge/install.sh`
remains there: it runs `scripts/install.sh`, for the URL v0.7.0 and its docs give.

| Path | What it is |
|---|---|
| `core/` | The runtime library: pipeline, plugins, listeners, config, cost, session store, auth. Package table in [`core/README.md`](core/README.md) |
| `cmd/cortex{,-envoy}/` | Sidecar binaries. [`cmd/README.md`](cmd/README.md) says which binary pins which shape |
| `cmd/cortex-cpex/` | No Go code: the `authbridge-cpex` Dockerfile and the CPEX FFI pins |
| `cmd/agentop/` | The TUI over the session API, and the component the root README leads with. Flags and keys: [`cmd/agentop/README.md`](cmd/agentop/README.md) |
| `praxis/` | Rust workspace for praxis mode: `cortex-praxis-filters`. Outside `go.work`. See #1714 |
| `docs/` | Plugin and framework reference ([`docs/README.md`](docs/README.md) is the index). `docs/superpowers/{plans,specs}/` holds dated design records and is **still written to** |
| `scripts/` | `install.sh` (laptop installer), `keycloak_sync.py` (scope mappings from `routes.yaml`), `profile-tags/` (build-tag profiles), `hooks/commit-msg`, `dev/` |
| `deploy/` | `proxy-init/` (iptables init container), `sparc-service/`, `lineage-attach/` |
| `demos/` | End-to-end demos; [`demos/README.md`](demos/README.md) gives the order |
| `tests/` | Python tests |

## AuthBridge Binaries

| Binary | Mode | Listeners | Plugins |
|--------|------|-----------|---------|
| `cmd/cortex/` | proxy-sidecar (default) | HTTP forward + reverse proxies | full (incl. parsers) |
| `cmd/cortex-envoy/` | envoy-sidecar | gRPC ext_proc on :9090 | `envoy` profile: full minus inference-router and script |
| `authbridge-cpex` _(image: `cortex` + cgo + `cpex` profile)_ | proxy-sidecar | HTTP forward + reverse proxies | sidecar plugins + `cpex` (cgo) |
| `authbridge-lite` _(image: `cortex` + `lite` profile)_ | proxy-sidecar | HTTP forward + reverse proxies | sidecar-minimum plugin set |

Every sidecar binary pins one deployment shape and refuses a mismatching
`mode:` at boot. **Config format:** YAML with `${ENV_VAR}` expansion, mode presets,
and startup validation.

**Plugins are all opt-in.** Each one lives in a `core/plugins/all/plugins_<name>.go`
file gated by `//go:build include_plugin_<name>` (`plugins_cpex.go` also needs the
`cpex` tag). Every binary that registers plugins blank-imports `core/plugins/all`.
The one exception is the session-budget Redis driver: its module requires core, so
each binary links it in its own `storage_redis.go`. A build with no `-tags` registers
no plugins and rejects every config it is handed. Tag sets come from
[`scripts/profile-tags`](scripts/profile-tags/), one profile per shipped artifact.

**Go modules:** 10 in total, all on Go 1.26.10 except the three `demos/*` modules (1.24).
- `core/`, `core/storage/redis/` (nested, but its own module), `cmd/cortex`,
  `cmd/cortex-envoy`, `cmd/agentop`, `scripts/profile-tags`, `scripts/readme-demo`.
  `go.work` links these 7.
- `demos/{echo,finance-sparc,ibac}/` are self-contained and outside the workspace.
- `core/` is **consumed outside this repo**, so removing exported API there is a
  cross-repo change.

## Plugin contracts

The full contract is [`docs/plugin-reference.md`](docs/plugin-reference.md). These
three rules break silently when you get them wrong:

- **Directional body capabilities.** `WritesRequestBody` (calls `pctx.SetBody` or
  `pctx.SetRequestModel`) and `WritesResponseBody` (calls `pctx.SetResponseBody`).
  `WritesResponseBody` is the SSE streaming predicate: both proxy listeners leave
  incremental relay for the buffered path only when some plugin declares it. So a
  request-only mutator (`tool-prune`, `context-guru`) keeps streaming. `pipeline.New`
  lets mutators chain in each direction, and lets no mutator precede a
  `ReadsBody`-only plugin, so readers see what the agent saw. A writer learns whether
  its own write applied from `SetBody`'s result (false under `on_error: observe`), not
  from `pctx.BodyMutated()`.
- **Plugin metrics.** Plugins that implement `pipeline.MetricsProvider` show counters
  on `GET /v1/pipeline` and in agentop's plugin pane. Optional interfaces are not
  promoted through `configuredPlugin`'s embedded `Plugin`, so forward a new one there
  explicitly, or it is invisible for every plugin that has config. Counters reset on
  restart **and on config hot-reload**.
- **Classification.** Parsers (`mcp-parser`, `a2a-parser`, `inference-parser`) set
  `IsAction = true` only for user-meaningful actions. Default-false means bypass.
  Guardrails read `pctx.Classification()` → `(anyAction, anyBypass)`: skip on
  `anyBypass`, pass through on `!anyAction`, and judge only when
  `anyAction && !anyBypass`. Keep protocol vocabulary in the parsers. See
  [Classifying requests](docs/plugin-reference.md#classifying-requests-as-actions-vs-protocol-mechanics).

## Important Port Mapping

| Port | Component | Protocol | Purpose |
|------|-----------|----------|---------|
| 15123 | Envoy | TCP | Outbound listener (iptables redirects app traffic here) |
| 15124 | Envoy | TCP | Inbound listener (iptables redirects incoming traffic here) |
| 8080 | authbridge | HTTP | Reverse proxy (proxy-sidecar inbound) |
| 8081 | authbridge | HTTP | Forward proxy (proxy-sidecar outbound; `HTTP_PROXY` target) |
| 8082 / 8083 | authbridge | TCP | Transparent egress / inbound listeners (iptables REDIRECT targets) — see [`cmd/README.md`](cmd/README.md#ports) |
| 9090 | authbridge | gRPC | Ext-proc server (called by Envoy) |
| 9091 | authbridge | HTTP | Health |
| 9093 | authbridge | HTTP | Stats + config inspection (`/stats`, `/config`, `/reload/status`, `/tls-bridge/unread`, and `/debug/pprof/*` where `listener.bind_loopback_only` is set) |
| 9094 | authbridge | HTTP | Session events API (JSON snapshots + SSE stream) |
| 9901 | Envoy | HTTP | Admin interface (bound to 127.0.0.1) |

A local install uses other ports: `:47600` for the proxy and `:47601` for the session API.

## Session Events API (`:9094`)

When `session.enabled` is true (default) and `listener.session_api_addr` is set
(default `:9094`; `:47601` on a local install), the binary serves its session store
over HTTP. agentop is its main client.

**Trust model:** no authentication. Bind only on in-cluster addresses, never behind
ingress. Payloads may contain raw user messages, LLM completions and tool results.

**Before you change `core/sessionapi/`, `core/session/` or what a listener records,
read [`docs/session-api.md`](docs/session-api.md).** It is the reference for every
endpoint, the event schema (each `error.kind`, `ttfbMs`, `bytesUp`/`bytesDown`,
tunnel rows, `subagent`), the 5-value invocation `action` vocabulary, paging, and the
guards on the two `DELETE` routes. The producer-side contract is in
[`docs/plugin-reference.md`](docs/plugin-reference.md#emitting-session-events).

## Config Hot-Reload (`:9093/reload/status`)

The binary watches its config file (`/etc/authbridge/config.yaml`) via `core/reloader`
and atomically swaps the plugin pipelines without a restart. In-flight requests finish
on the old pipeline.

- **Reloads:** any plugin list change and any plugin `config:` subtree edit.
- **Needs a restart:** `mode` and every field of `listener`, `session`, `cost_ledger`,
  `mtls`, `tls_bridge`, `spiffe`, `memory` and `stats` (`validateReloadable` in
  `core/reloader/reloader.go`).
- **Bad YAML stays safe:** the active pipeline keeps serving, and `/reload/status`
  shows the error with `reloads_failed` incremented.

Lifecycle, debounce and the operator workflow:
[`docs/framework-architecture.md` §9](docs/framework-architecture.md#9-config-hot-reload).

## Top-level `mtls:` configuration

An `mtls:` block (`mode: permissive | strict`, its only field) turns on transport mTLS
on the proxy-sidecar listeners, with the SVID from the spiffe Provider. In
envoy-sidecar mode the block is a no-op: Envoy does mTLS from the `envoy-config`
ConfigMap. Changing the block needs a restart, and a reload that tries is refused.
**Read [`docs/framework-architecture.md` §8a](docs/framework-architecture.md#8a-mtls-layer)
before you change either shape.** Nothing in-tree exercises the Envoy filter chains:
re-verify them by hand after a change.

## Code Style and Checks

### Go
- Run `go vet ./...` and `gofmt -l .` from each module you touched, before you push.
  **Always give `gofmt` a path:** a bare `gofmt -l` reads stdin, scans nothing and
  exits 0.
- **Only `go vet` is a CI gate**, on all 10 modules (cpex code is vetted with
  `-tags cpex` in `go-ci-cpex`). pre-commit has no Go hooks, and CI's `go fmt ./...`
  rewrites the checkout and exits 0, so formatting drift never fails a build.
- Run per-module commands with `GOWORK=off`, as every CI Go job but `core`'s does,
  so each module resolves its own `replace` directives.
- If a change deletes a package or its last import of a dependency, also run
  `go mod tidy -diff` in every module. `go-tidy-check` gates on it, and
  `build`/`vet`/`test` all pass while it fails.
- **`cmd/agentop` tests: use shards and `-short`.** One sequential run takes about 6
  minutes on a Mac. While you change code, run
  `scripts/dev/go-test-shards.sh -short cmd/agentop` (about 20 s): `-short` skips the
  setup, service, doctor and uninstall scenes. Before you push, run the full suite once
  with `scripts/dev/go-test-shards.sh -race cmd/agentop` (about 2 min). The script
  tests only the root package, so also run, from `cmd/agentop`:
  `go test -race $(go list ./... | grep -v 'agentop$')`. Run the full suite also when
  you change those scenes' code. CI runs `go test -race ./...`, not `-short`.
- Log with `log/slog`. The binaries log under their own name (`cortex`,
  `cortex-envoy`). The `authbridge-lite` image runs `cortex`, so it logs as `cortex`.

### Python and shell
- Python 3.12+. `python-keycloak>=7.1.1,<8` for all Keycloak admin calls. Every
  `get_or_create_*` helper checks existence before it creates.
- `ruff` runs in pre-commit with `exclude: ^tests/`. `tests/` does not lint clean, so
  widening the filter is a separate change.
- **`deploy/proxy-init/init-iptables.sh` is `set -e` only.** Do not add `pipefail`
  without testing: its iptables probes rely on tolerated failures.

### PR titles
`<Prefix>: <Subject starting with uppercase>`. The check is case-insensitive, so
`feat:` passes. The prefix also files the PR in the release notes. The accepted
prefixes and the release-notes mapping are in
[CONTRIBUTING.md](CONTRIBUTING.md#pr-title-convention).

## Gotchas and Known Issues

1. **Session store memory, and IBAC user intent under chatty traffic.** The store keeps **every** event a session produces: `session.max_events` is unset and `session.ttl` defaults to never, by design (see `SessionConfig.Limits`). `session.max_sessions` (100, oldest-used first) is the default bound, and it limits how MANY sessions are kept, not how big one gets. Set `max_events` for a FIFO per-session cap and `ttl` to bound how long raw prompts stay in memory. Clients read deep sessions by paging with `?before=<seq>`, not by raising `limit`.

    **Interning** (`core/session/intern.go`) keeps one copy of each distinct string per session, with one table per **conversation** (model + first message, split from a sibling by the second), so that subagents taking turns do not evict each other's table. Rules for changing it:
    - **An event with nothing to intern must not clear the table.** A CONNECT tunnel-open lands between an inference request and its response on every bridged request, and a reset there makes the next turn copy the whole conversation again.
    - **Measure with a fixture that interleaves contentless events.** A clean run of turns is not what live traffic looks like.
    - Message-header arrays share one growing backing array per conversation. Roles and per-result `ToolUseID`s intern below the length threshold (`internID`). The tool manifest (descriptions and schemas) and tool results intern too.
    - `InferenceTool.Parameters` is `pipeline.RawJSON` so that it interns as a string. **It must stay a NAMED string type with a `MarshalJSON` method:** OPA's `ast.InterfaceToValue` reads a plain or aliased string as a JSON string, and every policy that indexes `input.inference.tools[_].parameters` then goes silently undefined (`core/plugins/opa/tool_parameters_rego_test.go` guards it).
    - MCP `Params`/`Result` do not intern (they are `map[string]any`, unmeasured).

    **Reading the store costs memory too.** An encoder expands every shared string back out. So `GET /v1/sessions/{id}` writes one event at a time (`core/sessionapi/snapshot.go`, `BenchmarkSnapshotHeap`), and agentop decodes one event at a time through the same `session.Interner` (`cmd/agentop/apiclient/snapshot.go`, `BenchmarkDecodeSessionViewHeap`). agentop keeps streamed `/v1/events` arrivals only for the session it is watching (`watchingSession`: `selectedSess`, pane-agnostic, or the timeline's session). The others update their summary and are dropped. `model.pluginRuns` still counts every session. When you judge a memory change:
    - **Measure both processes.** agentop has been the larger one.
    - **RSS is not heap.** `/debug/pprof/*` is served only where `listener.bind_loopback_only` is set.
    - `memory.limit_mb` (default 2048 on a local install) is a soft `debug.SetMemoryLimit`, set at boot and refused on reload. Too low means slow, never a crash. It does not count CPEX's cgo memory.

    **The session archive is disk, not memory** (`core/session/archive`, on for a local install bound to loopback, off elsewhere unless `session.archive.dir` is set: `config.ArchiveRunsOnLocalInstall`). Every memory bound above still applies, but eviction no longer loses history. On disk it interns the same seven fields the store does, and it is bounded by `retention_days` (30) and `max_bytes` (2 GiB). `DELETE /v1/sessions` erases it with the store. A burst it cannot keep up with is dropped from the archive and counted, never from memory. At startup `cmd/cortex` replays the last six hours into the usage ring (`usage.Aggregator.Replay`, not through the store) **beside live traffic, not before it**, so `/healthz` answers at once. The segment list is taken before any listener starts: a process never appends to a segment it did not open, so nothing live is read back. The replay fills a private ring from `BeginReplay`, and `FinishReplay` first applies any clear, delete or rename the live ring took meanwhile. The replay has `usageReplayBudget` (60s) or does not run. User-facing details: [`docs/laptop-service.md`](docs/laptop-service.md).

    Two layered defenses keep the inbound A2A user intent visible to IBAC when an agent sends dozens of outbound events per turn:

    - **Primary fix — `listener.skip_hosts`** ([`docs/framework-architecture.md`](docs/framework-architecture.md#skipping-infrastructure-hosts-listenerskip_hosts) has the example): infrastructure destinations (OTel collectors, metrics, log shippers) whose traffic bypasses the pipeline AND session recording. Do NOT add hosts that need IBAC or token-exchange policy — bypass means bypass. A change needs a pod restart.

    - **Egress boundary — `listener.egress_allow_hosts`**, or `MOCA_EGRESS_ALLOW` when the field is empty: the forward proxy's deny-by-default allow list. An off-list destination is refused 403 `destination_denied` by name, before the pipeline, DNS and any dial (shape: [`docs/session-api.md`](docs/session-api.md#event-schema)). Rules:
      - Entries are exact `host:port`. Portless and wildcard entries are startup errors, and so is an unbracketed IPv6 literal: write `[2001:db8::1]:443`. A destination with no port is judged at 80 for http and 443 otherwise. A malformed authority is denied by name, never a panic.
      - **It is checked BEFORE `skip_hosts`**, so a skip pattern cannot exempt an off-list host.
      - **Scope: the forward proxy only.** The transparent listener (enforce-redirect) and extproc (envoy-sidecar) do not consult it. A deployment that uses either must hold its egress elsewhere: deny-by-default here is not deny-by-default for the pod.
      - It judges the **name**. `listener.egress_guard` judges the resolved **address**, so an allow-list entry is not a permit for the guard.
      - Example: `listener: {egress_allow_hosts: ["github-tool-mcp:8080", "api.anthropic.com:443"]}`, or `MOCA_EGRESS_ALLOW=github-tool-mcp:8080,api.anthropic.com:443`. A change needs a pod restart, and each refusal leaves a `phase: "denied"` row with an `egress-allowlist` invocation.

    - **Backstop — intent pin**, only when `session.max_events` is set: the store pins the most recent inbound A2A request against FIFO eviction. Only the latest intent is pinned, so stale intents cannot starve the buffer. IBAC's `pctx.LastIntent()` can still have no event after deletion, `max_sessions` eviction, a `ttl` expiry, a restart, or before the first inbound request. Reach for `skip_hosts` first when the noisy traffic is identifiable infrastructure.

## DCO Sign-Off (Mandatory)

Every commit **must** carry a `Signed-off-by` trailer. Commit with `git commit -s`.
CI fails a PR without it. To sign off existing commits: `git rebase --signoff main`.

## Commit Attribution Policy

Do NOT use `Co-Authored-By` trailers for AI attribution. End commit messages and PR
bodies with:

    Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>

Never add `Co-authored-by`, `Made-with`, `🤖 Generated with [Claude Code](https://claude.com/claude-code)`
or similar lines. `scripts/hooks/commit-msg` rewrites a `Co-authored-by:` trailer that
names Claude, Cursor or Anthropic into `Assisted-By`, and removes `Made-with:` lines.
It never fails a commit, and it does nothing for PR bodies. Install it with
`pre-commit install --hook-type pre-commit --hook-type commit-msg`.
