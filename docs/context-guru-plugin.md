# `context-guru`

Compacts an agent's outbound LLM request context before forwarding,
using the embedded context-guru engine. `OnResponse` is currently a
pass-through; model-driven expand/restore is a later integration. The
plugin also implements `OnFinish`, `Init`, and `Shutdown` for the
keep-alive feature below, and `Resend` for tool deferral. It ships in the `local`, `full` and
`fullcpex` build profiles (see `scripts/profile-tags`), because its
engine pulls a large transitive dependency set and the `local`
config enables it by default (preset `xhigh`). `lite`, `envoy` and
`cpex` leave it out; a build with none of those tags needs
`-tags include_plugin_contextguru` to link it.

**Headers it removes:** the plugin removes every `x-context-guru-*` header from each outbound request before it goes upstream, on every path. These headers talk to context-guru (for example, `x-context-guru-request-kind: compaction`), and a provider must not see them. The plugin reads them first. Its own model calls and pings keep their `X-Context-Guru-LLM` sentinel.

Inference request paths are fixed, not configurable: `/v1/chat/completions`, `/v1/completions`, `/v1/messages`, `/v1/responses` — the same routes `context-guru-proxy` itself hardcodes. A `/v1/responses` body (OpenAI Responses API: `instructions` + `input`) is parsed as that shape, not as Chat Completions.

- `cheap_model` (object) — optional "cheap" LLM endpoint for model-backed components (summarize, extract:code); omitted means those degrade to deterministic/no-op.
  - `base_url` — OpenAI-compatible endpoint base.
  - `model` — model name to call.
  - `api_key` — optional bearer token.
  - `max_tokens` — completion cap, default 4096.
  - `timeout_ms` — per-call timeout, default 150000.
  - `max_concurrent` — bounds concurrent calls to this model, process-wide, across every request. Default 4 — the same default as `context-guru-proxy`'s `--cheap-model-concurrent` flag, which this mirrors. A call that would exceed it waits for a slot, or returns the request's own context error if that context is canceled first.
- `engine` (object) — native context-guru config (preset / pipeline / per-component / store / cache), passed through verbatim. Default: `preset: xhigh`. It includes the model-calling components (`cache_aware_summarizer`, `extract_llm`, `extract_llm_sweep`). They act only where a model resolves: a `cheap_model` block, or the request's own model (below).
  - `marker_mode` is capped at `summary`, whatever the preset or config says. `full` emits reversible `<<cg:…>>` markers and the `context_guru_expand` tool, and Cortex cannot answer that tool yet (`OnResponse` is a pass-through).
  - `cache.strategy` — picks a named prompt-cache policy: `none`, `auto`, `1-ping`, `2-ping`, `1-hour-head`, `1-hour-on-wait`, `1-hour-always`, `30-min-ping`. Resolved by context-guru itself (`config.LookupStrategy`/`applyStrategy`), not reimplemented in this plugin — see context-guru's own [`docs/how-to/cache-strategies.md`](https://github.com/rossoctl/context-guru/blob/main/docs/how-to/cache-strategies.md). This is the SAME `cache:` block `context-guru-proxy`'s `--config` loads, so a strategy behaves identically in both. Unset: this plugin fills `cache.strategy: auto` itself, but only when the operator's `engine:` YAML has no `cache:` key at all — any operator-authored `cache:` block is left completely alone. This is what keeps keep-alive "on by default on every install" true with no plugin-level default of its own.

**The request's own model (`model.source: incoming`).** A component configured with `model.source: incoming` calls the model the agent itself is using, on the agent's own credential:

- **OpenAI Chat Completions:** through `llmclient`, on the request's endpoint and `Authorization`.
- **Anthropic Messages** (`/v1/messages`, Claude Code's traffic): through context-guru's `cheapmodel.Anthropic`, on the request's endpoint and credential (`x-api-key` or `Authorization`). The credential is held in memory, masked, one hold per credential, and released 2 hours after its last use (on a timer, not only on the next matching request), on a config reload, or at `Shutdown`. The request's own `Anthropic-Beta` oauth-* values (`tooldefer.AuthBetas`) are held too and sent on the call when `cheapmodel` set none: a Claude subscription (OAuth) token can need one to authenticate directly against `api.anthropic.com`, and without it the call 401s — failing open, but silently, since the summarizer then does nothing that turn. It is held, not borrowed, because `cache_aware_summarizer` commissions its call in the background and the request returns first. On Anthropic the summarizer asks one question appended to the previous turn's forwarded body, so the provider's prompt cache serves the whole transcript. The plugin keeps that body in memory for the last 16 threads (main agent only, a request that declares tools, a 200 response only, at most 8 MiB per body and 96 MiB in total). It keys the body by thread, the key the summarizer asks with, so a main conversation that moved to a new thread (after a rewind) still finds its body.
- **OpenAI Responses API:** none. A Responses request uses the `model` block, or the component degrades to deterministic.

Each such call is reported to the request it was made for as a spend op (`pluginSpendMicros`). A call that returns after its request was accounted (the summarizer's background call can) is booked as a detached spend: one cost event with purpose `detached-spend`, priced at the call's own model and prompt size. It adds to `pluginSpendMicros` (or its units and unknown fields) and counts as no request. A late call that cannot be booked (no accountant, or no session) is dropped and counted in the `late_model_calls_dropped` metric.

**Under `on_error: observe`, no model is built at all**, static or incoming: every
model-calling component degrades to deterministic/no-op, and `model.source: incoming`'s
credential is never held. `observe` is a shadow pass — nothing it does is supposed to take
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

  **Upgrade note for clusters:** a cluster that already runs context-guru (for compaction, for example) gets keep-alive with no config change. From then on Cortex holds each tracked thread's credential and last request, masked, and sends billed pings on those credentials. `session-budget` counts the pings' tokens and calls where accounting is on (so a session can reach its limit sooner than before), `litellm-budget-track` does not count them, and off a local install they go through `HTTPS_PROXY`. To keep the old behaviour, set `engine.cache.strategy: none` before you upgrade. Every decision (whether to ping, how much, when to stop) stays inside the context-guru engine, not this plugin.

  **Threads:** each thread of a session has its own cache entry, so the keeper tracks each one apart. The keeper uses the plugin's own thread split ([Threads](#threads)): a named agent (a Claude Code subagent or agent-team teammate, a Bob child, a Codex or OpenCode subagent) is its own thread, and a session's other requests are split by prefix continuity. So a one-shot call (a title, a permission check) is its own entry, and it does not replace the main thread's last turn. The keeper does not ping a thread whose responses called no tool yet. A request on one thread does not end another thread's idle span, and each ping is accounted on its own thread. A request with no usable message list falls back to the agent header: a named agent is its own thread, and any other request is the main thread. Such a request marked as a subagent's (`cc_is_subagent=true`) that sends no agent id is not tracked, because nothing tells it apart from the session's other subagents.

  **Ordering:** `OnFinish` reads `pctx.Headers` after the whole outbound chain has run, so the credential a ping holds is whatever actually went upstream — this holds regardless of where `context-guru` sits relative to a token-exchange or credential-injection plugin. A short-lived exchanged token can still expire before the ping fires; the ping then gets a 4xx from the upstream, which stops keep-alive for that session.

  **Transport:** pings dial the upstream directly with the system root CAs. They do not use `tls_bridge.upstream_ca_bundle` / `upstream_insecure` or the mesh mTLS dialer, so a private-CA or mTLS-only upstream refuses every ping. Ping failures and refusals are logged at DEBUG; a successful ping logs at INFO. A model with no rate in Cortex's pricing table pings with no cost ceiling enforced, and logs `costUSD` 0.

  **Spend:** each ping is billed to the caller's credential and logged as `context-guru keep-alive ping`. Where accounting is on, the plugin also hands the ping to the accountant host (`core/accounting`'s `Host.Ping`). The host refuses a ping with no idle window, a status outside 200-299, or no usage. Otherwise it prices the ping with Cortex's rate table and writes it to the cost ledger and the usage aggregator as a response event, never through the session store: a ping is not agent activity. The event's cost record keeps `source` and `provenance` as for any request (how the cost was arrived at), carries `purpose: keep-alive`, and carries the `basis` of the thread's last request's billing mode, because a ping uses the same credential. The ledger and `/v1/usage` keep a ping out of every request figure: it counts in `keepAlivePings`, never in `requests`, `costMicros`, the tokens or the latency mean. Its spend is split by basis, as [pricing.md](pricing.md#cost-records) describes: only a ping on an API key is money (`keepAliveMicros`); a ping on a subscription login is booked in usage-limit units (`keepAliveUnits`, with its API-equivalent dollars beside them), and one whose billing mode is unknown is shown apart (`keepAliveUnknownMicros`). The metrics push books ping dollars on the same rule. The first ping after a restart has no last request to read, so it is `unknown`. The thread's next priced request then carries the ping's facts, so the accountant credits the cache it kept warm as a `rescue` under the ping's own `ChangeID` (`ka:<thread>:<window>`). A manual ping is priced but credits no rescue. The host also hands each ping to its listeners: the session store adds its spend to the session's `keepAlive*` figures (without counting it as activity), and `session-budget` adds its tokens and one call to the session's counters. agentop includes the API-key pings in every spend figure (`costMicros + keepAliveMicros`) and takes them off every net saving, because the keeper's `rescue` is in the saving. Where accounting is off, the log line is the only record, and no budget counts a ping. `litellm-budget-track` does not count pings.

  **What a ping replays:** the last request of the thread, as it went upstream. A request that a router translated to another protocol, or gave upstream headers to (`inference-router`), is not tracked: its body, path and headers in the pipeline are not the bytes on the wire, so a ping could not replay it.

  **Credential and content retention:** for each tracked thread, Cortex keeps in memory, masked, the caller's credential and the thread's last request body. The body is the whole forwarded conversation, which each ping re-sends. Both are released on that thread's next request, on plugin `Shutdown` or a config reload, or at the thread's hold deadline, whichever comes first. The deadline depends on the cache tier the provider billed:
  - A thread with a 5-minute cache: `(max_pings + 1) × idle_seconds`. With the engine's wait plan on (the default), `max_pings` is `cache.keepalive_max_pings` (2) idle with no particular cause, `cache.keepalive_max_pings + cache.keepalive_human_extra_pings` (2+1=3) waiting on a person, or `cache.keepalive_tool_wait_max_pings` (8) waiting on its own tool call — 12, 16 or 36 minutes respectively at `cache.keepalive_idle_seconds`'s default of 240. (The engine's own classifier also has a background-wait count, but Cortex never gives it the open-task data that cause needs, so a thread here is never classified that way.)
  - A thread whose request asks `ttl: "1h"` on every cache breakpoint, and whose cache the provider billed at the 1-hour tier: the engine's 1-hour schedule, one ping at 59 minutes, so 118 minutes. It is never more than 236 minutes (context-guru's `keepalive.MaxHold1h`). There is no `engine.cache` setting for this schedule.

  **Hot-reload:** every config hot-reload tears down and rebuilds the pipeline, which drops all keep-alive tracking state. During the drain window, the outgoing instance may still send one more ping for a session the new instance no longer tracks.

- `cache_mode` (string) — override whether the engine places prompt-cache breakpoints on the outbound request at all: `"auto"` (default; unset behaves the same), `"on"` or `"off"`. This is `apply.Opts.CacheMode`, threaded through unchanged — the same field and the same three values as `context-guru-proxy`'s `CACHE_MODE` environment variable, so a value means the same thing in both. A bad value is a config error at `Configure`, not a silent fallback to `auto`.

## Threads

The engine splits each session into threads. It keeps its state, and the accountant prices, one thread apart from another. The plugin sets `apply.Opts.ThreadOf` with context-guru's `thread.ResolveSession`, the rule `context-guru-proxy` uses:

1. A request that names its agent is that agent's thread, `a:<agent id>`. The agent id is `pctx.Subagent()`: `X-Claude-Code-Agent-Id`, a Bob child's task id, or a Codex or OpenCode subagent's own id (see `subagent` in [session-api.md](session-api.md)).
2. Any other request goes by prefix continuity. If it continues a thread's history, it is on that thread. If it continues none, it starts a thread of its own, `p:<hash>`. So a session's one-shot calls (a title, a permission check) get their own threads, and they do not look like the main agent dropped context.
3. A request with no usable message list is the session's primary thread.

The accountant thread follows from it:

- The primary thread is `main`, as before.
- A named agent keeps `core/accounting`'s rule: its subagent id, with its parent from `pctx.ParentSubagent()` (see `parentSubagent` in [session-api.md](session-api.md)), or `main`. This holds also when the engine refuses the agent id (it takes only `[A-Za-z0-9_.:@-]`, and Cortex takes any printable id) and gives the request a `p:` thread.
- A prefix-continuity thread is the engine's id, `p:<hash>`. If it continues another thread by content (an edited turn, a rewind), that thread is its parent.

The plugin finds the thread once, at the start of `OnRequest`. The keep-alive keeper, the 1-hour cache choice, the engine, the accountant and `OnFinish` all use that one thread. After the pipeline, the plugin clears a stale keep-alive candidate on that thread, as `context-guru-proxy` does (`offload.ClearStaleKeepAliveCandidate`). The thread state is in memory, one tracker per plugin instance. A config hot-reload drops it.

After each response, `OnFinish` runs context-guru's `lifecycle.Finish`, with the session and thread that the engine used on the request. It does these steps:

1. It releases a `cache_aware_summarizer` summary that the turn deferred. Without this step, the summary waits for the thread's next request or a fallback timer.
2. It runs the warm sweep's question, if the turn registered one.
3. If the response has usage, it keeps the billed input (fresh + cache read + cache write) under the session and under the thread.

The summarizer's size trigger reads only the thread's figure (`Ctx.PrevBilledInputThread`). Before this change Cortex wrote only the session figure, so the trigger declined on every turn and the summarizer never fired (#2130).

## Tool deferral (`preset: ultrahigh`)

`ultrahigh` is `xhigh` plus toolfilter's `defer: auto`. A deferred tool stays in the request with `defer_loading: true`. The provider bills no definition for it, and loads it through its own tool search when the model asks. The decision and the rewrite are context-guru's (`tooldefer.Flow`), so Cortex and `context-guru-proxy` defer the same tools. Before #2150, Cortex accepted `ultrahigh` but deferred nothing.

**Where it runs.** All of these must be true:

- The request is Anthropic Messages (`/v1/messages`).
- The listener can send the original request again when the provider refuses the deferred one (`pipeline.Context.Resendable`). The forward proxy sets it, with no `hop`, and so does `transparentproxy`, which runs on the same `forwardproxy.Server` path. `reverseproxy` and `extproc` (Envoy) do not; there the plugin defers nothing and logs one WARN.
- The plugin runs under `on_error: enforce`.
- There is a session-state database (`stateDB` in the `context-guru configured` log line). Deferral keeps its record of each thread there, and its tool history and probe verdicts in `context-guru-tooldefer.db` beside it.

**What decides a tool.** context-guru's rule. An MCP server is deferred whole, from the history of the user's last 3 sessions, and only when its tools weigh at least 200 tokens together. `toolfilter.always_defer` defers a tool without the history rule, and `toolfilter.keep` never defers it. A model is deferred only after a probe passes. The probe is one small request on the agent's own credential. It runs in the background on the first request that needs it, and only for a model that has a price in Cortex's rate table.

**A custom `toolfilter` block turns deferral off.** A `components.toolfilter` block replaces the preset's whole `toolfilter` block, so it must repeat `defer: auto`:

```yaml
engine:
  preset: ultrahigh
  components:
    toolfilter:
      defer: auto              # without it, ultrahigh defers nothing
      always_defer: [mcp__github]
```

`Configure` logs a WARN when the preset defers tools and the block turned deferral off.

**When the provider refuses.** On a 400 or 422 to a deferred request, the plugin has the forward proxy send the request without deferral (`Resend`, reason `tool_defer_refused`). The client never sees that refusal: Claude Code would turn its tool search off for the session. If the original succeeds, the refusal was deferral's, so the plugin turns deferral off for that model until a restart. If the original is refused because its transcript names a tool that `tools` lacks, the plugin sends it once more with a deferred stub for that tool (`tool_defer_stub`).

**What the accountant books.** The provider bills each deferred tool as a stub, not as the definition that was sent. So the plugin gives the accountant host that billed body (`pipeline.Context.SetBilledBody`, accountant R56), and reports the change as `context-guru.tooldefer` operations. A request sent again books no figure (`upstream_rounds=2`). A deferral that cannot be described as a stub books no figure (`deferral_unplaced`).

**Not ported:** the `ENABLE_TOOL_SEARCH` opt-out (context-guru's `POST /tool-search`), the preset picker's probe ask (`POST /tool-defer/probe`), and the client-search mode for OpenAI wires.

**A side effect, at every preset.** The plugin implements `pipeline.Resender` so it can retry a refused deferred request, but that makes it a `Resender` in every chain it runs in, whatever the preset. On a chain with this plugin, the forward proxy now reads up to 64 KiB of every non-2xx upstream answer before relaying it (`Resendable`'s own cost), even for a request `ultrahigh` never touches. This is unmeasurable for an ordinary error body, but a slow-streaming or open-ended error body is now held back until it ends or that limit is hit, where before it streamed straight through.

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
