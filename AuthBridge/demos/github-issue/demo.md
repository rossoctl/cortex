# GitHub Issue Agent Demo with AuthBridge

This demo shows the **GitHub Issue Agent** running with **AuthBridge** for transparent,
zero-trust token management. AuthBridge adds automatic identity registration, inbound JWT
validation, and outbound token exchange — all without changing the agent code.

This demo extends the [upstream GitHub Issue Agent demo](https://github.com/kagenti/kagenti/blob/main/docs/demos/demo-github-issue.md)
by replacing manual token handling with AuthBridge's automatic token exchange.

## Choose Your Deployment Method

| Guide | Description | Best For |
|-------|-------------|----------|
| **[Manual Deployment](demo-manual.md)** | Deploy everything via `kubectl` and YAML manifests | Full control, debugging, understanding internals |
| **UI Deployment** *(coming soon)* | Import agent and tool via the Kagenti dashboard | Quick start, UI-driven workflow |

Both guides share the same infrastructure setup (webhook, Keycloak, ConfigMaps) and
produce identical AuthBridge security behavior.

## What This Demo Shows

1. **Agent identity** — The agent registers with Keycloak using its SPIFFE ID, no hardcoded secrets
2. **Inbound validation** — Requests are validated (JWT signature, issuer, audience) before reaching the agent
3. **Transparent token exchange** — When the agent calls the GitHub tool, AuthBridge exchanges the token automatically
4. **Subject preservation** — The end user's identity (`sub` claim) is preserved through the exchange
5. **Scope-based access** — The tool uses token scopes to determine public or privileged GitHub API access

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
- **git-issue-agent** — the A2A agent
- **spiffe-helper** — fetches SPIFFE credentials from SPIRE
- **kagenti-client-registration** — registers the agent with Keycloak
- **envoy-proxy** — intercepts traffic for JWT validation and token exchange

## Key Differences Between Deployment Methods

| Aspect | Manual | UI |
|--------|--------|----|
| **Agent deployment** | `kubectl apply -f` YAML | Kagenti UI "Import New Agent" |
| **Tool deployment** | `kubectl apply -f` YAML | Kagenti UI "Import New Tool" |
| **Image source** | Pre-built (`ghcr.io/kagenti/...`) | Built from source via Shipwright |
| **Agent service** | `git-issue-agent-service:8000` | `git-issue-agent:8080` |
| **Container name** | `git-issue-agent` | `agent` |
| **Primary interaction** | CLI (`curl` via test-client pod) | Kagenti UI chat |
| **Env var configuration** | In YAML manifests | UI form + optional `kubectl patch` |

## Files Reference

| File | Description |
|------|-------------|
| [demo-manual.md](demo-manual.md) | Full manual deployment guide |
| [setup_keycloak.py](setup_keycloak.py) | Keycloak configuration script |
| [k8s/configmaps.yaml](k8s/configmaps.yaml) | ConfigMaps for AuthBridge sidecars |
| [k8s/git-issue-agent-deployment.yaml](k8s/git-issue-agent-deployment.yaml) | Agent deployment YAML (manual) |
| [k8s/github-tool-deployment.yaml](k8s/github-tool-deployment.yaml) | GitHub tool deployment YAML (manual) |

## Related

- [Multi-Target Demo](../multi-target/demo.md) — route-based token exchange to multiple tools
- [Access Policies Proposal](../../PROPOSAL-access-policies.md) — role-based delegation control
- [AuthBridge Overview](../../README.md) — architecture and design
