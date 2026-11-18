# Inference/Keepalive/Cache Metrics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Push the `llm_inference_*`, `llm_keepalive_*`, and `llm_cache_*` metric families so the
ported central-collector dashboards (Requests, Machines, Inference, Cache, Fleet) show real data,
with no listener or collector changes.

**Architecture:** Tier 1 (requests/tokens/cost/keepalive/cache-miss) adds new OTel instruments
directly inside `core/accounting/metrics.go`'s existing `Sink.Observe`, the one call site that
already has `accountant.Usage`/`accountant.Result`/`accountant.RequestMeta` per request. Tier 2
(duration/ttfb/bytes/status) adds a new `session.Recorder` in a new package
`core/accounting/reqstats`, reading fields `pipeline.go` already computes onto every
`model.SessionEvent` regardless of listener. Both share the one `metric.MeterProvider`
`push.Start` already builds.

**Tech Stack:** Go 1.26, `go.opentelemetry.io/otel/metric` + `go.opentelemetry.io/otel/sdk/metric`
(already a dependency via `core/accounting/push`), `github.com/rossoctl/accountant` v0.3.0 (already
pinned).

**Spec:** [docs/superpowers/specs/2026-11-18-inference-metrics-design.md](../specs/2026-11-18-inference-metrics-design.md)

## Global Constraints

- `core/accounting` must not import context-guru.
- A nil `*accounting.Sink` / a `reqstats.Recorder` built from a nil `MeterProvider` must be a safe
  no-op — metrics_push failing or being disabled must not affect request handling (fail open).
- No change to any listener (`core/listener/forwardproxy`, `extproc`, reverse-proxy) or to
  `deploy/central/`.
- OTel instrument names use dots (`llm.inference.*`, `llm.keepalive.*`, `llm.cache.*`), matching
  the existing `llm.savings.*` / `llm.proxy.export_failures` convention; the Prometheus exporter's
  naming conversion produces the underscore names the dashboards query.
- Histogram instruments use the OTel SDK's default bucket boundaries (no custom boundaries exist
  today for `llm.savings.*` either).

## Review Focus

- **A ping observation** (`meta.RequestID == ""`) must land only in `llm.keepalive.*`, never in
  `llm.inference.tokens`/`llm.inference.cost_usd`/`llm.inference.estimated_cost_usd` — a ping is
  not an inference request. Task 1's tests pin this.
- **A `Sink` built from a nil `MeterProvider`** (`NewSink(nil, rates)` returns `nil`, per existing
  code at `metrics.go:69`) must mean `Observe` on it is still a safe no-op — already guarded by the
  existing `s == nil` check, but the new counters must be created (or skipped) inside the same
  `NewSink` path so a nil provider never reaches `mp.Meter(...)`. Task 1's tests pin this.
- **A tunnel `SessionEvent`** (`e.Tunnel == true`) must be skipped by the Tier 2 recorder —
  opaque bytes carry no HTTP status or meaningful duration semantics for these dashboards. Task 2's
  tests pin this.
- **A response-phase `SessionEvent` with `StatusCode == 0`** (recorded before a status was known —
  a denial or connection reset) must not silently vanish from `llm_inference_requests_total`'s
  count without a decision being visible — Task 2 records it under `status_class="unknown"` rather
  than dropping it, and a test pins that count is not lost.
- **A `*accounting.Sink` built with Tier 1 enabled must still agree with the existing
  `llm.savings.*` counters to the micro** on a shared fixture — Task 1's test reuses
  `maskResult()` / `testRates` so the new counters are checked against the same priced figures
  `TestSink_SavedMatchesTheLedgerToTheMicro` already proves correct, not a second, divergent
  computation.

---

### Task 1: Tier 1 — inference, keepalive, and cache-miss counters in `core/accounting`

**Files:**
- Modify: `core/accounting/metrics.go` (add instruments to `NewSink`, record them in `Observe`)
- Test: `core/accounting/metrics_test.go` (add cases; reuses existing `manual`, `sum`, `testRates`,
  `apiBilling`, `maskResult` helpers already in that file)

