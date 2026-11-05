# demos — agent notes: Kubernetes deployment

Loaded by Claude Code when you read a file under `demos/`. It covers how the AuthBridge sidecar runs in a Kubernetes pod: the pod shape, the files and ConfigMaps the operator provides, Envoy, Keycloak, and the cluster gotchas. The repo-wide rules are in the root [`CLAUDE.md`](../CLAUDE.md). The iptables details are in [`deploy/proxy-init/CLAUDE.md`](../deploy/proxy-init/CLAUDE.md), and the auth plugin paths in [`core/plugins/CLAUDE.md`](../core/plugins/CLAUDE.md). Paths in backticks are written from the repo root.

## How the Components Work Together

The operator (in a separate repo) injects AuthBridge sidecars
into workload pods. Default deployment shape (proxy-sidecar mode):

```
         ┌────────────────────────────────────┐
         │            WORKLOAD POD            │
         │                                    │
         │  cortex ──► SPIRE Agent            │  (in-process
         │    - spiffe.Provider reads SVIDs   │   Workload API
         │      over the Workload API and     │   client; shaped
         │      mirrors them under /opt/      │   by the `spiffe:`
         │    - Reverse proxy: inbound JWT    │   config block)
         │    - Forward proxy: outbound       │
         │      token exchange                │
         │       │                            │
         │  Your Application                  │
         │    (HTTP_PROXY → forward proxy)    │
         └────────────────────────────────────┘

         The operator also creates a Secret with client-id +
         client-secret and mounts it at /shared/.

         For envoy-sidecar mode, replace cortex with
         the authbridge-envoy image (Envoy + ext_proc) and add a
         proxy-init container for iptables.
```

There is no `spiffe-helper` sidecar and no `SPIRE_ENABLED` gate. SPIRE
credentials are fetched in-process by `core/spiffe`'s Provider.

## Shared Volume Contract

The sidecar and the operator communicate through files on shared volumes:

