# Push the inference, keepalive, and cache metric families — design

## Intent and why

`metrics_push` (PR #1956 / `feat/accountant-integration`) currently pushes only `llm.savings.*`.
Five of the six central-collector dashboards ported from context-guru's VM stack
(`requests.json`, `machines.json`, `inference.json`, `cache.json`, `fleet.json`) query metric
families (`llm_inference_*`, `llm_keepalive_*`, `llm_cache_*`) that Cortex never emits, so they
show "no data". `savings.json` already works. This adds the missing families so the ported
dashboards show real data, with no dashboard or collector changes.

## Goals

- Emit `llm_inference_requests_total`, `llm_inference_tokens_total`, `llm_inference_cost_usd_total`,
  `llm_inference_estimated_cost_usd_total` from data the accountant already computes per request.
- Emit `llm_keepalive_pings_total`, `llm_keepalive_cost_usd_total`, `llm_keepalive_estimated_cost_usd_total`
  for keep-alive pings (an observation with an empty `RequestID`).
- Emit `llm_cache_misses_total`, `llm_cache_miss_cost_usd_total` for a non-ping observation with
  `CacheRead == 0`.
- Emit `llm_inference_duration_seconds_bucket`, `llm_inference_ttfb_seconds_bucket`,
  `llm_inference_request_bytes_total`, `llm_inference_response_bytes_total`, and a
  status-labeled `llm_inference_requests_total` for the HTTP-transport view the Requests
  dashboard needs, from `SessionEvent` fields the pipeline already populates
  (`Duration`, `Ttfb`, `BytesUp`, `BytesDown`, `StatusCode`), regardless of listener.

## Non-goals

- `llm_inference_added_seconds_bucket` (context-guru's own added-latency figure) — out of scope;
  Cortex has no equivalent measurement today.
- Changing `deploy/central/` (collector config, dashboards) — not needed; per-metric labels pass
  the collector's `transform/cortex` processor untouched (only resource attributes are stripped).
- Changing any listener (`core/listener/forwardproxy`, `extproc`, reverse-proxy) — not needed; the
  data already exists on `pipeline.Context` / `model.SessionEvent`.

## Anti-goals

- Must not add per-request latency to the hot path. Both new call sites are on paths that already
  run once per request off the critical path (`Sink.Observe` runs on a background goroutine per
  `Host.observe`; the new `Recorder` runs where `usage.Aggregator` and the cost ledger already run).
- Must not make metrics_push fail closed. A nil `*accounting.Sink` / nil `Recorder` must still be a
  safe no-op, exactly like the existing `llm.savings.*` sink.

## Constraints

- `core/accounting` may not import context-guru (existing constraint, unrelated to this change).
- The two new `llm_inference_requests_total` producers (Tier 1, scoped to accounted/priced
  traffic; Tier 2, scoped to all proxied HTTP traffic) must not collide under one name with
  different semantics. Tier 2 owns the plain name since it matches what `requests.json`,
  `machines.json`, and `fleet.json` actually query (status-class breakdowns of all traffic).
  Tier 1's accounted-request counter is folded into `llm_inference_tokens_total`/
  `llm_inference_cost_usd_total` instead of emitting a separate total — see Task 1.

## Approaches considered

1. **New instrumentation in each listener** (what the original estimate assumed). Rejected:
   `core/listener/forwardproxy`, `extproc`, and the reverse-proxy would each need separate timing
   and byte-counting code, triplicating logic the pipeline already centralizes.
2. **Hook into `inference-parser`'s existing settle path** (`OnResponseFrame` / `accountant.go`).
   Rejected for Tier 2: that hook fires before the listener has finished computing
   `Duration`/`Ttfb`/`BytesDown` (those are set by the listener after the pipeline response pass),
   and it has no visibility into non-inference traffic (plain proxied requests, denials) that the
   Requests dashboard also wants to count.
3. **A new `session.Recorder`** (chosen), registered via `sessions.AddRecorder` alongside
   `usage.Aggregator` and the cost ledger. It is called once per recorded event, request and
   response phases both, with the exact fields needed, already correct across every listener.

Tier 1 keeps its own approach unchanged — new counters alongside `llm.savings.*` in
`core/accounting/metrics.go`'s `Sink.Observe`, since that is already the one call site for
per-request accountant data (Usage, Result, ping detection).

## The how, as decided

- **Tier 1** location: `core/accounting/metrics.go`, `Sink.Observe`. New instruments created in
  `NewSink` alongside `acmetrics.New`, on the same `metric.MeterProvider`.
- **Tier 2** location: new package `core/accounting/reqstats`. Exports a constructor taking the
  shared `metric.MeterProvider` (the same one `push.Start` returns) and returning a
  `session.Recorder`. `Record` switches on `e.Tunnel` (skip), then `e.Phase`:
  `SessionRequest` → add `e.BytesUp` to `llm_inference_request_bytes_total`;
  `SessionResponse` → add `e.BytesDown` to `llm_inference_response_bytes_total`, record
  `e.Duration`/`e.Ttfb` (seconds, float64) into histograms, add 1 to
  `llm_inference_requests_total` labeled `status_class` (derived from `e.StatusCode`: `"2xx"`,
  `"4xx"`, `"5xx"`, or `"unknown"` when 0).
- **Wiring**: `startMetricsPush` (`cmd/cortex/accounting.go`) returns the `metric.MeterProvider`
  in addition to the `*accounting.Sink` it already returns. `cmd/cortex/main.go` builds the
  `reqstats.Recorder` from it and calls `sessions.AddRecorder(...)` next to `usageAgg`/`costLedger`
  (main.go:746/798), guarded the same way those are (nil-safe, no metrics_push → no-op).
- **Naming**: OTel instrument names use dots (`llm.inference.requests`, matching the existing
  `llm.savings.*` / `llm.proxy.export_failures` convention); the Prometheus exporter's
  dot-to-underscore + `_total`/`_seconds` conversion is what produces the exact names the
  dashboards query.
- **Histogram buckets**: use the OTel SDK's default bucket boundaries (as `llm.savings.*`'s
  instruments do — no custom boundaries exist today) — matches the existing convention; revisit
  only if a reviewer flags the default buckets as wrong for proxy-latency ranges.

## What's left to the builder

- Exact instrument descriptions/units (follow the existing `llm.savings.*` comment style).
- Whether `status_class == "unknown"` (StatusCode 0: denials recorded before a status, dropped
  connections) should still count toward `llm_inference_requests_total` or be excluded — builder's
  call, documented at the point of decision.
- Test coverage: a fake `metric.MeterProvider` (manual reader) replay for both the `Sink` additions
  and the new `Recorder`, following the existing pattern in `core/accounting/push/push_test.go`'s
  `extraReaders`.

## Amendment, 2026-11-20: the outcome and cause labels

The central Cache dashboard groups `llm_keepalive_pings_total` by `outcome` and the two miss
counters by `cause`. Cortex pushed neither label, so its series showed as "Value". Both now
carry context-guru's label values, so the two proxies stack on one axis.

- **`outcome`** (`ok`, `failed`, `timeout`). `Host.Ping` counts every ping the keeper reports,
  once. A priced ping is `ok`. A refused ping is counted by `Sink.CountPing`: `timeout` when the
  report has `TimedOut`, `failed` for a status outside 200-299, else `ok` with no cost. A timeout
  has no usage, so it is booked at the keeper's projected cost (`KeepAliveReport.CostUSD`), in
  both `cost_usd` and `estimated_cost_usd`. The ledger is unchanged: a refused ping still writes
  no cost event.
- **`cause`** (`cold_start`, `ttl_expiry`, `model_changed`, `other`), from `missCause`:
  `model_changed` for a break with `CauseModelChanged` or a `modelChangeOp` ChangeID; else
  `ttl_expiry` when the library's phase is `cold`; else `cold_start` when the phase is `unknown`
  and `RequestEnd` had not seen the thread; else `other`. Cortex sees no body diff, so
  context-guru's `prefix_change`, `compaction_after_rewrite` and `bypass_flip` are `other` here.

## Amendment, 2026-11-23: added_seconds, after all — three Non-goals reversed

#2063 (`cg/11-inference-metrics`) adds `llm_inference_added_seconds_bucket` on top of this spec,
reversing three of its Non-goals and one Tier 2 bullet above:

- `llm_inference_added_seconds_bucket` is no longer out of scope. It is `reqstats.Recorder`'s
  fifth histogram, from the new `model.SessionEvent.Added` field (wall-clock time from listener
  entry to the request reaching the upstream).
- All three listeners (`forwardproxy`, `extproc`, `reverseproxy`) changed after all, each to
  stamp `pipeline.Context.UpstreamSentAt` once, at its own "handed to upstream" point.
- The histograms do NOT use the SDK's default buckets. `added_seconds`, like `duration_seconds`
  and `ttfb_seconds`, uses explicit second-sized boundaries (`TestNew_HistogramsUseSecondSizedBuckets`
  pins them) — the default buckets this spec called for turned out wrong for proxy-latency ranges,
  the exact case the original bullet flagged as the reason to revisit.
- The Tier 2 bullet above (`llm_inference_requests_total` from `StatusCode`) also counts
  `model.SessionDenied` rows, not only `model.SessionResponse`: a policy denial carries a real
  `StatusCode` and `Duration` the same way a response does (reqstats.go).

Still true from the base spec: no `deploy/central/` change, the OTel dot-naming convention, and
the Tier 2 byte/duration/ttfb counters as designed.

## Amendment, 2026-11-25: the Ping failure ratio replaces the Ping outcomes bars

The central Cache dashboard no longer draws `llm_keepalive_pings_total` by `outcome` as bars.
Almost every ping is `ok`, so the bars showed one series and no split. The **Ping failure ratio**
stat takes their place:

- The numerator is the pings with `outcome` `failed` or `timeout` over the selected range.
- The denominator is the sent pings: `ok`, `failed` and `timeout`.
- A `skipped` ping was not sent, and a ping with no `outcome` (a build before 2026-11-20) has an
  unknown outcome. Neither is in the ratio.
- A fleet whose pings all succeed reads 0 %. No ping in the range reads no data.
