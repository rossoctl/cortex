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

**Session state is kept on disk.** The plugin keeps the state a session needs between turns
(the summary checkpoint, the last billed input, freeze decisions and similar) in context-guru's
`session_state` table, in a SQLite file. It is always on while the plugin is on; there is no
setting for it. So the state survives a restart and a config reload, and the prompt prefix the
provider has cached stays the same.

- Where: `~/.cortex/context-guru-state.db` on a local install. Otherwise
  `<user cache dir>/cortex/context-guru-state.db` (`$XDG_CACHE_HOME`, or `~/.cache` on Linux).
  In a pod the file goes with the pod's filesystem.
- What: everything the state holds, including text the plugin made from the conversation (the
  summary, extract rewrites, frozen replacements). Mode 0600 in a 0700 directory.
- How long: a key stays 24 hours in memory (`store.state_ttl_seconds` overrides this), and a
  row stays 30 days on disk after its last write. Writes are batched once a second, off the
  request path.
- Fails open: with no usable file, the plugin logs one WARN and keeps the state in memory only.
  With `store.enabled: false` there is no state and no file.
