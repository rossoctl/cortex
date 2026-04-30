# Vault Implementation Plan — Kagenti Extensions

## Goal

Implement the Vault pattern to allow Kagenti workloads to retrieve static credentials (API keys, database passwords) from Hashicorp Vault using their SPIFFE identity for authentication.

**Key requirement:** Prioritize SPIFFE workloads using JWT auth method.

---

## Phase 1: authlib/vault — Reusable Library (Week 1-2)

### Goal
Create a protocol-agnostic Vault client library in `authlib/vault/` that can be imported by any Go project.

### Tasks

#### 1.1 Package Structure
```
authlib/vault/
├── client.go       # Vault client wrapper
├── auth.go         # Authentication methods (JWT, Kubernetes SA, token)
├── secret.go       # Secret reading (KV v1/v2)
├── cache.go        # Lease-aware secret caching (extends authlib/cache)
├── config.go       # Configuration structs
├── errors.go       # Typed errors
├── client_test.go  # Unit tests
└── README.md       # Library documentation
```

#### 1.2 Core Features

**Client wrapper (`client.go`):**
```go
type Client struct {
    vaultClient *vault.Client
    config      *Config
    cache       *SecretCache
    logger      *log.Logger
}

func NewClient(cfg *Config) (*Client, error)
func (c *Client) Authenticate() error
func (c *Client) ReadSecret(path, field string) (string, int64, error)
func (c *Client) Close() error
```

**Authentication methods (`auth.go`):**
```go
// Priority 1: JWT/OIDC auth (SPIFFE pattern)
func (c *Client) AuthenticateJWT(jwtPath, role string) error

// Priority 2: Kubernetes SA auth (fallback)
func (c *Client) AuthenticateKubernetes(role string) error

// Priority 3: Token auth (dev/testing)
func (c *Client) AuthenticateToken(token string) error

// Helper: Start token renewal goroutine
func (c *Client) startTokenRenewal(secret *vault.Secret)
```

**Secret operations (`secret.go`):**
```go
// Auto-detect KV v1/v2 and extract field
func (c *Client) ReadSecret(path, field string) (value string, leaseDuration int64, err error)

// Batch read multiple secrets
func (c *Client) ReadSecrets(requests []SecretRequest) ([]SecretResponse, error)
```

**Caching (`cache.go`):**
```go
// Extend authlib/cache with Vault-specific features
type SecretCache struct {
    cache *cache.Cache // Reuse authlib/cache
}

func (sc *SecretCache) Get(path string) (string, bool)
func (sc *SecretCache) Set(path, value string, leaseDuration int64)
```

**Configuration (`config.go`):**
```go
type Config struct {
    Address    string        `yaml:"address"`     // "https://vault.example.com"
    AuthMethod string        `yaml:"auth_method"` // "jwt", "kubernetes", "token"
    Role       string        `yaml:"role"`        // Vault role name
    JWTPath    string        `yaml:"jwt_path"`    // "/opt/jwt_svid.token"
    CacheTTL   time.Duration `yaml:"cache_ttl"`   // Max cache duration
    TLSConfig  *TLSConfig    `yaml:"tls"`         // TLS settings
}

type SecretRequest struct {
    Path  string `yaml:"path"`  // "secret/data/github/token"
    Field string `yaml:"field"` // "token"
}
```

#### 1.3 Code Reuse from Klaviger

**Direct reuse (with attribution):**
- KV v1/v2 detection logic (`secret.go`)
- Kubernetes SA auth pattern (`auth.go`)
- Lease-aware caching strategy (`cache.go`)
- Basic client setup

**Attribution in comments:**
```go
// Package vault provides Vault integration for Kagenti workloads.
//
// This implementation follows patterns from:
// - Hashicorp Vault Go SDK documentation
// - Klaviger project (github.com/grs/klaviger) - studied as prior art
// - IBM Trusted Service Identity examples (github.com/IBM/trusted-service-identity)
//
// Key patterns adapted from Klaviger:
// - KV v1/v2 auto-detection
// - Lease-aware caching
// - Kubernetes SA authentication
//
// Key additions for Kagenti:
// - JWT/OIDC authentication (SPIFFE pattern from IBM TSI)
// - Token renewal with goroutine
// - Integration with authlib/cache
package vault
```