**Interfaces:**
- Consumes: `Sink.Observe(meta accountant.RequestMeta, u accountant.Usage, res accountant.Result, endpoint string, promptTotal int)`
  (existing signature, unchanged) — `u.Input`, `u.CacheRead`, `u.CacheWrite`, `u.CacheWrite1h`
  (`int`), `res.Unpriced` (`[]accountant.Unpriced`, non-empty means this observation is estimated),
  `meta.RequestID` (`string`, empty means a ping). Pricing: reuse `s.pricer` (already set just
  above the existing `s.sink.Observe` call) and the existing `USD(resolver, endpoint, model,
  promptTotal, tier, count)` / `pricingTier` helpers already in this file to price `u`'s four
  tiers at `s.pricer.resolver`, `s.pricer.request.endpoint`, `meta.Model`, `s.pricer.request.promptTotal`.
- Produces: nothing new is exported — these are internal instruments on the `Sink` struct, pushed
  automatically by the shared `MeterProvider` `NewSink` already receives.

New instruments (all created in `NewSink`, stored as new fields on `Sink`, created with
`mp.Meter(MeterName)` — the same meter `acmetrics.New` uses):

| Name | Kind | Recorded when |
|---|---|---|
| `llm.inference.tokens` | `Int64Counter`, attribute `tier` (`"input"`/`"cache_read"`/`"cache_write"`/`"cache_write_1h"`) | non-ping observation, one `Add` per non-zero tier |
| `llm.inference.cost_usd` | `Float64Counter` | non-ping observation, `u` priced at `s.pricer` |
| `llm.inference.estimated_cost_usd` | `Float64Counter` | non-ping observation with `len(res.Unpriced) > 0` |
| `llm.keepalive.pings` | `Int64Counter` | ping observation (`meta.RequestID == ""`) |
| `llm.keepalive.cost_usd` | `Float64Counter` | ping observation, `u` priced at `s.pricer` |
| `llm.keepalive.estimated_cost_usd` | `Float64Counter` | ping observation with `len(res.Unpriced) > 0` |
| `llm.cache.misses` | `Int64Counter` | non-ping observation with `u.CacheRead == 0` |
| `llm.cache.miss_cost_usd` | `Float64Counter` | non-ping observation with `u.CacheRead == 0`, `u` priced at `s.pricer` |

A failure to create any instrument in `NewSink` is handled exactly like the existing
`acmetrics.New` failure a few lines above: log a `slog.Warn` naming which instrument, and return
`nil` (no sink at all) — do not partially wire a `Sink` with some counters missing, which would
make `Observe` branch on which fields are non-nil.

- [ ] **Step 1: Write the failing tests**

