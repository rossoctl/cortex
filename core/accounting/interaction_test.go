package accounting

import (
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/cost/pricing"
)

// interactionResult is the accountant's exact interaction case (design §6.2, the library's
// TestARescuedRequestWithAContentChangeReportsTheInteraction): a ping kept a 1255-token system
// prompt warm, a mask cut messages[0] from 1214 tokens to 13, and the provider read 1255 and
// wrote 13. The rescue books 1255 write -> read, the mask 1201 at the read tier, and the
// interaction is the other 1201 write -> read, which no operator owns.
func interactionResult() (accountant.Result, accountant.Usage) {
	res := accountant.Result{
		Ops: []accountant.PricedOp{{Kind: accountant.KindRewrite, Operator: "context-guru.mask", OpID: "op1",
			ChangeID: "mask:1", Saved: accountant.TierTokens{CacheRead: 1201}}},
		CacheEffects: []accountant.CacheEffect{{ID: "ce:1", Kind: accountant.EffectRescue, ChangeID: "ka:main:1",
			Saved: accountant.TierTokens{CacheWrite5m: 1255}, Caused: accountant.TierTokens{CacheRead: 1255}}},
		Interaction:       accountant.TierTokens{CacheWrite5m: 1201},
		InteractionCaused: accountant.TierTokens{CacheRead: 1201},
	}
	return res, accountant.Usage{CacheRead: 1255, CacheWrite: 13}
}

// figuredSavings is Host.Savings' path after Compute: accountant.Figure at the endpoint, then
// savingsBilled with its dollars.
func figuredSavings(t *testing.T, r pricing.Resolver, res accountant.Result, u accountant.Usage, mode accountant.BillingMode) Extras {
	t.Helper()
	const model, host = "claude-sonnet-4-5", "api.anthropic.com"
	total := u.Input + u.CacheRead + u.CacheWrite
	fig := accountant.Figure(accountant.FigureInput{Model: model, Billed: u, Result: res}, sizedRates{r: r, endpoint: host})
	price := func(m, at string, tt accountant.TierTokens) (float64, bool) { return TierUSD(r, at, m, total, tt) }
	_, ex := savingsBilled(res, newFigured(fig), price, nil, model, host, mode, config.DefaultSubscriptionWeights())
	return ex
}

// interactionMetric is llm.savings.interaction.usd, saved minus added, for res observed once.
func interactionMetric(t *testing.T, r pricing.Resolver, res accountant.Result, u accountant.Usage, mode accountant.BillingMode) float64 {
	t.Helper()
	mp, rd := manual(t)
	s := NewSink(mp, r)
	meta := accountant.RequestMeta{RequestID: "r1", Session: "s1", Model: "claude-sonnet-4-5", At: time.Now(),
		Billing: accountant.Billing{Mode: mode}}
	s.Observe(meta, u, 0, res, "api.anthropic.com", u.Input+u.CacheRead+u.CacheWrite, false)
	return sum(t, rd, "llm.savings.interaction.usd", map[string]string{"direction": "saved"}) -
		sum(t, rd, "llm.savings.interaction.usd", map[string]string{"direction": "added"})
}

// On a rescued request, the savings record books the interaction the metrics book, to the
// micro, and puts it in no operator's saving (rossoctl/cortex#2139: the record dropped it).
func TestSavings_TheInteractionIsBookedAsTheMetricsBookIt(t *testing.T) {
	rates := testRates(pricing.ProvConfigured)
	res, u := interactionResult()
	ex := figuredSavings(t, rates, res, u, accountant.BillingAPI)
	want := interactionMetric(t, rates, res, u, accountant.BillingAPI)
	if want <= 0 {
		t.Fatalf("setup: the metrics booked interaction %.9f, want a saving", want)
	}
	if !micro(ex.InteractionUSD, want) {
		t.Errorf("Extras.InteractionUSD = %.9f, metrics interaction.usd = %.9f: they must agree", ex.InteractionUSD, want)
	}

	// No operator owns it: the savings are those of the same request with no interaction.
	bare := res
	bare.Interaction, bare.InteractionCaused = accountant.TierTokens{}, accountant.TierTokens{}
	const model, host = "claude-sonnet-4-5", "api.anthropic.com"
	price := func(m, at string, tt accountant.TierTokens) (float64, bool) { return TierUSD(rates, at, m, 1268, tt) }
	figOf := func(r accountant.Result) *figured {
		return newFigured(accountant.Figure(accountant.FigureInput{Model: model, Billed: u, Result: r}, sizedRates{r: rates, endpoint: host}))
	}
	with, _ := savingsBilled(res, figOf(res), price, nil, model, host, accountant.BillingAPI, config.DefaultSubscriptionWeights())
	without, _ := savingsBilled(bare, figOf(bare), price, nil, model, host, accountant.BillingAPI, config.DefaultSubscriptionWeights())
	if len(with) != len(without) {
		t.Fatalf("%d savings with the interaction, %d without", len(with), len(without))
	}
	for i := range with {
		if !micro(with[i].USD, without[i].USD) || !micro(with[i].PenaltyUSD, without[i].PenaltyUSD) {
			t.Errorf("saving %s: %.9f/%.9f with the interaction, %.9f/%.9f without: no operator owns it",
				with[i].Operator, with[i].USD, with[i].PenaltyUSD, without[i].USD, without[i].PenaltyUSD)
		}
	}
}

// On a subscription the metrics price no interaction, so the record books none either.
func TestSavings_NoInteractionOnASubscription(t *testing.T) {
	rates := testRates(pricing.ProvConfigured)
	res, u := interactionResult()
	if got := figuredSavings(t, rates, res, u, accountant.BillingSubscription).InteractionUSD; got != 0 {
		t.Errorf("subscription Extras.InteractionUSD = %.9f, want 0", got)
	}
	if got := interactionMetric(t, rates, res, u, accountant.BillingSubscription); got != 0 {
		t.Errorf("subscription interaction.usd = %.9f, want 0", got)
	}
}

// SavingsBilled, with no figure, prices the interaction at the one prompt size it is given, as
// it prices every other dollar.
func TestSavingsBilled_PricesTheInteractionAtTheBilledSize(t *testing.T) {
	rates := testRates(pricing.ProvConfigured)
	res, _ := interactionResult()
	const model, host, total = "claude-sonnet-4-5", "api.anthropic.com", 1268
	price := func(m, at string, tt accountant.TierTokens) (float64, bool) { return TierUSD(rates, at, m, total, tt) }
	_, ex := SavingsBilled(res, price, nil, model, host, accountant.BillingAPI, config.DefaultSubscriptionWeights())
	want, _ := price(model, host, res.Interaction.Sub(res.InteractionCaused))
	if want <= 0 || !micro(ex.InteractionUSD, want) {
		t.Errorf("Extras.InteractionUSD = %.9f, want %.9f", ex.InteractionUSD, want)
	}
}