**New implementation:**
- JWT auth method (from IBM TSI docs)
- Lease renewal goroutine
- Batch secret reading
- Integration with existing `authlib/cache/`

#### 1.4 Testing

**Unit tests (`client_test.go`):**
- Mock Vault server (using `httptest`)
- Test JWT auth flow
- Test KV v1/v2 detection
- Test lease-aware caching
- Test error handling

**Integration tests (optional, requires real Vault):**
- Docker Compose with Vault + SPIRE
- End-to-end JWT auth test

#### 1.5 Documentation

**`authlib/vault/README.md`:**
```markdown
# authlib/vault — Vault Integration

Pure Go library for Hashicorp Vault integration with SPIFFE support.

## Features
- JWT/OIDC authentication (SPIFFE JWT-SVID)
- Kubernetes service account authentication
- KV v1 and KV v2 secret engine support
- Lease-aware caching with automatic renewal
- No protocol dependencies (no gRPC, no Envoy)

## Usage
[Example code]

## Authentication Methods
[Details on JWT, Kubernetes SA, token auth]

## Configuration
[Config struct reference]
```

### Deliverables (Phase 1)
- [ ] `authlib/vault/` package with all core features
- [ ] Unit tests with >80% coverage
- [ ] README.md with usage examples
- [ ] Integration with existing `authlib/cache/`
- [ ] Update `authlib/go.mod` with Vault SDK dependency

**Estimated effort:** 5-7 days

---

## Phase 2: vault-fetcher — Init Container CLI (Week 3)

### Goal
Build a standalone CLI tool that fetches secrets from Vault at pod startup and writes them to files.

### Tasks

#### 2.1 Directory Structure
```
AuthBridge/vault-fetcher/
├── main.go                # CLI entrypoint
├── config.yaml.example    # Example configuration
├── Dockerfile             # Minimal image (distroless)
├── README.md              # Usage documentation
└── scripts/
    └── test-local.sh      # Local testing script
```

#### 2.2 CLI Features

**Main command:**
```bash
vault-fetcher --config=/etc/vault-fetcher/config.yaml
```

**Configuration file (`config.yaml`):**
```yaml
vault:
  address: "https://vault.example.com"
  auth_method: "jwt"
  role: "github-agent-role"
  jwt_path: "/opt/jwt_svid.token"
  cache_ttl: "5m"

secrets:
  - path: "secret/data/github/token"
    field: "token"
    output: "/shared/secrets/github-token"
    mode: "0600"  # File permissions
  
  - path: "secret/data/database/postgres"
    field: "password"
    output: "/shared/secrets/db-password"
    mode: "0600"

# Optional: Output formats
output_formats:
  # Write all secrets to an env file
  env_file:
    enabled: true
    path: "/shared/secrets/.env"
    format: "KEY=value"
  
  # Write all secrets to a JSON file
  json_file:
    enabled: false
    path: "/shared/secrets/credentials.json"
```

**Environment variable overrides:**
```bash
VAULT_ADDR               # Override vault.address
VAULT_ROLE               # Override vault.role
JWT_PATH                 # Override vault.jwt_path
CONFIG_FILE              # Override config file path
```

**Behavior:**
1. Load configuration (file + env overrides)
2. Create Vault client (using `authlib/vault`)
3. Authenticate (JWT method by default)
4. Fetch all configured secrets
5. Write secrets to output files with correct permissions
6. Optional: Write env file and/or JSON file
7. Log success/failure
8. Exit with status code (0 = success, non-zero = failure)

**Error handling:**
- Retry authentication (3 attempts with backoff)
- Fail fast on config errors
- Log detailed errors for debugging
- Exit with non-zero code on any failure (pod will restart)

#### 2.3 Implementation (`main.go`)

