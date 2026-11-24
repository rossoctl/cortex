# `context-guru`

Compacts an agent's outbound LLM request context before forwarding,
using the embedded context-guru engine. `OnResponse` is currently a
pass-through; model-driven expand/restore is a later integration. The
plugin also implements `OnFinish`, `Init`, and `Shutdown` for the
keep-alive feature below. It ships in the `local`, `full` and
`fullcpex` build profiles (see `scripts/profile-tags`), because its
engine pulls a large transitive dependency set and the `local`
config enables it by default (preset `xhigh`). `lite`, `envoy` and
`cpex` leave it out; a build with none of those tags needs
`-tags include_plugin_contextguru` to link it.

Inference request paths are fixed, not configurable: `/v1/chat/completions`, `/v1/completions`, `/v1/messages` — the same routes `context-guru-proxy` itself hardcodes.

- `cheap_model` (object) — optional "cheap" LLM endpoint for model-backed components (summarize, extract:code); omitted means those degrade to deterministic/no-op.
  - `base_url` — OpenAI-compatible endpoint base.
  - `model` — model name to call.
  - `api_key` — optional bearer token.
  - `max_tokens` — completion cap, default 4096.
  - `timeout_ms` — per-call timeout, default 150000.
  - `max_concurrent` — bounds concurrent calls to this model, process-wide, across every request. Default 4 — the same default as `context-guru-proxy`'s `--cheap-model-concurrent` flag, which this mirrors. A call that would exceed it waits for a slot, or returns the request's own context error if that context is canceled first.
- `engine` (object) — native context-guru config (preset / pipeline / per-component / store / cache), passed through verbatim. Default: `preset: balanced`.
  - `cache.strategy` — picks a named prompt-cache policy: `none`, `auto`, `1-ping`, `2-ping`, `1-hour-head`, `1-hour-on-wait`, `1-hour-always`, `30-min-ping`. Resolved by context-guru itself (`config.LookupStrategy`/`applyStrategy`), not reimplemented in this plugin — see context-guru's own [`docs/how-to/cache-strategies.md`](https://github.com/rossoctl/context-guru/blob/main/docs/how-to/cache-strategies.md). This is the SAME `cache:` block `context-guru-proxy`'s `--config` loads, so a strategy behaves identically in both. Unset: this plugin fills `cache.strategy: auto` itself, but only when the operator's `engine:` YAML has no `cache:` key at all — any operator-authored `cache:` block is left completely alone. This is what keeps keep-alive "on by default on every install" true with no plugin-level default of its own.

**The request's own model (`model.source: incoming`).** A component configured with `model.source: incoming` calls the model the agent itself is using, on the agent's own credential. OpenAI Chat Completions only: through `llmclient`, on the request's endpoint and `Authorization`. On other dialects the component uses the `model` block, or degrades to deterministic. Each such call is reported to the request it was made for as a spend op (`pluginSpendMicros`). A call that returns after its request was accounted is dropped and counted in the `late_model_calls_dropped` metric.

**Under `on_error: observe`, no model is built at all**, static or incoming: every
model-calling component degrades to deterministic/no-op. `observe` is a shadow pass — nothing it does is supposed to take
effect — and a real, billed model call on the agent's own credential is the opposite of that,
the same reasoning that already keeps the keep-alive keeper from pinging under `observe`.

**Session state is kept on disk.** The plugin keeps the state a session needs between turns
(the summary checkpoint, the last billed input, freeze decisions and similar) in context-guru's
`session_state` table, in a SQLite file. It is always on while the plugin is on; there is no
setting for it. So the state survives a restart and a config reload, and the prompt prefix the
provider has cached stays the same.

- Where: `~/.cortex/context-guru-state.db` on a local install. Otherwise
  `<user cache dir>/cortex/context-guru-state.db` (`$XDG_CACHE_HOME`, or `~/.cache` on Linux).
- In a cluster: the file is lost when the pod's filesystem is replaced (a reschedule, a
  rollout, a pod delete). It survives config reloads, which run inside the same process. A
  container restart (the process exited) also gets a new writable layer, so the file survives
  that only when the cache directory is on a volume that outlives the container, such as an
  `emptyDir` mounted at `$XDG_CACHE_HOME`. Nothing is shared between pods: each pod keeps its
  own state.
- What: everything the state holds, including text the plugin made from the conversation (the
  summary, extract rewrites, frozen replacements). Mode 0600 in a 0700 directory.
