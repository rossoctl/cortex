---
name: demo
description: Use when building, debugging, or running an AuthBridge demo end-to-end on a Kind cluster with SPIFFE/SPIRE, Keycloak, and Istio ambient mesh — covers the demo directory layout, the Keycloak setup scripts, and the recurring failure modes (ext_proc header ordering, ambient-mesh inbound path, Keycloak scope assignment).
---

# Skill: AuthBridge Demo Development

This skill captures knowledge from building, debugging, and running AuthBridge demos end-to-end on Kind clusters with SPIFFE/SPIRE, Keycloak, and Istio ambient mesh.

> **Some entries below are historical.** They describe failures from the
> pre-cortex#411 multi-sidecar shape, when `spiffe-helper` and
> `client-registration` were separate containers. Neither exists now — SVIDs are
> fetched in-process by `authlib/spiffe` and registration runs in the operator —
> but the diagnoses are kept because the same *symptoms* still appear in older
> clusters. Such entries are marked HISTORICAL.

## Repository Context

- **Repo:** `rossoctl/cortex` (monorepo)
- **Container registry:** `ghcr.io/rossoctl/cortex/<image-name>`
- **Agent examples repo:** `rossoctl/examples` (separate repo, images NOT published to GHCR)
- **Demo guides:** `demos/<demo-name>/demo-manual.md` (manual kubectl) and `demo-ui.md` (UI-driven)

## Demo Directory Convention

Each demo lives under `demos/<demo-name>/`:

```
demos/<demo-name>/
├── k8s/
│   ├── configmaps.yaml              # Per-demo ConfigMap *overrides* — typically
│   │                                #   authbridge-config and authproxy-routes;
│   │                                #   the installer supplies the defaults
│   ├── <agent>-deployment.yaml      # Agent Deployment + Service
│   └── <tool>-deployment.yaml       # Tool Deployment + Service (if applicable)
├── setup_keycloak.py                # Keycloak realm/client/scope/user setup
├── demo.md                          # Index page linking manual + UI guides
├── demo-manual.md                   # Full manual deployment guide (kubectl only)
└── demo-ui.md                       # UI-driven deployment guide
```

## Building and Loading Images for Kind

Agent/tool images from `rossoctl/examples` must be built locally and loaded into Kind:

```bash
# Build from agent-examples repo
docker build -t ghcr.io/rossoctl/examples/<agent>:latest ./a2a/<agent>/
docker build -t ghcr.io/rossoctl/examples/<tool>:latest ./mcp/<tool>/

# Build AuthBridge sidecar images. Every plugin is opt-in, so GO_BUILD_TAGS must
# name a profile: a build without it registers no plugins and rejects every
# config it is handed.
cd cortex
docker build -f cmd/authbridge-proxy/Dockerfile \
  --build-arg GO_BUILD_TAGS="$(go -C scripts/profile-tags run . full)" \
  -t ghcr.io/rossoctl/cortex/authbridge:latest .
docker build -f cmd/authbridge-envoy/Dockerfile \
  --build-arg GO_BUILD_TAGS="$(go -C scripts/profile-tags run . envoy)" \
  -t ghcr.io/rossoctl/cortex/authbridge-envoy:latest .

cd deploy/proxy-init
docker build -f Dockerfile.init -t ghcr.io/rossoctl/cortex/proxy-init:latest .

# Load into Kind
kind load docker-image <image> --name rossoctl
```

`scripts/local-build-and-test.sh` builds and Kind-loads `authbridge`,
`authbridge-envoy`, `authbridge-lite` and `proxy-init` (plus `spiffe-idp-setup`
from the rossoctl repo) — prefer it over building by hand. Note it does not build
`authbridge-cpex` or `authbridge-praxis`.

Use fully qualified image names in Dockerfiles (e.g., `docker.io/library/golang:1.26-alpine`) to avoid Podman/Buildah "short-name resolution enforced" errors in Shipwright builds.

## Envoy Config: No Longer Hand-Maintained Per Demo (HISTORICAL)

This section used to list five demo files that each carried a copy of the same
inbound listener, to be edited in lockstep. **None of them does now** — four of
the five files no longer exist (`demos/single-target/` and `authproxy/` are both
gone), and `demos/github-issue/k8s/configmaps.yaml` has shrunk to
`authbridge-config` + `authproxy-routes` with no listener in it at all.

