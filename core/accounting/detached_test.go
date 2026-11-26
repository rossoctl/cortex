package accounting

import (
	"math"
	"testing"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// detachedPctx is a request of session s1 to api.anthropic.com, billed on an API key.
func detachedPctx() *pipeline.Context {
	pctx := &pipeline.Context{SessionID: "s1", Host: "api.anthropic.com"}
	pipeline.SetState(pctx, billingKey, &accountant.Billing{Mode: accountant.BillingAPI})
	return pctx
}

// summarizerCall is a background summarizer call: 5,000 fresh input tokens and 300 output.
func summarizerCall() *accountant.ModelCall {
	return &accountant.ModelCall{Model: "claude-haiku-4-5", Detached: true,
		Usage: accountant.CallUsage{Usage: accountant.Usage{Input: 5000}, Output: 300}}
}

// cortex#2140: a model call that finishes after its request was accounted is booked as one cost
// event of Purpose detached-spend. Its one SpendRecord holds the call priced at its own model
// (CallUSD), in the basis of the request's billing mode, and the listener gets the same figure.
func TestLateOps_BooksTheSpend(t *testing.T) {
	rec := &fakeRecorder{}
	h := New(Options{Resolver: pingRates(t), CostRecorders: []session.Recorder{rec}})
	var got []DetachedSpend
	h.AddDetachedSpendListener(func(d DetachedSpend) { got = append(got, d) })

	handler := h.LateOps(detachedPctx())
	if handler == nil {
		t.Fatal("LateOps gave no handler for a request with a session")
	}
	op := accountant.Operation{ID: "op1", Kind: accountant.KindSpend, Operator: "context-guru", Call: summarizerCall()}
	if kept := handler([]accountant.Operation{op}); kept != 1 {
		t.Fatalf("kept = %d, want 1", kept)
	}
	_ = h.Close() // waits for the booking goroutine

	// 5,000 x $3/M + 300 x $15/M = $0.0195.
	want, _ := CallUSD(pingRates(t), op.Call, "api.anthropic.com")
	if math.Abs(want-0.0195) > 1e-9 {
		t.Fatalf("CallUSD = %v, want 0.0195", want)
	}
	ids, evs := rec.got()
	if len(evs) != 1 || ids[0] != "s1" {
		t.Fatalf("events = %d (sessions %v), want 1 on s1", len(evs), ids)
	}
	e := evs[0]
	if e.Phase != model.SessionResponse || e.Host != "api.anthropic.com" || e.Inference == nil ||
		e.Inference.Model != "claude-haiku-4-5" || e.Inference.TotalTokens != 0 {
		t.Errorf("event = phase %v host %q inference %+v", e.Phase, e.Host, e.Inference)
	}
	r, ok := event.Record(e)
	if !ok {
		t.Fatal("no cost record on the event")
	}
	if !r.IsDetachedSpend() || r.Priced() || r.Thread != "main" {
		t.Errorf("record purpose %q priced %v thread %q, want detached-spend, unpriced, main", r.Purpose, r.Priced(), r.Thread)
	}
	if len(r.Spends) != 1 {
		t.Fatalf("Spends = %+v, want one", r.Spends)
	}
	sp := r.Spends[0]
	if sp.Operator != "context-guru" || sp.Basis != BasisUSD || !sp.Counted() || math.Abs(sp.USD-want) > 1e-9 {
		t.Errorf("spend = %+v, want context-guru, usd, counted, USD %v", sp, want)
	}
	if f := r.Figures(); f.PluginSpendMicros != 19500 {
		t.Errorf("PluginSpendMicros = %d, want 19500", f.PluginSpendMicros)
	}
	if len(got) != 1 || got[0].Session != "s1" || got[0].Figures.PluginSpendMicros != 19500 {
		t.Errorf("listener got %+v, want one call of 19500 micros on s1", got)
	}
}

// A late op that is not a spend changes no byte that was sent, and a request with no session has
// nothing to book on: neither is kept, so the pipeline counts them as dropped.
func TestLateOps_KeepsOnlySpendsOnASession(t *testing.T) {
	rec := &fakeRecorder{}
	h := New(Options{Resolver: pingRates(t), CostRecorders: []session.Recorder{rec}})
	if h.LateOps(&pipeline.Context{}) != nil {
		t.Error("LateOps gave a handler for a request with no session")
	}
	handler := h.LateOps(detachedPctx())
	kept := handler([]accountant.Operation{
		{ID: "r", Kind: accountant.KindRewrite, ChangeID: "c1"},
		{ID: "s", Kind: accountant.KindSpend}, // no Call: nothing to price
	})
	if kept != 0 {
		t.Errorf("kept = %d, want 0", kept)
	}
	_ = h.Close()
	if _, evs := rec.got(); len(evs) != 0 {
		t.Errorf("recorded %d events, want none", len(evs))
	}
	var nilHost *Host
	if nilHost.LateOps(detachedPctx()) != nil {
		t.Error("a nil Host gave a handler")
	}
}

// A late call whose usage is unknown (the provider answered with an error) is refused by
// ComputeStandalone. Its record says Refused and counts in no total: no figure says $0 as if the
// cost were known.
func TestLateOps_UnknownUsageIsRefused(t *testing.T) {
	rec := &fakeRecorder{}
	h := New(Options{Resolver: pingRates(t), CostRecorders: []session.Recorder{rec}})
	handler := h.LateOps(detachedPctx())
	call := &accountant.ModelCall{Model: "claude-haiku-4-5", Detached: true}
	// No ID, as cgModel and meteredDispatcher send it: the refusal must still find the op.
	if kept := handler([]accountant.Operation{{Kind: accountant.KindSpend, Call: call}}); kept != 1 {
		t.Fatalf("kept = %d, want 1", kept)
	}
	_ = h.Close()
	_, evs := rec.got()
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	r, _ := event.Record(evs[0])
	if len(r.Spends) != 1 || !r.Spends[0].Refused || r.Spends[0].Counted() || !r.Figures().IsZero() {
		t.Errorf("spends = %+v figures %+v, want one refused spend and zero figures", r.Spends, r.Figures())
	}
}

// After Close, a late call books nothing, and the handler says so, so the pipeline counts it.
func TestLateOps_AfterCloseKeepsNothing(t *testing.T) {
	h := New(Options{Resolver: pingRates(t)})
	handler := h.LateOps(detachedPctx())
	_ = h.Close()
	op := accountant.Operation{Kind: accountant.KindSpend, Call: summarizerCall()}
	if kept := handler([]accountant.Operation{op}); kept != 0 {
		t.Errorf("kept = %d after Close, want 0", kept)
	}
}

// Host implements the pipeline's optional capability, so Run hands it the late ops.
var _ pipeline.LateOpsAccountant = (*Host)(nil)

// A detached call's Result reaches llm.savings.added.usd{kind="spend"} at the ledger's figure
// (CallUSD at the call's own host and prompt size), and moves no llm.inference.* or
// llm.keepalive.* counter: it is neither the agent's request nor a ping.
func TestSink_ObserveDetachedBooksOnlyTheSpend(t *testing.T) {
	mp, rd := manual(t)
	s := NewSink(mp, zone)
	op := spendOp("sp1", "m2", "other.example")
	res := accountant.Result{Version: 1, Ops: []accountant.PricedOp{op}}
	s.ObserveDetached(accountant.RequestMeta{RequestID: "r1", Session: "s1", Billing: apiBilling}, res, "api.example")

	want, _ := CallUSD(zone, op.Call, "other.example")
	got := sum(t, rd, "llm.savings.added.usd", map[string]string{"kind": "spend"})
	if want <= 0 || !micro(got, want) {
		t.Errorf("pushed spend usd = %.9f, want %.9f (CallUSD)", got, want)
	}
	for _, name := range []string{"llm.inference.tokens", "llm.inference.cost_usd", "llm.keepalive.pings", "llm.keepalive.cost_usd"} {
		if v := sum(t, rd, name, nil); v != 0 {
			t.Errorf("%s = %v, want 0", name, v)
		}
	}
}
