# CLAUDE.md - Rossoctl Extensions

This file provides context for Claude (AI assistant) when working with the `cortex` monorepo.

## AI Assistant Instructions

- **Always work in your own git worktree — never in the shared top-level checkout.**
  Several Claude Code sessions run against this repo at once. They share one object
  store, which is fine, but a shared *working tree* is not: `git checkout` in one
  session rewrites files under another, and two sessions' uncommitted edits land in one
  index. Before starting work:

  ```sh
  # Fetch into a ref only you write, so nothing can move it underneath you.
  git fetch https://github.com/rossoctl/cortex.git main:refs/base/<topic>
  git worktree add .worktrees/<topic> -b <branch> refs/base/<topic>
  ```

  Then stay in that directory. Leave the top-level checkout alone — treat it as a
  reference copy someone else may be using. When the branch is merged or abandoned, tear
  down all three things you created:

  ```sh
  git worktree remove .worktrees/<topic>   # --force if untracked files remain
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
    That is how `CLAUDE.md` got reverted mid-session to a pre-rename state. Nothing but
    you writes `refs/base/<topic>`, so there is no window to lose and no verification
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
    it. `abctl service restart` always replaces that instance and cuts every attached
    session. `abctl service install` only does so when it has something to change or a
    running proxy to adopt — with nothing to do it prints `Already current` and leaves
    the proxy alone. `--ref=main` is an `install.sh` flag, not an `abctl` one; it picks
    which installer script runs, so whether it interrupts anything depends on what that
    install then finds. Coordinate before any of it.

- **Use `Assisted-By` for attribution** — never add `Co-Authored-By`, `Generated with Claude Code`, or similar trailers. See [Commit Attribution Policy](#commit-attribution-policy) below.

## Repository Overview

**cortex** contains Kubernetes security extensions for the [Rossoctl](https://github.com/rossoctl/rossoctl) ecosystem. It provides **zero-trust authentication** for Kubernetes workloads through transparent token exchange and dynamic Keycloak client registration using SPIFFE/SPIRE identities.

The sidecar injection webhook lives in a separate repo: [rossoctl/operator](https://github.com/rossoctl/operator).

**GitHub:** `github.com/rossoctl/cortex`
**Container registry:** `ghcr.io/rossoctl/cortex/<image-name>`
**License:** Apache 2.0

## What AuthBridge Does

AuthBridge provides **zero-trust, transparent token management** for Kubernetes workloads. It combines three capabilities:

1. **Automatic Identity** -- Workloads obtain SPIFFE IDs from SPIRE and auto-register as Keycloak clients
2. **Inbound JWT Validation** -- Incoming requests are validated (signature, issuer, audience) by the authbridge binary
3. **Outbound Token Exchange** -- Outgoing requests get their tokens automatically exchanged for the correct target audience (OAuth 2.0 RFC 8693)

All of this happens transparently via sidecar injection -- no application code changes required.

## Top-Level Directory Structure

There is no `authbridge/` subdirectory: what used to live there is the repo root.

```
cortex/
├── authlib/                          # Shared auth library (Go module)
│   ├── plugins/                      #   Every plugin; each owns its own config
│   │   ├── jwtvalidation/            #     JWKS-backed JWT verifier (validation/)
│   │   └── tokenexchange/            #     RFC 8693 exchange client + token cache
│   ├── bypass/                       #   Path pattern matcher
│   ├── spiffe/                       #   SPIFFE credential sources (in-process
│   │                                 #   Workload API client + /opt file mirror)
│   ├── routing/                      #   Host-to-audience router
│   ├── auth/                         #   HandleInbound + HandleOutbound composition
│   ├── listener/                     #   All listener implementations (extproc, proxies)
│   └── config/                       #   Mode presets, YAML config, validation
│
├── cmd/authbridge-proxy/             # proxy-sidecar mode (default). Full plugin set.
│   ├── main.go                       #   (the authbridge-lite image is this binary
│   │                                 #    built with the `lite` profile's tags)
│   ├── Dockerfile                    #   proxy-sidecar image (the `authbridge` image)
│   └── entrypoint.sh
│
├── cmd/authbridge-envoy/             # envoy-sidecar mode. Full plugin set.
│   ├── main.go
│   ├── Dockerfile                    #   envoy-sidecar combined image (Envoy + authbridge-envoy)
│   └── entrypoint.sh
│
├── cmd/authbridge-cpex/              # proxy-sidecar mode + cpex plugin. -tags cpex, cgo required.
│   ├── main.go
│   ├── Dockerfile                    #   proxy-sidecar build linking libcpex_ffi.a
│   ├── CPEX_FFI_VERSION              #   pinned CPEX FFI ABI version (build-arg source of truth)
│   └── entrypoint.sh
│
├── cmd/authbridge-praxis/            # proxy-sidecar rendered into a Praxis proxy config.
│   ├── main.go                       #   PAUSED: ships in no image, registers no
│   ├── Dockerfile                    #   plugins, has no demo — but deliberately kept
│   └── entrypoint.sh                 #   and kept compiling. Do not delete.
│
├── cmd/abctl/                        # Terminal UI over the session API (:9094).
│   ├── tui/                          #   Panes: sessions, events, pipeline, catalog
│   ├── edit/, apiclient/,            #   Pipeline editing, API client, cluster
│   │   cluster/, toolscan/           #   port-forward, tool manifest scanning
│   └── README.md                     #   Full flags + keybindings
│
├── cmd/README.md                     # Which binary pins which deployment shape
│
├── proxy-init/                       # iptables init container (envoy-sidecar + proxy-sidecar enforce-redirect modes)
│   ├── init-iptables.sh              #   iptables setup script
│   ├── Dockerfile.init               #   proxy-init container image
│   ├── Makefile                      #   docker-build-init + load-image targets
│   └── README.md
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
│   ├── local-build-and-test.sh       # Build every image and load it into Kind
│   ├── verify-spire-keycloak.sh      # Platform preflight for a local dev cluster
│   ├── profile-tags/                 # Build-tag resolver: one profile per artifact
│   ├── readme-demo/                  # Generates the README demo animation
│   └── hooks/commit-msg              # Rewrites Co-Authored-By to Assisted-By
│
├── storage/redis/                    # Redis driver for the storage.Store interface
│                                     # (its own module)
│
├── sparc-service/                    # Python SPARC reflection service (own image)
├── lineage-attach/                   # OTel shim + scripts for lineage propagation
│
├── demos/                            # 12 scenarios — see demos/README.md for the order
│   ├── weather-agent/                #   Getting started (+ advanced, + abctl walkthrough)
│   ├── github-issue/                 #   Token exchange + scope-based access (largest)
│   ├── token-exchange-routes/        #   Routes config reference
│   ├── mcp-parser/                   #   Enabling the outbound mcp-parser plugin
│   ├── ibac/, hr-cpex/,              #   Guardrail / policy demos
│   │   finance-sparc/                #   (echo, ibac and finance-sparc are
│   ├── echo/, mtls/, lineage/        #    self-contained Go modules)
│   ├── session-budget/               #   Redis-backed budget tracking
│   └── context-guru/                 #   Opt-in context-guru plugin
│
├── keycloak_sync.py                  # Declarative Keycloak sync tool (routes.yaml driven)
├── tests/                            # Python tests (keycloak_sync)
├── go.work                           # Workspace linking 9 of the 12 Go modules
├── install.sh                        # Laptop installer (abctl + the local proxy service)
├── .github/
│   ├── workflows/                    # CI/CD (ci.yaml, build.yaml, release-binaries.yaml,
│   │                                 # security-scans, scorecard, spellcheck)
│   └── ISSUE_TEMPLATE/               # Bug report, feature request, epic templates
├── .pre-commit-config.yaml           # Pre-commit hooks (whitespace, yaml/json, ruff; no Go hooks)
└── CLAUDE.md                         # This file — the only one in the repo
```

## Major Components

### 1. AuthBridge Binaries (Go)

**Sidecar binaries** providing transparent traffic interception for both inbound JWT validation and outbound OAuth 2.0 token exchange (RFC 8693). All sidecar binaries but `authbridge-praxis` pin one deployment shape and refuse a mismatching `mode:` at boot; mode is no longer selected at runtime. The `authbridge-lite` **image** is a build variant of the proxy binary (not a separate binary) — see below.

**Library:** `authlib/` — see [`authlib/README.md`](authlib/README.md) for the library reference
**Language:** Go 1.26.5 (`go.work` and the nine workspace modules; the
three self-contained `demos/*` modules are still on 1.24)
**Deployment shapes:** [`cmd/README.md`](cmd/README.md) — which binary pins which shape
**Framework internals:** [`docs/framework-architecture.md`](docs/framework-architecture.md), [`docs/plugin-reference.md`](docs/plugin-reference.md), [`docs/architecture.md`](docs/architecture.md)

See the binary-by-binary table below (image, mode, listeners, plugins) for
the full enumeration — kept as the single list rather than repeated here.

**Common:**
- `authlib/` — shared auth library (JWT validation, token exchange, caching, routing, all listener implementations, all plugins).
- `proxy-init/init-iptables.sh` — traffic interception setup (Istio ambient mesh compatible). Used by envoy-sidecar mode (`redirect`) and by proxy-sidecar mode's `enforce-redirect` egress guard.
- `proxy-init/Dockerfile.init` — proxy-init container image.

**Ports (envoy-sidecar):** 15123 (outbound), 15124 (inbound), 9090 (ext-proc), 9901 (admin)
**Ports (proxy-sidecar / lite):** 8080 (reverse proxy), 8081 (forward proxy), 9091 (health), 9093 (stats), 9094 (session API)

### 2. Client Registration

Keycloak client registration for workloads is handled by the
**operator** (separate repo) — see `operator/docs/operator-managed-client-registration.md`.
The operator creates a Secret with `client-id.txt` + `client-secret.txt`
and the webhook mounts it at `/shared/` in the workload pod. The
in-pod `client-registration` sidecar that previously lived in this
repo has been removed.

## How the Components Work Together

The operator (in a separate repo) injects AuthBridge sidecars
into workload pods. Default deployment shape (proxy-sidecar mode):

```
         ┌────────────────────────────────────┐
         │            WORKLOAD POD            │
         │                                    │
         │  authbridge-proxy ──► SPIRE Agent  │  (in-process
         │    - spiffe.Provider reads SVIDs   │   Workload API
         │      over the Workload API and     │   client; shaped
         │      mirrors them under /opt/      │   by the `spiffe:`
         │    - Reverse proxy: inbound JWT    │   config block)
         │    - Forward proxy: outbound       │
         │      token exchange                │
         │       │                            │
         │  Your Application                  │
         │    (HTTP_PROXY → forward proxy)    │
         └────────────────────────────────────┘

         The operator also creates a Secret with client-id +
         client-secret and mounts it at /shared/.

         For envoy-sidecar mode, replace authbridge-proxy with
         the authbridge-envoy image (Envoy + ext_proc) and add a
         proxy-init container for iptables.
```

There is no `spiffe-helper` sidecar and no `SPIRE_ENABLED` gate. SPIRE
credentials are fetched in-process by `authlib/spiffe`'s Provider.

## AuthBridge Binaries

Sidecar binaries, one Dockerfile each; the `authbridge-lite` image is a build variant of the proxy binary (proxy Dockerfile + the `lite` profile's tags):

| Binary | Mode | Listeners | Plugins |
|--------|------|-----------|---------|
| `cmd/authbridge-proxy/` | proxy-sidecar (default) | HTTP forward + reverse proxies | full (incl. parsers) |
| `cmd/authbridge-envoy/` | envoy-sidecar | gRPC ext_proc on :9090 | full (incl. parsers) |
| `cmd/authbridge-cpex/` | proxy-sidecar | HTTP forward + reverse proxies | full + `cpex` (cgo) |
| `cmd/authbridge-praxis/` | proxy-sidecar | HTTP (Praxis-rendered) | **none** — paused, see [`cmd/README.md`](cmd/README.md) |
| `authbridge-lite` _(image: proxy + `lite` profile)_ | proxy-sidecar | HTTP forward + reverse proxies | sidecar-minimum plugin set (see `scripts/profile-tags`) |

Every sidecar binary but praxis pins one deployment shape and refuses a
mismatching `mode:` at boot; praxis pins none. Mode is no longer selected at
runtime. See [`cmd/README.md`](cmd/README.md) for which binary pins which shape.

`cmd/abctl/` is also a Go module here but is not a sidecar — it is the
operator-facing TUI over the session API, and the component the root README leads
with. See [`cmd/abctl/README.md`](cmd/abctl/README.md) for flags and keybindings.

**Plugins are all opt-in.** Each one lives in a `cmd/*/plugins_<name>.go` file
gated by `//go:build include_plugin_<name>` (15 such files in
`cmd/authbridge-proxy/`); `main.go` imports no plugin package directly, so a build
with no `-tags` registers no plugins at all and rejects every config it is handed.
Tag sets come from [`scripts/profile-tags`](scripts/profile-tags/), one profile per
shipped artifact.

