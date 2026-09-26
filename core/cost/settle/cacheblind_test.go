package settle

import (
	"net/http"
	"testing"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/pipeline"
)

// A GATEWAY FIGURE THAT PRICES ONLY THE UNCACHED TIERS IS NOT AN ANSWER ABOUT THIS REQUEST.
//
// Measured against ete-litellm over four days of one laptop's traffic: of 322 non-streamed
// responses carrying a usable cost header, 208 matched the rate table EXACTLY (ratio 1.000,
// which is the strongest confirmation available that the table and its gateway multiplier are
// right) and the other 114 matched the input+output cost alone, with both cache tiers priced at
// nothing. Nothing landed in between at a 5% tolerance.
//
// The signature was unmistakable once the requests were read one at a time rather than in
// aggregate: many reported the identical $0.000205 — exactly 90 input + 9 output tokens at
// sonnet-5's rates — while carrying 55k-416k cache tokens worth up to $0.77. Cache tokens are
// most of what an agent's traffic costs, so the figure is not slightly low, it is a different
// quantity.
//
// Preferring it understated those 114 requests by 17.8x ($0.225 charged against $4.00 modelled).
// The gateway's own spend records bill them in full, so this is the header disagreeing with its
// own gateway, not a discount.
//
// WHY THE HEADER IS STILL KEPT ON ReportedUSD: it is what the gateway said, and the drift
// reporter needs both figures to say anything. Refusing it as the CHARGED figure and discarding
// it are different acts.
//
// cacheTierCtx carries all four tiers, which costing_test.go's ctx cannot: this defect is
// invisible without cache tokens, which is exactly why the 2026-09-11 validation in settle.go —
// a 16-input / 4-output call — could not have caught it.
func cacheTierCtx(headers map[string]string, input, output, cacheWrite, cacheRead int) *pipeline.Context {
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	return &pipeline.Context{
		Host:            "gw.internal",
		ResponseHeaders: h,
		Extensions: pipeline.Extensions{Inference: &pipeline.InferenceExtension{
			Model:            "claude-opus-5",
			InputTokens:      input,
			OutputTokens:     output,
			CacheWriteTokens: cacheWrite,
			CacheReadTokens:  cacheRead,
		}},
	}
}

// The fixture's figures, in the tier arithmetic rather than as literals, so the assertion says
// which rate each count is charged at. tierMicros: cacheRead 3, input 7, cacheWrite 11,
// output 23 micro-dollars per token.
const (
	blindInput, blindOutput    = 1000, 500
	blindCacheWr, blindCacheRd = 2000, 100_000
)

func uncachedUSD() float64 {
	return (blindInput*tierMicros[pricing.TierInput] + blindOutput*tierMicros[pricing.TierOutput]) / 1e6
}

func cachedUSD() float64 {
	return (blindCacheWr*tierMicros[pricing.TierCacheWrite] + blindCacheRd*tierMicros[pricing.TierCacheRead]) / 1e6
}

