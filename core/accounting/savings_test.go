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
		{Kind: accountant.KindRewrite, Operator: "context-guru.mask", ChangeID: "c", DryRun: true, Saved: accountant.TierTokens{Input: 5}},
		{Kind: accountant.KindRewrite, Operator: "context-guru.mask", ChangeID: "d", Saved: accountant.TierTokens{Input: 5}},
	}}
	got := byOperator(t, func() []event.Saving { s, _ := Savings(res, (&pricer{}).price, "m", "h"); return s }())
	if !got["tool-prune"].Projected {
		t.Error("an operator whose every op is a dry run is not projected")
	}
	if got["context-guru.mask"].Projected {
		t.Error("an operator with an applied op is projected")
	}
}

// A spend op (an operator's own model call) is priced with the call's model and host, not the
// request's, and is a cost of its operator. A stated CostUSD is a fact and wins over the rates.
func TestSavings_SpendOpPricedWithItsOwnModel(t *testing.T) {
	res := accountant.Result{Ops: []accountant.PricedOp{
		{Kind: accountant.KindSpend, Operator: "context-guru.summarize", ChangeID: "s:1",
			Call: &accountant.ModelCall{Model: "half", Host: "other.example", Usage: accountant.CallUsage{Usage: accountant.Usage{Input: 1000}}}},
		{Kind: accountant.KindSpend, Operator: "context-guru.judge", ChangeID: "s:2",
			Call: &accountant.ModelCall{Model: "half", Usage: accountant.CallUsage{Usage: accountant.Usage{Input: 1000}, CostUSD: 0.5}}},
	}}
	p := &pricer{}
	ss, _ := Savings(res, p.price, "claude-sonnet-4-5", "api.anthropic.com")
	got := byOperator(t, ss)
	if s := got["context-guru.summarize"]; !near(s.PenaltyUSD, 500e-6) || s.USD != 0 {
		t.Errorf("summarize = %+v, want penalty 500e-6 (1000 input at half the rate)", s)
	}
	if s := got["context-guru.judge"]; !near(s.PenaltyUSD, 0.5) {
		t.Errorf("judge = %+v, want the stated cost 0.5", s)
	}
	var sawCall bool
	for _, c := range p.calls {
		if c == [2]string{"half", "other.example"} {
			sawCall = true
		}
	}
	if !sawCall {
		t.Errorf("price calls = %v, want one with the call's model and host", p.calls)
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
	ss, _ := SavingsBilled(res, (&pricer{}).price, "m", "h", accountant.BillingSubscription, config.DefaultSubscriptionWeights())
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
	ss, _ = SavingsBilled(res, (&pricer{}).price, "m", "h", accountant.BillingSubscription, w)
	s = byOperator(t, ss)["tool-prune"]
	if !near(s.Units, 500) || !near(s.PenaltyUnits, 16) {
		t.Errorf("custom weights: Units = %v, PenaltyUnits = %v; want 500 and 16", s.Units, s.PenaltyUnits)
	}
}

func TestSavingsBilled_UnknownBasis(t *testing.T) {
	res := accountant.Result{Ops: []accountant.PricedOp{{Kind: accountant.KindRewrite, Operator: "tool-prune", ChangeID: "tp",
		Saved: accountant.TierTokens{CacheRead: 1000}}}}
	ss, _ := SavingsBilled(res, (&pricer{}).price, "m", "h", accountant.BillingUnknown, config.DefaultSubscriptionWeights())
	s := byOperator(t, ss)["tool-prune"]
	if s.Basis != BasisUnknown || !s.Estimated || s.Units != 0 {
		t.Errorf("saving = %+v, want basis unknown, estimated, no units", s)
	}
	ss, _ = SavingsBilled(res, (&pricer{}).price, "m", "h", accountant.BillingAPI, config.DefaultSubscriptionWeights())
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
			h, err := New(Options{Resolver: rates})
			if err != nil {
				t.Fatal(err)
			}
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
