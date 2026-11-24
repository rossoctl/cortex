package settle

import (
	"math"
	"net/http"
	"testing"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/pipeline"
)

// tierMicros is what rates charges per token in each tier, in micro-dollars.
//
// DISTINCT PER TIER, and that is the entire point of the numbers. Charging 1e-6 in all four
// tiers would make every expectation built on this a function of the token TOTAL and of
// nothing else: 1,000 tokens priced as uncached input and 1,000
// priced as cache reads — a 10x error on the real card, and the exact mistake the
// four-way split exists to prevent — settled to the identical figure, so no test using
// this table could see a transposition anywhere between pricing.UsageFromInference and
// pricing.Cost. Coverage of that rested entirely on ONE other fixture (tieredTable, via
// the 26 input tokens in the halves test), where the whole margin for an
// input/cache-read swap is 140 micros in 5.1 million.
//
// FOUR PRIMES, no two of which sum to or divide a third, so no transposition can
// coincide. 1/2/4/8 would not do: a fixture carrying twice as many of the 4-tier's
// tokens as the 8-tier's prices the same either way round, and a swap of those two would
// pass. Ordered cache_read < input < cache_write < output so the SHAPE matches a real
// rate card and nothing here reads as an inverted one; the values themselves are
// synthetic and are nobody's price list.
//
// Pinned end to end by TestSettle_PricesEachTierAtItsOwnRate, which also asserts the
// four settled figures stay pairwise distinct — the property every other expectation in
// this package now depends on.
var tierMicros = map[pricing.Tier]float64{
	pricing.TierCacheRead:  3,
	pricing.TierInput:      7,
	pricing.TierCacheWrite: 11,
	pricing.TierOutput:     23,
}

// rates builds a table charging tierMicros per token, so a settled total says WHICH
// tiers the tokens were priced in and not merely how many there were. Expectations
// against it are written as explicit per-tier arithmetic — `(1000*7 + 500*23) / 1e6`
// rather than a bare 0.0185 — so the tier each count is charged at is visible at the
// assertion.
func rates(t *testing.T) pricing.Resolver {
	t.Helper()
	var r pricing.Rates
	for tier, perToken := range tierMicros {
		r.Base[tier], r.Set[tier] = perToken*1e-6, true
	}
	tab, err := pricing.NewTable([]pricing.Entry{{Host: "*", Model: "*", Rates: r, Prov: pricing.ProvConfigured}})
	if err != nil {
		t.Fatal(err)
	}
	return pricing.NewRegistry(tab)
}

// fallbackUSD is what a ctx(_, input, output) fixture settles to from the rates table:
// the input count at the INPUT tier's rate plus the output count at the OUTPUT tier's,
// which for the standard (1000, 500) fixture is 0.0185.
//
// Named per tier rather than written as a literal so the expectation says which rate each
// count is charged at. A bare 0.0185 asserts the arithmetic and not the ATTRIBUTION, and
// attribution is what a flat table could not express: under the old one-rate helper the
// same 0.0015 was correct however the two counts were shuffled between tiers.
func fallbackUSD(input, output int) float64 {
	return (float64(input)*tierMicros[pricing.TierInput] + float64(output)*tierMicros[pricing.TierOutput]) / 1e6
}

