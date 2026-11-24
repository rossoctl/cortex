package accounting

import (
	"context"
	"net/http"
	"testing"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/pipeline"
)

// pricer is a price function for Savings with round per-token rates: input 1, cache write 5m
// 1.25, cache write 1h 2, cache read 0.1, all in micro-dollars. A model named "half" costs half,
// so a test can see which model priced a figure; a model named "est" is estimated. calls records
// each (model, host) asked for.
type pricer struct {
	calls [][2]string
}

func (p *pricer) price(model, host string, tt accountant.TierTokens) (float64, bool) {
	p.calls = append(p.calls, [2]string{model, host})
	usd := (float64(tt.Input)*1 + float64(tt.CacheWrite5m)*1.25 + float64(tt.CacheWrite1h)*2 + float64(tt.CacheRead)*0.1) / 1e6
	if model == "half" {
		usd /= 2
	}
	return usd, model == "est"
}

func byOperator(t *testing.T, ss []event.Saving) map[string]event.Saving {
	t.Helper()
	out := map[string]event.Saving{}
	for _, s := range ss {
		if _, dup := out[s.Operator]; dup {
			t.Fatalf("two savings for operator %q: %+v", s.Operator, ss)
		}
		if s.Component != s.Operator {
			t.Errorf("Component %q != Operator %q: both are the dotted name (W7.1)", s.Component, s.Operator)
		}
		out[s.Operator] = s
	}
	return out
}

func TestSavings_TwoOperatorsSplit(t *testing.T) {
	res := accountant.Result{Version: 3, Ops: []accountant.PricedOp{
		{Kind: accountant.KindRewrite, Operator: "tool-prune", ChangeID: "tp:1", Saved: accountant.TierTokens{CacheRead: 1000}},
		{Kind: accountant.KindRewrite, Operator: "context-guru.mask", ChangeID: "m:1",
			Saved: accountant.TierTokens{Input: 100, CacheWrite5m: 40}, Caused: accountant.TierTokens{CacheWrite5m: 8}},
		{Kind: accountant.KindRewrite, Operator: "tool-prune", ChangeID: "tp:2", Saved: accountant.TierTokens{CacheRead: 500}},
	}}
	p := &pricer{}
	ss, ex := Savings(res, p.price, "claude-sonnet-4-5", "api.anthropic.com")
	got := byOperator(t, ss)
	if len(got) != 2 {
		t.Fatalf("savings = %+v, want one per operator", ss)
	}
	tp, m := got["tool-prune"], got["context-guru.mask"]
	if !near(tp.USD, 150e-6) || tp.PenaltyUSD != 0 || tp.Tier != "cache_read" || tp.TokensAvoided != 1500 {
		t.Errorf("tool-prune = %+v, want USD 150e-6, no penalty, tier cache_read, 1500 tokens", tp)
	}
	if !near(m.USD, 150e-6) || !near(m.PenaltyUSD, 10e-6) || m.Tier != "input" || m.TokensAvoided != 140 {
		t.Errorf("mask = %+v, want USD 150e-6, penalty 10e-6, tier input, 140 tokens", m)
	}
	for _, s := range ss {
		if s.OpsVersion != 3 || s.Basis != BasisUSD || s.Projected || s.Estimated || s.Units != 0 {
			t.Errorf("%s = %+v, want OpsVersion 3, basis usd, applied, not estimated, no units", s.Operator, s)
		}
	}
	if ex.OpsVersion != 3 || ex.UnattributedBreakUSD != 0 {
		t.Errorf("extras = %+v", ex)
	}
	for _, c := range p.calls {
		if c != [2]string{"claude-sonnet-4-5", "api.anthropic.com"} {
			t.Errorf("priced with %v, want the request's model and host", c)
		}
	}
}

