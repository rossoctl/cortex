package settle

import (
	"net/http"
	"testing"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/pipeline"
)

// truncatedCtx is a response whose prompt counts landed and whose output count never
// arrived — an Anthropic stream that died before message_delta.
//
// text/event-stream with the placeholder zero LiteLLM stamps on every stream, which is
// the shape this really has in production: the zero is not an answer, so Settle falls
// back to the table, and the table can only price the half it was given.
//
// PresentKinds carries the Output bit with a zero tally, exactly as a real truncated
// Anthropic stream does, so nothing here can pass by reading the mask.
func truncatedCtx() *pipeline.Context {
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	h.Set(ResponseCostHeader, "0")
	return &pipeline.Context{
		Host:            "gw.internal",
		ResponseHeaders: h,
		Extensions: pipeline.Extensions{Inference: &pipeline.InferenceExtension{
			Model:           "claude-opus-5",
			InputTokens:     1000,
			CacheReadTokens: 0,
			PromptTokens:    1000,
			PresentKinds:    1 | 8, // KindInput | KindOutput, as Anthropic always asserts
		}},
	}
}

// A truncated stream is priced, and disclosed as INEXACT. Before this it was priced and
// published as an exact figure, so the aggregate counted a lower bound in CostMicros
// with nothing anywhere saying so, and a budget was enforced against it.
func TestSettle_TruncatedStreamIsDisclosedInexact(t *testing.T) {
	s := Settle(truncatedCtx(), rates(t))
	if !s.Priced {
		t.Fatal("Priced = false; the prompt cost is real and known, and refusing to price it understates spend by more than disclosing a floor does")
	}
	if !s.Incomplete {
		t.Error("Incomplete = false; a prompt-only figure for a response that should have had output is not an exact total")
	}
	if s.IncompleteReason != pricing.ReasonOutputUncounted {
		t.Errorf("IncompleteReason = %q, want %q", s.IncompleteReason, pricing.ReasonOutputUncounted)
	}
	if s.Source != event.SourceUsageFallback {
		t.Errorf("Source = %q, want %q", s.Source, event.SourceUsageFallback)
	}
	// DISCLOSURE, not adjustment: the figure itself is untouched. Estimating the missing
	// completion would be worse than reporting a known-low number and saying it is low.
	//
	// Priced at the INPUT tier's rate, not at a flat per-token one — the fixture's 1,000
	// tokens are uncached input, and a floor charged off the wrong tier would be a second
	// error hiding inside a disclosed one.
	if want := fallbackUSD(1000, 0); s.CostUSD != want {
		t.Errorf("CostUSD = %v, want %v (1000 uncached prompt tokens at the input rate) — the figure must not be adjusted", s.CostUSD, want)
	}
}

// A gateway figure is exact by assertion: the header states what the call charged
// whatever our counters saw. Inferring incompleteness from a token tally would attach a
// caveat to the one authoritative number in the system.
func TestSettle_GatewayFigureIsNeverIncomplete(t *testing.T) {
	pctx := truncatedCtx()
	pctx.ResponseHeaders.Set("Content-Type", "application/json")
	pctx.ResponseHeaders.Set(ResponseCostHeader, "0.25")

	s := Settle(pctx, rates(t))
	if s.CostUSD != 0.25 || s.Source != event.SourceGatewayHeader {
		t.Fatalf("expected the gateway figure to win, got %v from %q", s.CostUSD, s.Source)
	}
	if s.Incomplete {
		t.Errorf("Incomplete = true (%q) on a gateway-reported cost; exactness there is the gateway's assertion, not an inference from our token tallies", s.IncompleteReason)
	}
}

// A declared-free zero reaches the SAME SourceGatewayHeader arm as a positive figure,
// which is why Settle gates on the source rather than merely on "the header was not
// positive". Without that, a free call with truncated counters would be disclosed as a
// floor of zero — a lower bound on nothing.
func TestSettle_DeclaredFreeIsNeverIncomplete(t *testing.T) {
	pctx := truncatedCtx()
	pctx.ResponseHeaders.Set("Content-Type", "application/json") // not a stream: the zero IS an answer
	s := Settle(pctx, rates(t))
	if !s.DeclaredFree {
		t.Fatalf("fixture is not a declared-free call (source %q); the rest of this test proves nothing", s.Source)
	}
	if s.Incomplete {
		t.Errorf("Incomplete = true (%q) on a declared-free call; the gateway said it charged nothing, which is an exact total", s.IncompleteReason)
	}
}

// A complete stream must not be flagged. A false caveat is a permanent warning with
// nothing to act on, which is how an operator learns to ignore the real one.
func TestSettle_CompleteStreamIsNotIncomplete(t *testing.T) {
	pctx := truncatedCtx()
	inf := pctx.Extensions.Inference
	inf.OutputTokens, inf.CompletionTokens, inf.FinishReason = 500, 500, "end_turn"

	s := Settle(pctx, rates(t))
	if !s.Priced {
		t.Fatal("Priced = false")
	}
	if s.Incomplete {
		t.Errorf("Incomplete = true (%q) for a stream that reported its output", s.IncompleteReason)
	}
}

// A stream that dies before ANY usage arrives never reaches the flag, and this pins the
// "must have a prompt-side count" half of the predicate rather than leaving it
// incidental.
//
// Two independent guards stop it, which is why it is worth asserting at this level: the
// parser's own no_response_body guard returns because TotalTokens is 0, and even when
// settleCost IS reached — which it now is on that path, since the body-less-cost fix
// calls it before returning — Settle finds no usage to price, so Priced is false and the
// disclosure gate never opens. There is no figure here to be a lower bound OF.
func TestSettle_NoUsageAtAllIsUnpricedNotIncomplete(t *testing.T) {
	pctx := truncatedCtx()
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "claude-opus-5"}

	s := Settle(pctx, rates(t))
	if s.Priced {
		t.Error("Priced = true with no counters at all; unknown usage is not a free request")
	}
	if s.Incomplete {
		t.Errorf("Incomplete = true (%q); with no figure at all there is nothing to qualify, and publishing a caveat on nothing would count unpriced traffic as partially priced", s.IncompleteReason)
	}
}

// The disclosure has to survive onto the wire record, because the aggregate is where the
// totals are read. A Settled.Incomplete that NewRecord dropped would be knowledge that
// reaches nothing — which is exactly the state this fix found the parser's "token counts
// will be incomplete" log line in.
func TestNewRecord_CarriesIncomplete(t *testing.T) {
	s := Settle(truncatedCtx(), rates(t))
	ev := NewRecord(s, nil)
	if !ev.Incomplete || ev.IncompleteReason != pricing.ReasonOutputUncounted {
		t.Errorf("Event.Incomplete/%q = %v/%q; the distinction did not reach the wire", pricing.ReasonOutputUncounted, ev.Incomplete, ev.IncompleteReason)
	}
	// Settled and Priced() must both stand. Un-settling a floor would send a consumer's
	// own fallback down a rate table to recompute the identical prompt-only figure and
	// label THAT one exact — strictly worse than disclosing this one.
	if !ev.Settled {
		t.Error("Event.Settled = false; a floor is a settled figure")
	}
	if !ev.Priced() {
		t.Error("Priced() = false; a consumer must keep these dollars in its total")
	}
	if ev.CostUSD != s.CostUSD {
		t.Errorf("CostUSD = %v, want %v unchanged", ev.CostUSD, s.CostUSD)
	}
}
