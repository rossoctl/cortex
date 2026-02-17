# GitHub Issue Agent Demo with AuthBridge

This document provides detailed steps for running the **GitHub Issue Agent** demo
with **AuthBridge** providing transparent, zero-trust token management.

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
┌─────────────────────────────────────────────────────────────────────────────────┐
│                              KUBERNETES CLUSTER                                  │
│                                                                                  │
│  ┌───────────────────────────────────────────────────────────────────────────┐   │
│  │                  GIT-ISSUE-AGENT POD (namespace: team1)                   │   │
│  │                                                                           │   │
│  │  ┌─────────────────┐  ┌─────────────┐  ┌──────────────────────────────┐  │   │
│  │  │  git-issue-agent │  │   spiffe-   │  │      client-registration     │  │   │
│  │  │  (A2A agent,     │  │   helper    │  │  (registers with Keycloak    │  │   │
│  │  │   port 8000)     │  │             │  │   using SPIFFE ID)           │  │   │
│  │  └─────────────────┘  └─────────────┘  └──────────────────────────────┘  │   │
│  │                                                                           │   │
│  │  ┌───────────────────────────────────────────────────────────────────┐    │   │
│  │  │                    AuthBridge Sidecar (envoy-proxy)                │    │   │
│  │  │  Inbound (port 15124):                                            │    │   │
│  │  │    - Validates JWT (signature + issuer + audience)                 │    │   │
│  │  │    - Rejects invalid/missing tokens with 401                      │    │   │
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
│  │  Provides SPIFFE     │          │  - demo realm        │                      │
│  │  identities (SVIDs)  │          │  - token exchange    │                      │
│  └──────────────────────┘          └──────────────────────┘                      │
└──────────────────────────────────────────────────────────────────────────────────┘
```

## Token Flow

```
  Kagenti UI           Agent Pod              Keycloak        GitHub Tool
    │                     │                      │                 │
    │  1. Login           │                      │                 │
    │  (get user token)   │                      │                 │
    │────────────────────────────────────────────►│                 │
    │◄────────────────────────────────────────────│                 │
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
    │              Agent processes prompt,        │                 │
    │              calls GitHub tool              │                 │
    │                     │                      │                 │
    │              ┌──────┴──────┐               │                 │
    │              │  AuthBridge │               │                 │
    │              │  OUTBOUND   │               │                 │
    │              │  intercepts │               │                 │
    │              └──────┬──────┘               │                 │
    │                     │                      │                 │
    │                     │  3. Token Exchange    │                 │
    │                     │  (RFC 8693)           │                 │
    │                     │─────────────────────►│                 │
    │                     │◄─────────────────────│                 │
    │                     │  New token:           │                 │
    │                     │  aud=github-tool      │                 │
    │                     │  sub=<original user>  │                 │
    │                     │                      │                 │
    │                     │  4. Forward request   │                 │
    │                     │  with exchanged token │                 │
    │                     │──────────────────────────────────────►│
    │                     │                      │                 │
    │                     │                      │    Tool validates│
    │                     │                      │    token, checks │
    │                     │                      │    scopes, uses  │
    │                     │                      │    appropriate   │
    │                     │                      │    GitHub PAT    │
    │                     │                      │                 │
    │                     │◄──────────────────────────────────────│
    │◄────────────────────│  5. Response          │                 │
    │  GitHub issues      │                      │                 │