// A break is one net cost (Caused priced minus Saved priced), charged to the operator whose change
// it carries; it never adds its Saved to USD. A break with no ChangeID, or with a ChangeID no
// applied op reported, is unattributed.
func TestSavings_Breaks(t *testing.T) {
	res := accountant.Result{
		Ops: []accountant.PricedOp{
			{Kind: accountant.KindRewrite, Operator: "context-guru.mask", ChangeID: "m:1", Saved: accountant.TierTokens{Input: 100}},
			{Kind: accountant.KindRewrite, Operator: "context-guru.dry", ChangeID: "d:1", DryRun: true, Saved: accountant.TierTokens{Input: 10}},
		},
		CacheEffects: []accountant.CacheEffect{
			{Kind: accountant.EffectBreak, ChangeID: "m:1", Saved: accountant.TierTokens{CacheRead: 1000}, Caused: accountant.TierTokens{CacheWrite5m: 1000}, Estimated: true},
			{Kind: accountant.EffectBreak, Saved: accountant.TierTokens{CacheRead: 200}, Caused: accountant.TierTokens{CacheWrite5m: 200}},
			{Kind: accountant.EffectBreak, ChangeID: "d:1", Saved: accountant.TierTokens{CacheRead: 100}, Caused: accountant.TierTokens{CacheWrite5m: 100}},
		},
	}
	p := &pricer{}
	ss, ex := Savings(res, p.price, "m", "h")
	got := byOperator(t, ss)
	m := got["context-guru.mask"]
	if !near(m.USD, 100e-6) {
		t.Errorf("mask USD = %v, want 100e-6: a break's Saved is never added to USD", m.USD)
	}
	if want := (1000*1.25 - 1000*0.1) / 1e6; !near(m.PenaltyUSD, want) {
		t.Errorf("mask PenaltyUSD = %v, want %v", m.PenaltyUSD, want)
	}
	if !m.Estimated {
		t.Error("mask not estimated, but its break was")
	}
	if d := got["context-guru.dry"]; d.PenaltyUSD != 0 || !d.Projected {
		t.Errorf("dry-run operator = %+v: a dry run caused no break, and it is projected", d)
	}
	if want := (200*1.15 + 100*1.15) / 1e6; !near(ex.UnattributedBreakUSD, want) {
		t.Errorf("UnattributedBreakUSD = %v, want %v", ex.UnattributedBreakUSD, want)
	}
}

func TestSavings_RescueGoesToTheKeeper(t *testing.T) {
	res := accountant.Result{
		Ops: []accountant.PricedOp{{Kind: accountant.KindRewrite, Operator: "tool-prune", ChangeID: "tp:1", Saved: accountant.TierTokens{CacheRead: 100}}},
		CacheEffects: []accountant.CacheEffect{{Kind: accountant.EffectRescue, ChangeID: "ka:main:2026-11-17T10:00:00Z",
			Saved: accountant.TierTokens{CacheWrite5m: 1000}, Caused: accountant.TierTokens{CacheRead: 1000}}},
	}
	ss, _ := Savings(res, (&pricer{}).price, "m", "h")
	got := byOperator(t, ss)
	k, ok := got[KeeperOperator]
	if !ok {
		t.Fatalf("no saving for the keeper: %+v", ss)
	}
	if want := (1000*1.25 - 1000*0.1) / 1e6; !near(k.USD, want) || k.PenaltyUSD != 0 || k.Tier != "cache_write" {
		t.Errorf("keeper = %+v, want USD %v, tier cache_write", k, want)
	}
	if tp := got["tool-prune"]; !near(tp.USD, 10e-6) {
		t.Errorf("tool-prune = %+v: the rescue must not be credited to it", tp)
	}
}

func TestSavings_DryRunOnlyOperatorIsProjected(t *testing.T) {
	res := accountant.Result{Ops: []accountant.PricedOp{
		{Kind: accountant.KindRewrite, Operator: "tool-prune", ChangeID: "a", DryRun: true, Saved: accountant.TierTokens{Input: 10}},
		{Kind: accountant.KindRewrite, Operator: "tool-prune", ChangeID: "b", DryRun: true, Saved: accountant.TierTokens{Input: 20}},
		{Kind: accountant.KindRewrite, Operator: "context-guru.mask", ChangeID: "d", Saved: accountant.TierTokens{Input: 5}},
	}}
	got := byOperator(t, func() []event.Saving { s, _ := Savings(res, (&pricer{}).price, "m", "h"); return s }())
	if !got["tool-prune"].Projected {
		t.Error("an operator whose every op is a dry run is not projected")
	}
	if got["context-guru.mask"].Projected {
		t.Error("an operator with only applied ops is projected")
	}
	if len(got) != 2 {
		t.Errorf("savings = %+v, want one per operator: a single-kind operator gets one saving", got)
	}
}