- How long: a key stays 24 hours in memory (`store.state_ttl_seconds` overrides this), and a
  row stays 30 days on disk after its last write. Writes are batched once a second, off the
  request path.
- Fails open: with no usable file, the plugin logs one WARN and keeps the state in memory only,
  where a key keeps the engine's default lifetime (10000s), not 24 hours. With
  `store.enabled: false` there is no state and no file.
- Failures after the start: a write or read that fails later (a full disk, a locked file) logs a
  WARN and is counted in the `state_db_errors` metric.
- `context_window` (object) — where model context windows come from, for the engine's fraction-of-window triggers (`cache_aware_summarizer`'s 0.9 among them). Those triggers fire only on an exact window.
  - `model_info` — `litellm` (the LiteLLM public model map, fetched in the background and cached) or `off` (no network). The default depends on where Cortex runs: `litellm` on a local install, `off` anywhere else, so a pod does not fetch the map from the public internet on its first request (and again after every hot reload) unless its config asks. With `off`, only `windows` gives an exact window, so set `windows` or `model_info: litellm` in a cluster to let the fraction triggers fire. An explicit `model_info` always wins.
  - `model_info_url` — overrides the map's URL. Setting it with `model_info` unset turns the map on, on any install.
  - `windows` — model id to window in tokens. Checked first, and treated as exact.
**Keep-alive** pings an idle session's prompt cache before it expires, so the next turn is a cache hit instead of a full re-send. Each ping is billed to the caller's own credential. It is on by default, on every install: whether to ping, how often, and on what schedule is entirely `engine.cache`'s decision (`cache.strategy`, or the `cache.keepalive_*` keys directly — see context-guru's own [`docs/how-to/cache-strategies.md`](https://github.com/rossoctl/context-guru/blob/main/docs/how-to/cache-strategies.md)), not a plugin-level setting. Set `cache.strategy: none` to turn it off.

There used to be a plugin-level `keep_alive` block here (`enabled`, `idle_seconds`, `max_pings`, `max_usd_per_ping`, `min_prefix_tokens`), predating `engine.cache` support. It is retired: every one of its knobs is a `cache.keepalive_*` key on the engine's own config already, so keeping both was keeping two copies of the same decision. A config that still sends a `keep_alive` block gets one `WARN` naming the retired key and is otherwise unaffected — the key is accepted and ignored, not refused, so an old document keeps loading.

  **Upgrade note for clusters:** a cluster that already runs context-guru (for compaction, for example) gets keep-alive with no config change. From then on Cortex holds each tracked thread's credential and last request, masked, and sends billed pings on those credentials. `session-budget` and `litellm-budget-track` do not count the pings, and off a local install they go through `HTTPS_PROXY`. To keep the old behaviour, set `engine.cache.strategy: none` before you upgrade. Every decision (whether to ping, how much, when to stop) stays inside the context-guru engine, not this plugin.

  **Threads:** each thread of a session has its own cache entry, so the keeper tracks each one apart. A Claude Code subagent or agent-team teammate is its own thread, keyed by its `X-Claude-Code-Agent-Id`; every other request is the session's main thread. A request on one thread does not end another thread's idle span, and each ping is accounted on its own thread. A request marked as a subagent's (`cc_is_subagent=true`) that sends no agent id is not tracked, because nothing tells it apart from the session's other subagents.

  **Ordering:** `OnFinish` reads `pctx.Headers` after the whole outbound chain has run, so the credential a ping holds is whatever actually went upstream — this holds regardless of where `context-guru` sits relative to a token-exchange or credential-injection plugin. A short-lived exchanged token can still expire before the ping fires; the ping then gets a 4xx from the upstream, which stops keep-alive for that session.

  **Transport:** pings dial the upstream directly with the system root CAs. They do not use `tls_bridge.upstream_ca_bundle` / `upstream_insecure` or the mesh mTLS dialer, so a private-CA or mTLS-only upstream refuses every ping. Ping failures and refusals are logged at DEBUG; a successful ping logs at INFO. A model with no rate in Cortex's pricing table pings with no cost ceiling enforced, and logs `costUSD` 0.

  **Spend:** each ping is billed to the caller's credential and logged as `context-guru keep-alive ping`. Where accounting is on, the plugin also hands the ping to the accountant host (`core/accounting`'s `Host.Ping`). The host refuses a ping with no idle window, a status outside 200-299, or no usage. Otherwise it prices the ping with Cortex's rate table and writes it to the cost ledger and the usage aggregator as a response event, never through the session store: a ping is not agent activity. The event's cost record keeps `source` and `provenance` as for any request (how the cost was arrived at), carries `purpose: keep-alive`, and carries the `basis` of the thread's last request's billing mode, because a ping uses the same credential. The ledger and `/v1/usage` keep a ping out of every request figure: it counts in `keepAlivePings`, never in `requests`, `costMicros`, the tokens or the latency mean. Its spend is split by basis, as [pricing.md](pricing.md#cost-records) describes: only a ping on an API key is money (`keepAliveMicros`); a ping on a subscription login is booked in usage-limit units (`keepAliveUnits`, with its API-equivalent dollars beside them), and one whose billing mode is unknown is shown apart (`keepAliveUnknownMicros`). The metrics push books ping dollars on the same rule. The first ping after a restart has no last request to read, so it is `unknown`. The thread's next priced request then carries the ping's facts, so the accountant credits the cache it kept warm as a `rescue` under the ping's own `ChangeID` (`ka:<thread>:<window>`). A manual ping is priced but credits no rescue. Where accounting is off, the log line is the only record. Pings are not counted by `session-budget` or `litellm-budget-track`.

  **What a ping replays:** the last request of the thread, as it went upstream. A request that a router translated to another protocol, or gave upstream headers to (`inference-router`), is not tracked: its body, path and headers in the pipeline are not the bytes on the wire, so a ping could not replay it.

  **Credential and content retention:** for each tracked thread, Cortex keeps in memory, masked, the caller's credential and the thread's last request body. The body is the whole forwarded conversation, which each ping re-sends. Both are released on that thread's next request, on plugin `Shutdown` or a config reload, or at the thread's hold deadline, whichever comes first. The deadline depends on the cache tier the provider billed:
  - A thread with a 5-minute cache: `(max_pings + 1) × idle_seconds`, where `max_pings` is `cache.keepalive_max_pings` (2 by default) ordinarily, but up to `cache.keepalive_tool_wait_max_pings` (8 by default) for a thread waiting on its own tool call, since the engine's wait plan (on by default) raises the ping count for that wait. 12 minutes at the plain default, up to 36 minutes for a tool wait, with `cache.keepalive_idle_seconds` at its default of 240.
  - A thread whose request asks `ttl: "1h"` on every cache breakpoint, and whose cache the provider billed at the 1-hour tier: the engine's 1-hour schedule, one ping at 59 minutes, so 118 minutes. It is never more than 236 minutes (context-guru's `keepalive.MaxHold1h`). There is no `engine.cache` setting for this schedule.

  **Hot-reload:** every config hot-reload tears down and rebuilds the pipeline, which drops all keep-alive tracking state. During the drain window, the outgoing instance may still send one more ping for a session the new instance no longer tracks.