Inbound interception is configured through `authbridge-config` and rendered by
the operator, not hand-written into demo ConfigMaps. The one hand-maintained
Envoy filter chain left in the repo is
`demos/mtls/k8s/envoy-config-mtls.yaml`; nothing needs lockstep edits
any more.

## Critical Bugs and Fixes

### 1. iptables Backend Mismatch (proxy-init)

**Symptom:** Inbound traffic bypasses Envoy — requests reach the agent without JWT validation.

**Root cause:** Alpine 3.18's default `iptables` uses the **nf_tables** backend. Kind/kubeadm use **iptables-legacy**. Rules set via nft have no effect when the cluster uses legacy.

**Fix:** `init-iptables.sh` auto-detects the backend via `detect_iptables_cmd()` (prefers `iptables-legacy`). All iptables calls use `${IPT}` variable. Override with `IPTABLES_CMD` env var if needed.

**Verification:** proxy-init logs must show `Using iptables command: iptables-legacy`.

**Diagnosis:**
```bash
docker exec <control-plane> iptables --version   # Check node backend
docker exec <control-plane> bash -c '             # Inspect pod's rules
  PID=$(crictl inspect $(crictl ps --name envoy-proxy -q | head -1) | jq -r .info.pid)
  nsenter -t $PID -n iptables -t nat -L PREROUTING -n -v
  nsenter -t $PID -n iptables -t nat -L PROXY_INBOUND -n -v'
```

### 2. Envoy Filter Ordering (ext_proc never sees direction header)

**Symptom:** ext_proc logs `=== Outbound Request Headers ===` for INBOUND traffic. All requests treated as outbound.

**Root cause:** `x-authbridge-direction: inbound` was at the **route level** (`request_headers_to_add` in `virtual_hosts`). Route headers are applied by the **router filter** — the LAST in the HTTP filter chain. ext_proc runs BEFORE router and never sees the header.

**Fix:** Add a **Lua filter** BEFORE ext_proc in the inbound listener:
```yaml
http_filters:
- name: envoy.filters.http.lua
  typed_config:
    "@type": type.googleapis.com/envoy.extensions.filters.http.lua.v3.Lua
    inline_code: |
      function envoy_on_request(request_handle)
        request_handle:headers():add("x-authbridge-direction", "inbound")
      end
- name: envoy.filters.http.ext_proc
  ...
- name: envoy.filters.http.router
  ...
```

**Key lesson:** Envoy HTTP filter execution order is: Lua → ext_proc → router. Route-level `request_headers_to_add` only takes effect during routing. Always use a filter to inject headers ext_proc needs.

### 3. SPIFFE File Permission Denied (HISTORICAL)

**Symptom:** `cat: /opt/jwt_svid.token: Permission denied` from a second container.

**Root cause:** spiffe-helper ran as root and wrote the file `0600`, while the
reader (client-registration) ran as UID 1000.

**Fix at the time:** align `RunAsUser` / `RunAsGroup` across both containers.

**Today this symptom cannot recur:** the in-process mirror writes
`/opt/jwt_svid.token` mode `0644` (`authlib/spiffe/mirror.go`), not `0600`, and the
`client-registration` reader is gone. `svid_key.pem` is still `0600`, so a
different-UID reader of *the key* would be denied — that is a different symptom
from the one above.

### 4. Istio Ambient Mesh Inbound Path

With Istio ambient mesh, inbound traffic enters via **OUTPUT** (ztunnel delivery), NOT PREROUTING. `init-iptables.sh` PROXY_OUTPUT rule 1 (mark 0x539 + !uid 1337 + dst LOCAL) handles this correctly.

**Diagnosis:** If PROXY_INBOUND REDIRECT has 0 packets but PROXY_OUTPUT rule 1 has non-zero, ambient mesh is active.

## Debugging Techniques

### Inbound Validation

```bash
# Direct to inbound port (bypasses iptables, tests ext_proc only)
AGENT_POD_IP=$(kubectl get pod -n <ns> -l app=<agent> -o jsonpath='{.items[0].status.podIP}')
kubectl exec test-client -n <ns> -- curl -s -o /dev/null -w "%{http_code}" http://$AGENT_POD_IP:15124/.well-known/agent.json
# Expected: 401

# Through service (full path: iptables + ext_proc)
kubectl exec test-client -n <ns> -- curl -s http://<agent-service>:8000/.well-known/agent.json
# Expected: {"error":"unauthorized","message":"missing Authorization header"}
```

