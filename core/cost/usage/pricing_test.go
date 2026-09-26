package usage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/pipeline"
)

// resolverFor builds a table pricing one model on any endpoint.
func resolverFor(t *testing.T, model string, input, output float64) *pricing.Registry {
	t.Helper()
	var r pricing.Rates
	r.Base[pricing.TierInput], r.Set[pricing.TierInput] = input, true
	r.Base[pricing.TierOutput], r.Set[pricing.TierOutput] = output, true
	tab, err := pricing.NewTable([]pricing.Entry{
		{Host: "*", Model: model, Rates: r, Prov: pricing.ProvConfigured},
	})
	if err != nil {
		t.Fatal(err)
	}
	return pricing.NewRegistry(tab)
}

// pricedRespEvent is an inference response event with the given host, model and tokens.
func pricedRespEvent(host, model string, input, output int) *pipeline.SessionEvent {
	return &pipeline.SessionEvent{
		Phase:      pipeline.SessionResponse,
		Host:       host,
		StatusCode: 200,
		Inference: &pipeline.InferenceExtension{
			Model:        model,
			InputTokens:  input,
			OutputTokens: output,
			TotalTokens:  input + output,
		},
	}
}

func snapshotOf(a *Aggregator, now time.Time) Snapshot {
	return a.Snapshot(10*BucketWidth, BucketWidth, "", GroupNone)
}

// TestPricing_AggregatorPricesWithoutACostEvent is the gap this closes.
//
// Cost used to require litellm-budget-track to be in the pipeline, because it was
// the only thing that published a figure. A deployment without it reported every
// request unpriced however many tokens it burned — which is what an abctl capture
// with only tool-prune and inference-parser rows shows.
func TestPricing_AggregatorPricesWithoutACostEvent(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }),
		WithPricing(resolverFor(t, "claude-opus-5", 5.0/1e6, 25.0/1e6)))

	a.Record("s1", pricedRespEvent("gw.internal:4000", "claude-opus-5", 1000, 500))

	snap := snapshotOf(a, now)
	if !snap.Priced {
		t.Fatal("Priced = false; the resolver should have priced this request")
	}
	// 1000*5 + 500*25 = 5000 + 12500 micros
	if want := int64(17_500); snap.Totals.CostMicros != want {
		t.Errorf("CostMicros = %d, want %d", snap.Totals.CostMicros, want)
	}
	if snap.Totals.PricedRequests != 1 {
		t.Errorf("PricedRequests = %d, want 1", snap.Totals.PricedRequests)
	}
	if len(snap.UnpricedBy) != 0 {
		t.Errorf("UnpricedBy = %v, want empty", snap.UnpricedBy)
	}
}

// TestPricing_CostEventStillWins: a settled figure from the gateway outranks
// anything modelled, so the plugin's number is never second-guessed.
func TestPricing_CostEventStillWins(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }),
		WithPricing(resolverFor(t, "claude-opus-5", 5.0/1e6, 25.0/1e6)))

	ev := pricedRespEvent("gw.internal", "claude-opus-5", 1000, 500)
	raw, err := json.Marshal(event.Event{CostUSD: 1.0, Source: event.SourceGatewayHeader})
	if err != nil {
		t.Fatal(err)
	}
	ev.Plugins = map[string]json.RawMessage{event.Key: raw}
	a.Record("s1", ev)

	snap := snapshotOf(a, now)
	if want := int64(1_000_000); snap.Totals.CostMicros != want {
		t.Errorf("CostMicros = %d, want %d (the plugin's figure, not the model's)", snap.Totals.CostMicros, want)
	}
}

