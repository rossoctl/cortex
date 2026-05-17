# Weather Agent — Advanced AuthBridge Demo (UI or kubectl)

This guide is the **advanced** companion to the beginner
[Weather Agent demo](demo-ui.md). It keeps the same weather agent and MCP tool
images, but turns on the full platform story:

- **Outbound token exchange** from the agent to the weather tool (RFC 8693),
  so the tool receives an access token minted for its audience.
- **AuthBridge on the tool** as well as the agent, so **JWT validation happens at
  the tool’s Envoy ingress** (ext_proc) before traffic reaches the MCP server.
- **Audience alignment**: the token exchange `target_audience` is the weather
  tool’s **SPIFFE-based Keycloak client ID** (the same string written to
  `/shared/client-id.txt` by client-registration). That matches what AuthBridge
  expects for inbound JWT `aud`, so you can demonstrate ingress verification
  without custom application code in the tool.

The beginner [demo-ui.md](demo-ui.md) is unchanged: new users can follow it
without Keycloak scope tuning or token exchange.

For a UI-driven walkthrough that also covers GitHub PATs and privileged scopes,
see [GitHub Issue Agent demo-ui](../github-issue/demo-ui.md). This advanced
weather demo is smaller in scope but hits the same **exchange + validate**
pattern with a trivial MCP backend.

## What This Demo Shows

1. **Agent identity** — SPIFFE registration with Keycloak for
   `weather-service-advanced`.
2. **Inbound validation on the agent** — same as the beginner demo.
3. **Transparent token exchange** — when the agent calls the tool, AuthBridge
   exchanges the caller’s token for one whose audience includes the tool’s
   SPIFFE client ID.
4. **Inbound validation on the tool** — AuthBridge on the tool pod validates
   `iss`, signature (JWKS), and `aud` **before** the MCP process sees the
   request. Logs show `[Inbound]` / `Token validated` on `envoy-proxy` (or the
   combined `authbridge` container when that feature gate is enabled).
5. **No GitHub tokens or PATs** — only Keycloak, SPIRE, and public weather APIs.

## Kagenti 0.2+ / current operator (important)

On current platforms, **three things** are required together for the full demo:

1. **`AgentRuntime`** — The mutating webhook **skips** injection unless a matching
   `agent.kagenti.dev/v1alpha1` `AgentRuntime` exists for the Deployment. Apply
   `k8s/agentruntime-weather-tool-advanced.yaml` and
   `k8s/agentruntime-weather-service-advanced.yaml` (the deploy script applies them
   when the CRD is present) and restart the Deployments so new pods are admitted
   after the CR exists.

2. **AgentRuntime `spec.type` for the tool** — If `spec.type: tool`, the operator
   can relabel the pod to `kagenti.io/type: tool`, and the **`injectTools` feature
   gate** is off by default, so **no AuthBridge** is injected. This demo uses
   **`spec.type: agent`** for the tool’s `AgentRuntime` so the pod keeps
   `kagenti.io/type: agent` (the image is still `weather_tool`; only the API
   classification changes).

3. **Operator-managed Keycloak client registration**. The operator's
   `ClientRegistrationReconciler` watches `kagenti.io/type: agent` pods and
   creates the SPIFFE-shaped client in Keycloak (and the corresponding
   `kagenti-keycloak-client-credentials-<hash>` Secret). The legacy
   `kagenti.io/client-registration-inject: "true"` label is **no longer
   used** — it referenced an in-pod `kagenti-client-registration`
   sidecar that was removed in #411. Setting that label today disables
   operator-managed registration and leaves the workload with no
   credentials at all.