```

## Key Security Properties

| Property | How It's Achieved |
|----------|-------------------|
| **No hardcoded agent secrets** | Client credentials dynamically generated by client-registration using SPIFFE ID |
| **Identity-based auth** | SPIFFE ID is both the pod identity and the Keycloak client ID |
| **Inbound validation** | AuthBridge validates all incoming requests before they reach the agent |
| **Audience-scoped tokens** | Original token scoped to Agent; exchanged token scoped to GitHub tool |
| **User attribution** | `sub` and `preferred_username` preserved through token exchange |
| **Scope-based authorization** | Tool uses token scopes to determine access level (public vs. privileged) |
| **Transparent to agent code** | The agent makes plain HTTP calls; AuthBridge handles all token management |

---

## Prerequisites

Ensure you have completed the Kagenti platform setup as described in the
[Installation Guide](https://github.com/kagenti/kagenti/blob/main/docs/install.md).

You should also have:
- The [kagenti-extensions](https://github.com/kagenti/kagenti-extensions) repo cloned
- Python 3.9+ with `venv` support
- Two GitHub Personal Access Tokens (PATs):
  - `<PUBLIC_ACCESS_PAT>` — access to public repositories only
  - `<PRIVILEGED_ACCESS_PAT>` — access to all repositories

See the [upstream demo](https://github.com/kagenti/kagenti/blob/main/docs/demos/demo-github-issue.md#required-github-pat-tokens) for instructions on creating GitHub PAT tokens.

---

## Step 1: Deploy the Webhook with AuthBridge Support

The kagenti-webhook automatically injects AuthBridge sidecars into agent deployments.
Deploy it with the AuthBridge demo flag:

```bash
cd kagenti-webhook

# Deploy webhook + create namespace + apply ConfigMaps
AUTHBRIDGE_DEMO=true AUTHBRIDGE_NAMESPACE=team1 ./scripts/webhook-rollout.sh
```

This automatically:
1. Builds and deploys the kagenti-webhook
2. Creates the `team1` namespace with the `kagenti-enabled=true` label
3. Applies the required ConfigMaps for AuthBridge (environments, authbridge-config, envoy-config, spiffe-helper-config)

> **Note:** If you want to use a different namespace, set `AUTHBRIDGE_NAMESPACE=<your-namespace>` and update all subsequent commands accordingly.

---

## Step 2: Configure Keycloak

Keycloak needs to be configured with the correct clients, scopes, and users for the
token exchange flow between the agent and the GitHub tool.

### Port-forward Keycloak

```bash
kubectl port-forward service/keycloak-service -n keycloak 8080:8080
```

### Run the setup script

In a new terminal:

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
| **Realm** | `demo` | Keycloak realm for the demo |
| **Client** | `github-tool` | Target audience for token exchange |
| **Scope** | `agent-team1-git-issue-agent-aud` | Realm DEFAULT — auto-adds Agent's SPIFFE ID to all tokens |
| **Scope** | `github-tool-aud` | Realm OPTIONAL — for exchanged tokens targeting the tool |
| **Scope** | `github-full-access` | Realm OPTIONAL — for privileged GitHub API access |
| **User** | `alice` (password: `alice123`) | Regular user — public access |
| **User** | `bob` (password: `bob123`) | Privileged user — full access |

---

## Step 3: Apply ConfigMaps

Apply the demo-specific ConfigMaps that configure AuthBridge for this demo. These
ConfigMaps tell the sidecars how to connect to Keycloak and where to exchange tokens.

> **Important: Apply ConfigMaps BEFORE deploying the agent.** The agent pod reads
> ConfigMap values at startup. If the agent starts before the ConfigMaps are correct,
> the client will be registered in the wrong realm. If this happens, delete the stale
> Keycloak client, fix the ConfigMaps, and restart the agent deployment with
> `kubectl rollout restart deployment/git-issue-agent -n team1`.

```bash
cd AuthBridge

# Apply the GitHub Issue demo ConfigMaps
kubectl apply -f demos/github-issue/k8s/configmaps.yaml
```

Verify the ConfigMap has the correct values:

```bash
kubectl get configmap environments -n team1 -o jsonpath='{.data.KEYCLOAK_REALM}'
# Expected: demo
```

> **Note:** If you're using a different namespace or service account, edit
> `configmaps.yaml` and update the `namespace` and `EXPECTED_AUDIENCE` fields.
> If SPIRE is not available, set `SPIRE_ENABLED: "false"` in the `environments`
> ConfigMap and update `EXPECTED_AUDIENCE` to match the static client ID format
> (e.g., `team1/git-issue-agent` instead of `spiffe://...`).

