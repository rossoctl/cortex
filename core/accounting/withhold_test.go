package accounting

import (
	"net/http"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/pipeline"
)

// namedOp is operator with a plugin name of its own, so two can run in one pipeline.
type namedOp struct {
	operator
	name string
}

func (o *namedOp) Name() string { return o.name }

// overlapRequest runs a request on which two operators report the removal of the same tool:
// prune-a removes it from the body, prune-b claims the same removal. The library fails a fatal
// check (rossoctl/cortex#2134).
func overlapRequest(t *testing.T, h *Host) *pipeline.Context {
	t.Helper()
	opA, opB := pruneOp(), pruneOp()
	opA.ChangeID, opB.ChangeID = "a:big", "b:big"
	a := &namedOp{operator{fwd: forwardedBody("hi"), ops: []accountant.Operation{opA}}, "prune-a"}
	b := &namedOp{operator{ops: []accountant.Operation{opB}}, "prune-b"}
	p, err := pipeline.New([]pipeline.Plugin{a, b}, pipeline.WithAccountant(h))
	if err != nil {
		t.Fatal(err)
	}
	return runOn(t, p, "s1", "/v1/messages", clientBody("hi"), 200, coldUsage())
}

// TestHost_SavingsWithheldOnAFatalCheck: two operators claim one removal. The library's sum check
// fails, so Cortex books no saving and names the gate, where it used to book the saving twice.
func TestHost_SavingsWithheldOnAFatalCheck(t *testing.T) {
	mp, rd := manual(t)
	rates := testRates(pricing.ProvConfigured)
	// The Sink gets its own resolver: fixedRates records its last lookup, and the Sink prices on
	// the observe goroutine while Savings resolves on this one.
	h := New(Options{Resolver: rates, Sink: NewSink(mp, testRates(pricing.ProvConfigured))})
	h.computeWait = time.Hour // wait on the record, not the clock
	pctx := overlapRequest(t, h)

	res, ok := h.Compute(pctx)
	if !ok || res.Reliable() {
		t.Fatalf("Compute = ok %v, checks %+v: the test needs a fatal check", ok, res.Checks)
	}
	ss, ex, ok := h.Savings(pctx)
	if !ok {
		t.Fatal("Savings: no result")
	}
	if len(ss) != 0 {
		t.Errorf("savings = %+v, want none: a fatal check books no saving", ss)
	}
	if ex.Withheld != WithheldCheckSum {
		t.Errorf("Withheld = %q, want %q", ex.Withheld, WithheldCheckSum)
	}
	if ex.UnattributedBreakUSD != 0 || ex.InteractionUSD != 0 {
		t.Errorf("UnattributedBreakUSD = %v, InteractionUSD = %v, want 0 on a withheld request (a guard: this request has no interaction)",
			ex.UnattributedBreakUSD, ex.InteractionUSD)
	}
	// Latched: a second call answers the same.
	if again, ex2, _ := h.Savings(pctx); len(again) != 0 || ex2.Withheld != WithheldCheckSum {
		t.Errorf("second Savings = %+v %q, want none and %q", again, ex2.Withheld, WithheldCheckSum)
	}

	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if got := sum(t, rd, "llm.savings.withheld", map[string]string{"reason": WithheldCheckSum}); got != 1 {
		t.Errorf("llm.savings.withheld{reason=check_sum} = %v, want 1", got)
	}
}