## Architecture

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                         Kubernetes (e.g. namespace team1)                  │
│                                                                              │
│  ┌────────────────────────────────────────────────────────────────────────┐ │
│  │  weather-service-advanced (agent + AuthBridge + SPIRE)                 │ │
│  │    Inbound: validate user JWT (aud = agent SPIFFE)                     │ │
│  │    Outbound: match host weather-tool-advanced-mcp → token exchange       │ │
│  └───────────────────────────────┬────────────────────────────────────────┘ │
│                                  │ Bearer: exchanged token (aud ⊇ tool SPIFFE)│
│                                  ▼                                            │
│  ┌────────────────────────────────────────────────────────────────────────┐ │
│  │  weather-tool-advanced (MCP + AuthBridge + SPIRE)                      │ │
│  │    Inbound: validate exchanged JWT at Envoy / ext_proc                 │ │
│  │    mcp container: streamable HTTP MCP (port 8000)                        │ │
│  └────────────────────────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────────────────────┘
```

Unlike the GitHub MCP tool, the weather tool listens on **port 8000**. AuthBridge’s
gRPC ext_proc listener uses **9090 inside the pod**, so there is no port clash
with the pattern described for `github-tool` on 9090 in the
[GitHub Issue demo-ui](../github-issue/demo-ui.md).

## Prerequisites

Same platform assumptions as
[GitHub Issue demo-ui prerequisites](../github-issue/demo-ui.md#prerequisites):

- Kagenti installed with Keycloak (`keycloak` namespace), SPIRE, and the
  admission webhook that injects AuthBridge.
- Target namespace (default `team1`) with installer-provided `authbridge-config`,
  `envoy-config`, `spiffe-helper-config`, and `keycloak-admin-secret`.
- Python 3.10+ and `pip install -r AuthBridge/requirements.txt` for the Keycloak
  helper script.
- For **chat-style** verification: an LLM (Ollama on the host with the model
  referenced in the Deployment, or OpenAI with keys in a Secret) and outbound
  port **11434** excluded on the agent (see below).

## Resource Names (kubectl path)

To avoid colliding with an existing beginner `weather-service` /
`weather-tool`, this demo uses:

| Kind | Name |
|------|------|
| Deployments | `weather-tool-advanced`, `weather-service-advanced` |
| Services | `weather-tool-advanced-mcp` (port 8000), `weather-service-advanced` (8080→8000) |
| ServiceAccounts | `weather-tool-advanced`, `weather-service-advanced` |

SPIFFE IDs (trust domain `localtest.me`):

- Agent: `spiffe://localtest.me/ns/team1/sa/weather-service-advanced`
- Tool: `spiffe://localtest.me/ns/team1/sa/weather-tool-advanced`

If you change namespace or ServiceAccount names, update
`k8s/configmaps-advanced.yaml` (`host`, `target_audience`) and re-run
`setup_keycloak_weather_advanced.py` with matching `-n`, `-a`, and `-t`.

## Step 1: Keycloak (Python)

From the **AuthBridge** directory (repository path `AuthBridge/`):

```bash
cd AuthBridge
python -m venv venv
source venv/bin/activate
pip install -r requirements.txt
```

Port-forward Keycloak if you use the default public URL:

```bash
kubectl port-forward service/keycloak-service -n keycloak 8080:8080
```

Run setup **after** the tool pod has registered in Keycloak, or pass
`--wait-tool-client` so the script waits for the tool SPIFFE client:

```bash
python demos/weather-agent/setup_keycloak_weather_advanced.py \
  -n team1 \
  -a weather-service-advanced \
  -t weather-tool-advanced \
  --wait-tool-client
```

Re-run once **after the agent** is running so optional exchange scopes attach to
the agent’s dynamic client (same pattern as the GitHub demo).

The script:

- Ensures realm default scope `agent-team1-weather-service-advanced-aud` adds the
  agent SPIFFE to access-token `aud` (UI / alice tokens).
- Adds optional scope `weather-tool-exchange-aud` with an audience mapper to
  the **tool SPIFFE** (used during token exchange).
- Enables `standard.token.exchange.enabled` on the tool (and agent) Keycloak
  clients once they exist.

## Step 2: ConfigMap (authproxy-routes)

```bash
kubectl apply -f AuthBridge/demos/weather-agent/k8s/configmaps-advanced.yaml
```

If you are not using `team1`, edit `metadata.namespace` and the `target_audience`
SPIFFE string first.

## Step 3: Deploy Tool, Then Agent