// TestPricing_UnpricedPairsAreNamed: a gap has to be nameable, not just counted.
// "Cost is incomplete" is not actionable; "api.anthropic.com / gpt-5 is unpriced"
// tells an operator exactly which pricing entry to add.
func TestPricing_UnpricedPairsAreNamed(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }),
		WithPricing(resolverFor(t, "claude-opus-5", 5.0/1e6, 25.0/1e6)))

	a.Record("s1", pricedRespEvent("gw.internal", "claude-opus-5", 1000, 500)) // priced
	a.Record("s1", pricedRespEvent("api.openai.com", "gpt-5", 100, 50))        // no rate
	a.Record("s1", pricedRespEvent("api.openai.com", "gpt-5", 100, 50))        // again

	snap := snapshotOf(a, now)
	if snap.Totals.Requests != 3 {
		t.Fatalf("Requests = %d, want 3", snap.Totals.Requests)
	}
	if snap.Totals.PricedRequests != 1 {
		t.Errorf("PricedRequests = %d, want 1 — the total covers only the priced subset", snap.Totals.PricedRequests)
	}
	if got := snap.UnpricedBy["api.openai.com gpt-5"]; got != 2 {
		t.Errorf("UnpricedBy[%q] = %d, want 2; map = %v", "api.openai.com gpt-5", got, snap.UnpricedBy)
	}
	if _, ok := snap.UnpricedBy["gw.internal claude-opus-5"]; ok {
		t.Error("a priced pair appeared in UnpricedBy")
	}
}

// TestPricing_NonInferenceTrafficIsNotCountedUnpriced: a plain proxied request has
// no model and no tokens. Naming it as an unpriced pair would bury the real gaps
// under every non-LLM call the proxy handled.
func TestPricing_NonInferenceTrafficIsNotCountedUnpriced(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }),
		WithPricing(resolverFor(t, "claude-opus-5", 5.0/1e6, 25.0/1e6)))

	a.Record("s1", &pipeline.SessionEvent{
		Phase: pipeline.SessionResponse, Host: "example.com", StatusCode: 200,
	})

	snap := snapshotOf(a, now)
	if snap.Totals.Requests != 1 {
		t.Fatalf("Requests = %d, want 1", snap.Totals.Requests)
	}
	if len(snap.UnpricedBy) != 0 {
		t.Errorf("UnpricedBy = %v, want empty for non-inference traffic", snap.UnpricedBy)
	}
}

// TestPricing_NoResolverIsStillUnpricedNotAPanic keeps the aggregator usable with
// no pricing wired at all, which is every deployment that has not configured it.
func TestPricing_NoResolverIsStillUnpricedNotAPanic(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }))

	a.Record("s1", pricedRespEvent("gw.internal", "claude-opus-5", 1000, 500))

	snap := snapshotOf(a, now)
	if snap.Priced {
		t.Error("Priced = true with no resolver and no cost event")
	}
	if got := snap.UnpricedBy["gw.internal claude-opus-5"]; got != 1 {
		t.Errorf("UnpricedBy = %v, want the pair named even with no resolver", snap.UnpricedBy)
	}
}

// TestPricing_UnpricedByFoldsAcrossBuckets: the map is summed over the window, so
// it must survive a coarser resolution the same way the counters do.
func TestPricing_UnpricedByFoldsAcrossBuckets(t *testing.T) {
	base := time.Now().Truncate(BucketWidth)
	clock := base
	a := New(WithClock(func() time.Time { return clock }))

	a.Record("s1", pricedRespEvent("h", "m", 10, 5))
	clock = base.Add(BucketWidth)
	a.Record("s1", pricedRespEvent("h", "m", 10, 5))

	snap := a.Snapshot(10*BucketWidth, 5*BucketWidth, "", GroupNone)
	if got := snap.UnpricedBy["h m"]; got != 2 {
		t.Errorf("UnpricedBy across two buckets = %d, want 2", got)
	}
}

// TestPricing_ErroredResponsesAreNotNamedAsGaps: snapshot.go promises UnpricedBy
// "names the pricing entry to add". A 5xx, or a denial after the parser ran, carries
// a model with zero tokens — so naming it would advertise a missing rate for a model
// that already has one, and the entry an operator added would never clear the row.
func TestPricing_ErroredResponsesAreNotNamedAsGaps(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }),
		WithPricing(resolverFor(t, "claude-opus-5", 5.0/1e6, 25.0/1e6)))

	// Upstream 500: the parser recorded the model, the provider reported no usage.
	a.Record("s1", &pipeline.SessionEvent{
		Phase:      pipeline.SessionResponse,
		Host:       "gw.internal",
		StatusCode: 500,
		Inference:  &pipeline.InferenceExtension{Model: "claude-opus-5"},
	})
	// A denial, same shape.
	a.Record("s1", &pipeline.SessionEvent{
		Phase:     pipeline.SessionDenied,
		Host:      "gw.internal",
		Inference: &pipeline.InferenceExtension{Model: "claude-opus-5"},
	})

	snap := snapshotOf(a, now)
	if len(snap.UnpricedBy) != 0 {
		t.Errorf("UnpricedBy = %v, want empty — these models have a rate; the requests had no tokens", snap.UnpricedBy)
	}
}

