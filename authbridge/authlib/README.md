# authlib — Shared Auth Building Blocks

A pure Go library providing reusable building blocks for JWT validation, OAuth 2.0 token exchange, and SPIFFE-based authentication. No protocol dependencies (no gRPC, no Envoy).

## Packages

**Shared building blocks** (used by ≥2 plugins or the framework):

| Package | Purpose |
|---------|---------|
| `contracts/` | Tiny, stable vocabulary plugins depend on without importing each other — role constants (`RoleUser`, `RoleAssistant`, …), the `ContentSource` / `Fragment` shape for guardrail-parser interop, and claim constants (`ClaimAuthorizationHeader`, …) for `PluginCapabilities.Claims` mutex declarations. |
| `bypass/` | Path pattern matcher for public endpoints (health, agent card). Any inbound gate plugin (jwt-validation, SAML, mTLS) can use it. |
| `routing/` | Host-to-audience router with glob pattern matching. Used by token-exchange; future routed plugins are expected to reuse it. |
| `auth/` | Composition layer: `HandleInbound` + `HandleOutbound` — used internally by the `jwt-validation` and `token-exchange` plugins. Lingering in authlib/ for now; plugin-internal in practice. |
| `config/` | YAML config loader, mode presets, credential-file waiters, top-level validation |
| `pipeline/` | Plugin pipeline + lifecycle (`Configurable`, `Initializer`, `Shutdowner`) — see [docs/framework-architecture.md](../docs/framework-architecture.md) |
| `session/` | In-memory session store + `SessionSummary` aggregation, backing the `:9094` API |
| `sessionapi/` | HTTP API (`/v1/sessions`, `/v1/events` SSE, `/v1/pipeline`) exposing the session store |
| `reloader/` | fsnotify-based config hot-reload — atomic pipeline swap on ConfigMap change |
| `observe/` | `/stats`, `/config`, `/reload/status` HTTP server |

**Plugins and their internals**:

| Package | Purpose |
|---------|---------|
| `plugins/` | Registry + parser plugins (a2a-parser, mcp-parser, inference-parser) + shared `Build` + `StatsSource` contract |
| `plugins/jwtvalidation/` | The `jwt-validation` plugin. Owns `plugins/jwtvalidation/validation/` (JWKS-backed JWT verifier). |
| `plugins/tokenexchange/` | The `token-exchange` plugin. Owns `plugins/tokenexchange/exchange/` (RFC 8693 client), `plugins/tokenexchange/cache/` (token TTL cache), `plugins/tokenexchange/spiffe/` (JWT-SVID file source). |
| `plugins/plugintesting/` | Test helpers — stubs of jwt-validation / token-exchange that skip file IO, for listener-level tests. |

Packages that used to live at `authlib/validation`, `authlib/exchange`, `authlib/cache`, `authlib/spiffe` moved under their owning plugin. They had no reuse outside that plugin; keeping them at `authlib/` top-level implied wider usefulness than reality. New plugins should follow the same pattern: if the package is plugin-internal, colocate it under `plugins/<plugin>/`.

## Usage

Plugins own their own configuration and construct any `auth.Auth` they need internally from their per-plugin config (see [docs/plugin-reference.md](../docs/plugin-reference.md)). Host processes (cmd/authbridge) just load the YAML, call `plugins.Build`, run the pipelines, and let each plugin handle its own dependencies.

```go
import (
    "github.com/rossoctl/cortex/authbridge/authlib/config"
    "github.com/rossoctl/cortex/authbridge/authlib/plugins"
)

cfg, _ := config.Load("config.yaml")
config.ApplyPreset(cfg)
_ = config.Validate(cfg) // mode + listener combo only; plugins self-validate

inbound,  _ := plugins.Build(cfg.Pipeline.Inbound.Plugins)
outbound, _ := plugins.Build(cfg.Pipeline.Outbound.Plugins)

_ = inbound.Start(ctx)   // invokes Init on plugins that implement pipeline.Initializer
_ = outbound.Start(ctx)

// Listeners drive the pipelines — see cmd/authbridge/listener/*
```

For direct use of the `auth/` composition layer (outside of plugins), see `auth/auth.go` — `auth.New(cfg)` takes an `auth.Config` containing the specific building blocks a caller needs.

## Go Module

```
module github.com/rossoctl/cortex/authbridge/authlib
```

Direct dependencies: `lestrrat-go/jwx/v2`, `gobwas/glob`, `gopkg.in/yaml.v3`. No gRPC or Envoy deps.