```bash
kubectl apply -f AuthBridge/demos/weather-agent/k8s/weather-tool-advanced.yaml
kubectl rollout status deployment/weather-tool-advanced -n team1 --timeout=300s

python demos/weather-agent/setup_keycloak_weather_advanced.py \
  -n team1 --wait-tool-client

kubectl apply -f AuthBridge/demos/weather-agent/k8s/weather-service-advanced.yaml
kubectl rollout status deployment/weather-service-advanced -n team1 --timeout=420s

python demos/weather-agent/setup_keycloak_weather_advanced.py -n team1
```

### Ollama and outbound port exclusion

The sample agent manifest sets `LLM_API_BASE` to `host.docker.internal:11434` and
annotates the pod with `kagenti.io/outbound-ports-exclude: "11434"`. If you
import through the UI instead, set **Outbound Ports to Exclude** to `11434` the
same way as in [demo-ui.md](demo-ui.md#ollama-port-exclusion).

## Step 4 (Optional): Kagenti UI

You can mirror the GitHub Issue UI flow with these substitutions:

- **Tool**: enable **AuthBridge sidecar injection** and **SPIRE**; image
  `ghcr.io/kagenti/agent-examples/weather_tool`; streamable HTTP; Service name
  pattern ending in `-mcp` that matches the `host` entry in
  `configmaps-advanced.yaml`.
- **Agent**: image `ghcr.io/kagenti/agent-examples/weather_service`;
  `MCP_URL` pointing at the advanced MCP service;
  `authproxy-routes` list shape as in
  [Kagenti version notes](../github-issue/demo-ui.md#kagenti-version-notes-ui-import-and-authbridge).

If the UI backend cannot yet express the route list, apply
`k8s/configmaps-advanced.yaml` with kubectl and avoid duplicating routes in the UI.

## Step 5: Verify Manually

### Pods

```bash
kubectl get pods -n team1 | grep advanced
# Expect 2/2 for both workloads when injection is on (proxy-sidecar
# default = agent + authbridge-proxy; envoy-sidecar = agent + envoy-proxy
# plus the proxy-init init container).
```

### Tool ingress logs

```bash
# Container name depends on the resolved AuthBridge mode:
#   proxy-sidecar (default): -c authbridge-proxy
#   envoy-sidecar:           -c envoy-proxy
kubectl logs deployment/weather-tool-advanced -n team1 -c authbridge-proxy 2>&1 | grep "\[Inbound\]"
```

### Agent outbound exchange logs

```bash
kubectl logs deployment/weather-service-advanced -n team1 -c authbridge-proxy 2>&1 | grep -E "Resolver|exchange|Injecting token"
```

### CLI token + A2A

Follow [demo-ui.md — Step 6](demo-ui.md#step-6-test-via-cli), but use:

- Service: `weather-service-advanced:8080`
- SPIFFE / client lookup: `spiffe://localtest.me/ns/team1/sa/weather-service-advanced`

## Automated Deploy and Verify (CI-oriented)

The script **`deploy_and_verify_advanced.sh`** applies the manifests, runs the
Keycloak setup (including waiting for the tool client), waits for rollouts, and
verifies **end-to-end** without relying on an LLM:

1. Password grant for user **alice** (create `alice` / `alice123` if missing —
   the setup script creates her).
2. RFC 8693 token exchange using the **agent** Keycloak client credentials as
   the authenticated client and alice’s token as the `subject_token`, with
   `audience` set to the **tool SPIFFE** and scope `openid weather-tool-exchange-aud`.
3. HTTP `POST` to `http://weather-tool-advanced-mcp:8000/mcp` with a minimal
   JSON-RPC `initialize` body. The tool uses **streamable HTTP**; send
   `Accept: application/json, text/event-stream` (otherwise the MCP server
   often returns **406** even when AuthBridge already accepted the JWT). **HTTP
   401 is a hard failure**; a **2xx** response means the JWT was accepted and
   the initialize handshake completed.
4. The same `initialize` request **without** an `Authorization` header must
   return **401** (AuthBridge rejects the call before the MCP app runs).
   `deploy_and_verify_advanced.sh` checks this as a negative test.

Run from anywhere:

```bash
./AuthBridge/demos/weather-agent/deploy_and_verify_advanced.sh
```

Environment variables:

| Variable | Purpose |
|----------|---------|
| `NAMESPACE` | Target namespace (default `team1`) |
| `SKIP_DEPLOY=1` | Only run verification (resources must already exist) |
| `KC_INTERNAL` | Keycloak base URL inside the cluster (default `http://keycloak-service.keycloak.svc:8080`) |
| `KC_USER_CLIENT_ID` | Realm public client for password grant (default **`weather-advanced-e2e`**, created by the Keycloak script; the `kagenti` UI client often has direct access grants disabled) |
| `KC_USER_CLIENT_SECRET` | Set only if the password client is confidential |
| `KEYCLOAK_ADMIN_USERNAME` / `KEYCLOAK_ADMIN_PASSWORD` | For admin REST calls from the verify pod |

The script also **warns** if it cannot find obvious inbound/outbound log markers
(the container name it inspects depends on the resolved AuthBridge mode —
`authbridge-proxy` for proxy-sidecar, `envoy-proxy` for envoy-sidecar).

## Cleanup

```bash
kubectl delete deployment -n team1 \
  weather-service-advanced weather-tool-advanced --ignore-not-found
kubectl delete svc -n team1 \
  weather-service-advanced weather-tool-advanced-mcp --ignore-not-found
kubectl delete sa -n team1 \
  weather-service-advanced weather-tool-advanced --ignore-not-found
```

Keycloak clients for the SPIFFE IDs can be removed from the admin console if you
no longer need them.

## Troubleshooting

| Symptom | Likely cause | Mitigation |
|---------|--------------|------------|
| Tool pod **CrashLoopBackOff** (container `mcp`) | The `weather_tool` image runs as **UID 1001**; a `securityContext` that forces a different UID (e.g. only `runAsNonRoot: true` with a project-assigned user on OpenShift, or a mismatch with `chown`ed `/app` in the image) prevents `uv run` from reading the app tree | The manifests set **`runAsUser` / `runAsGroup` / `fsGroup: 1001`** to match the [upstream Dockerfile](https://github.com/kagenti/agent-examples/blob/main/mcp/weather_tool/Dockerfile). Re-apply `weather-tool-advanced.yaml`. If it still fails, run `kubectl logs -n team1 deploy/weather-tool-advanced -c mcp --previous` and `kubectl describe pod` for OOMKilled or `CreateContainerError`. |
| `invalid_scope` / 503 on agent | Optional exchange scope not on agent client | Re-run `setup_keycloak_weather_advanced.py` after the agent is running |
| `invalid audience` / 401 on agent | Default agent audience scope missing on UI client | Re-run setup; log out and back in to the UI |
| 401 on tool MCP | Wrong `target_audience` or scope mapper | `target_audience` must equal tool SPIFFE; scope `weather-tool-exchange-aud` must map that audience |
| Token exchange denied | Tool Keycloak client missing `standard.token.exchange.enabled` | Re-run setup with `--wait-tool-client` after the tool pod registers |
| No `[Inbound]` log line | Combined sidecar logging format | Grep for `Token validated` or increase log window |

See also the operational table in the AuthBridge testing skill used for this repo.

## Related Files

| File | Role |
|------|------|
| [k8s/configmaps-advanced.yaml](k8s/configmaps-advanced.yaml) | `authproxy-routes` for token exchange |
| [k8s/weather-tool-advanced.yaml](k8s/weather-tool-advanced.yaml) | Tool Deployment + Service + SA |
| [k8s/weather-service-advanced.yaml](k8s/weather-service-advanced.yaml) | Agent Deployment + Service + SA |
| [setup_keycloak_weather_advanced.py](setup_keycloak_weather_advanced.py) | Keycloak realm tuning |
| [deploy_and_verify_advanced.sh](deploy_and_verify_advanced.sh) | One-shot deploy + CI-style verification |