// TestModelledCost_Prices1hWrites: 600 of 1000 cache-write tokens were written
// with a 1-hour TTL. The 5m portion (400) prices at the ordinary cache-write
// rate; the 1h portion (600) prices at 2x input — Anthropic's documented 1h
// premium, which modelledCost applies because the model name is a Claude one.
func TestModelledCost_Prices1hWrites(t *testing.T) {
	const (
		inputRate = 3.80e-6 // Sonnet rates, USD/token
		cw5mRate  = 4.75e-6
		cw1hRate  = inputRate * 2.0 // Anthropic's documented 1h premium
	)
	var r pricing.Rates
	r.Base[pricing.TierInput], r.Set[pricing.TierInput] = inputRate, true
	r.Base[pricing.TierCacheWrite], r.Set[pricing.TierCacheWrite] = cw5mRate, true
	tab, err := pricing.NewTable([]pricing.Entry{{Host: "*", Model: "*", Rates: r, Prov: pricing.ProvConfigured}})
	if err != nil {
		t.Fatal(err)
	}
	resolver := pricing.NewRegistry(tab)

	u := pricing.Usage{CacheWrite: 1000, CacheWrite1h: 600}
	micros, tiers, prov, ok, refusal, estimated1h := modelledCost(resolver, "gw.internal", "claude-sonnet-4-5", u, u.PromptTotal())
	if !ok {
		t.Fatalf("modelledCost refused: %v", refusal)
	}
	if prov != pricing.ProvConfigured {
		t.Errorf("provenance = %v, want ProvConfigured", prov)
	}
	if estimated1h {
		t.Error("estimated1h = true, want false: a Claude model derives the 1h rate from Anthropic's documented 2x-input premium, not a guess")
	}

	wantUSD := 400*cw5mRate + 600*cw1hRate
	wantMicros, ok := pricing.MicrosFromUSD(wantUSD)
	if !ok {
		t.Fatal("test fixture produced an unrepresentable USD figure")
	}
	if micros != wantMicros {
		t.Errorf("micros = %d, want %d (400*cw5m + 600*cw1h = %v USD)", micros, wantMicros, wantUSD)
	}
	if got := tiers[pricing.TierCacheWrite]; got != wantMicros {
		t.Errorf("tiers[TierCacheWrite] = %d, want %d — the whole write cost should land on this tier", got, wantMicros)
	}
}

// onehourRates builds the same Sonnet-like table TestModelledCost_Prices1hWrites
// uses, so the 1h tests below share one fixture.
func onehourRates(t *testing.T) (pricing.Resolver, float64, float64, float64) {
	t.Helper()
	const (
		inputRate  = 3.80e-6
		cw5mRate   = 4.75e-6
		outputRate = 19.00e-6
	)
	var r pricing.Rates
	r.Base[pricing.TierInput], r.Set[pricing.TierInput] = inputRate, true
	r.Base[pricing.TierCacheWrite], r.Set[pricing.TierCacheWrite] = cw5mRate, true
	r.Base[pricing.TierOutput], r.Set[pricing.TierOutput] = outputRate, true
	tab, err := pricing.NewTable([]pricing.Entry{{Host: "*", Model: "*", Rates: r, Prov: pricing.ProvConfigured}})
	if err != nil {
		t.Fatal(err)
	}
	return pricing.NewRegistry(tab), inputRate, cw5mRate, outputRate
}

// TestSettle_OutputHalfExcludesCacheWrite1hPremium: the output half carries no
// cache-write tokens, so it must carry no 1h premium either. If outputOnly zeroed
// CacheWrite but left CacheWrite1h set, the premium would land on the output half.
// Asserted through SettleInput, the path a real response takes.
func TestSettle_OutputHalfExcludesCacheWrite1hPremium(t *testing.T) {
	resolver, inputRate, cw5mRate, outputRate := onehourRates(t)
	cw1hRate := inputRate * 2.0 // Anthropic's documented 1h premium

	inf := &pipeline.InferenceExtension{
		Model:              "claude-sonnet-4-5",
		CacheWriteTokens:   1000,
		CacheWrite1hTokens: 600,
		OutputTokens:       100,
		FinishReason:       "end_turn", // keeps IncompleteReason from firing for an unrelated cause
	}
	settled := SettleInput(Input{Host: "gw.internal", Inference: inf}, resolver)
	if !settled.HasModelled || !settled.HasPrompt || !settled.HasOutput {
		t.Fatalf("settled = %+v, want HasModelled/HasPrompt/HasOutput all true", settled)
	}

	wantOutputMicros, ok := pricing.MicrosFromUSD(100 * outputRate)
	if !ok {
		t.Fatal("test fixture produced an unrepresentable USD figure")
	}
	gotOutputMicros, ok := pricing.MicrosFromUSD(settled.OutputUSD)
	if !ok {
		t.Fatal("OutputUSD did not round-trip through MicrosFromUSD")
	}
	if gotOutputMicros != wantOutputMicros {
		t.Errorf("OutputUSD = %d micros, want %d (100 output tokens x the output rate, no 1h premium)",
			gotOutputMicros, wantOutputMicros)
	}

	wantWholeMicros, ok := pricing.MicrosFromUSD(400*cw5mRate + 600*cw1hRate + 100*outputRate)
	if !ok {
		t.Fatal("test fixture produced an unrepresentable USD figure")
	}
	promptMicros, _ := pricing.MicrosFromUSD(settled.PromptUSD)
	outputMicros, _ := pricing.MicrosFromUSD(settled.OutputUSD)
	if got := promptMicros + outputMicros; got != wantWholeMicros {
		t.Errorf("PromptUSD + OutputUSD = %d micros, want %d (the whole modelled cost)", got, wantWholeMicros)
	}
	wholeMicros, _ := pricing.MicrosFromUSD(settled.ModelledUSD)
	if wholeMicros != wantWholeMicros {
		t.Errorf("ModelledUSD = %d micros, want %d", wholeMicros, wantWholeMicros)
	}
}

