# core — the Cortex runtime library

The Go module every sidecar binary and both external consumers import. It holds the
plugin framework, the listeners, the cost pipeline, the session store, and the auth
primitives — roughly 53,000 non-test lines across 24 packages.

It was called `authlib` and described as *"the shared auth library"*. Measured by
non-test lines, auth is **1.6%** of it:

| Theme | Packages | Lines | Share |
|---|---|---|---|
| Framework | `pipeline`, `plugins`, `listener`, `config`, `spiffe` | 30,485 | 57.1% |
| Cost | `cost/*` | 11,288 | 21.1% |
| Observability | `session`, `sessionapi`, `observe`, `redact` | 5,368 | 10.0% |
| **Auth** | `auth`, `bypass`, `capabilities` | **882** | **1.6%** |

The remainder is transport, storage and integration glue — the table covers themes,
not every package.

**It is not protocol-free.** An earlier version of this file claimed no gRPC and no
Envoy dependency; that stopped being true when the listeners moved in. `listener/extproc`
is an Envoy ext_proc gRPC server, and `core` has 20 direct dependencies including
`google.golang.org/grpc`, `envoyproxy/go-control-plane`, OPA, OpenTelemetry and CPEX.

## Packages

**Framework** — the plugin machinery and the processes' entry points:

| Package | Purpose |
|---------|---------|
| `pipeline/` | Plugin interface + lifecycle (`Configurable`, `Initializer`, `Shutdowner`) — see [docs/framework-architecture.md](../docs/framework-architecture.md) |
| `plugins/` | Registry + every concrete plugin, each owning its own config — see [docs/plugin-reference.md](../docs/plugin-reference.md) |
| `listener/` | Every listener implementation: the Envoy `extproc` gRPC server, the forward/reverse/transparent HTTP proxies |
| `config/` | YAML config with mode presets, `${ENV_VAR}` expansion, credential-file waiters, top-level validation |
| `bootstrap/` | Process-level helpers shared by the binaries — logging setup, the SIGUSR1 toggle, the health and stats servers |
| `reloader/` | fsnotify-based config hot-reload — atomic pipeline swap on ConfigMap change |
| `capabilities/` | Capability interfaces protocol extensions implement, plus the vocabulary plugins share without importing each other — role constants, the `ContentSource` / `Fragment` shape, and `PluginCapabilities.Claims` constants |

**Cost** — 21% of the module, and the part the old description omitted entirely:

| Package | Purpose |
|---------|---------|
| `cost/pricing/` | Owns model rates and the arithmetic that turns tokens into money |
| `cost/settle/` | Turns one response's facts into one settled cost |
| `cost/event/` | The canonical wire shape of the per-request cost |
| `cost/ledger/` | Persists per-minute cost and token totals to disk |
| `cost/usage/` | Aggregates session events into fixed-width time buckets |

**Observability**:

| Package | Purpose |
|---------|---------|
| `session/` | In-memory session store correlating requests, with message interning — see the chatty-traffic gotcha in [CLAUDE.md](../CLAUDE.md) |
| `sessionapi/` | The `:9094` HTTP API — `/v1/sessions`, `/v1/events` SSE, `/v1/pipeline` |
| `observe/` | The `:9093` server — `/stats`, `/config`, `/reload/status` |
| `redact/` | Best-effort stripping of sensitive values before anything is stored or served |

**Auth and identity.** The theme table's 882-line Auth figure is `auth` + `bypass` +
`capabilities` — `capabilities` is grouped here by subject even though it is listed
under Framework above, which is where it is used from. `spiffe` and `routing` are
counted under Framework, not here:

| Package | Purpose |
|---------|---------|
| `auth/` | Composes building blocks into `HandleInbound` / `HandleOutbound`. Used internally by `jwt-validation` and `token-exchange`; plugin-internal in practice |
| `bypass/` | Path pattern matcher for public endpoints (health, agent card). Any inbound gate plugin can use it |
| `spiffe/` | Framework-shared SPIFFE credential helpers: in-process Workload API client plus the `/opt` file mirror |
| `routing/` | Host-to-audience router with glob matching. Used by `token-exchange` |