// TestPricing_TruncatedStreamWithNoUsageIsANamedGap is the other side of the test
// above, and the boundary between them is the STATUS, not the empty token split.
//
// A 200 whose usage could not be extracted was invisible: it counted in Requests and
// in nothing else — not PricedRequests, not PriceableRequests, not UnpricedBy — so
// nine priced requests plus one of these reported "9 of 9 priced", full parity, while
// real spend was missing from the total. Counts.PriceableRequests promises that ratio
// "reaches parity when it should"; this is the case that made it lie.
//
// The shape is a TRUNCATED OPENAI-DIALECT STREAM: OpenAI reports every counter on the
// final chunk, so a stream that dies mid-body has a model, a 200, and nothing else.
// It cannot reach the incomplete-reason floor either, because a floor needs a
// prompt-side count to be a lower bound of — which is exactly why Anthropic, whose
// message_start carries the prompt, is caught there and never arrives here.
func TestPricing_TruncatedStreamWithNoUsageIsANamedGap(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }),
		WithPricing(resolverFor(t, "gpt-5", 5.0/1e6, 25.0/1e6)))

	// Nine ordinary priced turns.
	for i := 0; i < 9; i++ {
		a.Record("s1", pricedRespEvent("api.openai.com", "gpt-5", 1000, 500))
	}
	// One truncated stream: 200, a model, no counters and no finish reason. A rate
	// for this pair EXISTS — the gap is the usage, not the table, which is why the
	// figure can never be recovered by pricing.
	a.Record("s1", &pipeline.SessionEvent{
		Phase:      pipeline.SessionResponse,
		Host:       "api.openai.com",
		StatusCode: 200,
		Inference:  &pipeline.InferenceExtension{Model: "gpt-5"},
	})

	snap := snapshotOf(a, now)
	if snap.Totals.Requests != 10 {
		t.Fatalf("Requests = %d, want 10", snap.Totals.Requests)
	}
	if snap.Totals.PricedRequests != 9 {
		t.Errorf("PricedRequests = %d, want 9 — the truncated stream produced no figure", snap.Totals.PricedRequests)
	}
	if snap.Totals.PriceableRequests != 10 {
		t.Errorf("PriceableRequests = %d, want 10 — a 200 that named a model belongs in the coverage denominator", snap.Totals.PriceableRequests)
	}
	// The invariant this finding falsified: coverage must NOT read as complete.
	if snap.Totals.PricedRequests == snap.Totals.PriceableRequests {
		t.Error("priced == priceable with one request's cost missing: the coverage ratio reports parity it has not earned")
	}
	if got := snap.UnpricedBy["api.openai.com gpt-5"]; got != 1 {
		t.Errorf("UnpricedBy[%q] = %d, want 1; map = %v — the parser reported a model, so the gap is nameable", "api.openai.com gpt-5", got, snap.UnpricedBy)
	}
}

