# Cortex × context-guru — LiteLLM integration test plan and results

**Date:** 2026-10-13
**Branch:** `feat/contextguru-keepalive` (Cortex) with context-guru at `feat/export-keepalive-keeper`
(`go.mod` `replace` to the local worktree).
**Harness:** [`scripts/dev/integration-contextguru-litellm.py`](../../../scripts/dev/integration-contextguru-litellm.py)
**Upstream:** `https://ete-litellm.ai-models.vpc.res.ibm.com` (LiteLLM 1.85.5)

## 1. Goal

Prove, against a real gateway and real cheap models, that the context-guru plugin in Cortex:

1. Accepts Anthropic Messages, OpenAI Chat Completions and OpenAI Responses traffic, streamed and
   not streamed, and compacts it without breaking the answer.
2. Receives the configuration objects that Cortex is given (engine preset, `keep_alive`, `model`).
3. Lets cost — and, if possible, savings — reach the response row.
4. Runs the keep-alive: an ordinary ping, and a ping replaced by a `cache_aware_summarizer` summary.

The engine runs the **extra-high preset** (`preset: xhigh`) in every compaction run.

## 2. How to run

```sh
LITELLM_API_KEY=sk-... python3 -I scripts/dev/integration-contextguru-litellm.py \
  --pricing --static-model azure/gpt-5-mini        # the full run (section 5)
LITELLM_API_KEY=sk-... python3 -I scripts/dev/integration-contextguru-litellm.py \
  --pricing --static-model azure/gpt-5-mini --preset off --only D   # uncompacted baseline
```

Flags: `--keep` keeps the work dir (config, cortex log, every response, session JSON, `tokens.json`).
`--only C,D,A` or `--only D1,D7` selects scenarios. `--pricing` adds a rate override (section 7.3).
`--static-model` sets the plugin's own `model:` block. Exit code is non-zero on any FAIL; a GAP is
a known integration limit and does not fail the run.

What the harness does:

1. Builds `cmd/cortex` with the `full` profile tags plus `include_plugin_contextguru`.
2. Starts cortex with an isolated `HOME`, listeners on free `127.0.0.1` ports in 48700–48799, the
   cost ledger and session archive off, `LOG_LEVEL=debug`, and a `tls_bridge` with a generated CA.
   It does not touch a live install (`:47600`, `~/.cortex`).
3. Sends HTTPS requests to the real LiteLLM host through the forward proxy (`CONNECT`, bridged), with
   `X-Session-Id` per case. The cortex process itself has every proxy env var cleared.
4. Reads `/v1/pipeline`, `/v1/sessions/{id}` and the cortex log to check each scenario.

Cost: the response rows of one `xhigh` run total about $0.07 (run 3). The `extract_llm` calls and the
keep-alive pings are extra; the pings cost $0.004 each, and the `extract_llm` spend is not measured (6.5).

## 3. Request shape

