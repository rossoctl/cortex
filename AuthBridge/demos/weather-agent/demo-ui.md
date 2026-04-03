# Weather Agent Demo with AuthBridge

This guide walks through deploying the **Weather Service Agent** with **AuthBridge**
using the **Kagenti UI** for agent and tool deployment. Infrastructure setup
(webhook, Keycloak, ConfigMaps) is done via CLI, while the agent and tool are
imported and deployed through the Kagenti dashboard.

This is the recommended **getting-started** demo for AuthBridge. It demonstrates
inbound JWT validation and automatic identity registration with a simple agent
that doesn't require token exchange. For a more advanced demo showing outbound
token exchange and scope-based access control, see the
[GitHub Issue Agent demo](../github-issue/demo.md).

## What This Demo Shows

1. **Agent identity** — The agent automatically registers with Keycloak using its
   SPIFFE ID, with no hardcoded secrets
2. **Inbound validation** — Requests to the agent are validated (JWT signature,
   issuer, and audience) before reaching the agent code
3. **Transparent outbound passthrough** — When the agent calls the weather tool,
   AuthBridge passes the request through without modification (default outbound
   policy), so agents work out-of-the-box with any tool or LLM provider
4. **Zero code changes** — The agent and tool source code require no modifications;
   all security is handled by AuthBridge sidecars

## Architecture

```
┌──────────────────────────────────────────────────────────────────────────────────┐
│                              KUBERNETES CLUSTER                                  │
│                                                                                  │
│  ┌───────────────────────────────────────────────────────────────────────────┐   │
│  │               WEATHER-SERVICE POD (namespace: team1)                      │   │
│  │                                                                           │   │
│  │  ┌──────────────────┐  ┌─────────────┐  ┌──────────────────────────────┐  │   │
│  │  │ weather-service  │  │   spiffe-   │  │      client-registration     │  │   │
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
│  │  │    - HTTP: Passthrough (default policy, no token exchange)        │    │   │
│  │  │    - HTTPS: TLS passthrough (no interception)                     │    │   │
│  │  └───────────────────────────────────────────────────────────────────┘    │   │
│  └───────────────────────────────────────────────────────────────────────────┘   │
│                                      │                                           │
│                     Plain HTTP call  │(no token exchange)                        │
│                                      ▼                                           │
│  ┌───────────────────────────────────────────────────────────────────────────┐   │
│  │               WEATHER-TOOL POD (namespace: team1)                         │   │
│  │                                                                           │   │
│  │  ┌──────────────────────────────────────────────────────────────────┐     │   │
│  │  │                  weather-tool (port 8000)                        │     │   │
│  │  │  - MCP server: provides get_weather tool                         │     │   │
│  │  │  - Calls public weather API (Open-Meteo)                         │     │   │
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
│  │  identities (SVIDs)  │          │  - JWKS for inbound  │                      │
│  │                      │          │    JWT validation    │                      │
│  └──────────────────────┘          └──────────────────────┘                      │
└──────────────────────────────────────────────────────────────────────────────────┘
```

## Prerequisites

