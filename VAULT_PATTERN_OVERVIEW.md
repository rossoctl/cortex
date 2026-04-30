# Vault Pattern Overview — Kagenti Extensions

## The Problem

Legacy applications need **static credentials** (API keys, database passwords, service tokens) that can't be dynamically exchanged like OAuth tokens. These credentials:
- Are often hardcoded or in environment variables
- Need to be rotated periodically
- Should be access-controlled based on workload identity
- Require audit trails

## The Solution: Two Complementary Patterns

Kagenti uses **two different patterns** for different credential types:

### Pattern 1: Dynamic Token Exchange (Existing — AuthBridge Unified Binary)

**Use case:** Service-to-service calls within the mesh where both sides support OAuth 2.0

**How it works:**
```
Agent → AuthBridge sidecar → Token exchange (RFC 8693) → Target Service
```

**Flow:**
1. Agent makes HTTP request to `target-service.namespace.svc`
2. Envoy intercepts the request (iptables redirect)
3. AuthBridge binary (ext-proc) exchanges the agent's token for one with target's audience
4. Target service validates the exchanged token

**Example:** Agent calling another internal service that uses Keycloak

**Handled by:** `cmd/authbridge/` unified binary (already implemented)

---

### Pattern 2: Static Credential Retrieval (New — Vault Integration)

**Use case:** Legacy APIs or external services that require static API keys/credentials

**How it works:**
```
Pod startup → Vault fetcher → Authenticate with SPIFFE → Fetch secrets → Write to files → App reads files
```

**Flow:**
1. Pod starts, spiffe-helper writes JWT-SVID to `/opt/jwt_svid.token`
2. **Vault fetcher init container** (new component):
   - Reads the JWT-SVID
   - Authenticates to Vault using JWT auth method
   - Fetches configured secrets (e.g., GitHub API key, database password)
   - Writes secrets to shared volume (e.g., `/shared/secrets/github-token`)
   - Exits
3. Application container starts, reads secrets from files
4. Application uses static credentials to call external APIs

**Example:** Agent needs to call GitHub API using a personal access token stored in Vault

**Handled by:** New `vault-fetcher` init container (to be implemented)

---

## Why Two Separate Components?

### AuthBridge Unified Binary (`cmd/authbridge/`)
- **Purpose:** Traffic interception and dynamic token manipulation
- **Pattern:** "On every HTTP request, do something"
- **Lifecycle:** Long-running sidecar
- **Protocol:** HTTP-specific (Envoy ext-proc)
- **Token type:** Dynamic OAuth 2.0 access tokens with short TTL

### Vault Fetcher Init Container (`AuthBridge/vault-fetcher/`)
- **Purpose:** One-time secret retrieval at pod startup
- **Pattern:** "Fetch once at startup, write to files, exit"
- **Lifecycle:** Init container (runs to completion before app starts)
- **Protocol:** Agnostic (just writes files)
- **Token type:** Static credentials (API keys, passwords) that may be long-lived

**Analogy:**
- **AuthBridge** is like a security guard at the door checking every visitor's badge and issuing temporary passes
- **Vault fetcher** is like picking up your permanent office keys from HR on your first day

---

## How the Webhook Fits In

