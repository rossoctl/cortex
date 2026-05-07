# Webhook Integration Guide — vault-fetcher

This document describes how to integrate vault-fetcher with the kagenti-operator webhook for automatic injection.

## Overview

The kagenti-operator webhook can automatically inject vault-fetcher as an init container when pods are labeled with `kagenti.io/vault-fetcher-inject: "true"`.

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│                    WORKLOAD POD                         │
│                                                         │
│  INIT CONTAINERS (sequential):                         │
│  ┌────────────────────────────────────────────┐        │
│  │ 1. proxy-init (iptables setup)             │        │
│  └────────────────────────────────────────────┘        │
│  ┌────────────────────────────────────────────┐        │
│  │ 2. client-registration (Keycloak)          │        │
│  └────────────────────────────────────────────┘        │
│  ┌────────────────────────────────────────────┐        │
│  │ 3. vault-fetcher (NEW)                     │        │
│  │    - Reads JWT-SVID from spiffe-helper     │        │
│  │    - Authenticates to Vault                │        │
│  │    - Fetches configured secrets            │        │
│  │    - Writes to /shared/secrets/*           │        │
│  │    - Exits                                 │        │
│  └────────────────────────────────────────────┘        │
│                                                         │
│  SIDECARS (continuous):                                │
│  ┌────────────────────────────────────────────┐        │
│  │ spiffe-helper → writes JWT-SVID            │        │
│  └────────────────────────────────────────────┘        │
│  ┌────────────────────────────────────────────┐        │
│  │ authbridge (envoy + ext-proc)              │        │
│  │   - Validates inbound JWTs                 │        │
│  │   - Exchanges tokens for outbound          │        │
│  └────────────────────────────────────────────┘        │
│                                                         │
│  APPLICATION CONTAINER:                                │
│  ┌────────────────────────────────────────────┐        │
│  │ Your Agent/App                             │        │
│  │  - Reads secrets from /shared/secrets/*    │        │
│  │  - Makes HTTP calls (AuthBridge handles)   │        │
│  └────────────────────────────────────────────┘        │
└─────────────────────────────────────────────────────────┘
```

## Webhook Implementation

### Label-Based Injection

**Pod label:**
```yaml
metadata:
  labels:
    kagenti.io/type: agent
    kagenti.io/inject: "true"
    kagenti.io/vault-fetcher-inject: "true"  # Enable vault-fetcher
```

**Webhook behavior:**
- Checks for `kagenti.io/vault-fetcher-inject: "true"` label
- Injects vault-fetcher init container if label present
- Mounts required volumes and ConfigMaps

### Required Resources

**1. ConfigMap: `vault-fetcher-config`**
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: vault-fetcher-config
  namespace: <workload-namespace>
data:
  config.yaml: |
    vault:
      address: "https://vault.vault.svc.cluster.local:8200"
      auth_method: "jwt"
      role: "<agent-role>"
    secrets:
      - path: "secret/data/github/token"
        field: "token"
        output: "/shared/secrets/github-token"
```

**Must exist before pod creation.** Webhook does not create this.

### Webhook Injection Logic

**File:** `kagenti-operator/pkg/webhook/mutator.go` (example)

```go
func (m *Mutator) injectVaultFetcher(pod *corev1.Pod) error {
    // Check label
    if pod.Labels["kagenti.io/vault-fetcher-inject"] != "true" {
        return nil
    }

    // Add init container
    initContainer := corev1.Container{
        Name:  "vault-fetcher",
        Image: "ghcr.io/kagenti/kagenti-extensions/vault-fetcher:latest",
        Args:  []string{"--config=/etc/vault-fetcher/config.yaml"},
        VolumeMounts: []corev1.VolumeMount{
            {
                Name:      "spiffe-svid",
                MountPath: "/opt",
                ReadOnly:  true,
            },
            {
                Name:      "shared-secrets",
                MountPath: "/shared/secrets",
            },
            {
                Name:      "vault-fetcher-config",
                MountPath: "/etc/vault-fetcher",
                ReadOnly:  true,
            },
        },
        SecurityContext: &corev1.SecurityContext{
            RunAsUser:                pointer.Int64(65532),
            RunAsGroup:               pointer.Int64(65532),
            RunAsNonRoot:             pointer.Bool(true),
            AllowPrivilegeEscalation: pointer.Bool(false),
            Capabilities: &corev1.Capabilities{
                Drop: []corev1.Capability{"ALL"},
            },
            SeccompProfile: &corev1.SeccompProfile{
                Type: corev1.SeccompProfileTypeRuntimeDefault,
            },
        },
    }

    // Insert after client-registration, before app container
    pod.Spec.InitContainers = append(pod.Spec.InitContainers, initContainer)

    // Add volumes
    pod.Spec.Volumes = append(pod.Spec.Volumes,
        corev1.Volume{
            Name: "shared-secrets",
            VolumeSource: corev1.VolumeSource{
                EmptyDir: &corev1.EmptyDirVolumeSource{},
            },
        },
        corev1.Volume{
            Name: "vault-fetcher-config",
            VolumeSource: corev1.VolumeSource{
                ConfigMap: &corev1.ConfigMapVolumeSource{
                    LocalObjectReference: corev1.LocalObjectReference{
                        Name: "vault-fetcher-config",
                    },
                },
            },
        },
    )

    // Mount shared-secrets in app container
    for i := range pod.Spec.Containers {
        pod.Spec.Containers[i].VolumeMounts = append(
            pod.Spec.Containers[i].VolumeMounts,
            corev1.VolumeMount{
                Name:      "shared-secrets",
                MountPath: "/shared/secrets",
                ReadOnly:  true,
            },
        )
    }

    return nil
}
```

### Injection Order

**Critical:** Init containers run sequentially. Order matters!

1. **proxy-init** — Sets up iptables (runs first)
2. **client-registration** — Registers with Keycloak
3. **vault-fetcher** — Fetches secrets (after client-registration completes)
4. Application container starts (all init containers done)

**Why this order?**
- vault-fetcher needs JWT-SVID from spiffe-helper (running as sidecar)
- spiffe-helper starts with other sidecars, writes JWT-SVID early
- vault-fetcher reads JWT-SVID when it starts

### Configuration Examples

#### Minimal (GitHub token only)

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: vault-fetcher-config
data:
  config.yaml: |
    vault:
      address: "https://vault.vault.svc:8200"
      auth_method: "jwt"
      role: "github-agent"
    secrets:
      - path: "secret/data/github/token"
        field: "token"
        output: "/shared/secrets/github-token"
```

#### Multiple secrets

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: vault-fetcher-config
data:
  config.yaml: |
    vault:
      address: "https://vault.vault.svc:8200"
      auth_method: "jwt"
      role: "data-agent"
    secrets:
      - path: "secret/data/github/token"
        field: "token"
        output: "/shared/secrets/github-token"
      - path: "secret/data/slack/webhook"
        field: "url"
        output: "/shared/secrets/slack-webhook"
      - path: "secret/data/database/postgres"
        field: "password"
        output: "/shared/secrets/db-password"
    output_formats:
      env_file:
        enabled: true
        path: "/shared/secrets/.env"
```

#### With Kubernetes SA auth (no SPIFFE)

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: vault-fetcher-config
data:
  config.yaml: |
    vault:
      address: "https://vault.vault.svc:8200"
      auth_method: "kubernetes"  # Use K8s SA instead of JWT-SVID
      role: "my-app-role"
    secrets:
      - path: "secret/data/api-key"
        field: "key"
        output: "/shared/secrets/api-key"
```

## Vault Setup

### JWT Auth Configuration

```bash
# Enable JWT auth method
vault auth enable jwt

# Configure with SPIRE OIDC discovery URL
vault write auth/jwt/config \
  oidc_discovery_url="http://spire-server.spire.svc.cluster.local:8081" \
  default_role="default-agent"

# Create role for GitHub agents
vault write auth/jwt/role/github-agent \
  role_type="jwt" \
  bound_audiences="vault" \
  bound_claims_type="glob" \
  bound_claims='{"sub":"spiffe://localtest.me/ns/*/sa/github-agent/*"}' \
  user_claim="sub" \
  policies="github-agent" \
  ttl="1h"

# Create policy
vault policy write github-agent - <<EOF
path "secret/data/github/*" {
  capabilities = ["read"]
}
path "secret/data/slack/*" {
  capabilities = ["read"]
}
EOF

# Store secrets
vault kv put secret/github/token token="ghp_xxxxx"
vault kv put secret/slack/webhook url="https://hooks.slack.com/..."
```

## Testing

### Manual Testing (without webhook)

See `k8s/example-deployment.yaml` for a complete example with manual injection.

```bash
# Apply ConfigMap
kubectl apply -f k8s/configmap-vault-fetcher.yaml

# Deploy pod with manual injection
kubectl apply -f k8s/example-deployment.yaml

# Check init container logs
kubectl logs github-agent -c vault-fetcher

# Verify secrets were written
kubectl exec github-agent -- ls -la /shared/secrets
kubectl exec github-agent -- cat /shared/secrets/github-token
```

### With Webhook

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: github-agent
  labels:
    kagenti.io/type: agent
    kagenti.io/inject: "true"
    kagenti.io/vault-fetcher-inject: "true"  # Webhook will inject
spec:
  serviceAccountName: github-agent
  containers:
  - name: agent
    image: my-agent:latest
    # Webhook adds shared-secrets volume mount automatically
```

```bash
kubectl apply -f agent-with-webhook.yaml
kubectl logs github-agent -c vault-fetcher
```

## Troubleshooting

### vault-fetcher init container fails

```bash
# Check logs
kubectl logs <pod> -c vault-fetcher

# Common issues:
# 1. ConfigMap not found
kubectl get cm vault-fetcher-config -n <namespace>

# 2. JWT-SVID not ready
kubectl logs <pod> -c spiffe-helper

# 3. Vault auth fails
kubectl exec <pod> -c vault-fetcher -- cat /opt/jwt_svid.token
# Verify SPIFFE ID matches Vault role's bound_claims

# 4. Secret not found in Vault
vault kv get secret/github/token
```

### ConfigMap not mounted

```bash
# Verify ConfigMap exists
kubectl get cm vault-fetcher-config -n <namespace> -o yaml

# Check pod spec
kubectl get pod <pod> -o yaml | grep -A 10 vault-fetcher-config
```

### Secrets not accessible by app

```bash
# Verify secrets were written
kubectl exec <pod> -- ls -la /shared/secrets

# Check file permissions
kubectl exec <pod> -- stat /shared/secrets/github-token

# Verify volume mount in app container
kubectl get pod <pod> -o yaml | grep -A 5 "name: shared-secrets"
```

## Migration Guide

### From Manual ConfigMap to Webhook

**Before (manual):**
```yaml
spec:
  initContainers:
  - name: vault-fetcher
    image: ghcr.io/kagenti/kagenti-extensions/vault-fetcher:latest
    # ... full config ...
```

**After (webhook):**
```yaml
metadata:
  labels:
    kagenti.io/vault-fetcher-inject: "true"
spec:
  # Webhook injects everything automatically
```

### From Hardcoded Secrets to Vault

**Before:**
```yaml
env:
- name: GITHUB_TOKEN
  value: "ghp_xxxxx"  # ❌ Hardcoded
```

**After:**
```yaml
env:
- name: GITHUB_TOKEN_FILE
  value: "/shared/secrets/github-token"
```

**App code:**
```python
# Before
token = os.environ["GITHUB_TOKEN"]

# After
with open(os.environ["GITHUB_TOKEN_FILE"]) as f:
    token = f.read().strip()
```

## Security Considerations

1. **ConfigMap permissions:** Restrict who can edit `vault-fetcher-config`
2. **Vault policies:** Grant minimal permissions (only secrets needed)
3. **File permissions:** vault-fetcher uses 0600 by default
4. **SPIFFE ID validation:** Vault roles should use specific SPIFFE ID patterns
5. **Namespace isolation:** Each namespace has its own vault-fetcher-config

## Future Enhancements

- [ ] Sidecar mode for secret rotation
- [ ] Multiple ConfigMaps (default + pod-specific)
- [ ] Secret templating in ConfigMap
- [ ] Webhook validation (fail fast if ConfigMap missing)
- [ ] Metrics endpoint for monitoring

## Related Documentation

- [vault-fetcher README](../README.md) — CLI usage
- [authlib/vault README](../../authlib/vault/README.md) — Library reference
- [VAULT_PATTERN_OVERVIEW.md](../../../VAULT_PATTERN_OVERVIEW.md) — Architecture