---

## Step 4: Create GitHub PAT Secret

The GitHub tool needs PAT tokens to access the GitHub API. Create a Kubernetes secret
with your tokens:

```bash
kubectl create secret generic github-tool-secrets -n team1 \
  --from-literal=INIT_AUTH_HEADER="Bearer <PRIVILEGED_ACCESS_PAT>" \
  --from-literal=UPSTREAM_HEADER_TO_USE_IF_IN_AUDIENCE="Bearer <PRIVILEGED_ACCESS_PAT>" \
  --from-literal=UPSTREAM_HEADER_TO_USE_IF_NOT_IN_AUDIENCE="Bearer <PUBLIC_ACCESS_PAT>"
```

Replace `<PRIVILEGED_ACCESS_PAT>` and `<PUBLIC_ACCESS_PAT>` with your actual GitHub
Personal Access Tokens.

---

## Step 5: Deploy the GitHub Tool

Deploy the GitHub MCP tool as a target service. This deployment does **not** get
AuthBridge injection (it is the target, not the caller):

```bash
kubectl apply -f demos/github-issue/k8s/github-tool-deployment.yaml
```

Wait for the tool to be ready:

```bash
kubectl wait --for=condition=available --timeout=120s deployment/github-tool -n team1
```

---

## Step 6: Deploy the GitHub Issue Agent

Deploy the agent with AuthBridge labels. The kagenti-webhook will automatically inject
the AuthBridge sidecars (proxy-init, spiffe-helper, client-registration, envoy-proxy):

```bash
kubectl apply -f demos/github-issue/k8s/git-issue-agent-deployment.yaml
```

Wait for the agent to be ready:

```bash
kubectl wait --for=condition=available --timeout=180s deployment/git-issue-agent -n team1
```

> **Note:** The agent may take longer to start because it waits for SPIFFE credentials
> and Keycloak client registration to complete before becoming ready.

### Verify injected containers

Confirm that the webhook injected the AuthBridge sidecars:

```bash
kubectl get pod -n team1 -l app=git-issue-agent -o jsonpath='{.items[0].spec.containers[*].name}'
```

Expected output (with SPIFFE):

```
git-issue-agent spiffe-helper kagenti-client-registration envoy-proxy
```

### Enable service accounts for the registered client

The dynamically registered client needs service accounts enabled for `client_credentials` grant:

```bash
kubectl exec deployment/git-issue-agent -n team1 -c git-issue-agent -- sh -c '
CLIENT_ID=$(cat /shared/client-id.txt)

ADMIN_TOKEN=$(curl -s http://keycloak-service.keycloak.svc:8080/realms/master/protocol/openid-connect/token \
  -d "grant_type=password" \
  -d "client_id=admin-cli" \
  -d "username=admin" \
  -d "password=admin" | jq -r ".access_token")

INTERNAL_ID=$(curl -s -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://keycloak-service.keycloak.svc:8080/admin/realms/demo/clients?clientId=$CLIENT_ID" | jq -r ".[0].id")

curl -s -X PUT -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  "http://keycloak-service.keycloak.svc:8080/admin/realms/demo/clients/$INTERNAL_ID" \
  -d "{\"clientId\": \"$CLIENT_ID\", \"serviceAccountsEnabled\": true}"

echo "Service accounts enabled for: $CLIENT_ID"
'
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
kubectl logs deployment/git-issue-agent -n team1 -c git-issue-agent
```

Expected:

```
INFO:     Started server process [17]
INFO:     Waiting for application startup.
INFO:     Application startup complete.
INFO:     Uvicorn running on http://0.0.0.0:8000 (Press CTRL+C to quit)
```

