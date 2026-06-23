# Weather Agent — Advanced AuthBridge Demo (UI)

UI-driven companion to the beginner [Weather Agent demo](demo-ui.md). Same agent
and tool images, but with the full **token-exchange + ingress validation** story
turned on.

## How this differs from the standard demo

| | Standard ([demo-ui.md](demo-ui.md)) | **Advanced (this guide)** |
|---|---|---|
| AuthBridge on tool | Off (passthrough) | **On** — JWT validated at the tool's ingress |
| Outbound from agent | Passthrough | **RFC 8693 token exchange** to the tool's audience |
| Keycloak setup | None | One Python script (audience scopes + token-exchange enabled) |
| `authproxy-routes` | Not used | One outbound route, set in the UI |
| Resource names | `weather-service`, `weather-tool` | `weather-service-advanced`, `weather-tool-advanced` |

The advanced names live alongside the beginner names so both demos can run in
the same namespace.

## Prerequisites

- Beginner [demo-ui.md prerequisites](demo-ui.md#prerequisites) (Kagenti UI
  reachable, an LLM provider).
- In **`team1`**: installer-provided `authbridge-config`,
  `authbridge-runtime-config`, `spiffe-helper-config`, `envoy-config`. No
  extra Secrets or ConfigMaps are required up front. **`keycloak-admin-secret`
  is not in `team1`.** Operator 0.2+ keeps it in **`kagenti-system`** for
  client registration; `NotFound` in `team1` is expected:
  ```bash
  kubectl get secret keycloak-admin-secret -n kagenti-system
  ```
- Python 3.10+ for the Keycloak setup script (Step 1).
- For OpenAI: a `team1` Secret named `openai-secret` (created in Step 3).

SPIFFE IDs (trust domain `localtest.me`):

- Agent: `spiffe://localtest.me/ns/team1/sa/weather-service-advanced`
- Tool: `spiffe://localtest.me/ns/team1/sa/weather-tool-advanced`

---

## Step 1: Configure Keycloak (one-time)

Adds the audience scopes and enables `standard.token.exchange.enabled` on the
agent and tool clients.

```bash
cd authbridge
python -m venv venv && source venv/bin/activate
pip install -r requirements.txt

# Port-forward Keycloak if your shell can't reach keycloak.localtest.me directly
kubectl port-forward -n keycloak svc/keycloak-service 8080:8080 &

python demos/weather-agent/setup_keycloak_weather_advanced.py \
  -n team1 --wait-tool-client
```

`--wait-tool-client` blocks until the tool pod (Step 2) registers its SPIFFE
client. **Re-run the same command after Step 3** so the agent client picks up
the optional exchange scope. The script:

- Adds realm default scope `agent-team1-weather-service-advanced-aud` (puts the
  agent SPIFFE in `aud` for UI / `alice` tokens).
- Adds optional scope `weather-tool-exchange-aud` (puts the tool SPIFFE in
  `aud` during token exchange).
- Enables token exchange on both Keycloak clients.
- Creates demo user `alice` (used by the optional CLI verify in Step 5).

---

## Step 2: Import the Weather Tool via Kagenti UI

> ⚠️ **Use the `-advanced` names exactly.** **Tool Name** must be
> `weather-tool-advanced` (not `weather-tool`). The Keycloak script in
> Step 1 registered SPIFFE / audience scopes for the `-advanced`
> ServiceAccount; if you import as `weather-tool`, the Service ends up as
> `weather-tool-mcp` instead of `weather-tool-advanced-mcp` and the
> `MCP_URL` + outbound route in Step 3 won't resolve.

