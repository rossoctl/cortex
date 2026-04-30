# Klaviger Code Analysis — What We Can Reuse

## Overview

**Repository:** https://github.com/grs/klaviger  
**Owner:** Gordon Sim (@grs), Red Hat employee  
**Created:** March 9, 2026  
**Status:** Personal project, no license yet (as of inspection)  
**Purpose:** Sidecar proxy for configurable access control (reverse + forward proxy)

**Relation to Kagenti:** Reference implementation, not a competitor. Shows patterns we can learn from.

---

## Klaviger's Vault Integration

Klaviger implements Vault token injection as one of several "forward proxy modes" for outgoing requests.

### File: `internal/tokeninjector/vault_injector.go`

**What it does:**
1. Authenticates to Vault using:
   - **Kubernetes service account** (reads JWT from `/var/run/secrets/kubernetes.io/serviceaccount/token`)
   - **Token auth** (direct Vault token)
2. Reads secrets from Vault (supports KV v1 and KV v2)
3. Caches secrets with TTL (respects Vault lease duration)
4. Injects secrets as `Authorization: Bearer <token>` header

**Key features:**
- ✅ Lease-aware caching (uses `secret.LeaseDuration`)
- ✅ KV v1/v2 auto-detection (`secret.Data["data"]` wrapper check)
- ✅ Prometheus metrics integration
- ✅ Structured logging (zap)
- ❌ **Missing: JWT/OIDC auth** (SPIFFE pattern)
- ❌ **Missing: Lease renewal** (TODO comment in code)

---

## Code We Can Reuse

### 1. Vault Client Wrapper Pattern

**From Klaviger:**
```go
type VaultInjector struct {
    address    string
    path       string        // Secret path (e.g., "secret/data/my-token")
    field      string        // Field name in secret (e.g., "token")
    authMethod string
    role       string
    cacheTTL   time.Duration
    cache      *util.TokenCache
    client     *vault.Client
    logger     *zap.Logger
}
```

**What we can reuse:**
- ✅ Overall structure (client wrapper with config fields)
- ✅ Field extraction logic (KV v1/v2 detection)
- ✅ Cache key strategy (path-based)
- ✅ Lease duration handling

**What we need to change:**
- Add JWT auth method (read JWT-SVID from file)
- Use our existing `authlib/cache/` instead of Klaviger's `util.TokenCache`
- Adapt logging to our patterns

---

### 2. KV v1/v2 Auto-Detection

**From Klaviger (`readSecret` function):**
```go
// Check if this is a KV v2 secret (has "data" wrapper)
if data, ok := secret.Data["data"].(map[string]interface{}); ok {
    // KV v2
    if val, ok := data[i.field]; ok {
        tokenValue, ok = val.(string)
    }
} else {
    // KV v1 or other secret engine
    if val, ok := secret.Data[i.field]; ok {
        tokenValue, ok = val.(string)
    }
}
```

**Reusability:** ✅ **Copy directly with attribution**

This is a standard pattern for Vault's KV v1/v2 secret engines. The structure is:
- **KV v1:** `secret.Data[field]`
- **KV v2:** `secret.Data["data"][field]`

We can use this exact logic in our `authlib/vault/secret.go`.

---

### 3. Kubernetes Service Account Auth

**From Klaviger (`authenticateKubernetes` function):**
```go
func (i *VaultInjector) authenticateKubernetes() error {
    // Read service account token
    jwtPath := os.Getenv("KUBERNETES_SERVICE_ACCOUNT_TOKEN_PATH")
    if jwtPath == "" {
        jwtPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
    }

    jwt, err := os.ReadFile(jwtPath)
    if err != nil {
        return fmt.Errorf("failed to read service account token: %w", err)
    }

    // Prepare auth request
    data := map[string]interface{}{
        "role": i.role,
        "jwt":  string(jwt),
    }

    // Authenticate
    secret, err := i.client.Logical().Write("auth/kubernetes/login", data)
    if err != nil {
        return fmt.Errorf("kubernetes auth failed: %w", err)
    }

    // Set token
    i.client.SetToken(secret.Auth.ClientToken)
    return nil
}
```

**Reusability:** ✅ **Copy and adapt**

We can use this pattern for Kubernetes SA auth, then add a parallel JWT auth method that reads from `/opt/jwt_svid.token` instead.

---

### 4. Lease-Aware Caching