---

## Step 8: Test the AuthBridge Flow

These tests verify both **inbound** JWT validation and **outbound** token exchange
end-to-end.

### Setup

```bash
# Start a test client pod (sends requests from outside the agent pod)
kubectl run test-client --image=nicolaka/netshoot -n team1 --restart=Never -- sleep 3600
kubectl wait --for=condition=ready pod/test-client -n team1 --timeout=30s

# Get the agent's client credentials
CLIENT_ID=$(kubectl exec deployment/git-issue-agent -n team1 -c envoy-proxy -- cat /shared/client-id.txt)
CLIENT_SECRET=$(kubectl exec deployment/git-issue-agent -n team1 -c envoy-proxy -- cat /shared/client-secret.txt)
echo "Agent Client ID: $CLIENT_ID"

# Get a service account token (simulating what the UI would obtain)
TOKEN=$(kubectl exec test-client -n team1 -- curl -s -X POST \
  "http://keycloak-service.keycloak.svc:8080/realms/demo/protocol/openid-connect/token" \
  -d "grant_type=client_credentials" \
  -d "client_id=$CLIENT_ID" \
  -d "client_secret=$CLIENT_SECRET" | jq -r '.access_token')
```

### Determine the agent service URL

The service name and port depend on how the agent was deployed:
- **Deployed via Kagenti UI**: service name is typically `git-issue-agent` on port `8080`
- **Deployed via kubectl**: service name and port match the YAML (e.g., `git-issue-agent-service:8000`)

```bash
# Check the actual service name and port
kubectl get svc -n team1 | grep git-issue-agent

# Set the variable for subsequent commands
AGENT_URL="http://git-issue-agent:8080"
# Or if deployed via kubectl:
# AGENT_URL="http://git-issue-agent-service:8000"
```

### 8a. Inbound Rejection - No Token

```bash
kubectl exec test-client -n team1 -- curl -s $AGENT_URL/.well-known/agent.json
# Expected: {"error":"unauthorized","message":"missing Authorization header"}
```

### 8b. Inbound Rejection - Invalid Token

```bash
kubectl exec test-client -n team1 -- curl -s \
  -H "Authorization: Bearer invalid-token" \
  $AGENT_URL/.well-known/agent.json
# Expected: {"error":"unauthorized","message":"token validation failed: ..."}
```

### 8c. Valid Token - Agent Card

```bash
kubectl exec test-client -n team1 -- curl -s \
  -H "Authorization: Bearer $TOKEN" \
  $AGENT_URL/.well-known/agent.json | jq
```

```json
# Expected: Agent card JSON response
{
  "capabilities": {
    "streaming": true
  },
  "defaultInputModes": [
    "text"
  ],
  "defaultOutputModes": [
    "text"
  ],
  "description": "Answer queries about Github issues",
  "name": "Github issue agent",
  "preferredTransport": "JSONRPC",
  "protocolVersion": "0.3.0",
  "securitySchemes": {
    "Bearer": {
      "bearerFormat": "JWT",
      "description": "OAuth 2.0 JWT token",
      "scheme": "bearer",
      "type": "http"
    }
  },
  "skills": [
    {
      "description": "Answer queries by searching through a given slack server",
      "examples": [
        "Find me the issues with the most comments in kubernetes/kubernetes",
        "Show all issues assigned to me across any repository"
      ],
      "id": "github_issue_agent",
      "name": "Github issue agent",
      "tags": [
        "git",
        "github",
        "issues"
      ]
    }
  ],
  "url": "http://0.0.0.0:8000/",
  "version": "1.0.0"
}
```

### 8d. End-to-End: Query GitHub Issues

This is the full demo flow — the request goes through inbound validation, reaches
the agent, the agent calls the GitHub tool (token exchange happens transparently),
and returns the result.