The **kagenti-webhook** (in [kagenti-operator](https://github.com/kagenti/kagenti-operator)) is the **orchestrator** that injects ALL sidecars into workloads:

**Current injections:**
1. `proxy-init` (init container) — Sets up iptables
2. `spiffe-helper` (sidecar) — Fetches JWT-SVIDs from SPIRE
3. `client-registration` (init container) — Registers with Keycloak
4. `envoy-proxy` + `authbridge` (sidecar) — Traffic interception & token exchange

**New injection (Vault pattern):**
5. `vault-fetcher` (init container) — Fetches secrets from Vault

**Control:** Pod label `kagenti.io/vault-fetcher-inject: "true"`

**Configuration:** ConfigMap `vault-fetcher-config` in the pod's namespace:
```yaml
vault:
  address: "https://vault.example.com"
  auth_method: "jwt"
  role: "github-agent-role"
secrets:
  - path: "secret/data/github/token"
    field: "token"
    output: "/shared/secrets/github-token"
```

**Execution order:**
1. `proxy-init` runs (sets up iptables)
2. `client-registration` runs (registers with Keycloak)
3. `vault-fetcher` runs ← NEW (fetches secrets from Vault)
4. Application container starts (reads secrets, makes HTTP calls)
5. `spiffe-helper` + `authbridge` + `envoy-proxy` run as sidecars (handle ongoing traffic)

---

## Architecture Diagram

```
┌─────────────────────────────────────────────────────────────────────┐
│                          WORKLOAD POD                               │
│                                                                     │
│  INIT CONTAINERS (run sequentially):                               │
│  ┌────────────────────────────────────────────────────┐            │
│  │ 1. proxy-init (iptables setup)                     │            │
│  └────────────────────────────────────────────────────┘            │
│  ┌────────────────────────────────────────────────────┐            │
│  │ 2. client-registration (Keycloak setup)            │            │
│  └────────────────────────────────────────────────────┘            │
│  ┌────────────────────────────────────────────────────┐            │
│  │ 3. vault-fetcher (NEW)                             │            │
│  │    - Read JWT-SVID from spiffe-helper              │            │
│  │    - Authenticate to Vault (JWT auth)              │            │
│  │    - Fetch secrets                                 │            │
│  │    - Write to /shared/secrets/*                    │            │
│  │    - Exit                                          │            │
│  └────────────────────────────────────────────────────┘            │
│                                                                     │
│  SIDECARS (run continuously):                                      │
│  ┌────────────────────────────────────────────────────┐            │
│  │ spiffe-helper → writes JWT-SVID                    │            │
│  └────────────────────────────────────────────────────┘            │
│  ┌────────────────────────────────────────────────────┐            │
│  │ envoy-proxy + authbridge (unified binary)          │            │
│  │   - Intercepts HTTP traffic (iptables)             │            │
│  │   - Validates inbound JWTs                         │            │
│  │   - Exchanges tokens for outbound calls            │            │
│  └────────────────────────────────────────────────────┘            │
│                                                                     │
│  APPLICATION CONTAINER:                                            │
│  ┌────────────────────────────────────────────────────┐            │
│  │ Your Agent/App                                     │            │
│  │  - Reads secrets from /shared/secrets/*            │            │
│  │  - Makes HTTP calls (intercepted by AuthBridge)    │            │
│  └────────────────────────────────────────────────────┘            │
│                                                                     │
│  SHARED VOLUMES:                                                   │
│  - /opt/jwt_svid.token (spiffe-helper → vault-fetcher, client-reg)│
│  - /shared/client-id.txt, /shared/client-secret.txt               │
│  - /shared/secrets/* (vault-fetcher → app)                        │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
           │                                    │
           ▼                                    ▼
   ┌──────────────┐                   ┌──────────────┐
   │   Vault      │                   │  Keycloak    │
   │ (JWT auth)   │                   │ (OAuth 2.0)  │
   └──────────────┘                   └──────────────┘
```

---

## Example Use Case: GitHub Agent

**Scenario:** An agent needs to:
1. **Call GitHub API** using a personal access token (stored in Vault)
2. **Call another Kagenti service** using dynamic token exchange

**Setup:**

1. **Store GitHub token in Vault:**
   ```bash
   vault kv put secret/github/agent-token token="ghp_xxxxxxxxxxxx"
   ```

2. **Configure Vault role for the agent's SPIFFE ID:**
   ```hcl
   # Role allows agent with specific SPIFFE ID to access GitHub token
   path "secret/data/github/agent-token" {
     capabilities = ["read"]
   }
   ```

3. **Deploy agent with labels:**
   ```yaml
   metadata:
     labels:
       kagenti.io/type: "agent"
       kagenti.io/inject: "true"
       kagenti.io/vault-fetcher-inject: "true"  # Enable Vault fetcher
   ```

4. **Create ConfigMap for vault-fetcher:**
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
         - path: "secret/data/github/agent-token"
           field: "token"
           output: "/shared/secrets/github-token"
   ```

**Runtime behavior:**

1. **Pod starts:**
   - `vault-fetcher` init container authenticates to Vault using JWT-SVID
   - Fetches GitHub token, writes to `/shared/secrets/github-token`
   - Exits

2. **Agent reads GitHub token:**
   ```python
   with open('/shared/secrets/github-token', 'r') as f:
       github_token = f.read().strip()
   
   # Call GitHub API with static token
   headers = {'Authorization': f'Bearer {github_token}'}
   response = requests.get('https://api.github.com/user', headers=headers)
   ```

3. **Agent calls internal service:**
   ```python
   # This call is intercepted by AuthBridge, token is dynamically exchanged
   response = requests.get('http://data-service.namespace.svc/query')
   # AuthBridge handles everything transparently
   ```

---

## Why Not Add Vault to the Unified Binary?

**Short answer:** Different concerns, different lifecycles.

**Long answer:**

### If we added Vault to `cmd/authbridge/`:
- ❌ Vault fetching happens **once at startup**, not on every HTTP request
- ❌ Secrets need to be available **before** the app container starts
- ❌ Adds complexity to the ext-proc request path (which needs to be fast)
- ❌ Mixes init-time concerns with runtime traffic handling

### Separate init container:
- ✅ **Single responsibility:** Fetch secrets at startup, write files, exit
- ✅ **Init container semantics:** Runs to completion before app starts
- ✅ **Simpler debugging:** Separate logs, clear failure mode
- ✅ **Reusable:** Can be used without AuthBridge (e.g., non-HTTP workloads)

### Could we support both?
Yes, in the future we could add a "Vault outbound mode" to the unified binary for **dynamic secret injection** (fetching a fresh secret on every HTTP request). But that's a different use case than what we're solving here.

**Phase 1 (now):** Init container for static credentials at startup
**Phase 2 (future, if needed):** Dynamic Vault injection in unified binary

---

## Summary

- **AuthBridge unified binary** = Dynamic token exchange for service-to-service calls
- **Vault fetcher init container** = One-time static credential retrieval at startup
- **Webhook** = Injects both components into workloads
- **SPIFFE/JWT auth** = How workloads authenticate to Vault (using JWT-SVID)
- **Different tools for different jobs**, orchestrated together by the webhook