// A spend op (an operator's own model call) is not a saving (W7.1): it is one SpendRecord, priced
// with the call's model and host, and its operator gets no Saving for it. A stated CostUSD is a
// fact and wins over the rates.
func TestSavings_SpendOpIsASpendRecordNotASaving(t *testing.T) {
	res := accountant.Result{Ops: []accountant.PricedOp{
		{Kind: accountant.KindSpend, Operator: "context-guru.summarize", ChangeID: "s:1",
			Call: &accountant.ModelCall{Model: "half", Host: "other.example", Usage: accountant.CallUsage{Usage: accountant.Usage{Input: 1000}}}},
		{Kind: accountant.KindSpend, Operator: "context-guru.judge", ChangeID: "s:2",
			Call: &accountant.ModelCall{Model: "half", Usage: accountant.CallUsage{Usage: accountant.Usage{Input: 1000}, CostUSD: 0.5}}},
	}}
	p := &pricer{}
	ss, ex := Savings(res, p.price, "claude-sonnet-4-5", "api.anthropic.com")
	if len(ss) != 0 {
		t.Errorf("savings = %+v, want none: a spend is never inside a Saving", ss)
	}
	if len(ex.Spends) != 2 {
		t.Fatalf("spends = %+v, want one per spend op", ex.Spends)
	}
	sum, judge := ex.Spends[0], ex.Spends[1]
	if sum.Operator != "context-guru.summarize" || sum.ChangeID != "s:1" || sum.Model != "half" ||
		sum.Host != "other.example" || sum.Input != 1000 || !near(sum.USD, 500e-6) || sum.Basis != BasisUSD {
		t.Errorf("summarize spend = %+v, want 1000 input at half the rate on other.example", sum)
	}
	if judge.Host != "api.anthropic.com" || !near(judge.USD, 0.5) || judge.Estimated {
		t.Errorf("judge spend = %+v, want the stated cost 0.5 on the request's host, not estimated", judge)
	}
	if p.calls[0] != [2]string{"half", "other.example"} {
		t.Errorf("price calls = %v, want the call's model and host", p.calls)
	}
}

// A spend's Basis follows the billing mode, as a saving's does.
func TestSavingsBilled_SpendBasisFollowsTheMode(t *testing.T) {
	res := accountant.Result{Ops: []accountant.PricedOp{{Kind: accountant.KindSpend, Operator: "context-guru.summarize",
		Call: &accountant.ModelCall{Model: "m", Usage: accountant.CallUsage{Usage: accountant.Usage{CacheRead: 1000}, Output: 10}}}}}
	for mode, want := range map[accountant.BillingMode]string{
		accountant.BillingAPI: BasisUSD, accountant.BillingSubscription: BasisSubscriptionEquivalent, accountant.BillingUnknown: BasisUnknown,
	} {
		_, ex := SavingsBilled(res, (&pricer{}).price, nil, "m", "h", mode, config.DefaultSubscriptionWeights())
		if len(ex.Spends) != 1 || ex.Spends[0].Basis != want {
			t.Errorf("%s: spends = %+v, want one with basis %s", mode, ex.Spends, want)
			continue
		}
		sp := ex.Spends[0]
		if want == BasisSubscriptionEquivalent {
			if !near(sp.Units, 1000*0.1+10*5) || !sp.Estimated {
				t.Errorf("subscription spend = %+v, want 150 units, estimated", sp)
			}
		} else if sp.Units != 0 {
			t.Errorf("%s spend has units: %+v", mode, sp)
		}
	}
}

// A spend's own Currency, from its SpendPrice closure, reaches the record even though the
// primary request's Currency isn't threaded through SavingsBilled at all: the two are
// independent (the call's host can resolve to a different rate table than the request's — a
// credits-billed gateway beside a USD-endpoint operator call, review finding), and
// event.Figures, not this layer, is where a mismatch is caught.
func TestSavingsBilled_SpendCurrencyFollowsItsOwnCall(t *testing.T) {
	res := accountant.Result{Ops: []accountant.PricedOp{{Kind: accountant.KindSpend, Operator: "context-guru.summarize",
		Call: &accountant.ModelCall{Model: "m", Usage: accountant.CallUsage{Usage: accountant.Usage{CacheRead: 1000}}}}}}
	spend := func(c *accountant.ModelCall, host string) (float64, bool, string) { return 5, false, "CR" }
	_, ex := SavingsBilled(res, (&pricer{}).price, spend, "m", "h", accountant.BillingAPI, config.DefaultSubscriptionWeights())
	if len(ex.Spends) != 1 || ex.Spends[0].Currency != "CR" {
		t.Errorf("spends = %+v, want one with Currency CR", ex.Spends)
	}
}