> **Note:** JWT tokens passed via `kubectl exec -– curl -H "Authorization: Bearer $TOKEN"`
> can get mangled by double shell expansion. To avoid this, we exec into the test-client
> pod and run all commands from inside it.

#### Step 1: Open a shell inside the test-client pod

```bash
kubectl exec -it test-client -n team1 -- sh
```

#### Step 2: Get credentials and a token

Inside the test-client pod, run:

```bash
# Get a Keycloak admin token
ADMIN_TOKEN=$(curl -s http://keycloak-service.keycloak.svc:8080/realms/master/protocol/openid-connect/token \
  -d "grant_type=password" \
  -d "client_id=admin-cli" \
  -d "username=admin" \
  -d "password=admin" | jq -r ".access_token")

# Look up the agent's client in the demo realm
CLIENTS=$(curl -s -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://keycloak-service.keycloak.svc:8080/admin/realms/demo/clients?clientId=team1/git-issue-agent")
INTERNAL_ID=$(echo "$CLIENTS" | jq -r ".[0].id")
CLIENT_ID=$(echo "$CLIENTS" | jq -r ".[0].clientId")

# Get the client secret
CLIENT_SECRET=$(curl -s -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://keycloak-service.keycloak.svc:8080/admin/realms/demo/clients/$INTERNAL_ID/client-secret" | jq -r ".value")

echo "Client ID:     $CLIENT_ID"
echo "Secret length: ${#CLIENT_SECRET}"

# Get an OAuth token for the agent
TOKEN=$(curl -s -X POST \
  "http://keycloak-service.keycloak.svc:8080/realms/demo/protocol/openid-connect/token" \
  -d "grant_type=client_credentials" \
  --data-urlencode "client_id=$CLIENT_ID" \
  --data-urlencode "client_secret=$CLIENT_SECRET" | jq -r ".access_token")

echo "Token length:  ${#TOKEN}"
```

You should see output like:

```
Client ID:     team1/git-issue-agent
Secret length: 32
Token length:  1165
```

#### Step 3: Send a prompt to the agent

Still inside the test-client pod, send the A2A v0.3.0 request:

> **Note:** This request may take 30-60 seconds as the agent calls the LLM and the
> GitHub tool. The Envoy route timeout is set to 300 seconds (5 minutes) to
> accommodate slow LLM inference. If you still see `upstream request timeout`,
> check **Retrieving async results** below.

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

#### Step 4: Exit the test-client pod

```bash
exit
```

### Retrieving async results

If the request timed out but the agent completed the task in the background,
check the agent logs for the task ID:

```bash
kubectl logs deployment/git-issue-agent -n team1 -c agent --tail=5
# Look for: Task <TASK_ID> saved successfully / TaskState.completed
```

Then exec back into the test-client pod and retrieve the result:

```bash
kubectl exec -it test-client -n team1 -- sh

# (re-run the token setup from Step 2 above, then:)
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

### 8e. Check Token Exchange Logs

Verify that AuthBridge performed the token exchange:

```bash
kubectl logs deployment/git-issue-agent -n team1 -c envoy-proxy 2>&1 | grep -i "token"
```

Expected:

```
[Token Exchange] Configuration loaded, attempting token exchange
[Token Exchange] Successfully exchanged token, replacing Authorization header
```

### Clean Up Test Client

```bash
kubectl delete pod test-client -n team1 --ignore-not-found
```

---

## Step 9: Chat via Kagenti UI (Optional)

If you have the Kagenti UI running, you can also interact with the agent through the
dashboard:

1. Navigate to the **Agent Catalog** in the Kagenti UI
2. Select the `team1` namespace
3. Under **Available Agents**, select `git-issue-agent` and click **View Details**
4. In the chat input, type:

   ```
   List issues in kagenti/kagenti repo
   ```

5. The agent will process the request with full AuthBridge security:
   - Your UI token is validated on inbound
   - The token is exchanged for the GitHub tool's audience
   - The tool accesses GitHub with the appropriate PAT

---

## How AuthBridge Changes the Original Demo

| Aspect | Original Demo | With AuthBridge |
|--------|--------------|-----------------|
| **Agent secrets** | Manual PAT token configuration | Dynamic credentials via SPIFFE + client-registration |
| **Inbound auth** | No validation | JWT validation (signature, issuer, audience) |
| **Token management** | Agent code handles tokens | Transparent sidecar — agent code unchanged |
| **Token for tool** | Same PAT token passed through | OAuth token exchange (RFC 8693) |
| **User attribution** | No user tracking | `sub` claim preserved through exchange |
| **Access control** | Single PAT for all users | Scope-based: public vs. privileged |

---

## Troubleshooting

### Invalid Client or Invalid Client Credentials

**Symptom:** `{"error":"invalid_client","error_description":"Invalid client or Invalid client credentials"}`

**Cause:** The client was registered in the wrong Keycloak realm (typically `master` instead
of `demo`). This happens when the `environments` ConfigMap is updated **after** the agent
pod has already started — the pod reads ConfigMap values at startup time.

**Fix:**

```bash
# 1. Check which realm the client was registered in
kubectl exec deployment/git-issue-agent -n team1 -c kagenti-client-registration -- \
  sh -c 'echo "KEYCLOAK_REALM=$KEYCLOAK_REALM"'

# 2. If it shows "master" instead of "demo", delete the stale client
kubectl exec test-client -n team1 -- sh -c '
ADMIN_TOKEN=$(curl -s http://keycloak-service.keycloak.svc:8080/realms/master/protocol/openid-connect/token \
  -d "grant_type=password" -d "client_id=admin-cli" -d "username=admin" -d "password=admin" | jq -r ".access_token")
CLIENT_ID="team1/git-issue-agent"
INTERNAL_ID=$(curl -s -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://keycloak-service.keycloak.svc:8080/admin/realms/master/clients?clientId=$CLIENT_ID" | jq -r ".[0].id")
curl -s -X DELETE -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://keycloak-service.keycloak.svc:8080/admin/realms/master/clients/$INTERNAL_ID"
echo "Deleted stale client from master realm"'

# 3. Verify ConfigMap is correct, then restart
kubectl get configmap environments -n team1 -o jsonpath='{.data.KEYCLOAK_REALM}'
# Should show: demo
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
kubectl logs deployment/git-issue-agent -n team1 -c git-issue-agent
```

### GitHub Tool Returns 401

**Symptom:** Tool rejects the exchanged token

**Fix:** Verify the tool's environment variables match the Keycloak configuration:
- `ISSUER` should be `http://keycloak.localtest.me:8080/realms/demo`
- `AUDIENCE` should be `github-tool`

---

## Cleanup

### Delete Deployments

```bash
kubectl delete -f demos/github-issue/k8s/git-issue-agent-deployment.yaml
kubectl delete -f demos/github-issue/k8s/github-tool-deployment.yaml
kubectl delete secret github-tool-secrets -n team1
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
| `demos/github-issue/demo.md` | This guide |
| `demos/github-issue/setup_keycloak.py` | Keycloak configuration script |
| `demos/github-issue/k8s/configmaps.yaml` | ConfigMaps for AuthBridge sidecars |
| `demos/github-issue/k8s/git-issue-agent-deployment.yaml` | Agent deployment with AuthBridge labels |
| `demos/github-issue/k8s/github-tool-deployment.yaml` | GitHub tool deployment (no injection) |

## Next Steps

- **Multi-Target Demo**: See the [multi-target demo](../multi-target/demo.md) for
  route-based token exchange to multiple tool services
- **Access Policies**: See the [access policies proposal](../../PROPOSAL-access-policies.md)
  for role-based delegation control
- **AuthBridge Overview**: See the [AuthBridge README](../../README.md) for architecture details
