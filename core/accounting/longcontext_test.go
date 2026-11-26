package accounting

import (
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/pipeline"
)

// longContextRates doubles every rate when the prompt is above 200k tokens, the way Anthropic
// bills a long-context request: the whole request at the higher rate.
type longContextRates struct{ base, long pricing.Rates }

func (l *longContextRates) Resolve(_, _ string, promptTotal int) (pricing.Rates, pricing.Provenance) {
	if promptTotal > 200_000 {
		return l.long, pricing.ProvConfigured
	}
	return l.base, pricing.ProvConfigured
}
func (l *longContextRates) CurrencyFor(string, string) string { return "USD" }

func newLongContextRates() *longContextRates {
	b := testRates(pricing.ProvConfigured).rates
	lr := b
	for i := range lr.Base {
		lr.Base[i] *= 2
	}
	return &longContextRates{base: b, long: lr}
}

// longContextRequest is a tool prune that takes a prompt of about 250k tokens to about 150k: the
// change crosses the 200k line.
func longContextRequest(t *testing.T, h *Host) (*pipeline.Context, pipeline.InferenceExtension) {
	t.Helper()
	const bpt = 4 // bytes per token in the bodies below
	big := `{"name":"Big","description":"` + strings.Repeat("x", 100_000*bpt) + `","input_schema":{"type":"object"}}`
	conv := `[{"role":"user","content":[{"type":"text","text":"hi` + strings.Repeat("lorem ipsum ", 150_000*bpt/12) +
		`","cache_control":{"type":"ephemeral"}}]}]`
	client := []byte(`{"model":"claude-sonnet-4-5","max_tokens":10,"tools":[` + big + `,` + smallTool + `],"messages":` + conv + `}`)
	fwd := []byte(`{"model":"claude-sonnet-4-5","max_tokens":10,"tools":[` + smallTool + `],"messages":` + conv + `}`)
	op := accountant.Operation{
		Kind: accountant.KindRewrite, At: accountant.Position{Section: accountant.SectionTools, Index: 0},
		SizeBefore: accountant.Size{Bytes: len(big)}, ChangeID: "test:big",
	}
	usage := pipeline.InferenceExtension{InputTokens: 3, CacheWriteTokens: len(fwd) / bpt, OutputTokens: 500}
	return request(t, h, "s1", "/v1/messages", client, &operator{fwd: fwd, ops: []accountant.Operation{op}}, 200, usage), usage
}

// A change that takes a prompt from above the long-context line to below it saves the whole bill
// difference: the removed tokens at the long rate, plus the premium every token that stays no
// longer pays, output included. The ledger and the metrics book the same dollars
// (rossoctl/cortex#2135: Cortex booked only the removed tokens at the base rate, 71% too low).
func TestSavings_CrossingTheLongContextLineSavesTheWholeBillDifference(t *testing.T) {
	rates := newLongContextRates()
	h := New(Options{Resolver: rates})
	t.Cleanup(func() { _ = h.Close() })
	pctx, usage := longContextRequest(t, h)

	res, ok := h.Compute(pctx)
	if !ok {
		t.Fatal("Compute refused")
	}
	var saved accountant.TierTokens
	for _, o := range res.Ops {
		saved = saved.Add(o.Saved)
	}
	after := usage.InputTokens + usage.CacheWriteTokens
	before := after + saved.Input + saved.CacheWrite5m + saved.CacheWrite1h + saved.CacheRead
	if before <= 200_000 || after > 200_000 {
		t.Fatalf("setup: the change does not cross the 200k line (before=%d after=%d)", before, after)
	}

	// The bill without the change at its own size's rates, minus the bill as sent.
	stay := accountant.TierTokens{Input: usage.InputTokens, CacheWrite5m: usage.CacheWriteTokens}
	promptBefore, _ := TierUSD(rates, "api.anthropic.com", "claude-sonnet-4-5", before, stay.Add(saved))
	promptAfter, _ := TierUSD(rates, "api.anthropic.com", "claude-sonnet-4-5", after, stay)
	outBefore, _ := USD(rates, "api.anthropic.com", "claude-sonnet-4-5", before, pricing.TierOutput, int64(usage.OutputTokens))
	outAfter, _ := USD(rates, "api.anthropic.com", "claude-sonnet-4-5", after, pricing.TierOutput, int64(usage.OutputTokens))
	want := promptBefore + outBefore - promptAfter - outAfter

	ss, ex, ok := h.Savings(pctx)
	if !ok || len(ss) != 1 {
		t.Fatalf("Savings = %+v, %v; want one saving", ss, ok)
	}
	if got := ss[0].USD - ss[0].PenaltyUSD; !micro(got, want) {
		t.Errorf("ledger saving = $%.6f, want the whole bill difference $%.6f", got, want)
	}
	if ex.UnattributedBreakUSD != 0 {
		t.Errorf("UnattributedBreakUSD = %v, want 0: the prune owns the whole re-rate", ex.UnattributedBreakUSD)
	}

	mp, rd := manual(t)
	s := NewSink(mp, rates)
	meta := accountant.RequestMeta{RequestID: "r1", Session: "s1", Model: "claude-sonnet-4-5", At: time.Now(), Billing: apiBilling}
	s.Observe(meta, accountant.Usage{Input: usage.InputTokens, CacheWrite: usage.CacheWriteTokens},
		usage.OutputTokens, res, "api.anthropic.com", after, false)
	pushed := sum(t, rd, "llm.savings.saved.usd", nil) - sum(t, rd, "llm.savings.added.usd", nil)
	if !micro(pushed, want) {
		t.Errorf("pushed saving = $%.6f, want the ledger's $%.6f", pushed, want)
	}
}

// An op that adds tokens and pushes the prompt above the line made the prompt larger, so it owns the
// re-rate (accountant.Figure): its penalty is its added tokens at the long rate, plus the premium the
// tokens that stay now pay. No part of it is a request-level cost.
func TestSavings_AnAddThatCrossesTheLinePaysThePremium(t *testing.T) {
	rates := newLongContextRates()
	res := accountant.Result{Ops: []accountant.PricedOp{{Kind: accountant.KindRewrite, Operator: "inject",
		Caused: accountant.TierTokens{Input: 60_000}}}}
	billed := accountant.Usage{Input: 210_000}
	fig := accountant.Figure(accountant.FigureInput{Model: "m", Billed: billed, Result: res},
		sizedRates{r: rates, endpoint: "h"})
	price := func(m, at string, tt accountant.TierTokens) (float64, bool) {
		return TierUSD(rates, at, m, billed.Input, tt)
	}
	ss, ex := savingsBilled(res, newFigured(fig), price, nil, "m", "h", accountant.BillingAPI, config.DefaultSubscriptionWeights())
	if len(ss) != 1 {
		t.Fatalf("savings = %+v, want one", ss)
	}
	if want := 60_000*6e-6 + 150_000*(6e-6-3e-6); !micro(ss[0].PenaltyUSD, want) || ss[0].USD != 0 {
		t.Errorf("inject USD=%v PenaltyUSD=%v, want 0 and %v (its tokens at the long rate, plus the premium of the 150k that stay)",
			ss[0].USD, ss[0].PenaltyUSD, want)
	}
	if ex.UnattributedBreakUSD != 0 {
		t.Errorf("UnattributedBreakUSD = %v, want 0: inject owns the re-rate", ex.UnattributedBreakUSD)
	}
}
