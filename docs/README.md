# Cortex documentation

Cortex's documentation lives next to the code it describes. This directory
holds the repo-level pieces; this file is the index.

## Getting started (laptop)

| If you want to… | Read |
|---|---|
| Install and run Cortex on a laptop | [root README](../README.md) |
| Route a specific agent through Cortex | [`agents/`](agents/) — one guide per agent: [claude-code](agents/claude-code.md), [codex](agents/codex.md), [opencode](agents/opencode.md), [IBM Bob](agents/bob.md), [any other agent](agents/any-agent.md) |
| Run the service, group sessions into sessions, troubleshoot | [`laptop-service.md`](laptop-service.md) |
| Cut token cost with tool pruning | [`laptop-token-savings.md`](laptop-token-savings.md) |
| Customize Cortex with a small Python or shell script | [`script-plugin.md`](script-plugin.md) |
| Run an agent on a local model, or switch inference servers | [`laptop-local-model.md`](laptop-local-model.md) |
| Use the `agentop` TUI | [`cmd/agentop/README.md`](../cmd/agentop/README.md) |
| Export sessions to files | [`session-dump.md`](session-dump.md) |

## Kubernetes and architecture

| If you want to… | Read |
|---|---|
| Move from a laptop to a cluster | [`kubernetes.md`](kubernetes.md) |
| Understand the sidecar shapes and deployment | [`architecture.md`](architecture.md) |
| Which binary pins which deployment shape | [`cmd/README.md`](../cmd/README.md) |
| Run a demo | [`demos/README.md`](../demos/README.md) |

## Plugins

| If you want to… | Read |
|---|---|
| Configure a plugin | [`plugin-catalog.md`](plugin-catalog.md) — the index, one summary per plugin; each plugin's full reference is its own `<name>-plugin.md` in this directory |
| Write a plugin | [`plugin-reference.md`](plugin-reference.md) and [`plugin-tutorial.md`](plugin-tutorial.md); for a quick one in Python or shell, [`script-plugin.md`](script-plugin.md) |
| Understand the pipeline internals and hot-reload | [`framework-architecture.md`](framework-architecture.md) |

Per-plugin reference docs: [a2a-parser](a2a-parser-plugin.md),
[context-guru](context-guru-plugin.md), [cpex](cpex-plugin.md),
[ibac](ibac-plugin.md), [inference-parser](inference-parser-plugin.md),
[inference-router](inference-router-plugin.md),
[jwt-validation](jwt-validation-plugin.md),
[lineage-telemetry](lineage-telemetry-plugin.md),
[litellm-budget-track](litellm-budget-track-plugin.md),
[mcp-parser](mcp-parser-plugin.md), [opa](opa-plugin.md),
[sparc](sparc-plugin.md), [static-inject](static-inject-plugin.md),
[session-budget](session-budget-plugin.md),
[token-broker](token-broker-plugin.md),
[token-exchange](token-exchange-plugin.md),
[tool-prune](tool-prune-plugin.md).

## Operator reference

| If you want to… | Read |
|---|---|
| Set up model pricing and gateway discounts, or read the `cost` record | [`pricing.md`](pricing.md) |
| Read sessions and events over HTTP — endpoints, event schema, SSE | [`session-api.md`](session-api.md) |
| See the rates actually in effect | `agentop pricing [--host <gateway>]` |
| Implement an IdP plugin for token-exchange | [`idp-plugin-contract.md`](idp-plugin-contract.md) |
| The lineage OTLP wire contract | [`lineage-wire-contract.md`](lineage-wire-contract.md) |

## Configuration reference

The runtime config has ten top-level sections. They are documented next to the
subsystem each one drives rather than in one file, so this table is the index.

| Section | What it configures | Documented in |
|---|---|---|
| `mode:` | Which deployment shape the binary serves; must match the binary | [`cmd/README.md`](../cmd/README.md) |
| `pipeline:` | The plugin composition, and each plugin's own `config:` | [`plugin-catalog.md`](plugin-catalog.md), [`plugin-reference.md`](plugin-reference.md) |
| `pricing:` | Model rates, gateway discounts, resolution order | [`pricing.md`](pricing.md) |
| `cost_ledger:` | The durable per-minute cost ledger | [`laptop-service.md`](laptop-service.md) |
| `session:` | Session store TTL, event/session caps, id headers | [`framework-architecture.md`](framework-architecture.md#operator-configuration-session) |
| `session.archive:` | The laptop's on-disk session history, its bounds, and clearing it | [`laptop-service.md`](laptop-service.md#session-history-is-kept-in-cortexsessions) |
| `stats:` | The diagnostic listener (`/stats`, `/config`, `/reload/status`, `/pricing/table`, `/tls-bridge/unread`), default `:9093` | [`framework-architecture.md`](framework-architecture.md) |
| `spiffe:` | SVID sourcing over the Workload API and the `/opt` file mirror | [`architecture.md`](architecture.md#spiffe-configuration) |
| `listener:` | Listener addresses, `roles`, interception mode, `skip_hosts`, `decline_responses_websockets` | [`cmd/README.md`](../cmd/README.md) (ports, roles), [`framework-architecture.md`](framework-architecture.md#skipping-infrastructure-hosts-listenerskip_hosts) (reload rules, `skip_hosts`, `decline_responses_websockets`) |
| `mtls:` | Transport mTLS on the listeners, both deployment shapes | [`framework-architecture.md`](framework-architecture.md#8a-mtls-layer) |
| `tls_bridge:` | The laptop TLS bridge and its CA | [`laptop-service.md`](laptop-service.md#the-tls_bridge-configuration) |

Two things no config file can answer, because the effective values come from the
file *plus* compiled-in defaults: what rates are in effect (`agentop pricing --host
<gateway>`) and what pipeline is running (`agentop pipeline get`, or `GET /v1/pipeline`).
`GET /config` on the diagnostic listener reports the config **as written**, not as
resolved.

## In this directory

- [`agents/`](agents/) — one guide per supported agent.
- [`proposals/`](proposals/) — design records for work that has since shipped.
  They are kept for the reasoning, not as current documentation; each carries a
  status line pointing at the doc that describes present behaviour.
- `assets/` — images used by the root README.

Dated implementation plans and design specs live in
[`docs/superpowers/`](../docs/superpowers/).