| Path | Writer | Reader | Content |
|------|--------|--------|---------|
| `/opt/jwt_svid.token` | spiffe.Provider mirror | authbridge (token-exchange) + external readers | JWT SVID, audience from the plugin's `identity.jwt_audience`. Written **only** when a plugin requests a JWT-SVID for an audience (today: `token-exchange` with `identity.type: spiffe`) — an X.509-only workload never produces it |
| `/opt/svid.pem` | spiffe.Provider mirror | external readers (debugging, future Envoy SDS) | X.509 SVID leaf cert (PEM) |
| `/opt/svid_key.pem` | spiffe.Provider mirror | external readers | X.509 SVID private key (PEM) |
| `/opt/svid_bundle.pem` | spiffe.Provider mirror | external readers | SPIRE trust bundle (PEM, may concatenate multiple CAs) |
| `/shared/client-id.txt` | operator (Secret mount) | authbridge (`jwt-validation`'s `audience_file`) | SPIFFE ID or workload name |
| `/shared/client-secret.txt` | operator (Secret mount) | authbridge (`token-exchange`) | Keycloak client secret |

The X.509 SVID files are mirrored to disk by the in-process
`spiffe.Provider` when `spiffe.mirror_files` is on (the default); the files
exist for external readers (e2e probes,
debugging, future Envoy filesystem SDS) and are kept fresh on every
rotation. The listener itself reads SVIDs in-memory via
`spiffe.X509Source` and never re-reads the files. authbridge enables
mTLS only when `mtls:` is configured at the top level of the
runtime config; absent that block, today's plaintext behavior is
preserved.

## ConfigMaps and Secrets Expected at Runtime

When the operator injects sidecars, the target namespace needs these resources.
The rossoctl Helm chart's `agent-namespaces.yaml` and
`authbridge-template-configmaps.yaml` templates render them; the operator copies
them into agent namespaces that don't already have them:

| Resource | Kind | Used by | Keys |
|----------|------|---------|------|
| `authbridge-config` | ConfigMap | authbridge | `KEYCLOAK_URL`, `KEYCLOAK_REALM`, `PLATFORM_CLIENT_IDS` (optional), `TOKEN_URL` (optional, derived from KEYCLOAK_URL+KEYCLOAK_REALM), `ISSUER` (optional, derived or explicit for split-horizon DNS), `DEFAULT_OUTBOUND_POLICY` (optional, defaults to `passthrough`). Inbound audience validation uses `CLIENT_ID` from `/shared/client-id.txt` by default (the other audience sources are in [`core/plugins/CLAUDE.md`](../core/plugins/CLAUDE.md)). Target audience and scopes are configured per-route in `authproxy-routes`. |
| `authbridge-runtime-config` | ConfigMap | authbridge | The runtime `config.yaml` — `mode`, `listener`, `pipeline`, and the top-level `session` / `stats` / `mtls` / `spiffe` / `tls_bridge` / `memory` / `pricing` / `cost_ledger` blocks. Note the name differs from the `authbridge-runtime` volume the operator mounts it through. |
| `keycloak-admin-secret` | Secret | operator (ClientRegistrationReconciler) | `KEYCLOAK_ADMIN_USERNAME`, `KEYCLOAK_ADMIN_PASSWORD` |
| `authproxy-routes` | ConfigMap (optional) | authbridge | `routes.yaml` -- per-host token exchange rules (format below) |
| `envoy-config` | ConfigMap | Envoy (inside the `authbridge-envoy` combined image, envoy-sidecar mode only) | `envoy.yaml` (full Envoy configuration) |

**`authproxy-routes` format** (`routes.yaml`):
```yaml
routes:
  - host: "github-tool-mcp"
    target_audience: "github-tool"
    token_scopes: "openid github-tool-aud github-full-access"
  - host: "auth-target-*"
    target_audience: "auth-target"
    token_scopes: "openid auth-target-aud"
```

**Note:** `authproxy-routes` is optional. Without it, all outbound traffic passes through unchanged (the default policy is `passthrough`). Only create it when the agent needs to call services that require token exchange. Set `DEFAULT_OUTBOUND_POLICY: "exchange"` in `authbridge-config` to restore the legacy behavior.

## Envoy Configuration

Envoy config lives in the `envoy-config` ConfigMap rendered by the [rossoctl Helm chart](https://github.com/rossoctl/rossoctl) at install time (template: `charts/rossoctl/templates/agent-namespaces.yaml` / `authbridge-template-configmaps.yaml`). Key listeners: `outbound_listener` (15123), `inbound_listener` (15124). Inbound listener injects `x-authbridge-direction: inbound` header. Both use ext_proc cluster pointing to the authbridge binary on localhost:9090.

## Changing Envoy Configuration
- Edit the `envoy.yaml` template in the [rossoctl Helm chart](https://github.com/rossoctl/rossoctl)
  (`charts/rossoctl/templates/agent-namespaces.yaml` or
  `authbridge-template-configmaps.yaml`) and `helm upgrade`
- Key listener/cluster names: `outbound_listener`, `inbound_listener`, `original_destination`, `ext_proc_cluster`
- After changes, restart the affected pods so they pick up the new ConfigMap content

## Keycloak Setup Scripts

The two main demo setup scripts (`demos/echo/` and `demos/finance-sparc/` have their own):

| Script | Location | Use Case |
|--------|----------|----------|
| `setup_keycloak_weather_advanced.py` | `demos/weather-agent/` | Weather agent (advanced) demo: realm setup, scopes for token exchange to the weather tool's audience, alice user. Used by `deploy_and_verify_advanced.sh` (run by hand; no workflow runs it). |
| `setup_keycloak.py` | `demos/github-issue/` | GitHub issue integration demo (creates github-tool client, github-tool-aud + github-full-access scopes, alice + bob users) |

**Common Keycloak defaults across all scripts:**
- URL: `http://keycloak.localtest.me:8080`
- Realm: `rossoctl`
- Admin: `admin` / `admin`

**Note:** the two scripts above share the same helper function patterns (`get_or_create_realm`, `get_or_create_client`, `get_or_create_client_scope`, etc.) and are idempotent.

## Adding New Keycloak Resources to Setup
- Edit the appropriate `setup_keycloak*.py` script
- Use the `get_or_create_*` helper pattern for idempotency
- All scripts use `python-keycloak` library (KeycloakAdmin class)

## Building Everything Locally

`scripts/dev/local-build-and-test.sh` orchestrates every image
the platform needs (`spiffe-idp-setup` from rossoctl, plus
`authbridge`, `authbridge-envoy`, `authbridge-lite`, `proxy-init`
from this repo) and loads them into a Kind cluster. Run it from
the repo root:

```bash
ROSSOCTL_DIR=../rossoctl ./scripts/dev/local-build-and-test.sh
```

To build a single image directly. The Go images build from the repo root, and
proxy-init builds from `deploy/proxy-init/`:

```bash
# proxy-init (iptables init container, envoy-sidecar + proxy-sidecar enforce-redirect)
cd deploy/proxy-init
make docker-build-init
make load-image                     # Uses KIND_CLUSTER_NAME env var (default: rossoctl)
cd ../..

# Sidecar images. Pick whichever you need; the operator selects the image per
# workload from the resolved AuthBridge mode. Plugins are all opt-in, so these two
# Dockerfiles refuse to build without GO_BUILD_TAGS naming a profile.
podman build -f cmd/cortex/Dockerfile \
  --build-arg GO_BUILD_TAGS="$(go -C scripts/profile-tags run . full)" \
  -t authbridge:latest .                                              # proxy-sidecar (default)
podman build -f cmd/cortex-envoy/Dockerfile \
  --build-arg GO_BUILD_TAGS="$(go -C scripts/profile-tags run . envoy)" \
  -t authbridge-envoy:latest .                                        # envoy-sidecar
# authbridge-lite: same proxy Dockerfile, built with the `lite` profile.
podman build -f cmd/cortex/Dockerfile \
  --build-arg GO_BUILD_TAGS="$(go -C scripts/profile-tags run . lite)" \
  -t authbridge-lite:latest .

kind load docker-image authbridge:latest       --name rossoctl
kind load docker-image authbridge-envoy:latest --name rossoctl
kind load docker-image authbridge-lite:latest  --name rossoctl
```

## Running the Full Demo

1. Set up a Kind cluster with SPIRE + Keycloak (use [Rossoctl installer](https://www.rossoctl.dev/docs/get-started/kubernetes))
2. Deploy the webhook via [operator](https://github.com/rossoctl/operator)
3. See the [Cortex demos index](README.md) for a recommended learning path:
   - **Getting started**: `demos/weather-agent/demo-ui.md` (inbound validation, UI deployment)
   - **Full flow**: `demos/github-issue/demo-ui.md` (token exchange + scope-based access)
   - **Routes config reference**: `demos/token-exchange-routes/README.md` (single + multi-target route patterns)

The recommended end-to-end flow is the weather-agent advanced demo, which
exercises the combined sidecar shape with token exchange to a tool's audience:

```bash
# Apply manifests, run Keycloak setup, verify end-to-end
demos/weather-agent/deploy_and_verify_advanced.sh
```

For an interactive walkthrough see
[`demos/weather-agent/demo-ui-advanced.md`](weather-agent/demo-ui-advanced.md).

## Kubernetes Manifests
- Example deployment YAMLs in `demos/*/k8s/`

## Important Cross-Component Relationships

1. **Envoy Proxy UID:** Envoy runs as UID 1337. The `proxy-init` iptables rules exclude this UID from redirection to prevent loops. The operator runs the injected proxy container as the same UID (`Proxy.UID`, default 1337), whichever image it is. The `authbridge` image's own default is `USER 1001`, so a pod built without the operator runs it as 1001, which proxy-init does not exempt.

2. **Port Coordination:** the `proxy-init` iptables rules redirect to Envoy's 15123 (outbound) and 15124 (inbound); see Important Port Mapping in the root [`CLAUDE.md`](../CLAUDE.md#important-port-mapping).

## Cluster gotchas

- **ISSUER vs TOKEN_URL**: `ISSUER` must be the Keycloak **frontend URL** (what appears in the `iss` claim of tokens), while `TOKEN_URL` is the **internal service URL**. These are often different in Kubernetes (e.g., `http://keycloak.localtest.me:8080` vs `http://keycloak-service.keycloak.svc:8080`).

- **TLS passthrough is one-way**: Outbound HTTPS traffic passes through Envoy without token exchange via the TLS passthrough filter chain. Only plaintext HTTP outbound traffic reaches authbridge. With the default outbound policy of `"passthrough"`, even plaintext HTTP traffic is forwarded unchanged unless it matches an explicit route in `authproxy-routes`.

- **Outbound passthrough is the safe default**: `DEFAULT_OUTBOUND_POLICY` defaults to `passthrough`, so token exchange only happens for hosts explicitly listed in `authproxy-routes`, where target audience and scopes are configured per-route. That is what lets outbound traffic to LLM inference endpoints (e.g., Ollama via `host.docker.internal`) work untouched. If this were set to `exchange`, every outbound HTTP call would attempt token exchange and fail for non-Keycloak destinations.

- **Route host patterns must match the HTTP Host header**: The `host` field in `authproxy-routes` is matched against the HTTP `Host` header, which the client sets from the URL hostname. For in-cluster calls that is the **short Kubernetes service name** from `MCP_URL` (e.g., `github-tool-mcp`), not the FQDN. Glob patterns (`*`, `?`, `[...]`) work, but a plain service name is the common case; the wrong pattern (e.g., `*.github-issue-tool*.svc.cluster.local`) silently falls through to the default passthrough policy.

- **Envoy config not embedded:** The envoy-proxy sidecar mounts `envoy-config` ConfigMap at `/etc/envoy`. This ConfigMap must exist in the target namespace before workloads are created.

- **Envoy Lua filter required for inbound**: The `x-authbridge-direction: inbound` header MUST be injected via a Lua filter before the ext_proc filter in the inbound listener. Route-level `request_headers_to_add` does NOT work because the router filter runs after ext_proc.

- **Admin credentials in ConfigMap**: the rossoctl Helm chart's
    `agent-namespaces.yaml` template stores Keycloak admin credentials
    in `authbridge-config` (a ConfigMap, not a Secret). This is for
    demo / dev clusters only — production should use a Kubernetes
    Secret and mount via SecretKeyRef.

- **Keycloak scope assignment for dynamically registered clients**: When the operator's `ClientRegistrationReconciler` auto-registers an agent as a Keycloak client, the client may not inherit all necessary scopes. The agent's own audience scope (e.g., `agent-team1-git-issue-agent-aud`) must be a **default** client scope for inbound JWT audience validation to work. Token exchange scopes (e.g., `github-tool-aud`, `github-full-access`) must be **optional** client scopes for `client_credentials` grants with explicit `scope=` to succeed. Re-run the demo's `setup_keycloak.py` after the agent is deployed to assign these scopes to the registered client.
