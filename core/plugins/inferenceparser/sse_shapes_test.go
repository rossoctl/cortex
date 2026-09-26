package inferenceparser

import (
	"context"
	"net/http"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// A BUFFERED SSE BODY IS STILL SSE WITH A BOM, OR WITH CR-ONLY LINE ENDINGS.
//
// The event-stream format allows CRLF, LF or CR as the terminator and requires a decoder to strip one
// leading byte-order mark. Without normalisation both shapes can fall through to the JSON arm, where
// unmarshalling fails and usage stays unset — and with no cost header there is nothing else to settle
// from, so the request goes unpriced.
//
// HOW MUCH EACH ONE COSTS IS NOT THE SAME, and the BOM half is narrower than it reads. A BOM defeats
// the `data:` prefix check on the FIRST line only, so it matters for a body whose first line is that
// prefix — see the single-frame row below. Any body with a later `data:` line, which includes every
// multi-frame `event:`-first stream Anthropic and LiteLLM send, is found by the "\ndata:" fallback
// whatever precedes it. The CR-only half is the broad one: a CR-only body contains no "\ndata:" at all,
// so nothing finds its framing.
//
// Driven through OnResponseFrame with the WHOLE body on a terminal frame, which is how a
// reverse proxy hands a buffered event-stream response to the parser.
func TestOnResponseFrame_BufferedSSEShapesStillYieldUsage(t *testing.T) {
	const (
		// The field order matters for the detection half: `event:` first means the `data:`
		// prefix check cannot match, so only the line-ending-aware scan finds the frame.
		openAIFrames = "event: chunk%[1]sdata: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]," +
			"\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":500,\"total_tokens\":1500}}%[1]s" +
			"data: [DONE]%[1]s"
	)

	// One frame carrying the same usage, for the BOM row below.
	const singleUsageFrame = "data: {\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":500," +
		"\"total_tokens\":1500}}\n\n"

	for _, tc := range []struct {
		name string
		body string
	}{
		{"LF, the ordinary shape", sseBody(openAIFrames, "\n\n")},
		// EXERCISE, NOT DISCRIMINATION, and worth saying so: both parsers TrimSpace each line, so a
		// trailing \r needs no normalisation and this row passes with normalizeSSE's CRLF branch
		// removed. It stays as regression coverage for the shape; the rows that DISCRIMINATE are
		// the CR-only and BOM ones below.
		{"CRLF", sseBody(openAIFrames, "\r\n\r\n")},
		// CR-only, which no "\ndata:" scan can see.
		{"CR only", sseBody(openAIFrames, "\r\r")},
		// One leading BOM, which " \t\r\n" trimming does not remove.
		{"BOM then LF", "\xef\xbb\xbf" + sseBody(openAIFrames, "\n\n")},
		{"BOM then CR only", "\xef\xbb\xbf" + sseBody(openAIFrames, "\r\r")},
		// A BOM before the ONLY frame, which is where stripping it is load-bearing: with a
		// second line present the "\ndata:" fallback finds the framing whatever precedes the
		// first line, so a multi-frame BOM body passes even unstripped. One frame, and the
		// BOM is the only thing between the detector and a `data:` prefix.
		{"BOM before the only frame", "\xef\xbb\xbf" + singleUsageFrame},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewInferenceParser()
			pctx := &pipeline.Context{
				Path: "/v1/chat/completions",
				Extensions: pipeline.Extensions{
					Inference: &pipeline.InferenceExtension{Model: "claude-opus-5", Stream: true},
				},
			}
			// The response Content-Type is what selects the SSE arm — the request's stream
			// flag deliberately does not (see OnResponseFrame). Without it every row here
			// would take the buffered-JSON path and the test would assert nothing about
			// framing.
			pctx.ResponseHeaders = http.Header{"Content-Type": []string{"text/event-stream"}}

			p.OnResponseFrame(context.Background(), pctx, []byte(tc.body), true)

			ext := pctx.Extensions.Inference
			if ext.TotalTokens != 1500 {
				t.Errorf("TotalTokens = %d, want 1500: this body is SSE, so it has to reach the SSE "+
					"parser rather than the JSON one — where it fails to unmarshal and the usage, "+
					"and any figure modelled from it, are lost", ext.TotalTokens)
			}
			if ext.InputTokens != 1000 || ext.OutputTokens != 500 {
				t.Errorf("split = %d input / %d output, want 1000 / 500", ext.InputTokens, ext.OutputTokens)
			}
		})
	}
}

// sseBody renders the frame template with the given line terminator.
func sseBody(template, sep string) string {
	out := ""
	for _, part := range splitTemplate(template) {
		out += part + sep
	}
	return out
}

// splitTemplate cuts the template on its %[1]s placeholders, which is where a terminator goes.
func splitTemplate(template string) []string {
	var parts []string
	rest := template
	for {
		i := indexOf(rest, "%[1]s")
		if i < 0 {
			if rest != "" {
				parts = append(parts, rest)
			}
			return parts
		}
		parts = append(parts, rest[:i])
		rest = rest[i+len("%[1]s"):]
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