// TestSettle_CacheWrite1hEstimatedWhenFallbackRateUsed covers the rule stated on
// settle.go's estimated1h comment: price at the 5m rate and mark the figure
// estimated when the 1h rate cannot be derived from a documented premium — true
// for a non-Anthropic model here, and false for an Anthropic model's
// documented-premium derivation.
func TestSettle_CacheWrite1hEstimatedWhenFallbackRateUsed(t *testing.T) {
	resolver, _, _, _ := onehourRates(t)

	for _, tc := range []struct {
		name          string
		model         string
		wantEstimated bool
	}{
		{"non-Anthropic model falls back to the 5m rate, estimated", "gpt-5", true},
		{"Anthropic model derives the documented 2x-input premium, not estimated", "claude-sonnet-4-5", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inf := &pipeline.InferenceExtension{
				Model:              tc.model,
				CacheWriteTokens:   1000,
				CacheWrite1hTokens: 600,
				OutputTokens:       100,
				FinishReason:       "end_turn",
			}
			settled := SettleInput(Input{Host: "gw.internal", Inference: inf}, resolver)
			if !settled.HasModelled {
				t.Fatal("settled.HasModelled = false, want true")
			}
			if settled.ModelledIncomplete != tc.wantEstimated {
				t.Errorf("ModelledIncomplete = %v, want %v", settled.ModelledIncomplete, tc.wantEstimated)
			}
			if tc.wantEstimated {
				if settled.ModelledIncompleteReason != pricing.ReasonCacheWrite1hRateEstimated {
					t.Errorf("ModelledIncompleteReason = %q, want %q", settled.ModelledIncompleteReason, pricing.ReasonCacheWrite1hRateEstimated)
				}
				// The modelled figure is also what was CHARGED here (no gateway header, no
				// declared-free), so Incomplete/IncompleteReason mirror it.
				if !settled.Incomplete || settled.IncompleteReason != pricing.ReasonCacheWrite1hRateEstimated {
					t.Errorf("Incomplete/IncompleteReason = %v/%q, want true/%q",
						settled.Incomplete, settled.IncompleteReason, pricing.ReasonCacheWrite1hRateEstimated)
				}
			} else if settled.ModelledIncompleteReason != "" {
				t.Errorf("ModelledIncompleteReason = %q, want empty", settled.ModelledIncompleteReason)
			}
		})
	}
}

// TestSettle_AnOversized1hSubsetIsDroppedNotRefused: a breakdown claiming more 1h
// writes than writes is not real, but it is an optional breakdown. The request
// prices as if it had no 1h writes, and the input and output dollars are kept.
func TestSettle_AnOversized1hSubsetIsDroppedNotRefused(t *testing.T) {
	resolver, inputRate, cw5mRate, outputRate := onehourRates(t)
	inf := &pipeline.InferenceExtension{
		Model:              "claude-sonnet-4-5",
		InputTokens:        5000,
		CacheWriteTokens:   100,
		CacheWrite1hTokens: 200,
		OutputTokens:       1000,
		FinishReason:       "end_turn",
	}
	settled := SettleInput(Input{Host: "gw.internal", Inference: inf}, resolver)
	if !settled.HasModelled || settled.RejectedReason != "" {
		t.Fatalf("HasModelled=%v RejectedReason=%q, want a priced figure and no refusal",
			settled.HasModelled, settled.RejectedReason)
	}
	want, ok := pricing.MicrosFromUSD(5000*inputRate + 100*cw5mRate + 1000*outputRate)
	if !ok {
		t.Fatal("test fixture produced an unrepresentable USD figure")
	}
	if got, _ := pricing.MicrosFromUSD(settled.ModelledUSD); got != want {
		t.Errorf("ModelledUSD = %d micros, want %d (every cache write at the 5m rate)", got, want)
	}
}