**From Klaviger (`Inject` function):**
```go
// Cache the token
ttl := i.cacheTTL
if leaseDuration > 0 {
    // Use lease duration if available, but cap at configured TTL
    leaseTTL := time.Duration(leaseDuration) * time.Second
    if leaseTTL < ttl {
        ttl = leaseTTL
    }
}
i.cache.Set(cacheKey, token, ttl)
```

**Reusability:** ✅ **Copy logic, adapt to our cache**

Strategy: Use the shorter of (configured TTL, Vault lease duration). This prevents using expired secrets.

We'll implement this in `authlib/vault/cache.go`, potentially extending our existing `authlib/cache/` package.

---

### 5. Configuration Structure

**From Klaviger example config (`examples/configs/vault-integration.yaml`):**
```yaml
forwardProxy:
  hostRules:
    - hostPattern: "internal.example.com"
      mode:
        type: "vault"
        vault:
          address: "https://vault.example.com"
          path: "secret/data/internal-api-token"
          field: "token"
          authMethod: "kubernetes"
          role: "klaviger"
          cacheTtl: "5m"
```

**Reusability:** ✅ **Adapt to our config structure**

Our config will be simpler (init container, not dynamic routing):
```yaml
vault:
  address: "https://vault.example.com"
  auth_method: "jwt"  # Our addition
  jwt_path: "/opt/jwt_svid.token"  # Our addition
  jwt_audience: "vault"  # Our addition
  role: "my-agent-role"
  cache_ttl: "5m"

secrets:
  - path: "secret/data/github/token"
    field: "token"
    output: "/shared/secrets/github-token"
```

---

## What We Need to Add (Not in Klaviger)

### 1. JWT/OIDC Auth Method (SPIFFE Pattern)

Klaviger only supports Kubernetes SA auth and token auth. We need to add JWT auth for SPIFFE workloads.

**Implementation (based on IBM TSI pattern):**
```go
func (c *Client) AuthenticateJWT(jwtPath, role, audience string) error {
    // Read JWT-SVID from file
    jwt, err := os.ReadFile(jwtPath)
    if err != nil {
        return fmt.Errorf("failed to read JWT-SVID: %w", err)
    }

    // Prepare auth request
    data := map[string]interface{}{
        "role": role,
        "jwt":  string(jwt),
    }

    // Authenticate to Vault's JWT auth backend
    secret, err := c.client.Logical().Write("auth/jwt/login", data)
    if err != nil {
        return fmt.Errorf("JWT auth failed: %w", err)
    }

    if secret == nil || secret.Auth == nil {
        return fmt.Errorf("no auth info returned from vault")
    }

    // Set token
    c.client.SetToken(secret.Auth.ClientToken)

    c.logger.Info("authenticated with vault using JWT",
        zap.String("role", role),
        zap.Duration("lease_duration", time.Duration(secret.Auth.LeaseDuration)*time.Second),
    )

    return nil
}
```

**Key differences from Kubernetes SA auth:**
- Auth endpoint: `auth/jwt/login` (not `auth/kubernetes/login`)
- JWT source: `/opt/jwt_svid.token` (not `/var/run/secrets/kubernetes.io/serviceaccount/token`)
- JWT must have correct audience claim (configured in SPIRE)

---

### 2. Lease Renewal (Background Goroutine)

Klaviger has a TODO comment for lease renewal. We should implement it:

```go
// TODO in Klaviger: Implement token renewal based on lease duration

// Our implementation:
func (c *Client) startTokenRenewal(secret *vault.Secret) {
    if secret.Auth.Renewable {
        go c.renewTokenLoop(secret.Auth.ClientToken, secret.Auth.LeaseDuration)
    }
}

func (c *Client) renewTokenLoop(token string, leaseDuration int) {
    // Renew at 2/3 of lease duration
    renewInterval := time.Duration(leaseDuration) * time.Second * 2 / 3
    ticker := time.NewTicker(renewInterval)
    defer ticker.Stop()

    for range ticker.C {
        secret, err := c.client.Auth().Token().RenewSelf(leaseDuration)
        if err != nil {
            c.logger.Error("failed to renew vault token", zap.Error(err))
            // TODO: Re-authenticate if renewal fails
            continue
        }
        c.logger.Info("vault token renewed",
            zap.Duration("new_lease", time.Duration(secret.Auth.LeaseDuration)*time.Second))
    }
}
```

