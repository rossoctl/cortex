# GitHub Issue Agent Demo with AuthBridge (Manual Deployment)

This guide walks through deploying the **GitHub Issue Agent** with **AuthBridge**
using `kubectl` commands exclusively. All resources — agent, tool, ConfigMaps, and
secrets — are deployed via Kubernetes manifests.

For a UI-driven deployment using the Kagenti dashboard, see [demo-ui.md](demo-ui.md).

This demo extends the [upstream GitHub Issue Agent demo](https://github.com/kagenti/kagenti/blob/main/docs/demos/demo-github-issue.md)
by replacing manual token handling with AuthBridge's automatic token exchange.

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
│  │  ┌──────────────────┐  ┌─────────────┐  ┌──────────────────────────────┐  │   │
│  │  │  git-issue-agent │  │   spiffe-   │  │      client-registration     │  │   │
│  │  │  (A2A agent,     │  │   helper    │  │  (registers with Keycloak    │  │   │
│  │  │   port 8000)     │  │             │  │   using SPIFFE ID)           │  │   │
│  │  └──────────────────┘  └─────────────┘  └──────────────────────────────┘  │   │
│  │                                                                           │   │
│  │  ┌───────────────────────────────────────────────────────────────────┐    │   │
│  │  │                AuthProxy Sidecar (envoy-proxy container)          │    │   │
│  │  │  Envoy + ext_proc (go-processor)                                  │    │   │
│  │  │  Inbound (port 15124):                                            │    │   │
│  │  │    - Validates JWT (signature + issuer + audience via JWKS)       │    │   │
│  │  │    - Returns 401 Unauthorized for invalid/missing tokens          │    │   │
│  │  │  Outbound (port 15123):                                           │    │   │
│  │  │    - HTTP: Exchanges token via Keycloak → aud: github-tool        │    │   │
│  │  │    - HTTPS: TLS passthrough (no interception)                     │    │   │
│  │  └───────────────────────────────────────────────────────────────────┘    │   │
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
| **Inbound validation** | [AuthProxy](../../AuthProxy/README.md) validates all incoming requests (JWT signature, issuer, audience) before they reach the agent |
| **Audience-scoped tokens** | Original token scoped to Agent; exchanged token scoped to GitHub tool |
| **User attribution** | `sub` and `preferred_username` preserved through token exchange |
| **Scope-based authorization** | Tool uses token scopes to determine access level (public vs. privileged) |
| **Transparent to agent code** | The agent makes plain HTTP calls; AuthBridge handles all token management |

### Inbound Verification (AuthProxy)

The AuthBridge sidecar includes [AuthProxy](../../AuthProxy/README.md), an Envoy-based
ext_proc that validates **every** inbound request before it reaches the agent. The
ext_proc (port 9090) performs three checks on the `Authorization: Bearer <token>` header:

1. **Signature** — Verifies the JWT signature against Keycloak's JWKS keys
   (auto-refreshed via cache). Rejects tampered or forged tokens.
2. **Issuer** — Confirms the `iss` claim matches the expected Keycloak realm
   (`ISSUER` in `authbridge-config`). Rejects tokens from other identity providers.
3. **Audience** — If `EXPECTED_AUDIENCE` is set, confirms the `aud` claim includes
   the agent's SPIFFE ID. Rejects tokens intended for a different service.

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

See the [upstream demo](https://github.com/kagenti/kagenti/blob/main/docs/demos/demo-github-issue.md#required-github-pat-tokens) for instructions on creating GitHub PAT tokens.

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

## Step 1: Deploy the Webhook with AuthBridge Support

The kagenti-webhook automatically injects AuthBridge sidecars into agent deployments.
Deploy it with the AuthBridge demo flag:

```bash
cd kagenti-webhook

# Deploy webhook + create namespace
# AUTHBRIDGE_K8S_DIR points at this demo's k8s manifests (default is single-target)
CLUSTER=kagenti-dev AUTHBRIDGE_DEMO=true AUTHBRIDGE_NAMESPACE=team1 \
  AUTHBRIDGE_K8S_DIR=AuthBridge/demos/github-issue/k8s \
  ./scripts/webhook-rollout.sh
```

This automatically:
1. Builds and deploys the kagenti-webhook
2. Creates the `team1` namespace with the `kagenti-enabled=true` label
3. Applies any `configmaps-webhook.yaml` found in the specified k8s directory

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
cd AuthBridge

# Create virtual environment (if not already done)
python -m venv venv
source venv/bin/activate
pip install --upgrade pip
pip install -r requirements.txt

# Run the Keycloak setup for this demo
python demos/github-issue/setup_keycloak.py
```

Or with a custom namespace/service account:

```bash
python demos/github-issue/setup_keycloak.py --namespace myns --service-account mysa
```

This creates:

| Resource | Name | Purpose |
|----------|------|---------|
| **Realm** | `kagenti` | Keycloak realm for the demo |
| **Client** | `github-tool` | Target audience for token exchange |
| **Scope** | `agent-team1-git-issue-agent-aud` | Realm DEFAULT — auto-adds Agent's SPIFFE ID to all tokens |
| **Scope** | `github-tool-aud` | Realm OPTIONAL — for exchanged tokens targeting the tool |
| **Scope** | `github-full-access` | Realm OPTIONAL — for privileged GitHub API access |
| **User** | `alice` (password: `alice123`) | Regular user — public access |
| **User** | `bob` (password: `bob123`) | Privileged user — full access |

---

## Step 3: Apply Demo ConfigMaps

The Kagenti installer creates default ConfigMaps (`environments`,
`spiffe-helper-config`, `envoy-config`, `authbridge-config`) with the correct
`kagenti` realm settings and 300s Envoy timeouts. This step overrides only
`authbridge-config` with demo-specific values — the token exchange target
audience (`github-tool`), scopes, and the agent's SPIFFE ID for inbound
audience validation. Apply this **before** deploying the agent.

```bash
cd AuthBridge

# Override authbridge-config for this demo (sets TARGET_AUDIENCE=github-tool)
kubectl apply -f demos/github-issue/k8s/configmaps.yaml
```

Verify the demo ConfigMap was applied:

```bash
kubectl get configmap authbridge-config -n team1 -o jsonpath='{.data.TARGET_AUDIENCE}'
# Expected: github-tool
```

> **Note:** If you're using a different namespace or service account, edit
> `configmaps.yaml` and update the `namespace` and `EXPECTED_AUDIENCE` fields.

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

Deploy the agent with AuthBridge labels. The kagenti-webhook will automatically inject
the AuthBridge sidecars (proxy-init, spiffe-helper, client-registration, envoy-proxy):

```bash
kubectl apply -f demos/github-issue/k8s/git-issue-agent-deployment.yaml
# Wait for the agent to be ready:
kubectl wait --for=condition=available --timeout=180s deployment/git-issue-agent -n team1
```

> **Note:** The agent may take longer to start because it waits for SPIFFE credentials
> and Keycloak client registration to complete before becoming ready.

### Verify injected containers

Confirm that the webhook injected the AuthBridge sidecars:

```bash
kubectl get pod -n team1 -l app.kubernetes.io/name=git-issue-agent -o jsonpath='{.items[0].spec.containers[*].name}'
```

Expected output (with SPIFFE):

```txt
agent spiffe-helper kagenti-client-registration envoy-proxy
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
git-issue-agent-58768bdb67-xxxxx   4/4     Running   0          2m
github-tool-7f8c9d6b44-yyyyy      1/1     Running   0          3m
```

### Check client registration

```bash
kubectl logs deployment/git-issue-agent -n team1 -c kagenti-client-registration
```

Expected:

```
SPIFFE credentials ready!
Client ID (SPIFFE ID): spiffe://localtest.me/ns/team1/sa/git-issue-agent
Created Keycloak client "spiffe://localtest.me/ns/team1/sa/git-issue-agent"
Client registration complete!
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
> are used by the sidecars (spiffe-helper, client-registration, Envoy), not by the
> agent container directly. The agent falls back to JWKS-based JWT validation
> (`JWKS_URI is set`), which is the correct behavior — AuthBridge's Envoy sidecar
> handles inbound JWT validation and outbound token exchange on behalf of the agent.
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

The `/.well-known/agent.json` endpoint is publicly accessible — AuthBridge's
go-processor [bypasses JWT validation](https://github.com/kagenti/kagenti-extensions/pull/133)
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
> the same Keycloak instance), AuthProxy's validation ensures only tokens from the
> correct realm are accepted. This prevents cross-realm token reuse attacks.

### 8e. Valid Token - Agent Card

```bash
# Get the agent's client credentials
CLIENT_ID=$(kubectl exec deployment/git-issue-agent -n team1 -c envoy-proxy -- cat /shared/client-id.txt)
CLIENT_SECRET=$(kubectl exec deployment/git-issue-agent -n team1 -c envoy-proxy -- cat /shared/client-secret.txt)
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

> **Tip:** The `envoy-proxy` container runs both Envoy and the go-processor (ext_proc).
> Inbound validation messages from the go-processor include the `[Inbound]` marker.
> Filter by `"[Inbound]"` to see only inbound validation output.

```bash
kubectl logs deployment/git-issue-agent -n team1 -c envoy-proxy 2>&1 | grep "\[Inbound\]"
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

Check the ext_proc logs to confirm both inbound validation and outbound token
exchange are working. The `envoy-proxy` container runs the AuthProxy ext_proc that
handles both directions.

**Inbound validation logs** (JWT signature, issuer, audience checks):

```bash
kubectl logs deployment/git-issue-agent -n team1 -c envoy-proxy 2>&1 | grep -i "inbound"
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
kubectl logs deployment/git-issue-agent -n team1 -c envoy-proxy 2>&1 | grep "^2026/" | grep "\[Token Exchange\]"
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

## How AuthBridge Changes the Original Demo

| Aspect | Original Demo | With AuthBridge |
|--------|--------------|-----------------|
| **Agent secrets** | Manual PAT token configuration | Dynamic credentials via SPIFFE + client-registration |
| **Inbound auth** | No validation | [AuthProxy](../../AuthProxy/README.md) validates JWT (signature, issuer, audience) via ext_proc |
| **Token management** | Agent code handles tokens | Transparent sidecar — agent code unchanged |
| **Token for tool** | Same PAT token passed through | OAuth token exchange (RFC 8693) |
| **User attribution** | No user tracking | `sub` claim preserved through exchange |
| **Access control** | Single PAT for all users | Scope-based: public vs. privileged |

---

## Troubleshooting

### Invalid Client or Invalid Client Credentials

**Symptom:** `{"error":"invalid_client","error_description":"Invalid client or Invalid client credentials"}`

**Cause:** The agent pod's `environments` ConfigMap was missing or incorrect at startup,
so the client-registration sidecar registered the client with wrong settings.

**Fix:**

```bash
# 1. Verify the installer's environments ConfigMap has the correct realm
kubectl get configmap environments -n team1 -o jsonpath='{.data.KEYCLOAK_REALM}'
# Should show: kagenti

# 2. Re-apply the demo ConfigMap and restart
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

### Agent Pod Not Starting (4/4 containers)

**Symptom:** Pod shows 3/4 or less containers ready

**Fix:** Check each container's logs:

```bash
kubectl logs deployment/git-issue-agent -n team1 -c kagenti-client-registration
kubectl logs deployment/git-issue-agent -n team1 -c spiffe-helper
kubectl logs deployment/git-issue-agent -n team1 -c envoy-proxy
kubectl logs deployment/git-issue-agent -n team1 -c agent
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
kubectl delete -f demos/github-issue/k8s/configmaps-webhook.yaml
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
| `demos/github-issue/demo-manual.md` | This guide |
| `demos/github-issue/demo-ui.md` | UI-driven deployment guide |
| `demos/github-issue/setup_keycloak.py` | Keycloak configuration script |
| `demos/github-issue/k8s/configmaps.yaml` | Demo-specific authbridge-config override |
| `demos/github-issue/k8s/git-issue-agent-deployment.yaml` | Agent deployment with AuthBridge labels |
| `demos/github-issue/k8s/github-tool-deployment.yaml` | GitHub tool deployment (no injection) |

## Next Steps

- **UI Deployment**: See [demo-ui.md](demo-ui.md) for deploying via the Kagenti dashboard
- **AuthProxy Details**: See the [AuthProxy README](../../AuthProxy/README.md) for inbound
  JWT validation and outbound token exchange internals
- **Multi-Target Demo**: See the [multi-target demo](../multi-target/demo.md) for
  route-based token exchange to multiple tool services
- **Access Policies**: See the [access policies proposal](../../PROPOSAL-access-policies.md)
  for role-based delegation control
- **AuthBridge Overview**: See the [AuthBridge README](../../README.md) for architecture details