func ctx(headers map[string]string, input, output int) *pipeline.Context {
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	return &pipeline.Context{
		Host:            "gw.internal",
		ResponseHeaders: h,
		Extensions: pipeline.Extensions{Inference: &pipeline.InferenceExtension{
			Model:        "claude-opus-5",
			InputTokens:  input,
			OutputTokens: output,
		}},
	}
}

// The precedence rule, which is the reason this package exists: implemented twice, in two
// shapes, the two can disagree about the same request.
func TestSettle_Precedence(t *testing.T) {
	const json = "application/json"
	for _, tc := range []struct {
		name     string
		headers  map[string]string
		wantCost float64
		wantSrc  string
		wantProv pricing.Provenance
		priced   bool
		free     bool
	}{{
		name:     "gateway figure wins outright",
		headers:  map[string]string{"Content-Type": json, ResponseCostHeader: "0.25"},
		wantCost: 0.25,
		wantSrc:  event.SourceGatewayHeader,
		wantProv: pricing.ProvAuthoritative,
		priced:   true,
	}, {
		name:     "the -original fallback is the charged cost, not a list price",
		headers:  map[string]string{"Content-Type": json, ResponseCostOriginalHeader: "0.25"},
		wantCost: 0.25,
		wantSrc:  event.SourceGatewayHeader,
		wantProv: pricing.ProvAuthoritative,
		priced:   true,
	}, {
		// 1,000 input tokens at the input rate plus 500 output ones at the output rate.
		name:     "no header at all falls back to the table",
		headers:  map[string]string{"Content-Type": json},
		wantCost: fallbackUSD(1000, 500),
		wantSrc:  event.SourceUsageFallback,
		wantProv: pricing.ProvConfigured,
		priced:   true,
	}, {
		// The zero LiteLLM stamps on every stream is a placeholder, not an answer.
		name:     "a stream's zero falls back like an absent header",
		headers:  map[string]string{"Content-Type": "text/event-stream", ResponseCostHeader: "0"},
		wantCost: fallbackUSD(1000, 500),
		wantSrc:  event.SourceUsageFallback,
		wantProv: pricing.ProvConfigured,
		priced:   true,
	}, {
		// A zero on a NON-streamed response is the gateway saying "free". Pricing it
		// from the usage block would bill a cache hit.
		name:     "a declared zero is an answer, and suppresses the fallback",
		headers:  map[string]string{"Content-Type": json, ResponseCostHeader: "0"},
		wantCost: 0,
		wantSrc:  event.SourceGatewayHeader,
		wantProv: pricing.ProvAuthoritative,
		priced:   true,
		free:     true,
	}, {
		// Garbage is not a figure and not a declaration of free either.
		name:     "an unusable header falls back to the table",
		headers:  map[string]string{"Content-Type": json, ResponseCostHeader: "abc"},
		wantCost: fallbackUSD(1000, 500),
		wantSrc:  event.SourceUsageFallback,
		wantProv: pricing.ProvConfigured,
		priced:   true,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := Settle(ctx(tc.headers, 1000, 500), rates(t))
			if got.Priced != tc.priced {
				t.Fatalf("Priced = %v, want %v", got.Priced, tc.priced)
			}
			if got.CostUSD != tc.wantCost {
				t.Errorf("CostUSD = %v, want %v", got.CostUSD, tc.wantCost)
			}
			if got.Source != tc.wantSrc {
				t.Errorf("Source = %q, want %q", got.Source, tc.wantSrc)
			}
			if got.Provenance != tc.wantProv {
				t.Errorf("Provenance = %v, want %v", got.Provenance, tc.wantProv)
			}
			if got.DeclaredFree != tc.free {
				t.Errorf("DeclaredFree = %v, want %v", got.DeclaredFree, tc.free)
			}
		})
	}
}