func TestSettle_CacheBlindHeaderLosesToTheTable(t *testing.T) {
	const json = "application/json"
	full := uncachedUSD() + cachedUSD()
	for _, tc := range []struct {
		name       string
		header     string
		input      int
		output     int
		cacheWrite int
		cacheRead  int
		wantCost   float64
		wantSrc    string
		wantBlind  bool
	}{{
		// The defect: the header is the uncached half to the cent, and the cache tiers carry
		// 94% of what the request cost.
		name:   "a header pricing only input and output is refused",
		header: "0.0185", // == uncachedUSD()
		input:  blindInput, output: blindOutput, cacheWrite: blindCacheWr, cacheRead: blindCacheRd,
		wantCost:  full,
		wantSrc:   event.SourceUsageFallback,
		wantBlind: true,
	}, {
		// The 208-row majority. Same tokens, a header that prices all four tiers: it wins, as
		// it always has, and nothing about this path changes.
		name:   "a header pricing every tier still wins outright",
		header: "0.3405", // == full
		input:  blindInput, output: blindOutput, cacheWrite: blindCacheWr, cacheRead: blindCacheRd,
		wantCost:  0.3405,
		wantSrc:   event.SourceGatewayHeader,
		wantBlind: false,
	}, {
		// THE CASE THE GUARD MUST NOT CONFUSE, and it is every ordinary request: with no cache
		// tokens the uncached figure IS the whole figure, so "the header equals the uncached
		// tiers" is true of a perfectly correct header. Keyed on the cache tiers being material,
		// never on the equality alone.
		name:   "a cache-free request keeps its header",
		header: "0.0185",
		input:  blindInput, output: blindOutput,
		wantCost:  0.0185,
		wantSrc:   event.SourceGatewayHeader,
		wantBlind: false,
	}, {
		// Immaterial cache: 100 cache reads is $0.0003 against $0.0185, under the tolerance. A
		// header that omitted it would be indistinguishable from one that rounded, so the
		// guard leaves it alone rather than substituting a figure on noise.
		name:   "an immaterial cache component does not trip the guard",
		header: "0.0185",
		input:  blindInput, output: blindOutput, cacheRead: 100,
		wantCost:  0.0185,
		wantSrc:   event.SourceGatewayHeader,
		wantBlind: false,
	}, {
		// A header that is low but matches NOTHING in particular. This package cannot say which
		// figure is wrong, so it keeps the gateway's — the drift reporter's job is to say so,
		// and inventing a substitution here would be the "set a multiplier" mistake in code.
		name:   "an unexplained low header is kept, not second-guessed",
		header: "0.1000",
		input:  blindInput, output: blindOutput, cacheWrite: blindCacheWr, cacheRead: blindCacheRd,
		wantCost:  0.1,
		wantSrc:   event.SourceGatewayHeader,
		wantBlind: false,
	}, {
		// A DISCOUNTED FULL-TIER HEADER JUST ABOVE A 5% CACHE SHARE, which the first version of
		// this guard refused — the fixed bug inverted, charging a figure the gateway never said.
		//
		// 394 cache reads is $0.001182 against $0.0185 uncached: a 6.0% cache share, clear of a
		// 5% materiality floor. This header prices all four tiers and takes 4% off, landing at
		// $0.0188947 — inside a match window of [0.017575, 0.019425] derived from the uncached
		// figure. At that cache share "the uncached tiers" and "the whole thing, discounted" are
		// the SAME interval, so no comparison against the uncached figure can separate them, and
		// drift.go would call the identical pair agreement at the identical 5%.
		//
		// So the floor is not materiality but DISJOINTNESS — see headerOmitsCache. It costs no
		// real coverage: the 114 observed rows sit near a 99% cache share, 90 input tokens
		// against 55k-416k cached ones.
		name:   "a discounted full-tier header at a low cache share is not read as cache-blind",
		header: "0.0188947",
		input:  blindInput, output: blindOutput, cacheRead: 394,
		wantCost:  0.0188947,
		wantSrc:   event.SourceGatewayHeader,
		wantBlind: false,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			pctx := cacheTierCtx(map[string]string{
				"Content-Type": json, ResponseCostHeader: tc.header,
			}, tc.input, tc.output, tc.cacheWrite, tc.cacheRead)

			got := Settle(pctx, rates(t))

			if !got.Priced {
				t.Fatalf("not priced: %+v", got)
			}
			if d := got.CostUSD - tc.wantCost; d > 1e-9 || d < -1e-9 {
				t.Errorf("CostUSD = %.6f, want %.6f", got.CostUSD, tc.wantCost)
			}
			if got.Source != tc.wantSrc {
				t.Errorf("Source = %q, want %q", got.Source, tc.wantSrc)
			}
			if got.HeaderOmittedCache != tc.wantBlind {
				t.Errorf("HeaderOmittedCache = %v, want %v", got.HeaderOmittedCache, tc.wantBlind)
			}
			// Whichever figure won, the gateway's own remains available: the drift reporter
			// compares the pair, and a refusal that erased it would leave nothing to report.
			if !got.HasReported {
				t.Error("the gateway's figure was discarded; drift has nothing to compare")
			}
		})
	}
}

// THE SUBSTITUTION HAS TO REACH THE RECORD, or it is knowledge held in memory and lost on disk.
//
// Settled carries both figures so the drift reporter can compare them, and the parser's warning
// fires once per endpoint and model — neither of which lets anyone audit a ledger afterwards. On
// the wire a substituted row would otherwise be Source: usage-fallback and nothing else, which
// is also what a stream that never had a header looks like: the per-request analysis that found
// this defect could not have been repeated on a ledger written after the fix.
func TestNewRecord_CarriesTheRefusedGatewayFigure(t *testing.T) {
	const json = "application/json"
	blind := Settle(cacheTierCtx(map[string]string{
		"Content-Type": json, ResponseCostHeader: "0.0185",
	}, blindInput, blindOutput, blindCacheWr, blindCacheRd), rates(t))
	rec := NewRecord(blind, nil)

	if !rec.HeaderOmittedCache {
		t.Error("the record does not say the gateway's figure was refused")
	}
	if rec.GatewayUSD != 0.0185 {
		t.Errorf("GatewayUSD = %v, want the refused header's 0.0185", rec.GatewayUSD)
	}
	if rec.CostUSD != uncachedUSD()+cachedUSD() {
		t.Errorf("CostUSD = %v, want the modelled figure", rec.CostUSD)
	}

	// And NOT duplicated on the ordinary path: where the header wins it IS CostUSD, and Source
	// says so, so a second copy would be bytes on every event saying nothing.
	won := Settle(cacheTierCtx(map[string]string{
		"Content-Type": json, ResponseCostHeader: "0.3405",
	}, blindInput, blindOutput, blindCacheWr, blindCacheRd), rates(t))
	rec = NewRecord(won, nil)
	if rec.HeaderOmittedCache || rec.GatewayUSD != 0 {
		t.Errorf("a header that won is republished as a refusal: omitted=%v gateway=%v",
			rec.HeaderOmittedCache, rec.GatewayUSD)
	}
}

// The substitution must not happen when there is nothing to substitute. A model with no rates
// leaves HasModelled false, and a guard that fired anyway would unprice a request the gateway
// had priced — trading a low figure for none at all.
func TestSettle_CacheBlindGuardNeedsAModelledFigure(t *testing.T) {
	pctx := cacheTierCtx(map[string]string{
		"Content-Type": "application/json", ResponseCostHeader: "0.0185",
	}, blindInput, blindOutput, blindCacheWr, blindCacheRd)

	got := Settle(pctx, nil) // no rates wired at all

	if got.HeaderOmittedCache {
		t.Error("guard fired with no modelled figure to fall back to")
	}
	if !got.Priced || got.CostUSD != 0.0185 || got.Source != event.SourceGatewayHeader {
		t.Errorf("the gateway's figure should stand: priced=%v cost=%.6f src=%q",
			got.Priced, got.CostUSD, got.Source)
	}
}