```go
// A ping must not be counted as an inference token/cost observation.
func TestSink_PingCountsOnlyUnderKeepalive(t *testing.T) {
	mp, rd := manual(t)
	rates := testRates(pricing.ProvConfigured)
	s := NewSink(mp, rates)
	meta := accountant.RequestMeta{RequestID: "", Session: "s1", Model: "claude-sonnet-4-5", At: time.Now(), Billing: apiBilling}
	u := accountant.Usage{Input: 100, CacheRead: 50}
	s.Observe(meta, u, accountant.Result{}, "api.example.com", 150)

	if got := sum(t, rd, "llm.inference.tokens", nil); got != 0 {
		t.Errorf("ping counted under llm.inference.tokens: %v", got)
	}
	if got := sum(t, rd, "llm.keepalive.pings", nil); got != 1 {
		t.Errorf("llm.keepalive.pings = %v, want 1", got)
	}
}

// A non-ping request with no cache read is a cache miss, priced the same as the savings sink prices it.
func TestSink_CacheMissCountsAndPrices(t *testing.T) {
	mp, rd := manual(t)
	rates := testRates(pricing.ProvConfigured)
	s := NewSink(mp, rates)
	meta := accountant.RequestMeta{RequestID: "r1", Session: "s1", Model: "claude-sonnet-4-5", At: time.Now(), Billing: apiBilling}
	u := accountant.Usage{Input: 100}
	s.Observe(meta, u, accountant.Result{}, "api.example.com", 100)

	if got := sum(t, rd, "llm.cache.misses", nil); got != 1 {
		t.Errorf("llm.cache.misses = %v, want 1", got)
	}
	want := USD(rates, "api.example.com", "claude-sonnet-4-5", 100, pricing.TierInput, 100)
	if got := sum(t, rd, "llm.cache.miss_cost_usd", nil); !micro(got, want) {
		t.Errorf("llm.cache.miss_cost_usd = %v, want %v", got, want)
	}
}

// A request with a cache read is not a cache miss.
func TestSink_CacheReadIsNotAMiss(t *testing.T) {
	mp, rd := manual(t)
	rates := testRates(pricing.ProvConfigured)
	s := NewSink(mp, rates)
	meta := accountant.RequestMeta{RequestID: "r1", Session: "s1", Model: "claude-sonnet-4-5", At: time.Now(), Billing: apiBilling}
	u := accountant.Usage{Input: 100, CacheRead: 50}
	s.Observe(meta, u, accountant.Result{}, "api.example.com", 150)

	if got := sum(t, rd, "llm.cache.misses", nil); got != 0 {
		t.Errorf("llm.cache.misses = %v, want 0", got)
	}
}

// An unpriced result's inference cost is estimated, not actual.
func TestSink_UnpricedResultIsEstimated(t *testing.T) {
	mp, rd := manual(t)
	rates := testRates(pricing.ProvConfigured)
	s := NewSink(mp, rates)
	meta := accountant.RequestMeta{RequestID: "r1", Session: "s1", Model: "claude-sonnet-4-5", At: time.Now(), Billing: apiBilling}
	u := accountant.Usage{Input: 100, CacheRead: 50}
	res := accountant.Result{Unpriced: []accountant.Unpriced{{OpID: "op1"}}}
	s.Observe(meta, u, res, "api.example.com", 150)

	if got := sum(t, rd, "llm.inference.estimated_cost_usd", nil); got <= 0 {
		t.Errorf("llm.inference.estimated_cost_usd = %v, want > 0", got)
	}
	if got := sum(t, rd, "llm.inference.cost_usd", nil); got != 0 {
		t.Errorf("llm.inference.cost_usd = %v, want 0 (should be counted as estimated, not actual)", got)
	}
}

// NewSink(nil, rates) must still return nil — no partial wiring.
func TestNewSink_NilProviderStillReturnsNil(t *testing.T) {
	rates := testRates(pricing.ProvConfigured)
	if s := NewSink(nil, rates); s != nil {
		t.Errorf("NewSink(nil, rates) = %v, want nil", s)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./core/accounting/... -run 'TestSink_PingCountsOnlyUnderKeepalive|TestSink_CacheMissCountsAndPrices|TestSink_CacheReadIsNotAMiss|TestSink_UnpricedResultIsEstimated|TestNewSink_NilProviderStillReturnsNil' -v`
