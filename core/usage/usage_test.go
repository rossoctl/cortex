package usage

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rossoctl/cortex/core/costevent"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/pricing"
)

// fixedClock returns a controllable now, so bucket boundaries are exact rather
// than dependent on when the test happens to run.
func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func respEvent(at time.Time, status int, dur time.Duration, model string, tokens int) *pipeline.SessionEvent {
	e := &pipeline.SessionEvent{
		At:         at,
		Direction:  pipeline.Outbound,
		Phase:      pipeline.SessionResponse,
		StatusCode: status,
		Duration:   dur,
	}
	if model != "" || tokens > 0 {
		e.Inference = &pipeline.InferenceExtension{Model: model, TotalTokens: tokens}
	}
	return e
}

// withCost attaches a settled cost event under costevent.Key — the key production
// publishes — exactly as a listener would after SnapshotPlugins. Returns e so it
// composes with respEvent.
//
// inference-parser is what settles and publishes the figure; litellm-budget-track
// consumes it to enforce a budget.
//
// Both keys appear in this package deliberately. Every fixture here used to publish
// costevent.PluginName, the frozen LEGACY key, which meant the aggregator's whole
// cost coverage exercised only costevent.Find's FALLBACK branch: deleting its
// primary Plugins[Key] lookup left this package green while /v1/usage would have
// reported every request unpriced. The default is now the production key, and
// withLegacyCost keeps the compatibility path held down rather than untested.
func withCost(t *testing.T, e *pipeline.SessionEvent, costUSD float64) *pipeline.SessionEvent {
	t.Helper()
	return withCostUnderKey(t, e, costUSD, costevent.Key)
}

// withLegacyCost publishes under costevent.PluginName, the frozen legacy key. Only
// TestCostOf_DecodesTheLegacyProducerKey should use it; everything else wants the
// production key.
func withLegacyCost(t *testing.T, e *pipeline.SessionEvent, costUSD float64) *pipeline.SessionEvent {
	t.Helper()
	// The deprecation warning is the POINT here, not an oversight to be silenced
	// generally: this helper exists to hold the compatibility path down, and reading the
	// legacy key is exactly what it must keep doing. Switching to costevent.Key would
	// leave the frozen spelling untested and re-open the coverage hole this helper was
	// added to close — every cost fixture once exercised only the legacy key, which meant
	// nothing proved the production key was decoded at all.
	//nolint:staticcheck // SA1019: deliberately exercising the deprecated key.
	return withCostUnderKey(t, e, costUSD, costevent.PluginName)
}

// withCostUnderKey is the shared body, parameterised by key so the two spellings
// cannot drift in anything but the key itself.
func withCostUnderKey(t *testing.T, e *pipeline.SessionEvent, costUSD float64, key string) *pipeline.SessionEvent {
	t.Helper()
	raw, err := json.Marshal(costevent.Event{
		CostUSD: costUSD,
		Source:  costevent.SourceGatewayHeader,
	})
	if err != nil {
		t.Fatalf("marshal cost event: %v", err)
	}
	if e.Plugins == nil {
		e.Plugins = map[string]json.RawMessage{}
	}
	e.Plugins[key] = raw
	return e
}

// The legacy key must keep decoding: session stores hold events recorded under it,
// and a sidecar in a mixed-version deployment still publishes it — which is why
// costevent.PluginName's literal value is frozen.
//
// This is the one fixture in the package deliberately left on that key. It exists
// because the swap to costevent.Key would otherwise have moved the fallback from
// over-tested to untested in a single commit, and the fallback is a real
// compatibility guarantee for events already sitting in a store.
func TestCostOf_DecodesTheLegacyProducerKey(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 30, 0, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	a.Record("s1", withLegacyCost(t, respEvent(now, 200, time.Second, "claude-opus-5", 1000), 0.0421))

	snap := a.Snapshot(time.Minute, BucketWidth, "", GroupNone)
	if snap.Totals.CostMicros != 42_100 {
		t.Errorf("CostMicros = %d, want 42100 — a legacy-key record must still decode", snap.Totals.CostMicros)
	}
	if snap.Totals.PricedRequests != 1 {
		t.Errorf("PricedRequests = %d, want 1", snap.Totals.PricedRequests)
	}
}

// TestCountsAddFoldsPricedRequests is why coverage is a counter and not a
// boolean: buckets are summed when a client asks for a coarser resolution, and
// a bool cannot express "12 of 40 PRICEABLE requests in this window were priced".
//
// Priceable, not Requests. The gap a client renders is
// PriceableRequests-minus-PricedRequests; taking it against Requests counts every
// health check and tool call as unpriced traffic and never reaches zero. See both
// fields' godoc.
func TestCountsAddFoldsPricedRequests(t *testing.T) {
	a := Counts{Requests: 10, CostMicros: 500, PricedRequests: 4, PriceableRequests: 9}
	a.Add(Counts{Requests: 5, CostMicros: 250, PricedRequests: 3, PriceableRequests: 6})

	if a.Requests != 15 {
		t.Errorf("Requests = %d, want 15", a.Requests)
	}
	if a.CostMicros != 750 {
		t.Errorf("CostMicros = %d, want 750", a.CostMicros)
	}
	if a.PricedRequests != 7 {
		t.Errorf("PricedRequests = %d, want 7", a.PricedRequests)
	}
	if a.PriceableRequests != 15 {
		t.Errorf("PriceableRequests = %d, want 15", a.PriceableRequests)
	}
	if unpriced := a.PriceableRequests - a.PricedRequests; unpriced != 8 {
		t.Errorf("unpriced = %d, want 8", unpriced)
	}
}

// TestCountsAdd_EveryInt64FieldSaturatesAndSaysSo walks Counts BY REFLECTION, which is the
// only way this test cannot rot.
//
// A hand-written table of the fields would have to be extended by whoever adds a field —
// the same person who would have to remember to use the checked accumulate in Add, and the
// evidence in Add's own doc is that this is exactly what gets forgotten (an out-of-module
// copy hand-summed these fields and silently missed PricedRequests). Reflection asks the
// struct instead of asking a list, so a new int64 field added with a bare `+=` fails here on
// the day it lands.
//
// Both halves are asserted, because either alone is a lie: the clamp (the number must not
// wrap into a negative) and the disclosure (a clamped total must say it is a floor).
func TestCountsAdd_EveryInt64FieldSaturatesAndSaysSo(t *testing.T) {
	rt := reflect.TypeOf(Counts{})
	checked := 0
	for i := range rt.NumField() {
		f := rt.Field(i)
		if f.Type.Kind() != reflect.Int64 {
			continue
		}
		checked++
		t.Run(f.Name, func(t *testing.T) {
			for _, dir := range []struct {
				name       string
				start, add int64
				want       int64
			}{
				{"upward", math.MaxInt64, 1, math.MaxInt64},
				{"downward", math.MinInt64, -1, math.MinInt64},
			} {
				t.Run(dir.name, func(t *testing.T) {
					var a, b Counts
					reflect.ValueOf(&a).Elem().Field(i).SetInt(dir.start)
					reflect.ValueOf(&b).Elem().Field(i).SetInt(dir.add)
					a.Add(b)
					if got := reflect.ValueOf(a).Field(i).Int(); got != dir.want {
						t.Errorf("%s = %d after overflowing %s, want %d — a bare += wraps here, and "+
							"the wrapped figure then sits in the durable ledger for its full retention",
							f.Name, got, dir.name, dir.want)
					}
					if !a.Saturated {
						t.Errorf("%s hit the int64 %s bound and Saturated is false; a silently clamped "+
							"total is the same class of lie as a wrapped one, and a client has no way "+
							"to know the figure is a floor", f.Name, dir.name)
					}
				})
			}
		})
	}
	// The loop itself must have found the fields, or a rename made this test a no-op that
	// reports success — which is the failure mode the reflection was chosen to avoid.
	if checked < 12 {
		t.Errorf("walked %d int64 fields of Counts, expected at least 12; the struct was "+
			"renamed or retyped and this test now asserts nothing", checked)
	}
}