// Both figures are carried even when one wins, because the drift check compares the pair
// that was actually decided rather than recomputing a half of it.
func TestSettle_CarriesBothFigures(t *testing.T) {
	got := Settle(ctx(map[string]string{"Content-Type": "application/json", ResponseCostHeader: "0.25"}, 1000, 500), rates(t))
	if !got.HasReported || got.ReportedUSD != 0.25 {
		t.Errorf("reported = %v (has=%v), want 0.25", got.ReportedUSD, got.HasReported)
	}
	if want := fallbackUSD(1000, 500); !got.HasModelled || got.ModelledUSD != want {
		t.Errorf("modelled = %v (has=%v), want %v", got.ModelledUSD, got.HasModelled, want)
	}
	if got.ModelledProv != pricing.ProvConfigured {
		t.Errorf("ModelledProv = %v, want configured", got.ModelledProv)
	}
}

// A TIER MIX-UP is the error the four-way split exists to prevent, and it is the one
// arithmetic mistake in this package that costs real money silently: a cache read billed
// as uncached input is a 10x overcharge on the real card, and for a long-running agent —
// whose traffic is overwhelmingly cache reads — that is most of the bill.
//
// Pinned directly rather than incidentally. One fixture per tier, the same 1,000 tokens in
// each, each expected at its own rate: a transposition anywhere between
// InferenceExtension's field names and pricing.Cost's rate lookup moves two of the four
// figures. Before the rates helper carried distinct per-tier values, every one of these
// four settled to the identical number and the only thing in the package that could see a
// swap was 26 input tokens inside another fixture's 641k-token prompt.
//
// The pairwise-distinctness check at the end is a guard on the FIXTURE, not on Settle: it
// is the property that makes every other expectation here tier-sensitive, so a future
// edit that quietly gives two tiers the same rate has to fail something.
func TestSettle_PricesEachTierAtItsOwnRate(t *testing.T) {
	const n = 1000
	settled := make(map[pricing.Tier]float64, len(tierMicros))
	for _, tc := range []struct {
		tier pricing.Tier
		inf  pipeline.InferenceExtension
	}{
		{pricing.TierInput, pipeline.InferenceExtension{InputTokens: n}},
		{pricing.TierCacheWrite, pipeline.InferenceExtension{CacheWriteTokens: n}},
		{pricing.TierCacheRead, pipeline.InferenceExtension{CacheReadTokens: n}},
		{pricing.TierOutput, pipeline.InferenceExtension{OutputTokens: n}},
	} {
		t.Run(tc.tier.String(), func(t *testing.T) {
			inf := tc.inf
			inf.Model = "claude-opus-5"
			h := http.Header{}
			// No cost header: the modelled figure has to be the one that wins, or this
			// asserts a gateway's number instead of the table's tiers.
			h.Set("Content-Type", "application/json")
			got := Settle(&pipeline.Context{
				Host:            "gw.internal",
				ResponseHeaders: h,
				Extensions:      pipeline.Extensions{Inference: &inf},
			}, rates(t))
			if !got.Priced || got.Source != event.SourceUsageFallback {
				t.Fatalf("not priced from the table: %+v", got)
			}
			if want := float64(n) * tierMicros[tc.tier] / 1e6; got.CostUSD != want {
				t.Errorf("CostUSD = %v, want %v — %d %s tokens must charge the %s rate (%v/token), and no other tier's",
					got.CostUSD, want, n, tc.tier, tc.tier, tierMicros[tc.tier]*1e-6)
			}
			settled[tc.tier] = got.CostUSD
		})
	}

	byCost := make(map[float64]pricing.Tier, len(settled))
	for tier, usd := range settled {
		if other, dup := byCost[usd]; dup {
			t.Errorf("%s and %s both settle %d tokens at %v; two tiers charging one rate makes every expectation in this package blind to a swap between them",
				tier, other, n, usd)
		}
		byCost[usd] = tier
	}
}

