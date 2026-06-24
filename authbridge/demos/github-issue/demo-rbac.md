# GitHub Issue Agent Demo with AuthBridge (Manual Deployment)

This guide walks through deploying the **GitHub Issue Agent** with **AuthBridge**
using `kubectl` commands exclusively. All resources — agent, tool, ConfigMaps, and
secrets — are deployed via Kubernetes manifests.

For a UI-driven deployment using the Kagenti dashboard, see [demo-ui.md](demo-ui.md).
For a simpler getting-started demo, see the [Weather Agent demo](../weather-agent/demo-ui.md).

## What This Demo Shows

In this demo, we deploy the GitHub Issue Agent and GitHub MCP Tool with AuthBridge
providing end-to-end security:

1. **Agent identity** — The agent automatically registers with Keycloak using its
   SPIFFE ID, with no hardcoded secrets
2. **Inbound validation** — Requests to the agent are validated (JWT signature,
   issuer, and audience) before reaching the agent code
3. **Transparent token exchange** — When the agent calls the GitHub tool, AuthBridge
   automatically exchanges the user's token for one scoped to the tool
4. **Subject preservation** — The end user's identity (`sub` claim) is preserved
   through the exchange, enabling per-user authorization at the tool
5. **Scope-based access** — The tool uses token scopes to determine whether to
   grant public or privileged GitHub API access

## Architecture

```
┌──────────────────────────────────────────────────────────────────────────────────┐
│                              KUBERNETES CLUSTER                                  │
│                                                                                  │
│  ┌───────────────────────────────────────────────────────────────────────────┐   │
│  │                  GIT-ISSUE-AGENT POD (namespace: team1)                   │   │
│  │                                                                           │   │
│  │  ┌──────────────────┐  ┌───────────────────────────────────────────--─┐   │   │
│  │  │  git-issue-agent │  │  AuthBridge sidecar (combined image)         │   │   │
│  │  │  (A2A agent,     │  │  Container name depends on resolved mode:    │   │   │
│  │  │   port 8000)     │  │    proxy-sidecar (default): authbridge-proxy │   │   │
│  │  └──────────────────┘  │    envoy-sidecar:           envoy-proxy      │   │   │
│  │                        │                                              │   │   │
│  │                        │  Inbound:                                    │   │   │
│  │                        │    - Validates JWT (signature + issuer +     │   │   │
│  │                        │      audience via JWKS)                      │   │   │
│  │                        │    - Returns 401 for invalid/missing tokens  │   │   │
│  │                        │  Outbound:                                   │   │   │
│  │                        │    - HTTP: Exchanges token via Keycloak      │   │   │
│  │                        │      → aud: github-tool                      │   │   │
│  │                        │    - HTTPS: TLS passthrough                  │   │   │
│  │                        │                                              │   │   │
│  │                        │  spiffe-helper bundled inside the image      │   │   │
│  │                        │  (gated by SPIRE_ENABLED).                   │   │   │
│  │                        │  Keycloak client registration is             │   │   │
│  │                        │  operator-managed; the resulting Secret      │   │   │
│  │                        │  is mounted at /shared/client-{id,secret}.txt│   │   │
│  │                        └────────────────────────────────────────────--┘   │   │
│  └───────────────────────────────────────────────────────────────────────────┘   │
│                                      │                                           │
│                      Exchanged token │(aud: github-tool)                         │
│                                      ▼                                           │
│  ┌───────────────────────────────────────────────────────────────────────────┐   │
│  │                  GITHUB-TOOL POD (namespace: team1)                       │   │
│  │                                                                           │   │
│  │  ┌──────────────────────────────────────────────────────────────────┐     │   │
│  │  │                     github-tool (port 9090)                      │     │   │
│  │  │  - Validates token (aud: github-tool, issuer: Keycloak)          │     │   │
│  │  │  - Token has github-full-access scope? → PRIVILEGED_ACCESS_PAT   │     │   │
│  │  │  - Otherwise → PUBLIC_ACCESS_PAT                                 │     │   │
│  │  └──────────────────────────────────────────────────────────────────┘     │   │
│  └───────────────────────────────────────────────────────────────────────────┘   │
│                                                                                  │
├──────────────────────────────────────────────────────────────────────────────────┤
│                            EXTERNAL SERVICES                                     │
│                                                                                  │
│  ┌──────────────────────┐          ┌──────────────────────┐                      │
│  │   SPIRE (namespace:  │          │ KEYCLOAK (namespace: │                      │
│  │       spire)         │          │     keycloak)        │                      │
│  │                      │          │                      │                      │
│  │  Provides SPIFFE     │          │  - kagenti realm     │                      │
│  │  identities (SVIDs)  │          │  - token exchange    │                      │
│  └──────────────────────┘          └──────────────────────┘                      │
└──────────────────────────────────────────────────────────────────────────────────┘
```

## Token Flow