// TestCountsAdd_TheWrapMeasuredInPricingIsClosed is the disclosure in
// pricing.MaxCostMicros, executed.
//
// That comment states the arithmetic — math.MaxInt64 / MaxCostMicros is 1023, so 1,024
// requests each priced at the bound wrap the aggregate to -9214364837600034816 — and names
// Counts.Add as the place that has to close it, because no per-request bound can. This runs
// exactly that scenario and asserts the total never goes negative.
//
// The count is deliberately past 1,024. A test that stopped at the wrap point would pass on
// an implementation that wraps once and then keeps accumulating from a negative base.
func TestCountsAdd_TheWrapMeasuredInPricingIsClosed(t *testing.T) {
	const atBound = int64(pricing.MaxCostMicros) - 1 // the largest figure MicrosFromUSD admits
	var total Counts
	for n := 1; n <= 2_000; n++ {
		total.Add(Counts{Requests: 1, CostMicros: atBound, PricedRequests: 1})
		if total.CostMicros < 0 {
			t.Fatalf("CostMicros = %d after %d requests at %d micros: the aggregate wrapped, which "+
				"is the failure pricing.MaxCostMicros documents and cannot fix",
				total.CostMicros, n, atBound)
		}
	}
	if total.CostMicros != math.MaxInt64 {
		t.Errorf("CostMicros = %d, want math.MaxInt64 (%d) — the sum is meant to CLAMP at the "+
			"ceiling, not to stop accumulating or to reset", total.CostMicros, int64(math.MaxInt64))
	}
	if !total.Saturated {
		t.Error("2,000 requests at the cost bound produced a clamped total that does not disclose " +
			"it; the number is then a ceiling presented as a sum")
	}
	// Requests is 2,000 and exact. The disclosure must not be read as "nothing here is
	// trustworthy": one field saturated, the others are still sums.
	if total.Requests != 2_000 {
		t.Errorf("Requests = %d, want 2000 — saturation on one field must not disturb another",
			total.Requests)
	}
}

// TestCountsAdd_SaturationIsInheritedByAnyTotalContainingIt is why the flag is OR-ed rather
// than recomputed.
//
// Buckets are folded into totals and into coarser buckets, and a fold does not repeat the
// addition that saturated. If the flag did not travel, the bucket would say its figure is a
// floor and the window total containing it would say its own figure is exact — the same
// number, two different claims, and the client reads the one that is wrong.
func TestCountsAdd_SaturationIsInheritedByAnyTotalContainingIt(t *testing.T) {
	saturated := Counts{Requests: 1, CostMicros: math.MaxInt64, Saturated: true}
	total := Counts{Requests: 1, CostMicros: 5}
	total.Add(saturated)
	if !total.Saturated {
		t.Error("a total that folded in a saturated bucket reports itself exact; the floor is " +
			"inherited by every sum the bucket is part of")
	}
	// And the flag is not sticky the other way: folding a clean bucket into a clean total
	// must not manufacture a caveat, or the disclosure means nothing.
	clean := Counts{Requests: 1, CostMicros: 5}
	clean.Add(Counts{Requests: 1, CostMicros: 5})
	if clean.Saturated {
		t.Error("an ordinary fold set Saturated; a caveat that appears on clean data trains an " +
			"operator to ignore it")
	}
}