// An operator with applied and dry-run ops gets two savings: the applied figure, and a projected
// one, so a "would have saved" amount never enters an applied figure.
func TestSavings_DryRunAmountsStayOutOfTheAppliedFigure(t *testing.T) {
	res := accountant.Result{Ops: []accountant.PricedOp{
		{Kind: accountant.KindRewrite, Operator: "context-guru.mask", ChangeID: "a", Saved: accountant.TierTokens{Input: 100}},
		{Kind: accountant.KindRewrite, Operator: "context-guru.mask", ChangeID: "b", DryRun: true,
			Saved: accountant.TierTokens{Input: 40}, Caused: accountant.TierTokens{Input: 8}},
	}}
	ss, _ := Savings(res, (&pricer{}).price, "m", "h")
	if len(ss) != 2 {
		t.Fatalf("savings = %+v, want an applied and a projected one", ss)
	}
	applied, projected := ss[0], ss[1]
	if applied.Projected || !near(applied.USD, 100e-6) || applied.PenaltyUSD != 0 || applied.TokensAvoided != 100 {
		t.Errorf("applied = %+v, want USD 100e-6 and nothing from the dry run", applied)
	}
	if !projected.Projected || !near(projected.USD, 40e-6) || !near(projected.PenaltyUSD, 8e-6) || projected.Operator != "context-guru.mask" {
		t.Errorf("projected = %+v, want the dry run's figure, projected", projected)
	}
}

func TestSavings_InBaselineIsIgnored(t *testing.T) {
	res := accountant.Result{
		Ops: []accountant.PricedOp{{Kind: accountant.KindRewrite, Operator: "tool-prune", ChangeID: "tp:1", Saved: accountant.TierTokens{Input: 100}}},
		CacheEffects: []accountant.CacheEffect{
			{Kind: accountant.EffectBreak, ChangeID: "tp:1", InBaseline: true, Saved: accountant.TierTokens{CacheRead: 1000}, Caused: accountant.TierTokens{CacheWrite5m: 1000}},
			{Kind: accountant.EffectBreak, InBaseline: true, Saved: accountant.TierTokens{CacheRead: 1000}, Caused: accountant.TierTokens{CacheWrite5m: 1000}},
		},
	}
	ss, ex := Savings(res, (&pricer{}).price, "m", "h")
	if tp := byOperator(t, ss)["tool-prune"]; tp.PenaltyUSD != 0 {
		t.Errorf("tool-prune penalty = %v, want 0: the client body has that break too", tp.PenaltyUSD)
	}
	if ex.UnattributedBreakUSD != 0 {
		t.Errorf("UnattributedBreakUSD = %v, want 0", ex.UnattributedBreakUSD)
	}
}

// A saving that cost more than it saved keeps both figures: nothing is floored here.
func TestSavings_NegativeNetIsKept(t *testing.T) {
	res := accountant.Result{Ops: []accountant.PricedOp{{Kind: accountant.KindRewrite, Operator: "context-guru.mask", ChangeID: "m",
		Saved: accountant.TierTokens{CacheRead: 100}, Caused: accountant.TierTokens{CacheWrite5m: 1000}}}}
	ss, _ := Savings(res, (&pricer{}).price, "m", "h")
	s := byOperator(t, ss)["context-guru.mask"]
	if !near(s.USD, 10e-6) || !near(s.PenaltyUSD, 1250e-6) {
		t.Errorf("mask = %+v, want USD 10e-6 and penalty 1250e-6, both kept", s)
	}
}