```go
package main

import (
    "flag"
    "log"
    "os"
    
    "github.com/kagenti/kagenti-extensions/authbridge/authlib/vault"
)

type Config struct {
    Vault   vault.Config   `yaml:"vault"`
    Secrets []SecretConfig `yaml:"secrets"`
    Output  OutputConfig   `yaml:"output_formats"`
}

type SecretConfig struct {
    Path   string `yaml:"path"`
    Field  string `yaml:"field"`
    Output string `yaml:"output"`
    Mode   string `yaml:"mode"` // Octal file permissions
}

type OutputConfig struct {
    EnvFile  *EnvFileConfig  `yaml:"env_file"`
    JSONFile *JSONFileConfig `yaml:"json_file"`
}

func main() {
    configFile := flag.String("config", "/etc/vault-fetcher/config.yaml", "Config file path")
    flag.Parse()
    
    // Load config
    cfg, err := loadConfig(*configFile)
    if err != nil {
        log.Fatalf("Failed to load config: %v", err)
    }
    
    // Create Vault client
    client, err := vault.NewClient(&cfg.Vault)
    if err != nil {
        log.Fatalf("Failed to create Vault client: %v", err)
    }
    defer client.Close()
    
    // Authenticate
    if err := client.Authenticate(); err != nil {
        log.Fatalf("Failed to authenticate to Vault: %v", err)
    }
    log.Println("Successfully authenticated to Vault")
    
    // Fetch secrets
    fetched := make(map[string]string)
    for _, secret := range cfg.Secrets {
        value, _, err := client.ReadSecret(secret.Path, secret.Field)
        if err != nil {
            log.Fatalf("Failed to fetch secret %s: %v", secret.Path, err)
        }
        
        // Write to file
        if err := writeSecretToFile(secret.Output, value, secret.Mode); err != nil {
            log.Fatalf("Failed to write secret to %s: %v", secret.Output, err)
        }
        
        fetched[secret.Field] = value
        log.Printf("Secret fetched: %s -> %s", secret.Path, secret.Output)
    }
    
    // Optional: Write env file
    if cfg.Output.EnvFile != nil && cfg.Output.EnvFile.Enabled {
        if err := writeEnvFile(cfg.Output.EnvFile.Path, fetched); err != nil {
            log.Fatalf("Failed to write env file: %v", err)
        }
    }
    
    // Optional: Write JSON file
    if cfg.Output.JSONFile != nil && cfg.Output.JSONFile.Enabled {
        if err := writeJSONFile(cfg.Output.JSONFile.Path, fetched); err != nil {
            log.Fatalf("Failed to write JSON file: %v", err)
        }
    }
    
    log.Println("All secrets fetched successfully")
}
```

#### 2.4 Container Image

**Dockerfile:**
```dockerfile
# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /build
COPY authlib/ authlib/
COPY vault-fetcher/ vault-fetcher/

WORKDIR /build/vault-fetcher
RUN go mod download
RUN CGO_ENABLED=0 go build -o vault-fetcher -ldflags="-s -w" .

# Runtime stage
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /build/vault-fetcher/vault-fetcher /usr/local/bin/vault-fetcher

USER 1000:1000

ENTRYPOINT ["/usr/local/bin/vault-fetcher"]
```

**Build & push:**
```bash
cd AuthBridge/vault-fetcher
podman build -t ghcr.io/kagenti/kagenti-extensions/vault-fetcher:latest .
podman push ghcr.io/kagenti/kagenti-extensions/vault-fetcher:latest
```

#### 2.5 Testing

**Local testing script (`scripts/test-local.sh`):**
```bash
#!/bin/bash
set -euo pipefail

# Start Vault in dev mode
docker run -d --name vault-dev -p 8200:8200 \
  -e VAULT_DEV_ROOT_TOKEN_ID=test-token \
  hashicorp/vault:latest

# Wait for Vault to be ready
sleep 2

# Configure Vault
export VAULT_ADDR=http://localhost:8200
export VAULT_TOKEN=test-token

# Enable JWT auth
vault auth enable jwt

# Configure JWT auth (using SPIRE OIDC discovery)
vault write auth/jwt/config \
  oidc_discovery_url="http://spire-server.spire.svc:8081" \
  default_role="test-role"

# Create policy
vault policy write test-policy - <<EOF
path "secret/data/*" {
  capabilities = ["read"]
}
EOF

# Create role
vault write auth/jwt/role/test-role \
  role_type="jwt" \
  bound_audiences="vault" \
  user_claim="sub" \
  policies="test-policy" \
  ttl="1h"

# Put test secret
vault kv put secret/github token="ghp_test123"

# Run vault-fetcher
./vault-fetcher --config=config.yaml.example

# Verify
cat /tmp/secrets/github-token

# Cleanup
docker stop vault-dev && docker rm vault-dev
```