### Release binaries

`v*` tag pushes trigger `.github/workflows/release-binaries.yaml`, which
cross-compiles `authbridge-proxy` and `abctl` for linux/darwin ×
amd64/arm64 and attaches tarballs to the GitHub Release. `authbridge-proxy`
ships in variants that mirror the container images:

| Variant | Tarball name shape | Matches |
|---|---|---|
| unqualified (default plugins) | `authbridge-proxy_<ver>_<os>_<arch>.tar.gz` | `authbridge` image |
| `-lite` (sidecar-minimum plugin set — see `scripts/profile-tags`) | `authbridge-proxy-lite_<ver>_<os>_<arch>.tar.gz` | `authbridge-lite` image |
| `-sessionbudget` (default + opt-in session-budget) | `authbridge-proxy-sessionbudget_<ver>_<os>_<arch>.tar.gz` | no image today |

One variant per opt-in plugin currently offered for try-out (today:
`-sessionbudget`) — never enumerate combos. To add one, append to the
`proxy_variants` array in the workflow. `authbridge-cpex` stays image-only
(needs cgo); `context-guru` is opt-in but not yet offered as a variant.

**Go modules** (12 in total; `go.work` links 9 of them — the three
self-contained `demos/*` modules are outside the workspace. `go-tidy-check` in
`ci.yaml` does cover all 12: it discovers them with `find`, not from `go.work`):
- `authlib/` — pure library: validation, exchange, cache, bypass, spiffe, routing, auth, config, all listener implementations, all plugins. **Consumed outside this repo**, so removing exported API here is a cross-repo change.
- `cmd/authbridge-{proxy,envoy,cpex,praxis}/` — thin main packages that import authlib and start the listeners they need; they import no plugin package directly. (The `authbridge-lite` image is `authbridge-proxy` built with the `lite` profile.)
- `cmd/abctl/` — the TUI; also released as a standalone binary.
- `storage/redis/`, `scripts/{profile-tags,readme-demo}/`, and the self-contained `demos/{echo,finance-sparc,ibac}/`.
- `go.work` — workspace linking authlib + the binaries for local development.

**Config format:** YAML with `${ENV_VAR}` expansion, mode presets, and startup validation. The `mode` field must match the binary for all but `authbridge-praxis`, which pins no mode.

## Component Details

### Sidecar Runtime Behavior

The mode-specific authbridge binaries handle both traffic directions. Auth logic
and all listener implementations live in `authlib/` (under `authlib/listener/`);
each binary's `main.go` just imports the listeners it needs and the plugins it
wants to register.

**Inbound path** (`x-authbridge-direction: inbound`):
- Validates JWT signature via JWKS (auto-refreshing cache from `TOKEN_URL`-derived JWKS endpoint)
- Validates issuer claim against `ISSUER` env var
- Validates audience against `CLIENT_ID` (from `/shared/client-id.txt` or env var)
- Returns 401 with JSON error body for invalid/missing tokens
- Removes `x-authbridge-direction` header before forwarding to app

**Outbound path** (no direction header):
- Default policy is **passthrough** -- outbound requests pass through unchanged unless a route matches
- Uses a **route resolver** to match the request's `Host` header against patterns in `authproxy-routes` ConfigMap
- If a route matches: reads `target_audience` and `token_scopes` from the route, obtains a token via `client_credentials` grant, and injects it as `Authorization: Bearer <token>`
- If no route matches: applies the default outbound policy (`passthrough` or `exchange`)
- Returns 503 if exchange fails for a routed host (prevents unauthenticated calls)
- The `DEFAULT_OUTBOUND_POLICY` env var controls the fallback behavior (default: `passthrough`)

**Route resolver (outbound):**
- Reads `/etc/authproxy/routes.yaml` (default path; override with `ROUTES_CONFIG_PATH` env var in standalone deployments)
- Each route entry has: `host` (glob pattern), `target_audience`, `token_scopes`
- Host matching uses `filepath.Match` semantics (supports `*`, `?`, `[...]` patterns)
- Most commonly, `host` is a plain Kubernetes service name (e.g., `github-tool-mcp`) because the HTTP client sets the Host header from the URL hostname
- Routes file is loaded once at startup; restart the pod to pick up changes