func TestSavingsBilled_SubscriptionIsUnits(t *testing.T) {
	res := accountant.Result{Version: 1, Ops: []accountant.PricedOp{{Kind: accountant.KindRewrite, Operator: "tool-prune", ChangeID: "tp",
		Saved: accountant.TierTokens{CacheRead: 1000}, Caused: accountant.TierTokens{CacheWrite5m: 8}}}}
	ss, _ := SavingsBilled(res, (&pricer{}).price, nil, "m", "h", accountant.BillingSubscription, config.DefaultSubscriptionWeights())
	s := byOperator(t, ss)["tool-prune"]
	if s.Basis != BasisSubscriptionEquivalent || !s.Estimated {
		t.Errorf("saving = %+v, want basis subscriptionEquivalent, estimated", s)
	}
	if !near(s.Units, 100) || !near(s.PenaltyUnits, 10) {
		t.Errorf("Units = %v, PenaltyUnits = %v; want 100 (1000 cache reads x 0.1) and 10 (8 writes x 1.25)", s.Units, s.PenaltyUnits)
	}
	// The API-equivalent dollars are still there.
	if !near(s.USD, 100e-6) || !near(s.PenaltyUSD, 10e-6) {
		t.Errorf("USD = %v, PenaltyUSD = %v; want the API-equivalent figures", s.USD, s.PenaltyUSD)
	}

	w := config.SubscriptionWeights{Input: 1, CacheWrite5m: 2, CacheWrite1h: 3, CacheRead: 0.5, Output: 4}
	ss, _ = SavingsBilled(res, (&pricer{}).price, nil, "m", "h", accountant.BillingSubscription, w)
	s = byOperator(t, ss)["tool-prune"]
	if !near(s.Units, 500) || !near(s.PenaltyUnits, 16) {
		t.Errorf("custom weights: Units = %v, PenaltyUnits = %v; want 500 and 16", s.Units, s.PenaltyUnits)
	}
}

// A break's units are booked by the units' own sign, not the dollars': with no rate for the model
// every break prices at $0, and a net cost in units must still be a penalty, not a negative
// saving (review finding: Units=-1150, PenaltyUnits=0).
func TestSavingsBilled_BreakUnitsFollowTheirOwnSign(t *testing.T) {
	res := accountant.Result{
		Ops: []accountant.PricedOp{{Kind: accountant.KindRewrite, Operator: "tool-prune", ChangeID: "tp"}},
		CacheEffects: []accountant.CacheEffect{{Kind: accountant.EffectBreak, ChangeID: "tp",
			Saved: accountant.TierTokens{CacheRead: 1000}, Caused: accountant.TierTokens{CacheWrite5m: 1000}}},
	}
	noRate := func(string, string, accountant.TierTokens) (float64, bool) { return 0, true }
	ss, _ := SavingsBilled(res, noRate, nil, "m", "h", accountant.BillingSubscription, config.DefaultSubscriptionWeights())
	s := byOperator(t, ss)["tool-prune"]
	if !near(s.Units, 0) || !near(s.PenaltyUnits, 1150) {
		t.Errorf("Units = %v, PenaltyUnits = %v; want 0 and 1150 (1000 x 1.25 - 1000 x 0.1)", s.Units, s.PenaltyUnits)
	}
}

func TestSavingsBilled_UnknownBasis(t *testing.T) {
	res := accountant.Result{Ops: []accountant.PricedOp{{Kind: accountant.KindRewrite, Operator: "tool-prune", ChangeID: "tp",
		Saved: accountant.TierTokens{CacheRead: 1000}}}}
	ss, _ := SavingsBilled(res, (&pricer{}).price, nil, "m", "h", accountant.BillingUnknown, config.DefaultSubscriptionWeights())
	s := byOperator(t, ss)["tool-prune"]
	if s.Basis != BasisUnknown || !s.Estimated || s.Units != 0 {
		t.Errorf("saving = %+v, want basis unknown, estimated, no units", s)
	}
	ss, _ = SavingsBilled(res, (&pricer{}).price, nil, "m", "h", accountant.BillingAPI, config.DefaultSubscriptionWeights())
	if s := byOperator(t, ss)["tool-prune"]; s.Basis != BasisUSD || s.Estimated {
		t.Errorf("api saving = %+v, want basis usd, not estimated", s)
	}
}