Expected: FAIL (metric names not found / instruments don't exist yet)

- [ ] **Step 3: Implement the new instruments and `Observe` wiring in `core/accounting/metrics.go`**

Add the eight instruments as new `Sink` fields, created in `NewSink` right after the existing
`acmetrics.New` call, same error-handling style (`slog.Warn` + `return nil` on any creation
error). Add the recording logic as a new unexported method, e.g. `observeInferenceMetrics(meta,
u, res)`, called from `Observe` right after the existing `s.sink.Observe(...)` line, guarded by
the same `s == nil || s.sink == nil` nil check `Observe` already has at its top (so no second nil
check is needed inside the new method). Use `meta.RequestID == ""` for ping detection and
`u.CacheRead == 0` for a miss, each only evaluated on the non-ping branch.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./core/accounting/... -run 'TestSink_PingCountsOnlyUnderKeepalive|TestSink_CacheMissCountsAndPrices|TestSink_CacheReadIsNotAMiss|TestSink_UnpricedResultIsEstimated|TestNewSink_NilProviderStillReturnsNil' -v`
Expected: PASS

- [ ] **Step 5: Run the full accounting package test suite to confirm no regression**

Run: `go test ./core/accounting/... -v`
Expected: PASS (including the pre-existing `TestSink_SavedMatchesTheLedgerToTheMicro`)

- [ ] **Step 6: Commit**

```bash
git add core/accounting/metrics.go core/accounting/metrics_test.go
git commit -s -m "feat(accounting): Push llm.inference/keepalive/cache metrics"
```

---

### Task 2: Tier 2 — `core/accounting/reqstats`, a `session.Recorder` for duration/ttfb/bytes/status

**Files:**
- Create: `core/accounting/reqstats/reqstats.go`
- Test: `core/accounting/reqstats/reqstats_test.go`

**Interfaces:**
- Consumes: `model.SessionEvent` fields (`Phase model.SessionPhase` — `model.SessionRequest` /
  `model.SessionResponse`; `Tunnel bool`; `StatusCode int`; `Duration time.Duration`; `Ttfb
  time.Duration`; `BytesUp int64`; `BytesDown int64`), all already populated by
  `core/pipeline/pipeline.go:715` before any `session.Recorder.Record` is called.
- Produces: `func New(mp metric.MeterProvider) session.Recorder` — returns a no-op `session.Recorder`
  when `mp == nil`, exactly as `accounting.NewSink` does for a nil provider. The returned value's
  `Record(sessionID string, e *model.SessionEvent)` method is what Task 3 registers via
  `sessions.AddRecorder(...)`.

New instruments (all created in `New`, using `mp.Meter("github.com/rossoctl/cortex/core/accounting/reqstats")`):

| Name | Kind | Recorded when |
|---|---|---|
| `llm.inference.requests` | `Int64Counter`, attribute `status_class` (`"2xx"`/`"4xx"`/`"5xx"`/`"unknown"`) | `e.Phase == model.SessionResponse && !e.Tunnel` |
| `llm.inference.duration_seconds` | `Float64Histogram` | same condition, `e.Duration.Seconds()` |
| `llm.inference.ttfb_seconds` | `Float64Histogram` | same condition, only when `e.Ttfb > 0` (per `model.SessionEvent.Ttfb`'s own doc: zero means "never measured", not "zero latency") |
| `llm.inference.response_bytes` | `Int64Counter` | same condition, `e.BytesDown` |
| `llm.inference.request_bytes` | `Int64Counter` | `e.Phase == model.SessionRequest && !e.Tunnel`, `e.BytesUp` |

`status_class` derivation: `e.StatusCode / 100` gives `2`/`4`/`5` for the normal cases; `0` (or any
value outside 100–599) maps to `"unknown"`. This is a pure function, e.g. `statusClass(code int)
string`, unit-testable on its own.

- [ ] **Step 1: Write the failing tests**

```go
package reqstats

import (
	"context"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/model"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func manual(t *testing.T) (*sdkmetric.MeterProvider, *sdkmetric.ManualReader) {
	t.Helper()
	rd := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(rd))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	return mp, rd
}

func sum(t *testing.T, rd *sdkmetric.ManualReader, name string) float64 {
	// same shape as core/accounting's sum() helper: collect, find name, total every data point
	// across every attribute set. (Implementer: copy that helper's body; no attribute filter
	// needed for these tests — assert count/attributes separately where the test cares.)
	t.Helper()
	var rm metricdata.ResourceMetrics
	_ = rd.Collect(context.Background(), &rm)
	var total float64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					total += float64(p.Value)
				}
			case metricdata.Sum[float64]:
				for _, p := range d.DataPoints {
					total += p.Value
				}
			case metricdata.Histogram[float64]:
				for _, p := range d.DataPoints {
					total += float64(p.Count)
				}
			}
		}
	}
	return total
}

func TestRecord_ResponsePhaseCountsRequestAndDuration(t *testing.T) {
	mp, rd := manual(t)
	r := New(mp)
	r.Record("s1", &model.SessionEvent{Phase: model.SessionResponse, StatusCode: 200, Duration: 2 * time.Second, Ttfb: 500 * time.Millisecond, BytesDown: 1024})

	if got := sum(t, rd, "llm.inference.requests"); got != 1 {
		t.Errorf("llm.inference.requests = %v, want 1", got)
	}
	if got := sum(t, rd, "llm.inference.duration_seconds"); got != 1 {
		t.Errorf("llm.inference.duration_seconds count = %v, want 1", got)
	}
	if got := sum(t, rd, "llm.inference.response_bytes"); got != 1024 {
		t.Errorf("llm.inference.response_bytes = %v, want 1024", got)
	}
}

func TestRecord_RequestPhaseCountsOnlyBytesUp(t *testing.T) {
	mp, rd := manual(t)
	r := New(mp)
	r.Record("s1", &model.SessionEvent{Phase: model.SessionRequest, BytesUp: 512})

	if got := sum(t, rd, "llm.inference.request_bytes"); got != 512 {
		t.Errorf("llm.inference.request_bytes = %v, want 512", got)
	}
	if got := sum(t, rd, "llm.inference.requests"); got != 0 {
		t.Errorf("llm.inference.requests = %v, want 0 on a request-phase row", got)
	}
}

