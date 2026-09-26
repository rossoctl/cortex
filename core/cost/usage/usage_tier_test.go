package usage

import (
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/pipeline"
)

// tierResolver prices all FOUR tiers. resolverFor sets only input and output, which cannot
// tell a per-tier split from the prompt/output halves that already exist — the cache tiers
// are the whole point.
//
// Rates chosen so tokens and money rank differently: cache-read is cheapest per token and
// output dearest, so the fixture below has cache-read winning on volume and losing on cost.
func tierResolver(t *testing.T, model string) *pricing.Registry {
	t.Helper()
	var r pricing.Rates
	for tier, perToken := range map[pricing.Tier]float64{
		pricing.TierInput:      7.0 / 1e6,
		pricing.TierCacheWrite: 11.0 / 1e6,
		pricing.TierCacheRead:  3.0 / 1e6,
		pricing.TierOutput:     23.0 / 1e6,
	} {
		r.Base[tier], r.Set[tier] = perToken, true
	}
	tab, err := pricing.NewTable([]pricing.Entry{
		{Host: "*", Model: model, Rates: r, Prov: pricing.ProvConfigured},
	})
	if err != nil {
		t.Fatal(err)
	}
	return pricing.NewRegistry(tab)
}

// tierRespEvent is pricedRespEvent with the cache tiers filled in.
func tierRespEvent(model string, input, cacheWrite, cacheRead, output int) *pipeline.SessionEvent {
	return &pipeline.SessionEvent{
		Phase:      pipeline.SessionResponse,
		Host:       "gw.internal:4000",
		StatusCode: 200,
		Inference: &pipeline.InferenceExtension{
			Model:            model,
			InputTokens:      input,
			CacheWriteTokens: cacheWrite,
			CacheReadTokens:  cacheRead,
			OutputTokens:     output,
			TotalTokens:      input + cacheWrite + cacheRead + output,
		},
	}
}

// fallbackFixtureWithUsage runs one event with NO cost record through an aggregator that
// has a rate table — the fallback path — and fails loudly if nothing priced, since an
// unpriced snapshot would make every assertion below vacuous.
func fallbackFixtureWithUsage(t *testing.T, input, cacheWrite, cacheRead, output int) Snapshot {
	t.Helper()
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }),
		WithPricing(tierResolver(t, "claude-opus-5")))
	a.Record("s1", tierRespEvent("claude-opus-5", input, cacheWrite, cacheRead, output))
	snap := snapshotOf(a, now)
	if !snap.Priced || snap.Totals.CostMicros == 0 {
		t.Fatalf("fixture broken: the fallback path priced nothing (%+v)", snap.Totals)
	}
	return snap
}

// fallbackFixtureNoRates is the same event with no resolver at all.
func fallbackFixtureNoRates(t *testing.T) Snapshot {
	t.Helper()
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }))
	a.Record("s1", tierRespEvent("claude-opus-5", 1000, 2000, 100000, 20000))
	return snapshotOf(a, now)
}

// The FALLBACK path prices events itself, and must produce the same split the record path
// carries.
//
// THIS TEST EXISTS BECAUSE TestFoldInto_CarriesEveryCountsField USES A PUBLISHED RECORD.
// There are two sources of cost in this package and that guard exercises one, so the
// fallback could ship entirely dead behind a green suite — which is exactly the shape of
// defect that has bitten this feature's surface three times already.
func TestAggregator_TheFallbackPathCarriesTheTierSplit(t *testing.T) {
	got := fallbackFixtureWithUsage(t, 1000, 2000, 100000, 20000).Totals

	// 100k cache-read tokens at 3 = 300k micros; 20k output tokens at 23 = 460k. So
	// cache-read wins on volume 5:1 and loses on money, and a split derived from token
	// counts fails here.
	if got.OutputCostMicros <= got.CacheReadCostMicros {
		t.Errorf("output %d is not above cache-read %d — the fallback split looks "+
			"token-derived, or was never populated", got.OutputCostMicros, got.CacheReadCostMicros)
	}
	for name, v := range map[string]int64{
		"input": got.InputCostMicros, "cache-write": got.CacheWriteCostMicros,
		"cache-read": got.CacheReadCostMicros, "output": got.OutputCostMicros,
	} {
		if v == 0 {
			t.Errorf("%s tier is zero on a request that used every tier", name)
		}
	}
	// Each tier is its own count at its own rate.
	for name, pair := range map[string]struct{ have, want int64 }{
		"input":       {got.InputCostMicros, 1000 * 7},
		"cache-write": {got.CacheWriteCostMicros, 2000 * 11},
		"cache-read":  {got.CacheReadCostMicros, 100000 * 3},
		"output":      {got.OutputCostMicros, 20000 * 23},
	} {
		if pair.have != pair.want {
			t.Errorf("%s = %d micros, want %d", name, pair.have, pair.want)
		}
	}
}

// A request the rate table cannot price contributes NO mix. Zeros here would dilute a mix
// drawn from its neighbours and silently reshape the split.
func TestAggregator_AnUnpricedRequestAddsNoMix(t *testing.T) {
	got := fallbackFixtureNoRates(t).Totals

	if got.CostMicros != 0 {
		t.Fatalf("fixture broken: something priced this with no resolver (%d)", got.CostMicros)
	}
	if got.InputCostMicros != 0 || got.CacheWriteCostMicros != 0 ||
		got.CacheReadCostMicros != 0 || got.OutputCostMicros != 0 {
		t.Errorf("an unpriced request contributed a mix: %+v", got)
	}
}