func TestUnits(t *testing.T) {
	w := config.DefaultSubscriptionWeights()
	got := Units(w, accountant.TierTokens{Input: 10, CacheWrite5m: 8, CacheWrite1h: 5, CacheRead: 100}, 2)
	if want := 10*1 + 8*1.25 + 5*2 + 100*0.1 + 2*5.0; !near(got, want) {
		t.Errorf("Units = %v, want %v", got, want)
	}
}

// Host.Savings runs Compute and prices its Result with the process rate table, at the request's
// model, endpoint and prompt total, and stamps the thread and the basis the client's credential
// implies.
func TestHost_Savings(t *testing.T) {
	for _, c := range []struct {
		name   string
		header http.Header
		basis  string
	}{
		{"api key", http.Header{"X-Api-Key": {"sk-ant-api03-x"}}, BasisUSD},
		{"subscription", http.Header{"Authorization": {"Bearer sk-ant-oat01-x"}}, BasisSubscriptionEquivalent},
		{"no credential", http.Header{}, BasisUnknown},
	} {
		t.Run(c.name, func(t *testing.T) {
			rates := testRates(pricing.ProvConfigured)
			h := New(Options{Resolver: rates})
			t.Cleanup(func() { _ = h.Close() })
			pctx := requestWithHeaders(t, h, "s1", c.header, clientBody("hi"),
				&operator{fwd: forwardedBody("hi"), ops: []accountant.Operation{pruneOp()}}, 200, coldUsage())
			ss, ex, ok := h.Savings(pctx)
			if !ok {
				t.Fatal("Savings: no result")
			}
			got := byOperator(t, ss)["op"]
			if got.USD <= 0 || got.Basis != c.basis || got.OpsVersion == 0 {
				t.Errorf("saving = %+v, want a priced saving with basis %s and the rules version", got, c.basis)
			}
			if c.basis == BasisSubscriptionEquivalent && got.Units <= 0 {
				t.Errorf("subscription saving has no units: %+v", got)
			}
			if ex.Thread != "main" || ex.OpsVersion != got.OpsVersion {
				t.Errorf("extras = %+v, want thread main and the rules version", ex)
			}
			if rates.endpoint != "api.anthropic.com" || rates.model != "claude-sonnet-4-5" || rates.promptTotal != 1803 {
				t.Errorf("priced at %q %q %d, want the request's endpoint, model and prompt total (1803)",
					rates.endpoint, rates.model, rates.promptTotal)
			}
			// Latched like Compute: a second call answers the same.
			again, _, ok := h.Savings(pctx)
			if !ok || len(again) != len(ss) || again[0].USD != ss[0].USD {
				t.Errorf("second Savings = %+v %v, want the same", again, ok)
			}
		})
	}
	if _, _, ok := (*Host)(nil).Savings(nil); ok {
		t.Error("a nil host answered")
	}
}

// requestWithHeaders is request with the client's headers, so RequestStart reads a credential.
func requestWithHeaders(t *testing.T, h *Host, session string, hdr http.Header, client []byte, op *operator, status int, usage pipeline.InferenceExtension) *pipeline.Context {
	t.Helper()
	p, err := pipeline.New([]pipeline.Plugin{op}, pipeline.WithAccountant(h))
	if err != nil {
		t.Fatal(err)
	}
	inf := usage
	if inf.Model == "" {
		inf.Model = "claude-sonnet-4-5"
	}
	pctx := &pipeline.Context{Host: "api.anthropic.com", Path: "/v1/messages", Method: "POST", Body: client,
		SessionID: session, Headers: hdr}
	pctx.Extensions.Inference = &inf
	if act := p.Run(context.Background(), pctx); act.Type != pipeline.Continue {
		t.Fatalf("action = %v", act.Type)
	}
	pctx.StatusCode = status
	return pctx
}