#### 2.6 Documentation

**`AuthBridge/vault-fetcher/README.md`:**
```markdown
# vault-fetcher — Vault Secret Fetcher for Kubernetes

Init container that fetches secrets from Hashicorp Vault using SPIFFE JWT-SVID authentication.

## Features
- SPIFFE-native (uses JWT-SVID from spiffe-helper)
- Fetches multiple secrets in one run
- Writes secrets to files with correct permissions
- Optional env file and JSON output
- Fail-fast error handling

## Usage

### Kubernetes Deployment
[Example YAML with init container]

### Configuration
[Config reference]

### Authentication
[JWT auth setup guide]

## Examples
[Complete examples]
```

### Deliverables (Phase 2)
- [ ] `vault-fetcher` CLI with config file support
- [ ] Dockerfile for init container image
- [ ] Local testing script
- [ ] README.md with usage examples
- [ ] Example Kubernetes deployment YAML

**Estimated effort:** 3-4 days

---

## Phase 3: Webhook Integration (Week 4)

### Goal
Make vault-fetcher injectable via kagenti-webhook (in kagenti-operator repo).

### Tasks

#### 3.1 Webhook Changes (kagenti-operator repo)

**Add vault-fetcher init container injection:**

1. **Label control:**
   ```yaml
   kagenti.io/vault-fetcher-inject: "true"
   ```

2. **ConfigMap for configuration:**
   ```yaml
   apiVersion: v1
   kind: ConfigMap
   metadata:
     name: vault-fetcher-config
     namespace: agent-namespace
   data:
     config.yaml: |
       vault:
         address: "https://vault.example.com"
         auth_method: "jwt"
         role: "github-agent-role"
       secrets:
         - path: "secret/data/github/token"
           field: "token"
           output: "/shared/secrets/github-token"
   ```

3. **Init container injection (in webhook mutator):**
   ```go
   // Add vault-fetcher init container
   if shouldInjectVaultFetcher(pod) {
       initContainers = append(initContainers, corev1.Container{
           Name:  "vault-fetcher",
           Image: "ghcr.io/kagenti/kagenti-extensions/vault-fetcher:latest",
           Args:  []string{"--config=/etc/vault-fetcher/config.yaml"},
           VolumeMounts: []corev1.VolumeMount{
               {Name: "spiffe-svid", MountPath: "/opt", ReadOnly: true},
               {Name: "shared-secrets", MountPath: "/shared/secrets"},
               {Name: "vault-fetcher-config", MountPath: "/etc/vault-fetcher"},
           },
       })
   }
   ```

4. **Update webhook documentation**

#### 3.2 Demo Setup

**Create `AuthBridge/demos/vault-pattern/`:**

```
AuthBridge/demos/vault-pattern/
├── README.md                    # Complete walkthrough
├── demo-ui.md                   # UI-based demo
├── setup_vault.sh               # Vault setup script
├── setup_keycloak.py            # Keycloak setup (reuse existing)
└── k8s/
    ├── vault-config.yaml        # Vault JWT auth config
    ├── configmaps.yaml          # vault-fetcher-config + authbridge-config
    ├── github-agent.yaml        # Agent deployment with vault-fetcher
    └── test-target.yaml         # Test target service
```

**Demo scenario:**
1. Agent needs to call GitHub API (requires PAT from Vault)
2. Agent also calls internal service (uses AuthBridge token exchange)
3. Shows both Vault pattern (static creds) and OAuth pattern (dynamic tokens)

