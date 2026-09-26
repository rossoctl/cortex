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

// A saving is a slice of THIS request's prompt, so it prices at the rates the request
// landed on — long-context premium included.
//
// Keying the threshold on the saving's own token count instead put a ~10k-token slice at
// At(10000), below a 200k threshold that the 641k-token request it came out of is well past,
// and under-reported every saving on exactly the long-context turns where the premium
// applies. Asserted as a RATE (dollars per avoided token) rather than a dollar total, so the
// byte-ratio token estimate can change without rewriting the expectation.
func TestAvoided_PricesSavingsAtTheRequestsPromptSize(t *testing.T) {
	pctx := &pipeline.Context{
		Host: "gw.internal",
		Extensions: pipeline.Extensions{
			Inference: &pipeline.InferenceExtension{
				Model: "claude-opus-5", InputTokens: 26,
				CacheWriteTokens: 640_985, OutputTokens: 1_075,
			},
			Custom: map[string]any{
				"tool-prune" + pipeline.PluginEventSuffix: map[string]any{
					"bytesRemoved": 34_645, "bodyBytesAfter": 2_384_550,
				},
			},
		},
	}
	got := Avoided(pctx, tieredTable(t))
	if len(got) != 1 {
		t.Fatalf("Avoided returned %d savings, want 1: %+v", len(got), got)
	}
	s := got[0]
	if s.TokensAvoided <= 0 || s.USD <= 0 {
		t.Fatalf("saving not priced: %+v", s)
	}
	// The prompt landed in the cache-write tier, which is 8e-06 above the threshold and
	// 6.25e-06 below it.
	if want := float64(s.TokensAvoided) * 8e-6; math.Abs(s.USD-want) > 1e-6 {
		t.Errorf("saving priced at %v/token (%v for %d tokens), want the above-threshold "+
			"8e-06 (%v); the below-threshold rate would give %v",
			s.USD/float64(s.TokensAvoided), s.USD, s.TokensAvoided, want,
			float64(s.TokensAvoided)*6.25e-6)
	}
	if s.Tier != pricing.TierCacheWrite.String() {
		t.Errorf("Tier = %q, want %q", s.Tier, pricing.TierCacheWrite.String())
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

// Publish writes both keys: abctl is a separate binary that can lag the proxy, and an
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
	// Both keys must carry the amendment, or an older abctl shows a stale total.
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
