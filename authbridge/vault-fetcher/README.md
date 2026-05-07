# vault-fetcher — Vault Secret Fetcher for Kubernetes

Init container that fetches secrets from Hashicorp Vault using SPIFFE JWT-SVID authentication and writes them to files for application consumption.

## Features

- **SPIFFE-native** — Uses JWT-SVID from spiffe-helper for authentication
- **Multiple secrets** — Fetches multiple secrets in one run
- **Flexible output** — Individual files, environment file, or JSON
- **Secure permissions** — Configurable file permissions (default: 0600)
- **Fail-fast** — Clear error messages, exits with non-zero on failure
- **Minimal footprint** — Distroless container image, runs as non-root

## Quick Start

### Build

```bash
cd AuthBridge/vault-fetcher
podman build -t vault-fetcher:latest .
```

### Run Locally

```bash
# With config file
./vault-fetcher --config=config.yaml

# Show version
./vault-fetcher --version
```

### Deploy in Kubernetes

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: my-agent
spec:
  initContainers:
  - name: vault-fetcher
    image: ghcr.io/kagenti/kagenti-extensions/vault-fetcher:latest
    args:
    - --config=/etc/vault-fetcher/config.yaml
    volumeMounts:
    - name: spiffe-svid
      mountPath: /opt
      readOnly: true
    - name: shared-secrets
      mountPath: /shared/secrets
    - name: vault-fetcher-config
      mountPath: /etc/vault-fetcher
      readOnly: true

  containers:
  - name: app
    image: my-app:latest
    volumeMounts:
    - name: shared-secrets
      mountPath: /shared/secrets
      readOnly: true
    # App reads /shared/secrets/github-token, etc.

  volumes:
  - name: spiffe-svid
    emptyDir: {}
  - name: shared-secrets
    emptyDir: {}
  - name: vault-fetcher-config
    configMap:
      name: vault-fetcher-config
```

## Configuration

See [`config.yaml.example`](config.yaml.example) for a complete example.

### Minimal Configuration

```yaml
vault:
  address: "https://vault.example.com"
  auth_method: "jwt"
  role: "my-agent-role"

secrets:
  - path: "secret/data/github/token"
    field: "token"
    output: "/shared/secrets/github-token"
```

### Authentication Methods

#### JWT Auth (SPIFFE — Recommended)

```yaml
vault:
  auth_method: "jwt"
  role: "github-agent-role"
  jwt_path: "/opt/jwt_svid.token"  # Written by spiffe-helper
  jwt_audience: "vault"
```

**Vault setup:**
```bash
vault write auth/jwt/config \
  oidc_discovery_url="http://spire-server.spire.svc:8081"

vault write auth/jwt/role/github-agent-role \
  role_type="jwt" \
  bound_audiences="vault" \
  bound_claims='{"sub":"spiffe://localtest.me/ns/*/sa/github-agent/*"}' \
  policies="github-agent" \
  ttl="1h"
```

#### Kubernetes SA Auth

```yaml
vault:
  auth_method: "kubernetes"
  role: "my-app-role"
```

#### Token Auth (Dev/Testing)

```yaml
vault:
  auth_method: "token"
  token: "hvs.CAESIAbc..."
```

Or via environment variable:
```bash
export VAULT_TOKEN="hvs.CAESIAbc..."
vault-fetcher --config=config.yaml
```

### Secret Specification

```yaml
secrets:
  - path: "secret/data/github/token"   # KV v2 path
    field: "token"                      # Field name in secret
    output: "/shared/secrets/github-token"  # Where to write
    mode: "0600"                        # File permissions (optional)
```

**Supported secret engines:**
- KV v1: `secret/github/token`
- KV v2: `secret/data/github/token` (auto-detected)

### Output Formats

#### Individual Files (Default)

```yaml
secrets:
  - path: "secret/data/github/token"
    field: "token"
    output: "/shared/secrets/github-token"
```

Result:
```bash
/shared/secrets/
└── github-token  # Contains: ghp_xxxxx
```

#### Environment File

```yaml
output_formats:
  env_file:
    enabled: true
    path: "/shared/secrets/.env"
```

Result:
```bash
# /shared/secrets/.env
TOKEN=ghp_xxxxx
URL=https://hooks.slack.com/...
PASSWORD=secret123
```

Usage:
```bash
source /shared/secrets/.env
echo $TOKEN
```

#### JSON File

```yaml
output_formats:
  json_file:
    enabled: true
    path: "/shared/secrets/credentials.json"