// TestPricing_SettledZeroIsNotRePriced: the plugin charges nothing for a gateway
// that reported a present cost of 0 — a genuine free call. The aggregator used to
// see no event, fall through to its rate table, and invent a cost for it.
func TestPricing_SettledZeroIsNotRePriced(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }),
		WithPricing(resolverFor(t, "claude-opus-5", 5.0/1e6, 25.0/1e6)))

	ev := pricedRespEvent("gw.internal", "claude-opus-5", 1000, 500)
	raw, err := json.Marshal(event.Event{
		CostUSD: 0, Source: event.SourceGatewayHeader, Settled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ev.Plugins = map[string]json.RawMessage{event.Key: raw}
	a.Record("s1", ev)

	snap := snapshotOf(a, now)
	if snap.Totals.CostMicros != 0 {
		t.Errorf("CostMicros = %d, want 0 — the gateway declared this call free", snap.Totals.CostMicros)
	}
	if snap.Totals.PricedRequests != 1 {
		t.Errorf("PricedRequests = %d, want 1 — a settled zero IS priced", snap.Totals.PricedRequests)
	}
	// The DECLARED-FREE case for the Priced flag, and the only place it is pinned.
	//
	// Snapshot derives Priced from PricedRequests, never from CostMicros: this window
	// has a settled figure and no dollars, so reading the dollar total would report
	// "cost unavailable" for traffic the gateway explicitly priced at zero. A client
	// must render $0.0000 here and "cost unavailable" only when Priced is false — two
	// different truths that a CostMicros test collapses into one.
	if !snap.Priced {
		t.Error("Priced = false for a settled-zero window: a declared-free call is priced, and a client would report cost unavailable for a total the gateway actually stated")
	}
	if len(snap.UnpricedBy) != 0 {
		t.Errorf("UnpricedBy = %v, want empty", snap.UnpricedBy)
	}
}

// TestPricing_AbsurdHeaderFigureCannotWrapTheAggregate is the aggregate side of
// cost/event's unbounded conversion.
//
// A gateway header of 1e13 became MaxInt64 micros, and Counts.Add is plain int64
// addition, so TWO such requests wrapped the window total to −2 micros — verified on
// arm64. Nothing re-derives that total: the ring forgets it in six hours, the durable
// ledger keeps it thirty days.
//
// The figure is refused as a PRICE (event.Event.Priced is false for it), so the
// aggregator falls through to its rate table and reports what the tokens actually cost.
// That is the point of refusing rather than clamping: the request stays measured, and
// the number reported is one this code can defend.
func TestPricing_AbsurdHeaderFigureCannotWrapTheAggregate(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }),
		WithPricing(resolverFor(t, "claude-opus-5", 5.0/1e6, 25.0/1e6)))

	raw, err := json.Marshal(event.Event{
		CostUSD: 1e13, Source: event.SourceGatewayHeader,
		Provenance: "authoritative", Settled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		ev := pricedRespEvent("gw.internal", "claude-opus-5", 1000, 500)
		ev.Plugins = map[string]json.RawMessage{event.Key: raw}
		a.Record("s1", ev)
	}

	snap := snapshotOf(a, now)
	if snap.Totals.CostMicros < 0 {
		t.Fatalf("CostMicros = %d: two saturated figures wrapped the aggregate negative", snap.Totals.CostMicros)
	}
	// The modelled fallback, twice: 1000*5 + 500*25 = 17500 micros per request.
	if want := int64(35_000); snap.Totals.CostMicros != want {
		t.Errorf("CostMicros = %d, want %d — the table's figure, not the header's garbage", snap.Totals.CostMicros, want)
	}
	if got := snap.PricedBy["authoritative"]; got != 0 {
		t.Errorf("PricedBy[authoritative] = %d, want 0 — an out-of-range figure carries no authority", got)
	}
}

// TestPricing_PriceableExcludesNonInferenceTraffic is the coverage-denominator fix.
//
// Requests counts every proxied response, while only inference can ever be priced, so
// comparing the two made a correctly configured sidecar report permanent partial
// coverage with an empty gap list.
func TestPricing_PriceableExcludesNonInferenceTraffic(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }),
		WithPricing(resolverFor(t, "claude-opus-5", 5.0/1e6, 25.0/1e6)))

	// One priced inference call plus nine MCP/health responses carrying no model.
	a.Record("s1", pricedRespEvent("gw.internal", "claude-opus-5", 1000, 500))
	for i := 0; i < 9; i++ {
		a.Record("s1", &pipeline.SessionEvent{
			Phase: pipeline.SessionResponse, Host: "tool.internal", StatusCode: 200,
		})
	}

	snap := snapshotOf(a, now)
	if snap.Totals.Requests != 10 {
		t.Fatalf("Requests = %d, want 10", snap.Totals.Requests)
	}
	if snap.Totals.PriceableRequests != 1 {
		t.Errorf("PriceableRequests = %d, want 1 — only the inference call could be priced", snap.Totals.PriceableRequests)
	}
	if snap.Totals.PricedRequests != 1 {
		t.Errorf("PricedRequests = %d, want 1", snap.Totals.PricedRequests)
	}
	// Full coverage: priced == priceable, so a client must not disclose a gap.
	if snap.Totals.PricedRequests != snap.Totals.PriceableRequests {
		t.Error("coverage is complete but priced != priceable")
	}
}

