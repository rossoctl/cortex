package inferenceparser

import (
	"context"
	"net/http"
	"testing"

	"github.com/rossoctl/cortex/core/costevent"
	"github.com/rossoctl/cortex/core/costing"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/pricing"
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

// The claim "a stream that dies before ANY usage publishes nothing" lives in
// bodyless_cost_test.go as TestBodylessResponse_StreamPlaceholderZeroPublishesNothing. It was
// written twice: anthropicStreamCtx() and bodylessCtx("0", true) with an SSE content type are the
// same context, driven the same way, asserting the same thing — and the surviving copy sits in the
// family whose subject is exactly that Content-Type discrimination.

// openAIStreamCtx is a text/event-stream response to a /v1/chat/completions request, with
// LiteLLM's placeholder zero cost header. reqStream is what the REQUEST asked for, which the
// rows below vary independently of what arrived.
func openAIStreamCtx(contentType string, reqStream bool) *pipeline.Context {
	h := http.Header{}
	h.Set("Content-Type", contentType)
	h.Set(costing.ResponseCostHeader, "0")
	pctx := &pipeline.Context{
		Direction:       pipeline.Outbound,
		Host:            "gw.internal",
		Path:            "/v1/chat/completions",
		ResponseHeaders: h,
		Extensions: pipeline.Extensions{Inference: &pipeline.InferenceExtension{
			Model:  "claude-opus-5",
			Stream: reqStream,
		}},
	}
	pctx.SetCurrentPlugin("inference-parser", pipeline.InvocationPhaseResponse)
	return pctx
}

// A truncated OpenAI stream: a usage-bearing chunk with a RUNNING total, and no finish_reason
// because the stream died before one arrived.
const openAITruncatedSSE = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
	"data: {\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":500,\"total_tokens\":1500}}\n\n"

// The same stream reaching its end: the last chunk carries finish_reason, so the tally is final.
const openAICompleteSSE = openAITruncatedSSE +
	"data: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":" +
	"{\"prompt_tokens\":1000,\"completion_tokens\":500,\"total_tokens\":1500}}\n\ndata: [DONE]\n\n"

// TestTruncatedStream_IsFlaggedWhicheverWayItWasDelivERED is the parity the caveat depends on.
//
// pricing.IncompleteReason asks whether the RESPONSE arrived as a stream, because a counted output
// on a stream is a running total and on a whole body is final. Only the per-frame path answered:
// StreamedResponse is set by getOrCreateStreamState, which a buffered dispatch never reaches — so
// the SAME BYTES were flagged as a floor when delivered frame by frame and published as an exact
// figure when delivered whole. Both proxies buffer an event-stream response whenever a plugin in
// the chain declares WritesResponseBody, so the unflagged path is the one a rewriting chain takes.
//
// The last two rows are why the mark is taken from the BYTES rather than from "an SSE parser ran":
// OnResponse picks its parser from the request's Stream flag, and a JSON envelope answered to a
// streaming request is routine. Marking that would print "+ partial" over a figure that is exact.
func TestTruncatedStream_IsFlaggedWhicheverWayItWasDelivered(t *testing.T) {
	for _, tc := range []struct {
		name         string
		body         string
		contentType  string
		reqStream    bool
		perFrame     bool
		wantStreamed bool
		wantReason   string
		// noCounters marks the row where the body parses to nothing, so the mark is the only
		// observable there.
		noCounters bool
	}{
		{
			name:         "truncated stream buffered whole",
			body:         openAITruncatedSSE,
			contentType:  "text/event-stream",
			wantStreamed: true,
			wantReason:   pricing.ReasonOutputUncounted,
		},
		{
			name:         "truncated stream frame by frame",
			body:         openAITruncatedSSE,
			contentType:  "text/event-stream",
			perFrame:     true,
			wantStreamed: true,
			wantReason:   pricing.ReasonOutputUncounted,
		},
		{
			name:         "complete stream buffered whole carries no caveat",
			body:         openAICompleteSSE,
			contentType:  "text/event-stream",
			wantStreamed: true,
			wantReason:   "",
		},
		{
			// A JSON envelope on the request-flag arm, which is what every gateway error page is.
			// The SSE parser reads no `data:` line here so nothing is counted — measured, and the
			// pre-existing behaviour this arm's own comment describes — which is precisely why the
			// MARK is what this row asserts: it must stay false on bytes that never streamed, or the
			// caveat would ride on whatever a later parser did manage to count.
			name:         "json envelope answered to a streaming request",
			body:         `{"usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500}}`,
			contentType:  "application/json",
			reqStream:    true,
			wantStreamed: false,
			wantReason:   "",
			noCounters:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewInferenceParser()
			p.SetPricingResolver(bodylessRates(t))
			pctx := openAIStreamCtx(tc.contentType, tc.reqStream)

			if tc.perFrame {
				// sseframe strips the framing, so each frame is a bare payload.
				for _, frame := range []string{
					`{"choices":[{"delta":{"content":"hi"}}]}`,
					`{"usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500}}`,
				} {
					p.OnResponseFrame(context.Background(), pctx, []byte(frame), false)
				}
				p.OnResponseFrame(context.Background(), pctx, nil, true)
			} else if tc.reqStream && tc.contentType == "application/json" {
				pctx.ResponseBody = []byte(tc.body)
				p.OnResponse(context.Background(), pctx)
			} else {
				// One terminal frame carrying the whole wire body, framing intact.
				p.OnResponseFrame(context.Background(), pctx, []byte(tc.body), true)
			}

			ext := pctx.Extensions.Inference
			if ext.StreamedResponse != tc.wantStreamed {
				t.Errorf("StreamedResponse = %v, want %v: this is the fact the caveat is derived from",
					ext.StreamedResponse, tc.wantStreamed)
			}
			if tc.noCounters {
				if ext.OutputTokens != 0 || ext.CompletionTokens != 0 {
					t.Fatalf("output = %d/%d counted; this row is the one where nothing parses, so it no longer says what it claims",
						ext.OutputTokens, ext.CompletionTokens)
				}
				return
			}
			// Precondition: every other row must reach the branch under test with a counted output.
			if ext.OutputTokens == 0 && ext.CompletionTokens == 0 {
				t.Fatalf("no output counted; the fixture does not reach the branch under test")
			}
			ev, ok := publishedCost(t, pctx)
			if !ok {
				t.Fatal("no cost record published")
			}
			if ev.IncompleteReason != tc.wantReason {
				t.Errorf("IncompleteReason = %q, want %q", ev.IncompleteReason, tc.wantReason)
			}
			if ev.Incomplete != (tc.wantReason != "") {
				t.Errorf("Incomplete = %v with reason %q", ev.Incomplete, ev.IncompleteReason)
			}
			// The dollars stand either way — a floor is still money owed.
			if !ev.Priced() {
				t.Errorf("Priced() = false: the caveat qualifies the figure, it does not withdraw it")
			}
		})
	}
}
