# Cortex documentation

Cortex's documentation lives next to the code it describes. This directory
holds the repo-level pieces; this file is the index.

## Start using Cortex

| If you want to… | Read |
|---|---|
| Install Cortex, see it work, and undo it | [Getting started](getting-started.md) |
| Install and run Cortex on Windows (preview) | [Cortex on Windows](windows.md) |
| Route your agent through Cortex | [Claude Code](agents/claude-code.md), [Codex](agents/codex.md), [OpenCode](agents/opencode.md), [IBM Bob](agents/bob.md), [Pi](agents/pi.md), [any other agent](agents/any-agent.md) |
| See what agentop can do, with a screen for each task | [Using agentop](using-agentop.md) |
| Fix a problem | [Troubleshooting](troubleshooting.md) |

## Use Cortex every day

| If you want to… | Read |
|---|---|
| Change inference servers: ete-int, ete-ext, Bob or a local model | [`inference-servers.md`](inference-servers.md) |
| Send each terminal to the server that its API key selects | [Choosing a server by the key](inference-router-plugin.md#choosing-a-server-by-the-key-a-request-sends) |
| Use a server whose protocol Cortex cannot translate to | [`translation-server.md`](translation-server.md) |
| Change Cortex with a small Python or shell script | [`script-plugin.md`](script-plugin.md) |
| Run Cortex on a remote machine | [`remote.md`](remote.md) |
| Add your agent's sessions from before Cortex | [`agentop import`](../cmd/agentop/README.md#importing-agent-history-agentop-import) |
| Set model prices and gateway discounts | [`pricing.md`](pricing.md). To see the rates in effect, run `agentop pricing [--host <gateway>]` |
| Set a daily spend cap for your agents | [`litellm-budget-track`](litellm-budget-track-plugin.md) |
| Use fewer tokens: remove the tools that your agent never calls | [`laptop-token-savings.md`](laptop-token-savings.md) |
| Allow or block agent requests with a policy | [`opa`](opa-plugin.md), [a script plugin](script-plugin.md), or [`sparc`](sparc-plugin.md) to check tool calls |
| Run, stop or remove the service | [`laptop-service.md`](laptop-service.md) |
| Update Cortex, or turn automatic updates off | [Automatic updates](laptop-service.md#automatic-updates). To update now, run `agentop update` |
| Pause Cortex, or stop one agent from using it | [Three ways to turn it off](laptop-service.md#three-ways-to-turn-it-off) |
| Find your session and cost history on disk, or clear it | [Where your data lives](laptop-service.md#where-your-data-lives) |
| Apply policy to agent traffic in praxis mode | [`praxis-policy.md`](praxis-policy.md) |
| Measure traffic — tokens, latency, errors — **without content** | [`session-export.md`](session-export.md) |
| Keep prompts, completions and tool results as files — **with content** | [`session-dump.md`](session-dump.md) |

## Reference

### Run Cortex in Kubernetes

| If you want to… | Read |
|---|---|
| Move from a laptop to a cluster | [`kubernetes.md`](kubernetes.md) |
| Learn the sidecar shapes | [`architecture.md`](architecture.md) |
| Find which binary serves which deployment shape | [`cmd/README.md`](../cmd/README.md) |
| Keep whole sessions from a cluster as files | [`session-dump.md`](session-dump.md) |
| Run a demo | [`cortex-demos/README.md`](https://github.com/rossoctl/cortex-demos/blob/main/README.md) |
| Validate the tokens of inbound requests | [`jwt-validation`](jwt-validation-plugin.md) |
| Get the right credential for each downstream call | [`token-exchange`](token-exchange-plugin.md), [`token-exchange-aiac`](token-exchange-aiac-plugin.md), [`token-broker`](token-broker-plugin.md) |
| Deny agent actions that do not match the user's intent | [`ibac`](ibac-plugin.md) |
| Refuse requests to private addresses | [`listener.egress_guard`](framework-architecture.md#refusing-private-destinations-listeneregress_guard) |
| Send lineage telemetry over OTLP | [`lineage-telemetry`](lineage-telemetry-plugin.md) |

### Configure and write plugins

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

## Contribute to Cortex

| If you want to… | Read |
|---|---|
| Build, test and lint, and open a pull request | [`CONTRIBUTING.md`](../CONTRIBUTING.md) |
| Find a package in the runtime library | [`core/README.md`](../core/README.md) |
| Find which binary serves which deployment shape | [`cmd/README.md`](../cmd/README.md) |
| Write an IdP plugin or read a wire contract | [Contracts for contributors and integrators](#contracts-for-contributors-and-integrators) |

## In this directory

- [`agents/`](agents/) — one guide per supported agent.
- [`proposals/`](proposals/) — design records for work that has since shipped.
  They are kept for the reasoning, not as current documentation; each carries a
  status line pointing at the doc that describes present behaviour.
- `assets/` — images used by the root README. `assets/agentop/` holds the screens of [Using agentop](using-agentop.md), made by `scripts/readme-demo`.

Dated implementation plans and design specs live in
[`docs/superpowers/`](../docs/superpowers/).
