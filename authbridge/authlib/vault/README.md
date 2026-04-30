# authlib/vault — Vault Integration for Kagenti

Pure Go library for Hashicorp Vault integration with SPIFFE support.

## Features

- **JWT/OIDC authentication** (SPIFFE JWT-SVID) — Priority 1
- **Kubernetes service account authentication** — Fallback
- **Token authentication** — Dev/testing
- **KV v1 and KV v2 secret engine support** with auto-detection
- **Lease-aware caching** with automatic token renewal
- **Thread-safe** with proper locking
- **No protocol dependencies** (no gRPC, no Envoy) — pure library

## Quick Start

```go
package main

import (
    "context"
    "log"
    
    "github.com/kagenti/kagenti-extensions/authbridge/authlib/vault"
)

func main() {
    // Create configuration
    cfg := &vault.Config{
        Address:    "https://vault.example.com",
        AuthMethod: "jwt",
        Role:       "my-agent-role",
        JWTPath:    "/opt/jwt_svid.token",
        CacheTTL:   5 * time.Minute,
    }
    
    // Create client
    client, err := vault.NewClient(cfg)
    if err != nil {
        log.Fatalf("Failed to create client: %v", err)
    }
    defer client.Close()
    
    // Authenticate
    ctx := context.Background()
    if err := client.Authenticate(ctx); err != nil {
        log.Fatalf("Failed to authenticate: %v", err)
    }
    
    // Read a secret
    value, leaseDuration, err := client.ReadSecret(ctx, "secret/data/github/token", "token")
    if err != nil {
        log.Fatalf("Failed to read secret: %v", err)
    }
    
    log.Printf("Secret value: %s (lease: %ds)", value, leaseDuration)
}
```

## Authentication Methods

### JWT Auth (SPIFFE Pattern - Priority 1)

Authenticates using a SPIFFE JWT-SVID from spiffe-helper.

```go
cfg := &vault.Config{
    Address:     "https://vault.example.com",
    AuthMethod:  "jwt",
    Role:        "github-agent-role",
    JWTPath:     "/opt/jwt_svid.token",   // Written by spiffe-helper
    JWTAudience: "vault",                  // Must match Vault JWT auth config
}
```

**Vault setup:**

```bash
# Enable JWT auth
vault auth enable jwt

# Configure with SPIRE OIDC discovery URL
vault write auth/jwt/config \
  oidc_discovery_url="http://spire-server.spire.svc:8081" \
  default_role="github-agent"

# Create role with SPIFFE ID pattern
vault write auth/jwt/role/github-agent \
  role_type="jwt" \
  bound_audiences="vault" \
  bound_claims_type="glob" \
  bound_claims='{"sub":"spiffe://localtest.me/ns/*/sa/github-agent/*"}' \
  user_claim="sub" \
  policies="github-agent" \
  ttl="1h"
```

### Kubernetes SA Auth (Fallback)

Authenticates using the pod's Kubernetes service account token.

```go
cfg := &vault.Config{
    Address:             "https://vault.example.com",
    AuthMethod:          "kubernetes",
    Role:                "my-app-role",
    KubernetesTokenPath: "/var/run/secrets/kubernetes.io/serviceaccount/token",
}
```

**Vault setup:**

```bash
# Enable Kubernetes auth
vault auth enable kubernetes

# Configure Kubernetes auth
vault write auth/kubernetes/config \
  kubernetes_host="https://kubernetes.default.svc:443"

# Create role
vault write auth/kubernetes/role/my-app-role \
  bound_service_account_names="my-app" \
  bound_service_account_namespaces="default" \
  policies="my-policy" \
  ttl="1h"
```

### Token Auth (Dev/Testing)

Directly uses a Vault token (not recommended for production).

```go
cfg := &vault.Config{
    Address:    "https://vault.example.com",
    AuthMethod: "token",
    Token:      "hvs.CAESIJlWiw...",
}
```

## Reading Secrets

### Single Secret

```go
value, leaseDuration, err := client.ReadSecret(ctx, "secret/data/github/token", "token")
```

**Path format:**
- **KV v2:** `secret/data/<path>` (e.g., `secret/data/github/token`)
- **KV v1:** `secret/<path>` (e.g., `secret/github/token`)

The library auto-detects which version you're using.

### Multiple Secrets (Batch)

```go
requests := []vault.SecretRequest{
    {Path: "secret/data/github/token", Field: "token"},
    {Path: "secret/data/database/postgres", Field: "password"},
}

responses, err := client.ReadSecrets(ctx, requests)
for _, resp := range responses {
    log.Printf("%s: %s (lease: %ds)", resp.Path, resp.Value, resp.LeaseDuration)
}
```

### List Secrets

```go
secrets, err := client.ListSecrets(ctx, "secret/metadata/github")
// Returns: ["token", "webhook-secret", ...]
```

## Caching

Secrets are automatically cached with lease-aware TTL:

**Cache TTL calculation:**
```
effective_ttl = min(configured_cache_ttl, vault_lease_duration)
```

This ensures secrets are refreshed before they expire in Vault.

**Cache behavior:**
- Cache hit: Returns immediately, no Vault call
- Cache miss: Fetches from Vault, caches result
- Expired entry: Automatically refreshed on next access
- 30-second buffer subtracted from TTL to ensure refresh

**Clear cache manually:**
```go
client.ClearCache()
```