// TestPricing_PriceableCountsUnpricedInference: an unpriceable-for-lack-of-rate
// request belongs in the denominator, or the gap it represents would vanish.
func TestPricing_PriceableCountsUnpricedInference(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }),
		WithPricing(resolverFor(t, "claude-opus-5", 5.0/1e6, 25.0/1e6)))

	a.Record("s1", pricedRespEvent("gw.internal", "claude-opus-5", 1000, 500)) // priced
	a.Record("s1", pricedRespEvent("api.openai.com", "gpt-5", 100, 50))        // no rate

	snap := snapshotOf(a, now)
	if snap.Totals.PriceableRequests != 2 {
		t.Errorf("PriceableRequests = %d, want 2 — both were inference", snap.Totals.PriceableRequests)
	}
	if snap.Totals.PricedRequests != 1 {
		t.Errorf("PricedRequests = %d, want 1", snap.Totals.PricedRequests)
	}
	if got := snap.UnpricedBy["api.openai.com gpt-5"]; got != 1 {
		t.Errorf("UnpricedBy = %v, want the gap named", snap.UnpricedBy)
	}
}

// TestPricing_ProvenanceReachesTheSnapshot closes the spec's success criterion that
// cost be "labelled with provenance". Before this the field was written by the plugin
// and read by nothing, so /v1/usage reported a total with no way to tell a gateway's
// own figure from one modelled off a shipped table.
func TestPricing_ProvenanceReachesTheSnapshot(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }),
		WithPricing(resolverFor(t, "claude-opus-5", 5.0/1e6, 25.0/1e6)))

	// One authoritative figure from the plugin.
	ev := pricedRespEvent("gw.internal", "claude-opus-5", 10, 5)
	raw, err := json.Marshal(event.Event{
		CostUSD: 1.0, Source: event.SourceGatewayHeader,
		Provenance: "authoritative", Settled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ev.Plugins = map[string]json.RawMessage{event.Key: raw}
	a.Record("s1", ev)

	// Two the aggregator modelled from the configured table.
	a.Record("s1", pricedRespEvent("gw.internal", "claude-opus-5", 1000, 500))
	a.Record("s1", pricedRespEvent("gw.internal", "claude-opus-5", 1000, 500))

	snap := a.Snapshot(10*BucketWidth, BucketWidth, "", GroupNone)
	if got := snap.PricedBy["authoritative"]; got != 1 {
		t.Errorf("PricedBy[authoritative] = %d, want 1; map = %v", got, snap.PricedBy)
	}
	if got := snap.PricedBy["configured"]; got != 2 {
		t.Errorf("PricedBy[configured] = %d, want 2; map = %v", got, snap.PricedBy)
	}
	// It must survive JSON, since that is how /v1/usage delivers it.
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"pricedBy"`) {
		t.Errorf("snapshot JSON carries no pricedBy: %s", b)
	}
}

// Avoided cost must never reach spend. This is the invariant that keeps a counterfactual
// out of a real total, and it is asserted here because usage.go:212 is the only place in the
// codebase that sums money — one line, guarded once.
//
// Two records, identical except that one carries a large avoided figure. Every money field
// the aggregator reports must be identical across the two.
func TestAggregator_TotalsAreInvariantToAvoidedCost(t *testing.T) {
	snapshotWith := func(t *testing.T, avoided []event.Saving) Snapshot {
		t.Helper()
		a := New(WithPricing(nil))
		raw, err := json.Marshal(event.Event{
			CostUSD:    0.25,
			Source:     event.SourceGatewayHeader,
			Provenance: "authoritative",
			Settled:    true,
			// A deliberately absurd figure: if it leaks into a total, no rounding
			// tolerance could hide it.
			Avoided: avoided,
		})
		if err != nil {
			t.Fatal(err)
		}
		ev := &pipeline.SessionEvent{
			Phase:     pipeline.SessionResponse,
			Host:      "gw.internal",
			Inference: &pipeline.InferenceExtension{Model: "claude-opus-5", InputTokens: 100, OutputTokens: 10},
			Plugins:   map[string]json.RawMessage{event.Key: raw},
		}
		a.Record("session-1", ev)
		return a.Snapshot(10*BucketWidth, BucketWidth, "", GroupNone)
	}

	without := snapshotWith(t, nil)
	with := snapshotWith(t, []event.Saving{
		{Component: "tool-prune", TokensAvoided: 500_000, USD: 999.99, Tier: "cache_write", Estimated: true},
		{Component: "some-future-plugin", TokensAvoided: 1_000, USD: 42, Tier: "input", Projected: true},
	})

	if with.Totals.CostMicros != without.Totals.CostMicros {
		t.Errorf("CostMicros = %d with avoided cost, %d without — a counterfactual entered spend",
			with.Totals.CostMicros, without.Totals.CostMicros)
	}
	if with.Totals.PricedRequests != without.Totals.PricedRequests {
		t.Errorf("PricedRequests = %d vs %d", with.Totals.PricedRequests, without.Totals.PricedRequests)
	}
	if with.Totals.Requests != without.Totals.Requests {
		t.Errorf("Requests = %d vs %d", with.Totals.Requests, without.Totals.Requests)
	}
	// And the figure that IS real still lands: an invariant that holds because nothing
	// was recorded at all would prove nothing.
	if with.Totals.CostMicros != 250_000 {
		t.Errorf("CostMicros = %d, want the record's own 250000", with.Totals.CostMicros)
	}

	// THE OTHER HALF OF THE SAME PROOF. Everything above shows the savings did not reach
	// spend, which a fold that dropped them entirely would also show. These two assertions
	// are what distinguish "kept out of the total" from "never counted".
	if without.Totals.AvoidedMicros != 0 {
		t.Errorf("AvoidedMicros = %d with no savings on the record, want 0",
			without.Totals.AvoidedMicros)
	}
	// 999.99 alone, in micros. The $42 entry is PROJECTED — observe mode left every byte on
	// the wire — so it is money that WAS spent and must not appear here. 1041990000 is the
	// figure a fold that summed both would report, and it is the whole reason this asserts an
	// exact number rather than `> 0`.
	if got, want := with.Totals.AvoidedMicros, int64(999_990_000); got != want {
		t.Errorf("AvoidedMicros = %d, want %d — the applied saving and only it (1041990000 "+
			"would mean the projected $42 was counted as money not spent)", got, want)
	}
}

// A saving on a request NOTHING COULD PRICE must still be counted, which is the case
// event.Record was split out from Decode to serve — its own doc calls it the interesting
// one, and it is the case a fold reading the priced record would silently lose.
//
// The record here is unsettled with no cost at all: event.Event.Priced is false, so the
// aggregator's cost arm does not take it and the request lands in the unpriced gap. The prompt
// tokens were still removed, and the estimate of what they would have cost is still the only
// evidence tool-prune did anything on this request.
func TestAggregator_CountsASavingOnAnUnpricedRequest(t *testing.T) {
	a := New(WithPricing(nil))
	raw, err := json.Marshal(event.Event{
		// No CostUSD, not settled: unpriced, and deliberately so.
		Source: event.SourceUsageFallback,
		Avoided: []event.Saving{
			{Component: "tool-prune", TokensAvoided: 2_000, USD: 0.5, Tier: "input", Estimated: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	a.Record("session-1", &pipeline.SessionEvent{
		Phase:     pipeline.SessionResponse,
		Host:      "gw.internal",
		Inference: &pipeline.InferenceExtension{Model: "claude-opus-5", InputTokens: 100, OutputTokens: 10},
		Plugins:   map[string]json.RawMessage{event.Key: raw},
	})

	totals := a.Snapshot(10*BucketWidth, BucketWidth, "", GroupNone).Totals
	if got, want := totals.AvoidedMicros, int64(500_000); got != want {
		t.Errorf("AvoidedMicros = %d, want %d — a saving on an unpriced request is lost, which "+
			"is what reading the PRICED record instead of the record does", got, want)
	}
	// The rest of the record's shape is unchanged by the saving: still unpriced, still counted
	// as a coverage gap. Asserted so this test cannot pass by the record having been priced
	// after all, which would make the case above untested.
	if totals.CostMicros != 0 || totals.PricedRequests != 0 {
		t.Errorf("CostMicros = %d, PricedRequests = %d; want both zero — the record carries no "+
			"price, so this test would otherwise be exercising the priced path",
			totals.CostMicros, totals.PricedRequests)
	}
}
