package inferenceparser

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/accounting"
	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/cost/settle"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/toolprune"
)

// countingAccountant stands in for *accounting.Host.
type countingAccountant struct {
	calls  int
	saving []event.Saving
	extras accounting.Extras
}

func (c *countingAccountant) Savings(*pipeline.Context) ([]event.Saving, accounting.Extras, bool) {
	c.calls++
	return c.saving, c.extras, true
}

// settleCost runs on every finalization path, and a listener can repeat a terminal frame, so it
// can run more than once for one request. The accountant is asked once, at the first pass with
// usage.
func TestSettleCost_CallsAccountingOnce(t *testing.T) {
	acc := &countingAccountant{}
	p := NewInferenceParser()
	p.acct = acc

	// No usage yet: nothing to account.
	pctx := &pipeline.Context{Path: "/v1/messages", ResponseHeaders: http.Header{}}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "claude-sonnet-4-5"}
	p.settleCost(pctx)
	if acc.calls != 0 {
		t.Fatalf("Savings called %d times before the usage arrived", acc.calls)
	}

	pctx.Extensions.Inference.InputTokens = 10
	pctx.Extensions.Inference.CacheWriteTokens = 900
	p.settleCost(pctx)
	p.settleCost(pctx)
	if acc.calls != 1 {
		t.Errorf("Savings called %d times, want 1", acc.calls)
	}
}

