# plugins

Built-in plugins and the open plugin registry. Plugin authoring docs live under
[`docs/`](../../docs/):

- Tutorial: [`docs/plugin-tutorial.md`](../../docs/plugin-tutorial.md)
- Reference: [`docs/plugin-reference.md`](../../docs/plugin-reference.md) — config conventions, invocation contract, registration rules
- Framework architecture: [`docs/framework-architecture.md`](../../docs/framework-architecture.md)

## Built-in plugins

All seventeen, with config and behavior, in
[`docs/plugin-catalog.md`](../../docs/plugin-catalog.md); each plugin also
has a dedicated reference doc there (`docs/<name>-plugin.md`).

| Name | Purpose |
|---|---|
| `jwt-validation` | Inbound JWT signature / issuer / audience verification |
| `token-exchange` | Outbound RFC 8693 token exchange with per-host routes |
| `a2a-parser` | Parse Agent-to-Agent JSON-RPC traffic into `Extensions.A2A` |
| `mcp-parser` | Parse Model Context Protocol traffic into `Extensions.MCP` |
| `inference-parser` | Parse OpenAI chat completions, Anthropic Messages and OpenAI Responses inference traffic into `Extensions.Inference`, and price the finished response |
| `inference-router` | Send each coding agent's new sessions to the inference server chosen for it; a session stays on the server it started on until moved |
| `ibac` | Outbound Intent-Based Access Control: LLM judge denies outbound HTTP that doesn't align with the user's most recent intent. Catches prompt-injection / data-exfiltration attempts |
| `opa` | OPA policy evaluation on inbound and outbound requests via bundle download |
| `tool-prune` | Remove unused tool definitions from the outbound inference manifest |
| `litellm-budget-track` | Daily spend ledger and budget cap, accumulated from the cost record `inference-parser` settles |
| `static-inject` | Swap a placeholder credential for a real static one on outbound requests |
| `token-broker` | Acquire tokens from an external broker service per host-based routes |
| `lineage-telemetry` | Emit facts-only OTel lineage spans per HTTP exchange |
| `sparc` | Pre-tool reflection against a SPARC reflection service |
| `session-budget` | Per-session token / call / duration budgets via Redis |
| `context-guru` | Compact the outbound LLM request context |
| `cpex` | Bridge hooks to the CPEX policy framework (cgo builds of `cortex` with the `cpex` tag) |

## Reusable building blocks for plugin authors

Cross-cutting helpers live as top-level packages in `core/` so plugins can share them without growing a dependency on each other:

| Package | Use |
|---|---|
| [`core/llmclient`](../llmclient/) | OpenAI-compatible chat-completions client with JSON-from-prose extraction. Use when a plugin needs an LLM in the loop (policy judges, content scorers, intent matchers). The IBAC plugin's `judge.go` is the in-tree reference consumer. |
| [`core/bypass`](../bypass/) | Path / host pattern matchers for skip-list configuration. |
| [`core/routing`](../routing/) | Host-to-target route resolver. |

## Registry

Plugins self-register via `RegisterPlugin(name, factory)` from `init()`.
Third-party plugins can register from any Go module and are linked in via
side-effect import. See
[`docs/plugin-reference.md`](../../docs/plugin-reference.md#registering-a-plugin)
for the contract and
[`docs/plugin-tutorial.md`](../../docs/plugin-tutorial.md#step-6--out-of-tree-plugins)
for the walkthrough.

### Build-tag plugin exclusion

Every plugin is selected at build time using Go build tags. Its side-effect
import lives in a dedicated `plugins_<name>.go` file (in each `cmd/` binary)
gated by `//go:build include_plugin_<name>`. A build with no tags registers
no plugins and rejects any config that names one, so artifacts are built from
named profiles in `scripts/profile-tags`:

| Profile | Carries |
|---------|---------|
| `local` | the three parsers + `tool-prune` |
| `full` | all fourteen non-optional plugins |
| `lite` | jwt-validation, token-exchange, litellm-budget-track, static-inject |
| `envoy`, `cpex` | the sets those binaries shipped historically |

See the [architecture doc](../../docs/architecture.md#build-tag-plugin-selection)
for usage examples and instructions for tagging additional plugins.