```
  User/Client          Agent Pod              Keycloak        GitHub Tool
    │                     │                      │                 │
    │  1. Get token       │                      │                 │
    │  (client_credentials│                      │                 │
    │   or user login)    │                      │                 │
    │───────────────────────────────────────────►│                 │
    │◄───────────────────────────────────────────│                 │
    │  Token: aud=Agent's SPIFFE ID              │                 │
    │                     │                      │                 │
    │  2. Send prompt     │                      │                 │
    │  + Bearer token     │                      │                 │
    │────────────────────►│                      │                 │
    │                     │                      │                 │
    │              ┌──────┴──────┐               │                 │
    │              │  AuthBridge │               │                 │
    │              │  INBOUND    │               │                 │
    │              │  validates: │               │                 │
    │              │  ✓ signature│               │                 │
    │              │  ✓ issuer   │               │                 │
    │              │  ✓ audience │               │                 │
    │              └──────┬──────┘               │                 │
    │                     │                      │                 │
    │              Agent processes prompt,       │                 │
    │              calls GitHub tool             │                 │
    │                     │                      │                 │
    │              ┌──────┴──────┐               │                 │
    │              │  AuthBridge │               │                 │
    │              │  OUTBOUND   │               │                 │
    │              │  intercepts │               │                 │
    │              └──────┬──────┘               │                 │
    │                     │                      │                 │
    │                     │  3. Token Exchange   │                 │
    │                     │  (RFC 8693)          │                 │
    │                     │─────────────────────►│                 │
    │                     │◄─────────────────────│                 │
    │                     │  New token:          │                 │
    │                     │  aud=github-tool     │                 │
    │                     │  sub=<original user> │                 │
    │                     │                      │                 │
    │                     │  4. Forward request  │                 │
    │                     │  with exchanged token│                 │
    │                     │───────────────────────────────────────►│
    │                     │                      │                 │
    │                     │                      │   Tool validates│
    │                     │                      │   token, checks │
    │                     │                      │   scopes, uses  │
    │                     │                      │   appropriate   │
    │                     │                      │   GitHub PAT    │
    │                     │                      │                 │
    │                     │◄───────────────────────────────────────│
    │◄────────────────────│  5. Response         │                 │
    │  GitHub issues      │                      │                 │
```

## Key Security Properties

| Property | How It's Achieved |
|----------|-------------------|
| **No hardcoded agent secrets** | Client credentials dynamically generated by client-registration using SPIFFE ID |
| **Identity-based auth** | SPIFFE ID is both the pod identity and the Keycloak client ID |
| **Inbound validation** | AuthBridge validates all incoming requests (JWT signature, issuer, audience) before they reach the agent |
| **Audience-scoped tokens** | Original token scoped to Agent; exchanged token scoped to GitHub tool |
| **User attribution** | `sub` and `preferred_username` preserved through token exchange |
| **Scope-based authorization** | Tool uses token scopes to determine access level (public vs. privileged) |
| **Transparent to agent code** | The agent makes plain HTTP calls; AuthBridge handles all token management |

### Inbound Verification (AuthProxy)

The AuthBridge sidecar includes an Envoy-based ext_proc that validates **every**
inbound request before it reaches the agent. The ext_proc (port 9090) performs
three checks on the `Authorization: Bearer <token>` header:

1. **Signature** — Verifies the JWT signature against Keycloak's JWKS keys
   (auto-refreshed via cache). Rejects tampered or forged tokens.
2. **Issuer** — Confirms the `iss` claim matches the expected Keycloak realm
   (`ISSUER` in `authbridge-config`). Rejects tokens from other identity providers.
3. **Audience** — Confirms the `aud` claim includes the agent's CLIENT_ID
   (from `/shared/client-id.txt`). Rejects tokens intended for a different agent.

