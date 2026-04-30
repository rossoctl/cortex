# authlib — Shared Auth Building Blocks

A pure Go library providing reusable building blocks for JWT validation, OAuth 2.0 token exchange, and SPIFFE-based authentication. No protocol dependencies (no gRPC, no Envoy).

## Packages

| Package | Purpose |
|---------|---------|
| `validation/` | JWKS-backed JWT verifier (`lestrrat-go/jwx`) with required audience parameter |
| `exchange/` | RFC 8693 token exchange + client credentials grant with pluggable auth |
| `cache/` | SHA-256 keyed token cache with TTL eviction |
| `bypass/` | Path pattern matcher for public endpoints (health, agent card) |
| `spiffe/` | SPIFFE credential sources (file-based JWT-SVID) |
| `routing/` | Host-to-audience router with glob pattern matching |
| `auth/` | Composition layer: `HandleInbound` + `HandleOutbound` |
| `config/` | YAML config, mode presets, startup validation, URL derivation |
| `vault/` | Hashicorp Vault integration with JWT/OIDC auth (SPIFFE), KV v1/v2 support, lease-aware caching |

## Usage

```go
import (
    "github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
    "github.com/kagenti/kagenti-extensions/authbridge/authlib/config"
)

// Load and resolve config
cfg, _ := config.Load("config.yaml")
resolved, _ := config.Resolve(ctx, cfg)
handler := auth.New(*resolved)

// Handle requests
inResult := handler.HandleInbound(ctx, authHeader, path, "")
outResult := handler.HandleOutbound(ctx, authHeader, host)
```

## Go Module

```
module github.com/kagenti/kagenti-extensions/authbridge/authlib
```

Direct dependencies: `lestrrat-go/jwx/v2`, `gobwas/glob`, `gopkg.in/yaml.v3`, `hashicorp/vault/api`. No gRPC or Envoy deps.