**Configuration loading:** (config format is covered above, in the binaries section.)
- Plugin settings are local to each plugin under `pipeline.*.plugins[].config`; the runtime YAML itself only carries `mode`, `listener`, `session`, `stats`, and the pipeline composition. See [`docs/plugin-reference.md`](docs/plugin-reference.md) for the per-plugin decode pattern.
- The operator-supplied env vars (`KEYCLOAK_URL`, `KEYCLOAK_REALM`, `TOKEN_URL`, `ISSUER`, `DEFAULT_OUTBOUND_POLICY`, `CLIENT_ID`) are consumed by the default `authbridge-runtime-config` via `${VAR}` expansion — they land inside the appropriate plugin's `config:` block rather than a top-level section.
- `jwt-validation` derives `jwks_url` from `issuer` when omitted (appends `/protocol/openid-connect/certs`).
- `token-exchange` derives `token_url` from `keycloak_url + keycloak_realm` when omitted (Keycloak convention).
- Credential files: the **operator** registers each workload with Keycloak and creates a Secret containing `client-id.txt` + `client-secret.txt`; the operator's webhook mounts that Secret at `/shared/client-id.txt` and `/shared/client-secret.txt` in containers that share the `shared-data` volume. SPIRE-issued credentials are sourced in-process via the `spiffe.Provider` (built from the top-level `spiffe:` block in `authbridge-runtime-config`) — authbridge's hot path reads X.509 SVIDs from an in-memory `spiffe.X509Source` (no per-handshake file I/O), and `token-exchange` consumes a JWT-SVID from the injected Provider via `plugins.BuildWithSPIFFE`. The Provider also mirrors `/opt/jwt_svid.token`, `/opt/svid.pem`, `/opt/svid_key.pem`, and `/opt/svid_bundle.pem` for external readers (e2e probes, debugging, future Envoy filesystem SDS). The `spiffe-helper` binary is no longer bundled in any image, and the `SPIRE_ENABLED` env var no longer gates anything. `jwt-validation` reads the audience from `/shared/client-id.txt` via `audience_file`; `token-exchange` reads client credentials via `client_id_file` / `client_secret_file`. Each plugin attempts a synchronous read at Configure time and falls back to a background poll from its `Init` goroutine if the file isn't yet readable. The legacy in-pod `client-registration` sidecar has been removed entirely; the `rossoctl.io/client-registration-inject: "true"` label is **no longer functional** — the operator's `ClientRegistrationReconciler` still treats it as a "skip operator-managed registration" signal (`SkipReason` in `operator/internal/clientreg/names.go:58`), but the legacy sidecar that the label deferred to is gone. Setting it today silently breaks registration; do not add it to new manifests.
- Outbound route config: `token-exchange` reads `/etc/authproxy/routes.yaml` by default (path is per-plugin, configured via `routes.file` in its config block); inline rules can be declared under `routes.rules`.
- Outbound `default_policy`: `passthrough` (default) or `exchange`, configured per-plugin (no top-level `DEFAULT_OUTBOUND_POLICY` field anymore; the env var is still expanded into the plugin config by `authbridge-runtime-config`).

**Key library packages (authlib/):**
- `authlib/plugins/jwtvalidation/validation/` -- JWKS-backed JWT verifier (used internally by `jwt-validation` plugin)
- `authlib/plugins/tokenexchange/exchange/` -- RFC 8693 token exchange client (used internally by `token-exchange` plugin)
- `authlib/plugins/tokenexchange/cache/` -- SHA-256 keyed token cache
- `authlib/routing/` -- Host-to-audience route resolver (used internally by `token-exchange` plugin)
- `authlib/auth/` -- `HandleInbound` + `HandleOutbound` composition; each plugin instance constructs its own `auth.Auth` from its own local config
- `authlib/config/` -- Mode presets, YAML config loader, credential-file waiters, top-level (mode + listener + session) validation
- `authlib/pipeline/` -- Plugin interface + lifecycle (`Configurable`, `Initializer`, `Shutdowner`); see [`docs/framework-architecture.md`](docs/framework-architecture.md)
- `authlib/plugins/` -- The concrete plugins + registry; see [`docs/plugin-reference.md`](docs/plugin-reference.md) for the per-plugin config convention