Ensure you have completed the Kagenti platform setup as described in the
[Installation Guide](https://github.com/kagenti/kagenti/blob/main/docs/install.md),
including the Kagenti UI.

You should also have:
- The Kagenti UI running at `http://kagenti-ui.localtest.me:8080`
- An LLM provider — either:
  - **Ollama** running locally with a model (e.g. `llama3.2:3b-instruct-fp16`), or
  - **OpenAI API key** (recommended for most reliable results; see
    [agent-examples#173](https://github.com/kagenti/agent-examples/issues/173) for
    known Ollama + crewai compatibility issues)

---

## Installer-Provided Resources

The Kagenti installer creates everything this demo needs in the target namespace:

- **`kagenti` realm** in Keycloak
- **`keycloak-admin-secret`** Secret (Keycloak admin credentials)
- **`authbridge-config`**, **`spiffe-helper-config`**, **`envoy-config`** ConfigMaps

No additional Keycloak configuration, Secrets, or ConfigMaps are required for
this demo. The weather agent uses outbound passthrough (no token exchange), and
inbound JWT validation works with signature and issuer checks alone.

> If your Keycloak admin credentials differ from the default (`admin`/`admin`),
> update the secret:
> ```bash
> kubectl create secret generic keycloak-admin-secret -n team1 \
>   --from-literal=KEYCLOAK_ADMIN_USERNAME=<your-admin-user> \
>   --from-literal=KEYCLOAK_ADMIN_PASSWORD=<your-admin-password> \
>   --dry-run=client -o yaml | kubectl apply -f -
> ```

---

## Step 1: Import the Weather Tool via Kagenti UI

1. Navigate to [Import Tool](http://kagenti-ui.localtest.me:8080/tools/import)
   in the Kagenti UI.

2. In the **Namespace** drop-down, choose `team1`.

3. Select **Deploy From Image** as the deployment method.

4. For **Container Image**, use `ghcr.io/kagenti/agent-examples/weather_tool`.

5. Pick a corresponding **Image Tag** or keep the default `latest`.

6. Set **MCP Transport Protocol** to `streamable HTTP`.

7. Make sure **Enable AuthBridge sidecar injection** is **unchecked**.

8. Make sure **Enable SPIRE identity (spiffe-helper sidecar)** is **unchecked**.

   > The weather tool is a simple MCP server calling a public weather API. It
   > does not need AuthBridge sidecars or token validation.

9. Click **Deploy New Tool**.

### Verify the tool is running

```bash
kubectl get pods -n team1 | grep weather-tool
# Expected: weather-tool-xxxx   1/1   Running   0   ...
```

---

## Step 2: Import the Weather Agent via Kagenti UI

1. Navigate to [Import Agent](http://kagenti-ui.localtest.me:8080/agents/import)
   in the Kagenti UI.

2. In the **Namespace** drop-down, choose `team1`.

3. Select **Build from Source** as the deployment method.

4. Under **Source Repository** select:
   - **Git Repository URL**: `https://github.com/kagenti/agent-examples`
   - **Git Branch**: `main`
   - **Select Example**: `Weather Service Agent`
   - **Source Path**: `a2a/weather_service`

5. **Protocol**: `A2A`

6. **Framework**: `LangGraph`

7. **Workload Type** select `Deployment`.

8. Make sure **Enable AuthBridge sidecar injection** is checked.

9. Make sure **Enable SPIRE identity (spiffe-helper sidecar)** is checked.

10. Under **Port Configuration**, set **Service Port** to `8080` and **Target Port** to `8000`

11. Under **Environment Variables**, click **Import from File/URL**,
    Select **From URL** and provide the **URL** from this repo:
    - For Ollama: `https://raw.githubusercontent.com/kagenti/agent-examples/refs/heads/main/a2a/weather_service/.env.ollama`
    - For OpenAI: `https://raw.githubusercontent.com/kagenti/agent-examples/refs/heads/main/a2a/weather_service/.env.openai`
    - Click **Fetch & Parse** — this populates all environment variables including
      LLM settings and `MCP_URL`. No manual editing is needed.
    - Click **Import** to set all the env. variables.

    The Ollama variant sets all direct values. The OpenAI variant includes
    **Secret** type entries referencing `openai-secret` for `LLM_API_KEY`
    and `OPENAI_API_KEY`.

    > **Tip:** You can also upload the file directly from your local system.
    > **OpenAI prerequisite:** If using OpenAI, create the secret first:
    > ```bash
    > kubectl create secret generic openai-secret -n team1 \
    >   --from-literal=apikey="<YOUR_OPENAI_API_KEY>"
    > ```

12. Click **Build & Deploy Agent**.

Wait for the Shipwright build to complete and the deployment to become ready.

---

## Step 3: Verify the Deployment

### Check pod status

```bash
kubectl get pods -n team1
```

Expected output:

```
NAME                               READY   STATUS    RESTARTS   AGE
weather-service-58768bdb67-xxxxx   4/4     Running   0          2m
weather-tool-7f8c9d6b44-yyyyy     1/1     Running   0          5m
```

> **Note:** The agent pod should show **4/4** containers — the agent itself plus
> three AuthBridge sidecars (spiffe-helper, kagenti-client-registration, envoy-proxy)
> injected by the webhook.

### Verify injected containers

```bash
kubectl get pod -n team1 -l app.kubernetes.io/name=weather-service -o jsonpath='{.items[0].spec.containers[*].name}'
```

Expected:

```
agent kagenti-client-registration envoy-proxy spiffe-helper
```

### Check client registration

```bash
kubectl logs deployment/weather-service -n team1 -c kagenti-client-registration
```

Expected:

```
SPIFFE credentials ready!
Client ID (SPIFFE ID): spiffe://localtest.me/ns/team1/sa/weather-service
Created Keycloak client "spiffe://localtest.me/ns/team1/sa/weather-service"
Client registration complete!
```

### Check agent logs

```bash
kubectl logs deployment/weather-service -n team1 -c agent
```

Expected:

```
INFO:     Started server process [17]
INFO:     Waiting for application startup.
INFO:     Application startup complete.
INFO:     Uvicorn running on http://0.0.0.0:8000 (Press CTRL+C to quit)
```

### Check the service endpoint

```bash
kubectl get svc -n team1 | grep weather-service
```

Expected:

```
weather-service   ClusterIP   10.96.x.x   <none>   8080/TCP   5m
```

The service maps **port 8080** to the agent's internal port 8000.

---

## Step 4: Verify LLM Provider

The agent uses an LLM for inference. Follow the section that matches your chosen
provider.

### Option A: Ollama (local models)

Verify Ollama is running:

```bash
ollama list
```

You should see `llama3.2:3b-instruct-fp16` (or whichever model you configured) on
the list. If Ollama is not running, start it in a separate terminal (`ollama serve`)
and ensure the model is pulled (`ollama pull llama3.2:3b-instruct-fp16`).

> **Note:** The `.env.ollama` file defaults to `LLM_API_BASE=http://host.docker.internal:11434/v1`,
> which reaches Ollama running on your host machine via the Kind/Docker Desktop gateway.
> If you deploy Ollama inside the cluster instead, patch the agent:
> ```bash
> kubectl set env deployment/weather-service -n team1 -c agent \
>   LLM_API_BASE="http://ollama.ollama.svc:11434/v1"
> ```

#### Ollama Port Exclusion

AuthBridge's `proxy-init` init container redirects traffic through Envoy. By
default, only port 8080 (Keycloak) is excluded. Ollama traffic on port 11434
gets intercepted, which corrupts LLM streaming responses.

**Fix:** Add the `kagenti.io/outbound-ports-exclude` annotation to the
deployment so `proxy-init` skips Ollama's port:

```bash
kubectl patch deployment weather-service -n team1 --type=merge -p='
{"spec":{"template":{"metadata":{"annotations":{"kagenti.io/outbound-ports-exclude":"11434"}}}}}'
kubectl rollout status deployment/weather-service -n team1 --timeout=120s
```

### Option B: OpenAI

Verify the OpenAI secret exists (see the prerequisite note in
[Step 2](#step-2-import-the-weather-agent-via-kagenti-ui)):

```bash
kubectl get secret openai-secret -n team1
```

Verify the agent has the correct environment variables:

```bash
kubectl exec deployment/weather-service -n team1 -c agent -- env | grep -E "LLM_|OPENAI"
```

Expected:

```
LLM_API_BASE=https://api.openai.com/v1
LLM_MODEL=gpt-4o-mini-2024-07-18
LLM_API_KEY=sk-...
OPENAI_API_KEY=sk-...
```

> **Note:** OpenAI uses HTTPS, which AuthBridge passes through via TLS passthrough.
> No Ollama port exclusion workaround is needed.

---

## Step 5: Chat via Kagenti UI

1. Navigate to the **Agents** to open **Agents Catalog** in the Kagenti UI.
2. Select the `team1` namespace.
3. Under **Available Agents**, select `weather-service` and click **View Details**.
4. Verify the **Agent Card** is visible (this confirms the agent is running and
   the `/.well-known/*` bypass is working).
5. Use the **Chat** panel to send a message, e.g. "What is the weather in New York?".
6. The agent should respond with current weather information.

> **Troubleshooting:** If UI chat returns a `401`, verify that both the UI and
> AuthBridge are configured against the same `kagenti` realm. You can also use
> [Step 6: Test via CLI](#step-6-test-via-cli) to test the AuthBridge flow
> independently.

---

## Step 6: Test via CLI

Test the AuthBridge flow from the command line to verify inbound validation.

### Setup

```bash
# Start a test client pod
kubectl run test-client --image=nicolaka/netshoot -n team1 --restart=Never -- sleep 3600
kubectl wait --for=condition=ready pod/test-client -n team1 --timeout=30s
```

### 6a. Agent Card - Public Endpoint (No Token Required)

The `/.well-known/agent.json` endpoint is publicly accessible — AuthBridge's
go-processor bypasses JWT validation for `/.well-known/*`, `/healthz`, `/readyz`,
and `/livez` by default:

```bash
kubectl exec test-client -n team1 -- curl -s \
  http://weather-service:8080/.well-known/agent.json | jq .name
# Expected: "weather_service"
```

### 6b. Inbound Rejection - No Token

Non-public endpoints require a valid JWT:

```bash
kubectl exec test-client -n team1 -- curl -s \
  http://weather-service:8080/
# Expected: {"error":"unauthorized","message":"missing Authorization header"}
```

### 6c. Inbound Rejection - Invalid Token

A malformed or tampered token fails the JWKS signature check:

```bash
kubectl exec test-client -n team1 -- curl -s \
  -H "Authorization: Bearer invalid-token" \
  http://weather-service:8080/
# Expected: {"error":"unauthorized","message":"token validation failed: failed to parse/validate token: ..."}
```

### 6d. End-to-End Test with Valid Token

Open a shell inside the test-client pod to avoid JWT shell expansion issues:

```bash
kubectl exec -it test-client -n team1 -- sh
```

Inside the pod, get credentials and send a request:

```bash
# Get a Keycloak admin token from the kagenti realm
ADMIN_TOKEN=$(curl -s http://keycloak-service.keycloak.svc:8080/realms/kagenti/protocol/openid-connect/token \
  -d "grant_type=password" \
  -d "client_id=admin-cli" \
  -d "username=admin" \
  -d "password=admin" | jq -r ".access_token")

echo "Admin token length: ${#ADMIN_TOKEN}"

# Look up the agent's client in the kagenti realm
SPIFFE_ID="spiffe://localtest.me/ns/team1/sa/weather-service"
CLIENTS=$(curl -s -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://keycloak-service.keycloak.svc:8080/admin/realms/kagenti/clients" \
  --data-urlencode "clientId=$SPIFFE_ID" --get)
CLIENT_ID=$(echo "$CLIENTS" | jq -r ".[0].clientId")
CLIENT_SECRET=$(echo "$CLIENTS" | jq -r ".[0].secret")

echo "Client ID:     $CLIENT_ID"
echo "Secret length: ${#CLIENT_SECRET}"

# Get an OAuth token for the agent
TOKEN=$(curl -s -X POST \
  "http://keycloak-service.keycloak.svc:8080/realms/kagenti/protocol/openid-connect/token" \
  -d "grant_type=client_credentials" \
  --data-urlencode "client_id=$CLIENT_ID" \
  --data-urlencode "client_secret=$CLIENT_SECRET" | jq -r ".access_token")

echo "Token length:  ${#TOKEN}"

# Send a prompt to the agent (A2A v0.3.0)
curl -s --max-time 300 \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -X POST http://weather-service:8080/ \
  -d '{
    "jsonrpc": "2.0",
    "id": "test-1",
    "method": "message/send",
    "params": {
      "message": {
        "role": "user",
        "messageId": "msg-001",
        "parts": [{"type": "text", "text": "What is the weather in New York?"}]
      }
    }
  }' | jq
```

Exit the pod when done:

```bash
exit
```

### 6e. Verify AuthProxy Logs (Inbound)

Check the ext_proc logs to confirm inbound validation is working:

```bash
kubectl logs deployment/weather-service -n team1 -c envoy-proxy 2>&1 | grep "\[Inbound\]"
```

Expected:

```
[Inbound] Token validated - issuer: http://keycloak.localtest.me:8080/realms/kagenti, audience: [spiffe://localtest.me/ns/team1/sa/weather-service ...]
[Inbound] JWT validation succeeded, forwarding request
```

### Clean Up Test Client

```bash
kubectl delete pod test-client -n team1 --ignore-not-found
```

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

# 3. Restart the agent to retry registration
kubectl rollout restart deployment/weather-service -n team1
```

### Agent Missing Environment Variables

**Symptom:** Agent fails to start or can't reach the weather tool

**Cause:** The UI deployment didn't include all required environment variables.

**Fix:** Patch the deployment directly:

```bash
kubectl set env deployment/weather-service -n team1 -c agent \
  MCP_URL="http://mcp-weather-tool-headless:8000/mcp"
kubectl rollout status deployment/weather-service -n team1 --timeout=180s
```

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
kubectl rollout restart deployment/weather-service -n team1
```

### Agent Pod Not Starting (4/4 containers)

**Symptom:** Pod shows 3/4 or less containers ready

**Fix:** Check each container's logs:

```bash
kubectl logs deployment/weather-service -n team1 -c kagenti-client-registration
kubectl logs deployment/weather-service -n team1 -c spiffe-helper
kubectl logs deployment/weather-service -n team1 -c envoy-proxy
kubectl logs deployment/weather-service -n team1 -c agent
```

---

## Cleanup

### Via Kagenti UI

1. Go to the **Agent Catalog**, find `weather-service`, and click **Delete**.
2. Go to the **Tool Catalog**, find `weather-tool`, and click **Delete**.

### Via CLI

```bash
kubectl delete deployment weather-service -n team1
kubectl delete deployment weather-tool -n team1
kubectl delete svc weather-service -n team1
kubectl delete svc weather-tool -n team1
kubectl delete pod test-client -n team1 --ignore-not-found
```

### Delete Namespace (removes everything)

```bash
kubectl delete namespace team1
```

---

## Next Steps

- **Advanced Demo**: See the [GitHub Issue Agent demo](../github-issue/demo.md) for
  outbound token exchange, scope-based access control, and Alice vs Bob scenarios
- **AuthProxy Details**: See the [AuthProxy README](../../AuthProxy/README.md) for inbound
  JWT validation and outbound token exchange internals
- **Multi-Target Demo**: See the [multi-target demo](../multi-target/demo.md) for
  route-based token exchange to multiple tool services
- **AuthBridge Overview**: See the [AuthBridge README](../../README.md) for architecture details
