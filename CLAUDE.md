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
  session's start directory. The cost of `.claude/worktrees/` is that the session reads
  the top-level checkout's copy of this file. When a branch changes `CLAUDE.md`, read the
  worktree's copy.

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

## Repository Overview

**cortex** contains Kubernetes security extensions for the [Rossoctl](https://github.com/rossoctl/rossoctl) ecosystem. It provides **zero-trust authentication** for Kubernetes workloads through transparent token exchange and dynamic Keycloak client registration using SPIFFE/SPIRE identities.

The sidecar injection webhook lives in a separate repo: [rossoctl/operator](https://github.com/rossoctl/operator).

**GitHub:** `github.com/rossoctl/cortex`
**Container registry:** `ghcr.io/rossoctl/cortex/<image-name>`
**License:** Apache 2.0

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
`~/.cortex/`, and the sidecar binaries — `cortex`, `cortex-envoy`, `cortex-cpex`,
`cortex-praxis`, renamed from `authbridge-*` as the same kind of clean break (see
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

## What Cortex Does

Cortex provides **zero-trust, transparent token management** for Kubernetes workloads. It combines three capabilities:

1. **Automatic Identity** -- Workloads obtain SPIFFE IDs from SPIRE and auto-register as Keycloak clients
2. **Inbound JWT Validation** -- Incoming requests are validated (signature, issuer, audience) by the authbridge binary
3. **Outbound Token Exchange** -- Outgoing requests get their tokens automatically exchanged for the correct target audience (OAuth 2.0 RFC 8693)

All of this happens transparently via sidecar injection -- no application code changes required.

## Top-Level Directory Structure

What used to live under `authbridge/` is the repo root. Only `authbridge/install.sh`
remains there: it runs `scripts/install.sh`, for the URL v0.7.0 and its docs give.

```
cortex/
├── core/                             # The runtime library (Go module). Auth is ~1% of it;
│   │                                 # see core/README.md for the measured breakdown.
│   ├── pipeline/                     #   Plugin interface + lifecycle
│   ├── plugins/                      #   Every plugin; each owns its own config
│   │   ├── jwtvalidation/            #     JWKS-backed JWT verifier (validation/)
│   │   ├── script/catalog/           #     Script plugins shipped in agentop;
│   │   │                             #     `agentop script install <name>`
│   │   └── tokenexchange/            #     RFC 8693 exchange client + token cache
│   ├── listener/                     #   All listener implementations (extproc, proxies)
│   ├── config/                       #   Mode presets, YAML config, validation
│   ├── cost/                         #   pricing, settle, ledger, event, usage
│   ├── session/  sessionapi/         #   Session store + the :9094 API
│   ├── observe/  redact/             #   The :9093 server; sensitive-value stripping
│   ├── auth/  bypass/                #   HandleInbound/Outbound; path matcher
│   ├── capabilities/                 #   Capability interfaces extensions implement
│   ├── spiffe/                       #   SPIFFE credential sources (in-process
│   │                                 #   Workload API client + /opt file mirror)
│   ├── routing/                      #   Host-to-audience router
│   ├── memstore/                     #   Process-scoped TTL map (vs storage/, persistent)
│   ├── bootstrap/                    #   Process startup: logging, health, stats servers
│   ├── tlsconfig/  tlsbridge/        #   Builds tls.Config values; forges bridge certs
│   ├── peerproc/                     #   Which process holds a TCP connection
│   │                                 #   (macOS pcblist_n, Linux /proc)
│   └── storage/redis/                #   Redis driver for the storage.Store interface
│                                     #   (its own module, nested but not part of core)
│
├── cmd/cortex/                       # proxy-sidecar mode (default). Full plugin set.
│   ├── main.go                       #   (the authbridge-lite image is this binary
│   │                                 #    built with the `lite` profile's tags)
│   ├── Dockerfile                    #   proxy-sidecar image (the `authbridge` image)
│   └── entrypoint.sh
│
├── cmd/cortex-envoy/                 # envoy-sidecar mode. `envoy` profile: full minus
│   │                                 #   inference-router and script
│   ├── main.go
│   ├── Dockerfile                    #   envoy-sidecar combined image (Envoy + cortex-envoy)
│   └── entrypoint.sh
│
├── cmd/cortex-cpex/                  # The authbridge-cpex image: no Go code. Builds cmd/cortex
│   │                                 #   with cgo + -tags cpex (`cpex` profile)
│   ├── Dockerfile                    #   proxy-sidecar build linking libcpex_ffi.a
│   ├── CPEX_FFI_VERSION              #   pinned CPEX release tag (e.g. v0.3.1)
│   ├── CPEX_FFI_ABI                  #   FFI ABI integer that release must report
│   ├── CPEX_FFI_SHA256               #   pinned digest of each target's FFI tarball
│   └── entrypoint.sh
│
├── cmd/cortex-praxis/                # proxy-sidecar rendered into a Praxis proxy config.
│   ├── main.go                       #   PAUSED: ships in no image, registers no
│   ├── Dockerfile                    #   plugins, has no demo — but deliberately kept
│   └── entrypoint.sh                 #   and kept compiling. Do not delete.
│
├── cmd/agentop/                      # Terminal UI over the session API (:9094).
│   ├── tui/                          #   Panes: sessions, agents, events, timeline,
│   │                                 #   pipeline, catalog, usage
│   ├── edit/, apiclient/,            #   Pipeline editing, API client, cluster
│   │   cluster/, toolscan/           #   port-forward, tool manifest scanning
│   └── README.md                     #   Full flags + keybindings
│
├── cmd/README.md                     # Which binary pins which deployment shape
│
├── docs/                             # Plugin + framework reference, proposals, assets
│   ├── architecture.md               #   How a request flows through the pipeline
│   ├── plugin-reference.md           #   Producer-side plugin contract
│   ├── plugin-catalog.md             #   Per-plugin config fields
│   ├── pricing.md                    #   Rates, discounts, overrides, the cost record
│   ├── framework-architecture.md     #   Pipeline internals, hot-reload
│   ├── proposals/, assets/           #   Repo-level proposals + images
│   └── superpowers/{plans,specs}/    #   Dated design records. STILL WRITTEN TO —
│                                     #   not an inert archive.
│
├── scripts/
│   ├── install.sh                    # Laptop installer: downloads, verifies and stages a
│   │                                 #   release, then execs `agentop setup --from <stage>`
│   ├── keycloak_sync.py              # Declarative Keycloak sync tool (routes.yaml driven)
│   ├── fetch-cpex-ffi.sh             # Download + verify the pinned libcpex_ffi.a
│   ├── dev/                          # Loose dev-only shell scripts
│   │   ├── local-build-and-test.sh   #   Build every image and load it into Kind
│   │   ├── verify-spire-keycloak.sh  #   Platform preflight for a local dev cluster
│   │   ├── verify-moved-ca-diagnostics.sh #   Repro for issue #1033's moved-CA diagnostics
│   │   └── verify-reboot-lingering.sh #   Real-reboot check of systemd lingering (#964)
│   ├── profile-tags/                 # Build-tag resolver: one profile per artifact
│   ├── readme-demo/                  # Generates the README demo animation
│   └── hooks/commit-msg              # Rewrites Co-Authored-By to Assisted-By
│
├── deploy/                           # Cluster-side deployables that run beside the sidecar
│   ├── proxy-init/                   #   iptables init container (envoy-sidecar + proxy-sidecar enforce-redirect modes)
│   │   ├── init-iptables.sh          #     iptables setup script
│   │   ├── Dockerfile.init           #     proxy-init container image
│   │   ├── Makefile                  #     docker-build-init + load-image targets
│   │   └── README.md
│   ├── sparc-service/                #   Python SPARC reflection service (own image)
│   └── lineage-attach/               #   OTel shim + scripts for lineage propagation
│
├── demos/                            # see demos/README.md for the order
│   ├── weather-agent/                #   Getting started (+ advanced, + agentop walkthrough)
│   ├── github-issue/                 #   Token exchange + scope-based access (largest)
│   ├── token-exchange-routes/        #   Routes config reference
│   ├── ibac/, hr-cpex/,              #   Guardrail / policy demos
│   │   finance-sparc/                #   (echo, ibac and finance-sparc are
│   ├── echo/, lineage/               #    self-contained Go modules)
│   ├── session-budget/               #   Redis-backed budget tracking
│   ├── context-guru/                 #   Opt-in context-guru plugin
│   └── coding-agent-cpex/            #   CPEX policy on a coding agent
│
├── tests/                            # Python tests
├── go.work                           # Workspace linking 8 of the 11 Go modules
├── .github/
│   ├── workflows/                    # CI/CD (ci.yaml, build.yaml, release-binaries.yaml,
│   │                                 # security-scans, scorecard, spellcheck)
│   └── ISSUE_TEMPLATE/               # laptop-feedback form + config
├── .pre-commit-config.yaml           # Pre-commit hooks (whitespace, yaml/json, ruff; no Go hooks)
└── CLAUDE.md                         # This file; nested ones are listed at the top
```

## Major Components

### 1. AuthBridge Binaries (Go)

**Sidecar binaries** providing transparent traffic interception for both inbound JWT validation and outbound OAuth 2.0 token exchange (RFC 8693). See [AuthBridge Binaries](#authbridge-binaries) below for the list.

**Library:** `core/` — see [`core/README.md`](core/README.md) for the library reference
**Language:** Go 1.26.10 (`go.work` and the eight workspace modules; the
three self-contained `demos/*` modules are still on 1.24)
**Deployment shapes:** [`cmd/README.md`](cmd/README.md) — which binary pins which shape
**Framework internals:** [`docs/framework-architecture.md`](docs/framework-architecture.md), [`docs/plugin-reference.md`](docs/plugin-reference.md), [`docs/architecture.md`](docs/architecture.md)

**Common:**
- `core/` — the runtime library: the plugin framework and registry, every listener (Envoy ext_proc plus the HTTP proxies), the cost pipeline, the session store, and the auth primitives. ~84k non-test lines, of which auth is ~1%. **Not protocol-free** — it depends on `google.golang.org/grpc` and `envoyproxy/go-control-plane`. See [`core/README.md`](core/README.md).
- `deploy/proxy-init/init-iptables.sh` — traffic interception setup (Istio ambient mesh compatible). Used by envoy-sidecar mode (`redirect`) and by proxy-sidecar mode's `enforce-redirect` egress guard.
- `deploy/proxy-init/Dockerfile.init` — proxy-init container image.

Ports: see [Important Port Mapping](#important-port-mapping).

### 2. Client Registration

Keycloak client registration for workloads is handled by the
**operator** (separate repo) — see `operator/docs/operator-managed-client-registration.md`.
The operator creates a Secret with `client-id.txt` + `client-secret.txt`
and the webhook mounts it at `/shared/` in the workload pod.

## AuthBridge Binaries

Sidecar binaries, one Dockerfile each; the `authbridge-lite` and `authbridge-cpex` images are build variants of the proxy binary (`lite`: proxy Dockerfile + the `lite` profile's tags; `cpex`: `cmd/cortex-cpex/Dockerfile`, cgo + `-tags cpex` + the `cpex` profile):

| Binary | Mode | Listeners | Plugins |
|--------|------|-----------|---------|
| `cmd/cortex/` | proxy-sidecar (default) | HTTP forward + reverse proxies | full (incl. parsers) |
| `cmd/cortex-envoy/` | envoy-sidecar | gRPC ext_proc on :9090 | `envoy` profile: full minus inference-router and script |
| `authbridge-cpex` _(image: proxy + cgo + `cpex` profile)_ | proxy-sidecar | HTTP forward + reverse proxies | sidecar plugins + `cpex` (cgo) |
| `cmd/cortex-praxis/` | proxy-sidecar | HTTP (Praxis-rendered) | **none** — paused, see [`cmd/README.md`](cmd/README.md) |
| `authbridge-lite` _(image: proxy + `lite` profile)_ | proxy-sidecar | HTTP forward + reverse proxies | sidecar-minimum plugin set (see `scripts/profile-tags`) |

Every sidecar binary but praxis pins one deployment shape and refuses a
mismatching `mode:` at boot; praxis pins none. See [`cmd/README.md`](cmd/README.md) for which binary pins which shape.

`cmd/agentop/` is also a Go module here but is not a sidecar — it is the
operator-facing TUI over the session API, and the component the root README leads
with. See [`cmd/agentop/README.md`](cmd/agentop/README.md) for flags and keybindings.

**Plugins are all opt-in.** Each one lives in a `core/plugins/all/plugins_<name>.go`
file gated by `//go:build include_plugin_<name>` (`plugins_cpex.go` also needs the
`cpex` tag). Every binary that registers plugins blank-imports `core/plugins/all`,
so one file serves every binary. The one exception is the session-budget Redis
driver: its module requires core, so each binary links it in its own
`storage_redis.go`. `main.go` imports no plugin package directly, so a build
with no `-tags` registers no plugins at all and rejects every config it is handed.
Tag sets come from [`scripts/profile-tags`](scripts/profile-tags/), one profile per
shipped artifact.

### Release binaries

`v*` tag pushes trigger `.github/workflows/release-binaries.yaml`, which
cross-compiles `cortex` and `agentop` for linux/darwin ×
amd64/arm64 and attaches tarballs to the GitHub Release. Its default
`cortex` tarball, `cortex_<ver>_<os>_<arch>.tar.gz`, is built with the
default plugin set (the `authbridge` image's). The `lite` profile ships only
as the `authbridge-lite` image, and opt-in plugins such as `sessionbudget` and
`context-guru` are built from source. To publish a pure-Go variant, append a
`<suffix>:<build-tags>` entry to the `proxy_variants` array in the workflow —
never enumerate combos.

The one other variant is `cortex-cpex_<ver>_<os>_<arch>.tar.gz`: `cortex`
built with cgo, the `cpex` tag and the `fullcpex` profile (`full` + `cpex`).
It cannot cross-compile, so a separate `build-cpex` matrix job builds it
natively for darwin/arm64 (`macos-14`, macOS 14.5 floor) and linux/amd64 and
linux/arm64 (static musl, in `golang:alpine` via `docker run`); there is no
darwin/amd64 FFI. The executable inside is still `cortex`, and `cortex
--version` prints `cortex (cpex) <ver>`. The workflow's jobs are `version`,
`build`, `build-cpex` (both `contents: read`) and `publish`, which alone
writes, refuses to publish unless every expected tarball arrived, writes one
`checksums.txt` over all of them, and attests every tarball's build
provenance. `install.sh --cpex` installs it; `make cortex-cpex` builds the same
thing locally to `bin/cortex-cpex/cortex`.

**Go modules** (11 in total; `go.work` links 8 of them — the three
self-contained `demos/*` modules are outside the workspace. `go-tidy-check` in
`ci.yaml` does cover all 11: it discovers them with `find`, not from `go.work`):
- `core/` — the runtime library: `pipeline`, `plugins`, `listener`, `config`, `spiffe` (the framework, ~58% of it); `cost/{pricing,settle,ledger,event,usage}` (~17%); `session`, `sessionapi`, `observe`, `redact` (~14%); `auth`, `bypass`, `capabilities` (~1%); plus transport and storage glue. **Consumed outside this repo**, so removing exported API here is a cross-repo change.
- `cmd/cortex{,-envoy,-praxis}/` — thin main packages that import core and start the listeners they need; they import no plugin package directly. (The `authbridge-lite` and `authbridge-cpex` images are `cortex` built with the `lite` and `cpex` profiles; `cmd/cortex-cpex/` holds only the latter's Dockerfile and FFI pins.)
- `cmd/agentop/` — the TUI; also released as a standalone binary.
- `core/storage/redis/`, `scripts/{profile-tags,readme-demo}/`, and the self-contained `demos/{echo,finance-sparc,ibac}/`.
- `go.work` — workspace linking core + the binaries for local development.

**Config format:** YAML with `${ENV_VAR}` expansion, mode presets, and startup validation. The `mode` field must match the binary for all but `cortex-praxis`, which pins no mode.

## Component Details

### Sidecar Runtime Behavior

The mode-specific authbridge binaries handle both traffic directions. Auth logic
and all listener implementations live in `core/` (under `core/listener/`);
each binary's `main.go` imports the listeners it needs and blank-imports
`core/plugins/all`; build tags pick the plugins.

The inbound and outbound auth paths — JWT validation, the route resolver, token
exchange, config loading and the credential files — are in
[`core/plugins/CLAUDE.md`](core/plugins/CLAUDE.md).

**Key library packages (core/):**
- `core/plugins/jwtvalidation/validation/` -- JWKS-backed JWT verifier (used internally by `jwt-validation` plugin)
- `core/plugins/tokenexchange/exchange/` -- RFC 8693 token exchange client (used internally by `token-exchange` plugin)
- `core/plugins/tokenexchange/cache/` -- SHA-256 keyed token cache
- `core/routing/` -- Host-to-audience route resolver (used internally by `token-exchange` plugin)
- `core/auth/` -- `HandleInbound` + `HandleOutbound` composition; each plugin instance constructs its own `auth.Auth` from its own local config
- `core/config/` -- Mode presets, YAML config loader, credential-file waiters, top-level (mode + listener + session) validation
- `core/pipeline/` -- Plugin interface + lifecycle (`Configurable`, `Initializer`, `Shutdowner`); see [`docs/framework-architecture.md`](docs/framework-architecture.md)
- `core/plugins/` -- The concrete plugins + registry; see [`docs/plugin-reference.md`](docs/plugin-reference.md) for the per-plugin config convention

**Directional body capabilities.** `PluginCapabilities` declares body writes
per direction: `WritesRequestBody` (calls `pctx.SetBody` or `pctx.SetRequestModel`)
and `WritesResponseBody` (calls `pctx.SetResponseBody`). `WritesResponseBody` is the
SSE streaming predicate — both proxy listeners fall back from incremental relay
to the buffered path only when some plugin declares it. A request-only mutator
(`tool-prune`, `context-guru`) therefore keeps streaming, because requests are
never streamed in the first place. `pipeline.New` lets any number of mutators
chain in each direction — a request mutator sees `pctx.Body` as the one before
it left it, and the response side runs in reverse — and lets no mutator of
either direction precede a `ReadsBody`-only plugin. Readers, parsers included,
therefore see what the agent saw: the request the client sent and the response
it receives. A writer learns whether its own write applied from
`SetBody`'s result (false under `on_error: observe`), not from
`pctx.BodyMutated()`, which is true once any writer's bytes took effect. See
[`docs/plugin-reference.md`](docs/plugin-reference.md#capability-fields).

**Plugin metrics.** Plugins that implement `pipeline.MetricsProvider` have their
counters surfaced on `GET /v1/pipeline` and rendered in agentop's plugin pane.
Optional interfaces are not promoted through `configuredPlugin`'s embedded
`Plugin`, so a new one must be forwarded there explicitly or it is invisible for
every plugin that has config. Counters are per-process and reset on restart
**and on config hot-reload**.

**Plugin classification.** Protocol parsers (`mcp-parser`, `a2a-parser`, `inference-parser`) populate an `IsAction bool` field on their respective extensions to classify each request as either a user-meaningful action or protocol mechanics. Default-false means "not classified as action" — guardrails treat it as bypass. Parsers explicitly set `IsAction = true` for the small set of action methods (`tools/call` / `prompts/get` / `resources/read` for MCP; `message/send` / `message/stream` for A2A; every populated case for inference). Guardrails (`ibac` today; future rate limiters, audit loggers, etc.) read the aggregated verdict via `pctx.Classification()` which returns `(anyAction, anyBypass)`. A defense-in-depth guardrail skips on `anyBypass`, passes through on `!anyAction` (no parser claimed this traffic), and judges only when `anyAction && !anyBypass`. This puts the protocol-specific bypass-vs-action vocabulary in each parser — adding a new guardrail or new protocol does not multiply work at the guardrail layer. See [`docs/plugin-reference.md` "Classifying requests"](docs/plugin-reference.md#classifying-requests-as-actions-vs-protocol-mechanics) for the contract.

### keycloak_sync.py

Declarative Keycloak synchronization tool that maintains client scope mappings based on `routes.yaml`. Idempotent, used in multi-target demos for dynamic scope assignments.

Dependencies: see [Python Code](#python-code-keycloak_syncpy-demo-setup-scripts).

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

The authbridge binary watches its config file (`/etc/authbridge/config.yaml`) via `core/reloader`. When the ConfigMap changes, kubelet syncs the new content into the mount (~60s), the watcher detects it, and the binary rebuilds + atomically swaps the plugin pipelines without a pod restart. In-flight requests finish on the previous pipeline; new requests go to the new one.

**What reloads:** any plugin list change (add/remove/reorder) and any plugin `config:` subtree edit.

**What doesn't reload (pod restart required):** `mode` and every field of `listener`, `session`, `cost_ledger`, `mtls`, `tls_bridge`, `spiffe` and `memory` (`validateReloadable` in `core/reloader/reloader.go`).

**Bad YAML stays safe:** if Load/Validate/Build/Start fails, the active pipeline keeps serving and the error is exposed on `/reload/status` with `reloads_failed` incremented. The pod never goes unhealthy from a bad edit.

Operator workflow:

```sh
kubectl edit configmap authbridge-config-<agent> -n <ns>
kubectl port-forward -n <ns> deploy/<agent> 9093:9093 &
curl http://localhost:9093/reload/status  # last_success, reloads_ok, active_config_sha256
curl http://localhost:9093/config         # now-active config (ConfigProvider closure)
```

See [`docs/framework-architecture.md`](docs/framework-architecture.md#9-config-hot-reload) §9 for the reload lifecycle, debounce / symlink-swap handling, and the drain-window behavior.

## Top-level `mtls:` configuration

An `mtls:` block (`mode: permissive | strict`, its only field) turns on transport mTLS
on the proxy-sidecar listeners, with the SVID from the spiffe Provider. In
envoy-sidecar mode the block is a no-op: Envoy does mTLS from the `envoy-config`
ConfigMap. Changing the block needs a restart, and a reload that tries is refused.
**Read [`docs/framework-architecture.md` §8a](docs/framework-architecture.md#8a-mtls-layer)
before you change either shape.** Nothing in-tree exercises the Envoy filter chains:
re-verify them by hand after a change.

## CI/CD Workflows

| Workflow | Trigger | Purpose |
|----------|---------|---------|
| `ci.yaml` | PR to main/release-*, push to main | Pre-commit; `go vet` and build for all 11 modules, and tests for all but the three demos; a macOS job for `peerproc`, `tlsbridge`, `cmd/cortex` and `cmd/agentop`; the cgo `cpex` builds (`go-ci-cpex`); `go mod tidy -diff` for all 11 modules; proxy-init iptables, Python, install-script and release-notes-script tests. `go fmt` rewrites rather than fails, so it does not gate |
| `build.yaml` | Tag push (`v*`), push to main, manual; PRs to main build amd64 only and skip `**.md` | Multi-arch Docker builds for all six matrix images: proxy-init, authbridge (proxy-sidecar combined), authbridge-envoy (envoy-sidecar combined), authbridge-lite (proxy Dockerfile built with the `lite` profile from `scripts/profile-tags`), authbridge-cpex, and sparc-service (Python). Every Go image passes `GO_BUILD_TAGS` naming a profile — plugins are all opt-in, so an image built without tags registers none |
| `security-scans.yaml` | PR to main | Dependency review, shellcheck, YAML lint, Hadolint, Bandit, Trivy, CodeQL |
| `scorecard.yaml` | Weekly / push to main | OpenSSF Scorecard security health metrics |
| `release-binaries.yaml` | Tag push (`v*`), manual; push to main publishes the main-latest channel when `MAIN_CHANNEL_ENABLED` is set | Release tarballs — see [Release binaries](#release-binaries) |
| `pr-verifier.yml` | PR | PR title check (below) |
| `spellcheck_action.yml` | PR | Spellcheck on markdown files — **disabled** (`if: false`) |

Also: `release-smoke-linux.yaml`, `dependabot-tidy.yml`, `install-stats.yaml`, `project.yml`, `self-assign.yml`, `stale.yaml`.

### PR Title Convention

PR titles must follow the format:

```
<Prefix>: <Subject starting with uppercase>
```

The CI title check (`deepakputhraya/action-pr-title` via the shared
`pr-verifier` workflow) matches prefixes **case-insensitively**
(`prefix_case_sensitive: false`), accepting this set in any case: `Build`,
`Chore`, `CI`, `Docs`, `Feat`, `Fix`, `Perf`, `Refactor`, `Revert`, `Style`,
`Test`, `Feature`, `Bug fix`, `Proposal`, `Breaking change`, `Other`,
`Other/Misc`. `scripts/release_notes.sh` also lowercases before matching, so
the prefix files the PR the same way in either case. The capitalized spelling
is the repo's convention, not an enforced rule — lowercase commit-style
prefixes (`feat:`, `fix:`) pass.

The prefix also files the PR in the release notes, which
`scripts/release_notes.sh` builds from the PRs merged since the previous
release: `Feat`/`Feature` under New features, `Fix`/`Bug fix`/`Perf` under Bug
fixes, `Breaking change` (or `!` before the colon) under Breaking changes,
`(deps)` titles and dependabot PRs under a folded Dependency updates, and
everything else under a collapsed Other changes.

## Container Images

All images are pushed to `ghcr.io/rossoctl/cortex/` from
`.github/workflows/build.yaml`:

| Image | Source | Description |
|-------|--------|-------------|
| **`authbridge`** | **`cmd/cortex/Dockerfile`** | **proxy-sidecar image (default mode): cortex, full plugin set incl. parsers. No Envoy.** |
| `authbridge-envoy` | `cmd/cortex-envoy/Dockerfile` | envoy-sidecar combined image: Envoy + cortex-envoy (ext_proc, `envoy` profile) |
| `authbridge-lite` | `cmd/cortex/Dockerfile` (+ `GO_BUILD_TAGS` from the `lite` profile) | proxy-sidecar image with a sidecar-minimum plugin set (see `scripts/profile-tags`). A build variant of `authbridge`, not a separate binary; not yet referenced by the operator's default config |
| `authbridge-cpex` | `cmd/cortex-cpex/Dockerfile` | proxy-sidecar build of `cmd/cortex` with the CPEX plugin. Two tag sources: the literal `cpex` tag, always required because it selects the FFI-backed cpex manager and gates `core/plugins/all/plugins_cpex.go`, plus the plugin tags its Dockerfile appends from `GO_BUILD_TAGS` (resolved from the `cpex` profile in `build.yaml`) — `cpex` alone registers no plugins. Links `libcpex_ffi.a` from a pinned CPEX release (CGO_ENABLED=1), fetched and verified by `scripts/fetch-cpex-ffi.sh`. Routes hooks through the CPEX framework (APL DSL + named CPEX policy plugins). The pins are in `cmd/cortex-cpex/`: `CPEX_FFI_VERSION` is the release tag, `CPEX_FFI_ABI` the FFI ABI integer, `CPEX_FFI_SHA256` the per-target tarball digests |
| `proxy-init` | `deploy/proxy-init/Dockerfile.init` | Alpine + iptables init container (envoy-sidecar + proxy-sidecar enforce-redirect modes) |
| `sparc-service` | `deploy/sparc-service/Dockerfile` | Python SPARC reflection service (FastAPI wrapper around the ALTK pre-tool reflection component), called by the `sparc` plugin |

CI no longer builds the legacy `authbridge-unified`, `authbridge-light`,
`client-registration` and `spiffe-helper` images. Older release tags still publish them.

## Pre-commit Hooks

Install: `pre-commit install`

Hooks:
- `trailing-whitespace`, `end-of-file-fixer`, `check-added-large-files` (max 1024KB), `check-yaml`, `check-json`, `check-merge-conflict`, `mixed-line-ending`
- `ai-assisted-by-trailer` — Rewrites `Co-Authored-By` to `Assisted-By` (commit-msg stage)
- `ruff`, `ruff-format` — Python linting/formatting, `exclude: ^tests/`: `tests/`
  does not lint clean, so widening the filter is a separate change.

**There are no Go hooks** — `gofmt` and `go vet` run nowhere in pre-commit. In
`ci.yaml` both run, but only one of them can fail:

- `go vet ./...` **is** a gate, on all 11 modules (`cmd/cortex`'s cpex builds
  and `core/plugins/cpex` are vetted with `-tags cpex` separately, in
  `go-ci-cpex`, which links the pinned `libcpex_ffi.a`).
- `go fmt ./...` is **not** a gate. `go fmt` is `gofmt -l -w`: it rewrites the
  checkout and exits 0, so formatting drift cannot fail a build. Run
  `gofmt -l .` yourself before pushing.

## Languages and Tech Stack

| Area | Technology |
|------|------------|
| AuthBridge sidecar binaries | Go 1.26.10, envoy-control-plane, lestrrat-go/jwx |
| agentop (TUI) | Go 1.26.10, bubbletea |
| keycloak_sync.py / setup scripts | Python 3.12, python-keycloak (`>=7.1.1,<8`) |
| sparc-service | Python 3.10+, FastAPI, agent-lifecycle-toolkit |
| Proxy | Envoy v1.37.1 (pinned by digest in `cmd/cortex-envoy/Dockerfile`) |
| Traffic interception | iptables (via init container) |
| Identity | SPIFFE/SPIRE (JWT-SVIDs) |
| Auth provider | Keycloak (OAuth2/OIDC, token exchange RFC 8693) |
| Packaging | Docker |
| CI | GitHub Actions |

## External Dependencies and Services

| Service | Required | Purpose |
|---------|----------|---------|
| Kubernetes | Yes | Target platform (v1.25+ recommended) |
| [operator](https://github.com/rossoctl/operator) | Yes | Injects AuthBridge sidecars into workload pods |
| Keycloak | Yes | OAuth2/OIDC provider, token exchange |
| SPIRE | Optional | SPIFFE identity (JWT-SVIDs) for workloads |

## Common Development Tasks

Image builds, Kind loads and the end-to-end demo are in
[`demos/CLAUDE.md`](demos/CLAUDE.md#building-everything-locally).

### Adding a New Component Image to CI

1. Add entry to `.github/workflows/build.yaml` matrix (`image_config` array)
2. Provide `name`, `context`, and `dockerfile` fields
3. For a Go image, add a case to the "Resolve build args" step that sets `GO_BUILD_TAGS` from a profile; without it the Dockerfile fails
4. Image will be pushed to `ghcr.io/rossoctl/cortex/<name>`

## Code Style and Conventions

### Go Code
- Run `go vet ./...` and `gofmt -l .` yourself before pushing, from each module you
  touched. **Always give `gofmt` a path** — a bare `gofmt -l` reads stdin, scans
  nothing and exits 0.
- **Only `go vet` is enforced**, by `ci.yaml`, on all 11 modules. pre-commit has no Go
  hooks, and `ci.yaml`'s `go fmt ./...` is `gofmt -l -w`, which rewrites and exits 0.
  See [CONTRIBUTING.md](CONTRIBUTING.md#code-style) for the contributor-facing version.
- Run per-module with `GOWORK=off`, as every CI Go job except the `core` one
  does, so each module resolves its own `replace` directives instead of pulling in
  workspace siblings.
- If a change deletes a package or its last import of a dependency, also run
  `go mod tidy -diff` in every module — `ci.yaml`'s `go-tidy-check` gates on it,
  and `build`/`vet`/`test` all pass while it fails.
- Logging with `log/slog`; the binaries log under their own name
  (`cortex`, `cortex-envoy`). Note the `authbridge-lite` image runs
  the `cortex` binary, so it logs as `cortex`.
- gRPC ext-proc uses `envoyproxy/go-control-plane` types (in `core/listener/extproc`)
- JWT validation uses `lestrrat-go/jwx/v2` (in `core/plugins/jwtvalidation/validation`)

### Python Code (keycloak_sync.py, demo setup scripts)
- Python 3.12+ syntax (type hints with `str | None`)
- Dependencies in `requirements.txt` — `python-keycloak>=7.1.1,<8`.
  Note `ci.yaml`'s Python job still pip-installs `python-keycloak==5.3.1`, two
  majors behind, so CI is not testing the version the project declares.
- `python-keycloak` for all Keycloak admin API calls
- Idempotent: every `get_or_create_*` helper checks existence before creating

### Shell Scripts
- Strict mode where it is safe to add: `scripts/dev/local-build-and-test.sh` uses
  `set -euo pipefail`, `install.sh` uses `set -eu`. **`deploy/proxy-init/init-iptables.sh`
  is `set -e` only** — do not "fix" it to `pipefail` without testing, its iptables
  probes rely on tolerated failures.
- Extensive inline documentation (especially `init-iptables.sh`, which explains
  iptables chain ordering, Istio interactions, and debugging tips)
- Idempotent: `init-iptables.sh` uses `iptables -N ... 2>/dev/null || true` and
  `iptables -F` before adding rules

## Gotchas and Known Issues

1. **Multiple Go modules:** The repo has 11 Go modules — `core/` and each `cmd/*/` but `cmd/cortex-cpex/` at the root, `core/storage/redis/` nested under `core/` but still its own module, both `scripts/*/`, and the three self-contained `demos/*/` ones. `go.work` links the first eight; the `demos/*` modules are deliberately outside the workspace. Local commands from a specific module directory should typically set `GOWORK=off` (as every CI Go job but `core`'s does) so the module resolves its own `replace` directives instead of pulling in workspace siblings.

2. **Avoid committing venvs:** Virtual environment directories should be gitignored (the repo's `.gitignore` has a `venv` pattern). Do not create and commit new virtual environments under version control.

3. **Session store memory, and IBAC user intent under chatty traffic.** The store keeps **every** event a session produces: `session.max_events` is unset and `session.ttl` defaults to never, by design (see `SessionConfig.Limits`). `session.max_sessions` (100, oldest-used first) is the default bound, and it limits how MANY sessions are kept, not how big one gets. Set `max_events` for a FIFO per-session cap and `ttl` to bound how long raw prompts stay in memory. Clients read deep sessions by paging with `?before=<seq>`, not by raising `limit`.

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

    **The session archive is disk, not memory** (`core/session/archive`, on for a local install bound to loopback, off elsewhere: `config.ArchiveRunsOnLocalInstall`). Every memory bound above still applies, but eviction no longer loses history. On disk it interns the same seven fields the store does, and it is bounded by `retention_days` (30) and `max_bytes` (2 GiB). `DELETE /v1/sessions` erases it with the store. A burst it cannot keep up with is dropped from the archive and counted, never from memory. At startup `cmd/cortex` replays the last six hours into the usage ring (`usage.Aggregator.Replay`, not through the store) **beside live traffic, not before it**, so `/healthz` answers at once. The segment list is taken before any listener starts: a process never appends to a segment it did not open, so nothing live is read back. The replay fills a private ring from `BeginReplay`, and `FinishReplay` first applies any clear, delete or rename the live ring took meanwhile. The replay has `usageReplayBudget` (60s) or does not run. User-facing details: [`docs/laptop-service.md`](docs/laptop-service.md).

    Two layered defenses keep the inbound A2A user intent visible to IBAC when an agent sends dozens of outbound events per turn:

    - **Primary fix — `listener.skip_hosts`** ([`docs/framework-architecture.md`](docs/framework-architecture.md#skipping-infrastructure-hosts-listenerskip_hosts) has the example): infrastructure destinations (OTel collectors, metrics, log shippers) whose traffic bypasses the pipeline AND session recording. Do NOT add hosts that need IBAC or token-exchange policy — bypass means bypass. A change needs a pod restart.

    - **Egress boundary — `listener.egress_allow_hosts`**, or `MOCA_EGRESS_ALLOW` when the field is empty: the forward proxy's deny-by-default allow list. An off-list destination is refused 403 `destination_denied` by name, before the pipeline, DNS and any dial (shape: [`docs/session-api.md`](docs/session-api.md#event-schema)). Rules:
      - Entries are exact `host:port`. Portless and wildcard entries are startup errors, and so is an unbracketed IPv6 literal: write `[2001:db8::1]:443`. A destination with no port is judged at 80 for http and 443 otherwise. A malformed authority is denied by name, never a panic.
      - **It is checked BEFORE `skip_hosts`**, so a skip pattern cannot exempt an off-list host.
      - **Scope: the forward proxy only.** The transparent listener (enforce-redirect) and extproc (envoy-sidecar) do not consult it. A deployment that uses either must hold its egress elsewhere: deny-by-default here is not deny-by-default for the pod.
      - It judges the **name**. `listener.egress_guard` judges the resolved **address**, so an allow-list entry is not a permit for the guard.
      - Example: `listener: {egress_allow_hosts: ["github-tool-mcp:8080", "api.anthropic.com:443"]}`, or `MOCA_EGRESS_ALLOW=github-tool-mcp:8080,api.anthropic.com:443`. A change needs a pod restart, and each refusal leaves a `phase: "denied"` row with an `egress-allowlist` invocation.

    - **Backstop — intent pin**, only when `session.max_events` is set: the store pins the most recent inbound A2A request against FIFO eviction. Only the latest intent is pinned, so stale intents cannot starve the buffer. IBAC's `LastIntent()` can still return nil after deletion, `max_sessions` eviction, a `ttl` expiry, or before the first inbound request. Reach for `skip_hosts` first when the noisy traffic is identifiable infrastructure.

## DCO Sign-Off (Mandatory)

All commits **must** include a `Signed-off-by` trailer (Developer Certificate of Origin).
Always use the `-s` flag when committing:

```sh
git commit -s -m "feat: Add new feature"
```

This adds a line like `Signed-off-by: Your Name <your@email.com>` to the commit message.
PRs without DCO sign-off will fail CI checks. To retroactively sign-off existing commits:

```sh
git rebase --signoff main
```

## Orchestration

This repo includes orchestrate skills for enhancing related repositories.
Run `/orchestrate <repo-url>` to start.

| Skill | Description |
|-------|-------------|
| `orchestrate` | Entry point — scan, plan, execute phases |
| `orchestrate:scan` | Assess repo structure, CI, security gaps |
| `orchestrate:plan` | Create phased enhancement plan |
| `orchestrate:precommit` | Add pre-commit hooks and linting |
| `orchestrate:tests` | Add test infrastructure |
| `orchestrate:ci` | Add CI/CD workflows |
| `orchestrate:security` | Add security governance files |
| `orchestrate:replicate` | Bootstrap skills into target repo |
| `orchestrate:review` | Review all orchestration PRs before merge |

Skills management:

| Skill | Description |
|-------|-------------|
| `skills` | Skills router — create, validate, scan |
| `skills:write` | Create or edit skills with proper structure |
| `skills:validate` | Validate skill format and naming |
| `skills:scan` | Audit repo for skill gaps |

## Commit Attribution Policy

When creating git commits, do NOT use `Co-Authored-By` trailers for AI attribution.
Instead, use `Assisted-By` to acknowledge AI assistance without inflating contributor stats:

    Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>

Never add `Co-authored-by`, `Made-with`, or similar trailers that GitHub parses as co-authorship.

### PR Bodies

PR descriptions end with the same `Assisted-By` trailer. Do not use `🤖 Generated with [Claude Code](https://claude.com/claude-code)` or similar.

A `commit-msg` hook in `scripts/hooks/commit-msg` rewrites a `Co-authored-by:` trailer that names Claude, Cursor or Anthropic into `Assisted-By`, and removes `Made-with:` lines. It never fails a commit, and it does nothing for PR bodies.
Install it via pre-commit:

```sh
pre-commit install --hook-type pre-commit --hook-type commit-msg
```