// tieredTable has a distinct rate per tier AND a long-context threshold at 200k that
// raises every tier, output included — the shape bundled.go gives the Sonnet 4.5 family
// (there, 1.5e-05 output becomes 2.25e-05 above 200k).
//
// Both properties earn their place. The flat one-micro-per-token `rates` table cannot catch
// an output half priced at the input rate; a table with no threshold cannot catch a figure
// that resolved its rates from its own token counts rather than the request's, which is
// exactly how the premium went missing from the halves and from savings. Rates are chosen
// so every expectation lands on a whole number of micros, because Cost rounds to micros and
// a fractional expectation would assert the rounding rather than the pricing.
func tieredTable(t *testing.T) pricing.Resolver {
	t.Helper()
	var r pricing.Rates
	for _, tr := range []struct {
		tier pricing.Tier
		per  float64
	}{
		{pricing.TierInput, 5e-6}, {pricing.TierCacheWrite, 6.25e-6},
		{pricing.TierCacheRead, 0.5e-6}, {pricing.TierOutput, 25e-6},
	} {
		r.Base[tr.tier], r.Set[tr.tier] = tr.per, true
	}
	var above pricing.ContextThreshold
	above.AbovePromptTokens = 200_000
	for _, tr := range []struct {
		tier pricing.Tier
		per  float64
	}{
		{pricing.TierInput, 6e-6}, {pricing.TierCacheWrite, 8e-6},
		{pricing.TierCacheRead, 0.6e-6}, {pricing.TierOutput, 30e-6},
	} {
		above.Rate[tr.tier], above.Set[tr.tier] = tr.per, true
	}
	r.Thresholds = []pricing.ContextThreshold{above}
	tab, err := pricing.NewTable([]pricing.Entry{{Host: "*", Model: "*", Rates: r, Prov: pricing.ProvConfigured}})
	if err != nil {
		t.Fatal(err)
	}
	return pricing.NewRegistry(tab)
}

// Each half is priced on its own tiers, so a per-row consumer can render a row-local
// figure instead of a running total — and both resolve their rates at the REQUEST's prompt
// size, so a long-context premium reaches the completion as well as the prompt.
//
// The usage is the turn that motivated the split: a long-running agent's cold cache write,
// where the prompt is two orders of magnitude more expensive than the completion and a
// cumulative cell hides that.
func TestSettle_PricesPromptAndOutputHalvesSeparately(t *testing.T) {
	reg := tieredTable(t)
	pctx := &pipeline.Context{
		Host:            "gw.internal",
		ResponseHeaders: http.Header{},
		Extensions: pipeline.Extensions{Inference: &pipeline.InferenceExtension{
			Model: "claude-opus-5", InputTokens: 26,
			CacheWriteTokens: 640_985, OutputTokens: 1_075,
		}},
	}
	got := Settle(pctx, reg)

	// 26x6 + 640,985x8 micros — the ABOVE-threshold input and cache-write rates, since
	// 641,011 prompt tokens are past 200k. Nothing of the output: at the output rate those
	// 1,075 tokens would add 32,250 micros, which is what a prompt half must not contain.
	if wantPrompt := (26*6 + 640_985*8) / 1e6; !got.HasPrompt || got.PromptUSD != wantPrompt {
		t.Errorf("PromptUSD = %v (has=%v), want %v", got.PromptUSD, got.HasPrompt, wantPrompt)
	}
	// 1,075x30 micros: the OUTPUT tier, at the ABOVE-threshold rate. Three ways to get
	// this wrong, all of which this pins:
	//   - the base output rate (25) = 26,875 micros, which is what resolving the
	//     threshold from the output half's own token counts produces, because a half with
	//     no prompt tokens lands at At(0)
	//   - the input rate (6) = 6,450 micros
	//   - the cache-write rate the prompt landed in (8) = 8,600 micros
	if wantOutput := (1_075 * 30) / 1e6; !got.HasOutput || got.OutputUSD != wantOutput {
		t.Errorf("OutputUSD = %v (has=%v), want %v", got.OutputUSD, got.HasOutput, wantOutput)
	}
	// The halves partition the modelled total: neither double-counts a tier, nothing falls
	// between them, and — the reason this assertion has teeth now — both resolved their
	// rates at the same prompt size, so the premium cannot land on one and miss the other.
	// Tolerance is one micro: each figure rounds to micros independently, so a partition
	// can legitimately differ from the whole in the last place.
	if sum := got.PromptUSD + got.OutputUSD; !got.HasModelled || math.Abs(sum-got.ModelledUSD) > 1e-6 {
		t.Errorf("halves sum to %v, want the modelled total %v (has=%v)", sum, got.ModelledUSD, got.HasModelled)
	}

	// With the gateway's own total winning, both halves stay the TABLE's. A consumer
	// showing a row-local figure must never end up with a share of the header, and the
	// difference proves why: the gateway charged 3.0652 against a modelled 5.160286, so
	// total-minus-prompt would report the completion as a negative cost. (Subtracting
	// ModelledUSD − PromptUSD would be sound — same table, same prompt size; it is mixing
	// the gateway's total with the table's half that cannot work.)
	pctx.ResponseHeaders.Set("Content-Type", "application/json")
	pctx.ResponseHeaders.Set(ResponseCostHeader, "3.0652")
	withHeader := Settle(pctx, reg)
	if withHeader.Source != event.SourceGatewayHeader {
		t.Fatalf("Source = %q, want the header to win", withHeader.Source)
	}
	if withHeader.PromptUSD != got.PromptUSD || withHeader.OutputUSD != got.OutputUSD {
		t.Errorf("halves moved with the header: prompt %v output %v, want %v and %v",
			withHeader.PromptUSD, withHeader.OutputUSD, got.PromptUSD, got.OutputUSD)
	}
	if diff := withHeader.CostUSD - withHeader.PromptUSD; diff >= 0 {
		t.Errorf("total-minus-prompt is %v; this fixture exists because that is negative", diff)
	}
}