---

### 3. Multiple Secret Fetching

Klaviger fetches one secret per request. Our init container should fetch **multiple secrets** in one run:

```go
type SecretConfig struct {
    Path   string `yaml:"path"`   // "secret/data/github/token"
    Field  string `yaml:"field"`  // "token"
    Output string `yaml:"output"` // "/shared/secrets/github-token"
}

func (f *Fetcher) FetchSecrets(configs []SecretConfig) error {
    for _, cfg := range configs {
        token, leaseDuration, err := f.client.ReadSecret(cfg.Path, cfg.Field)
        if err != nil {
            return fmt.Errorf("failed to fetch secret %s: %w", cfg.Path, err)
        }

        if err := f.writeSecretToFile(cfg.Output, token); err != nil {
            return fmt.Errorf("failed to write secret to %s: %w", cfg.Output, err)
        }

        f.logger.Info("secret fetched",
            zap.String("path", cfg.Path),
            zap.String("output", cfg.Output),
            zap.Int64("lease_duration", leaseDuration))
    }
    return nil
}
```

---

### 4. File Output Formats

Klaviger only injects HTTP headers. We need to write secrets to files in various formats:

**Individual files:**
```go
// /shared/secrets/github-token
func (f *Fetcher) writeSecretToFile(path, value string) error {
    dir := filepath.Dir(path)
    if err := os.MkdirAll(dir, 0755); err != nil {
        return err
    }
    return os.WriteFile(path, []byte(value), 0600) // Restrictive permissions
}
```

**Environment file (optional):**
```bash
# /shared/secrets/.env
GITHUB_TOKEN=ghp_xxxxx
DATABASE_PASSWORD=secret123
```

**JSON file (optional):**
```json
{
  "github_token": "ghp_xxxxx",
  "database_password": "secret123"
}
```

---

## Reusability Assessment

### Direct Copy with Attribution (60%)
- ✅ KV v1/v2 detection logic
- ✅ Kubernetes SA auth method
- ✅ Lease-aware caching strategy
- ✅ Basic Vault client setup
- ✅ Error handling patterns

### Adapt to Our Patterns (30%)
- 🔄 Configuration structure (simpler for our use case)
- 🔄 Cache implementation (use `authlib/cache/`)
- 🔄 Logging (adapt to our existing patterns)
- 🔄 Metrics (integrate with our observability)

### New Implementation (10%)
- ⚠️ JWT/OIDC auth method (SPIFFE pattern)
- ⚠️ Multiple secret fetching
- ⚠️ File output formats
- ⚠️ Init container CLI interface
- ⚠️ Lease renewal goroutine

---

## Licensing Considerations

**Current status:** Klaviger has **no license** (as of April 2026).

**Options:**
1. **Wait for license:** Contact Gordon Sim to clarify license intent
2. **Use as reference:** Implement our own code inspired by the patterns (not copy-paste)
3. **Attribute as prior art:** Document that we studied Klaviger's approach

**Recommendation:** Since Klaviger's Vault integration is relatively standard (uses Hashicorp's official Go client), and the patterns are well-documented in Vault's own documentation, we can:
- ✅ Implement our own version following the same patterns
- ✅ Attribute Klaviger as "prior art" in comments
- ✅ Focus on the 60% of logic that's standard Vault Go SDK usage

**Example attribution in code:**
```go
// Package vault provides Vault integration for Kagenti workloads.
//
// This implementation follows patterns from:
// - Hashicorp Vault Go SDK documentation
// - Klaviger project (github.com/grs/klaviger) - studied as prior art
// - IBM Trusted Service Identity examples
package vault
```

---

## Summary

**What we can reuse from Klaviger:**
- Overall architecture (client wrapper, caching, auth methods)
- KV v1/v2 detection (standard pattern)
- Lease-aware caching logic
- Kubernetes SA auth pattern

**What we need to add:**
- JWT/OIDC auth for SPIFFE (main gap)
- Multiple secret fetching
- File output instead of HTTP header injection
- CLI for init container use case
- Lease renewal implementation

**Reusability estimate:**
- ~60% of the core Vault logic is reusable (standard Vault SDK patterns)
- ~30% needs adaptation to our architecture
- ~10% is new functionality (SPIFFE auth, init container concerns)

**Next step:** Start with `authlib/vault/` implementing the reusable patterns, then build `vault-fetcher` CLI on top.
