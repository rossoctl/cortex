package settle

import (
	"net/http"
	"testing"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/pipeline"
)

// tierCtx is ctx() with the two CACHE tiers filled in, which the shared helper leaves at
// zero. A split that exercises only input and output cannot tell a per-tier figure from a
// prompt/output half, and the halves already exist — so the cache tiers are the whole
// point of this fixture.
func tierCtx(input, cacheWrite, cacheRead, output int) *pipeline.Context {
	return &pipeline.Context{
		Host:            "gw.internal",
		ResponseHeaders: http.Header{},
		Extensions: pipeline.Extensions{Inference: &pipeline.InferenceExtension{
			Model:            "claude-opus-5",
			InputTokens:      input,
			CacheWriteTokens: cacheWrite,
			CacheReadTokens:  cacheRead,
			OutputTokens:     output,
		}},
	}
}

// settleFixtureWithUsage settles one request against the tiered table and returns the
// PUBLISHED record, failing loudly if its own premise broke: an unpriced record would
// make every assertion below vacuous.
func settleFixtureWithUsage(t *testing.T, input, cacheWrite, cacheRead, output int) event.Event {
	t.Helper()
	s := Settle(tierCtx(input, cacheWrite, cacheRead, output), rates(t))
	if !s.HasModelled {
		t.Fatalf("fixture broken: nothing was modelled, so the assertions would be vacuous (%+v)", s)
	}
	return NewRecord(s, nil)
}

// settleFixtureNoRates is the same request with no rate table at all, so nothing can be
// modelled and no split can exist.
func settleFixtureNoRates(t *testing.T) event.Event {
	t.Helper()
	s := Settle(tierCtx(1000, 2000, 100000, 20000), nil)
	if s.HasModelled {
		t.Fatal("fixture broken: something was modelled with no resolver")
	}
	return NewRecord(s, nil)
}

// A settled request carries the modelled split, and the split is MONEY rather than tokens.
//
// The fixture inverts the two on purpose. Against the test table — cache-read 3 micros a
// token, output 23 — 100,000 cache-read tokens cost 300,000 micros while 20,000 output
// tokens cost 460,000. So cache-read wins on tokens 5:1 and loses on money, and an
// implementation that split by token count fails here. A fixture where both ranked the
// same way would pass either implementation.
func TestSettle_PublishesTheModelledTierSplit(t *testing.T) {
	rec := settleFixtureWithUsage(t, 1000, 2000, 100000, 20000)

	if rec.Tiers == nil {
		t.Fatal("a priced request published no tier split")
	}
	if rec.Tiers.Output <= rec.Tiers.CacheRead {
		t.Errorf("output %.6f is not above cache-read %.6f — the split looks token-derived",
			rec.Tiers.Output, rec.Tiers.CacheRead)
	}
	if rec.Tiers.Input <= 0 || rec.Tiers.CacheWrite <= 0 {
		t.Errorf("a tier that carried tokens has no cost: %+v", *rec.Tiers)
	}
	// Each tier is its own count at its own rate, to the micro.
	for name, got := range map[string]struct{ have, want float64 }{
		"input":       {rec.Tiers.Input, 1000 * tierMicros[pricing.TierInput] / 1e6},
		"cache-write": {rec.Tiers.CacheWrite, 2000 * tierMicros[pricing.TierCacheWrite] / 1e6},
		"cache-read":  {rec.Tiers.CacheRead, 100000 * tierMicros[pricing.TierCacheRead] / 1e6},
		"output":      {rec.Tiers.Output, 20000 * tierMicros[pricing.TierOutput] / 1e6},
	} {
		if got.have != got.want {
			t.Errorf("%s tier = %.6f, want %.6f", name, got.have, got.want)
		}
	}
}

// No rate table means no split, and the record says so by ABSENCE.
func TestSettle_NoRateTableLeavesTiersNil(t *testing.T) {
	rec := settleFixtureNoRates(t)

	if rec.Tiers != nil {
		t.Errorf("Tiers = %+v for a request with no rates; nil is how the record says "+
			"\"no split\", and a zero struct would read as four free tiers", *rec.Tiers)
	}
}