// A binary with no pricing wired must report unpriced, not panic. The trap is that the
// resolver is an INTERFACE: a nil one panics on call where a nil *Registry would not.
func TestSettle_NilResolverIsUnpriced(t *testing.T) {
	got := Settle(ctx(map[string]string{"Content-Type": "application/json"}, 1000, 500), nil)
	if got.Priced {
		t.Errorf("priced with no rate table: %+v", got)
	}
	if got.HasModelled {
		t.Error("claims a modelled figure with no resolver")
	}
	// A gateway figure still lands, because reading a header needs no rate table.
	withHeader := Settle(ctx(map[string]string{"Content-Type": "application/json", ResponseCostHeader: "0.25"}, 0, 0), nil)
	if !withHeader.Priced || withHeader.CostUSD != 0.25 {
		t.Errorf("gateway figure lost when no rates are configured: %+v", withHeader)
	}
}

// No inference at all is not a pricing gap: a plain proxied request has no model and no
// tokens, and inventing a figure for it would be worse than reporting none.
func TestSettle_NoInferenceIsSilent(t *testing.T) {
	pctx := &pipeline.Context{Host: "gw.internal", ResponseHeaders: http.Header{}}
	if got := Settle(pctx, rates(t)); got.Priced {
		t.Errorf("priced a request with no inference: %+v", got)
	}
	if got := Settle(nil, rates(t)); got.Priced {
		t.Error("priced a nil context")
	}
}

// Comparable is what keeps drift from blaming the rate table for a gateway's own
// discount layer: when the effective header is absent and the gateway reports an
// adjustment, the fallback figure is pre-adjustment and the two are not like for like.
func TestComparable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{"effective header present", map[string]string{ResponseCostHeader: "0.25"}, true},
		{"fallback with no adjustment", map[string]string{ResponseCostOriginalHeader: "0.25", DiscountAmountHeader: "0.0"}, true},
		{"fallback with a discount", map[string]string{ResponseCostOriginalHeader: "0.25", DiscountAmountHeader: "0.05"}, false},
		{"fallback with a margin", map[string]string{ResponseCostOriginalHeader: "0.25", MarginAmountHeader: "0.05"}, false},
		{"no headers", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Comparable(ctx(tc.headers, 0, 0)); got != tc.want {
				t.Errorf("Comparable = %v, want %v", got, tc.want)
			}
		})
	}
}

// Publish writes both keys: agentop is a separate binary that can lag the proxy, and an
// older one reads only the legacy key.
func TestPublish_WritesBothKeys(t *testing.T) {
	pctx := ctx(nil, 0, 0)
	Publish(pctx, event.Event{CostUSD: 0.25, Settled: true})
	for _, key := range []string{event.Key, event.PluginName} {
		if _, ok := pctx.Extensions.Custom[key+pipeline.PluginEventSuffix]; !ok {
			t.Errorf("nothing published under %q", key)
		}
	}
}