**Transport, storage and integration**:

| Package | Purpose |
|---------|---------|
| `tlsconfig/` | Builds `*crypto/tls.Config` values. Every importer aliases it `authtls`, because `crypto/tls` owns the bare name |
| `tlsbridge/` | The outbound TLS bridge — forges a certificate so plaintext interception can continue past a CONNECT |
| `storage/` | The `Store` interface and its provider registry. The Redis driver lives at `storage/redis/`, its own nested module, so its dependency stays out of `core` |
| `memstore/` | A generic, process-scoped, TTL key→value store, intentionally semantics-free. Contrast `storage/`, which is the cross-pod persistent one |
| `llmclient/` | Small helper for calling OpenAI-compatible endpoints |
| `placeholder/` | The convention for opaque credential handles. Named accurately despite reading like a TODO, and tied to the `abph_` wire prefix, which cannot change |
| `clientstate/` | The on-disk record `abctl` writes when it points an agent at a local proxy |
| `praxis/` | Converts a `config.Config` into a Praxis proxy config. **Paused** — ships in no image and registers no plugins, but deliberately kept compiling |

**Plugin internals** worth knowing about:

| Package | Purpose |
|---------|---------|
| `plugins/jwtvalidation/` | The `jwt-validation` plugin. Owns `validation/` (JWKS-backed verifier) |
| `plugins/tokenexchange/` | The `token-exchange` plugin. Owns `exchange/` (RFC 8693 client) and `cache/` (token TTL cache). Its JWT-SVID source is the framework's `spiffe/`, not a plugin-internal copy |
| `plugins/plugintesting/` | Test helpers — stubs that skip file IO, for listener-level tests |

Packages that used to live at `core/validation`, `core/exchange`, `core/cache` moved under their owning plugin. They had no reuse outside that plugin; keeping them at `core/` top-level implied wider usefulness than reality. New plugins should follow the same pattern: if the package is plugin-internal, colocate it under `plugins/<plugin>/`.

## Usage

Plugins own their own configuration and construct any `auth.Auth` they need internally from their per-plugin config (see [docs/plugin-reference.md](../docs/plugin-reference.md)). Host processes (cmd/authbridge-*) just load the YAML, call `plugins.Build`, run the pipelines, and let each plugin handle its own dependencies.

```go
import (
    "github.com/rossoctl/cortex/core/config"
    "github.com/rossoctl/cortex/core/plugins"
)

cfg, _ := config.Load("config.yaml")
config.ApplyPreset(cfg)
_ = config.Validate(cfg) // mode + listener combo only; plugins self-validate

inbound,  _ := plugins.Build(cfg.Pipeline.Inbound.Plugins)
outbound, _ := plugins.Build(cfg.Pipeline.Outbound.Plugins)

_ = inbound.Start(ctx)   // invokes Init on plugins that implement pipeline.Initializer
_ = outbound.Start(ctx)

// Listeners drive the pipelines — see core/listener/*
```

For direct use of the `auth/` composition layer (outside of plugins), see `auth/auth.go` — `auth.New(cfg)` takes an `auth.Config` containing the specific building blocks a caller needs.

## Go Module

```
module github.com/rossoctl/cortex/core
```

Twenty direct dependencies. The ones that shape the module: `envoyproxy/go-control-plane`
and `google.golang.org/grpc` (the ext_proc listener), `open-policy-agent/opa` (the `opa`
plugin), `spiffe/go-spiffe/v2`, `lestrrat-go/jwx/v2` (JWT), `go.opentelemetry.io/otel`,
`contextforge-org/cpex`, `maximhq/bifrost/core`, `rossoctl/context-guru`, `gobwas/glob`,
`fsnotify/fsnotify`, `tidwall/gjson` + `sjson`, `gopkg.in/yaml.v3`.

`core` is **consumed outside this repo** — by `rossoctl/operator` and
`rossoctl/rossoctl-cli`, both public repos that compile against this module, and
neither referenced anywhere else in this tree —
so removing exported API here is a cross-repo change.
