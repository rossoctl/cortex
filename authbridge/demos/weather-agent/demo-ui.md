# Weather Agent Demo with AuthBridge

This guide walks through deploying the **Weather Service Agent** with **AuthBridge**
using the **Rossoctl UI** for agent and tool deployment. Infrastructure setup
(webhook, Keycloak, ConfigMaps) is done via CLI, while the agent and tool are
imported and deployed through the Rossoctl dashboard.

This is the recommended **getting-started** demo for AuthBridge. It demonstrates
inbound JWT validation and automatic identity registration with a simple agent
that doesn't require token exchange. For a more advanced demo showing outbound
token exchange and scope-based access control, see the
[GitHub Issue Agent demo](../github-issue/demo.md). For the same weather images
with **token exchange and AuthBridge on the tool** (plus a CI-style verify script),
see [Weather Agent — Advanced](demo-ui-advanced.md). To observe the
plugin pipeline in real time while chatting with the agent, see
[Weather Agent with `abctl`](demo-with-abctl.md).

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
│  │  ┌──────────────────┐  ┌────────────────────────────────────────────┐    │   │
│  │  │ weather-service  │  │  AuthBridge sidecar (combined image)        │    │   │
│  │  │  (A2A agent,     │  │  Container name depends on resolved mode:   │    │   │
│  │  │   port 8000)     │  │    proxy-sidecar (default): authbridge-proxy│    │   │
│  │  └──────────────────┘  │    envoy-sidecar:           envoy-proxy     │    │   │
│  │                        │                                              │    │   │
│  │                        │  Inbound:                                    │    │   │
│  │                        │    - Validates JWT (signature + issuer +     │    │   │
│  │                        │      audience via JWKS)                      │    │   │
│  │                        │    - Returns 401 for invalid/missing tokens  │    │   │
│  │                        │  Outbound:                                   │    │   │
│  │                        │    - HTTP: Passthrough (default policy)      │    │   │
│  │                        │    - HTTPS: TLS passthrough (no interception)│    │   │
│  │                        │                                              │    │   │
│  │                        │  spiffe-helper is bundled inside the image  │    │   │
│  │                        │  and gated per-workload by SPIRE_ENABLED.   │    │   │
│  │                        │  Keycloak client registration is             │    │   │
│  │                        │  operator-managed (no in-pod sidecar);      │    │   │
│  │                        │  the operator mounts the resulting Secret    │    │   │
│  │                        │  at /shared/client-{id,secret}.txt.          │    │   │
│  │                        └────────────────────────────────────────────┘    │   │
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
│  │  Provides SPIFFE     │          │  - rossoctl realm     │                      │
│  │  identities (SVIDs)  │          │  - JWKS for inbound  │                      │
│  │                      │          │    JWT validation    │                      │
│  └──────────────────────┘          └──────────────────────┘                      │
└──────────────────────────────────────────────────────────────────────────────────┘
```

## Prerequisites

Ensure you have completed the Rossoctl platform setup as described in the
[Installation Guide](https://github.com/rossoctl/rossoctl/blob/main/docs/install.md),
including the Rossoctl UI.

You should also have:
- The Rossoctl UI running at `http://rossoctl-ui.localtest.me:8080`
- An LLM provider — either:
  - **Ollama** running locally with a model (e.g. `llama3.2:3b-instruct-fp16`), or
  - **OpenAI API key** (recommended for most reliable results; see
    [agent-examples#173](https://github.com/rossoctl/examples/issues/173) for
    known Ollama + crewai compatibility issues)

---

## Installer-Provided Resources

In **`team1`**: `authbridge-config`, `authbridge-runtime-config`, `spiffe-helper-config`,
`envoy-config`. No extra Secrets or ConfigMaps are required for this demo (outbound
passthrough; inbound JWT uses issuer/signature checks).

**`keycloak-admin-secret` is not in `team1`.** Operator 0.2+ keeps it in
**`rossoctl-system`** for client registration. `NotFound` in `team1` is expected:

```bash
kubectl get secret keycloak-admin-secret -n rossoctl-system
```

UI login: secret **`rossoctl-test-user`** in namespace **`keycloak`** (`admin` + password).
Realm **`rossoctl`** is created by the platform installer.

---

## Step 1: Import the Weather Tool via Rossoctl UI

1. Navigate to [Import Tool](http://rossoctl-ui.localtest.me:8080/tools/import)
   in the Rossoctl UI.

2. In the **Namespace** drop-down, choose `team1`, fill *Tool Name* with `weather-tool` (do not use uppercase)

3. Select **Deploy From Image** as the deployment method.

4. For **Container Image**, use `ghcr.io/rossoctl/examples/weather_tool`.

5. Pick a corresponding **Image Tag**, replace the default `v0.0.1` with `latest`.

6. Set **MCP Transport Protocol** to `streamable HTTP`.

7. **Enable AuthBridge sidecar injection** is unchecked by default for tools.
   Leave it unchecked.

8. **Enable SPIRE identity (spiffe-helper sidecar)** should be **unchecked**.

   > The weather tool is a simple MCP server calling a public weather API. It
   > does not need AuthBridge sidecars or token validation.

9. Click **Deploy Tool** and the button should change to "Deploying". If it is not changing review input for errors.

### Verify the tool is running

```bash
kubectl get pods -n team1 | grep weather-tool
# Expected: weather-tool-xxxx   1/1   Running   0   ...
```

---

## Step 2: Import the Weather Agent via Rossoctl UI

1. Navigate to [Import Agent](http://rossoctl-ui.localtest.me:8080/agents/import)
   in the Rossoctl UI.

2. In the **Namespace** drop-down, choose `team1`.

3. Select **Build from Source** as the deployment method.

4. Under **Source Repository** select:
   - **Git Repository URL**: `https://github.com/rossoctl/examples`
   - **Git Branch or Tag**: `main`
   - **Select Agent**: `Weather Service Agent`
   - **Source Subfolder**: `a2a/weather_service`

5. **Protocol**: `A2A`

6. **Framework**: `LangGraph`

7. **Workload Type** select `Deployment`.

8. **Enable AuthBridge sidecar injection** is checked by default for agents.
   Leave it checked.

9. **Enable SPIRE identity (spiffe-helper sidecar)** is checked by default.
   Leave it checked.

10. Under **Port Configuration**, set **Service Port** to `8080` and **Target Port** to `8000`

11. Under **Environment Variables**, click **Import from File/URL**,
    Select **From URL** and provide the **URL** from this repo:
    - For Ollama: `https://raw.githubusercontent.com/rossoctl/examples/refs/heads/main/a2a/weather_service/.env.ollama`
    - For OpenAI: `https://raw.githubusercontent.com/rossoctl/examples/refs/heads/main/a2a/weather_service/.env.openai`
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
    > If you had empty string for "openaiApiKey:" in .secret_values.yaml
    > the secret with empty string is already created so delete it
    > if you get "error: failed to create secret secrets "openai-secret" already exists"
    > ```bash
    > kubectl delete secret openai-secret -n team1
    > ```

12. **(Ollama only)** If using Ollama as your LLM provider, expand
    **AuthBridge Advanced Configuration** and enter `11434` in the
    **Outbound Ports to Exclude** field. This prevents AuthBridge from
    intercepting traffic to Ollama on the host machine. OpenAI users can
    skip this — HTTPS traffic passes through via TLS passthrough.

13. Click **Build & Deploy Agent**.

Wait for the Shipwright build to complete and the deployment to become ready.

---

## Step 3: Verify the Deployment

### Check pod status

```bash
kubectl get pods -n team1
```

Expected output (Step 2 defaults — `proxy-sidecar` mode):

```text
NAME                               READY   STATUS    RESTARTS   AGE
weather-service-58768bdb67-xxxxx   2/2     Running   0          2m
weather-tool-7f8c9d6b44-yyyyy     1/1     Running   0          5m
```

> **Note:** AuthBridge ships as a single combined sidecar image (since
> cortex#411). `weather-service` runs `agent` + the combined
> AuthBridge sidecar — `2/2` — regardless of whether SPIRE identity is
> enabled. The `spiffe-helper` is bundled inside the combined image and
> activated per workload via `SPIRE_ENABLED` (driven by the
> `rossoctl.io/spire: enabled` label); it is not a separate container. In
> `envoy-sidecar` mode the pod is still `2/2` (`agent` + the combined
> sidecar) plus a `proxy-init` init container for iptables setup. See the
> [AuthBridge deployment guide](https://github.com/rossoctl/rossoctl/blob/main/docs/authbridge/deployment-guide.md)
> for the full mode/label reference.

### Verify injected containers

```bash
kubectl get pod -n team1 -l app.kubernetes.io/name=weather-service -o jsonpath='{.items[0].spec.containers[*].name}'
```

Expected (Step 2 defaults — `proxy-sidecar` mode):

```text
agent authbridge-proxy
```

Or, in `envoy-sidecar` mode:

```text
agent envoy-proxy
```

The container *names* don't change with SPIRE — `spiffe-helper` runs inside
the combined sidecar, not as a separate container.

### Check operator-managed client registration

After cortex#411 / operator#361, client registration runs
in the operator (outside the workload pod). Verify the resulting
Secret is mounted into the agent's sidecar:

```bash
kubectl get pod -n team1 -l app.kubernetes.io/name=weather-service \
  -o jsonpath='{.items[0].spec.volumes[?(@.secret)].secret.secretName}'
# Expect a Secret name starting with: rossoctl-keycloak-client-credentials-
```

Inspect the actual SPIFFE-derived client ID written to /shared/client-id.txt:

```bash
SIDECAR=$(kubectl get pod -n team1 -l app.kubernetes.io/name=weather-service \
  -o jsonpath='{.items[0].spec.containers[*].name}' | tr ' ' '\n' \
  | grep -E '^(authbridge-proxy|envoy-proxy)$' | head -1)
kubectl exec deploy/weather-service -n team1 -c "$SIDECAR" -- cat /shared/client-id.txt
```

Expected — just the SPIFFE ID (the `Created Keycloak client …` log line
now lives in the operator's `rossoctl-controller-manager`
deployment in `rossoctl-system`, not the workload pod):

```
spiffe://localtest.me/ns/team1/sa/weather-service
```

To follow the operator-side registration:

```bash
kubectl logs -n rossoctl-system deployment/rossoctl-controller-manager \
  | grep -i clientregistration | tail -20
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

If you set the **Outbound Ports to Exclude** field to `11434` during import
(Step 2, item 12), this is already handled and no patch is needed.

Otherwise, add the annotation after deployment:

```bash
kubectl patch deployment weather-service -n team1 --type=merge -p='
{"spec":{"template":{"metadata":{"annotations":{"rossoctl.io/outbound-ports-exclude":"11434"}}}}}'
kubectl rollout status deployment/weather-service -n team1 --timeout=120s
```

### Option B: OpenAI

Verify the OpenAI secret exists (see the prerequisite note in
[Step 2](#step-2-import-the-weather-agent-via-rossoctl-ui)):

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

## Step 5: Chat via Rossoctl UI

1. Navigate to the **Agent Catalog** in the Rossoctl UI.
2. Select the `team1` namespace.
3. Under **Available Agents**, select `weather-service` and click **View Details**.
4. Verify the **Agent Card** is visible (this confirms the agent is running and
   the `/.well-known/*` bypass is working).
5. Use the **Chat** panel to send a message, e.g. "What is the weather in New York?".
6. The agent should respond with current weather information.

> **Troubleshooting:** If UI chat returns a `401`, verify that both the UI and
> AuthBridge are configured against the same `rossoctl` realm. You can also use
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

The `/.well-known/agent.json` endpoint is publicly accessible — authbridge
bypasses JWT validation for `/.well-known/*`, `/healthz`, `/readyz`,
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
# Get a Keycloak admin token from the rossoctl realm
ADMIN_TOKEN=$(curl -s http://keycloak-service.keycloak.svc:8080/realms/rossoctl/protocol/openid-connect/token \
  -d "grant_type=password" \
  -d "client_id=admin-cli" \
  -d "username=admin" \
  -d "password=admin" | jq -r ".access_token")

echo "Admin token length: ${#ADMIN_TOKEN}"

# Look up the agent's client in the rossoctl realm
SPIFFE_ID="spiffe://localtest.me/ns/team1/sa/weather-service"
CLIENTS=$(curl -s -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://keycloak-service.keycloak.svc:8080/admin/realms/rossoctl/clients" \
  --data-urlencode "clientId=$SPIFFE_ID" --get)
CLIENT_ID=$(echo "$CLIENTS" | jq -r ".[0].clientId")
CLIENT_SECRET=$(echo "$CLIENTS" | jq -r ".[0].secret")

echo "Client ID:     $CLIENT_ID"
echo "Secret length: ${#CLIENT_SECRET}"

# Get an OAuth token for the agent
TOKEN=$(curl -s -X POST \
  "http://keycloak-service.keycloak.svc:8080/realms/rossoctl/protocol/openid-connect/token" \
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

### 6e. Verify AuthBridge Logs (Inbound)

Check the authbridge logs to confirm inbound validation is working:

```bash
# For envoy-sidecar mode:
kubectl logs deployment/weather-service -n team1 -c envoy-proxy 2>&1 | grep "inbound authorized"

# For proxy-sidecar mode:
kubectl logs deployment/weather-service -n team1 -c authbridge-proxy 2>&1 | grep "inbound authorized"
```

Expected:

```
level=INFO msg="inbound authorized" subject=... clientID=rossoctl
```

> **Tip:** For detailed debug logs (audience, scopes, request path), enable debug
> logging — see [Debug Logging](#debug-logging) below.

### Clean Up Test Client

```bash
kubectl delete pod test-client -n team1 --ignore-not-found
```

---

## Troubleshooting

### Invalid Client or Invalid Client Credentials

**Symptom:** `{"error":"invalid_client","error_description":"Invalid client or Invalid client credentials"}`

**Cause:** The `keycloak-admin-secret` Secret or `authbridge-config` ConfigMap was missing
or incorrect at startup, so the operator's `ClientRegistrationReconciler` couldn't reach
Keycloak to register the client.

**Fix:**

```bash
# 1. Verify the keycloak-admin-secret exists (operator 0.2+ keeps it in rossoctl-system)
kubectl get secret keycloak-admin-secret -n rossoctl-system

# 2. Verify the authbridge-config ConfigMap has the correct realm
kubectl get configmap authbridge-config -n team1 -o jsonpath='{.data.KEYCLOAK_REALM}'
# Should show: rossoctl

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

If you see `30s` values instead of `300s`, reinstall Rossoctl (the installer
creates the correct defaults) and restart the agent:

```bash
kubectl rollout restart deployment/weather-service -n team1
```

### Agent card not available in the UI

This demo normally does not create `authproxy-routes`. If the UI still cannot load the
agent card while the agent container responds on port 8000, Envoy’s ext_proc path is
likely broken—often due to **invalid `authproxy-routes` YAML** left over from another
workflow or namespace reuse. Follow **Agent card not available** in the
[GitHub Issue Agent UI demo](../github-issue/demo-ui.md#agent-card-not-available-in-the-ui)
(check the AuthBridge sidecar logs — `authbridge-proxy` in proxy-sidecar mode,
`envoy-proxy` in envoy-sidecar mode — and fix or remove `authproxy-routes`
as described there).

### Agent Pod Not Starting

**Symptom:** Pod shows 1/2 (or 0/2) containers ready

**Fix:** Check the agent and the AuthBridge sidecar:

```bash
# AuthBridge sidecar — name depends on resolved mode:
#   proxy-sidecar (default): authbridge-proxy
#   envoy-sidecar:           envoy-proxy
kubectl logs deployment/weather-service -n team1 -c authbridge-proxy
kubectl logs deployment/weather-service -n team1 -c agent

# If the issue is operator-managed client registration not finishing,
# the workload pod waits on /shared/client-{id,secret}.txt. Inspect:
kubectl logs -n rossoctl-system deployment/rossoctl-controller-manager \
  | grep -iE "clientregistration|weather-service" | tail -20
```

---

## Switching modes

After operator#361 the cluster default is **proxy-sidecar**
(forward + reverse HTTP proxies, no Envoy, no iptables, no init container).
The operator resolves mode per workload from this chain:

1. `AgentRuntime.Spec.AuthBridgeMode` on the workload's CR (canonical).
2. `mode:` field on the namespace-level `authbridge-runtime-config` ConfigMap.
3. Deprecated `rossoctl.io/authbridge-mode` pod annotation (still honored).
4. Cluster default — `proxy-sidecar`.

### Switch to envoy-sidecar mode

Set it on the AgentRuntime CR (canonical surface):

```bash
kubectl patch agentruntime weather-service -n team1 --type=merge \
  -p '{"spec":{"authBridgeMode":"envoy-sidecar"}}'
kubectl rollout restart deployment weather-service -n team1
kubectl rollout status deployment weather-service -n team1 --timeout=120s
```

Or, on workloads without an AgentRuntime CR, the deprecated annotation:

```bash
kubectl patch deployment weather-service -n team1 --type=merge \
  -p '{"spec":{"template":{"metadata":{"annotations":{"rossoctl.io/authbridge-mode":"envoy-sidecar"}}}}}'
kubectl rollout status deployment weather-service -n team1 --timeout=120s
```

### Verify the resolved mode

```bash
kubectl get pod -n team1 -l app.kubernetes.io/name=weather-service \
  -o jsonpath='{range .spec.containers[*]}{.name}{"\n"}{end}'
# Expect:
#   proxy-sidecar (default): "agent" + "authbridge-proxy"
#   envoy-sidecar:           "agent" + "envoy-proxy" (plus a "proxy-init" init container)
```

### Switch back to proxy-sidecar (default)

Drop the override:

```bash
kubectl patch agentruntime weather-service -n team1 --type=json \
  -p '[{"op":"remove","path":"/spec/authBridgeMode"}]'
kubectl rollout restart deployment weather-service -n team1
```

### Key differences

| | proxy-sidecar (default) | envoy-sidecar |
|---|---|---|
| Image | `authbridge` (combined) | `authbridge-envoy` (combined) |
| Traffic interception | HTTP_PROXY env vars | iptables + Envoy |
| Init container | None | `proxy-init` (NET_ADMIN) |
| Container name | `authbridge-proxy` | `envoy-proxy` |
| Ollama port exclusion | Not needed | Required (annotation) |

> **Note:** Proxy-sidecar mode requires the agent to read the `PORT` env var.
> All agents in [rossoctl/examples](https://github.com/rossoctl/examples)
> support this since v0.1.0-alpha.11.

---

## Debug Logging

AuthBridge supports dynamic log-level switching for debugging auth failures
without redeploying.

### Toggle debug logging at runtime

Send `SIGUSR1` to the authbridge process. The container image is minimal (no
standalone `kill` or `grep` binaries), so use bash builtins to locate the PID:

```bash
# For envoy-sidecar mode:
kubectl exec deploy/weather-service -n team1 -c envoy-proxy -- \
  bash -c 'for f in /proc/[0-9]*/cmdline; do [ -r "$f" ] || continue; c=$(<"$f"); [[ "$c" == /usr/local/bin/authbridge* ]] && kill -USR1 "${f//[!0-9]/}" && break; done'

# For proxy-sidecar mode:
kubectl exec deploy/weather-service -n team1 -c authbridge-proxy -- \
  bash -c 'for f in /proc/[0-9]*/cmdline; do [ -r "$f" ] || continue; c=$(<"$f"); [[ "$c" == /usr/local/bin/authbridge* ]] && kill -USR1 "${f//[!0-9]/}" && break; done'
```

Send `SIGUSR1` again to toggle back to INFO level.

### What debug logs show

At DEBUG level, every auth decision logs full context:

- **Inbound**: request path, expected audience, token audience, scopes, subject
- **Outbound**: target host, audience, scopes, exchange success/failure details
- **Bypass**: which paths were skipped
- **Cache**: hit/miss for token exchange results

Example (Info lines are always visible; Debug lines appear after SIGUSR1 toggle):

```
level=DEBUG msg="validating inbound JWT" path=/ expectedAudience=spiffe://localtest.me/ns/team1/sa/weather-service
level=INFO  msg="inbound authorized" subject=... clientID=rossoctl
level=DEBUG msg="inbound authorized details" path=/ audience="[spiffe://...]" scopes="[openid ...]"
level=INFO  msg="outbound passthrough" host=weather-tool-mcp.team1.svc.cluster.local:8000 reason="no matching route"
```

---

## Cleanup

### Via Rossoctl UI

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
- **AuthBridge Binary**: See the [AuthBridge README](../../cmd/authbridge/README.md) for inbound
  JWT validation and outbound token exchange internals
- **Token-Exchange Routes**: See the [routes-configuration guide](../token-exchange-routes/README.md) for
  route-based token exchange to multiple tool services
- **AuthBridge Overview**: See the [AuthBridge README](../../README.md) for architecture details