func TestRecord_TunnelRowIsSkipped(t *testing.T) {
	mp, rd := manual(t)
	r := New(mp)
	r.Record("s1", &model.SessionEvent{Phase: model.SessionResponse, Tunnel: true, StatusCode: 200, BytesDown: 999})

	if got := sum(t, rd, "llm.inference.requests"); got != 0 {
		t.Errorf("tunnel row counted: llm.inference.requests = %v, want 0", got)
	}
	if got := sum(t, rd, "llm.inference.response_bytes"); got != 0 {
		t.Errorf("tunnel row counted: llm.inference.response_bytes = %v, want 0", got)
	}
}

func TestRecord_ZeroStatusCodeIsUnknownNotDropped(t *testing.T) {
	mp, rd := manual(t)
	r := New(mp)
	r.Record("s1", &model.SessionEvent{Phase: model.SessionResponse, StatusCode: 0, Duration: time.Second})

	if got := sum(t, rd, "llm.inference.requests"); got != 1 {
		t.Errorf("a StatusCode-0 row must still be counted: llm.inference.requests = %v, want 1", got)
	}
}

func TestRecord_ZeroTtfbIsNotRecorded(t *testing.T) {
	mp, rd := manual(t)
	r := New(mp)
	r.Record("s1", &model.SessionEvent{Phase: model.SessionResponse, StatusCode: 200, Duration: time.Second, Ttfb: 0})

	if got := sum(t, rd, "llm.inference.ttfb_seconds"); got != 0 {
		t.Errorf("llm.inference.ttfb_seconds count = %v, want 0 (Ttfb==0 means unmeasured)", got)
	}
}

func TestNew_NilProviderIsSafe(t *testing.T) {
	r := New(nil)
	r.Record("s1", &model.SessionEvent{Phase: model.SessionResponse, StatusCode: 200}) // must not panic
}