// The switch (design §5.9): the record's savings are the accountant's, end to end. A request
// tool-prune shrank, run through a pipeline with the accountant host attached, publishes
// tool-prune's saving as the accountant priced it — TierUSD over the op's Saved tokens at the
// request's model, host and prompt size — with basis "usd" for an API key.
func TestSettleCost_PublishesAccountantSavings(t *testing.T) {
	rates := bodylessRates(t)
	h := accounting.New(accounting.Options{Resolver: rates})
	t.Cleanup(func() { _ = h.Close() })
	tp := toolprune.New()
	if err := tp.Configure(json.RawMessage(`{"remove":["NotebookEdit"]}`)); err != nil {
		t.Fatal(err)
	}
	pipe, err := pipeline.New([]pipeline.Plugin{tp}, pipeline.WithAccountant(h))
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"claude-sonnet-4-5","max_tokens":10,"tools":[` +
		`{"name":"Read","description":"read a file","input_schema":{"type":"object"}},` +
		`{"name":"NotebookEdit","description":"` + strings.Repeat("edit a notebook ", 200) + `","input_schema":{"type":"object"}},` +
		`{"name":"Bash","description":"run a command","input_schema":{"type":"object"}}` +
		`],"messages":[{"role":"user","content":"` + strings.Repeat("lorem ipsum dolor ", 400) + `"}]}`
	pctx := &pipeline.Context{
		Host: "api.anthropic.com", Method: "POST", Path: "/v1/messages", SessionID: "s1",
		Body: []byte(body), Headers: http.Header{"X-Api-Key": {"sk-ant-api03-test"}}, ResponseHeaders: http.Header{},
	}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "claude-sonnet-4-5",
		Tools: []pipeline.InferenceTool{{Name: "Read"}, {Name: "NotebookEdit"}, {Name: "Bash"}}}
	if act := pipe.Run(context.Background(), pctx); act.Type != pipeline.Continue {
		t.Fatalf("action = %v", act.Type)
	}
	pctx.StatusCode = 200
	pctx.Extensions.Inference.InputTokens = len(pctx.Body) / 4
	pctx.Extensions.Inference.OutputTokens = 5

	p := NewInferenceParser()
	p.rates = rates
	p.SetAccounting(h)
	p.settleCost(pctx)

	ev, _ := publishedCost(t, pctx)
	if len(ev.Avoided) == 0 {
		t.Fatalf("no saving published: %+v", ev)
	}
	s := ev.Avoided[0]
	if s.Component != "tool-prune" || s.Operator != "tool-prune" || s.Basis != event.BasisUSD || s.Projected || s.OpsVersion == 0 {
		t.Errorf("saving = %+v, want tool-prune's, applied, basis usd, with the rules' version", s)
	}
	res, ok := h.Compute(pctx) // latched: the Result settleCost priced
	if !ok {
		t.Fatal("Compute has no Result")
	}
	prompt := pricing.UsageFromInference(pctx.Extensions.Inference).PromptTotal()
	for _, o := range res.Ops {
		if o.Operator != "tool-prune" || o.DryRun {
			continue
		}
		want, _ := accounting.TierUSD(rates, "api.anthropic.com", "claude-sonnet-4-5", prompt, o.Saved)
		if want <= 0 || math.Abs(s.USD-want) > 1e-12 {
			t.Errorf("USD = %v, want TierUSD of the op's Saved %+v = %v", s.USD, o.Saved, want)
		}
	}
	if got, want := ev.TotalAvoidedUSD(), s.USD; got != want {
		t.Errorf("TotalAvoidedUSD = %v, want the usd saving %v", got, want)
	}
	if ev.Thread != "main" {
		t.Errorf("Thread = %q, want main", ev.Thread)
	}
}

// What belongs to no operator's saving is carried on the record too: the thread, the
// unattributed break cost and each operator's own model call. A spend alone is reason to publish
// a record, because no other record holds it.
func TestSettleCost_PublishesTheAccountantsExtras(t *testing.T) {
	spend := event.SpendRecord{Operator: "context-guru.extract_llm", Model: "m", USD: 0.02, Basis: event.BasisUSD}
	p := NewInferenceParser() // no rates: nothing priced, so only the spend can publish
	p.acct = &countingAccountant{extras: accounting.Extras{
		Thread: "agent-1", UnattributedBreakUSD: 0.003, OpsVersion: 1, Spends: []event.SpendRecord{spend},
	}}
	pctx := &pipeline.Context{Path: "/v1/messages", ResponseHeaders: http.Header{}}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "unpriced", InputTokens: 10}
	p.settleCost(pctx)
	ev, _ := publishedCost(t, pctx)
	if ev.Thread != "agent-1" || ev.UnattributedBreakUSD != 0.003 || len(ev.Spends) != 1 || ev.Spends[0] != spend {
		t.Errorf("record = %+v, want the accountant's thread, unattributed break and spend", ev)
	}
	if got := ev.Figures().PluginSpendMicros; got != 20_000 {
		t.Errorf("PluginSpendMicros = %d, want 20000", got)
	}
}

// A zero prompt total must not drop an already-completed operator spend: Savings prices from
// the upstream prompt-tier counters, but a spend prices from its own operator call, independent
// of them (review finding — an error status with no prompt usage, or a context-guru call billed
// before an inference-router resend, used to lose the spend along with the never-priced
// savings). pctx.StatusCode set (the response was read) is what tells accountant() this is a
// real pass and not the one before any data arrived (TestSettleCost_CallsAccountingOnce).
func TestSettleCost_KeepsSpendAtZeroPromptTotal(t *testing.T) {
	spend := event.SpendRecord{Operator: "context-guru.extract_llm", Model: "m", USD: 0.02, Basis: event.BasisUSD}
	p := NewInferenceParser()
	acc := &countingAccountant{extras: accounting.Extras{Spends: []event.SpendRecord{spend}}}
	p.acct = acc
	pctx := &pipeline.Context{Path: "/v1/messages", ResponseHeaders: http.Header{}, StatusCode: 500}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "m"}
	p.settleCost(pctx)
	if acc.calls != 1 {
		t.Fatalf("Savings called %d times, want 1 at a zero prompt total with a decided response", acc.calls)
	}
	ev, ok := publishedCost(t, pctx)
	if !ok || len(ev.Spends) != 1 || ev.Spends[0] != spend {
		t.Errorf("record = %+v ok=%v, want the accountant's spend kept at a zero prompt total", ev, ok)
	}
}

// A STREAM CUT MID-BODY must not drop a completed spend either: forwardproxy's
// classifyBrokenStream resets pctx.StatusCode to 0 on a stream that broke before its final usage
// event, specifically so the outcome is not misread as a 200 — but a context-guru call already
// billed on that request is not undone by the break (review finding: StatusCode alone dropped
// this, where the broader-but-wrong ReportedOps check above had not). pctx.MarkResponseDelivered
// is what every listener calls before bytes reach the client, ahead of where a break can zero
// StatusCode, so it is what keeps this case asking.
func TestSettleCost_KeepsSpendOnAStreamCutMidBody(t *testing.T) {
	spend := event.SpendRecord{Operator: "context-guru.extract_llm", Model: "m", USD: 0.02, Basis: event.BasisUSD}
	p := NewInferenceParser()
	acc := &countingAccountant{extras: accounting.Extras{Spends: []event.SpendRecord{spend}}}
	p.acct = acc
	pctx := &pipeline.Context{Path: "/v1/messages", ResponseHeaders: http.Header{}}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "m"}
	pctx.MarkResponseDelivered()
	pctx.StatusCode = 0 // classifyBrokenStream's own reset, after delivery began
	p.settleCost(pctx)
	if acc.calls != 1 {
		t.Fatalf("Savings called %d times, want 1 for a response that was delivered then broke", acc.calls)
	}
	ev, ok := publishedCost(t, pctx)
	if !ok || len(ev.Spends) != 1 || ev.Spends[0] != spend {
		t.Errorf("record = %+v ok=%v, want the accountant's spend kept on a broken stream", ev, ok)
	}
}

// A STRUCTURAL OP REPORTED BEFORE THE RESPONSE must not trigger an early ask: an op such as a
// tool-prune removal reports during OnRequest, so it is already on pctx.ReportedOps() at the
// very first settle pass, before pctx.StatusCode is ever set. An earlier version of the gate
// above asked on ANY reported op, which called Host.Compute with an inference extension that
// was still all zeros — and Compute latches that answer forever (its own doc: "do not call
// Compute again to retry"), so the real counts that arrive on pass 2 below were never priced at
// all (review finding, reproduced). Pinned here directly against accountant(), which is where
// the gate lives; TestSettleCost_CallsAccountingOnce pins the same ordering through settleCost.
func TestAccountant_AnOpReportedBeforeTheResponseDoesNotForceAnEarlyAsk(t *testing.T) {
	acc := &countingAccountant{saving: []event.Saving{{Component: "tool-prune", Basis: event.BasisUSD, USD: 20}}}
	p := NewInferenceParser()
	p.acct = acc
	pctx := &pipeline.Context{Path: "/v1/messages", ResponseHeaders: http.Header{}}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "m"}
	pctx.ReportOps(accountant.Operation{Kind: accountant.KindRewrite, Operator: "tool-prune"})

	// Pass 1: the op is already reported, but the response has not been read yet.
	if a := p.accountant(pctx, settle.Settled{}); a.ok {
		t.Fatalf("accountant = %+v, want not asked before the response is decided", a)
	}
	if acc.calls != 0 {
		t.Fatalf("Savings called %d times, want 0 before the response is decided", acc.calls)
	}

	// Pass 2: the response arrives, with real prompt tokens.
	pctx.StatusCode = 200
	pctx.Extensions.Inference.InputTokens = 100
	a := p.accountant(pctx, settle.Settled{})
	if !a.ok || acc.calls != 1 {
		t.Fatalf("accountant = %+v (calls=%d), want asked exactly once the real counts arrived", a, acc.calls)
	}
}

func TestSettleCost_NoAccountantIsFine(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{Path: "/v1/messages", ResponseHeaders: http.Header{}}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "m", InputTokens: 10}
	p.settleCost(pctx) // must not panic
	p.SetAccounting(nil)
	if p.acct != nil {
		t.Error("SetAccounting(nil) stored a typed nil, which would read as set")
	}
}

// THE REFUSED-COUNTS GUARD, ported from settle.Avoided (the assertion that sat in
// core/cost/settle's bound_agreement_test): when settle refuses the token report, no saving is
// calibrated on it. The gateway's header still prices the request; the accountant is not asked,
// and the record carries no saving.
func TestSettleCost_NoSavingOnRefusedCounts(t *testing.T) {
	acc := &countingAccountant{saving: []event.Saving{{Component: "tool-prune", Basis: event.BasisUSD, USD: 20}}}
	p := NewInferenceParser()
	p.rates = bodylessRates(t)
	p.acct = acc
	pctx := &pipeline.Context{Path: "/v1/messages", ResponseHeaders: http.Header{
		settle.ResponseCostHeader: {"0.25"}, "Content-Type": {"application/json"},
	}}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "m", InputTokens: 1000, OutputTokens: -5}
	p.settleCost(pctx)
	ev, ok := publishedCost(t, pctx)
	if !ok || !ev.UsageRefused || ev.CostUSD != 0.25 {
		t.Fatalf("record = %+v ok=%v, want the header's $0.25 with the counts refused", ev, ok)
	}
	if acc.calls != 0 || len(ev.Avoided) != 0 {
		t.Errorf("accountant asked %d times, avoided = %+v on a response whose counters were refused", acc.calls, ev.Avoided)
	}
}