// Amend is how a budget adds the fields it owns without becoming the record's author.
func TestAmend(t *testing.T) {
	pctx := ctx(nil, 0, 0)
	if Amend(pctx, func(*event.Event) { t.Error("amended a record that was never published") }) {
		t.Error("Amend reported success with nothing published")
	}

	Publish(pctx, event.Event{CostUSD: 0.25, Settled: true})
	if !Amend(pctx, func(ev *event.Event) { ev.DailyTotalUSD = 9 }) {
		t.Fatal("Amend failed on a published record")
	}
	// Both keys must carry the amendment, or an older agentop shows a stale total.
	for _, key := range []string{event.Key, event.PluginName} {
		ev, ok := pctx.Extensions.Custom[key+pipeline.PluginEventSuffix].(event.Event)
		if !ok {
			t.Fatalf("%q missing after amend", key)
		}
		if ev.DailyTotalUSD != 9 || ev.CostUSD != 0.25 {
			t.Errorf("%q = %+v, want the amendment applied and the cost preserved", key, ev)
		}
	}
}

// Store/Load hand the full outcome to later plugins in the same request without widening
// the wire shape.
func TestStoreLoad(t *testing.T) {
	pctx := ctx(nil, 0, 0)
	if _, ok := Load(pctx); ok {
		t.Error("loaded an outcome that was never stored")
	}
	Store(pctx, Settled{CostUSD: 0.25, Priced: true, ModelledUSD: 0.3, HasModelled: true})
	got, ok := Load(pctx)
	if !ok {
		t.Fatal("stored outcome not found")
	}
	if got.CostUSD != 0.25 || got.ModelledUSD != 0.3 {
		t.Errorf("round trip lost data: %+v", got)
	}
}

// TestSettle_CacheWrite1hPremiumHeldToTheCeiling: the base figure passes CostByTier's
// ceiling, and the 1h premium takes the total above it. The whole request, its halves
// and its tiers must be refused and the refusal disclosed, as for any other figure past
// the ceiling. A premium that keeps the total under the ceiling still prices.
func TestSettle_CacheWrite1hPremiumHeldToTheCeiling(t *testing.T) {
	resolver := func(inputRate, cwRate float64) pricing.Resolver {
		var r pricing.Rates
		r.Base[pricing.TierInput], r.Set[pricing.TierInput] = inputRate, true
		r.Base[pricing.TierCacheWrite], r.Set[pricing.TierCacheWrite] = cwRate, true
		tab, err := pricing.NewTable([]pricing.Entry{{Host: "*", Model: "*", Rates: r, Prov: pricing.ProvConfigured}})
		if err != nil {
			t.Fatal(err)
		}
		return pricing.NewRegistry(tab)
	}
	inf := &pipeline.InferenceExtension{
		Model:              "claude-sonnet-4-5",
		CacheWriteTokens:   1000,
		CacheWrite1hTokens: 1000,
		FinishReason:       "end_turn",
	}

	// Base 1000 x $7.50 = $7,500 passes; the 1h rate is 2 x $6, so the premium is
	// 1000 x $4.50 = $4,500 and the total $12,000 is above the $10,000 ceiling.
	over := SettleInput(Input{Host: "gw.internal", Inference: inf}, resolver(6, 7.5))
	if over.HasModelled || over.HasPrompt || over.HasOutput || over.HasTiers || over.Priced {
		t.Errorf("over the ceiling: settled = %+v, want no modelled figure, no halves, no tiers, unpriced", over)
	}
	if over.RejectedReason != event.RejectedImplausible {
		t.Errorf("over the ceiling: RejectedReason = %q, want %q", over.RejectedReason, event.RejectedImplausible)
	}

	// The same shape at a hundredth of the rates: $120 total, priced.
	under := SettleInput(Input{Host: "gw.internal", Inference: inf}, resolver(0.06, 0.075))
	if !under.HasModelled || !under.Priced || under.RejectedReason != "" {
		t.Fatalf("under the ceiling: settled = %+v, want priced", under)
	}
	if got := math.Round(under.ModelledUSD); got != 120 {
		t.Errorf("under the ceiling: ModelledUSD = %v, want 120", under.ModelledUSD)
	}
}