1. Open [Import Tool](http://kagenti-ui.localtest.me:8080/tools/import).
2. **Namespace**: `team1` · **Tool Name**: `weather-tool-advanced` (exact).
3. **Deploy From Image** · **Container Image**:
   `ghcr.io/kagenti/agent-examples/weather_tool` · **Image Tag**: `latest`.
4. **MCP Transport Protocol**: `streamable HTTP`.
5. **Enable AuthBridge sidecar injection**: ✅ **check** (advanced demo
   validates JWTs at the tool's ingress — this is the difference vs. the
   standard demo).
6. **Enable SPIRE identity (spiffe-helper sidecar)**: ✅ **check**.
7. **Service Port** `8000` · **Target Port** `8000`.
8. Click **Build & Deploy Tool**.

Wait for the tool pod to be **Ready**. Once it registers in Keycloak, the
`setup_keycloak_weather_advanced.py` from Step 1 unblocks.

```bash
kubectl get pods -n team1 -l app.kubernetes.io/name=weather-tool-advanced
# Expect 3/3 (mcp + authbridge-proxy + spiffe-helper) in proxy-sidecar mode
```

---

## Step 3: Import the Weather Agent via Kagenti UI

(If you're using OpenAI, create the secret first — replace `<YOUR_OPENAI_API_KEY>`
with your real key; the shell variable expansion shown is intentional and works
only if you've already `export`ed it.)

```bash
kubectl create secret generic openai-secret -n team1 \
  --from-literal=apikey="<YOUR_OPENAI_API_KEY>"
```

> The UI agent import references `openai-secret` for both `LLM_API_KEY` and
> `OPENAI_API_KEY`. If the secret is empty (e.g. `$OPENAI_API_KEY` wasn't
> exported), the agent fails with `Error: No LLM API key configured.` — see
> [Troubleshooting](#troubleshooting).

> ⚠️ **Use the `-advanced` name exactly.** **Agent Name** must be
> `weather-service-advanced` (not `weather-service`). The Keycloak script
> in Step 1 registered SPIFFE / audience scopes for the `-advanced`
> ServiceAccount; the wrong name lands you with mismatched audiences and
> a 401/503 from token exchange.

Now the UI flow (order matches the actual import form top-to-bottom):

1. Open [Import Agent](http://kagenti-ui.localtest.me:8080/agents/import).
2. **Namespace**: `team1` · **Agent Name**: `weather-service-advanced` (exact).
3. **Build from Source**:
   - Git Repository URL: `https://github.com/kagenti/agent-examples`
   - Git Branch or Tag: `main`
   - Select Agent: `Weather Service Agent`
   - Source Subfolder: `a2a/weather_service`
4. **Protocol**: `A2A` · **Framework**: `LangGraph` · **Workload Type**:
   `Deployment`.
5. **Enable AuthBridge sidecar injection**: ✅ (default).
6. **Enable SPIRE identity**: ✅ (default).
7. Expand **Outbound Routing Rules** and add one route — this is what
   triggers the RFC 8693 exchange when the agent calls the tool. The form
   has three fields (currently unlabeled in the UI); fill them in this
   order:

   1. Host: `weather-tool-advanced-mcp`
   2. Target Audience: `spiffe://localtest.me/ns/team1/sa/weather-tool-advanced`
   3. Token Scopes: `openid weather-tool-exchange-aud`

   > If **Outbound Routing Rules** is missing or unresponsive, your Kagenti
   > backend may pre-date [kagenti#1194](https://github.com/kagenti/kagenti/pull/1194).
   > Apply the equivalent ConfigMap with kubectl (
   > `kubectl apply -f authbridge/demos/weather-agent/k8s/configmaps-advanced.yaml`)
   > and skip this expander. Same content, list-shaped `routes.yaml`.

8. **Service Port** `8080` · **Target Port** `8000`.
9. Under **Environment Variables**, click **Import from File/URL** → **From
   URL**, paste one of the beginner agent's env files, and click
   **Fetch & Parse**:
   - OpenAI: `https://raw.githubusercontent.com/kagenti/agent-examples/refs/heads/main/a2a/weather_service/.env.openai`
   - Ollama: `https://raw.githubusercontent.com/kagenti/agent-examples/refs/heads/main/a2a/weather_service/.env.ollama`

   The OpenAI variant adds `LLM_API_KEY` and `OPENAI_API_KEY` as **Secret**
   entries pointing at `openai-secret`.

   After import, **edit `MCP_URL`** in the variable list to point at the
   advanced tool service:
   ```text
   MCP_URL=http://weather-tool-advanced-mcp:8000/mcp
   ```
10. **(Ollama only)** Expand **AuthBridge Advanced Configuration** and set
    **Outbound Ports to Exclude** to `11434`. OpenAI uses HTTPS and needs no
    exclusion.
11. Click **Build & Deploy Agent**.

After the agent pod is **Ready**, re-run the Keycloak script so the agent's
dynamic client gets the optional exchange scope:

```bash
python demos/weather-agent/setup_keycloak_weather_advanced.py -n team1
```

---

## Step 4: Chat via Kagenti UI

> **Expected catalog quirk.** The **Agent Catalog** shows **two** entries:
> `weather-service-advanced` *and* `weather-tool-advanced`. The **Tool
> Catalog** is empty. This is by design — the tool's AgentRuntime CR
> uses `type: agent` so the operator applies `kagenti.io/type=agent`
> and AuthBridge gets injected (the `injectTools` feature gate is off
> by default; see the [kubectl appendix](#operator-gotchas)). Pick
> `weather-service-advanced` for chat.

1. **Agent Catalog** → namespace `team1` → `weather-service-advanced` →
   **View Details**. The agent card should render (proves the agent is up and
   `/.well-known/*` is bypassed).
2. In the **Chat** panel, ask: *"What is the weather in New York?"*
3. The response should be live weather. Behind the scenes:
   - UI's JWT (audience `agent-...-advanced-aud`) hits the agent's AuthBridge
     ingress.
   - AuthBridge on the agent matches the outbound route, exchanges for a token
     with `aud = weather-tool-advanced` SPIFFE.
   - AuthBridge on the tool validates that JWT before MCP sees it.

If chat returns `Connection error` or `No LLM API key configured`, see
[Troubleshooting](#troubleshooting) — those are LLM-side failures, not
AuthBridge failures.

---

## Step 5 (Optional): Verify via CLI

`deploy_and_verify_advanced.sh` exercises the AuthBridge / MCP path
end-to-end **without an LLM**. It's the right tool to confirm token exchange
and ingress validation when you want to isolate AuthBridge from agent-side
issues.

```bash
./authbridge/demos/weather-agent/deploy_and_verify_advanced.sh
```

What it does:

1. Password-grants `alice` against the `weather-advanced-e2e` Keycloak client.
2. Token-exchanges to the tool SPIFFE audience with scope
   `openid weather-tool-exchange-aud`.
3. `POST /mcp` with the exchanged token → expects **2xx** (JWT accepted, MCP
   `initialize` handshake completes). Sends
   `Accept: application/json, text/event-stream` so streamable HTTP doesn't
   return 406.
4. Repeats `POST /mcp` **without** an `Authorization` header → expects **401**
   (AuthBridge rejects before MCP runs).

> **`deploy_and_verify_advanced.sh` does NOT call the LLM.** It can succeed
> while UI chat still returns `Connection error` — see Troubleshooting.

Useful env knobs:

| Variable | Purpose |
|----------|---------|
| `NAMESPACE` | Default `team1` |
| `SKIP_DEPLOY=1` | Verify only, skip the apply step (resources must exist) |
| `KC_INTERNAL` | Keycloak base URL inside the cluster (default `keycloak-service.keycloak.svc:8080`) |
| `KC_USER_CLIENT_ID` | Public client for password grant (default `weather-advanced-e2e`) |
| `KEYCLOAK_ADMIN_USERNAME` / `KEYCLOAK_ADMIN_PASSWORD` | Admin REST credentials |

---

## Troubleshooting

| Symptom | Likely cause | Fix |
|---------|--------------|-----|
| UI returns `Error: LLM execution failed: Connection error.` | Agent can't reach its LLM. Ollama not running, or Outbound Ports to Exclude not set to `11434`. `deploy_and_verify_advanced.sh` doesn't catch this — it never calls the LLM. | Start Ollama (`ollama serve` + `ollama pull llama3.2:3b-instruct-fp16`), or re-import with the OpenAI `.env` URL. |
| UI returns `Error: No LLM API key configured. Set the LLM_API_KEY environment variable.` | `openai-secret` is empty (often because `$OPENAI_API_KEY` wasn't exported when you ran `kubectl create secret`), or the agent wasn't restarted after fixing it. | Recreate with the literal value, then verify `kubectl get secret openai-secret -n team1 -o jsonpath='{.data.apikey}' \| base64 -d \| wc -c` is non-zero, then `kubectl rollout restart deploy/weather-service-advanced -n team1`. |
| UI: **Outbound Routing Rules** expander missing | Kagenti backend pre-dates [kagenti#1194](https://github.com/kagenti/kagenti/pull/1194) | `kubectl apply -f authbridge/demos/weather-agent/k8s/configmaps-advanced.yaml` and skip the UI step. |
| UI: agent card not available | AuthBridge failed to load `authproxy-routes` (invalid YAML shape) | See the same section in the [GitHub Issue UI demo](../github-issue/demo-ui.md#agent-card-not-available-in-the-ui). |
| `401` on tool MCP from CLI verify | Wrong `target_audience` or scope mapper | `target_audience` must equal the tool SPIFFE; scope `weather-tool-exchange-aud` must map that audience. Re-run `setup_keycloak_weather_advanced.py`. |
| `invalid_scope` / `503` from agent | Optional exchange scope not on agent client | Re-run `setup_keycloak_weather_advanced.py -n team1` **after** the agent is running. |
| Token exchange denied | Tool client missing `standard.token.exchange.enabled` | Re-run setup with `--wait-tool-client` after the tool pod registers. |
| Tool pod CrashLoopBackOff (`mcp` container) | The `weather_tool` image runs as UID 1001; a `securityContext` overriding the user breaks `uv run` | Use the manifests in `k8s/` as-is (they set `runAsUser/Group/fsGroup: 1001`). On OpenShift, see the [upstream Dockerfile](https://github.com/kagenti/agent-examples/blob/main/mcp/weather_tool/Dockerfile). |
| Tool ingress logs missing `[Inbound]` | Combined sidecar uses different log text | Grep for `Token validated` instead, or increase log window. |
| Deleted the agent or tool, but the Deployment + Service reappear within seconds | The Kagenti backend's reconciliation service finalizes "orphaned" Shipwright builds by re-creating workloads | Also delete the Shipwright `Build` and `BuildRun` (see the [Cleanup](#cleanup) snippet). |
| Chat returns `Cannot connect to MCP weather service at http://weather-tool-advanced-mcp:8000/mcp` | UI import used the standard names (`weather-tool` / `weather-service`) instead of `-advanced`, so the actual Service is `weather-tool-mcp` and `MCP_URL` doesn't resolve | Re-import using the exact `-advanced` names. The Keycloak script from Step 1 also expects those names. |

Tool ingress and agent outbound logs (container name varies by AuthBridge mode
— `authbridge-proxy` for proxy-sidecar default, `envoy-proxy` for envoy-sidecar):

```bash
kubectl logs deploy/weather-tool-advanced -n team1 -c authbridge-proxy 2>&1 | grep -E "Inbound|Token validated"
kubectl logs deploy/weather-service-advanced -n team1 -c authbridge-proxy 2>&1 | grep -E "Resolver|exchange|Injecting token"
```

---

## Cleanup

Delete via the Kagenti UI (Tool Catalog / Agent Catalog), or via CLI:

```bash
kubectl delete deployment,svc,sa -n team1 \
  -l app.kubernetes.io/name=weather-service-advanced --ignore-not-found
kubectl delete deployment,svc,sa -n team1 \
  -l app.kubernetes.io/name=weather-tool-advanced --ignore-not-found

# Also delete the Shipwright Build/BuildRun, otherwise the Kagenti
# backend's reconciliation service treats them as "orphaned" and
# recreates the Deployment + Service + ServiceAccount within seconds:
kubectl delete build.shipwright.io,buildrun.shipwright.io -n team1 \
  -l app.kubernetes.io/name=weather-service-advanced --ignore-not-found
kubectl delete build.shipwright.io,buildrun.shipwright.io -n team1 \
  -l app.kubernetes.io/name=weather-tool-advanced --ignore-not-found
```

Keycloak clients for the SPIFFE IDs can be removed from the admin console.

---

## Appendix: kubectl-only path

If you'd rather skip the UI entirely, the same demo runs via raw manifests.

```bash
# 1. Keycloak setup (same as Step 1 above, but with --wait-tool-client first)
python authbridge/demos/weather-agent/setup_keycloak_weather_advanced.py \
  -n team1 --wait-tool-client &

# 2. Apply manifests
kubectl apply -f authbridge/demos/weather-agent/k8s/configmaps-advanced.yaml
kubectl apply -f authbridge/demos/weather-agent/k8s/weather-tool-advanced.yaml
kubectl apply -f authbridge/demos/weather-agent/k8s/agentruntime-weather-tool-advanced.yaml
kubectl rollout status deploy/weather-tool-advanced -n team1 --timeout=300s

kubectl apply -f authbridge/demos/weather-agent/k8s/weather-service-advanced.yaml
kubectl apply -f authbridge/demos/weather-agent/k8s/agentruntime-weather-service-advanced.yaml
kubectl rollout status deploy/weather-service-advanced -n team1 --timeout=420s

# 3. Re-run Keycloak setup so the agent client gets the optional exchange scope
python authbridge/demos/weather-agent/setup_keycloak_weather_advanced.py -n team1
```

The shipped agent manifest defaults to Ollama. To switch to OpenAI without the
UI:

```bash
kubectl create secret generic openai-secret -n team1 \
  --from-literal=apikey="<YOUR_OPENAI_API_KEY>"

kubectl set env deploy/weather-service-advanced -n team1 -c agent \
  LLM_API_BASE="https://api.openai.com/v1" \
  LLM_MODEL="gpt-4o-mini-2024-07-18" \
  LLM_API_KEY-

kubectl patch deploy weather-service-advanced -n team1 --type=json -p='[
  {"op":"add","path":"/spec/template/spec/containers/0/env/-","value":{
    "name":"LLM_API_KEY",
    "valueFrom":{"secretKeyRef":{"name":"openai-secret","key":"apikey"}}
  }},
  {"op":"add","path":"/spec/template/spec/containers/0/env/-","value":{
    "name":"OPENAI_API_KEY",
    "valueFrom":{"secretKeyRef":{"name":"openai-secret","key":"apikey"}}
  }}
]'
```

(The `LLM_API_KEY-` clears the literal `"ollama"` value from the manifest;
the `patch` re-adds it as a secret reference. Without the clear, both
definitions exist and the secret wins, but it's noisy in `kubectl describe`.)

### Operator gotchas

If the demo silently produces unprotected pods, check these:

- **`AgentRuntime` is required.** The mutating webhook **skips** AuthBridge
  injection unless an `agent.kagenti.dev/v1alpha1` `AgentRuntime` matches the
  Deployment. The k8s manifests here include them; if you build by hand,
  add them and **restart the Deployment** so new pods are admitted.
- **`spec.type: agent`, not `tool`.** With `spec.type: tool` the operator
  relabels the pod and the `injectTools` feature gate (off by default)
  controls injection — so the tool ends up with no AuthBridge. This demo
  uses `spec.type: agent` for the tool's runtime CR.
- **Don't set `kagenti.io/client-registration-inject: "true"`** — that label
  references a removed in-pod sidecar and disables operator-managed
  registration entirely (#411).

---

## Related Files

| File | Role |
|------|------|
| [k8s/configmaps-advanced.yaml](k8s/configmaps-advanced.yaml) | `authproxy-routes` for token exchange |
| [k8s/weather-tool-advanced.yaml](k8s/weather-tool-advanced.yaml) | Tool Deployment + Service + SA |
| [k8s/weather-service-advanced.yaml](k8s/weather-service-advanced.yaml) | Agent Deployment + Service + SA |
| [setup_keycloak_weather_advanced.py](setup_keycloak_weather_advanced.py) | Keycloak realm tuning |
| [deploy_and_verify_advanced.sh](deploy_and_verify_advanced.sh) | One-shot deploy + CI-style verification (no LLM) |
