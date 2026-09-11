package usage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
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
	raw, err := json.Marshal(costevent.Event{CostUSD: 1.0, Source: costevent.SourceGatewayHeader})
	if err != nil {
		t.Fatal(err)
	}
	ev.Plugins = map[string]json.RawMessage{costevent.PluginName: raw}
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

// TestPricing_SettledZeroIsNotRePriced: the plugin charges nothing for a gateway
// that reported a present cost of 0 — a genuine free call. The aggregator used to
// see no event, fall through to its rate table, and invent a cost for it.
func TestPricing_SettledZeroIsNotRePriced(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }),
		WithPricing(resolverFor(t, "claude-opus-5", 5.0/1e6, 25.0/1e6)))

	ev := pricedRespEvent("gw.internal", "claude-opus-5", 1000, 500)
	raw, err := json.Marshal(costevent.Event{
		CostUSD: 0, Source: costevent.SourceGatewayHeader, Settled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ev.Plugins = map[string]json.RawMessage{costevent.PluginName: raw}
	a.Record("s1", ev)

	snap := snapshotOf(a, now)
	if snap.Totals.CostMicros != 0 {
		t.Errorf("CostMicros = %d, want 0 — the gateway declared this call free", snap.Totals.CostMicros)
	}
	if snap.Totals.PricedRequests != 1 {
		t.Errorf("PricedRequests = %d, want 1 — a settled zero IS priced", snap.Totals.PricedRequests)
	}
	if len(snap.UnpricedBy) != 0 {
		t.Errorf("UnpricedBy = %v, want empty", snap.UnpricedBy)
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
	raw, err := json.Marshal(costevent.Event{
		CostUSD: 1.0, Source: costevent.SourceGatewayHeader,
		Provenance: "authoritative", Settled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ev.Plugins = map[string]json.RawMessage{costevent.PluginName: raw}
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
	snapshotWith := func(t *testing.T, avoided []costevent.Saving) Snapshot {
		t.Helper()
		a := New(WithPricing(nil))
		raw, err := json.Marshal(costevent.Event{
			CostUSD:    0.25,
			Source:     costevent.SourceGatewayHeader,
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
			Plugins:   map[string]json.RawMessage{costevent.Key: raw},
		}
		a.Record("session-1", ev)
		return a.Snapshot(10*BucketWidth, BucketWidth, "", GroupNone)
	}

	without := snapshotWith(t, nil)
	with := snapshotWith(t, []costevent.Saving{
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
}