```

Result:
```json
{
  "token": "ghp_xxxxx",
  "url": "https://hooks.slack.com/...",
  "password": "secret123"
}
```

### Environment Variable Overrides

Configuration values can be overridden via environment variables:

```bash
export VAULT_ADDR="https://vault.example.com"
export VAULT_ROLE="my-role"
export JWT_PATH="/custom/path/jwt.token"
export VAULT_TOKEN="hvs.CAESIAbc..."

vault-fetcher --config=config.yaml
```

## Usage in Kagenti

When deployed via the kagenti-webhook, the init container is automatically injected:

```yaml
apiVersion: v1
kind: Pod
metadata:
  labels:
    kagenti.io/type: agent
    kagenti.io/inject: "true"
    kagenti.io/vault-fetcher-inject: "true"  # Enable vault-fetcher
spec:
  serviceAccountName: github-agent
  # Webhook injects vault-fetcher init container automatically
```

ConfigMap in the same namespace:
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: vault-fetcher-config
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

## Exit Codes

- `0` — Success (all secrets fetched and written)
- `1` — Failure (config error, auth failure, secret not found, etc.)

On failure, the pod will not start (init container failed). Check logs:
```bash
kubectl logs my-pod -c vault-fetcher
```

## Security Considerations

### File Permissions

- Default file mode: `0600` (owner read/write only)
- Secrets directory: Created with `0755` permissions
- Container runs as non-root (UID 65532)

### Least Privilege

Only fetch secrets the application needs:
```yaml
# ✅ Good - minimal
secrets:
  - path: "secret/data/github/token"
    field: "token"
    output: "/shared/secrets/github-token"

# ❌ Bad - fetches everything under a path
# (auto-fetch not currently supported - by design)
```

Vault policy should match:
```hcl
path "secret/data/github/token" {
  capabilities = ["read"]
}
```

### Token Rotation

vault-fetcher runs once at pod startup. To get updated secrets:
- Restart the pod
- Or (future) use a sidecar mode with automatic refresh

## Troubleshooting

### Authentication Fails

```
Failed to authenticate to Vault: JWT auth failed
```

**Check:**
1. JWT-SVID exists: `kubectl exec pod -c vault-fetcher -- cat /opt/jwt_svid.token`
2. Vault JWT auth is configured: `vault read auth/jwt/config`
3. Vault role exists: `vault read auth/jwt/role/my-role`
4. SPIFFE ID matches role's `bound_claims`

### Secret Not Found

```
Failed to fetch secret secret/data/github/token: secret not found
```

**Check:**
1. Secret exists: `vault kv get secret/github/token`
2. Path is correct (KV v2 uses `secret/data/...`)
3. Vault policy allows read access

### Permission Denied

```
Failed to write secret to /shared/secrets/github-token: permission denied
```

**Check:**
1. Volume is mounted and writable
2. Container is not running as root with read-only filesystem
3. SELinux/AppArmor policies

## Development

### Local Testing

```bash
# Start Vault in dev mode
docker run -d --name vault-dev -p 8200:8200 \
  -e VAULT_DEV_ROOT_TOKEN_ID=test-token \
  hashicorp/vault:latest

# Configure Vault
export VAULT_ADDR=http://localhost:8200
export VAULT_TOKEN=test-token

vault kv put secret/github token="ghp_test123"

# Run vault-fetcher
export VAULT_TOKEN=test-token
./vault-fetcher --config=config.yaml.example

# Check output
cat /tmp/secrets/github-token
```

### Build for Multiple Architectures

```bash
podman build --platform=linux/amd64,linux/arm64 \
  -t ghcr.io/kagenti/kagenti-extensions/vault-fetcher:latest .
```

## Comparison with Other Tools

### vs. Vault Agent

| Feature | vault-fetcher | Vault Agent |
|---------|---------------|-------------|
| Purpose | Init container (fetch once) | Sidecar (continuous sync) |
| Complexity | Simple (~300 LOC) | Feature-rich |
| SPIFFE auth | ✅ Native | ➖ Via JWT auth |
| Resource usage | Minimal (exits after fetch) | Ongoing (stays running) |
| Use case | Static credentials at startup | Dynamic secrets, rotation |

### vs. External Secrets Operator

| Feature | vault-fetcher | External Secrets |
|---------|---------------|------------------|
| Approach | Init container | K8s Operator |
| Setup | ConfigMap per pod | SecretStore CRD |
| SPIFFE auth | ✅ Yes | ➖ Via service account |
| Dependency | None (self-contained) | Cluster-wide operator |
| Scope | Per-pod secrets | Cluster-wide management |

**Use vault-fetcher when:**
- You want SPIFFE-native auth
- You need per-pod secret configuration
- You prefer init containers over operators
- You want minimal dependencies

## License

Apache License 2.0 — See LICENSE file for details.
