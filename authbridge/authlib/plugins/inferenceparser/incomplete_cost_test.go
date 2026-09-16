package inferenceparser

import (
	"context"
	"net/http"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/costing"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
)

// End-to-end through the REAL parser: a truncated Anthropic stream must publish its
// prompt cost AND disclose that the figure is not exact.
//
// Driven with actual wire frames rather than a hand-built extension, because the whole
// defect lives in how those frames land: message_start carries the prompt counts and
// asserts the Output presence bit, message_delta carries the output tally — and when the
// second never arrives, everything looks complete except the tally itself.

// anthropicStreamCtx is a streaming /v1/messages response with LiteLLM's placeholder
// zero cost header, which is what every streamed response really carries.
func anthropicStreamCtx() *pipeline.Context {
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	h.Set(costing.ResponseCostHeader, "0")
	pctx := &pipeline.Context{
		Direction:       pipeline.Outbound,
		Host:            "gw.internal",
		Path:            anthropicMessagesPath,
		ResponseHeaders: h,
		Extensions: pipeline.Extensions{Inference: &pipeline.InferenceExtension{
			Model:  "claude-opus-5",
			Stream: true,
		}},
	}
	pctx.SetCurrentPlugin("inference-parser", pipeline.InvocationPhaseResponse)
	return pctx
}

const anthropicMessageStart = `{"type":"message_start","message":{"usage":` +
	`{"input_tokens":1000,"cache_read_input_tokens":500}}}`

// TestTruncatedAnthropicStream_PublishesAFloorAndSaysSo is the bug 2 regression.
func TestTruncatedAnthropicStream_PublishesAFloorAndSaysSo(t *testing.T) {
	p := NewInferenceParser()
	p.SetPricingResolver(bodylessRates(t))
	pctx := anthropicStreamCtx()

	// message_start lands; the stream then dies. The terminal frame carries nothing.
	p.OnResponseFrame(context.Background(), pctx, []byte(anthropicMessageStart), false)
	p.OnResponseFrame(context.Background(), pctx, nil, true)

	ext := pctx.Extensions.Inference
	// Precondition: this is the shape the bug needs. Prompt counted, output not, no stop
	// reason — and the Output presence BIT set anyway, which is what makes the mask
	// useless here and the whole reason the stop reason is the discriminator.
	if ext.PromptTokens != 1500 {
		t.Fatalf("PromptTokens = %d, want 1500; the fixture is not reproducing the truncated stream", ext.PromptTokens)
	}
	if ext.OutputTokens != 0 || ext.FinishReason != "" {
		t.Fatalf("output = %d / finishReason = %q; the fixture is not truncated", ext.OutputTokens, ext.FinishReason)
	}
	if ext.PresentKinds&8 == 0 {
		t.Fatal("Output presence bit is unset, so this no longer reproduces the case a bitmask cannot see")
	}

	ev, ok := publishedCost(t, pctx)
	if !ok {
		t.Fatal("no cost record published; the prompt cost is real and must not be dropped")
	}
	// 1500 prompt tokens at 1e-6 = $0.0015. The figure is NOT adjusted.
	if ev.CostUSD != 0.0015 {
		t.Errorf("CostUSD = %v, want 0.0015 (1000 input + 500 cache-read at 1e-6)", ev.CostUSD)
	}
	if !ev.Incomplete {
		t.Error("Incomplete = false: this is a floor published as a total, which is the bug")
	}
	if ev.IncompleteReason != pricing.ReasonOutputUncounted {
		t.Errorf("IncompleteReason = %q, want %q", ev.IncompleteReason, pricing.ReasonOutputUncounted)
	}
	if ev.Source != costevent.SourceUsageFallback {
		t.Errorf("Source = %q, want %q (a stream's zero header is a placeholder)", ev.Source, costevent.SourceUsageFallback)
	}
	// Still settled and still priced, so the dollars reach every total they belong in.
	if !ev.Settled || !ev.Priced() {
		t.Errorf("Settled = %v / Priced() = %v, want both true", ev.Settled, ev.Priced())
	}
}

// The contrast, on the same path with one more frame: a stream that reaches
// message_delta carries its output tally and a stop reason, and must be published with
// no caveat at all.
func TestCompleteAnthropicStream_CarriesNoCaveat(t *testing.T) {
	p := NewInferenceParser()
	p.SetPricingResolver(bodylessRates(t))
	pctx := anthropicStreamCtx()

	p.OnResponseFrame(context.Background(), pctx, []byte(anthropicMessageStart), false)
	p.OnResponseFrame(context.Background(), pctx, []byte(
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":240}}`), false)
	p.OnResponseFrame(context.Background(), pctx, nil, true)

	ext := pctx.Extensions.Inference
	if ext.OutputTokens != 240 || ext.FinishReason != "end_turn" {
		t.Fatalf("output = %d / finishReason = %q; the fixture is not a complete stream", ext.OutputTokens, ext.FinishReason)
	}
	ev, ok := publishedCost(t, pctx)
	if !ok {
		t.Fatal("no cost record published")
	}
	if ev.Incomplete {
		t.Errorf("Incomplete = true (%q) for a stream that reported its output; a false caveat is a permanent warning with nothing to act on", ev.IncompleteReason)
	}
	// 1500 prompt + 240 output at 1e-6.
	if ev.CostUSD != 0.00174 {
		t.Errorf("CostUSD = %v, want 0.00174", ev.CostUSD)
	}
}

// A stream that dies before ANY usage arrives must not be disclosed as a floor: there is
// no figure to be a lower bound of. It takes the no_response_body path instead, which the
// body-less-cost fix now routes through settleCost — so this asserts the two changes
// compose rather than colliding.
func TestStreamDyingBeforeAnyUsage_IsNotAFloor(t *testing.T) {
	p := NewInferenceParser()
	p.SetPricingResolver(bodylessRates(t))
	pctx := anthropicStreamCtx()

	p.OnResponseFrame(context.Background(), pctx, nil, true)

	if pctx.Extensions.Inference.TotalTokens != 0 {
		t.Fatalf("TotalTokens = %d, want 0; the fixture carried usage after all", pctx.Extensions.Inference.TotalTokens)
	}
	if ev, ok := publishedCost(t, pctx); ok {
		t.Errorf("published %+v; with no counters and no cost header there is nothing to say, and a caveat on nothing would count unpriced traffic as partially priced", ev)
	}
	if n := skipRows(pctx); n != 1 {
		t.Errorf("no_response_body Skip rows = %d, want 1", n)
	}
}