### ext_proc Direction Classification

```bash
kubectl logs deployment/<agent> -n <ns> -c envoy-proxy --since=30s 2>&1 | head -40
# "=== Inbound Request Headers ===" → correctly classified
# "=== Outbound Request Headers ===" → Lua filter missing or misconfigured
# "Traffic direction INBOUND" → Envoy knows it's inbound (from listener)
```

### AuthBridge Logs

```bash
# Inbound JWT validation
kubectl logs deployment/<agent> -n <ns> -c envoy-proxy 2>&1 | grep "\[Inbound\]"
# Token exchange (outbound)
kubectl logs deployment/<agent> -n <ns> -c envoy-proxy 2>&1 | grep "\[Token Exchange\]"
```

### Client Registration

There is no `rossoctl-client-registration` container to read logs from — after
cortex#411 registration runs in the operator's `ClientRegistrationReconciler`.
For the registration side, read the operator's controller logs in its own
namespace; only the Keycloak query below still applies here.

```bash
# Query Keycloak (use --data-urlencode for SPIFFE IDs)
curl -s -H "Authorization: Bearer $ADMIN_TOKEN" \
  --data-urlencode "clientId=spiffe://localtest.me/ns/<ns>/sa/<sa>" \
  --get "http://keycloak.localtest.me:8080/admin/realms/rossoctl/clients" | jq '.[0].clientId'
```

## Demo Deployment Checklist

1. **ConfigMaps FIRST** — Apply before deploying agents. Stale ConfigMaps cause silent registration failures.
2. **Rebuild after code changes** — `init-iptables.sh` changes → rebuild proxy-init. `configmaps.yaml` changes → `kubectl apply` + restart pods.
3. **Verify proxy-init backend** — Logs must show `Using iptables command: iptables-legacy`.
4. **Agent env vars** — git-issue-agent uses `MCP_URL` (NOT `MCP_SERVER_URL`) and needs `JWKS_URI`.
5. **Envoy timeout** — Set to `300s` for LLM agents. Default 15s causes `upstream request timeout`.
6. **Ollama** — Must be running (`ollama serve`) before end-to-end queries with local LLM.
7. **A2A protocol** — git-issue-agent uses v0.3.0 with method `message/send` (NOT `tasks/send`). Requires `messageId` field.
8. **Keycloak client ID with SPIRE** — Full SPIFFE ID (e.g., `spiffe://localtest.me/ns/team1/sa/git-issue-agent`), not a short name.
9. **Sidecar injection** — there is no `webhook-rollout.sh` in this repo any more; injection is the operator's webhook, so roll pods rather than re-running a script.
10. **Keycloak scopes** — `github-full-access` is OPTIONAL; must be explicitly requested in token requests.
11. **ISSUER vs TOKEN_URL** — `ISSUER` = Keycloak frontend URL (in token `iss` claim). `TOKEN_URL` = internal service URL. They differ in K8s.
12. **Keycloak port 8080** — Must be in `OUTBOUND_PORTS_EXCLUDE` to prevent ext_proc token exchange redirect loop.

## PR and Issue Conventions

- PR titles MUST follow conventional commits: `feat:`, `fix:`, `docs:`, `refactor:`, etc.
- Demo documentation PRs use `docs:` prefix.
- Bug fix PRs use `fix:` prefix.
- Split large changes into reviewable PRs (e.g., manual demo separate from UI demo, AuthProxy fixes separate from demo docs).

## What Triggers a Rebuild

| Change | Action |
|--------|--------|
| `init-iptables.sh` or `Dockerfile.init` | Rebuild proxy-init image, `kind load`, delete pod |
| `authlib/` or `cmd/authbridge-proxy/` | Rebuild the `authbridge` image, `kind load`, delete pod |
| `authlib/` or `cmd/authbridge-envoy/` | Rebuild the `authbridge-envoy` image, `kind load`, delete pod |
| `configmaps.yaml` (any section) | `kubectl apply -f configmaps.yaml`, delete pod |
| `*-deployment.yaml` | `kubectl apply -f <file>` (rolling update) |
| `setup_keycloak.py` | Re-run `python setup_keycloak.py` |
| Sidecar injection behaviour | Lives in `rossoctl/operator`, not here — rebuild and redeploy the operator there, then delete the agent pod for re-injection |
