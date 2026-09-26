# AuthBridge Demos

This directory contains demo scenarios showing AuthBridge providing zero-trust
authentication for Kubernetes agent workloads. Each demo progressively introduces
more AuthBridge capabilities.

> **Note:** These demos use the operator-injected combined sidecar (after
> cortex#411 — `authbridge` for proxy-sidecar, `authbridge-envoy`
> for envoy-sidecar, and `authbridge-lite`, the proxy image built with the
> sidecar-minimum `lite` profile from
> `scripts/profile-tags`). The previous `authbridge-unified` image and the per-component
> sidecars (`client-registration`, standalone `spiffe-helper`) have been
> removed.

## Available Demos

| Demo | Difficulty | What It Shows | Deployment |
|------|:----------:|---------------|:----------:|
| **[Weather Agent](weather-agent/demo-ui.md)** | Beginner | Inbound JWT validation, automatic identity registration, outbound passthrough | UI |
| **[Weather Agent (advanced)](weather-agent/demo-ui-advanced.md)** | Intermediate | Inbound on agent **and** tool, outbound token exchange, ingress JWT verification on the tool | [kubectl + script](weather-agent/demo-ui-advanced.md#automated-deploy-and-verify-ci-oriented) |
| **[GitHub Issue Agent](github-issue/demo.md)** | Intermediate | Inbound validation + outbound token exchange + scope-based access control | [UI](github-issue/demo-ui.md) or [Manual](github-issue/demo-manual.md) |
| **[Token-Exchange Routes](token-exchange-routes/README.md)** | Reference | How to write `authproxy-routes` for single- and multi-target token exchange | Configuration only |
| **[MCP Parser Plugin](mcp-parser/README.md)** | Reference | Enable the `mcp-parser` plugin to surface tool calls / resource reads in session events | Configuration only |
| **[Session Budget](session-budget/README.md)** | Reference | Test assets for the `session-budget` plugin, including a pause-mode webhook stub. Also see [`hitl-local.md`](session-budget/hitl-local.md) for a laptop-only walkthrough of `on_exceed: pause` (no Kubernetes required). | kubectl or local |
| **[abctl Walkthrough](weather-agent/demo-with-abctl.md)** | Reference | Watch the AuthBridge plugin pipeline live with the `abctl` TUI | Tooling only |
| **[IBAC](ibac/README.md)** | Intermediate | Intent-Based Access Control: LLM judge denies outbound HTTP that doesn't align with the user's recorded intent. Reproduces the email-poison / prompt-injection attack from `huang195/ibac`; chat with the agent through the rossoctl UI and see the exfiltration blocked, then `make show-result` for a pipeline-level forensic | UI + kubectl |
| **[SPARC (finance)](finance-sparc/README.md)** | Intermediate | SPARC pre-tool reflection: the `sparc` plugin blocks a hallucinated/ungrounded tool argument (an invented transaction id) before it executes and transparently asks the user to clarify, then approves the corrected call. Complements IBAC — SPARC verifies argument grounding, IBAC verifies intent alignment | UI + kubectl |
| **[Lineage](lineage/README.md)** | Intermediate | Per-request lineage on the Weather Agent pair: attach the sidecar with the lineage attach kit and see one turn first as 19 separate traces (the app forwards no `traceparent`), then as one trace of 70 spans with one root once the app's own propagation is switched on. Deploys the two stock images plain; edits nothing else | kubectl + scripts |
| **[Lineage attach kit](../deploy/lineage-attach/README.md)** | Reference | Attach per-request lineage to any existing Deployment: enable the `lineage-telemetry` plugin and every HTTP exchange becomes two facts-only spans (`request` + `response`, paired by `lineage.exchange.id`) sent to **any** OTLP consumer. A strategic-merge patch + ConfigMap, generated and validated; a propagate-only OTel shim for uninstrumented Python apps, activated by one env var | kubectl + scripts |
| **[CPEX Bridge (HR)](hr-cpex/README.md)** | Advanced | CPEX/APL declarative policy: one route chains a coarse APL predicate, an embedded Cedar PDP, RFC 8693 token exchange with a post-check, PII redaction and audit plugins. Same request, different data per caller (Bob sees an SSN, Eve gets it redacted). Self-contained: its own kind cluster + namespace, deployed via `make` rather than operator injection | [kubectl (make)](hr-cpex/README.md#quick-start) |
| **[Echo](echo/README.md)** | Beginner | Credential placeholder swap: the user's real token never reaches the agent — inbound `jwt-validation` forwards only an opaque placeholder — yet the agent's outbound call still carries a correctly exchanged real token to the upstream, which echoes what it received as ground truth | UI + kubectl (make) |
| **[Context Guru](context-guru/README.md)** | Intermediate | Context engineering: the `context-guru` outbound plugin compacts an agent's growing tool-output context before it reaches the LLM, so a task whose raw context exceeds the model's window still fits. Same agent/model/window in three modes (`off` / `observe` / `enforce`) shows the agent's answer flip from wrong to right | UI + kubectl (script) |
| **[mTLS](mtls/README.md)** | Reference | Agent-to-agent encryption via SPIRE X.509 SVIDs: authbridge's in-process `spiffe.Provider` mirrors SVIDs to disk on rotation and mTLS-encrypts both inbound and outbound traffic between two pods, in both proxy-sidecar and envoy-sidecar variants. Intentionally minimal — proves the mTLS layer only | kubectl (make) |

## Recommended Path

**New to AuthBridge?** Start with the demos in this order:

1. **[Weather Agent](weather-agent/demo-ui.md)** — Fastest way to see AuthBridge
   in action. Deploys via the Rossoctl UI with inbound JWT validation protecting
   the agent. No token exchange configuration needed; outbound traffic uses the
   default passthrough policy.

2. **[GitHub Issue Agent](github-issue/demo.md)** — Full AuthBridge demo with
   inbound validation *and* outbound token exchange. Shows how AuthBridge
   transparently exchanges tokens when the agent calls the GitHub tool, with
   scope-based access control (Alice vs Bob).

3. **[Token-Exchange Routes](token-exchange-routes/README.md)** — Reference
   for the `authproxy-routes` ConfigMap. Covers both single-target (one
   route) and multi-target (one agent → multiple tools, each with its own
   audience) patterns. Configuration-only — pair with one of the deployment
   demos above for a working stack.

## What Each Demo Covers

### Weather Agent (Getting Started)
- Deploy agent + tool via **Rossoctl UI**
- AuthBridge inbound JWT validation (signature, issuer, audience)
- Automatic SPIFFE identity registration with Keycloak
- Default outbound passthrough — agents work out-of-the-box with any tool or LLM
- CLI testing: public endpoints, token rejection, valid token

### Weather Agent (Advanced)
- Same images as the beginner demo, separate **`*-advanced`** Deployments so the
  getting-started flow stays untouched
- Outbound **token exchange** to the tool SPIFFE audience (`authproxy-routes`)
- AuthBridge **injected on the MCP tool** — JWT checks at Envoy before the tool process
- `deploy_and_verify_advanced.sh` for reproducible CI-style verification (Keycloak
  exchange + MCP `initialize` without requiring a working LLM)

### GitHub Issue Agent (Full AuthBridge Flow)
- Deploy agent + tool via **Rossoctl UI** or **kubectl**
- Keycloak configuration for token exchange (realm, clients, scopes)
- Inbound JWT validation protecting the agent
- Outbound OAuth 2.0 token exchange (RFC 8693) — agent-scoped token exchanged
  for tool-scoped token
- Subject preservation through exchange (`sub` claim maintained)
- Scope-based access control: Alice (public repos) vs Bob (all repos)
- Comprehensive CLI testing and AuthProxy log verification

### Token-Exchange Routes (Configuration Reference)
- How AuthBridge resolves the request `Host` header to a route entry
- ConfigMap shape for `authproxy-routes` — fields, glob patterns, ordering
- Single-target example (one route)
- Multi-target example (multiple routes, one agent → multiple tools)
- Mixing exchange and passthrough; tightening to `default_policy: exchange`
- Troubleshooting: `Host` mismatches, missing scopes, audience errors

### MCP Parser Plugin (Configuration Reference)
- How to enable the `mcp-parser` outbound plugin
- Surfaces MCP tool calls / resource reads / prompt invocations in
  session events for `abctl` and the `:9094` API
- Required `allow_mode_override: true` on the outbound ext_proc filter
  in envoy-sidecar mode

### Session Budget
- Cap how much an agent can spend per session — inference calls,
  tokens, or wall-clock time with Redis-backed counters shared
  across pods
- Three responses when a session hits its cap: `deny` (return 403),
  `observe` (log only, don't block), or `pause` (call a webhook and
  let a human approve or reject before the request continues)
- Cluster path ([`README.md`](session-budget/README.md)): production
  shape — plugin runs as a sidecar, session IDs come from inbound
  A2A requests, pause approvals go to a webhook stub deployed via
  `kubectl`
- Local path ([`hitl-local.md`](session-budget/hitl-local.md)):
  laptop-only walkthrough with Docker, `go run`, and `curl` — the
  fastest way to see pause-mode approval end-to-end, no Kubernetes
  required

### abctl Walkthrough (Tooling Reference)
- Run the `abctl` TUI against the weather-agent's session API
- See inbound JWT validation → protocol parsers → outbound exchange
  → LLM inference → response, paired live

### CPEX Bridge (HR) — APL Policy Engine
- **Self-contained**: own namespace `cpex-demo`, deployed with plain
  `kubectl apply` via `make deploy` (not operator-injected); the
  `hr-mcp` backend, Keycloak realm, and chat agent are all vendored
- Runs the `cpex` plugin (the `authbridge-cpex` image) with CPEX's
  policy language **APL** as a sidecar in front of an MCP backend
- **Embedded PDP**: APL consults Cedar through a `cedar:` step (the
  slot is engine-agnostic by design — OPA, AuthZen, …)
- **Explicit effects**: field redaction on request *and* response, a
  PII content guardrail, and structured audit logging — all as policy
  steps, not backend code
- **Authorization as action**: RFC 8693 token exchange (`delegate(...)`)
  is a first-class policy step, with a post-check that refuses to
  forward a token the IdP narrowed below what the call needs
- Seven deterministic curl scenarios plus an interactive LLM chat with
  mid-conversation persona switching (Bob / Eve / Alice)

## Prerequisites

Cluster-backed demos (everything above except the session-budget local
walkthrough) require:
- A Kubernetes cluster with the Rossoctl platform installed
  ([Installation Guide](https://github.com/rossoctl/rossoctl/blob/main/docs/getting-started/install.md))
- Keycloak deployed in the `keycloak` namespace (not used by the Lineage demo)
- SPIRE deployed (for demos using SPIFFE identity; not used by the Lineage demo)

UI-based demos additionally require:
- The Rossoctl UI running at `http://rossoctl-ui.localtest.me:8080`

The session-budget [`hitl-local.md`](session-budget/hitl-local.md)
walkthrough is Kubernetes-free — see its own Prerequisites section
(Docker, Ollama, Go).

## Common Setup: Keycloak Port-Forward

Most demos need Keycloak accessible at `http://keycloak.localtest.me:8080`.
If not already available via an ingress:

```bash
kubectl port-forward service/keycloak-service -n keycloak 8080:8080
```

## Common Setup: Python Environment

Demos that configure Keycloak need a Python virtual environment:

```bash
# from the repo root
python -m venv venv
source venv/bin/activate
pip install --upgrade pip
pip install -r requirements.txt
```

## Related Documentation

- [AuthBridge Overview](../docs/architecture.md) — Architecture and design
- [Rossoctl Operator](https://github.com/rossoctl/operator) — Admission webhook for sidecar injection (migrated from this repo)
