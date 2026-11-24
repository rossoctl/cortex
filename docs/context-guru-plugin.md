# `context-guru`

Compacts an agent's outbound LLM request context before forwarding,
using the embedded context-guru engine. `OnResponse` is currently a
pass-through; model-driven expand/restore is a later integration.
Opt-in at build time (`-tags include_plugin_contextguru`) because its
engine pulls a large transitive dependency set.

- `paths` (`[]string`) — inference request paths to compact. Default: `/v1/chat/completions`, `/v1/completions`, `/v1/messages`.
- `model` (object) — optional "cheap" LLM endpoint for model-backed components (summarize, extract:code); omitted means those degrade to deterministic/no-op.
  - `base_url` — OpenAI-compatible endpoint base.
  - `model` — model name to call.
  - `api_key` — optional bearer token.
  - `max_tokens` — completion cap, default 4096.
  - `timeout_ms` — per-call timeout, default 150000.
- `engine` (object) — native context-guru config (preset / pipeline / per-component / store), passed through verbatim. Default: `preset: balanced`.

**The request's own model (`model.source: incoming`).** A component configured with `model.source: incoming` calls the model the agent itself is using, on the agent's own credential. OpenAI Chat Completions only: through `llmclient`, on the request's endpoint and `Authorization`. On other dialects the component uses the `model` block, or degrades to deterministic. Each such call is reported to the request it was made for as a spend op (`pluginSpendMicros`). A call that returns after its request was accounted is dropped and counted in the `late_model_calls_dropped` metric.

**Under `on_error: observe`, no model is built at all**, static or incoming: every
model-calling component degrades to deterministic/no-op. `observe` is a shadow pass — nothing it does is supposed to take
effect — and a real, billed model call on the agent's own credential is the opposite of that.

## Where its own model calls go

The plugin sends its own model calls (summary, extract, and the `model.source: incoming` calls)
itself, through `llmclient`. They do not pass through the Cortex pipeline, so no pipeline plugin
sees them, and they are not in `cost_usd` or in any session budget. Their spend reaches the cost
record as `spends[]` (`pluginSpendMicros`); see [pricing.md](pricing.md#cost-records).

On a local install the calls ignore `HTTPS_PROXY`. A laptop shell that routes agents through
Cortex sets it to Cortex's own listener, and a call sent back there would be priced once as agent
traffic and again as the plugin's spend. Anywhere else the calls use `HTTPS_PROXY` as usual,
because in a cluster an egress proxy may be the only route to the model.
