# Cortex documentation

Cortex's documentation lives next to the code it describes. This directory
holds the repo-level pieces; this file is the index.

## New here

| If you want to… | Read |
|---|---|
| Install Cortex, see it work, and undo it | [Getting started](getting-started.md) |
| Route your agent through Cortex | [Claude Code](agents/claude-code.md), [Codex](agents/codex.md), [OpenCode](agents/opencode.md), [IBM Bob](agents/bob.md), [Pi](agents/pi.md), [any other agent](agents/any-agent.md) |
| Read the `agentop` screen | [Using agentop](../cmd/agentop/README.md#panes) |
| Fix a problem | [Troubleshooting](troubleshooting.md) |

## Daily use

| If you want to… | Read |
|---|---|
| Use a local model, or change inference servers | [`laptop-local-model.md`](laptop-local-model.md) |
| Use a server whose protocol Cortex cannot translate to | [`translation-server.md`](translation-server.md) |
| Change Cortex with a small Python or shell script | [`script-plugin.md`](script-plugin.md) |
| Run Cortex on a remote machine | [`remote.md`](remote.md) |
| Add your agent's sessions from before Cortex | [`agentop import`](../cmd/agentop/README.md#importing-agent-history-agentop-import) |
| Set model prices and gateway discounts | [`pricing.md`](pricing.md). To see the rates in effect, run `agentop pricing [--host <gateway>]` |
| Run, stop or update the service | [`laptop-service.md`](laptop-service.md) |
| Apply policy to agent traffic in praxis mode | [`praxis-policy.md`](praxis-policy.md) |
| Export sessions | [`session-export.md`](session-export.md) (content-free records), [`session-export-guide.md`](session-export-guide.md) (a walk-through), [`session-dump.md`](session-dump.md) (session files) |

## Reference

### Kubernetes

| If you want to… | Read |
|---|---|
| Move from a laptop to a cluster | [`kubernetes.md`](kubernetes.md) |
| Learn the sidecar shapes | [`architecture.md`](architecture.md) |
| Find which binary serves which deployment shape | [`cmd/README.md`](../cmd/README.md) |
| Run a demo | [`cortex-demos/README.md`](https://github.com/rossoctl/cortex-demos/blob/main/README.md) |

### Plugins

| If you want to… | Read |
|---|---|
| Find and configure a plugin | [`plugin-catalog.md`](plugin-catalog.md) |
| Remove the tools an agent never calls from its prompts | [`laptop-token-savings.md`](laptop-token-savings.md) |
| Write your first plugin | [`plugin-tutorial.md`](plugin-tutorial.md) |
| Write a plugin in Python or shell | [`script-plugin.md`](script-plugin.md) |
| Read the plugin contract | [`plugin-reference.md`](plugin-reference.md) |
| Learn the pipeline internals and hot-reload | [`framework-architecture.md`](framework-architecture.md) |

Per-plugin reference docs: [a2a-parser](a2a-parser-plugin.md),
[context-guru](context-guru-plugin.md), [cpex](cpex-plugin.md),
[ibac](ibac-plugin.md), [inference-parser](inference-parser-plugin.md),
[inference-router](inference-router-plugin.md),
[jwt-validation](jwt-validation-plugin.md),
[lineage-telemetry](lineage-telemetry-plugin.md),
[litellm-budget-track](litellm-budget-track-plugin.md),
[mcp-parser](mcp-parser-plugin.md), [opa](opa-plugin.md),
[script](script-plugin.md), [session-budget](session-budget-plugin.md),
[sparc](sparc-plugin.md), [static-inject](static-inject-plugin.md),
[token-broker](token-broker-plugin.md),
[token-exchange](token-exchange-plugin.md),
[token-exchange-aiac](token-exchange-aiac-plugin.md),
[tool-prune](tool-prune-plugin.md).

### Configuration and APIs

| If you want to… | Read |
|---|---|
| Find where each config section is documented | [`configuration.md`](configuration.md) |
| Read sessions and events over HTTP | [`session-api.md`](session-api.md) |
| See what agentop and `install.sh` need from the local proxy | [`proxy-contract.md`](proxy-contract.md) |

### Contracts for contributors and integrators

| If you want to… | Read |
|---|---|
| Write an IdP plugin for token-exchange | [`idp-plugin-contract.md`](idp-plugin-contract.md) |
| Read the lineage OTLP wire contract | [`lineage-wire-contract.md`](lineage-wire-contract.md) |

## In this directory

- [`agents/`](agents/) — one guide per supported agent.
- [`proposals/`](proposals/) — design records for work that has since shipped.
  They are kept for the reasoning, not as current documentation; each carries a
  status line pointing at the doc that describes present behaviour.
- `assets/` — images used by the root README.

Dated implementation plans and design specs live in
[`docs/superpowers/`](../docs/superpowers/).
