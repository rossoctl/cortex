# GitHub Issue Agent Demo with AuthBridge

This demo shows the **GitHub Issue Agent** running with **AuthBridge** for transparent,
zero-trust token management. AuthBridge provides automatic identity registration,
inbound JWT validation, and outbound token exchange — all without changing the
agent code.

For a simpler getting-started demo without token exchange, see the
[Weather Agent demo](../weather-agent/demo-ui.md).

## Choose Your Deployment Method

| Guide | Description | Best For |
|-------|-------------|----------|
| **[Manual Deployment](demo-manual.md)** | Deploy everything via `kubectl` and YAML manifests | Full control, debugging, understanding internals |
| **[UI Deployment](demo-ui.md)** | Import agent and tool via the Kagenti dashboard | Quick start, UI-driven workflow |

Both guides share the same infrastructure setup (webhook, Keycloak, ConfigMaps) and
produce identical AuthBridge security behavior.

## What This Demo Shows

1. **Agent identity** — The agent registers with Keycloak using its SPIFFE ID, no hardcoded secrets
2. **Inbound validation** — Requests are validated (JWT signature, issuer, audience) before reaching the agent
3. **Transparent token exchange** — When the agent calls the GitHub tool, AuthBridge exchanges the token automatically
4. **Subject preservation** — The end user's identity (`sub` claim) is preserved through the exchange
5. **Scope-based access** — The tool uses token scopes to determine public or privileged GitHub API access
6. **Access control (Alice vs Bob)** — Two users with different scopes get different GitHub API access levels: Alice (public only) cannot access private repos, while Bob (privileged) can access both (requires scope forwarding: [kagenti-extensions#139](https://github.com/kagenti/kagenti-extensions/issues/139))

## Architecture Overview

```
  User/UI ──► Agent Pod (with AuthBridge sidecars) ──► GitHub Tool
                  │                                        │
                  │ 1. Inbound: validate JWT               │
                  │ 2. Outbound: exchange token             │
                  │    (aud: agent → aud: github-tool)      │
                  │                                        │
                  └──── Keycloak (token exchange) ─────────┘
```

The agent pod includes four containers:
- **agent** — the A2A agent (port 8000)
- **spiffe-helper** — fetches SPIFFE credentials from SPIRE
- **kagenti-client-registration** — registers the agent with Keycloak
- **envoy-proxy** — intercepts traffic for JWT validation and token exchange

## Key Differences Between Deployment Methods

Both methods produce identical Kubernetes resources (same service names, ports, container
names, and labels). The only difference is *how* you deploy:

| Aspect | Manual | UI |
|--------|--------|----|
| **Agent deployment** | `kubectl apply -f` YAML | Kagenti UI "Import New Agent" |
| **Tool deployment** | `kubectl apply -f` YAML | Kagenti UI "Import New Tool" |
| **Image source** | Pre-built (`ghcr.io/kagenti/...`) | Built from source via Shipwright |
| **Primary interaction** | CLI (`curl` via test-client pod) | Kagenti UI chat |
| **Env var configuration** | In YAML manifests | UI form + optional `kubectl patch` |

Common names used by both:
- Agent service: `git-issue-agent:8080` (targetPort 8000)
- Agent container: `agent`
- Tool service: `github-tool-mcp:9090`

## Files Reference

| File | Description |
|------|-------------|
| [demo-manual.md](demo-manual.md) | Full manual deployment guide |
| [demo-ui.md](demo-ui.md) | UI-driven deployment guide |
| [setup_keycloak.py](setup_keycloak.py) | Keycloak configuration script |
| [k8s/configmaps.yaml](k8s/configmaps.yaml) | ConfigMaps for AuthBridge sidecars |
| [k8s/git-issue-agent-deployment.yaml](k8s/git-issue-agent-deployment.yaml) | Agent deployment YAML (manual) |
| [k8s/github-tool-deployment.yaml](k8s/github-tool-deployment.yaml) | GitHub tool deployment YAML (manual) |

## Related

- [All Demos](../README.md) — index of all AuthBridge demos
- [Weather Agent Demo](../weather-agent/demo-ui.md) — simpler getting-started demo (no token exchange)
- [Token-Exchange Routes](../token-exchange-routes/README.md) — route-based token exchange to multiple tools
- [Access Policies Proposal](../../PROPOSAL-access-policies.md) — role-based delegation control
- [AuthBridge Overview](../../README.md) — architecture and design