Requests that fail any check receive an immediate `401 Unauthorized` response from
Envoy — the agent application never sees them. This is tested in
[Step 8a–8c](#step-8-test-the-authbridge-flow).

---

## Prerequisites

Ensure you have completed the Kagenti platform setup as described in the
[Installation Guide](https://github.com/kagenti/kagenti/blob/main/docs/install.md).

You should also have:
- The [kagenti-extensions](https://github.com/kagenti/kagenti-extensions) repo cloned
- The [agent-examples](https://github.com/kagenti/agent-examples) repo cloned
- Python 3.9+ with `venv` support
- **Ollama running** with the `ibm/granite4:latest` model (or another model of your choice)
- Two GitHub Personal Access Tokens (PATs):
  - `<PUBLIC_ACCESS_PAT>` — access to public repositories only
  - `<PRIVILEGED_ACCESS_PAT>` — access to all repositories

### Creating GitHub Personal Access Tokens

Follow [GitHub's instructions](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens#creating-a-fine-grained-personal-access-token)
to create fine-grained PAT tokens:

- **`<PUBLIC_ACCESS_PAT>`** — select **Public Repositories (read-only)** access
- **`<PRIVILEGED_ACCESS_PAT>`** — select **All Repositories** access

This lets you demonstrate finer-grained authorization: a user with full access
can see issues on all repositories, while a user with partial access can only
see issues on public repositories.

### Build and Load Container Images

<!-- WORKAROUND: Remove this section once container images are published to a public
     registry. Track: https://github.com/kagenti/agent-examples/issues — no issue filed yet. -->

The agent and tool container images must be built locally and loaded into the kind
cluster (they are not published to a public registry):

```bash
cd <path-to>/agent-examples

# Build the GitHub tool image
docker build -t ghcr.io/kagenti/agent-examples/github-tool:latest \
  -f mcp/github_tool/Dockerfile mcp/github_tool/

# Build the GitHub Issue Agent image
docker build -t ghcr.io/kagenti/agent-examples/git-issue-agent:latest \
  -f a2a/git_issue_agent/Dockerfile a2a/git_issue_agent/

# Load both images into the kind cluster
kind load docker-image --name kagenti ghcr.io/kagenti/agent-examples/github-tool:latest
kind load docker-image --name kagenti ghcr.io/kagenti/agent-examples/git-issue-agent:latest
```

---

## Step 1: Deploy the Operator Webhook with AuthBridge Support

The [kagenti-operator](https://github.com/kagenti/kagenti-operator) webhook automatically injects AuthBridge sidecars into agent deployments. See the operator docs for installation.

Once the webhook is deployed, create the namespace and apply the ConfigMaps:

```bash
kubectl create namespace team1
kubectl apply -f authbridge/demos/github-issue/k8s/configmaps-webhook.yaml -n team1
```

> **Note:** If you want to use a different namespace, set `AUTHBRIDGE_NAMESPACE=<your-namespace>` and update all subsequent commands accordingly.

---

## Step 2: Configure Keycloak

Keycloak needs to be configured with the correct clients, scopes, and users for the
token exchange flow between the agent and the GitHub tool.

### Port-forward Keycloak (if needed)

The setup script connects to Keycloak at `http://keycloak.localtest.me:8080`.
If Keycloak is not already reachable at that address (e.g., via an ingress),
start a port-forward in a separate terminal:

```bash
kubectl port-forward service/keycloak-service -n keycloak 8080:8080
```

### Run the setup script

```bash
cd authbridge

# Create virtual environment (if not already done)
uv sync
source venv/bin/activate
uv pip install --upgrade pip
uv pip install -r requirements.txt

cd demos/github-issue
# Run the Keycloak setup for this demo
python setup_keycloak.py -rbac rbac/config.yaml -policy rbac/policy.yaml
```


This creates:

| Resource | Name | Purpose |
|----------|------|---------|
| **Realm** | `kagenti` | Keycloak realm for the demo |
| **Client** | `spiffe://localtest.me/ns/team1/sa/git-issue-agent` | Agent client (already existed) with github-agent role |
| **Client** | `github-tool` | Target audience for token exchange |
| **Client Role** | `github-agent` | Access role for the GitHub issue agent |
| **Client Role** | `github-tool-aud` | Audience role for GitHub tool access |
| **Client Role** | `github-full-access` | Full access role for GitHub tool operations |
| **Scope** | `github-agent` | Client scope for agent access (DEFAULT for agent client) |
| **Scope** | `github-tool-aud` | Client scope for tool audience (OPTIONAL for agent) |
| **Scope** | `github-full-access` | Client scope for privileged access (OPTIONAL for agent) |
| **User** | `alice` (password: `alice123`) | User with 'developer' role - has privileged access  |
| **User** | `bob` (password: `bob123`) | User with 'tech-support' role - has public access |
| **User** | `charlie` (password: `charlie123`) | User with 'Sales' role - has no access udner regular policy or public access under permissive policy |
---

## Step 3: Apply Demo ConfigMaps

The Kagenti installer creates default ConfigMaps (`authbridge-config`,
`spiffe-helper-config`, `envoy-config`) and the `keycloak-admin-secret` Secret
in the target namespace with the correct `kagenti` realm settings and 300s Envoy
timeouts. No manual secret creation is needed for this demo.

> If your Keycloak admin credentials differ from the default (`admin`/`admin`),
> update the secret:
> ```bash
> kubectl create secret generic keycloak-admin-secret -n team1 \
>   --from-literal=KEYCLOAK_ADMIN_USERNAME=<your-admin-user> \
>   --from-literal=KEYCLOAK_ADMIN_PASSWORD=<your-admin-password> \
>   --dry-run=client -o yaml | kubectl apply -f -
> ```

Apply the demo-specific ConfigMaps — the `authproxy-routes` ConfigMap
configures per-route token exchange (target audience and scopes for the
`github-tool` host), and `authbridge-config` sets the agent's SPIFFE ID for
inbound audience validation. Apply this **before** deploying the agent.

```bash
cd authbridge

# Apply demo ConfigMaps (authbridge-config and authproxy-routes)
kubectl apply -f demos/github-issue/k8s/configmaps.yaml
```

> **Note:** If you're using a different namespace, edit
> `configmaps.yaml` and update the `namespace` field.

---

## Step 4: Create GitHub PAT Secret

The GitHub tool needs PAT tokens to access the GitHub API. Create a Kubernetes secret
with your tokens:

```bash
export PRIVILEGED_ACCESS_PAT=<your-privileged-pat>
export PUBLIC_ACCESS_PAT=<your-public-pat>
```

Provide your actual GitHub Personal Access Tokens.

```bash
kubectl create secret generic github-tool-secrets -n team1 \
  --from-literal=INIT_AUTH_HEADER="Bearer $PRIVILEGED_ACCESS_PAT" \
  --from-literal=UPSTREAM_HEADER_TO_USE_IF_IN_AUDIENCE="Bearer $PRIVILEGED_ACCESS_PAT" \
  --from-literal=UPSTREAM_HEADER_TO_USE_IF_NOT_IN_AUDIENCE="Bearer $PUBLIC_ACCESS_PAT"
```

---

## Step 5: Deploy the GitHub Tool

Deploy the GitHub MCP tool as a target service. This deployment does **not** get
AuthBridge injection (it is the target, not the caller):

```bash
kubectl apply -f demos/github-issue/k8s/github-tool-deployment.yaml
# Wait for the tool to be ready:
kubectl wait --for=condition=available --timeout=120s deployment/github-tool -n team1
```

---

## Step 6: Deploy the GitHub Issue Agent

Deploy the agent with AuthBridge labels. The webhook will automatically
inject one combined AuthBridge sidecar (post-#411). In envoy-sidecar mode
it also injects a `proxy-init` init container for iptables setup:

```bash
kubectl apply -f demos/github-issue/k8s/git-issue-agent-deployment.yaml
# Wait for the agent to be ready:
kubectl wait --for=condition=available --timeout=180s deployment/git-issue-agent -n team1
```

> **Note:** The agent may take longer to start because it waits on
> `/shared/client-{id,secret}.txt` to be populated by the operator's
> `ClientRegistrationReconciler` before the AuthBridge sidecar becomes
> ready.

### Verify injected containers

Confirm that the webhook injected the combined AuthBridge sidecar:

```bash
kubectl get pod -n team1 -l app.kubernetes.io/name=git-issue-agent \
  -o jsonpath='{.items[0].spec.containers[*].name}'
```

Expected (proxy-sidecar mode, the cluster default):

```txt
agent authbridge-proxy
```

Or, in envoy-sidecar mode:

```txt
agent envoy-proxy
```

---

## Step 7: Validate the Deployment

### Check pod status

```bash
kubectl get pods -n team1
```

Expected output:

```
NAME                               READY   STATUS    RESTARTS   AGE
git-issue-agent-58768bdb67-xxxxx   2/2     Running   0          2m
github-tool-7f8c9d6b44-yyyyy      1/1     Running   0          3m
```

### Check operator-managed client registration

After kagenti-extensions#411 / kagenti-operator#361, registration runs in
the kagenti-operator (outside the workload pod). Verify the resulting
Secret was mounted into the agent's sidecar:

```bash
kubectl get pod -n team1 -l app.kubernetes.io/name=git-issue-agent \
  -o jsonpath='{.items[0].spec.volumes[?(@.secret)].secret.secretName}'
# Expect a Secret name starting with: kagenti-keycloak-client-credentials-
```

Follow the operator-side registration:

```bash
kubectl logs -n kagenti-system deployment/kagenti-controller-manager \
  | grep -iE "clientregistration|git-issue-agent" | tail -20
```

Expected (operator log lines, exact format depends on the operator's
log format):

```
ClientRegistrationReconciler: ensured Keycloak client
  spiffe://localtest.me/ns/team1/sa/git-issue-agent
ClientRegistrationReconciler: wrote Secret
  kagenti-keycloak-client-credentials-<hex8>
```

### Check agent logs

```bash
kubectl logs deployment/git-issue-agent -n team1 -c agent
```

Expected:

```
SVID JWT file /opt/jwt_svid.token not found.
SVID JWT file /opt/jwt_svid.token not found.
CLIENT_SECRET file not found at /shared/secret.txt
INFO: JWKS_URI is set - using JWT Validation middleware
INFO:     Started server process [17]
INFO:     Waiting for application startup.
INFO:     Application startup complete.
INFO:     Uvicorn running on http://0.0.0.0:8000 (Press CTRL+C to quit)
```

<!-- WORKAROUND: Remove this warning note once kagenti/agent-examples#129 is fixed. -->

> **These warnings are expected and harmless.** The agent's built-in auth code
> probes for SVID and client-secret files at startup. With AuthBridge, these files
> are produced and consumed inside the AuthBridge sidecar (and the operator's
> ClientRegistrationReconciler), not by the agent container directly. The agent
> falls back to JWKS-based JWT validation (`JWKS_URI is set`), which is the
> correct behavior — AuthBridge handles inbound JWT validation and outbound
> token exchange on behalf of the agent.
> These warnings will be removed once the agent's built-in auth logic is cleaned up
> ([kagenti/agent-examples#129](https://github.com/kagenti/agent-examples/issues/129)).

### Verify Ollama is running

The agent uses LLM for inference. You can use Ollama, OpenAI, or anything else you want.

For this demo we used Ollama. You should see `ibm/granite4:latest` (or whichever model you configured) on the list.
If Ollama is not running, start it in a separate terminal (`ollama serve`) and ensure the
model is pulled (`ollama pull ibm/granite4:latest`).

```bash
ollama pull ibm/granite4:latest
ollama list
ollama serve
```

> **Tip:** If using a different model, update `TASK_MODEL_ID` in
> `git-issue-agent-deployment.yaml` before deploying.

> **Note:** The `git-issue-agent-deployment.yaml` file defaults to `LLM_API_BASE=http://host.docker.internal:11434` and `OLLAMA_API_BASE=http://host.docker.internal:11434`,
> which reaches Ollama running on your host machine via the Kind/Docker Desktop gateway.
> If you deploy Ollama inside the cluster instead, modify the `git-issue-agent-deployment.yaml` file directly or patch the agent:
> ```bash
> kubectl set env deployment/git-issue-agent -n team1 -c agent \
>   LLM_API_BASE="http://ollama.ollama.svc:11434" \
>   OLLAMA_API_BASE="http://ollama.ollama.svc:11434"
> ```

---

## Step 8: Test the AuthBridge Flow

These tests verify both **inbound** JWT validation and **outbound** token exchange
end-to-end.

### Setup

```bash
# Start a test client pod (sends requests from outside the agent pod)
kubectl run test-client --image=nicolaka/netshoot -n team1 --restart=Never -- sleep 3600
kubectl wait --for=condition=ready pod/test-client -n team1 --timeout=30s
```

### 8a. Agent Card - Public Endpoint (No Token Required)

The `/.well-known/agent.json` endpoint is publicly accessible — authbridge
[bypasses JWT validation](https://github.com/kagenti/kagenti-extensions/pull/133)
for `/.well-known/*`, `/healthz`, `/readyz`, and `/livez` by default:

```bash
kubectl exec test-client -n team1 -- curl -s \
  http://git-issue-agent:8080/.well-known/agent.json | jq .name
# Expected: "Github issue agent"
```

### 8b. Inbound Rejection - No Token

Non-public endpoints require a valid JWT:

```bash
kubectl exec test-client -n team1 -- curl -s \
  http://git-issue-agent:8080/
# Expected: {"error":"unauthorized","message":"missing Authorization header"}
```

### 8c. Inbound Rejection - Invalid Token

A malformed or tampered token fails the JWKS signature check:

```bash
kubectl exec test-client -n team1 -- curl -s \
  -H "Authorization: Bearer invalid-token" \
  http://git-issue-agent:8080/
# Expected: {"error":"unauthorized","message":"token validation failed: ..."}
```

### 8d. Inbound Rejection - Wrong Issuer

A properly signed token from a **different Keycloak realm** has a valid signature
(same Keycloak instance) but the wrong `iss` claim. AuthProxy rejects it because
the issuer does not match the configured `ISSUER` in `authbridge-config`:

```bash
# Get a valid token from the master realm (different issuer than "kagenti")
WRONG_ISSUER_TOKEN=$(kubectl exec test-client -n team1 -- curl -s \
  "http://keycloak-service.keycloak.svc:8080/realms/master/protocol/openid-connect/token" \
  -d "grant_type=password" \
  -d "client_id=admin-cli" \
  -d "username=admin" \
  -d "password=admin" | jq -r '.access_token')

kubectl exec test-client -n team1 -- curl -s \
  -H "Authorization: Bearer $WRONG_ISSUER_TOKEN" \
  http://git-issue-agent:8080/
# Expected: {"error":"unauthorized","message":"token validation failed: invalid issuer: expected http://keycloak.localtest.me:8080/realms/kagenti, got ..."}
```

> **Why this matters:** Even though the token is cryptographically valid (signed by
> the same Keycloak instance), AuthProxy's issuer check ensures only tokens from the
> correct realm are accepted. This prevents cross-realm token reuse attacks.

### 8e. Valid Token - Agent Card

```bash
# The AuthBridge sidecar's container name depends on the resolved mode:
#   proxy-sidecar (default): authbridge-proxy
#   envoy-sidecar:           envoy-proxy
SIDECAR=$(kubectl get pod -n team1 -l app.kubernetes.io/name=git-issue-agent \
  -o jsonpath='{.items[0].spec.containers[*].name}' | tr ' ' '\n' \
  | grep -E '^(authbridge-proxy|envoy-proxy)$' | head -1)

# Get the agent's client credentials (mounted by the operator-managed
# ClientRegistration controller via the kagenti-keycloak-client-credentials
# Secret, then mounted into the sidecar at /shared/).
CLIENT_ID=$(kubectl exec deployment/git-issue-agent -n team1 -c "$SIDECAR" -- cat /shared/client-id.txt)
CLIENT_SECRET=$(kubectl exec deployment/git-issue-agent -n team1 -c "$SIDECAR" -- cat /shared/client-secret.txt)
echo "Agent Client ID: $CLIENT_ID"

# Get a service account token (simulating what the UI would obtain)
TOKEN=$(kubectl exec test-client -n team1 -- curl -s -X POST \
  "http://keycloak-service.keycloak.svc:8080/realms/kagenti/protocol/openid-connect/token" \
  -d "grant_type=client_credentials" \
  -d "client_id=$CLIENT_ID" \
  -d "client_secret=$CLIENT_SECRET" | jq -r '.access_token')

kubectl exec test-client -n team1 -- curl -s \
  -H "Authorization: Bearer $TOKEN" \
  http://git-issue-agent:8080/.well-known/agent.json | jq
```

```json
# Expected: Agent card JSON response
{
  "capabilities": { "streaming": true },
  "defaultInputModes": ["text"],
  "defaultOutputModes": ["text"],
  "description": "Answer queries about Github issues",
  "name": "Github issue agent",
  "preferredTransport": "JSONRPC",
  "protocolVersion": "0.3.0",
  ...
}
```

### 8f. Check Inbound Validation Logs

Verify that AuthProxy validated (and rejected) the inbound requests from steps 8b–8e.

> **Tip:** The combined AuthBridge sidecar runs the inbound JWT validation
> plugin. Inbound validation messages include the `[Inbound]` marker —
> filter by `"[Inbound]"` to see only inbound output.
>
> The container name depends on the resolved mode (`authbridge-proxy` for
> proxy-sidecar, `envoy-proxy` for envoy-sidecar). The `$SIDECAR` shell
> variable from §8e auto-detects it.

```bash
kubectl logs deployment/git-issue-agent -n team1 -c "$SIDECAR" 2>&1 | grep "\[Inbound\]"
```

Expected (one line per request in 8b–8e):

```
[Inbound] Missing Authorization header
[Inbound] JWT validation failed: failed to parse/validate token: ...
[Inbound] JWT validation failed: invalid issuer: expected http://keycloak.localtest.me:8080/realms/kagenti, got ...
[Inbound] Token validated - issuer: http://keycloak.localtest.me:8080/realms/kagenti, audience: [...]
[Inbound] JWT validation succeeded, forwarding request
```

> **Note:** Outbound token exchange logs (`[Token Exchange] ...`) will only appear
> after [Step 9](#step-9-end-to-end--query-github-issues), when the agent calls the
> GitHub tool.

---

## Step 9: End-to-End — Query GitHub Issues

This is the full demo flow — the request goes through inbound validation, reaches
the agent, the agent calls the GitHub tool (token exchange happens transparently),
and returns the result.

> **Prerequisite:** This step uses the `test-client` pod created in
> [Step 8 Setup](#setup). If you already deleted it, re-create it first.

> **Note:** JWT tokens passed via `kubectl exec -- curl -H "Authorization: Bearer $TOKEN"`
> can get mangled by double shell expansion. To avoid this, we exec into the test-client
> pod and run all commands from inside it.

### 9a. Open a shell inside the test-client pod

```bash
kubectl exec -it test-client -n team1 -- sh
```

### 9b. Get credentials and a token

Inside the test-client pod, run:

```bash
# Get a Keycloak admin token from the kagenti realm
ADMIN_TOKEN=$(curl -s http://keycloak-service.keycloak.svc:8080/realms/kagenti/protocol/openid-connect/token \
  -d "grant_type=password" \
  -d "client_id=admin-cli" \
  -d "username=admin" \
  -d "password=admin" | jq -r ".access_token")

echo "Admin token length: ${#ADMIN_TOKEN}"
# Expected: Admin token length: 782
# If 0 or 4 (null), Keycloak is not reachable or credentials are wrong — stop here.

# Look up the agent's client in the kagenti realm.
# The client ID is the SPIFFE ID (URL-encoded in the query parameter).
SPIFFE_ID="spiffe://localtest.me/ns/team1/sa/git-issue-agent"
CLIENTS=$(curl -s -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://keycloak-service.keycloak.svc:8080/admin/realms/kagenti/clients" \
  --data-urlencode "clientId=$SPIFFE_ID" --get)
INTERNAL_ID=$(echo "$CLIENTS" | jq -r ".[0].id")
CLIENT_ID=$(echo "$CLIENTS" | jq -r ".[0].clientId")

echo "Internal ID:   $INTERNAL_ID"
echo "Client ID:     $CLIENT_ID"
# If you see "null", the client was not found — check setup_keycloak.py ran.

# Get the client secret (extract directly from the client listing;
# the Keycloak /client-secret endpoint returns null for auto-registered clients)
CLIENT_SECRET=$(echo "$CLIENTS" | jq -r ".[0].secret")

echo "Secret length: ${#CLIENT_SECRET}"

# Get an OAuth token for the agent
TOKEN=$(curl -s -X POST \
  "http://keycloak-service.keycloak.svc:8080/realms/kagenti/protocol/openid-connect/token" \
  -d "grant_type=client_credentials" \
  --data-urlencode "client_id=$CLIENT_ID" \
  --data-urlencode "client_secret=$CLIENT_SECRET" | jq -r ".access_token")

echo "Token length:  ${#TOKEN}"
```

You should see output like:

```
Admin token length: 1543
Internal ID:   8577145b-cb77-4bbc-abde-1f5eb7643344
Client ID:     spiffe://localtest.me/ns/team1/sa/git-issue-agent
Secret length: 32
Token length:  1165
```

> **Troubleshooting:** If `INTERNAL_ID` shows `null`, the Keycloak query didn't find
> the client. Verify `$ADMIN_TOKEN` is not empty (Keycloak reachable?) and that
> `setup_keycloak.py` was run. You can also list all clients with:
> `curl -s -H "Authorization: Bearer $ADMIN_TOKEN" "http://keycloak-service.keycloak.svc:8080/admin/realms/kagenti/clients" | jq '.[].clientId'`

### 9c. Send a prompt to the agent

Still inside the test-client pod, send the A2A v0.3.0 request:

> **Note:** This request may take 30-60 seconds as the agent calls the LLM and the
> GitHub tool. The Envoy route timeout is set to 300 seconds (5 minutes) to
> accommodate slow LLM inference. If you still see `upstream request timeout`,
> check **Retrieving async results** below.
>
> **Important:** Ollama must be running and the model must be loaded before sending
> this request. If you see `OllamaException - upstream connect error`, ensure
> `ollama serve` is running and the model is pulled (see
> [Step 7: Verify Ollama](#verify-ollama-is-running)).

```bash
curl -s --max-time 300 \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -X POST http://git-issue-agent:8080/ \
  -d '{
    "jsonrpc": "2.0",
    "id": "test-1",
    "method": "message/send",
    "params": {
      "message": {
        "role": "user",
        "messageId": "msg-001",
        "parts": [{"type": "text", "text": "List issues in kagenti/kagenti repo"}]
      }
    }
  }' | jq
```

### 9d. Exit the test-client pod

```bash
exit
```

### Retrieving async results

If the request timed out but the agent completed the task in the background,
check the agent logs for the task ID:

```bash
kubectl logs deployment/git-issue-agent -n team1 -c agent --tail=50
# Look for: Task <TASK_ID> saved successfully / TaskState.completed
```

Then exec back into the test-client pod and retrieve the result:

```bash
kubectl exec -it test-client -n team1 -- sh

# (re-run the token setup from Step 9b above, then:)
curl -s --max-time 10 \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -X POST http://git-issue-agent:8080/ \
  -d '{
    "jsonrpc": "2.0",
    "id": "get-1",
    "method": "tasks/get",
    "params": {
      "id": "<TASK_ID>"
    }
  }' | jq '.result.artifacts[0].parts[0].text'
```

### 9e. Verify AuthProxy Logs (Inbound + Outbound)

Check the AuthBridge sidecar logs to confirm both inbound validation and
outbound token exchange are working. The combined sidecar handles both
directions; the container name depends on the resolved mode
(`authbridge-proxy` for proxy-sidecar, `envoy-proxy` for envoy-sidecar).

**Inbound validation logs** (JWT signature, issuer, audience checks):

```bash
kubectl logs deployment/git-issue-agent -n team1 -c "$SIDECAR" 2>&1 | grep -i "inbound"
```

Expected output:

```
[Inbound] Token validated - issuer: http://keycloak.localtest.me:8080/realms/kagenti, audience: [spiffe://localtest.me/ns/team1/sa/git-issue-agent ...]
[Inbound] JWT validation succeeded, forwarding request
```

If you ran the rejection tests (8b, 8c, 8d), you should also see:

```
[Inbound] Missing Authorization header
[Inbound] JWT validation failed: failed to parse/validate token: ...
[Inbound] JWT validation failed: invalid issuer: expected http://keycloak.localtest.me:8080/realms/kagenti, got ...
```

**Outbound token exchange logs** (RFC 8693 token exchange for the GitHub tool):

```bash
kubectl logs deployment/git-issue-agent -n team1 -c "$SIDECAR" 2>&1 | grep "^2026/" | grep "\[Token Exchange\]"
```

Expected:

```
[Token Exchange] Token URL: http://keycloak-service.keycloak.svc:8080/realms/kagenti/protocol/openid-connect/token
[Token Exchange] Client ID: spiffe://localtest.me/ns/team1/sa/git-issue-agent
[Token Exchange] Audience: github-tool
[Token Exchange] Scopes: openid github-tool-aud github-full-access
[Token Exchange] Successfully exchanged token
[Token Exchange] Successfully exchanged token, replacing Authorization header
```

### Clean Up Test Client

```bash
kubectl delete pod test-client -n team1 --ignore-not-found
```

---

## Step 10: Access Control — Alice vs Bob
This step demonstrates **scope-based access control**: two users with different
privilege levels get different GitHub API access through the same agent.

| User | Token Scope | Tool PAT Used | Public Repos | Private Repos |
|------|-------------|---------------|:------------:|:-------------:|
| **Alice** | `openid github-full-access` | `PRIVILEGED_ACCESS_PAT` | Yes | Yes |
| **Bob** | `openid` (no `github-full-access`) | `PUBLIC_ACCESS_PAT` | Yes | No |

The flow:
1. User authenticates with Keycloak using `password` grant
2. Request is sent to Agent (on behalf of User).
3. Agent invokes Tool to perform its github task
4. AuthBridge exchanges the token. Alice's (developer) token will include `github-full-access`, Bob's (tech-support) will not.
5. The GitHub tool checks for `REQUIRED_SCOPE` (`github-full-access`) in the exchanged token
6. Tokens with the scope get the privileged PAT; tokens without get the public-only PAT

> **Prerequisite:** You need a **private** GitHub repository that the `PRIVILEGED_ACCESS_PAT`
> can access but the `PUBLIC_ACCESS_PAT` cannot. Replace `<your-org/your-private-repo>`
> below with your own private repo.

### 10a. Open a shell inside the test-client pod

```bash
kubectl run test-client --image=nicolaka/netshoot -n team1 --restart=Never -- sleep 3600 2>/dev/null
kubectl wait --for=condition=ready pod/test-client -n team1 --timeout=30s
kubectl exec -it test-client -n team1 -- sh
```

### 10b. Get agent credentials

Inside the test-client pod, get the agent's client credentials (needed to request
user tokens that include the agent's audience):

```bash
ADMIN_TOKEN=$(curl -s http://keycloak-service.keycloak.svc:8080/realms/kagenti/protocol/openid-connect/token \
  -d "grant_type=password" \
  -d "client_id=admin-cli" \
  -d "username=admin" \
  -d "password=admin" | jq -r ".access_token")

SPIFFE_ID="spiffe://localtest.me/ns/team1/sa/git-issue-agent"
CLIENTS=$(curl -s -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://keycloak-service.keycloak.svc:8080/admin/realms/kagenti/clients" \
  --data-urlencode "clientId=$SPIFFE_ID" --get)
INTERNAL_ID=$(echo "$CLIENTS" | jq -r ".[0].id")
CLIENT_ID=$(echo "$CLIENTS" | jq -r ".[0].clientId")
CLIENT_SECRET=$(echo "$CLIENTS" | jq -r ".[0].secret")
echo "Client ID: $CLIENT_ID  Secret length: ${#CLIENT_SECRET}"
```

### 10c. Test as Alice (public access only)

Alice authenticates with Keycloak using `password` grant.

```bash
ALICE_TOKEN=$(curl -s -X POST \
  "http://keycloak-service.keycloak.svc:8080/realms/kagenti/protocol/openid-connect/token" \
  -d "grant_type=password" \
  -d "username=alice" \
  -d "password=alice123" \
  --data-urlencode "client_id=$CLIENT_ID" \
  --data-urlencode "client_secret=$CLIENT_SECRET" | jq -r ".access_token")

echo "Alice token length: ${#ALICE_TOKEN}"
echo "Alice scopes: $(echo $ALICE_TOKEN | cut -d. -f2 | base64 -d 2>/dev/null | jq -r '.scope')"
```

**Alice queries a public repo** (should succeed):

```bash
curl -s --max-time 300 \
  -H "Authorization: Bearer $ALICE_TOKEN" \
  -H "Content-Type: application/json" \
  -X POST http://git-issue-agent:8080/ \
  -d '{
    "jsonrpc": "2.0",
    "id": "alice-public",
    "method": "message/send",
    "params": {
      "message": {
        "role": "user",
        "messageId": "msg-alice-1",
        "parts": [{"type": "text", "text": "List issues in kagenti/kagenti repo"}]
      }
    }
  }' | jq '.result.artifacts[0].parts[0].text' | head -5
```

**Alice queries a private repo** (should succeed — PRIVILEGED_ACCESS_PAT has access):

```bash
curl -s --max-time 300 \
  -H "Authorization: Bearer $ALICE_TOKEN" \
  -H "Content-Type: application/json" \
  -X POST http://git-issue-agent:8080/ \
  -d '{
    "jsonrpc": "2.0",
    "id": "alice-private",
    "method": "message/send",
    "params": {
      "message": {
        "role": "user",
        "messageId": "msg-alice-2",
        "parts": [{"type": "text", "text": "List issues in <your-org/your-private-repo>"}]
      }
    }
  }' | jq '.result.artifacts[0].parts[0].text' | head -5
```

> **Expected:** Alice's request for the private repo succeeds because the GitHub tool
> uses `PRIVILEGED_ACCESS_PAT`, which has access to private repositories.

### 10d. Test as Bob

Bob authenticates with Keycloak using `password` grant.

```bash
BOB_TOKEN=$(curl -s -X POST \
  "http://keycloak-service.keycloak.svc:8080/realms/kagenti/protocol/openid-connect/token" \
  -d "grant_type=password" \
  -d "username=bob" \
  -d "password=bob123" \
  --data-urlencode "client_id=$CLIENT_ID" \
  --data-urlencode "client_secret=$CLIENT_SECRET" | jq -r ".access_token")

echo "Bob token length: ${#BOB_TOKEN}"
echo "Bob scopes: $(echo $BOB_TOKEN | cut -d. -f2 | base64 -d 2>/dev/null | jq -r '.scope')"
```

**Bob queries the same private repo** (should fail — PUBLIC_ACCESS_PAT cannot access it):

```bash
curl -s --max-time 300 \
  -H "Authorization: Bearer $BOB_TOKEN" \
  -H "Content-Type: application/json" \
  -X POST http://git-issue-agent:8080/ \
  -d '{
    "jsonrpc": "2.0",
    "id": "bob-private",
    "method": "message/send",
    "params": {
      "message": {
        "role": "user",
        "messageId": "msg-bob-1",
        "parts": [{"type": "text", "text": "List issues in <your-org/your-private-repo>"}]
      }
    }
  }' | jq '.result.artifacts[0].parts[0].text' | head -5
```

> **Expected:** Bob's request fails because the exchanged token dosn't contain
> `github-full-access`, so the GitHub tool uses `PUBLIC_ACCESS_PAT`.

### 10e. Verify scope-based PAT selection in tool logs

Check the GitHub tool logs to confirm that different PATs were selected based on scopes:

```bash
exit
kubectl logs deployment/github-tool -n team1 | grep -E "REQUIRED_SCOPE|scopes"
```

Expected output (Bob doesnt have required scope):

```
"The REQUIRED_SCOPE NOT IN scopes" requiredScope=github-full-access scopes="[agent-team1-git-issue-agent-aud profile github-tool-aud email openid]"
```

### 10f. Clean up

```bash
kubectl delete pod test-client -n team1 --ignore-not-found
```

---

## How AuthBridge Changes the Original Demo

| Aspect | Original Demo | With AuthBridge |
|--------|--------------|-----------------|
| **Agent secrets** | Manual PAT token configuration | Dynamic credentials via SPIFFE + client-registration |
| **Inbound auth** | No validation | AuthBridge validates JWT (signature, issuer, audience) via ext_proc |
| **Token management** | Agent code handles tokens | Transparent sidecar — agent code unchanged |
| **Token for tool** | Same PAT token passed through | OAuth token exchange (RFC 8693) |
| **User attribution** | No user tracking | `sub` claim preserved through exchange |
| **Access control** | Single PAT for all users | Scope-based: public vs. privileged |

---

## Troubleshooting

### Invalid Client or Invalid Client Credentials

**Symptom:** `{"error":"invalid_client","error_description":"Invalid client or Invalid client credentials"}`

**Cause:** The `keycloak-admin-secret` Secret or `authbridge-config` ConfigMap was missing
or incorrect at startup, so the client-registration sidecar couldn't register the client.

**Fix:**

```bash
# 1. Verify the keycloak-admin-secret exists
kubectl get secret keycloak-admin-secret -n team1

# 2. Verify the authbridge-config ConfigMap has the correct realm
kubectl get configmap authbridge-config -n team1 -o jsonpath='{.data.KEYCLOAK_REALM}'
# Should show: kagenti

# 3. Re-apply the demo ConfigMap and restart
kubectl apply -f demos/github-issue/k8s/configmaps.yaml
kubectl rollout restart deployment/git-issue-agent -n team1
```

### Client Registration Can't Reach Keycloak

**Symptom:** `Connection refused` when connecting to Keycloak

**Fix:** Ensure the proxy-init container excludes the Keycloak port from iptables redirect.
Check that `OUTBOUND_PORTS_EXCLUDE: "8080"` is set in the proxy-init env vars.

### Token Exchange Fails with "Audience not found"

**Symptom:** `{"error":"invalid_client","error_description":"Audience not found"}`

**Fix:** The `github-tool` client must exist in Keycloak. Run `setup_keycloak.py`.

### Token Exchange Fails with "Client is not within the token audience"

**Symptom:** Token exchange returns `access_denied`

**Fix:** The `agent-team1-git-issue-agent-aud` scope must be a realm default.
Run `setup_keycloak.py` to set it up.

### Agent Pod Not Starting

**Symptom:** Pod shows 1/2 (or 0/2) containers ready.

**Fix:** Check the agent and the AuthBridge sidecar; if the issue is
operator-managed registration not finishing, the workload pod waits on
`/shared/client-{id,secret}.txt`.

```bash
# AuthBridge sidecar — name depends on resolved mode:
#   proxy-sidecar (default): authbridge-proxy
#   envoy-sidecar:           envoy-proxy
kubectl logs deployment/git-issue-agent -n team1 -c authbridge-proxy
kubectl logs deployment/git-issue-agent -n team1 -c agent

# Operator-managed registration:
kubectl logs -n kagenti-system deployment/kagenti-controller-manager \
  | grep -iE "clientregistration|git-issue-agent" | tail -20
```

### GitHub Tool Returns 401

**Symptom:** Tool rejects the exchanged token

**Fix:** Verify the tool's environment variables match the Keycloak configuration:
- `ISSUER` should be `http://keycloak.localtest.me:8080/realms/kagenti`
- `AUDIENCE` should be `github-tool`

### Upstream Request Timeout

**Symptom:** `upstream request timeout` from Envoy

**Cause:** The LLM inference takes longer than the Envoy route timeout.

**Fix:** The installer's `envoy-config` ConfigMap sets route and ext_proc
timeouts to 300 seconds (5 min). If you still hit timeouts, verify the
ConfigMap has the correct values:

```bash
kubectl get configmap envoy-config -n team1 -o jsonpath='{.data.envoy\.yaml}' | grep "timeout:"
```

If you see `30s` values instead of `300s`, reinstall Kagenti (the installer
creates the correct defaults) and restart the agent:

```bash
kubectl rollout restart deployment/git-issue-agent -n team1
```

---

## Cleanup

### Delete Deployments

```bash
kubectl delete -f demos/github-issue/k8s/git-issue-agent-deployment.yaml
kubectl delete -f demos/github-issue/k8s/github-tool-deployment.yaml
kubectl delete secret github-tool-secrets -n team1
kubectl delete pod test-client -n team1 --ignore-not-found
```

### Delete ConfigMaps

```bash
kubectl delete -f demos/github-issue/k8s/configmaps.yaml
```

### Delete Namespace (removes everything)

```bash
kubectl delete namespace team1
```

### Remove Webhook (optional)

```bash
kubectl delete mutatingwebhookconfiguration kagenti-webhook-authbridge-mutating-webhook-configuration
```

---

## Files Reference

| File | Description |
|------|-------------|
| `demos/github-issue/demo-rbac.md` | This guide (variant or demo-manual.md) |
| `demos/github-issue/demo-manual.md` | This guide's baseline |
| `demos/github-issue/demo-ui.md` | UI-driven deployment guide |
| `demos/github-issue/setup_keycloak.py` | Keycloak configuration script |
| `demos/github-issue/k8s/configmaps.yaml` | Demo-specific authbridge-config override |
| `demos/github-issue/k8s/git-issue-agent-deployment.yaml` | Agent deployment with AuthBridge labels |
| `demos/github-issue/k8s/github-tool-deployment.yaml` | GitHub tool deployment (no injection) |
