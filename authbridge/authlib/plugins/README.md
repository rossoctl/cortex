# plugins

Built-in plugins and the open plugin registry. Plugin authoring docs live under
[`authbridge/docs/`](../../docs/):

- Tutorial: [`docs/plugin-tutorial.md`](../../docs/plugin-tutorial.md)
- Reference: [`docs/plugin-reference.md`](../../docs/plugin-reference.md) — config conventions, invocation contract, registration rules
- Framework architecture: [`docs/framework-architecture.md`](../../docs/framework-architecture.md)

## Built-in plugins

| Name | Purpose |
|---|---|
| `jwt-validation` | Inbound JWT signature / issuer / audience verification |
| `token-exchange` | Outbound RFC 8693 token exchange with per-host routes |
| `a2a-parser` | Parse Agent-to-Agent JSON-RPC traffic into `Extensions.A2A` |
| `mcp-parser` | Parse Model Context Protocol traffic into `Extensions.MCP` |
| `inference-parser` | Parse OpenAI-style / Ollama inference traffic into `Extensions.Inference` |
| [`ibac`](../../docs/ibac-plugin.md) | Outbound Intent-Based Access Control: LLM judge denies outbound HTTP that doesn't align with the user's most recent intent. Catches prompt-injection / data-exfiltration attempts |
| `opa` | OPA policy evaluation on inbound and outbound requests via bundle download |

## Reusable building blocks for plugin authors

Cross-cutting helpers live as top-level packages in `authlib/` so plugins can share them without growing a dependency on each other:

| Package | Use |
|---|---|
| [`authlib/llmclient`](../llmclient/) | OpenAI-compatible chat-completions client with JSON-from-prose extraction. Use when a plugin needs an LLM in the loop (policy judges, content scorers, intent matchers). The IBAC plugin's `judge.go` is the in-tree reference consumer. |
| [`authlib/bypass`](../bypass/) | Path / host pattern matchers for skip-list configuration. |
| [`authlib/routing`](../routing/) | Host-to-target route resolver. |

## Registry

Plugins self-register via `RegisterPlugin(name, factory)` from `init()`.
Third-party plugins can register from any Go module and are linked in via
side-effect import. See
[`docs/plugin-reference.md`](../../docs/plugin-reference.md#registering-a-plugin)
for the contract and
[`docs/plugin-tutorial.md`](../../docs/plugin-tutorial.md#step-6--out-of-tree-plugins)
for the walkthrough.