Every case sends two turns in one session. Turn 1 is a system prompt (a ~6k-token "handbook",
above Haiku 4.5's 4096-token cache minimum; Anthropic marks it `cache_control: ephemeral`), then a
tool history: the same 220-line worker log read twice (dedup target), and a 90-row JSON service
inventory (format / toon target), then a question. Turn 2 appends an answer and a second question.
The inventory has 13 `degraded` services, so the answer check is objective.

## 4. Scenarios

| ID | Scenario | Steps | Expected |
|---|---|---|---|
| C1 | Config reaches the plugin | Start cortex with `engine: {preset: xhigh}`, `keep_alive: {idle_seconds: 8, max_pings: 1, min_prefix_tokens: 1}`, `model: {base_url, model, api_key: ${LITELLM_API_KEY}}`. Read `/v1/pipeline` and the log. | The context-guru entry carries the config as written. The log has `context-guru configured … modelConfigured=true` and `keep-alive enabled … idle=8s maxPings=1`. |
| C2 | A bad engine config is refused | Start cortex with `extract_llm.model.api_key` in the engine config. | Cortex exits non-zero. The error contains `model.api_key is no longer a component setting` (the hint added in context-guru `f42b7f4`). |
| D1–D9 | Dialect × model matrix | Send turn 1 and turn 2 per row of the table in section 5. | Both turns return 200. |
| D*/answer | Compaction keeps the meaning | Read the turn-1 answer. | It names the degraded services. |
| D*/compact | The engine compacts | Read the log for the session. | `context-guru compacted request` on both turns, with the components that fired. |
| K1 | Cost reaches the response row | Read each response event's `plugins.cost`. | A settled cost record on every response row. |
| K2 | Savings reach the response row | Read `plugins.cost.avoided`. | A `context-guru` entry. (Known gap — section 6.1.) |
| A1 | Keep-alive ping | Wait `idle_seconds` after the Anthropic sessions' turn 2. | Exactly one ping per Anthropic session: status 200, `cacheRead > 0`. No ping on any OpenAI-wire session. The session API still holds exactly 2 inference requests per case (no re-entry). |
| A2 | Keep-alive with summary | Same sessions, `xhigh` puts `cache_aware_summarizer` in the pipeline. | The summary replaces the ping (`keep-alive ping (via cache_aware_summarizer)`). (Known gap — section 6.2.) |
| baseline | Savings in billed tokens | Re-run D1–D9 with `preset: off`. | Billed prompt tokens are higher than with `xhigh`. |

## 5. Results (2026-10-13, run 3 + baseline run 4)

**0 FAIL, 10 GAP.** Every GAP is one of the two known gaps in section 6 (K2 × 9 rows, A2 × 1).

| ID | Model | Path | Stream | Status | Compacted | Components fired | Cost on row | Answer |
|---|---|---|---|---|---|---|---|---|
| D1 | claude-haiku-4-5-20251001 | /v1/messages | no | 200, 200 | 84.4% (engine) | format dedup toon extract_llm | yes (rate table) | correct |
| D2 | claude-haiku-4-5-20251001 | /v1/messages | yes | 200, 200 | 84.4% | format dedup toon extract_llm | yes (rate table) | correct |
| D3 | azure/gpt-5-mini | /v1/chat/completions | no | 200, 200 | 57.9% | format dedup toon extract_llm | yes | correct |
| D4 | azure/gpt-5.3-codex | /v1/chat/completions | no | 200, 200 | 57.9% | format dedup toon extract_llm | yes | correct |
| D5 | claude-haiku-4-5-20251001 | /v1/chat/completions | no | 200, 200 | 57.9% | format dedup toon extract_llm | yes (gateway header) | correct |
| D6 | azure/gpt-5-mini | /v1/chat/completions | yes | 200, 200 | 57.9% | format dedup toon extract_llm | yes, only with `--pricing` | correct |
| D7 | azure/gpt-5-mini | /v1/responses | no | 200, 200 | 57.9% | format dedup toon extract_llm | yes | correct |
| D8 | azure/gpt-5.3-codex | /v1/responses | no | 200, 200 | 57.9% | format dedup toon extract_llm | yes | correct |
| D9 | azure/gpt-5.3-codex | /v1/responses | yes | 200, 200 | 57.9% | format dedup toon extract_llm | yes, only with `--pricing` | correct |

Other checks: C1 PASS, C2 PASS, A1 PASS (one ping each on D1 and D2: status 200, 10,681 cache-read
tokens, output 1, $0.00395), A1 OpenAI-not-pinged PASS, A1 no-re-entry PASS (18 of 18 rows).

**Billed prompt tokens, `preset: off` → `xhigh`** (from the response's own usage):

| Case | Turn 1 | Turn 2 |
|---|---|---|
| D1 / D2 anthropic haiku | 35,297 → 14,779 (−58%) | 35,326 → 14,808 (−58%) |
| D3 / D6 chat gpt-5-mini | 31,682 → 13,462 (−58%) | 31,714 → 13,494 (−57%) |
| D4 chat codex | 31,604 → 13,384 (−58%) | 31,636 → 13,416 (−58%) |
| D5 chat haiku | 35,301 → 14,783 (−58%) | 35,330 → 14,812 (−58%) |
| D7 responses gpt-5-mini | 31,597 → 13,377 (−58%) | 31,629 → 13,409 (−58%) |
| D8 / D9 responses codex | 31,604 → 13,384 (−58%) | 31,636 → 13,416 (−58%) |

The dollar difference is not reported as a result. The two runs had different provider-side cache
states, so per-row cost moved by cache hits as much as by compaction (D6 cost more with `xhigh`).
A dollar comparison needs a fresh prompt per run (a random nonce in the system prompt).

## 6. Not supported in this integration

Each item below was seen in the live run, and has its cause in the code.

### 6.1 Savings do not reach the response row or the ledger (K2)

Cortex prices a body reduction only for components listed in
[`savingComponents`](../../../core/cost/settle/avoided.go#L135), which is `["tool-prune"]`. The plugin
also publishes no plugin event (`plugins["context-guru"]` is absent on all rows), so `settle.Avoided`
has no facts to price. Result: `avoidedMicros` stays 0 for context-guru sessions, and the saving is
visible only in the log (`context-guru compacted request … tokensSaved=…`).

The *spend* does fall, because the cost is settled on the compacted request's real usage
(section 5). What is missing is the attribution: no row says "context-guru saved $X".

To support it: publish a `context-guru` plugin event with `bytesRemoved` / `bodyBytesAfter`
(the `pruneFacts` shape), and add `"context-guru"` to `savingComponents` with its test.

### 6.2 Keep-alive with summary cannot happen (A2)

`cache_aware_summarizer` never registers a keep-alive candidate in Cortex, so the keeper always logs
`substitute not offered reason=no_candidate` and sends an ordinary ping. Two independent causes, both
seen in the debug log:

1. **No context window.** Cortex calls `apply.BodyOpts` without `Window`
   ([plugin.go:315](../../../core/plugins/contextguru/plugin.go#L315)). The summarizer declines with
   gate `window_not_exact` (and `cg.cache_boundary … ctx_window=0`).
2. **The model adapter is too narrow.** `cgModel`
   ([plugin.go:130](../../../core/plugins/contextguru/plugin.go#L130)) implements only `Complete`. The
   summarizer needs a `components.MessagesModel` or a `components.PrefixAsker`. So even with a window,
   it would decline.

The unit test `TestCortexSubstitutesCacheAwareSummarizerForAPing` proves the keeper side works when a
candidate exists. The live run proves no candidate ever exists. For the same reasons, in Cortex `xhigh`
behaves like `housellm` and `high` like `housellm` minus the sweep: the summarizer is a no-op.

### 6.3 OpenAI keep-alive is not reachable on this gateway

The keeper pings OpenAI only on `/v1/responses` for GPT-5.6 or later
(`keepalive/ping.go` `eligible`, `modelinfo.OpenAI30MinuteCache`). LiteLLM's newest OpenAI model is
`gpt-5.5`. So no OpenAI session is ever pinged here (A1 confirms zero). This is correct behaviour,
but OpenAI ping traffic is untested end to end. An Anthropic model on the OpenAI wire
(`/v1/chat/completions`) is also never pinged, because the plugin maps that path to provider OpenAI.

### 6.4 `extract_llm`'s cheap-model override is ignored

`xhigh` sets `extract_llm.model: {source: incoming, model: claude-haiku-4-5}`. `cgModel` is not a
`components.Remodeler`, so the override is dropped and the call runs on:

- the **agent's own model** on `/v1/chat/completions` (incoming source — the frontier model the
  override exists to avoid), or
- the plugin's **static `model:`** on `/v1/messages` and `/v1/responses`, where the plugin builds no
  incoming client ([plugin.go:423](../../../core/plugins/contextguru/plugin.go#L423)). Without a static
  model, `extract_llm` does not run on those two paths at all (run 1: components were
  `format dedup toon` only, 36–53% saved instead of 58%).

The log line `cg.extract_llm.call … model=claude-haiku-4-5` names the configured id, not the model that
ran (context-guru `components/offload/extract_llm.go:774`), so it is misleading in Cortex.

### 6.5 The compaction model's own spend is invisible

`cgModel` uses `llmclient`, which returns no usage to the engine. Every `cg.extract_llm.call` line
shows `input_tokens=0 output_tokens=0 cost_usd=0`, and the gate `cheap_model_price_unconfigured`
fires. That call goes straight to the gateway (not through the proxy), so neither the session API nor
the cost record sees it. The real cost is on the gateway key only.

### 6.6 Keep-alive spend is in the log only

Each ping logs `context-guru keep-alive ping … costUSD=…` (Cortex emitter) and a second line
`context-guru: cache keep-alive ping …` (the engine's own). Neither reaches a session row, the cost
record or the ledger. This is by design until Cortex has a CostRecorder (spec §9.3); the plugin warns
about it at startup.

### 6.7 Other limits in the plugin, not exercised here

- `marker_mode: full` is capped to `summary` (no expand loop in Cortex), so `xhigh`'s offloaders
  leave breadcrumbs and stash nothing restorable.
- Subagent requests are never recorded for keep-alive (Cortex does not know which subagent).
- A static `model:` speaks OpenAI chat-completions only (`llmclient`).

## 7. Observations (not context-guru defects)

1. **Streamed OpenAI rows are unpriced without a rate override.** LiteLLM sends
   `X-Litellm-Response-Cost` on non-streamed responses only. A streamed response then falls back to
   Cortex's bundled rate table, which has haiku but not `azure/gpt-5-mini` or `azure/gpt-5.3-codex`.
   Run 1 (no override): D6 and D9 had no cost record. Run 3 (`--pricing`): priced.
2. **LiteLLM's cost header omits cache cost.** Cortex detects this and logs
   `pricing: the gateway's cost header omits cache cost, so the rate table was charged`.
3. **The `--pricing` rates are assumptions** (gpt-5-mini $0.25 / $0.025 / $2.00 per million input /
   cache read / output; gpt-5.3-codex $1.75 / $0.175 / $14.00). They make streamed rows priceable;
   they are not authoritative for that gateway.
4. **The engine's `pctSaved` is not the billed saving on Anthropic.** It logged 84.4% for D1; the
   billed prompt fell 58%. The engine measures the tokens it counts, which differ from the
   provider's count of the whole prompt.

## 8. Not covered yet (next tests)

1. OpenAI keep-alive, when the gateway offers a GPT-5.6+ model (`/v1/responses`).
2. A real agent client (Claude Code, Codex CLI) through the bridge, rather than synthetic bodies.
3. `on_error: observe` (shadow mode) and hot-reload of the plugin config with a ping pending.
4. A long session that reaches the `fire_on: pressure` and summarizer thresholds — after 6.2 is fixed.
5. A controlled dollar comparison (nonce per run, section 5).