// operatorTokens attributes tokens as SavingsBilled attributes dollars, so the cross-host
// harness's token comparison checks the same split the ledger will carry.
func TestOperatorTokens(t *testing.T) {
	res := accountant.Result{
		Ops: []accountant.PricedOp{
			{Kind: accountant.KindRewrite, Operator: "context-guru.mask", ChangeID: "m", Saved: accountant.TierTokens{Input: 100}},
			{Kind: accountant.KindRewrite, Operator: "tool-prune", ChangeID: "t", Saved: accountant.TierTokens{CacheRead: 50}},
		},
		CacheEffects: []accountant.CacheEffect{
			{Kind: accountant.EffectBreak, ChangeID: "m", Saved: accountant.TierTokens{CacheRead: 10}, Caused: accountant.TierTokens{CacheWrite5m: 10}},
			{Kind: accountant.EffectBreak, ChangeID: "t", InBaseline: true, Caused: accountant.TierTokens{CacheWrite5m: 99}},
			{Kind: accountant.EffectBreak, Caused: accountant.TierTokens{CacheWrite5m: 7}},
			{Kind: accountant.EffectRescue, ChangeID: "ka:main:w", Saved: accountant.TierTokens{CacheWrite5m: 30}, Caused: accountant.TierTokens{CacheRead: 30}},
		},
	}
	order, flows := operatorTokens(res)
	if len(order) != 3 || order[2] != KeeperOperator {
		t.Fatalf("order = %v, want mask, tool-prune, keeper", order)
	}
	if f := flows["context-guru.mask"]; f.saved != (accountant.TierTokens{Input: 100, CacheRead: 10}) || f.caused != (accountant.TierTokens{CacheWrite5m: 10}) {
		t.Errorf("mask = %+v", f)
	}
	if f := flows["tool-prune"]; f.caused != (accountant.TierTokens{}) {
		t.Errorf("tool-prune = %+v: an InBaseline break is not its", f)
	}
	if f := flows[KeeperOperator]; f.saved.CacheWrite5m != 30 || f.caused.CacheRead != 30 {
		t.Errorf("keeper = %+v", f)
	}
}

// Host.Savings prices an operator's model call whole, output included, at the call's own prompt
// size: an 8k-input, 1k-output call costs its input and its output, and is not estimated for want
// of an output rate. A long-context premium of the request does not reach it.
func TestHost_SavingsPricesSpendOutputAtTheCallsOwnSize(t *testing.T) {
	rates := testRates(pricing.ProvConfigured)
	// A long-context premium above 100k prompt tokens: input 10x. The request is above it, the
	// call is not.
	th := pricing.ContextThreshold{AbovePromptTokens: 100_000}
	th.Rate[pricing.TierInput], th.Set[pricing.TierInput] = 3e-5, true
	rates.rates.Thresholds = []pricing.ContextThreshold{th}
	h := New(Options{Resolver: &atRates{rates}})
	t.Cleanup(func() { _ = h.Close() })
	call := accountant.Operation{Kind: accountant.KindSpend, Operator: "context-guru.summarize", ChangeID: "s:1",
		Call: &accountant.ModelCall{Model: "claude-haiku-4-5", Usage: accountant.CallUsage{Usage: accountant.Usage{Input: 8000}, Output: 1000}}}
	usage := pipeline.InferenceExtension{InputTokens: 150_000, CacheWriteTokens: 1800}
	pctx := requestWithHeaders(t, h, "s1", http.Header{"X-Api-Key": {"k"}}, clientBody("hi"),
		&operator{fwd: forwardedBody("hi"), ops: []accountant.Operation{pruneOp(), call}}, 200, usage)
	ss, ex, ok := h.Savings(pctx)
	if !ok {
		t.Fatal("no result")
	}
	if len(ex.Spends) != 1 {
		t.Fatalf("spends = %+v, want one", ex.Spends)
	}
	sp := ex.Spends[0]
	if want := 8000*3e-6 + 1000*1.5e-5; !near(sp.USD, want) || sp.Estimated {
		t.Errorf("spend = %+v, want USD %v (input at the base rate plus output), not estimated", sp, want)
	}
	if sp.Thread != "main" || sp.Host != "api.anthropic.com" {
		t.Errorf("spend = %+v, want thread main and the request's host", sp)
	}
	for _, s := range ss {
		if s.Operator == "context-guru.summarize" {
			t.Errorf("the call's operator has a saving: %+v", s)
		}
	}
}

// atRates flattens its rates for the prompt size it is asked about, as pricing.Table does.
type atRates struct{ f *fixedRates }

func (a *atRates) Resolve(endpoint, model string, promptTotal int) (pricing.Rates, pricing.Provenance) {
	r, p := a.f.Resolve(endpoint, model, promptTotal)
	return r.At(promptTotal), p
}
func (a *atRates) CurrencyFor(e, m string) string { return a.f.CurrencyFor(e, m) }