**Directional body capabilities.** `PluginCapabilities` declares body writes
per direction: `WritesRequestBody` (calls `pctx.SetBody`) and
`WritesResponseBody` (calls `pctx.SetResponseBody`). `WritesResponseBody` is the
SSE streaming predicate — both proxy listeners fall back from incremental relay
to the buffered path only when some plugin declares it. A request-only mutator
(`tool-prune`, `context-guru`) therefore keeps streaming, because requests are
never streamed in the first place. `pipeline.New` allows at most one mutator per
direction, and no mutator of either direction may precede a `ReadsBody`-only
plugin. See [`docs/plugin-reference.md`](docs/plugin-reference.md#capability-fields).

**Plugin metrics.** Plugins that implement `pipeline.MetricsProvider` have their
counters surfaced on `GET /v1/pipeline` and rendered in abctl's plugin pane.
Optional interfaces are not promoted through `configuredPlugin`'s embedded
`Plugin`, so a new one must be forwarded there explicitly or it is invisible for
every plugin that has config. Counters are per-process and reset on restart
**and on config hot-reload**.

**Plugin classification.** Protocol parsers (`mcp-parser`, `a2a-parser`, `inference-parser`) populate an `IsAction bool` field on their respective extensions to classify each request as either a user-meaningful action or protocol mechanics. Default-false means "not classified as action" — guardrails treat it as bypass. Parsers explicitly set `IsAction = true` for the small set of action methods (`tools/call` / `prompts/get` / `resources/read` for MCP; `message/send` / `message/stream` for A2A; every populated case for inference). Guardrails (`ibac` today; future rate limiters, audit loggers, etc.) read the aggregated verdict via `pctx.Classification()` which returns `(anyAction, anyBypass)`. A defense-in-depth guardrail skips on `anyBypass`, passes through on `!anyAction` (no parser claimed this traffic), and judges only when `anyAction && !anyBypass`. This puts the protocol-specific bypass-vs-action vocabulary in each parser — adding a new guardrail or new protocol does not multiply work at the guardrail layer. See [`docs/plugin-reference.md` "Classifying requests"](docs/plugin-reference.md#classifying-requests-as-actions-vs-protocol-mechanics) for the contract.

### init-iptables.sh

Extensively documented shell script that sets up iptables for transparent traffic interception. Key features:

- **Outbound**: `PROXY_OUTPUT` chain in `nat OUTPUT`, redirects to Envoy port 15123
- **Inbound**: `PROXY_INBOUND` chain in `nat PREROUTING`, redirects to Envoy port 15124
- **Istio ambient mesh coexistence**: Handles ztunnel fwmark (0x539), HBONE port (15008), DNAT to POD_IP for inbound interception
- **Exclusions**: SSH (22), loopback, configurable `OUTBOUND_PORTS_EXCLUDE` and `INBOUND_PORTS_EXCLUDE`
- **Envoy UID 1337**: Excluded from outbound redirect to prevent loops
- **Mangle rule**: Sets fwmark on Envoy's local delivery to prevent ISTIO_OUTPUT redirect loop
- Uses `-I 1` (insert first) for chain ordering stability with Istio CNI

**Environment variables:**
| Variable | Default | Description |
|----------|---------|-------------|
| `PROXY_PORT` | 15123 | Envoy outbound listener |
| `INBOUND_PROXY_PORT` | 15124 | Envoy inbound listener |
| `PROXY_UID` | 1337 | Envoy process UID (excluded from redirect) |
| `OUTBOUND_PORTS_EXCLUDE` | (empty) | Comma-separated ports to exclude |
| `INBOUND_PORTS_EXCLUDE` | (empty) | Comma-separated ports to exclude |
| `POD_IP` | (required) | Pod IP via Downward API; used as DNAT target for ambient mesh inbound interception |

### keycloak_sync.py

Declarative Keycloak synchronization tool that maintains client scope mappings based on `routes.yaml`. Idempotent, used in multi-target demos for dynamic scope assignments.

**Dependencies:** `requirements.txt` — `python-keycloak>=7.1.1,<8`.
Note `ci.yaml` pip-installs `python-keycloak==5.3.1` for the Python test job,
two majors behind what the project declares.

There is no longer a `client_registration.py` in this repo. Workload
registration with Keycloak is the operator's job
(`ClientRegistrationReconciler`), which creates the Secret carrying
`client-id.txt` + `client-secret.txt` that the webhook mounts at `/shared/`.

### Envoy Configuration

Envoy config lives in the `envoy-config` ConfigMap rendered by the [rossoctl Helm chart](https://github.com/rossoctl/rossoctl) at install time (template: `charts/rossoctl/templates/agent-namespaces.yaml` / `authbridge-template-configmaps.yaml`). Key listeners: `outbound_listener` (15123), `inbound_listener` (15124). Inbound listener injects `x-authbridge-direction: inbound` header. Both use ext_proc cluster pointing to the authbridge binary on localhost:9090.

## Important Port Mapping

| Port | Component | Protocol | Purpose |
|------|-----------|----------|---------|
| 15123 | Envoy | TCP | Outbound listener (iptables redirects app traffic here) |
| 15124 | Envoy | TCP | Inbound listener (iptables redirects incoming traffic here) |
| 9090 | authbridge | gRPC | Ext-proc server (called by Envoy) |
| 9093 | authbridge | HTTP | Stats + config inspection (`/stats`, `/config`, `/reload/status`) |
| 9094 | authbridge | HTTP | Session events API (JSON snapshots + SSE stream) |
| 9901 | Envoy | HTTP | Admin interface (bound to 127.0.0.1) |

## Session Events API (`:9094`)

When `session.enabled` is true (default) and `listener.session_api_addr` is non-empty (default `:9094`), the authbridge binary exposes the captured session store over HTTP. Intended for operators debugging the plugin pipeline via `kubectl port-forward` and for the `abctl` TUI.

**Trust model:** no authentication. Bind only on in-cluster addresses, never behind ingress. Payloads may contain raw user messages, LLM completions, and tool results.

### Endpoints

| Method & Path | Format | Purpose |
|---|---|---|
| `GET /` | text | One-line-per-endpoint index. Answers "is this the session API, and on the right port?" — the reason a 404 here was worth replacing. |
| `GET /v1/sessions` | `application/json` | List active sessions: `{sessions: [{id, createdAt, updatedAt, eventCount, active}]}`. |
| `GET /v1/sessions/{id}` | `application/json` | The session's most recent events. `?limit=N` (default 500, max 2000) sets the window; `?before=<seq>` returns the page ending just before that event, so the whole session is reachable by paging backward from the tail. `totalEvents` is the session's true length and `oldestSeq` the oldest event the store still holds — both present only when this response is not the whole session, so a client can tell "this is the beginning" from "there is more behind me" without a second request. 404 if unknown/expired. **One response is still not a full snapshot:** with `session.max_events` unset a session can hold thousands of events, and one real session's whole history encoded to 1.1GB — 17s to write, against clients that time out in 10. That cap is why `before` exists — until it did, a session past 2000 events had a beginning no request could reach at any limit, while still costing memory. The response is written one event at a time rather than encoded whole, so serving it costs the proxy heap proportional to one event; see the chatty-traffic gotcha below. |
| `GET /v1/events` | `text/event-stream` | SSE stream of new events. Optional `?session=<id>` filters to one session. Heartbeat every 30s. |
| `GET /v1/pipeline` | `application/json` | Active pipeline composition: `{inbound: [...], outbound: [...]}`. Each plugin entry carries `name`, `direction`, `position`, `readsBody`, plus the static metadata (`requires`, `requiresAny`, `description`) and runtime `config` when present. abctl renders this as the Pipeline pane. |
| `GET /v1/plugins` | `application/json` | Catalog of every registered plugin (whether or not in the active pipeline): `{plugins: [{name, requires, requiresAny, description, ...}]}`. abctl renders this as the Catalog pane (`P` key). 404s when the binary's session API was constructed without `WithCatalog`. |
| `GET /healthz` | text | Liveness probe. |

### Quick examples

The `abctl` TUI handles port-forward + connection automatically — pick a
pod from the Namespaces → Pods picker. For raw HTTP exploration, set up
your own port-forward first:

```sh
POD=$(kubectl get pod -n team1 -l app.kubernetes.io/name=weather-agent \
  -o jsonpath='{.items[0].metadata.name}')
kubectl port-forward -n team1 $POD 9094:9094 &

# List sessions
curl -s http://localhost:9094/v1/sessions | jq

# Snapshot the most recently updated session
SID=$(curl -s http://localhost:9094/v1/sessions | jq -r '.sessions[0].id')
curl -s "http://localhost:9094/v1/sessions/$SID" | jq

# Page backward: the oldest event this response carries is the next cursor.
# Repeat until the first event's seq equals oldestSeq — that is the beginning.
curl -s "http://localhost:9094/v1/sessions/$SID?limit=500" \
  | jq '{cursor: .events[0].seq, oldestSeq, totalEvents}'
curl -s "http://localhost:9094/v1/sessions/$SID?limit=500&before=<cursor>" | jq '.events|length'

# Live tail every event
curl -N http://localhost:9094/v1/events

# Live tail a single session
curl -N "http://localhost:9094/v1/events?session=$SID"
```

### Event schema

Every event on `/v1/sessions/{id}` and `/v1/events` carries:

- `at`, `direction`, `phase` — when, which side, what stage. `phase` is one of `"request"`, `"response"`, or `"denied"` (terminal denial from a pipeline plugin — typically a jwt-validation failure).
- `seq` — the event's position in its session, counting from 1, and the cursor `?before=` takes. Gaps are normal: FIFO eviction drops a prefix, and the pinned intent can sit far ahead of the retained tail. Absent (zero) from a proxy that predates paging, which is how a client tells it cannot page there. **Unique within one incarnation of a session, not forever:** trimming events never reuses their numbers, but the counter lives on the store entry, and whole-session eviction (`session.ttl`, `max_sessions`) deletes that entry — so a session re-created under the same id restarts at 1. A cursor from before that point is above everything held, and the endpoint answers with the tail, since every event it holds does precede the cursor. A paging client must therefore order pages by `at`, not by `seq` (abctl does); the store cannot tell a re-created session from a trimmed one.
- `a2a` / `mcp` / `inference` — protocol parser payloads (one at most).
- `invocations` — per-plugin invocation records for every plugin that ran on the pipeline pass. Structured as `{inbound: [...], outbound: [...]}`; each entry carries `plugin`, `action` (one of 5 values — see below), `reason` (machine-stable code), and optional plugin-specific context (expected issuer, target audience, cache-hit flag, path, etc.). abctl renders one row per invocation, so operators see an explicit per-plugin timeline.
- `plugins` — escape-hatch map for plugin-specific observability. Keys are plugin names; values are the raw JSON each plugin emitted. Unknown plugins render as opaque JSON in abctl. See [`docs/plugin-reference.md`](docs/plugin-reference.md#emitting-session-events) for the producer contract.
- `identity`, `host`, `statusCode`, `error`, `durationMs` — request-level context.
- `httpMethod`, `httpPath` — the HTTP verb and path, so a request no parser recognized is still identifiable rather than showing only a host. Distinct from the `method` inside `a2a` / `mcp`, which is a protocol method name. On an opaque tunnel `httpMethod` is `CONNECT` and `httpPath` is absent — opaque bytes carry no request line. The path is query-stripped and percent-decoded, so query-borne credentials never reach the timeline, but a secret in a path *segment* (a bot token, a webhook path) does survive on this unauthenticated surface — worth knowing before exporting events off-box.

### Invocation action vocabulary

Every plugin emits one of these 5 action values per invocation, so operators can scan a timeline without memorizing plugin-specific verbs:

| `action` | Meaning | Example |
|---|---|---|
| `allow` | Gate plugin permitted the request | jwt-validation on valid token |
| `deny` | Gate plugin rejected the request; pipeline stops | jwt-validation on bad token, token-exchange on IdP failure |
| `skip` | Plugin ran but didn't act on this message | jwt-validation on a bypass path; parser whose body didn't match |
| `modify` | Plugin mutated the message | token-exchange replaced the Authorization header |
| `observe` | Plugin attached diagnostic data without changing flow | a2a-parser, mcp-parser, inference-parser when they match |

Use `reason` to discriminate within an action — e.g. `skip/path_bypass` vs `skip/no_matching_route` tell different stories at the detail-pane level but both scan as "skip" in the at-a-glance timeline.

**abctl's ACTION column is not only this vocabulary.** Two of its values are rendering, not plugin output: `—` when nothing acted, and `tunnel` for an opaque CONNECT — a row where no plugin ran, no protocol was parsed and there is no status, so METHOD and STATUS are blank too and the label is the only thing identifying it. Neither is ever emitted by a plugin, and neither is a verdict on the request.

> **Producer-side contract:** the authoritative definition of the 5-value vocabulary, the `Invocation` struct fields, and which diagnostic fields each plugin type populates lives in [`docs/plugin-reference.md`](docs/plugin-reference.md#emitting-session-events). Edit that file when the vocabulary changes; this table is the consumer-side summary.

### Gotcha: denied requests

Rejected requests (401 / 503) land as `phase: "denied"` events in `/v1/sessions` when at least one pipeline plugin appended an Invocation before rejecting. If you're debugging an unauthorized-access pattern, the default-session bucket (`GET /v1/sessions/default`) is where denial events aggregate.

### Disabling

Set `session.enabled: false` in the runtime config to turn off the store (and implicitly the API). Setting `listener.session_api_addr: ""` alone is not currently supported as a selective disable — the preset refills it; if you need store-on-API-off, raise an issue.

## Config Hot-Reload (`:9093/reload/status`)

The authbridge binary watches its config file (`/etc/authbridge/config.yaml`) via `authlib/reloader`. When the ConfigMap changes, kubelet syncs the new content into the mount (~60s), the watcher detects it, and the binary rebuilds + atomically swaps the plugin pipelines without a pod restart. In-flight requests finish on the previous pipeline; new requests go to the new one.

**What reloads:** any plugin list change (add/remove/reorder) and any plugin `config:` subtree edit.

**What doesn't reload (pod restart required):** `mode`, any `listener.*` address, and the session store parameters (`session.ttl`, `session.max_events`, `session.max_sessions`).

**Bad YAML stays safe:** if Load/Validate/Build/Start fails, the active pipeline keeps serving and the error is exposed on `/reload/status` with `reloads_failed` incremented. The pod never goes unhealthy from a bad edit.

Operator workflow:

```sh
kubectl edit configmap authbridge-config-<agent> -n <ns>
kubectl port-forward -n <ns> deploy/<agent> 9093:9093 &
curl http://localhost:9093/reload/status  # last_success, reloads_ok, active_config_sha256
curl http://localhost:9093/config         # now-active config (ConfigProvider closure)
```

See [`docs/framework-architecture.md`](docs/framework-architecture.md#9-config-hot-reload) §9 for the reload lifecycle, debounce / symlink-swap handling, and the drain-window behavior.

## Shared Volume Contract

The sidecar and the operator communicate through files on shared volumes:

| Path | Writer | Reader | Content |
|------|--------|--------|---------|
| `/opt/jwt_svid.token` | spiffe.Provider mirror | authbridge (token-exchange) + external readers | JWT SVID, audience from `spiffe.jwt_audience`. Written **only** when a plugin requests a JWT-SVID for an audience (today: `token-exchange` with `identity.type: spiffe`) — an X.509-only workload never produces it |
| `/opt/svid.pem` | spiffe.Provider mirror | external readers (debugging, future Envoy SDS) | X.509 SVID leaf cert (PEM) |
| `/opt/svid_key.pem` | spiffe.Provider mirror | external readers | X.509 SVID private key (PEM) |
| `/opt/svid_bundle.pem` | spiffe.Provider mirror | external readers | SPIRE trust bundle (PEM, may concatenate multiple CAs) |
| `/shared/client-id.txt` | operator (Secret mount) | authbridge (`jwt-validation`'s `audience_file`) | SPIFFE ID or workload name |
| `/shared/client-secret.txt` | operator (Secret mount) | authbridge (`token-exchange`) | Keycloak client secret |

The X.509 SVID files are mirrored to disk by the in-process
`spiffe.Provider` when `spiffe.mirror_files` is on (the default); the files
exist for external readers (e2e probes,
debugging, future Envoy filesystem SDS) and are kept fresh on every
rotation. The listener itself reads SVIDs in-memory via
`spiffe.X509Source` and never re-reads the files. authbridge enables
mTLS only when `mtls:` is configured at the top level of the
runtime config; absent that block, today's plaintext behavior is
preserved.

## Top-level `mtls:` configuration

When the runtime config carries an `mtls:` block, authbridge enables
transport-level mTLS on the proxy-sidecar listeners (forward + reverse
proxy). envoy-sidecar mode handles mTLS at the Envoy data-plane level
instead — see the **envoy-sidecar mTLS** subsection below.

```yaml
# authbridge-runtime-config ConfigMap (top-level)
mtls:
  mode: strict          # permissive | strict (omit block entirely for off)
  # cert_file / key_file / bundle_file optional —
  # default to /opt/svid.pem, /opt/svid_key.pem, /opt/svid_bundle.pem
```

| Mode | Inbound (reverse proxy `:8080`) | Outbound (forward proxy) |
|---|---|---|
| (no `mtls` block) | Plaintext only. | Plaintext only. |
| `permissive` (default when block present) | Byte-peek listener: TLS handshakes verified against the SPIRE trust bundle; plaintext callers served on the same port. ⚠️ Plaintext requests carry their full headers and bodies in the clear — including any `Authorization: Bearer ...` token already injected by `token-exchange`. Use only during rollout with cluster-network trust. | Plaintext — no TLS-wrap attempt. Matches envoy-sidecar's permissive and Istio's PeerAuthentication semantics (permissive is inbound-only). A permissive caller cannot reach a strict peer; mixed-mode deployments need both ends compatible. |
| `strict` | TLS only — non-TLS callers get the connection closed. | TLS or fail: handshake failure is a hard error, no fallback. |

In both modes, a successful TLS handshake that fails certificate
verification is always a hard error.

**Trust model:** any peer with a valid cert from the SPIRE trust bundle
can talk to this authbridge. Per-caller policy / SPIFFE allowlists are
out of scope; the trust bundle IS the policy. Plugins that want
per-caller decisions read `pctx.PeerCert` and check the URI SAN.

**Hot-reload boundary:** mTLS config (`mtls.mode`, cert paths) requires a
pod restart to apply, matching the existing rule for `listener.*`
addresses. Plugin-pipeline config keeps its own hot-reload behavior.

### envoy-sidecar mTLS

In envoy-sidecar mode the listeners live in Envoy, not authbridge,
so the top-level `mtls:` block is a no-op there. Equivalent semantics
are configured at the Envoy data-plane level by extending the
`envoy-config` ConfigMap with TLS blocks:

| Mode | Inbound (`:15124`) | Outbound (`:15123`) |
|---|---|---|
| `disabled` | plaintext (today's behavior) | plaintext (today's behavior) |
| `permissive` | `tls_inspector` listener filter + two filter chains: `transport_protocol: tls` chain terminates mTLS, `transport_protocol: raw_buffer` chain accepts plaintext | **plaintext** — no TLS-wrap attempt |
| `strict` | `tls_inspector` + single TLS filter chain; plaintext drops at filter chain match | `UpstreamTlsContext` on the `original_destination` cluster: TLS-or-fail, blanket |

X.509 SVIDs are read by Envoy directly from `/opt/svid.pem`,
`/opt/svid_key.pem`, `/opt/svid_bundle.pem` — the same paths
proxy-sidecar's mTLS uses. The spiffe Provider's file-mirror in
the `authbridge-envoy` binary keeps these fresh on rotation.

**Inbound parity with proxy-sidecar:** byte-identical observable
semantics — TLS handshakes terminate against the SPIRE trust bundle,
plaintext is served (permissive) or rejected (strict). Same outcome
as proxy-sidecar's `tlssniff.Listener`, just expressed as Envoy
filter chains. This matches Istio's PERMISSIVE/STRICT inbound exactly.

**Outbound is Istio-shaped:** Envoy has no native primitive for
"try TLS, fall back to plaintext on handshake failure" within an
`ORIGINAL_DST` cluster (and Istio itself doesn't do it — Pilot
pre-decides mesh membership). Permissive keeps outbound plaintext;
strict does blanket TLS-or-fail to everything the listener sees.
This works in practice because outbound calls that need plaintext —
Keycloak, JWKS, external HTTPS — never reach the listener: plugin
outbound uses Go `net/http` directly, and `proxy-init`'s iptables
doesn't redirect arbitrary HTTPS egress. **Proxy-sidecar matches
this**: its forward proxy now also dials plaintext in permissive
mode, so the two deployment shapes share one outbound semantics.

**Behavioral note:** a *permissive* caller cannot reach a *strict*
peer regardless of mode (its outbound is plaintext; the peer's
strict inbound rejects it). Mixed-mode deployments need both ends
compatible — both strict, both permissive, or one strict + the
other permissive on inbound only.

The operator's AgentRuntime CR's `Spec.MTLSMode` flows
through to a per-agent rendered envoy-config with the matching TLS
blocks (operator companion PR). The [`demos/mtls/`](demos/mtls/)
envoy-sidecar variant (`make demo-mtls-envoy*`) ships a hand-crafted
demo that proves the same Envoy YAML design at the data-plane level
without needing a CR.

## CI/CD Workflows

| Workflow | Trigger | Purpose |
|----------|---------|---------|
| `ci.yaml` | PR to main/release-* | Pre-commit; `go fmt`/`go vet`/build/test for authlib, both `scripts/*` and the `cmd/*` matrix; `go mod tidy -diff` for all 12 modules; Python tests. Note `go fmt` rewrites rather than fails, so it does not gate |
| `build.yaml` | Tag push (`v*`) or manual | Multi-arch Docker builds for all six matrix images: proxy-init, authbridge (proxy-sidecar combined), authbridge-envoy (envoy-sidecar combined), authbridge-lite (proxy Dockerfile built with the `lite` profile from `scripts/profile-tags`), authbridge-cpex, and sparc-service (Python). Every Go image passes `GO_BUILD_TAGS` naming a profile — plugins are all opt-in, so an image built without tags registers none |
| `security-scans.yaml` | PR to main | Dependency review, shellcheck, YAML lint, Hadolint, Bandit, Trivy, CodeQL |
| `scorecard.yaml` | Weekly / push to main | OpenSSF Scorecard security health metrics |
| `spellcheck_action.yml` | PR | Spellcheck on markdown files |

### PR Title Convention

PR titles must follow the format:

```
<Prefix>: <Subject starting with uppercase>
```

The CI title check (`deepakputhraya/action-pr-title`) is **case-sensitive** and
requires a **capitalized** prefix from this set: `Build`, `Chore`, `CI`, `Docs`,
`Feat`, `Fix`, `Perf`, `Refactor`, `Revert`, `Style`, `Test`, `Feature`,
`Bug fix`, `Proposal`, `Breaking change`, `Other`, `Other/Misc`.

Note: commit *message* examples elsewhere in this doc use lowercase
(`feat:` / `fix:`) — that is fine for commits, but the PR *title* check rejects
lowercase prefixes. Use `Fix:` / `Feat:` / `Docs:` in PR titles.

## Container Images

All images are pushed to `ghcr.io/rossoctl/cortex/` from
`.github/workflows/build.yaml`:

| Image | Source | Description |
|-------|--------|-------------|
| **`authbridge`** | **`cmd/authbridge-proxy/Dockerfile`** | **proxy-sidecar image (default mode): authbridge-proxy, full plugin set incl. parsers. No Envoy.** |
| `authbridge-envoy` | `cmd/authbridge-envoy/Dockerfile` | envoy-sidecar combined image: Envoy + authbridge-envoy (ext_proc, full plugin set) |
| `authbridge-lite` | `cmd/authbridge-proxy/Dockerfile` (+ `GO_BUILD_TAGS` from the `lite` profile) | proxy-sidecar image with a sidecar-minimum plugin set (see `scripts/profile-tags`). A build variant of `authbridge`, not a separate binary; not yet referenced by the operator's default config |
| `authbridge-cpex` | `cmd/authbridge-cpex/Dockerfile` | proxy-sidecar build with the CPEX plugin. Two tag sources: the literal `cpex` tag, always required because it gates `cmd/authbridge-cpex/main.go`, plus the plugin tags its Dockerfile appends from `GO_BUILD_TAGS` (resolved from the `cpex` profile in `build.yaml`) — `cpex` alone registers no plugins. Links `libcpex_ffi.a` from a pinned CPEX release (CGO_ENABLED=1). Routes hooks through the CPEX framework (APL DSL + named CPEX policy plugins). FFI ABI version is read from `cmd/authbridge-cpex/CPEX_FFI_VERSION` |
| `proxy-init` | `proxy-init/Dockerfile.init` | Alpine + iptables init container (envoy-sidecar + proxy-sidecar enforce-redirect modes) |
| `sparc-service` | `sparc-service/Dockerfile` | Python SPARC reflection service (FastAPI wrapper around the ALTK pre-tool reflection component), called by the `sparc` plugin |

None of these images bundle `spiffe-helper`, and `SPIRE_ENABLED` no
longer gates anything. SPIRE credentials are fetched in-process by
`authlib/spiffe`'s Provider.

The legacy `authbridge-unified`, `authbridge-light`, `client-registration`,
`spiffe-helper`, `auth-proxy`, and `demo-app` standalone images have
been removed from CI (the auth-proxy / demo-app source is still in-tree
for the standalone quickstart). Older release tags continue to publish
the old images.

## Pre-commit Hooks

Install: `pre-commit install`

Hooks:
- `trailing-whitespace`, `end-of-file-fixer`, `check-added-large-files` (max 1024KB), `check-yaml`, `check-json`, `check-merge-conflict`, `mixed-line-ending`
- `ai-assisted-by-trailer` — Rewrites `Co-Authored-By` to `Assisted-By` (commit-msg stage)
- `ruff`, `ruff-format` — Python linting/formatting, `exclude: ^tests/`. The
  filter was `files: ^authbridge/` until that tree became the repo root; it was
  inverted rather than dropped because `tests/` was never in scope and does not
  lint clean, so widening to cover it is a separate change.

**There are no Go hooks** — `gofmt` and `go vet` run nowhere in pre-commit. In
`ci.yaml` both run, but only one of them can fail:

- `go vet ./...` **is** a gate, on 7 of the 12 modules: `authlib`, both
  `scripts/*`, and the `cmd/{authbridge-proxy,authbridge-envoy,abctl,authbridge-praxis}`
  matrix. Not vetted anywhere: `cmd/authbridge-cpex` (deliberately excluded — it
  needs CGO and a pinned `libcpex_ffi.a`, so `build.yaml` covers it via the image
  build), `storage/redis`, and the three `demos/*` modules.
- `go fmt ./...` is **not** a gate. `go fmt` is `gofmt -l -w`: it rewrites the
  checkout and exits 0, so formatting drift cannot fail a build.

Formatting drift therefore reaches main — a few files are gofmt-dirty there
today, including three under `authlib`, where `go fmt` demonstrably runs on
every PR. Run `gofmt -l .` yourself before pushing, and `go vet` too if you
touched one of the five unvetted modules.

## Languages and Tech Stack

| Area | Technology |
|------|------------|
| AuthBridge sidecar binaries | Go 1.26.5, envoy-control-plane, lestrrat-go/jwx |
| abctl (TUI) | Go 1.26.5, bubbletea |
| keycloak_sync.py / setup scripts | Python 3.12, python-keycloak (`>=7.1.1,<8`) |
| sparc-service | Python 3.10+, FastAPI, agent-lifecycle-toolkit |
| Proxy | Envoy v1.37.1 (pinned by digest in `cmd/authbridge-envoy/Dockerfile`) |
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

## ConfigMaps and Secrets Expected at Runtime

When the operator injects sidecars, the target namespace needs these resources.
The rossoctl Helm chart's `agent-namespaces.yaml` and
`authbridge-template-configmaps.yaml` templates render them; the operator copies
them into agent namespaces that don't already have them:

| Resource | Kind | Used by | Keys |
|----------|------|---------|------|
| `authbridge-config` | ConfigMap | authbridge | `KEYCLOAK_URL`, `KEYCLOAK_REALM`, `PLATFORM_CLIENT_IDS` (optional), `TOKEN_URL` (optional, derived from KEYCLOAK_URL+KEYCLOAK_REALM), `ISSUER` (optional, derived or explicit for split-horizon DNS), `DEFAULT_OUTBOUND_POLICY` (optional, defaults to `passthrough`). Inbound audience validation uses `CLIENT_ID` from `/shared/client-id.txt`. Target audience and scopes are configured per-route in `authproxy-routes`. |
| `authbridge-runtime-config` | ConfigMap | authbridge | The runtime `config.yaml` — `mode`, `listener`, `pipeline`, and the top-level `session` / `stats` / `mtls` / `spiffe` / `tls_bridge` / `pricing` / `cost_ledger` blocks. Note the name differs from the `authbridge-runtime` volume the operator mounts it through. |
| `keycloak-admin-secret` | Secret | operator (ClientRegistrationReconciler) | `KEYCLOAK_ADMIN_USERNAME`, `KEYCLOAK_ADMIN_PASSWORD` |
| `authproxy-routes` | ConfigMap (optional) | authbridge | `routes.yaml` -- per-host token exchange rules (format below) |
| `spiffe-helper-config` | ConfigMap (legacy, unused) | (none) | Previously held `helper.conf` for the bundled `spiffe-helper` binary. Retained only for compatibility with older deployments; authbridge drives SPIRE configuration via the top-level `spiffe:` block in `authbridge-runtime-config` and does not read this ConfigMap. |
| `envoy-config` | ConfigMap | Envoy (inside the `authbridge-envoy` combined image, envoy-sidecar mode only) | `envoy.yaml` (full Envoy configuration) |

**`authproxy-routes` format** (`routes.yaml`):
```yaml
routes:
  - host: "github-tool-mcp"
    target_audience: "github-tool"
    token_scopes: "openid github-tool-aud github-full-access"
  - host: "auth-target-*"
    target_audience: "auth-target"
    token_scopes: "openid auth-target-aud"
```

**Note:** `authproxy-routes` is optional. Without it, all outbound traffic passes through unchanged (the default policy is `passthrough`). Only create it when the agent needs to call services that require token exchange. Set `DEFAULT_OUTBOUND_POLICY: "exchange"` in `authbridge-config` to restore the legacy behavior.

## Keycloak Setup Scripts

There are **two** setup scripts for different demo scenarios:

| Script | Location | Use Case |
|--------|----------|----------|
| `setup_keycloak_weather_advanced.py` | `demos/weather-agent/` | Weather agent (advanced) demo: realm setup, scopes for token exchange to the weather tool's audience, alice user. Drives the CI verify script `deploy_and_verify_advanced.sh`. |
| `setup_keycloak.py` | `demos/github-issue/` | GitHub issue integration demo (creates github-tool client, github-tool-aud + github-full-access scopes, alice + bob users) |

**Common Keycloak defaults across all scripts:**
- URL: `http://keycloak.localtest.me:8080`
- Realm: `rossoctl`
- Admin: `admin` / `admin`

**Note:** All scripts share the same helper function patterns (`get_or_create_realm`, `get_or_create_client`, `get_or_create_client_scope`, etc.) and are idempotent.

## Common Development Tasks

### Building Everything Locally

`scripts/local-build-and-test.sh` orchestrates every image
the platform needs (`spiffe-idp-setup` from rossoctl, plus
`authbridge`, `authbridge-envoy`, `authbridge-lite`, `proxy-init`
from this repo) and loads them into a Kind cluster. Run it from
the repo root:

```bash
ROSSOCTL_DIR=../rossoctl ./scripts/local-build-and-test.sh
```

To build a single image directly. All Docker build contexts are the repo root now,
so run these from the top of the checkout:

```bash
# proxy-init (iptables init container, envoy-sidecar + proxy-sidecar enforce-redirect)
cd proxy-init
make docker-build-init
make load-image                     # Uses KIND_CLUSTER_NAME env var (default: rossoctl)
cd ..

# Sidecar images. Pick whichever you need; the operator selects the image per
# workload from the resolved AuthBridge mode.
podman build -f cmd/authbridge-proxy/Dockerfile -t authbridge:latest .       # proxy-sidecar (default)
podman build -f cmd/authbridge-envoy/Dockerfile -t authbridge-envoy:latest . # envoy-sidecar
# authbridge-lite: same proxy Dockerfile, built with the `lite` profile
# from scripts/profile-tags. Plugins are all opt-in, so
# GO_BUILD_TAGS is required — omitting it registers no plugins.
podman build -f cmd/authbridge-proxy/Dockerfile \
  --build-arg GO_BUILD_TAGS="$(go -C scripts/profile-tags run . lite)" \
  -t authbridge-lite:latest .

kind load docker-image authbridge:latest       --name rossoctl
kind load docker-image authbridge-envoy:latest --name rossoctl
kind load docker-image authbridge-lite:latest  --name rossoctl
```

### Running the Full Demo

1. Set up a Kind cluster with SPIRE + Keycloak (use [Rossoctl installer](https://www.rossoctl.dev/docs/overview/quickstart))
2. Deploy the webhook via [operator](https://github.com/rossoctl/operator)
3. See the [AuthBridge demos index](demos/README.md) for a recommended learning path:
   - **Getting started**: `demos/weather-agent/demo-ui.md` (inbound validation, UI deployment)
   - **Full flow**: `demos/github-issue/demo-ui.md` (token exchange + scope-based access)
   - **Routes config reference**: `demos/token-exchange-routes/README.md` (single + multi-target route patterns)

The recommended end-to-end flow is the weather-agent advanced demo, which
exercises the combined sidecar shape with token exchange to a tool's audience:

```bash
# Apply manifests, run Keycloak setup, verify end-to-end
demos/weather-agent/deploy_and_verify_advanced.sh
```

For an interactive walkthrough see
[`demos/weather-agent/demo-ui-advanced.md`](demos/weather-agent/demo-ui-advanced.md).

### Adding a New Component Image to CI

1. Add entry to `.github/workflows/build.yaml` matrix (`image_config` array)
2. Provide `name`, `context`, and `dockerfile` fields
3. Image will be pushed to `ghcr.io/rossoctl/cortex/<name>`

## Common Tasks for Code Changes

### Modifying Token Exchange Logic
- Edit `authlib/plugins/tokenexchange/exchange/` -- the RFC 8693 token exchange client
- The token exchange POST parameters follow RFC 8693 exactly
- Test by rebuilding the affected image. `GO_BUILD_TAGS` is required — every
  plugin is opt-in, so a build without it registers none and rejects every
  config it is handed:
  `podman build -f cmd/authbridge-envoy/Dockerfile
  --build-arg GO_BUILD_TAGS="$(go -C scripts/profile-tags run . envoy)"
  -t authbridge-envoy:latest .` then `kind load docker-image
  authbridge-envoy:latest --name rossoctl`.

### Modifying Inbound JWT Validation
- Edit `authlib/plugins/jwtvalidation/validation/` -- the JWKS-backed JWT verifier
- JWKS cache auto-refreshes
- Direction detection: `x-authbridge-direction: inbound` header (injected by Envoy inbound listener config)

### Adding New iptables Rules
- Edit `proxy-init/init-iptables.sh`
- Follow the existing pattern: document the rule's purpose, Istio interaction, and chain ordering
- Test with and without Istio ambient mesh if possible
- Rebuild from `proxy-init/`: `make docker-build-init && make load-image`
  (the target is `load-image`, singular — `load-images` does not exist)

### Modifying Client Registration
Registration no longer lives here — it is the operator's
`ClientRegistrationReconciler` (see the [operator
repo](https://github.com/rossoctl/operator)). This repo only *consumes* the
resulting `/shared/client-id.txt` and `/shared/client-secret.txt`.

### Adding New Keycloak Resources to Setup
- Edit the appropriate `setup_keycloak*.py` script
- Use the `get_or_create_*` helper pattern for idempotency
- All scripts use `python-keycloak` library (KeycloakAdmin class)

### Changing Envoy Configuration
- Edit the `envoy.yaml` template in the [rossoctl Helm chart](https://github.com/rossoctl/rossoctl)
  (`charts/rossoctl/templates/agent-namespaces.yaml` or
  `authbridge-template-configmaps.yaml`) and `helm upgrade`
- Key listener/cluster names: `outbound_listener`, `inbound_listener`, `original_destination`, `ext_proc_cluster`
- After changes, restart the affected pods so they pick up the new ConfigMap content

## Code Style and Conventions

### Go Code
- Run `go vet ./...` and `gofmt -l .` yourself before pushing, from each module you
  touched. **Always give `gofmt` a path** — a bare `gofmt -l` reads stdin, scans
  nothing and exits 0.
- **Neither is enforced.** pre-commit has no Go hooks, and `ci.yaml`'s `go fmt ./...`
  is `gofmt -l -w`, which rewrites and exits 0. `go vet` *is* gated, but only on 7 of
  the 12 modules — see the Pre-commit Hooks section for which five are uncovered.
  See [CONTRIBUTING.md](CONTRIBUTING.md#code-style) for the contributor-facing version.
- Run per-module with `GOWORK=off`, as every CI Go job except the `authlib` one
  does, so each module resolves its own `replace` directives instead of pulling in
  workspace siblings.
- If a change deletes a package or its last import of a dependency, also run
  `go mod tidy -diff` in every module — `ci.yaml`'s `go-tidy-check` gates on it,
  and `build`/`vet`/`test` all pass while it fails.
- Logging with `log/slog`; the binaries log under their own name
  (`authbridge-proxy`, `authbridge-envoy`). Note the `authbridge-lite` image runs
  the `authbridge-proxy` binary, so it logs as `authbridge-proxy`.
- gRPC ext-proc uses `envoyproxy/go-control-plane` types (in `authlib/listener/extproc`)
- JWT validation uses `lestrrat-go/jwx/v2` (in `authlib/plugins/jwtvalidation/validation`)

### Python Code (keycloak_sync.py, demo setup scripts)
- Python 3.12+ syntax (type hints with `str | None`)
- Dependencies in `requirements.txt` — `python-keycloak>=7.1.1,<8`.
  Note `ci.yaml`'s Python job still pip-installs `python-keycloak==5.3.1`, two
  majors behind, so CI is not testing the version the project declares.
- `python-keycloak` for all Keycloak admin API calls
- Idempotent: every `get_or_create_*` helper checks existence before creating
- `ruff` / `ruff-format` cover the whole tree except `tests/`, which has
  pre-existing findings — see the Pre-commit Hooks section.

### Kubernetes Manifests
- Example deployment YAMLs in `demos/*/k8s/`

### Shell Scripts
- Strict mode where it is safe to add: `scripts/local-build-and-test.sh` uses
  `set -euo pipefail`, `install.sh` uses `set -eu`. **`proxy-init/init-iptables.sh`
  is `set -e` only** — do not "fix" it to `pipefail` without testing, its iptables
  probes rely on tolerated failures.
- Extensive inline documentation (especially `init-iptables.sh`, which explains
  iptables chain ordering, Istio interactions, and debugging tips)
- Idempotent: `init-iptables.sh` uses `iptables -N ... 2>/dev/null || true` and
  `iptables -F` before adding rules

## Important Cross-Component Relationships

1. **Envoy Proxy UID:** Envoy runs as UID 1337. The `proxy-init` iptables rules exclude this UID from redirection to prevent loops. The `authbridge` container also runs as UID 1337.

2. **Shared Volume Contract:** the sidecar and the operator communicate through
   files under `/opt/` and `/shared/` — see the
   [Shared Volume Contract](#shared-volume-contract) section above for the full
   writer/reader table.

3. **Port Coordination:** Envoy listens on 15123 (outbound) and 15124 (inbound). The ext-proc listens on 9090. The `proxy-init` iptables rules redirect to these ports.

## Gotchas and Known Issues

1. **Multiple Go modules:** The repo has 12 Go modules at the root — `authlib/`, each `cmd/*/`, `storage/redis/`, both `scripts/*/`, and the three self-contained `demos/*/` ones. `go.work` links the first nine; the `demos/*` modules are deliberately outside the workspace. Local commands from a specific module directory should typically set `GOWORK=off` (as every CI Go job but `authlib`'s does) so the module resolves its own `replace` directives instead of pulling in workspace siblings.

2. **Credential file race condition**: Each plugin that reads a credential file (jwt-validation's `audience_file`, token-exchange's `client_id_file` / `client_secret_file` / `jwt_svid_path`) tries a synchronous read at Configure time and, on miss, spawns an Init goroutine that polls indefinitely — emitting a WARN every ~60s while the file is still missing. OnRequest returns 503 until the file arrives. If the file never shows up (wrong path, missing volume mount), the pod stays unready for outbound traffic; follow the WARN lines to the misconfigured path.

3. **ISSUER vs TOKEN_URL**: `ISSUER` must be the Keycloak **frontend URL** (what appears in the `iss` claim of tokens), while `TOKEN_URL` is the **internal service URL**. These are often different in Kubernetes (e.g., `http://keycloak.localtest.me:8080` vs `http://keycloak-service.keycloak.svc:8080`).

4. **Keycloak port exclusion**: When using iptables interception, Keycloak's port (8080) must be excluded from redirect via `OUTBOUND_PORTS_EXCLUDE=8080`. Otherwise, token exchange requests from authbridge get redirected back to Envoy, creating a loop.

5. **TLS passthrough is one-way**: Outbound HTTPS traffic passes through Envoy without token exchange via the TLS passthrough filter chain. Only plaintext HTTP outbound traffic reaches authbridge. With the default outbound policy of `"passthrough"`, even plaintext HTTP traffic is forwarded unchanged unless it matches an explicit route in `authproxy-routes`.

6. **Outbound passthrough is the safe default**: `DEFAULT_OUTBOUND_POLICY` defaults to `passthrough`, so token exchange only happens for hosts explicitly listed in `authproxy-routes`, where target audience and scopes are configured per-route. That is what lets outbound traffic to LLM inference endpoints (e.g., Ollama via `host.docker.internal`) work untouched. If this were set to `exchange`, every outbound HTTP call would attempt token exchange and fail for non-Keycloak destinations.

7. **Route host patterns must match the HTTP Host header**: The `host` field in `authproxy-routes` is matched against the HTTP `Host` header, which the client sets from the URL hostname. For in-cluster calls that is the **short Kubernetes service name** from `MCP_URL` (e.g., `github-tool-mcp`), not the FQDN. Glob patterns (`*`, `?`, `[...]`) work, but a plain service name is the common case; the wrong pattern (e.g., `*.github-issue-tool*.svc.cluster.local`) silently falls through to the default passthrough policy.

8. **Envoy config not embedded:** The envoy-proxy sidecar mounts `envoy-config` ConfigMap at `/etc/envoy`. This ConfigMap must exist in the target namespace before workloads are created.

9. **Envoy Lua filter required for inbound**: The `x-authbridge-direction: inbound` header MUST be injected via a Lua filter before the ext_proc filter in the inbound listener. Route-level `request_headers_to_add` does NOT work because the router filter runs after ext_proc.

10. **iptables backend auto-detection**: `init-iptables.sh` auto-detects `iptables-legacy` vs `iptables-nft`. Override with `IPTABLES_CMD` env var if needed. Always verify with proxy-init logs after deployment.

11. **Admin credentials in ConfigMap**: the rossoctl Helm chart's
    `agent-namespaces.yaml` template stores Keycloak admin credentials
    in `authbridge-config` (a ConfigMap, not a Secret). This is for
    demo / dev clusters only — production should use a Kubernetes
    Secret and mount via SecretKeyRef.

12. **Keycloak scope assignment for dynamically registered clients**: When the operator's `ClientRegistrationReconciler` auto-registers an agent as a Keycloak client, the client may not inherit all necessary scopes. The agent's own audience scope (e.g., `agent-team1-git-issue-agent-aud`) must be a **default** client scope for inbound JWT audience validation to work. Token exchange scopes (e.g., `github-tool-aud`, `github-full-access`) must be **optional** client scopes for `client_credentials` grants with explicit `scope=` to succeed. Re-run the demo's `setup_keycloak.py` after the agent is deployed to assign these scopes to the registered client.

13. **Avoid committing venvs:** Virtual environment directories should be gitignored (the repo's `.gitignore` has a `venv` pattern). Do not create and commit new virtual environments under version control.

14. **Chatty observability traffic and IBAC user intent**: The session store keeps **every** event a session produces — `session.max_events` is unset by default, and so is time-based expiry (`session.ttl` defaults to never). What bounds memory by default is `session.max_sessions` (100, oldest-used evicted first), which limits how MANY sessions are kept and not how big any one of them gets: a single long-lived chatty session grows without limit. Set `session.max_events` to put a FIFO per-session cap back where that matters, and `session.ttl` to bound how long raw prompts sit in memory.

    Leaving `max_events` unset is deliberate rather than an oversight — see `SessionConfig.Limits`, which reasons that a trimmed store is lossy on exactly the sessions worth reading, and that FIFO eviction takes the *beginning* of a session. What that used to cost was readability: a response caps at 2000 events, so anything a long session held before that window was unreachable through any request, retained for no reader. `?before=<seq>` closes that gap by paging backward instead of by capping retention, so depth now costs memory and *is* readable.

    Retention is much cheaper than the event count suggests, and it is worth knowing why before reading a memory figure. An LLM request carries the whole conversation, so every turn re-sends every earlier message; the store keeps one event per request. Stored naively that is quadratic in turns, and it showed: a laptop proxy reached 3.37GB resident in 18 hours, against ~833MB for every Claude Code transcript on the same disk — the same conversations, appended once each. The store now keeps **one copy of each distinct message per session** and has every event reference it (`authlib/session/intern.go`), measured at 10.4x less heap on a 300-turn session.

    That sharing works by rolling a one-event table forward, which means **an event with nothing to intern must not clear it** — and until it was fixed, one did. A session's events are not all turns: every bridged HTTPS request records a CONNECT tunnel-open, and it lands *between* an inference request and its response (165 of 500 events on a live session). Each one used to roll an empty table forward, so the next turn matched nothing and kept its own copy of the whole conversation. On the benchmark fixture that is 31.33MB/session against 2.79MB — **11.2x, and it applied to every interned field**, so the tool-schema work below would largely not have shown up in production without it. When measuring anything about interning, use a fixture that interleaves contentless events; a clean run of turns is not what live traffic looks like. Strings are immutable, so nothing observable changes. What remains per-event is the array of message *headers*, which is still proportional to the conversation — so a very long single session still grows, just an order of magnitude more slowly. The tool manifest interns too — descriptions *and* schemas — and duplicates harder than the conversation does (154x on a live session against 6.5x, because a client re-sends its whole manifest on every request).

    The schemas were the last big duplicate and needed a type change to reach. `InferenceTool.Parameters` was `map[string]any`, which cost **4.1x its JSON text** to hold and could not be interned without a recursive walk that rewrites map values — a walk cannot lean on string immutability the way sharing a string can. Measured on a live session: 84KB per event, ~172MB across one 2050-event session. So the field became `pipeline.RawJSON`, a named string type that keeps the schema exactly as the client sent it, and it interns like any other string. Two consequences worth knowing: the API now shows schemas in the client's own key order (a map round-trip silently sorted them), and the type must stay a NAMED string with a `MarshalJSON` method — OPA's `ast.InterfaceToValue` treats a plain or aliased string as a JSON string, which would leave every policy indexing `input.inference.tools[_].parameters` undefined with no error (`authlib/plugins/opa/tool_parameters_rego_test.go` guards it). MCP `Params`/`Result` still do not intern, being `map[string]any`; they are unmeasured on this workload and the same field-type change is available if that changes.

    **Retention was only half of it, and reading the store was the more expensive half.** A memory figure that keeps climbing while traffic is idle is usually not retention: `GET /v1/sessions/{id}` used to encode the whole response into a single buffer before writing a byte, so serving one snapshot cost heap proportional to the *response* — ~246MB of heap growth for a 105MB response — and Go keeps that as idle heap rather than returning it promptly. abctl requests a snapshot on every <kbd>Enter</kbd> into a session, so RSS ratcheted a couple hundred megabytes per keystroke: four requests took a live proxy from 731MB to 1447MB, still 1447MB two minutes later. Interning could not have helped there, and the two are not alternatives — interning makes stored events *share* one copy of a repeated message, and an encoder expands every share back into its own bytes on the way out. The response is now written one event at a time (`authlib/sessionapi/snapshot.go`, `BenchmarkSnapshotHeap`). When judging any memory change here, note that **RSS is not heap** and the proxy exposes no heap metric — there is no `pprof` endpoint anywhere in this tree — so an RSS-per-event figure mixes retention with read-path churn and cannot settle either one.

    **And the client is the other end of the same problem.** Measured on a laptop, `abctl` sat at 1.64GB against the proxy's 1.03GB — larger than the process it was watching, and the only one of the two still climbing. It made both mistakes the server had just stopped making: it decoded each snapshot as one document (`json.Decoder` only bounds its buffer between *values*, and a whole snapshot is one value), and it kept every string the server's encoder had expanded back out of the store's sharing. Both are fixed in `cmd/abctl/apiclient/snapshot.go`: the response is decoded one event at a time and fed through the same `session.Interner` the store uses, exported for the purpose rather than reimplemented. On a 23.5MB document, `+32.0MB HeapSys` and 26.49MB retained became +0.0MB and 2.31MB (`BenchmarkDecodeSessionViewHeap`). When a memory figure here looks wrong, measure **both** processes — the proxy has not been the larger one for a while.

    Two layered defenses keep the inbound A2A user intent visible to IBAC even when an agent generates dozens of outbound events per turn:

    - **Primary fix — `listener.skip_hosts`**: list infrastructure destinations (OTel collectors, metrics endpoints, log shippers) whose traffic should bypass the pipeline AND session recording entirely. Matched requests are forwarded as a transparent proxy: no plugin runs, no event is appended. Patterns use the same `.`-delimited glob semantics as `authproxy-routes`; ports are stripped before matching. Example:
      ```yaml
      listener:
        skip_hosts:
          - "otel-collector.*.svc.cluster.local"
          - "*.metrics.local"
      ```
      Any change to `listener.skip_hosts` requires a pod restart (same rule as other `listener.*` fields). Do NOT add hosts here that need IBAC / token-exchange policy applied — bypass means bypass.

    - **Backstop — intent pin in the eviction policy**: this one only applies when `session.max_events` is set, because with no cap there is no FIFO eviction to survive. Where it is set, and even with `skip_hosts` empty, the session store pins the most-recent inbound A2A request event against that eviction. If the buffer overflows, every other event evicts in normal chronological order; the protected intent stays at its original timestamp, leaving a visible time gap in the timeline. Older intents from earlier turns are NOT pinned — only the latest one — so a multi-turn conversation with huge fan-out can't pile up stale intents and starve the buffer. The pin protects against FIFO eviction only: IBAC's `LastIntent()` survives buffer overflow as long as the session is still alive and an inbound A2A request has landed in it, but can still return nil after explicit deletion, eviction once `max_sessions` is exceeded, a configured `session.ttl` elapsing, or before the first inbound request arrives. The pin is defense-in-depth; reach for `skip_hosts` first when the offending traffic is identifiable infrastructure.

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

PR descriptions should end with the same `Assisted-By` trailer:

    Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>

Do not use `🤖 Generated with [Claude Code](https://claude.com/claude-code)` or similar.

A `commit-msg` hook in `scripts/hooks/commit-msg` enforces this automatically for commits.
Install it via pre-commit:

```sh
pre-commit install --hook-type pre-commit --hook-type commit-msg
```