func TestStatusClass(t *testing.T) {
	cases := map[int]string{200: "2xx", 201: "2xx", 404: "4xx", 500: "5xx", 0: "unknown", 999: "unknown"}
	for code, want := range cases {
		if got := statusClass(code); got != want {
			t.Errorf("statusClass(%d) = %q, want %q", code, got, want)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./core/accounting/reqstats/... -v`
Expected: FAIL (package `reqstats` does not exist yet)

- [ ] **Step 3: Implement `core/accounting/reqstats/reqstats.go`**

```go
package reqstats

func New(mp metric.MeterProvider) session.Recorder
func statusClass(code int) string
```

`New` mirrors `accounting.NewSink`'s nil-provider and instrument-creation-failure handling: `mp ==
nil` or any `mp.Meter(...).NewXxx` error returns a no-op `session.Recorder` (an unexported type
whose `Record` does nothing), logged with `slog.Warn` on the error case only. The returned
recorder's `Record` method implements the table above.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./core/accounting/reqstats/... -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add core/accounting/reqstats
git commit -s -m "feat(accounting): Add reqstats, a session.Recorder for inference duration/bytes/status metrics"
```

---

### Task 3: Wire both tiers into `cmd/cortex`

**Files:**
- Modify: `cmd/cortex/accounting.go:56-71` (`startMetricsPush` return signature)
- Modify: `cmd/cortex/main.go` (near line 598-599 and line 746)
- Test: `cmd/cortex/main_test.go` (new, self-contained test — do not extend
  `TestMain_WiresTheLedgerAndFlushesItOnFatalPaths` at line 377, which is a ledger-specific
  fatal-path check; write a separate AST walk, same technique, scoped only to `AddRecorder`)

**Interfaces:**
- Consumes: `reqstats.New(mp metric.MeterProvider) session.Recorder` (Task 2); `push.Start`'s
  existing return `(metric.MeterProvider, func(context.Context) error, error)` (unchanged).
- Produces: `startMetricsPush(c *config.Config, rates pricing.Resolver) (*accounting.Sink,
  metric.MeterProvider, func(context.Context) error)` — same as today plus the raw
  `metric.MeterProvider` as the second return value (nil on every no-push path, matching the
  existing nil-`Sink` cases on those same paths).

- [ ] **Step 1: Write the failing test**

In `cmd/cortex/main.go`, assign the recorder to a local named `reqStats` before passing it —
`reqStats := reqstats.New(mp)` then `sessions.AddRecorder(reqStats)` — so a plain
`*ast.Ident` argument check (the same technique `TestMain_WiresTheLedgerAndFlushesItOnFatalPaths`
uses for `costLedger` at line 419-424) can find it, rather than needing to match a `CallExpr`
argument.

```go
// cmd/cortex/main_test.go — new, self-contained test (parses main.go independently; does not
// share state with TestMain_WiresTheLedgerAndFlushesItOnFatalPaths).
func TestMain_PassesReqStatsRecorderToAddRecorder(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	addRecorder := false
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "AddRecorder" {
			return true
		}
		for _, a := range call.Args {
			if id, ok := a.(*ast.Ident); ok && id.Name == "reqStats" {
				addRecorder = true
			}
		}
		return true
	})
	if !addRecorder {
		t.Error("main.go never passes reqStats to AddRecorder: Tier 2 metrics would never be recorded")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/cortex/... -run TestMain_PassesReqstatsRecorderToAddRecorder -v`
Expected: FAIL

- [ ] **Step 3: Update `startMetricsPush` in `cmd/cortex/accounting.go`**

Change its return type to `(*accounting.Sink, metric.MeterProvider, func(context.Context) error)`.
On every existing no-push path (`err != nil` from `push.Start`), return `nil, nil,
func(context.Context) error { return nil }`. On the success path, return `accounting.NewSink(mp,
rates), mp, shutdown`.

- [ ] **Step 4: Update the call site and recorder registration in `cmd/cortex/main.go`**

At the existing call (around line 598), capture the new second return value as `mp` alongside
`sink`/`stopMetricsPush`. Near line 746, where `sessions.AddRecorder(usageAgg)` already runs, add:

```go
reqStats := reqstats.New(mp)
sessions.AddRecorder(reqStats)
```

Unconditionally — `reqstats.New(nil)` is a safe no-op per Task 2, so no extra nil-guard is needed
here, matching how `accounting.NewSink(nil, ...)` is already handled upstream. The local variable
name `reqStats` is pinned (not just `mp` inline) because Step 1's test matches it by name.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./cmd/cortex/... -v`
Expected: PASS

- [ ] **Step 6: Build the binary to confirm it compiles end-to-end**

Run: `go build ./...`
Expected: success, no errors

- [ ] **Step 7: Commit**

```bash
git add cmd/cortex/accounting.go cmd/cortex/main.go cmd/cortex/main_test.go
git commit -s -m "feat(cortex): Wire reqstats into the session recorder chain"
```

---

## Self-Review

**Spec coverage:** All nine named metrics in the spec's Goals section are covered: four Tier-1
inference metrics + 2 keepalive + 2 cache-miss in Task 1 (8 total — the spec's
`llm_inference_requests_total` for Tier 1 was explicitly folded away per the spec's own
Constraints section, leaving Tier 1 with exactly `tokens`/`cost_usd`/`estimated_cost_usd` plus the
keepalive/cache pairs), and the five Tier-2 transport metrics in Task 2. Non-goals
(`llm_inference_added_seconds_bucket`, collector/dashboard changes, listener changes) are not
built anywhere in this plan. Wiring (the one piece with no dedicated spec section but required for
either tier to run) is Task 3.

**Step scan:** Each test step gives exact assertions against named metrics; each code step gives
exact signatures and leaves only the mechanical body (the same shape the file's neighboring code
already uses) to the implementer.

**Type consistency:** `reqstats.New(mp metric.MeterProvider) session.Recorder` (Task 2) is
consumed with that exact signature in Task 3 Step 4. `startMetricsPush`'s new three-value return
(Task 3 Step 3) matches its call-site update (Task 3 Step 4). `model.SessionPhase`,
`model.SessionRequest`, `model.SessionResponse` are used consistently between the spec, Task 2's
interface block, and its tests.

**Review Focus:** all five lines above have a task and a test: ping-vs-keepalive (Task 1 Step 1),
nil-provider safety (Task 1 Step 1 + Task 2 Step 1), tunnel skip (Task 2 Step 1), StatusCode==0
not dropped (Task 2 Step 1), and agreement with the existing savings sink (Task 1 Step 5 running
the full suite including `TestSink_SavedMatchesTheLedgerToTheMicro`).

**Proportion:** this plan is longer than the spec because it pins exact test code (the spec
explicitly left test strategy to the builder) — each test block is the concrete form of one
Review Focus line or one Goals-section metric, not a transcription of the implementation, which
the plan still leaves to the implementer in every Step 3.