// TestHost_SavingsOneOperatorIsBooked: the same removal reported once passes every gate. The
// library cannot rule out a break on a thread's first request (penalty_unknown), so the saving is
// booked and marked estimated (accountant design §6.4).
func TestHost_SavingsOneOperatorIsBooked(t *testing.T) {
	mp, rd := manual(t)
	rates := testRates(pricing.ProvConfigured)
	// The Sink gets its own resolver: fixedRates records its last lookup, and the Sink prices on
	// the observe goroutine while Savings resolves on this one.
	h := New(Options{Resolver: rates, Sink: NewSink(mp, testRates(pricing.ProvConfigured))})
	h.computeWait = time.Hour // wait on the record, not the clock
	// Only the penalty_unknown branch may mark this saving estimated, so nothing else does: an API
	// key gives basis usd (every other basis is estimated), and a measured token count keeps the
	// op exact (a size in bytes only is converted, and marked estimated).
	op := pruneOp()
	op.SizeBefore.Tokens = 1000
	pctx := requestWithHeaders(t, h, "s1", http.Header{"X-Api-Key": {"sk-ant-api03-x"}}, clientBody("hi"),
		&operator{fwd: forwardedBody("hi"), ops: []accountant.Operation{op}}, 200, coldUsage())
	ss, ex, ok := h.Savings(pctx)
	if !ok || ex.Withheld != "" {
		t.Fatalf("Savings = ok %v, Withheld %q, want a booked saving", ok, ex.Withheld)
	}
	got := byOperator(t, ss)["op"]
	if got.USD <= 0 || got.TokensAvoided <= 0 {
		t.Errorf("saving = %+v, want a priced saving", got)
	}
	if got.Basis != BasisUSD {
		t.Fatalf("basis = %q, want usd", got.Basis)
	}
	res, _ := h.Compute(pctx)
	if !penaltyUnknown(res) {
		t.Fatalf("Unpriced = %+v, want penalty_unknown: a thread's first request has no history", res.Unpriced)
	}
	for _, o := range res.Ops {
		if o.Estimated {
			t.Fatalf("op %+v is estimated: the test needs an exact op to see the branch", o)
		}
	}
	if !got.Estimated {
		t.Errorf("saving = %+v, want Estimated: the Result lists penalty_unknown", got)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if n := sum(t, rd, "llm.savings.withheld", nil); n != 0 {
		t.Errorf("llm.savings.withheld = %v, want 0", n)
	}
}

func TestTokensBefore(t *testing.T) {
	for _, c := range []struct{ prompt, client, fwd, want int }{
		{1000, 2000, 1000, 2000}, // half the body removed: twice the billed prompt
		{1000, 1000, 1000, 1000}, // unchanged
		{1000, 800, 1000, 1000},  // an operator added bytes: the billed prompt itself
		{0, 2000, 1000, 0},       // no billed prompt
		{1000, 2000, 0, 0},       // no forwarded body
	} {
		if got := tokensBefore(c.prompt, c.client, c.fwd); got != c.want {
			t.Errorf("tokensBefore(%d, %d, %d) = %d, want %d", c.prompt, c.client, c.fwd, got, c.want)
		}
	}
}

// The library's sink gets Cortex's size measure: an op that saved more than the client's body had
// is withheld on the metrics side too, and the ledger and the counters agree.
func TestSink_ObserveSizedGivesTheSizeGate(t *testing.T) {
	mp, rd := manual(t)
	s := NewSink(mp, testRates(pricing.ProvConfigured))
	res := accountant.Result{Ops: []accountant.PricedOp{{Kind: accountant.KindRewrite, Operator: "tool-prune",
		Saved: accountant.TierTokens{Input: 5000}}}}
	meta := accountant.RequestMeta{Model: "claude-sonnet-4-5", Billing: accountant.Billing{Mode: accountant.BillingAPI}}
	s.ObserveSized(meta, accountant.Usage{Input: 100}, 0, res, "api.anthropic.com", 100, false, 1000)
	s.Observe(meta, accountant.Usage{Input: 100}, 0, res, "api.anthropic.com", 100, false) // no size: no gate
	if got := sum(t, rd, "llm.savings.withheld", map[string]string{"reason": WithheldImplausibleSize}); got != 1 {
		t.Errorf("llm.savings.withheld{reason=implausible_size} = %v, want 1", got)
	}
	if got := sum(t, rd, "llm.savings.withheld", nil); got != 1 {
		t.Errorf("llm.savings.withheld = %v, want 1: Observe gives no size, so it skips the size gate", got)
	}
}

// withholdSavings clears the interaction (rossoctl/cortex#2141 review): the library's metrics sink
// books none for a withheld request. A rescued request has an interaction, and that case needs a
// ping to build through the pipeline, so the helper is tested on interactionResult directly.
func TestWithholdSavingsClearsTheInteraction(t *testing.T) {
	res, u := interactionResult()
	ex := figuredSavings(t, testRates(pricing.ProvConfigured), res, u, accountant.BillingAPI)
	if ex.InteractionUSD == 0 {
		t.Fatal("InteractionUSD = 0 before the gate: the test needs an interaction to clear")
	}
	ex.UnattributedBreakUSD = 0.002
	ex.Spends = []event.SpendRecord{{Operator: "context-guru.summarize", USD: 0.01}}
	ss, got := withholdSavings(ex, WithheldCheckSum)
	if ss != nil {
		t.Errorf("savings = %+v, want none", ss)
	}
	if got.InteractionUSD != 0 || got.UnattributedBreakUSD != 0 || got.Withheld != WithheldCheckSum {
		t.Errorf("extras = %+v, want InteractionUSD 0, UnattributedBreakUSD 0, Withheld check_sum", got)
	}
	if len(got.Spends) != 1 {
		t.Errorf("spends = %+v, want the spend kept", got.Spends)
	}
}

// withSystem is body with a system prompt added and no op reported: what evolve's guidelines
// injection does to a body with no system (core/plugins/evolve, injectAnthropic).
func withSystem(body []byte) []byte {
	return append([]byte(`{"system":"Follow the team guidelines: keep changes small.",`), body[1:]...)
}

// An evolve-shaped writer beside tool-prune (rossoctl/cortex#2141 review). evolve changes system and
// reports no op, so the sum check fails for system with no op claiming it. accountant v0.10.0 does
// not gate on that: the reported removal in tools books its saving. A request where only evolve
// changed the body has nothing withheld, and no llm.savings.withheld.
func TestHost_SavingsKeepsAReportedOpBesideAnUnreportedWriter(t *testing.T) {
	for _, c := range []struct {
		name  string
		fwd   []byte
		ops   []accountant.Operation
		saved bool
	}{
		{"prune and evolve", withSystem(forwardedBody("hi")), []accountant.Operation{pruneOp()}, true},
		{"evolve only", withSystem(clientBody("hi")), nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			mp, rd := manual(t)
			h := New(Options{Resolver: testRates(pricing.ProvConfigured), Sink: NewSink(mp, testRates(pricing.ProvConfigured))})
			h.computeWait = time.Hour
			pctx := request(t, h, "s1", "/v1/messages", clientBody("hi"), &operator{fwd: c.fwd, ops: c.ops}, 200, coldUsage())
			res, ok := h.Compute(pctx)
			if !ok {
				t.Fatal("Compute: no result")
			}
			sumFailed := false
			for _, f := range res.Checks {
				if f.Check == accountant.CheckSum && f.Section == accountant.SectionSystem && f.Claims != nil && *f.Claims == 0 {
					sumFailed = true
				}
			}
			if !sumFailed {
				t.Fatalf("checks = %+v, want a sum failure for system with Claims 0", res.Checks)
			}
			ss, ex, _ := h.Savings(pctx)
			if ex.Withheld != "" {
				t.Errorf("Withheld = %q, want \"\": no op claims the section evolve changed", ex.Withheld)
			}
			if got := len(ss) > 0 && ss[0].TokensAvoided > 0; got != c.saved {
				t.Errorf("savings = %+v, want a booked saving: %v", ss, c.saved)
			}
			if err := h.Close(); err != nil {
				t.Fatal(err)
			}
			if n := sum(t, rd, "llm.savings.withheld", nil); n != 0 {
				t.Errorf("llm.savings.withheld = %v, want 0", n)
			}
		})
	}
}
