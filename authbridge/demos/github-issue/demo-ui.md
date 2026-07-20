# GitHub Issue Agent Demo with AuthBridge (UI Deployment)

This guide walks through deploying the **GitHub Issue Agent** with **AuthBridge**
using the **Rossoctl UI** for agent and tool deployment. Infrastructure setup
(webhook, Keycloak, ConfigMaps) is done via CLI, while the agent and tool are
imported and deployed through the Rossoctl dashboard.

For a fully manual deployment using only `kubectl`, see [demo-manual.md](demo-manual.md).

For a simpler getting-started demo that doesn't require token exchange, see the
[Weather Agent demo](../weather-agent/demo-ui.md).

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
│  │  ┌─────────────────┐  ┌────────────────────────────────────────────┐   │   │
│  │  │ git-issue-agent │  │  AuthBridge sidecar (combined image)        │   │   │
│  │  │  (A2A agent,    │  │  Container name depends on resolved mode:   │   │   │
│  │  │   port 8000)    │  │    proxy-sidecar (default): authbridge-proxy│   │   │
│  │  └─────────────────┘  │    envoy-sidecar:           envoy-proxy     │   │   │
│  │                       │                                              │   │   │
│  │                       │  Inbound:                                    │   │   │
│  │                       │    - Validates JWT (signature + issuer +     │   │   │
│  │                       │      audience via JWKS)                      │   │   │
│  │                       │    - Returns 401 for invalid/missing tokens  │   │   │
│  │                       │  Outbound:                                   │   │   │
│  │                       │    - HTTP: Exchanges token via Keycloak      │   │   │
│  │                       │      → aud: github-tool                      │   │   │
│  │                       │    - HTTPS: TLS passthrough                  │   │   │
│  │                       │                                              │   │   │
│  │                       │  spiffe-helper bundled inside the image     │   │   │
│  │                       │  (gated by SPIRE_ENABLED).                   │   │   │
│  │                       │  Keycloak client registration is             │   │   │
│  │                       │  operator-managed; the resulting Secret      │   │   │
│  │                       │  is mounted at /shared/client-{id,secret}.txt│   │   │
│  │                       └────────────────────────────────────────────┘   │   │
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
│  │  Provides SPIFFE     │          │  - rossoctl realm     │                      │
│  │  identities (SVIDs)  │          │  - token exchange    │                      │
│  └──────────────────────┘          └──────────────────────┘                      │
└──────────────────────────────────────────────────────────────────────────────────┘
```

## Prerequisites

Ensure you have completed the Rossoctl platform setup as described in the
[Installation Guide](https://github.com/rossoctl/rossoctl/blob/main/docs/install.md),
including the Rossoctl UI.

You should also have:
- The [cortex](https://github.com/rossoctl/cortex) repo cloned
- The Rossoctl UI running at `http://rossoctl-ui.localtest.me:8080`
- Python 3.10+ with `venv` support
- An LLM provider — either **Ollama** with `ibm/granite4:latest` (or another model)
  or an **OpenAI API key** (recommended for reliable function calling;
  see [agent-examples#173](https://github.com/rossoctl/examples/issues/173))
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

### Rossoctl version notes (UI import and AuthBridge)

- **Outbound routes from the UI (Step 5, item 12):** The API must write `authproxy-routes`
  `routes.yaml` as a **YAML list** of route objects (same shape as
  [k8s/configmaps.yaml](k8s/configmaps.yaml)). Older Rossoctl backends wrapped routes in a
  top-level `routes:` **map**, which **authbridge cannot parse**, so ext_proc
  never starts, Envoy returns **500** through the Service, and the UI shows **Agent card not
  available**. Use a build that includes [rossoctl/rossoctl#1194](https://github.com/rossoctl/rossoctl/pull/1194)
  ([#1195](https://github.com/rossoctl/rossoctl/issues/1195)), or use **Step 2 Option B**
  (`kubectl apply -f demos/github-issue/k8s/configmaps.yaml`) and skip adding duplicate
  routes in the UI.

- **Finalize after Shipwright:** If the build succeeds but the agent **Deployment never
  appears** and backend logs show **403** on ConfigMaps, upgrade the Helm chart to a
  release that includes [rossoctl/rossoctl#1192](https://github.com/rossoctl/rossoctl/pull/1192)
  ([#1191](https://github.com/rossoctl/rossoctl/issues/1191)).

---

## Step 1: Configure Keycloak

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
python -m venv venv
source venv/bin/activate
pip install --upgrade pip
pip install -r requirements.txt

# Run the Keycloak setup for this demo
python demos/github-issue/setup_keycloak.py
```

This creates:

| Resource | Name | Purpose |
|----------|------|---------|
| **Realm** | `rossoctl` | Keycloak realm for the demo |
| **Client** | `github-tool` | Target audience for token exchange |
| **Scope** | `agent-team1-git-issue-agent-aud` | Realm DEFAULT — auto-adds Agent's SPIFFE ID to all tokens |
| **Scope** | `github-tool-aud` | Realm OPTIONAL — for exchanged tokens targeting the tool |
| **Scope** | `github-full-access` | Realm OPTIONAL — for privileged GitHub API access |
| **User** | `alice` (password: `alice123`) | Regular user — public access |
| **User** | `bob` (password: `bob123`) | Demo user — request with `scope=github-full-access` for privileged access |

---

## Step 2: Apply Demo ConfigMaps

The Rossoctl installer creates default ConfigMaps (`authbridge-config`,
`spiffe-helper-config`, `envoy-config`) and the `keycloak-admin-secret` Secret
in the target namespace with the correct `rossoctl` realm settings and 300s Envoy
timeouts. No manual secret creation is needed for this demo.

> If your Keycloak admin credentials differ from the default (`admin`/`admin`),
> update the secret:
> ```bash
> kubectl create secret generic keycloak-admin-secret -n team1 \
>   --from-literal=KEYCLOAK_ADMIN_USERNAME=<your-admin-user> \
>   --from-literal=KEYCLOAK_ADMIN_PASSWORD=<your-admin-password> \
>   --dry-run=client -o yaml | kubectl apply -f -
> ```

The `authproxy-routes` ConfigMap (outbound routing rules for token exchange)
can be configured in two ways:

**Option A (recommended): Via the Rossoctl UI** — configure outbound routing
rules directly during agent import in Step 5 (item 12). No manual ConfigMap
creation needed. Requires a Rossoctl backend that writes list-shaped `routes.yaml`
(see [Rossoctl version notes](#rossoctl-version-notes-ui-import-and-authbridge) above);
otherwise use Option B or upgrade.

**Option B: Via kubectl** — apply the demo-specific ConfigMap that configures
per-route token exchange (target audience and scopes for the `github-tool` host):

```bash
cd authbridge

# Apply demo ConfigMaps (authproxy-routes)
kubectl apply -f demos/github-issue/k8s/configmaps.yaml
```

---

## Step 3: Create the GitHub Tool Secrets

The GitHub tool needs PAT tokens to access the GitHub API. Create a Kubernetes secret
with your tokens before importing the tool:

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

## Step 4: Import the GitHub Tool via Rossoctl UI

1. Navigate to [Import Tool](http://rossoctl-ui.localtest.me:8080/tools/import)
   in the Rossoctl UI.

2. In the **Namespace** drop-down, choose `team1`.

3. Select **Build from Source** as the deployment method.

4. Under **Source Code** select:
   - **Git Repository URL**: `https://github.com/rossoctl/examples`
   - **Git Branch or Tag**: `main`
   - **Select Tool**: `GitHub Tool`
   - **Source Subfolder**: `mcp/github_tool`

5. **Workload Type** select `Deployment`

6. Set **MCP Transport Protocol** to `streamable HTTP`

7. **Enable AuthBridge sidecar injection** is unchecked by default for tools.
   Leave it unchecked.

8. **Enable SPIRE identity (spiffe-helper sidecar)** should be **unchecked**.

   > The GitHub tool does not need AuthBridge sidecars — it validates incoming tokens
   > directly using its own JWKS logic. Injecting sidecars would cause a port 9090
   > conflict between the tool's MCP broker and the authbridge gRPC server.

9. Under **Port Configuration**, set **Service Port** to `9090` and **Target Port** to `9090`

   > The tool binary listens on port 9090. The agent's `MCP_URL` connects to
   > `http://github-tool-mcp:9090/mcp`, so both the service port and target port
   > must be 9090 to match.

10. Under **Environment Variables**, click **Import from File/URL**,
    Select **From URL** and provide the `.env` file from this repo:
    - **URL** `https://raw.githubusercontent.com/rossoctl/examples/refs/heads/main/mcp/github_tool/.env.authbridge`
    - Click **Fetch & Parse** — this populates all environment variables, including
      Secret references for the PAT tokens and direct values for Keycloak settings.
    - Click **Import** to set all the env. variables.

    The imported variables will show three **Secret** type entries referencing
    `github-tool-secrets` and three **Direct Value** entries for Keycloak configuration.
    No manual editing is needed.

    > **Tip:** You can also upload the file directly from your local system.

11. Click **Build & Deploy New Tool**.

You will be redirected to a **Build Progress** page where you can monitor the
Shipwright build. Wait for it to complete.

### Verify the tool is reachable

Confirm the tool service port is correct and the tool responds:

```bash
kubectl run test-mcp --image=curlimages/curl -n team1 --restart=Never --rm -it -- \
  curl -s -o /dev/null -w "%{http_code}" --max-time 5 http://github-tool-mcp:9090/mcp
```

Expected:

```
200 (SSE connection, may timeout after 5s — that's OK)
```

---

## Step 5: Import the GitHub Issue Agent via Rossoctl UI

1. Navigate to [Import Agent](http://rossoctl-ui.localtest.me:8080/agents/import)
   in the Rossoctl UI.

2. In the **Namespace** drop-down, choose `team1`.

3. Select **Build from Source** as the deployment method.

4. Under **Source Repository** select:
   - **Git Repository URL**: `https://github.com/rossoctl/examples`
   - **Git Branch or Tag**: `main`
   - **Select Agent**: `Git Issue Agent`
   - **Source Subfolder**: `a2a/git_issue_agent`

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
    - For Ollama: `https://raw.githubusercontent.com/rossoctl/examples/refs/heads/main/a2a/git_issue_agent/.env.ollama`
    - For OpenAI: `https://raw.githubusercontent.com/rossoctl/examples/refs/heads/main/a2a/git_issue_agent/.env.openai`
    - Click **Fetch & Parse** — this populates all environment variables including
     LLM settings, `MCP_URL`, and `JWKS_URI`. No manual editing is needed.
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

12. Expand **Outbound Routing Rules** and add a route for the GitHub tool:

    | Host | Target Audience | Token Scopes |
    |------|----------------|--------------|
    | `github-tool-mcp` | `github-tool` | `openid github-tool-aud github-full-access` |

    This tells AuthBridge to exchange tokens when the agent calls the GitHub
    tool service, requesting the correct audience and scopes for access control.

    > **Note:** This replaces the manual `kubectl apply` of `authproxy-routes`
    > ConfigMap from Step 2. If you already applied the ConfigMap in Step 2,
    > you can skip this — the UI-created routes will merge with existing ones.

    > **Troubleshooting:** If **Outbound Routing Rules** is missing, stays collapsed,
    > or does not respond when you click it, your Rossoctl UI build may not include this
    > control yet. Use **Step 2, Option B** instead:
    > `kubectl apply -f demos/github-issue/k8s/configmaps.yaml` from the `authbridge`
    > directory (same host, audience, and scopes as the table above). Then continue
    > with item 13. Confirm **Enable AuthBridge sidecar injection** (item 8) is still
    > checked before deploying.

13. **(Ollama only)** If using Ollama, expand **AuthBridge Advanced Configuration**
    and enter `11434` in the **Outbound Ports to Exclude** field.

14. Click **Build & Deploy Agent**.

Wait for the Shipwright build to complete and the deployment to become ready.

---

## Step 6: Verify the Deployment

### Check pod status

```bash
kubectl get pods -n team1
```

Expected output (after cortex#411 / operator#361:
one combined AuthBridge sidecar, registration is operator-managed):

```
NAME                               READY   STATUS    RESTARTS   AGE
git-issue-agent-77fc7dc6cd-xxxxx   2/2     Running   0          2m
github-tool-7f8c9d6b44-yyyyy      1/1     Running   0          5m
```

> **Note:** The agent pod shows **2/2** — the agent container plus the
> AuthBridge sidecar. In envoy-sidecar mode you'll also see a
> `proxy-init` init container that exits after setting up iptables.
> Shipwright **BuildRun** pods may still appear as `Completed` with a
> different ready count.

### Verify injected containers

```bash
kubectl get pod -n team1 -l app.kubernetes.io/name=git-issue-agent \
  -o jsonpath='{.items[0].spec.containers[*].name}'
```

Expected (proxy-sidecar mode, the cluster default):

```
agent authbridge-proxy
```

Or, in envoy-sidecar mode:

```
agent envoy-proxy
```

### Check operator-managed client registration

After operator#361 client registration runs in the operator
(outside the workload pod). Verify the resulting Secret is mounted
into the agent's sidecar:

```bash
kubectl get pod -n team1 -l app.kubernetes.io/name=git-issue-agent \
  -o jsonpath='{.items[0].spec.volumes[?(@.secret)].secret.secretName}'
# Expect a Secret name starting with: rossoctl-keycloak-client-credentials-
```

Follow the operator-side reconciler:

```bash
kubectl logs -n rossoctl-system deployment/rossoctl-controller-manager \
  | grep -iE "clientregistration|git-issue-agent" | tail -20
```

Expected (operator log lines, exact format depends on the operator's
log format):

```
ClientRegistrationReconciler: ensured Keycloak client
  spiffe://localtest.me/ns/team1/sa/git-issue-agent
ClientRegistrationReconciler: wrote Secret
  rossoctl-keycloak-client-credentials-<hex8>
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

<!-- WORKAROUND: Remove this warning note once rossoctl/examples#129 is fixed. -->

> **These warnings are expected and harmless.** The agent's built-in auth code
> probes for SVID and client-secret files at startup. With AuthBridge, these files
> are produced and consumed inside the AuthBridge sidecar (and the operator's
> ClientRegistrationReconciler), not by the agent container directly. The agent
> falls back to JWKS-based JWT validation (`JWKS_URI is set`), which is the
> correct behavior — AuthBridge handles inbound JWT validation and outbound
> token exchange on behalf of the agent.
> These warnings will be removed once the agent's built-in auth logic is cleaned up
> ([rossoctl/examples#129](https://github.com/rossoctl/examples/issues/129)).

### Check the service endpoint

```bash
kubectl get svc -n team1 | grep git-issue-agent
```

Expected:

```
git-issue-agent   ClusterIP   10.96.x.x   <none>   8080/TCP   5m
```

The service maps **port 8080** to the agent's internal port 8000.

---

## Step 7: Verify LLM Provider

The agent uses an LLM for inference. Follow the section that matches your chosen
provider.

> **Recommendation:** OpenAI (`gpt-4o-mini` or similar) is recommended for the most
> reliable function-calling experience. Local Ollama models may produce text-based
> tool outputs instead of structured function calls with `crewai 1.10.1`
> ([rossoctl/examples#173](https://github.com/rossoctl/examples/issues/173)).

### Option A: Ollama (local models)

Verify Ollama is running:

```bash
ollama list
```

You should see `ibm/granite4:latest` (or whichever model you configured) on the list.
If Ollama is not running, start it in a separate terminal (`ollama serve`) and ensure the
model is pulled (`ollama pull ibm/granite4:latest`).

> **Note:** The `.env.ollama` file defaults to `LLM_API_BASE=http://host.docker.internal:11434`,
> which reaches Ollama running on your host machine via the Kind/Docker Desktop gateway.
> If you deploy Ollama inside the cluster instead, patch the agent:
> ```bash
> kubectl set env deployment/git-issue-agent -n team1 -c agent \
>   LLM_API_BASE="http://ollama.ollama.svc:11434"
> ```

#### Ollama Port Exclusion

AuthBridge's `proxy-init` init container redirects traffic through Envoy. By
default, only port 8080 (Keycloak) is excluded. Ollama traffic on port 11434
gets intercepted, which corrupts LLM streaming responses.

If you set the **Outbound Ports to Exclude** field to `11434` during import
(Step 5, item 13), this is already handled and no patch is needed.

Otherwise, add the annotation after deployment:

```bash
kubectl patch deployment git-issue-agent -n team1 --type=merge -p='
{"spec":{"template":{"metadata":{"annotations":{"rossoctl.io/outbound-ports-exclude":"11434"}}}}}'
kubectl rollout status deployment/git-issue-agent -n team1 --timeout=120s
```

### Option B: OpenAI

Verify the OpenAI secret exists (see the prerequisite note in
[Step 5](#step-5-import-the-github-issue-agent-via-rossoctl-ui)):

```bash
kubectl get secret openai-secret -n team1
```

Verify the agent has the correct environment variables:

```bash
kubectl exec deployment/git-issue-agent -n team1 -c agent -- env | grep -E "LLM_|OPENAI"
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

## Step 8: Chat via Rossoctl UI

1. Navigate to the **Agent Catalog** in the Rossoctl UI.
2. Select the `team1` namespace.
3. Under **Available Agents**, select `git-issue-agent` and click **View Details**.
4. Verify the **Agent Card** is visible (this confirms the agent is running and
   the `/.well-known/*` bypass is working).
5. Use the **Chat** panel to send a message, e.g. "List 10 open issues in rossoctl/rossoctl repo".
6. The agent should respond with a list of GitHub issues.

> **Intermittent short responses:** Sometimes the model returns only a CrewAI-style
> planning line (e.g. `Thought: … Action: list_issues Action Input: …`) and stops
> **before** the GitHub MCP tool runs, so you do not get a real issue list. This is
> **not** Rossoctl UI caching or streaming truncation—the same text can appear from
> `message/send` in [Step 9d](#9d-end-to-end-test-with-valid-token). **Workaround for
> the demo:** send the same prompt again (in the UI or via `curl`) a few times until
> you see a full answer. OpenAI models usually behave more consistently than local
> Ollama for tool use; see [agent-examples#173](https://github.com/rossoctl/examples/issues/173).
> Deeper diagnosis: [Partial response (Thought and Action only)](#partial-response-thought-and-action-only-no-issue-list).

> **Troubleshooting:** If UI chat returns a `401`, verify that both the UI and
> AuthBridge are configured against the same `rossoctl` realm. You can also use
> [Step 9: Test via CLI](#step-9-test-via-cli) to test the full AuthBridge flow
> independently.

---

## Step 9: Test via CLI

Test the AuthBridge flow from the command line to verify inbound validation and
token exchange using a `rossoctl`-realm token.

### Setup

```bash
# Start a test client pod
kubectl run test-client --image=nicolaka/netshoot -n team1 --restart=Never -- sleep 3600
kubectl wait --for=condition=ready pod/test-client -n team1 --timeout=30s
```

### 9a. Agent Card - Public Endpoint (No Token Required)

The `/.well-known/agent.json` endpoint is publicly accessible — authbridge
[bypasses JWT validation](https://github.com/rossoctl/cortex/pull/133)
for `/.well-known/*`, `/healthz`, `/readyz`, and `/livez` by default:

```bash
kubectl exec test-client -n team1 -- curl -s \
  http://git-issue-agent:8080/.well-known/agent.json | jq .name
```

Expected:

```
"Github issue agent"
```

### 9b. Inbound Rejection - No Token

Non-public endpoints require a valid JWT:

```bash
kubectl exec test-client -n team1 -- curl -s \
  http://git-issue-agent:8080/
```

Expected:

```
{"error":"unauthorized","message":"missing Authorization header"}
```

### 9c. Inbound Rejection - Invalid Token (Signature Check)

A malformed or tampered token fails the JWKS signature check:

```bash
kubectl exec test-client -n team1 -- curl -s \
  -H "Authorization: Bearer invalid-token" \
  http://git-issue-agent:8080/
```

Expected:

```
{"error":"unauthorized","message":"token validation failed: failed to parse/validate token: ..."}
```

### 9d. End-to-End Test with Valid Token

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

# Look up the agent's client in the rossoctl realm.
# The client ID is the SPIFFE ID (URL-encoded in the query parameter).
SPIFFE_ID="spiffe://localtest.me/ns/team1/sa/git-issue-agent"
CLIENTS=$(curl -s -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://keycloak-service.keycloak.svc:8080/admin/realms/rossoctl/clients" \
  --data-urlencode "clientId=$SPIFFE_ID" --get)
INTERNAL_ID=$(echo "$CLIENTS" | jq -r ".[0].id")
CLIENT_ID=$(echo "$CLIENTS" | jq -r ".[0].clientId")

echo "Internal ID:   $INTERNAL_ID"
echo "Client ID:     $CLIENT_ID"

# Get the client secret (extract directly from the client listing;
# the /client-secret endpoint may return null for auto-registered clients)
CLIENT_SECRET=$(echo "$CLIENTS" | jq -r ".[0].secret")

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
  -X POST http://git-issue-agent:8080/ \
  -d '{
    "jsonrpc": "2.0",
    "id": "test-1",
    "method": "message/send",
    "params": {
      "message": {
        "role": "user",
        "messageId": "msg-001",
        "parts": [{"type": "text", "text": "List 10 open issues in rossoctl/rossoctl repo"}]
      }
    }
  }' | jq
```

> **Same intermittent behavior as UI chat:** If `jq` shows an artifact whose text is
> only `Thought:` / `Action:` / `Action Input:` (no issue list or `Final Answer:`),
> the run likely ended before the MCP tool executed. Run the same `curl` again a few
> times, or see [Partial response (Thought and Action only)](#partial-response-thought-and-action-only-no-issue-list).

Exit the pod when done:

```bash
exit
```

### 9e. Verify AuthProxy Logs (Inbound + Outbound)

Check the AuthBridge sidecar logs to confirm both inbound validation and
outbound token exchange are working. The combined sidecar handles both
directions; the container name depends on the resolved AuthBridge mode
(`authbridge-proxy` for proxy-sidecar, `envoy-proxy` for envoy-sidecar).

```bash
SIDECAR=$(kubectl get pod -n team1 -l app.kubernetes.io/name=git-issue-agent \
  -o jsonpath='{.items[0].spec.containers[*].name}' | tr ' ' '\n' \
  | grep -E '^(authbridge-proxy|envoy-proxy)$' | head -1)
```

**Inbound validation logs:**

```bash
kubectl logs deployment/git-issue-agent -n team1 -c "$SIDECAR" 2>&1 | grep "\[Inbound\]"
```

Expected:

```
[Inbound] Token validated - issuer: http://keycloak.localtest.me:8080/realms/rossoctl, audience: [spiffe://localtest.me/ns/team1/sa/git-issue-agent ...]
[Inbound] JWT validation succeeded, forwarding request
```

**Outbound token exchange logs:**

```bash
kubectl logs deployment/git-issue-agent -n team1 -c "$SIDECAR" 2>&1 | grep "^2026/" | grep "\[Token Exchange\]"
```

Expected:

```
[Token Exchange] Token URL: http://keycloak-service.keycloak.svc:8080/realms/rossoctl/protocol/openid-connect/token
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

<!-- WORKAROUND: Remove this note once cortex#139 is implemented.
     The full scope-forwarding feature in authbridge is required for this step to work
     end-to-end. Until that lands, the exchanged token always includes github-full-access
     (from the static token_scopes in the authproxy-routes ConfigMap).
     Track: https://github.com/rossoctl/cortex/issues/139 -->

> **Known limitation:** This step requires the authbridge scope forwarding feature
> ([cortex#139](https://github.com/rossoctl/cortex/issues/139)).
> Currently, `token_scopes` in the `authproxy-routes` ConfigMap is static per-route, so
> all exchanged tokens include `github-full-access` regardless of the original user's
> scopes. Once scope forwarding is implemented, Alice's exchanged token will omit
> `github-full-access` while Bob's will include it.

This step demonstrates **scope-based access control**: two users with different
privilege levels get different GitHub API access through the same agent.

| User | Token Scope | Tool PAT Used | Public Repos | Private Repos |
|------|-------------|---------------|:------------:|:-------------:|
| **Alice** | `openid` (no `github-full-access`) | `PUBLIC_ACCESS_PAT` | Yes | No |
| **Bob** | `openid github-full-access` | `PRIVILEGED_ACCESS_PAT` | Yes | Yes |

The flow:
1. User authenticates with Keycloak using `password` grant
2. Alice requests a token **without** `github-full-access`; Bob explicitly requests **with** it
   (`github-full-access` is a realm OPTIONAL scope — Keycloak only includes it when the
   token request contains `scope=openid github-full-access`)
3. AuthBridge exchanges the token — once scope forwarding is implemented
   ([#139](https://github.com/rossoctl/cortex/issues/139)), the exchanged
   token will preserve the scope difference
4. The GitHub tool checks for `REQUIRED_SCOPE` (`github-full-access`) in the exchanged token
5. Tokens with the scope get the privileged PAT; tokens without get the public-only PAT

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
# Helper: decode a JWT payload (base64url → JSON)
jwt_payload() {
  local p=$(echo "$1" | cut -d. -f2 | tr '_-' '/+')
  case $((${#p} % 4)) in 2) p="${p}==" ;; 3) p="${p}=" ;; esac
  echo "$p" | base64 -d
}

ADMIN_TOKEN=$(curl -s http://keycloak-service.keycloak.svc:8080/realms/rossoctl/protocol/openid-connect/token \
  -d "grant_type=password" \
  -d "client_id=admin-cli" \
  -d "username=admin" \
  -d "password=admin" | jq -r ".access_token")

SPIFFE_ID="spiffe://localtest.me/ns/team1/sa/git-issue-agent"
CLIENTS=$(curl -s -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://keycloak-service.keycloak.svc:8080/admin/realms/rossoctl/clients" \
  --data-urlencode "clientId=$SPIFFE_ID" --get)
INTERNAL_ID=$(echo "$CLIENTS" | jq -r ".[0].id")
CLIENT_ID=$(echo "$CLIENTS" | jq -r ".[0].clientId")
CLIENT_SECRET=$(echo "$CLIENTS" | jq -r ".[0].secret")
echo "Client ID: $CLIENT_ID  Secret length: ${#CLIENT_SECRET}"
```

### 10c. Test as Alice (public access only)

Alice authenticates with Keycloak using `password` grant **without** requesting the
`github-full-access` scope. Her token only has the default scopes.

```bash
ALICE_TOKEN=$(curl -s -X POST \
  "http://keycloak-service.keycloak.svc:8080/realms/rossoctl/protocol/openid-connect/token" \
  -d "grant_type=password" \
  -d "username=alice" \
  -d "password=alice123" \
  --data-urlencode "client_id=$CLIENT_ID" \
  --data-urlencode "client_secret=$CLIENT_SECRET" | jq -r ".access_token")

echo "Alice token length: ${#ALICE_TOKEN}"
echo "Alice scopes: $(jwt_payload $ALICE_TOKEN | jq -r '.scope')"
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
        "parts": [{"type": "text", "text": "List 10 open issues in rossoctl/rossoctl repo"}]
      }
    }
  }' | jq '.result.artifacts[0].parts[0].text' | head -5
```

**Alice queries a private repo** (should fail — PUBLIC_ACCESS_PAT cannot access it):

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

> **Expected:** Alice's request for the private repo fails because the GitHub tool
> uses `PUBLIC_ACCESS_PAT`, which has no access to private repositories.

### 10d. Test as Bob (privileged access)

Bob authenticates with `scope=openid github-full-access`, explicitly requesting
the privileged scope:

```bash
BOB_TOKEN=$(curl -s -X POST \
  "http://keycloak-service.keycloak.svc:8080/realms/rossoctl/protocol/openid-connect/token" \
  -d "grant_type=password" \
  -d "username=bob" \
  -d "password=bob123" \
  -d "scope=openid github-full-access" \
  --data-urlencode "client_id=$CLIENT_ID" \
  --data-urlencode "client_secret=$CLIENT_SECRET" | jq -r ".access_token")

echo "Bob token length: ${#BOB_TOKEN}"
echo "Bob scopes: $(jwt_payload $BOB_TOKEN | jq -r '.scope')"
```

**Bob queries the same private repo** (should succeed — PRIVILEGED_ACCESS_PAT has access):

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

> **Expected:** Bob's request succeeds because the exchanged token contains
> `github-full-access`, so the GitHub tool uses `PRIVILEGED_ACCESS_PAT`.

### 10e. Verify scope-based PAT selection in tool logs

Check the GitHub tool logs to confirm that different PATs were selected based on scopes:

```bash
exit
kubectl logs deployment/github-tool -n team1 | grep -E "REQUIRED_SCOPE|scopes"
```

Expected output (two requests, different scope outcomes):

```
This OIDC user has scopes "openid email profile"
The REQUIRED_SCOPE "github-full-access" NOT IN scopes [openid email profile]
This OIDC user has scopes "openid email profile github-full-access"
The REQUIRED_SCOPE "github-full-access" in scopes [openid email profile github-full-access]
```

### 10f. Clean up

```bash
kubectl delete pod test-client -n team1 --ignore-not-found
```

---

## Troubleshooting

### Agent card not available in the UI

**Symptom:** The catalog shows *Agent card not available*; HTTP through the agent
Service returns **500** (often with `server: envoy`); curling
`/.well-known/agent-card.json` or `/.well-known/agent.json` from the **agent**
container on port **8000** still works.

**Cause:** Commonly **authbridge** failed to load `authproxy-routes` (invalid YAML
shape) and never serves ext_proc, so Envoy cannot complete the request.

**Diagnose:**

```bash
# AuthBridge sidecar — name depends on the resolved mode:
SIDECAR=$(kubectl get pod -n team1 -l app.kubernetes.io/name=git-issue-agent \
  -o jsonpath='{.items[0].spec.containers[*].name}' | tr ' ' '\n' \
  | grep -E '^(authbridge-proxy|envoy-proxy)$' | head -1)
kubectl logs deployment/git-issue-agent -n team1 -c "$SIDECAR" 2>&1 \
  | grep -E "failed to load routes|unmarshal"
kubectl get configmap authproxy-routes -n team1 -o jsonpath='{.data.routes\.yaml}{"\n"}'
```

If logs mention `cannot unmarshal !!map into []resolver.yamlRoute`, the file should be a
**list** starting with `- host:` (not a `routes:` map). Match the `routes.yaml` block in
[k8s/configmaps.yaml](k8s/configmaps.yaml), apply it, then restart:

```bash
kubectl rollout restart deployment/git-issue-agent -n team1
```

Longer term, upgrade the Rossoctl backend per [rossoctl/rossoctl#1194](https://github.com/rossoctl/rossoctl/pull/1194).

### Partial response: Thought and Action only (no issue list)

**Symptom:** Chat or `message/send` returns text like
`Thought: … Action: list_issues Action Input: {"owner":"rossoctl",…}` but no formatted
issue list or `Final Answer:`.

**Cause:** The [git issue agent](https://github.com/rossoctl/examples/tree/main/a2a/git_issue_agent)
(CrewAI + MCP) occasionally completes a turn after the model emits a plan **without**
successfully executing the GitHub tool. The GitHub tool log may show `tools/list` (and
`initialize`) but **no** `tools/call` / `list_issues` for that attempt.

**Demo workaround:** Repeat the same prompt or `curl` a few times until the artifact
contains a full answer. Prefer OpenAI over Ollama for stable tool calling when possible
([agent-examples#173](https://github.com/rossoctl/examples/issues/173)).

**Optional check** (after a bad run, adjust time window as needed):

```bash
kubectl logs deployment/github-tool -n team1 --since=5m | grep -E 'tools/list|tools/call|list_issues|Processing request'
```

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
# Should show: rossoctl

# 3. Re-apply the demo ConfigMap and restart
kubectl apply -f demos/github-issue/k8s/configmaps.yaml
kubectl rollout restart deployment/git-issue-agent -n team1
```

### Agent Missing Environment Variables

**Symptom:** Agent returns `JWKS_URI or GITHUB_TOKEN env var must be set` or similar

**Cause:** The UI deployment didn't include all required environment variables.

**Fix:** Patch the deployment directly:

```bash
kubectl set env deployment/git-issue-agent -n team1 -c agent \
  MCP_URL="http://github-tool-mcp:9090/mcp" \
  JWKS_URI="http://keycloak-service.keycloak.svc:8080/realms/rossoctl/protocol/openid-connect/certs"
kubectl rollout status deployment/git-issue-agent -n team1 --timeout=180s
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
kubectl rollout restart deployment/git-issue-agent -n team1
```

### Agent Pod Not Starting (not fully ready)

**Symptom:** Pod never reaches **2/2** ready. Example: `1/2` or `CrashLoopBackOff`.

**Fix:** Check the agent and the AuthBridge sidecar; if the issue is
operator-managed registration not finishing, the pod waits on
`/shared/client-{id,secret}.txt`.

```bash
# AuthBridge sidecar — name depends on resolved mode:
#   proxy-sidecar (default): authbridge-proxy
#   envoy-sidecar:           envoy-proxy
kubectl logs deployment/git-issue-agent -n team1 -c authbridge-proxy
kubectl logs deployment/git-issue-agent -n team1 -c agent

# In envoy-sidecar mode, the proxy-init init container runs once;
# inspect its log via --previous if it exited with an error:
kubectl logs deployment/git-issue-agent -n team1 -c proxy-init --previous 2>/dev/null

# Operator-managed registration:
kubectl logs -n rossoctl-system deployment/rossoctl-controller-manager \
  | grep -iE "clientregistration|git-issue-agent" | tail -20
```

### Tool MCP Server Unreachable / Connection Reset

**Symptom:** Agent returns `Couldn't connect to the MCP server after 60 seconds`, or
direct curl to the tool gets `Connection reset by peer`.

**Possible causes:**

1. **AuthBridge sidecars injected** — If the webhook injected envoy-proxy into the tool
   pod, the authbridge gRPC server and tool MCP broker both bind to port 9090. Check container count:
   ```bash
   kubectl get pods -n team1 | grep github-tool
   # If you see 3/3 instead of 1/1, sidecars were injected
   ```
   **Fix:** Ensure **Enable AuthBridge sidecar injection** is **unchecked** when importing the tool (Step 4, item 7), then delete and re-import.

2. **Service port mismatch** — Verify the tool service uses port 9090 (matching the agent's `MCP_URL`):
   ```bash
   kubectl get svc github-tool-mcp -n team1 -o jsonpath='{.spec.ports[0].port}:{.spec.ports[0].targetPort}'
   # Should show 9090:9090. If not, patch:
   kubectl patch svc github-tool-mcp -n team1 --type='json' \
     -p='[{"op":"replace","path":"/spec/ports/0/port","value":9090},{"op":"replace","path":"/spec/ports/0/targetPort","value":9090}]'
   ```

### GitHub Tool Returns 401

**Symptom:** Tool rejects the exchanged token

**Fix:** Verify the tool's environment variables match the Keycloak configuration:
- `ISSUER` should be `http://keycloak.localtest.me:8080/realms/rossoctl`
- `AUDIENCE` should be `github-tool`

---

## Cleanup

### Via Rossoctl UI

1. Go to the **Agent Catalog**, find `git-issue-agent`, and click **Delete**.
2. Go to the **Tool Catalog**, find `github-tool`, and click **Delete**.

### Via CLI

```bash
kubectl delete deployment git-issue-agent -n team1
kubectl delete deployment github-tool -n team1
kubectl delete svc git-issue-agent -n team1
kubectl delete svc github-tool-mcp -n team1
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

---

## Files Reference

| File | Description |
|------|-------------|
| `demos/github-issue/demo-ui.md` | This guide |
| `demos/github-issue/demo-manual.md` | Fully manual deployment guide |
| `demos/github-issue/setup_keycloak.py` | Keycloak configuration script |
| `demos/github-issue/k8s/configmaps.yaml` | Demo-specific authbridge-config and authproxy-routes |

## Next Steps

- **Manual Deployment**: See [demo-manual.md](demo-manual.md) for deploying everything via `kubectl`
- **AuthBridge Binary**: See the [AuthBridge README](../../cmd/authbridge/README.md) for inbound
  JWT validation and outbound token exchange internals
- **Token-Exchange Routes**: See the [routes-configuration guide](../token-exchange-routes/README.md) for
  route-based token exchange to multiple tool services
- **AuthBridge Overview**: See the [AuthBridge README](../../README.md) for architecture details