- `cache_mode` (string) — override whether the engine places prompt-cache breakpoints on the outbound request at all: `"auto"` (default; unset behaves the same), `"on"` or `"off"`. This is `apply.Opts.CacheMode`, threaded through unchanged — the same field and the same three values as `context-guru-proxy`'s `CACHE_MODE` environment variable, so a value means the same thing in both. A bad value is a config error at `Configure`, not a silent fallback to `auto`.

## Where its own model calls go

The plugin sends two kinds of request itself, outside the pipeline: the `cheap_model` calls of the
LLM-backed components (and an `incoming` model's calls), and the keep-alive pings. Whether they
honour `HTTPS_PROXY` depends on the install:

- **On a local install** they ignore `HTTPS_PROXY`. On a laptop it points at Cortex itself, and a
  call sent back into Cortex's forward proxy would be priced as agent traffic (`costMicros`) and
  also recorded as the plugin's spend (`pluginSpendMicros`): one call in two totals.
- **Anywhere else** they use `HTTPS_PROXY` and `NO_PROXY` as any Go client does, because there
  `HTTPS_PROXY` is normally the cluster's own egress proxy, and where egress is mandatory a call
  that ignored it would fail.

**Caveat:** a cluster that points a pod's `HTTPS_PROXY` at that pod's own Cortex sidecar gets the
double count above for every plugin model call and every ping. Add the model and inference hosts
to `NO_PROXY` there. Sent directly, these calls run no pipeline plugin, so `session-budget` and
`litellm-budget-track` do not count them.
