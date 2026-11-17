# `context-guru`

Compacts an agent's outbound LLM request context before forwarding,
using the embedded context-guru engine. `OnResponse` is currently a
pass-through; model-driven expand/restore is a later integration. The
plugin also implements `OnFinish`, `Init`, and `Shutdown` for the
keep-alive feature below. Opt-in at build time
(`-tags include_plugin_contextguru`) because its engine pulls a large
transitive dependency set.

- `paths` (`[]string`) — inference request paths to compact. Default: `/v1/chat/completions`, `/v1/completions`, `/v1/messages`.
- `model` (object) — optional "cheap" LLM endpoint for model-backed components (summarize, extract:code); omitted means those degrade to deterministic/no-op.
  - `base_url` — OpenAI-compatible endpoint base.
  - `model` — model name to call.
  - `api_key` — optional bearer token.
  - `max_tokens` — completion cap, default 4096.
  - `timeout_ms` — per-call timeout, default 150000.
- `engine` (object) — native context-guru config (preset / pipeline / per-component / store), passed through verbatim. Default: `preset: balanced`.
- `context_window` (object) — where model context windows come from, for the engine's fraction-of-window triggers (`cache_aware_summarizer`'s 0.9 among them). Those triggers fire only on an exact window.
  - `model_info` — `litellm` (default: the LiteLLM public model map, fetched in the background and cached) or `off` (no network).
  - `model_info_url` — overrides the map's URL.
  - `windows` — model id to window in tokens. Checked first, and treated as exact.
- `keep_alive` (object) — opt-in. Pings an idle session's prompt cache before it expires, so the next turn is a cache hit instead of a full re-send. Off by default; every decision (whether to ping, how much, when to stop) stays inside the context-guru engine, not this plugin.
  - `enabled` (bool) — turn keep-alive on. Default: `false`.
  - `idle_seconds` (int) — idle time before the first ping, measured from the previous request's start. Default: `270`.
  - `max_pings` (int) — most pings sent in one idle span. Default: `2`.
  - `max_usd_per_ping` (float) — refuse a ping whose projected cost exceeds this, in USD. Default: `0.50`.
  - `min_prefix_tokens` (int) — billed prefix a session must reach before it becomes eligible for a ping. Default: `80000`.

  **Ordering:** `OnFinish` reads `pctx.Headers` after the whole outbound chain has run, so the credential a ping holds is whatever actually went upstream — this holds regardless of where `context-guru` sits relative to a token-exchange or credential-injection plugin. A short-lived exchanged token can still expire before the ping fires; the ping then gets a 4xx from the upstream, which stops keep-alive for that session.

  **Transport:** pings dial the upstream directly with the system root CAs. They do not use `tls_bridge.upstream_ca_bundle` / `upstream_insecure` or the mesh mTLS dialer, so a private-CA or mTLS-only upstream refuses every ping. Ping failures and refusals are logged at DEBUG; a successful ping logs at INFO. A model with no rate in Cortex's pricing table pings with no cost ceiling enforced, and logs `costUSD` 0.

  **Spend:** each ping is billed to the caller's credential and logged as `context-guru keep-alive ping`. Where accounting is on, the plugin also hands the ping to the accountant host (`core/accounting`'s `Host.Ping`). The host refuses a ping with no idle window, a status outside 200-299, or no usage. Otherwise it prices the ping with Cortex's rate table and hands it to the cost ledger and `/v1/usage` as a response event, never through the session store: a ping is not agent activity. The event's cost record keeps `source` and `provenance` as for any request (how the cost was arrived at) and carries `purpose: keep-alive`. The ledger and `/v1/usage` do not read `purpose` yet, so today they fold ping spend into the same rows and totals as the agent's own requests, each ping counting as one request. The ping's billing mode is that of the thread's last request. The thread's next priced request then carries the ping's facts, so the accountant credits the cache it kept warm as a `rescue` under the ping's own `ChangeID` (`ka:<thread>:<window>`). A manual ping is priced but credits no rescue. Where accounting is off, the log line is the only record.

  **Credential retention:** Cortex keeps the caller's credential in memory, masked, for up to `(max_pings + 1) × idle_seconds` per tracked session — about 13.5 minutes at the defaults above. It is released on that session's next request, on plugin `Shutdown` or a config reload, or at that deadline, whichever comes first.

  **Hot-reload:** every config hot-reload tears down and rebuilds the pipeline, which drops all keep-alive tracking state. During the drain window, the outgoing instance may still send one more ping for a session the new instance no longer tracks.