## Token Renewal

If the Vault token is renewable (JWT and Kubernetes auth), a background goroutine automatically renews it.

**Renewal timing:**
- Token is renewed at **2/3 of its lease duration** (Vault best practice)
- Renewal happens automatically in the background
- Failures are logged but renewal continues retrying

**Stop renewal:**
```go
client.Close() // Stops renewal goroutine and clears cache
```

## Error Handling

The library provides typed errors for better error handling:

```go
value, _, err := client.ReadSecret(ctx, "secret/data/missing", "token")
if err != nil {
    switch e := err.(type) {
    case *vault.SecretNotFoundError:
        log.Printf("Secret not found: %s", e.Path)
    case *vault.FieldNotFoundError:
        log.Printf("Field %s not found in secret %s", e.Field, e.Path)
    case *vault.AuthError:
        log.Printf("Auth failed (method=%s): %s", e.Method, e.Reason)
    case *vault.ConfigError:
        log.Printf("Config error (%s): %s", e.Field, e.Reason)
    default:
        log.Printf("Unknown error: %v", err)
    }
}
```

## TLS Configuration

### Skip TLS Verification (Development Only)

```go
cfg := &vault.Config{
    Address:       "https://vault.example.com",
    TLSSkipVerify: true, // ⚠️ DO NOT USE IN PRODUCTION
}
```

### Custom CA Certificate

```go
cfg := &vault.Config{
    Address: "https://vault.example.com",
    CACert:  "/etc/ssl/certs/vault-ca.pem",
}
```

### Mutual TLS (mTLS)

```go
cfg := &vault.Config{
    Address:    "https://vault.example.com",
    ClientCert: "/etc/ssl/certs/client.pem",
    ClientKey:  "/etc/ssl/private/client-key.pem",
}
```

## Vault Enterprise: Namespaces

```go
cfg := &vault.Config{
    Address:   "https://vault.example.com",
    Namespace: "my-namespace",
}
```

## Configuration via YAML

```yaml
vault:
  address: "https://vault.example.com"
  auth_method: "jwt"
  role: "github-agent-role"
  jwt_path: "/opt/jwt_svid.token"
  jwt_audience: "vault"
  cache_ttl: "5m"
  tls_skip_verify: false
  ca_cert: "/etc/ssl/certs/vault-ca.pem"
```

Load with:
```go
import "gopkg.in/yaml.v3"

var cfg vault.Config
yaml.Unmarshal(data, &cfg)
```

## Advanced Usage

### Access Underlying Vault Client

For operations not covered by this library:

```go
vaultClient := client.GetVaultClient()
secret, err := vaultClient.Logical().Read("some/custom/path")
```

## Architecture

```
┌─────────────────────────────────────────┐
│            Client                       │
│  - Coordinates all components           │
│  - Provides high-level API              │
└──────────┬───────────┬─────────┬────────┘
           │           │         │
           ▼           ▼         ▼
    ┌──────────┐ ┌─────────┐ ┌──────────┐
    │Authenti- │ │ Secret  │ │  Secret  │
    │  cator   │ │ Reader  │ │  Cache   │
    ├──────────┤ ├─────────┤ ├──────────┤
    │- JWT     │ │- KV v1  │ │- Lease   │
    │- K8s SA  │ │- KV v2  │ │  aware   │
    │- Token   │ │- Auto   │ │- SHA256  │
    │- Renewal │ │  detect │ │  keyed   │
    └──────────┘ └─────────┘ └──────────┘
           │           │
           └─────┬─────┘
                 ▼
        ┌────────────────┐
        │  Vault Client  │
        │ (Hashicorp SDK)│
        └────────────────┘
```

## Comparison with Other Tools

### vs. Klaviger's Vault Injector

**Klaviger** (studied as prior art):
- ✅ KV v1/v2 auto-detection (we reuse this pattern)
- ✅ Kubernetes SA auth (we reuse this pattern)
- ✅ Lease-aware caching (we reuse this pattern)
- ❌ No JWT/OIDC auth (we add this for SPIFFE)
- ❌ No token renewal (we implement this)
- ❌ HTTP header injection only (we support file output)

**This library:**
- ✅ All of Klaviger's patterns
- ✅ JWT/OIDC auth for SPIFFE workloads (priority 1)
- ✅ Background token renewal
- ✅ Pure library (no protocol dependencies)
- ✅ Batch secret reading
- ✅ Typed errors

### vs. Direct Vault SDK Usage

**Using Vault SDK directly:**
- ❌ Manual auth method selection
- ❌ Manual KV v1/v2 detection
- ❌ Manual caching logic
- ❌ Manual token renewal
- ❌ More boilerplate code

**This library:**
- ✅ All auth methods in one API
- ✅ Automatic KV version detection
- ✅ Built-in lease-aware caching
- ✅ Automatic token renewal
- ✅ Minimal boilerplate

## Attribution

This implementation follows patterns from:
- **Hashicorp Vault Go SDK** documentation
- **Klaviger project** (github.com/grs/klaviger) - studied as prior art for Kubernetes SA auth, KV auto-detection, and lease-aware caching
- **IBM Trusted Service Identity** examples (github.com/IBM/trusted-service-identity) - JWT/OIDC auth pattern for SPIFFE workloads

## License

Apache License 2.0 — See LICENSE file for details.