**Setup script (`setup_vault.sh`):**
```bash
#!/bin/bash
# Configure Vault with JWT auth for SPIFFE workloads

VAULT_ADDR=${VAULT_ADDR:-"http://vault.vault.svc:8200"}
SPIRE_OIDC_URL=${SPIRE_OIDC_URL:-"http://spire-server.spire.svc:8081"}

# Enable JWT auth
vault auth enable jwt

# Configure JWT auth
vault write auth/jwt/config \
  oidc_discovery_url="${SPIRE_OIDC_URL}" \
  default_role="github-agent"

# Create policy
vault policy write github-agent - <<EOF
path "secret/data/github/*" {
  capabilities = ["read"]
}
EOF

# Create role with SPIFFE ID pattern
vault write auth/jwt/role/github-agent \
  role_type="jwt" \
  bound_audiences="vault" \
  bound_claims_type="glob" \
  bound_claims='{"sub":"spiffe://localtest.me/ns/*/sa/github-agent/*"}' \
  user_claim="sub" \
  policies="github-agent" \
  ttl="1h"

# Put GitHub token
vault kv put secret/github/token token="${GITHUB_TOKEN}"

echo "Vault configuration complete"
```

### Deliverables (Phase 3)
- [ ] Webhook changes for vault-fetcher injection (kagenti-operator repo)
- [ ] Complete demo in `AuthBridge/demos/vault-pattern/`
- [ ] Vault setup script
- [ ] Demo documentation (README + demo-ui)
- [ ] Integration test

**Estimated effort:** 3-4 days

---

## Phase 4: Documentation & Polish (Week 5)

### Goal
Complete documentation and integrate into existing docs structure.

### Tasks

1. **Update AuthBridge/CLAUDE.md:**
   - Add Vault pattern section
   - Document vault-fetcher init container
   - Update shared volume contract

2. **Update root CLAUDE.md:**
   - Add vault-fetcher to container images table
   - Document new ConfigMap requirements

3. **Create authlib/vault/README.md:**
   - API reference
   - Usage examples
   - Authentication guide

4. **Add to demos index:**
   - Update `AuthBridge/demos/README.md`
   - Add vault-pattern to recommended learning path

5. **CI/CD updates:**
   - Add vault-fetcher to `.github/workflows/build.yaml`
   - Add unit tests to CI
   - Add integration test (optional)

### Deliverables (Phase 4)
- [ ] Updated CLAUDE.md files
- [ ] Complete API documentation
- [ ] CI/CD integration
- [ ] Final review and testing

**Estimated effort:** 2-3 days

---

## Timeline Summary

| Phase | Duration | Key Deliverables |
|-------|----------|------------------|
| Phase 1: authlib/vault | 5-7 days | Reusable library with JWT auth |
| Phase 2: vault-fetcher | 3-4 days | Init container CLI + Dockerfile |
| Phase 3: Webhook | 3-4 days | Injection logic + demo |
| Phase 4: Documentation | 2-3 days | Complete docs + CI |
| **Total** | **13-18 days** | **Production-ready Vault pattern** |

---

## Success Criteria

- [ ] `authlib/vault/` library with JWT auth support
- [ ] `vault-fetcher` init container working in Kind cluster
- [ ] Webhook can inject vault-fetcher based on pod label
- [ ] Demo showing GitHub agent fetching PAT from Vault
- [ ] Unit tests with >80% coverage
- [ ] Complete documentation
- [ ] CI/CD builds vault-fetcher image

---

## Open Questions

1. **Vault instance:** Do we need to add Vault to the Kind cluster setup, or will users bring their own?
2. **Lease renewal:** Should vault-fetcher stay running for renewal (sidecar mode), or is one-time fetch sufficient (init container mode)?
3. **Secret rotation:** Do we need to support secret rotation without pod restart?
4. **Vault namespace support:** Should we support Vault Enterprise namespaces?

---

## Next Steps

1. **Review this plan** and get approval
2. **Set up development environment** (Kind cluster with SPIRE)
3. **Start Phase 1** — Create `authlib/vault/` package
4. **Prototype JWT auth** — Test with real SPIRE + Vault integration

Ready to begin?