// TestCountsSaturatedOmittedWhenFalse keeps the wire quiet for the overwhelming majority of
// responses, on the same rule as PricedRequests below: absence is the clean answer.
func TestCountsSaturatedOmittedWhenFalse(t *testing.T) {
	b, err := json.Marshal(Counts{Requests: 3, CostMicros: 5})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "saturated") {
		t.Errorf("a clean Counts serialised the saturation flag, got %s", b)
	}
	b, err = json.Marshal(Counts{Requests: 3, Saturated: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"saturated":true`) {
		t.Errorf("a saturated Counts did not serialise the flag, got %s — the disclosure only "+
			"works if it reaches the client", b)
	}
}

// TestCountsPricedRequestsOmittedWhenZero keeps the wire quiet for deployments
// that price nothing.
func TestCountsPricedRequestsOmittedWhenZero(t *testing.T) {
	b, err := json.Marshal(Counts{Requests: 3})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "pricedRequests") {
		t.Errorf("zero PricedRequests should be omitted, got %s", b)
	}
}

// The cost is taken from the figure inference-parser already settled and published.
// The aggregator does hold a rate table of its own (see WithPricing), but a
// published figure always wins over a modelled one; no resolver is configured here,
// so the published path is the only one that can answer.
func TestRecord_PricesFromCostEvent(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 30, 0, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	a.Record("s1", withCost(t, respEvent(now, 200, time.Second, "claude-opus-5", 1000), 0.0421))

	snap := a.Snapshot(time.Minute, BucketWidth, "", GroupNone)
	if snap.Totals.Requests != 1 {
		t.Fatalf("Requests = %d, want 1", snap.Totals.Requests)
	}
	if snap.Totals.CostMicros != 42_100 {
		t.Errorf("CostMicros = %d, want 42100", snap.Totals.CostMicros)
	}
	if snap.Totals.PricedRequests != 1 {
		t.Errorf("PricedRequests = %d, want 1", snap.Totals.PricedRequests)
	}
}

// Partial coverage: the dollar total covers only the priced subset, and the gap
// between PricedRequests and Requests is what tells a client to say so rather
// than present the figure as complete.
func TestRecord_MixedCoverage(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 30, 0, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	a.Record("s1", withCost(t, respEvent(now, 200, time.Second, "claude-opus-5", 1000), 0.02))
	a.Record("s1", respEvent(now, 200, time.Second, "unpriced-model", 500)) // no cost event

	snap := a.Snapshot(time.Minute, BucketWidth, "", GroupNone)
	if snap.Totals.Requests != 2 {
		t.Fatalf("Requests = %d, want 2", snap.Totals.Requests)
	}
	if snap.Totals.PricedRequests != 1 {
		t.Errorf("PricedRequests = %d, want 1", snap.Totals.PricedRequests)
	}
	if snap.Totals.CostMicros != 20_000 {
		t.Errorf("CostMicros = %d, want 20000 (only the priced request)", snap.Totals.CostMicros)
	}
	// Tokens are unaffected by pricing: a request nobody could price is still a
	// request that sent tokens.
	if snap.Totals.Tokens != 1500 {
		t.Errorf("Tokens = %d, want 1500 (both requests)", snap.Totals.Tokens)
	}
}

func TestRecord_UnpricedContributesNoCost(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 30, 0, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	a.Record("s1", respEvent(now, 200, time.Second, "claude-opus-5", 700))

	snap := a.Snapshot(time.Minute, BucketWidth, "", GroupNone)
	if snap.Totals.CostMicros != 0 {
		t.Errorf("CostMicros = %d, want 0", snap.Totals.CostMicros)
	}
	if snap.Totals.PricedRequests != 0 {
		t.Errorf("PricedRequests = %d, want 0", snap.Totals.PricedRequests)
	}
	if snap.Totals.Tokens != 700 {
		t.Errorf("Tokens = %d, want 700", snap.Totals.Tokens)
	}
}

// Coverage must survive folding to a coarser resolution, which is the whole
// reason it is a counter.
func TestRecord_CoverageFoldsAcrossBuckets(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 2, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	a.Record("s1", withCost(t, respEvent(now.Add(-time.Minute), 200, time.Second, "m", 100), 0.01))
	a.Record("s1", respEvent(now.Add(-time.Minute), 200, time.Second, "m", 100))
	a.Record("s1", withCost(t, respEvent(now, 200, time.Second, "m", 100), 0.02))

	folded := a.Snapshot(2*time.Minute, 2*time.Minute, "", GroupNone).Buckets[0]
	if folded.Requests != 3 {
		t.Fatalf("folded Requests = %d, want 3", folded.Requests)
	}
	if folded.PricedRequests != 2 {
		t.Errorf("folded PricedRequests = %d, want 2", folded.PricedRequests)
	}
	if folded.CostMicros != 30_000 {
		t.Errorf("folded CostMicros = %d, want 30000", folded.CostMicros)
	}
}

// An idle minute must come back as a present, zeroed bucket. A client cannot
// otherwise tell "no traffic" from "fell off the ring", and that distinction is
// what makes a gap visible in a bar chart.
func TestSnapshot_IdleBucketsArePresentAndZero(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 30, 0, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	a.Record("s1", respEvent(now.Add(-9*time.Minute), 200, time.Second, "claude-sonnet-5", 100))

	snap := a.Snapshot(10*time.Minute, BucketWidth, "", GroupNone)
	if len(snap.Buckets) != 10 {
		t.Fatalf("got %d buckets, want 10", len(snap.Buckets))
	}
	if snap.Buckets[0].Requests != 1 {
		t.Errorf("oldest bucket requests = %d, want 1", snap.Buckets[0].Requests)
	}
	for i, b := range snap.Buckets[1:] {
		if b.Requests != 0 || b.Tokens != 0 {
			t.Errorf("bucket %d should be idle, got %+v", i+1, b.Counts)
		}
		if b.At.IsZero() {
			t.Errorf("idle bucket %d has no timestamp — a client cannot place it on an axis", i+1)
		}
	}
	// Buckets must be chronological and exactly one width apart.
	for i := 1; i < len(snap.Buckets); i++ {
		if d := snap.Buckets[i].At.Sub(snap.Buckets[i-1].At); d != BucketWidth {
			t.Errorf("gap between bucket %d and %d = %s, want %s", i-1, i, d, BucketWidth)
		}
	}
}

// Mean and stddev must come out of the running sums correctly. Values chosen so
// the answer is exact in binary floating point.
func TestSnapshot_LatencyMeanAndStdDev(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 30, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	// 1000ms, 2000ms, 3000ms -> mean 2000, population stddev sqrt(2/3)*1000.
	for _, ms := range []int{1000, 2000, 3000} {
		a.Record("s1", respEvent(now, 200, time.Duration(ms)*time.Millisecond, "m", 0))
	}

	b := a.Snapshot(time.Minute, BucketWidth, "", GroupNone).Buckets[0]
	if b.LatMeanMs != 2000 {
		t.Errorf("mean = %v, want 2000", b.LatMeanMs)
	}
	want := math.Sqrt(2.0/3.0) * 1000
	if math.Abs(b.LatStdDevMs-want) > 1e-6 {
		t.Errorf("stddev = %v, want %v", b.LatStdDevMs, want)
	}
}

// Identical samples have zero variance; float cancellation can make the
// intermediate negative, which would NaN the sqrt.
func TestSnapshot_IdenticalLatenciesGiveZeroStdDev(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 30, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))
	for i := 0; i < 5; i++ {
		a.Record("s1", respEvent(now, 200, 1234*time.Millisecond, "m", 0))
	}
	b := a.Snapshot(time.Minute, BucketWidth, "", GroupNone).Buckets[0]
	if b.LatStdDevMs != 0 {
		t.Errorf("stddev = %v, want exactly 0", b.LatStdDevMs)
	}
	if math.IsNaN(b.LatStdDevMs) {
		t.Error("stddev is NaN — negative variance was not guarded")
	}
}

// A zero duration means "not measured", not "instant": folding it in would drag
// the mean toward zero and misreport latency.
func TestSnapshot_ZeroDurationExcludedFromMean(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 30, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))
	a.Record("s1", respEvent(now, 200, 2*time.Second, "m", 0))
	a.Record("s1", respEvent(now, 200, 0, "m", 0)) // unmeasured

	b := a.Snapshot(time.Minute, BucketWidth, "", GroupNone).Buckets[0]
	if b.Requests != 2 {
		t.Errorf("requests = %d, want 2 (both count as traffic)", b.Requests)
	}
	// The mean is over MEASURED requests only. Dividing by Requests would report
	// 1000ms for a single 2000ms response, understating latency in proportion to
	// how many responses arrived unmeasured.
	if b.LatMeanMs != 2000 {
		t.Errorf("mean = %v, want 2000 (one measured 2000ms response)", b.LatMeanMs)
	}
	if b.LatSamples != 1 {
		t.Errorf("latSamples = %d, want 1 — only one response carried a duration", b.LatSamples)
	}
}

// The same dilution must not reappear when buckets are folded: a wider bucket's
// mean has to weight each source bucket by its measured-sample count, not by its
// request count.
func TestFold_LatencyExcludesUnmeasuredRequests(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 2, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	// Minute A: one measured 2000ms response plus 9 unmeasured ones.
	minA := now.Add(-time.Minute)
	a.Record("s1", respEvent(minA, 200, 2000*time.Millisecond, "m", 0))
	for i := 0; i < 9; i++ {
		a.Record("s1", respEvent(minA, 200, 0, "m", 0))
	}
	// Minute B: one measured 4000ms response.
	a.Record("s1", respEvent(now, 200, 4000*time.Millisecond, "m", 0))

	folded := a.Snapshot(2*time.Minute, 2*time.Minute, "", GroupNone).Buckets[0]
	if folded.Requests != 11 {
		t.Fatalf("requests = %d, want 11", folded.Requests)
	}
	// Two measured samples: (2000 + 4000) / 2 = 3000.
	if folded.LatMeanMs != 3000 {
		t.Errorf("folded mean = %v, want 3000 (two measured samples)", folded.LatMeanMs)
	}
	if folded.LatSamples != 2 {
		t.Errorf("folded latSamples = %d, want 2", folded.LatSamples)
	}
}

// >=400 counts as an error, and a denial counts too: an auth outage must not
// look like a traffic drop.
func TestRecord_ErrorsAndDenials(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 30, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	a.Record("s1", respEvent(now, 200, time.Second, "m", 10))
	a.Record("s1", respEvent(now, 429, time.Second, "m", 0))
	a.Record("s1", respEvent(now, 500, time.Second, "m", 0))
	denied := respEvent(now, 0, time.Second, "", 0)
	denied.Phase = pipeline.SessionDenied
	a.Record("s1", denied)

	b := a.Snapshot(time.Minute, BucketWidth, "", GroupStatus).Buckets[0]
	if b.Requests != 4 {
		t.Errorf("requests = %d, want 4", b.Requests)
	}
	if b.Errors != 3 {
		t.Errorf("errors = %d, want 3 (429, 500, denied)", b.Errors)
	}
	if _, ok := b.Series["denied"]; !ok {
		t.Errorf("denied events need their own status key; got keys %v", keys(b.Series))
	}
}

// Request events carry no status, duration or usage. Counting them would double
// every request.
func TestRecord_IgnoresRequestPhase(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 30, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))
	req := respEvent(now, 0, 0, "m", 0)
	req.Phase = pipeline.SessionRequest
	a.Record("s1", req)

	if got := a.Snapshot(time.Minute, BucketWidth, "", GroupNone).Totals.Requests; got != 0 {
		t.Errorf("requests = %d, want 0 — request-phase events must not count", got)
	}
}

// All three groupings accumulate simultaneously, so an operator cycling the
// group parameter sees the same history from each angle rather than each
// grouping starting empty when first selected.
func TestSnapshot_AllGroupingsPopulatedFromOnePass(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 30, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	e := respEvent(now, 200, time.Second, "claude-sonnet-5", 500)
	e.Invocations = &pipeline.Invocations{Outbound: []pipeline.Invocation{{Plugin: "inference-parser"}}}
	a.Record("s1", e)

	for _, tc := range []struct {
		group Group
		key   string
	}{
		{GroupMethod, "claude-sonnet-5"},
		{GroupStatus, "200"},
		{GroupPlugin, "inference-parser"},
	} {
		b := a.Snapshot(time.Minute, BucketWidth, "", tc.group).Buckets[0]
		if _, ok := b.Series[tc.key]; !ok {
			t.Errorf("group=%s missing key %q; got %v", tc.group, tc.key, keys(b.Series))
		}
	}
	if b := a.Snapshot(time.Minute, BucketWidth, "", GroupNone).Buckets[0]; b.Series != nil {
		t.Error("group=none must omit series entirely")
	}
}

// Cost needs a source. Without a cost event on the request, CostMicros stays
// zero AND Priced is false, so a client can say "unavailable" instead of
// rendering $0.00 — which would read as "this traffic was free".
//
// Formerly TestSnapshot_CostRequiresPricer: the assertions are unchanged, but the
// source is now the figure inference-parser publishes rather than an injected
// Pricer that no production caller ever supplied.
func TestSnapshot_CostRequiresACostSource(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 30, 30, 0, time.UTC)

	unpriced := New(WithClock(fixedClock(now)))
	unpriced.Record("s1", respEvent(now, 200, time.Second, "claude-sonnet-5", 1000))
	snap := unpriced.Snapshot(time.Minute, BucketWidth, "", GroupNone)
	if snap.Priced {
		t.Error("Priced = true with no cost event on any request")
	}
	if snap.Totals.CostMicros != 0 {
		t.Errorf("costMicros = %d, want 0", snap.Totals.CostMicros)
	}

	// $0.00152 — roughly 1000 sonnet input tokens at $1.52/Mtok.
	priced := New(WithClock(fixedClock(now)))
	priced.Record("s1", withCost(t, respEvent(now, 200, time.Second, "claude-sonnet-5", 1000), 0.00152))
	snap = priced.Snapshot(time.Minute, BucketWidth, "", GroupNone)
	if !snap.Priced {
		t.Error("Priced = false after a request carried a cost event")
	}
	if snap.Totals.CostMicros != 1520 {
		t.Errorf("costMicros = %d, want 1520", snap.Totals.CostMicros)
	}
}

// Priced is derived from what actually happened, not from whether a hook was
// installed. Partial coverage still reports Priced — the caller distinguishes
// complete from partial by comparing PricedRequests against Requests.
func TestSnapshot_PricedDerivedFromCoverage(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 30, 0, 0, time.UTC)

	t.Run("nothing priced", func(t *testing.T) {
		a := New(WithClock(fixedClock(now)))
		a.Record("s", respEvent(now, 200, time.Second, "m", 100))
		if a.Snapshot(time.Minute, BucketWidth, "", GroupNone).Priced {
			t.Error("Priced = true, want false when no request was priced")
		}
	})

	t.Run("partially priced still reports priced", func(t *testing.T) {
		a := New(WithClock(fixedClock(now)))
		a.Record("s", withCost(t, respEvent(now, 200, time.Second, "m", 100), 0.01))
		a.Record("s", respEvent(now, 200, time.Second, "n", 100))

		snap := a.Snapshot(time.Minute, BucketWidth, "", GroupNone)
		if !snap.Priced {
			t.Error("Priced = false, want true")
		}
		if snap.Totals.PricedRequests != 1 || snap.Totals.Requests != 2 {
			t.Errorf("coverage = %d/%d, want 1/2",
				snap.Totals.PricedRequests, snap.Totals.Requests)
		}
	})
}

// Per-session rings must isolate: one session's traffic cannot appear in
// another's chart, while both land in the all-sessions total.
func TestSnapshot_SessionIsolation(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 30, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	a.Record("alice", respEvent(now, 200, time.Second, "m", 100))
	a.Record("bob", respEvent(now, 200, time.Second, "m", 700))

	if got := a.Snapshot(time.Minute, BucketWidth, "alice", GroupNone).Totals.Tokens; got != 100 {
		t.Errorf("alice tokens = %d, want 100", got)
	}
	if got := a.Snapshot(time.Minute, BucketWidth, "bob", GroupNone).Totals.Tokens; got != 700 {
		t.Errorf("bob tokens = %d, want 700", got)
	}
	if got := a.Snapshot(time.Minute, BucketWidth, "", GroupNone).Totals.Tokens; got != 800 {
		t.Errorf("all-sessions tokens = %d, want 800", got)
	}
}

// An unknown session yields zeroed buckets, not an error: a live session that
// has produced no response events yet is a normal state.
func TestSnapshot_UnknownSessionIsZeroedNotError(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 30, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))
	snap := a.Snapshot(10*time.Minute, BucketWidth, "nope", GroupNone)
	if len(snap.Buckets) != 10 {
		t.Fatalf("got %d buckets, want 10", len(snap.Buckets))
	}
	if snap.Totals.Requests != 0 {
		t.Errorf("totals = %+v, want zero", snap.Totals)
	}
}

// At the cap, the NEWEST session must be answerable and the coldest ring
// reclaimed. Refusing new sessions instead would leave /v1/sessions listing a
// live session while /v1/usage?session=<id> returned zeros — indistinguishable
// from "that session did nothing" — and would stay that way for the life of the
// process, since the store expires sessions without telling the aggregator.
func TestRecord_SessionCapReclaimsColdestRing(t *testing.T) {
	base := time.Date(2026, 9, 4, 23, 0, 0, 0, time.UTC)
	now := base
	a := New(WithClock(func() time.Time { return now }), WithMaxSessions(2))

	// s1 at 23:00, s2 at 23:01 — both tracked, s1 is now the coldest.
	a.Record("s1", respEvent(base, 200, time.Second, "m", 100))
	a.Record("s2", respEvent(base.Add(time.Minute), 200, time.Second, "m", 100))

	// s3 arrives: the cap is reached, so s1's ring is reclaimed.
	now = base.Add(2 * time.Minute)
	a.Record("s3", respEvent(now, 200, time.Second, "m", 100))

	if got := a.Snapshot(10*time.Minute, BucketWidth, "s3", GroupNone).Totals.Requests; got != 1 {
		t.Errorf("newest session s3 must be answerable, got %d requests", got)
	}
	if got := a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupNone).Totals.Requests; got != 0 {
		t.Errorf("coldest session s1 should have been reclaimed, got %d requests", got)
	}
	if got := a.Snapshot(10*time.Minute, BucketWidth, "s2", GroupNone).Totals.Requests; got != 1 {
		t.Errorf("s2 was warmer than s1 and should survive, got %d requests", got)
	}
	// Every event still counts in the all-sessions aggregate regardless.
	if got := a.Snapshot(10*time.Minute, BucketWidth, "", GroupNone).Totals.Requests; got != 3 {
		t.Errorf("all-sessions requests = %d, want 3", got)
	}
}

// Memory must stay bounded no matter how many distinct session ids arrive.
func TestRecord_SessionRingsStayBounded(t *testing.T) {
	base := time.Date(2026, 9, 4, 23, 0, 0, 0, time.UTC)
	now := base
	a := New(WithClock(func() time.Time { return now }), WithMaxSessions(4))

	for i := 0; i < 200; i++ {
		now = base.Add(time.Duration(i) * time.Second)
		a.Record(fmt.Sprintf("session-%d", i), respEvent(now, 200, time.Second, "m", 1))
	}
	a.mu.RLock()
	n := len(a.sessions)
	a.mu.RUnlock()
	if n > 4 {
		t.Errorf("retained %d rings, cap is 4", n)
	}
}

// A slot reused on a later lap must reset, not accumulate onto stale data —
// this is what makes the ring self-expiring with no sweeper.
func TestRecord_StaleSlotResetsOnReuse(t *testing.T) {
	base := time.Date(2026, 9, 4, 20, 0, 30, 0, time.UTC)
	now := base
	a := New(WithClock(func() time.Time { return now }))

	a.Record("s1", respEvent(base, 200, time.Second, "m", 999))

	// Exactly one full lap later: same slot, different minute.
	later := base.Add(NumBuckets * BucketWidth)
	now = later
	a.Record("s1", respEvent(later, 200, time.Second, "m", 5))

	b := a.Snapshot(time.Minute, BucketWidth, "", GroupNone).Buckets[0]
	if b.Tokens != 5 {
		t.Errorf("tokens = %d, want 5 — stale lap data was not reset", b.Tokens)
	}
}

// Invocations is nil on any event no plugin recorded against — the common case.
// This panicked inside Store.Append (the request hot path) before the nil guard.
func TestRecord_NilInvocationsDoesNotPanic(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 30, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	e := respEvent(now, 200, time.Second, "m", 10)
	if e.Invocations != nil {
		t.Fatal("precondition: helper should leave Invocations nil")
	}
	a.Record("s1", e) // must not panic

	if got := a.Snapshot(time.Minute, BucketWidth, "", GroupPlugin).Buckets[0]; got.Requests != 1 {
		t.Errorf("requests = %d, want 1", got.Requests)
	}
}

// WithMaxSessions(0) means track NO per-session rings — the reading the name
// implies. It previously fell through a `> 0` guard and disabled the cap
// entirely, so a 0 from config would have meant "unlimited".
func TestWithMaxSessions_ZeroTracksNoSessions(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 30, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)), WithMaxSessions(0))

	a.Record("s1", respEvent(now, 200, time.Second, "m", 100))

	// The all-sessions aggregate still counts everything.
	if got := a.Snapshot(time.Minute, BucketWidth, "", GroupNone).Totals.Tokens; got != 100 {
		t.Errorf("all-sessions tokens = %d, want 100", got)
	}
	// But no per-session breakdown exists.
	if got := a.Snapshot(time.Minute, BucketWidth, "s1", GroupNone).Totals.Tokens; got != 0 {
		t.Errorf("per-session tokens = %d, want 0 with maxSessions=0", got)
	}
}

func TestParseWindow(t *testing.T) {
	if d, err := ParseWindow(""); err != nil || d != 10*time.Minute {
		t.Errorf("default = %v, %v; want 10m, nil", d, err)
	}
	for _, s := range []string{"10m", "1h", "6h"} {
		if _, err := ParseWindow(s); err != nil {
			t.Errorf("ParseWindow(%q) = %v, want nil", s, err)
		}
	}
	for _, s := range []string{"30s", "7h", "90s", "garbage"} {
		if _, err := ParseWindow(s); err == nil {
			t.Errorf("ParseWindow(%q) = nil, want an error", s)
		}
	}
}

// Validation errors must not echo caller input: this is served unauthenticated,
// so reflecting query bytes into a response body would be a reflection
// primitive.
func TestParseErrors_DoNotEchoInput(t *testing.T) {
	const probe = "<script>alert(1)</script>"
	if _, err := ParseGroup(probe); err == nil {
		t.Fatal("expected an error")
	} else if contains(err.Error(), probe) {
		t.Errorf("group error echoes caller input: %q", err)
	}
	if _, err := ParseWindow(probe); err == nil {
		t.Fatal("expected an error")
	} else if contains(err.Error(), probe) {
		t.Errorf("window error echoes caller input: %q", err)
	}
}

func contains(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func keys(m map[string]Counts) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// The model name is request-controlled, so label cardinality is set off-host. A
// caller varying it every request must not be able to grow a bucket's map
// without bound — that is memory growth driven from outside the process, on the
// synchronous session-append path.
func TestRecord_LabelCardinalityIsCapped(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 30, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	const attempts = maxLabelsPerBucket * 4
	for i := 0; i < attempts; i++ {
		a.Record("s1", respEvent(now, 200, time.Second, fmt.Sprintf("model-%d", i), 10))
	}

	b := a.Snapshot(time.Minute, BucketWidth, "", GroupMethod).Buckets[0]
	if len(b.Series) > maxLabelsPerBucket {
		t.Errorf("byMethod holds %d labels, want at most %d", len(b.Series), maxLabelsPerBucket)
	}
	if _, ok := b.Series[overflowLabel]; !ok {
		t.Errorf("excess labels should fold into %q; got keys %v", overflowLabel, keys(b.Series))
	}
	// Totals must survive the capping: the point is to bound keys, not lose data.
	if b.Requests != attempts {
		t.Errorf("requests = %d, want %d — capping must not drop traffic", b.Requests, attempts)
	}
	var seriesTotal int64
	for _, c := range b.Series {
		seriesTotal += c.Requests
	}
	if seriesTotal != attempts {
		t.Errorf("series sums to %d, want %d — segments must still sum to the bucket", seriesTotal, attempts)
	}
}

// A pathologically long model name must not be retained whole.
func TestRecord_LabelLengthIsCapped(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 30, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	huge := strings.Repeat("m", 100_000)
	a.Record("s1", respEvent(now, 200, time.Second, huge, 10))

	b := a.Snapshot(time.Minute, BucketWidth, "", GroupMethod).Buckets[0]
	for k := range b.Series {
		if len(k) > maxLabelLen {
			t.Errorf("retained a %d-byte label, cap is %d", len(k), maxLabelLen)
		}
	}
}

// TestTruncateLabel_CutsOnARuneBoundaryAndKeepsTheByteCap is the cap the byte cut was
// breaking.
//
// s[:maxLabelLen] can split a multi-byte sequence, and encoding/json then expands each
// invalid byte into a 3-byte U+FFFD — so the serialised label came out LONGER than the cap
// (measured in costledger: 121 bytes cut at 96 serialised at 100). The invalid fragment is
// the smaller problem; the byte cap silently not holding is the defect.
//
// EVERY CASE HAS A ONE-BYTE PREFIX, and that is load-bearing rather than incidental.
// maxLabelLen is 96, which is divisible by 2, 3 and 4 — so a label of uniform multi-byte
// runes has a rune boundary exactly AT the cap and the byte cut is accidentally correct. The
// first version of this test in costledger passed against the unfixed code for precisely that
// reason. One ASCII byte in front moves the cut to offset 95, which is a boundary for none of
// the three widths.
func TestTruncateLabel_CutsOnARuneBoundaryAndKeepsTheByteCap(t *testing.T) {
	for _, tc := range []struct {
		name string
		rune string
	}{
		{"two-byte runes", "é"},
		{"three-byte runes", "€"},
		{"four-byte runes", "𝄞"},
		{"the replacement character sanitising produces", "�"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The prefix is what stops 96 from landing on a boundary; see the doc above.
			in := "a" + strings.Repeat(tc.rune, maxLabelLen)
			got := truncateLabel(in)

			if len(got) > maxLabelLen {
				t.Errorf("truncateLabel returned %d bytes, cap is %d", len(got), maxLabelLen)
			}
			if !utf8.ValidString(got) {
				t.Errorf("truncateLabel returned invalid UTF-8 (%q): the cut landed inside a rune",
					got)
			}
			// The reason the boundary matters, asserted as the property rather than as UTF-8
			// validity: a round trip through the encoder must not change the bytes, or the byte
			// cap this function enforces does not hold on the wire.
			enc, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var back string
			if uerr := json.Unmarshal(enc, &back); uerr != nil {
				t.Fatalf("unmarshal: %v", uerr)
			}
			if back != got {
				t.Errorf("the label changed through encoding/json: %q became %q — an invalid "+
					"trailing byte was expanded into U+FFFD", got, back)
			}
			if len(enc)-2 > maxLabelLen {
				t.Errorf("the label serialises to %d bytes, past the %d-byte cap it was cut to; "+
					"the cap does not hold where it is spent", len(enc)-2, maxLabelLen)
			}
			// And the cut must not be so eager that it drops a whole rune it could have kept: at
			// most three bytes of slack, which is the walk-back limit.
			if maxLabelLen-len(got) > 3 {
				t.Errorf("truncateLabel returned %d bytes for a %d-byte cap: it walked back further "+
					"than the longest UTF-8 sequence", len(got), maxLabelLen)
			}
		})
	}
}

// TestRingLabel_SanitisesBeforeCapping pins the ORDER, which is the half that is easy to get
// backwards and impossible to notice.
//
// Sanitising triples a control byte (one byte becomes a 3-byte U+FFFD), so capping first
// would let 96 control bytes become 288 and break the bound that exists to make a label's
// memory and line length predictable.
func TestRingLabel_SanitisesBeforeCapping(t *testing.T) {
	got := ringLabel(strings.Repeat("\x1b", maxLabelLen))
	if len(got) > maxLabelLen {
		t.Errorf("ringLabel returned %d bytes for %d control bytes, cap is %d: the cap was applied "+
			"before the rewrite that expands each byte threefold", len(got), maxLabelLen, maxLabelLen)
	}
	if strings.ContainsRune(got, 0x1b) {
		t.Errorf("ringLabel kept an ESC byte: %q", got)
	}
	if !utf8.ValidString(got) {
		t.Errorf("ringLabel returned invalid UTF-8: %q — capping split a replacement character", got)
	}
}

// TestSnapshot_TheRingDoesNotServeControlBytesInALabel is the gap that mattered more than the
// cut.
//
// The ring did not sanitise AT ALL. The model comes off the parsed request body and the
// endpoint is the host the workload asked for, so GET /v1/usage served an ANSI escape
// straight out of memory — while the ledger's copy of the same label was clean, because
// costledger sanitises on write. Two surfaces, one request, different bytes: group=model
// showed the label as two series, and only the unfixed surface could reposition an operator's
// cursor. CWE-150.
//
// U+009B IS IN THE TABLE because it is the case a byte scan cannot see: it is the
// single-character CSI, encoded as 0xC2 0x9B, so nothing about it is below 0x20 and a
// terminal decoding UTF-8 acts on it exactly as on ESC [.
func TestSnapshot_TheRingDoesNotServeControlBytesInALabel(t *testing.T) {
	for _, tc := range []struct {
		name  string
		label string
	}{
		{"an ESC-bracket colour sequence", "claude\x1b[31m-opus"},
		{"a bare carriage return", "claude\r-opus"},
		{"DEL", "claude\x7f-opus"},
		{"U+009B, the single-byte CSI a byte scan misses", "claude\u009b2J-opus"},
		{"an invalid UTF-8 byte", "claude\xff-opus"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 9, 4, 23, 30, 30, 0, time.UTC)
			a := New(WithClock(fixedClock(now)))
			e := respEvent(now, 200, time.Second, tc.label, 10)
			// Both axes, because both are set off-host and neither passed through a sanitiser.
			e.Host = tc.label
			a.Record("s1", e)

			for _, g := range []Group{GroupMethod, GroupEndpoint} {
				for k := range mergeSeries(a.Snapshot(time.Minute, BucketWidth, "", g).Buckets) {
					if hasControlRunes(k) {
						t.Errorf("group=%s served the label %q, which still carries a control rune: "+
							"/v1/usage hands it to whatever renders it, and the ledger's copy of the "+
							"same label is clean", g, k)
					}
					if !utf8.ValidString(k) {
						t.Errorf("group=%s served invalid UTF-8: %q", g, k)
					}
					if !strings.Contains(k, "�") {
						t.Errorf("group=%s served %q with no replacement character: the hostile bytes "+
							"were DROPPED rather than replaced, which collapses tampering into a "+
							"plausible-looking label nobody would question", g, k)
					}
				}
			}
		})
	}
}

// inferenceEvent builds a priceable response event with an explicit token split.
//
// Separate from respEvent because that helper predates the split fields and takes
// a single scalar token count: the tests below are about the five-way breakdown,
// which respEvent cannot express.
func inferenceEvent(model string, in, cacheRead, cacheWrite, out, reasoning int, kinds uint8) *pipeline.SessionEvent {
	return &pipeline.SessionEvent{
		At:         time.Now(),
		Phase:      pipeline.SessionResponse,
		StatusCode: 200,
		Host:       "gw.example.com",
		Inference: &pipeline.InferenceExtension{
			Model:            model,
			InputTokens:      in,
			CacheReadTokens:  cacheRead,
			CacheWriteTokens: cacheWrite,
			OutputTokens:     out,
			ReasoningTokens:  reasoning,
			TotalTokens:      in + cacheRead + cacheWrite + out,
			PresentKinds:     kinds,
		},
	}
}

// TestFoldInto_CarriesEveryCountsField is the STRUCTURAL guard behind the warning in
// Counts.Add's own doc: a hand-written summation of this struct is where a field added to
// it goes missing, and the copy that hand-summed the fields once silently dropped
// PricedRequests under a comment saying every field had to be carried.
//
// foldInto still constructs a Counts by hand — the per-request values have to come from
// somewhere — but it no longer RE-ENUMERATES the token split, which it used to build twice
// in one function: once as `split` and again field by field inside the literal. Add carries
// that half now, so a field added to Counts and wired into Add reaches the bucket with no
// edit here.
//
// Reflection rather than a list of names, because a list is the thing that goes stale. One
// event carrying every countable field non-zero, and every field of the bucket total must
// come back non-zero: a new field lands here as a failure until it is either populated in
// the fold or deliberately accounted for.
func TestFoldInto_CarriesEveryCountsField(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }))

	// Every field at once, which takes one carefully built event: a 5xx (Errors) that
	// nonetheless carries a full token split (the five token fields, PresentKinds, Tokens)
	// and a published cost record that is settled, priced and disclosed inexact (CostMicros,
	// PricedRequests, PriceableRequests, IncompleteRequests) with an applied saving on it
	// (AvoidedMicros). Nothing here is decorative.
	e := inferenceEvent("claude-opus-5", 100, 2000, 50, 30, 12, 0b11111)
	e.At = now
	e.StatusCode = 500
	raw, err := json.Marshal(costevent.Event{
		CostUSD: 0.005, Source: costevent.SourceUsageFallback, Provenance: "configured",
		Settled: true, Incomplete: true, IncompleteReason: pricing.ReasonOutputUncounted,
		// The modelled split, which a real priced record carries: the fold has to move all
		// four or they are absent from every /v1/usage total. Values differ from each other
		// so a fold that carried one field into all four would still fail.
		Tiers: &costevent.TierCost{
			Input: 0.000007, CacheWrite: 0.000022, CacheRead: 0.0003, Output: 0.00046,
		},
		// APPLIED, not projected: TotalAvoidedMicros skips a projected saving, so an
		// observe-mode entry here would leave AvoidedMicros zero and this test would fail
		// for a reason that has nothing to do with the fold carrying the field.
		Avoided: []costevent.Saving{{
			Component: "tool-prune", TokensAvoided: 400, USD: 0.0012,
			Tier: "input", Provenance: "configured", Estimated: true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.Plugins = map[string]json.RawMessage{costevent.Key: raw}

	a.Record("s1", e)
	totals := a.Snapshot(10*BucketWidth, BucketWidth, "s1", GroupNone).Totals

	// Saturated is the ONE field one event cannot populate, and it is asserted the other way
	// round rather than skipped. It is a fault disclosure, not a carried value: it can only
	// be true if an addition hit the int64 ceiling, which a single event cannot do and which
	// no real deployment should ever see. Exempting it with a bare `continue` would let it
	// become permanently true — a caveat on every clean response — with nothing here to
	// notice, so the exemption is spelled as its own expectation instead.
	// TestCountsAdd_EveryInt64FieldSaturatesAndSaysSo covers the true case.
	//
	// RefusedTokenRequests is exempt on the same footing and for the same reason: this
	// event's token report is plausible, so refusing it would be the bug. Its non-zero case
	// is TestFoldInto_RefusesAnImplausibleTokenReportWhicheverFieldCarriesIt.
	assertedAbsent := map[string]bool{"Saturated": true, "RefusedTokenRequests": true}
	v := reflect.ValueOf(totals)
	for i := 0; i < v.NumField(); i++ {
		name := v.Type().Field(i).Name
		if assertedAbsent[name] {
			if !v.Field(i).IsZero() {
				t.Errorf("Counts.%s is set after folding one ordinary event: it is a fault "+
					"disclosure and ordinary traffic must leave it clean, or every response "+
					"carries a caveat that means nothing", name)
			}
			continue
		}
		if v.Field(i).IsZero() {
			t.Errorf("Counts.%s came back zero: the fold does not carry it, so this field is absent from every /v1/usage total. Carry it in foldInto (the token split rides along via Counts.Add) or, if one event genuinely cannot populate it, say so in assertedAbsent above.",
				name)
		}
	}
}

// tokenSources maps each token field of Counts to the pipeline.InferenceExtension field
// foldInto reads it from.
//
// A map rather than a list of cases, so the test below can assert it COVERS the struct: a
// seventh token field added to Counts and read from a new wire field fails here by name
// instead of arriving unguarded. That is the whole reason this is not six hand-written
// subtests — Tokens was unbounded for as long as it was because nothing enumerated the
// fields that come from the provider.
var tokenSources = map[string]string{
	"Tokens":           "TotalTokens",
	"InputTokens":      "InputTokens",
	"CacheReadTokens":  "CacheReadTokens",
	"CacheWriteTokens": "CacheWriteTokens",
	"OutputTokens":     "OutputTokens",
	"ReasoningTokens":  "ReasoningTokens",
}

// countsTokenFields returns the reflect field indexes of Counts' token figures, and fails if
// any of them has no entry in tokenSources.
func countsTokenFields(t *testing.T) []int {
	t.Helper()
	rt := reflect.TypeOf(Counts{})
	var out []int
	for i := range rt.NumField() {
		name := rt.Field(i).Name
		if !strings.HasSuffix(name, "Tokens") {
			continue
		}
		if _, ok := tokenSources[name]; !ok {
			t.Errorf("Counts.%s is a token figure with no entry in tokenSources: it is read from "+
				"a provider-controlled field and nothing here checks that an implausible value is "+
				"refused", name)
			continue
		}
		out = append(out, i)
	}
	if len(out) != len(tokenSources) {
		t.Fatalf("matched %d token fields of Counts against %d mapped sources; the map and the "+
			"struct have diverged and this test no longer covers what it claims",
			len(out), len(tokenSources))
	}
	return out
}

// TestFoldInto_RefusesAnImplausibleTokenReportWhicheverFieldCarriesIt is the token half of
// the bound cost already had.
//
// Cost is bounded per request twice over and tokens were not bounded at all: every figure
// came from a provider-controlled `int` on the wire, straight into the aggregate. So the
// same forged response that could not move the dollar total by more than $10,000 could move
// the token total by 9.2e18, and a negative one could move it DOWN — making a real bill look
// smaller, which is the direction that gets exploited.
//
// Three shapes per field, because they fail differently: past the ceiling (the plausible
// case), the whole int64 range (the wrap case, which Counts.Add now clamps but should never
// see), and negative (the subtraction). Driven by reflection over Counts so a token field
// added later cannot skip this.
func TestFoldInto_RefusesAnImplausibleTokenReportWhicheverFieldCarriesIt(t *testing.T) {
	fields := countsTokenFields(t)
	rt := reflect.TypeOf(Counts{})
	for _, i := range fields {
		name := rt.Field(i).Name
		wire := tokenSources[name]
		for _, bad := range []struct {
			what string
			v    int
		}{
			{"one past the plausible ceiling", maxPlausibleRequestTokens + 1},
			{"the whole int64 range", math.MaxInt},
			{"negative, which subtracts from the aggregate", -1},
		} {
			t.Run(name+"/"+bad.what, func(t *testing.T) {
				now := time.Now().Truncate(BucketWidth)
				a := New(WithClock(func() time.Time { return now }))
				e := inferenceEvent("claude-opus-5", 10, 20, 30, 40, 5, 0b11111)
				e.At = now
				reflect.ValueOf(e.Inference).Elem().FieldByName(wire).SetInt(int64(bad.v))
				a.Record("s1", e)
				totals := a.Snapshot(10*BucketWidth, BucketWidth, "s1", GroupNone).Totals

				if totals.RefusedTokenRequests != 1 {
					t.Errorf("RefusedTokenRequests = %d after %s = %d (%s), want 1 — a dropped "+
						"figure that is not counted is indistinguishable from traffic that used no "+
						"tokens", totals.RefusedTokenRequests, wire, bad.v, bad.what)
				}
				// The WHOLE report goes, not just the offending field: a believed figure beside a
				// refused one is a breakdown that cannot be reconciled against its own total.
				for _, j := range fields {
					if got := reflect.ValueOf(totals).Field(j).Int(); got != 0 {
						t.Errorf("Counts.%s = %d, want 0: %s was %s (%d), so no figure in this "+
							"report is trustworthy", rt.Field(j).Name, got, wire, bad.what, bad.v)
					}
				}
				if totals.PresentKinds != 0 {
					t.Errorf("PresentKinds = %#b after a refused report, want 0 — those bits assert "+
						"the provider REPORTED these kinds, which is the claim being refused",
						totals.PresentKinds)
				}
				// The request still happened, and this says nothing about whether it was billed.
				if totals.Requests != 1 {
					t.Errorf("Requests = %d, want 1: refusing the token report must not drop the "+
						"request", totals.Requests)
				}
			})
		}
	}
}

// TestFoldInto_AcceptsATokenReportAtTheCeiling is the control that keeps the bound from
// being a coverage gap dressed as a guard.
//
// A refusal is a figure withheld, so a bound set too low is the same defect in the other
// direction — and one that would only be discovered when a legitimately large context
// window arrived. The ceiling is INCLUSIVE, and the largest real call anyone can construct
// today is 5x under it.
func TestFoldInto_AcceptsATokenReportAtTheCeiling(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }))
	e := inferenceEvent("claude-opus-5", maxPlausibleRequestTokens, 0, 0, 0, 0, 0b1)
	e.At = now
	e.Inference.TotalTokens = maxPlausibleRequestTokens
	a.Record("s1", e)
	totals := a.Snapshot(10*BucketWidth, BucketWidth, "s1", GroupNone).Totals

	if totals.RefusedTokenRequests != 0 {
		t.Errorf("RefusedTokenRequests = %d for a report exactly at the ceiling, want 0: the bound "+
			"is inclusive, and refusing here withholds a figure that is real",
			totals.RefusedTokenRequests)
	}
	if totals.Tokens != maxPlausibleRequestTokens || totals.InputTokens != maxPlausibleRequestTokens {
		t.Errorf("Tokens = %d, InputTokens = %d, want %d for both", totals.Tokens,
			totals.InputTokens, int64(maxPlausibleRequestTokens))
	}
}

func TestCounts_CarriesTheTokenSplit(t *testing.T) {
	a := New()
	a.Record("s1", inferenceEvent("m", 100, 2000, 50, 30, 0, 0b1111))

	snap := a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupNone)

	if got := snap.Totals.InputTokens; got != 100 {
		t.Errorf("InputTokens = %d, want 100", got)
	}
	if got := snap.Totals.CacheReadTokens; got != 2000 {
		t.Errorf("CacheReadTokens = %d, want 2000", got)
	}
	if got := snap.Totals.CacheWriteTokens; got != 50 {
		t.Errorf("CacheWriteTokens = %d, want 50", got)
	}
	if got := snap.Totals.OutputTokens; got != 30 {
		t.Errorf("OutputTokens = %d, want 30", got)
	}
	// The legacy aggregate must keep working for clients written against it.
	if got := snap.Totals.Tokens; got != 2180 {
		t.Errorf("Tokens = %d, want 2180 (unchanged legacy sum)", got)
	}
}

func TestCounts_ReasoningIsNotAddedToOutput(t *testing.T) {
	// ReasoningTokens is a SUBSET of OutputTokens: the provider reports how much
	// of the generated output was reasoning. Adding them double-counts every
	// reasoning token, and at the output rate -- the most expensive tier.
	a := New()
	a.Record("s1", inferenceEvent("m", 0, 0, 0, 100, 40, 0b11001))

	snap := a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupNone)

	if got := snap.Totals.OutputTokens; got != 100 {
		t.Errorf("OutputTokens = %d, want 100 -- reasoning must not be added", got)
	}
	if got := snap.Totals.ReasoningTokens; got != 40 {
		t.Errorf("ReasoningTokens = %d, want 40 reported alongside, not folded in", got)
	}
}

func TestCounts_SplitAccumulatesAcrossEvents(t *testing.T) {
	a := New()
	a.Record("s1", inferenceEvent("m", 10, 100, 5, 3, 0, 0b1111))
	a.Record("s1", inferenceEvent("m", 20, 200, 7, 4, 0, 0b1111))

	snap := a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupNone)

	if got := snap.Totals.InputTokens; got != 30 {
		t.Errorf("InputTokens = %d, want 30", got)
	}
	if got := snap.Totals.CacheReadTokens; got != 300 {
		t.Errorf("CacheReadTokens = %d, want 300", got)
	}
}

func TestCounts_PresentKindsFoldsByOr(t *testing.T) {
	// One provider reports cache counters, another reports input/output. A reader of
	// the window total must be able to tell "no cache writes happened" from "nothing
	// here reports cache writes" -- otherwise a blank column is unreadable.
	//
	// TWO operand shapes, because no single pair pins `|=` on its own and each kills
	// a different mutation of Counts.Add:
	//
	//	disjoint     0b0110, 0b1001 -> 0b1111. Neither operand equals the union, so
	//	             outright assignment (`|=` -> `=`) dies here. A superset pair --
	//	             which an earlier version of this test used -- would not even do
	//	             that, and max() and an `if == 0` guard both survive it.
	//	overlapping  0b1001, 0b1001 -> 0b1001. `+=` CARRIES into a bit nothing
	//	             reported (0b10010, the Reasoning bit), so it dies here. Against a
	//	             disjoint pair `+` and `|` are arithmetically identical, which is
	//	             why the pair above cannot pin the operator Add's own godoc warns
	//	             about ("Do not pattern-match on the `+=` below").
	//
	// The overlapping case is the one that matters in production: two Output-only
	// responses in a bucket fold to the Reasoning bit, and a consumer then prints
	// "reasoning (of output) 0" -- a reported zero where nothing reported reasoning
	// at all, exactly the set-versus-zero confusion PresentKinds exists to resolve.
	// Carries can also overflow the uint8 and clear real bits.
	for _, tc := range []struct {
		name       string
		first, snd uint8
		want       uint8
	}{
		{"disjoint kills assignment", 0b0110, 0b1001, 0b1111},
		{"overlapping kills addition", 0b1001, 0b1001, 0b1001},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := New()
			a.Record("s1", inferenceEvent("first-reporter", 0, 200, 5, 0, 0, tc.first))
			a.Record("s1", inferenceEvent("second-reporter", 10, 0, 0, 5, 0, tc.snd))

			snap := a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupNone)

			if got := snap.Totals.PresentKinds; got != tc.want {
				t.Errorf("PresentKinds = %#b, want %#b (union of what any response reported)", got, tc.want)
			}
		})
	}
}

func TestCounts_PresentKindsZeroWhenNothingReports(t *testing.T) {
	a := New()
	a.Record("s1", inferenceEvent("m", 0, 0, 0, 0, 0, 0))

	snap := a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupNone)

	if got := snap.Totals.PresentKinds; got != 0 {
		t.Errorf("PresentKinds = %#b, want 0", got)
	}
}

func TestCounts_SplitSurvivesFolding(t *testing.T) {
	// fold() is the second place Counts are summed. It delegates to Counts.Add,
	// so a new field is carried automatically -- but that is exactly the property
	// that silently broke for PricedRequests once, which is why Add is exported
	// and why this test exists.
	a := New()
	a.Record("s1", inferenceEvent("m", 10, 100, 5, 3, 0, 0b1111))

	snap := a.Snapshot(10*time.Minute, 5*time.Minute, "s1", GroupNone)

	var in, cr int64
	var kinds uint8
	for _, b := range snap.Buckets {
		in += b.InputTokens
		cr += b.CacheReadTokens
		kinds |= b.PresentKinds
	}
	if in != 10 || cr != 100 {
		t.Errorf("folded buckets: InputTokens = %d (want 10), CacheReadTokens = %d (want 100)", in, cr)
	}
	if kinds != 0b1111 {
		t.Errorf("folded PresentKinds = %#b, want %#b", kinds, 0b1111)
	}
}

// One upstream must be one band. A CONNECT tunnel-open records the authority
// from the request line, port included, while the request parsed inside that
// tunnel records the bare host — so without stripping, "api.anthropic.com:443"
// and "api.anthropic.com" are two series for one host, and neither carries the
// real total.
func TestRecord_HostGroupingStripsThePort(t *testing.T) {
	now := time.Date(2026, 9, 17, 14, 0, 0, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	for _, h := range []string{"api.anthropic.com:443", "api.anthropic.com", "api.anthropic.com:443"} {
		e := respEvent(now, 200, time.Second, "claude-sonnet-5", 100)
		e.Host = h
		a.Record("s1", e)
	}

	b := a.Snapshot(time.Minute, BucketWidth, "", GroupHost).Buckets[0]
	if len(b.Series) != 1 {
		t.Fatalf("one host must be one series; got %v", keys(b.Series))
	}
	got, ok := b.Series["api.anthropic.com"]
	if !ok {
		t.Fatalf("series not keyed by the bare host; got %v", keys(b.Series))
	}
	if got.Requests != 3 {
		t.Errorf("Requests = %d, want 3 — the band must carry every request to that host", got.Requests)
	}
}

// An event the listener left host-less must not invent a band. It stays out of
// the series and abctl renders the difference as "(unlabelled)", which is what
// the other groupings already do for events they skip.
func TestRecord_HostGroupingSkipsEmptyHost(t *testing.T) {
	now := time.Date(2026, 9, 17, 14, 0, 0, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	withHost := respEvent(now, 200, time.Second, "claude-sonnet-5", 100)
	withHost.Host = "github-tool-mcp"
	a.Record("s1", withHost)
	a.Record("s1", respEvent(now, 200, time.Second, "claude-sonnet-5", 100)) // Host unset

	snap := a.Snapshot(time.Minute, BucketWidth, "", GroupHost)
	if snap.Totals.Requests != 2 {
		t.Fatalf("Totals.Requests = %d, want 2", snap.Totals.Requests)
	}
	b := snap.Buckets[0]
	if len(b.Series) != 1 {
		t.Fatalf("host-less event must not add a band; got %v", keys(b.Series))
	}
	if _, ok := b.Series[""]; ok {
		t.Error(`a "" key would render as a nameless band`)
	}
	// The banded total is deliberately less than the bucket total: that shortfall
	// is what the renderer names "(unlabelled)".
	if got := b.Series["github-tool-mcp"].Requests; got != 1 {
		t.Errorf("Requests = %d, want 1", got)
	}
}

// IPv6 authorities must survive: SplitHostPort understands the bracketed form,
// and a bare literal with no port is already the label.
func TestRecord_HostGroupingHandlesIPv6(t *testing.T) {
	now := time.Date(2026, 9, 17, 14, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ authority, want string }{
		// Every spelling of one address must be one band. SplitHostPort unwraps the
		// brackets when it finds a port, so the port-less form has to be unwrapped
		// too — otherwise "[::1]" is its own series for the host "::1", which is the
		// split this helper exists to prevent.
		{"[::1]:9094", "::1"},
		{"[::1]", "::1"},
		{"::1", "::1"},
		{"[2001:db8::1]:443", "2001:db8::1"},
		{"[2001:db8::1]", "2001:db8::1"},
		{"127.0.0.1:47600", "127.0.0.1"},
	} {
		t.Run(tc.authority, func(t *testing.T) {
			a := New(WithClock(fixedClock(now)))
			e := respEvent(now, 200, time.Second, "", 0)
			e.Host = tc.authority
			a.Record("s1", e)
			b := a.Snapshot(time.Minute, BucketWidth, "", GroupHost).Buckets[0]
			if _, ok := b.Series[tc.want]; !ok {
				t.Errorf("want key %q; got %v", tc.want, keys(b.Series))
			}
		})
	}
}

// An authority that carries a port but no host reduces to nothing, so it is
// treated as absent and joins the "(unlabelled)" remainder rather than keying a
// band with no name. Reachable from the Host header, so it is worth pinning: the
// point is that Totals stay whole while the series does not gain a blank key.
func TestRecord_HostGroupingSkipsAPortWithNoHost(t *testing.T) {
	now := time.Date(2026, 9, 17, 14, 0, 0, 0, time.UTC)
	for _, authority := range []string{":443", ":", "[]"} {
		t.Run(authority, func(t *testing.T) {
			a := New(WithClock(fixedClock(now)))
			good := respEvent(now, 200, time.Second, "", 0)
			good.Host = "api.anthropic.com"
			a.Record("s1", good)
			bad := respEvent(now, 200, time.Second, "", 0)
			bad.Host = authority
			a.Record("s1", bad)

			snap := a.Snapshot(time.Minute, BucketWidth, "", GroupHost)
			if snap.Totals.Requests != 2 {
				t.Fatalf("Totals.Requests = %d, want 2 — the request still happened", snap.Totals.Requests)
			}
			b := snap.Buckets[0]
			if _, ok := b.Series[""]; ok {
				t.Error(`a "" key would render as a nameless band`)
			}
			if len(b.Series) != 1 {
				t.Errorf("series = %v, want only the real host", keys(b.Series))
			}
		})
	}
}
