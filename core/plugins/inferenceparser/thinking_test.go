package inferenceparser

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// The thinking and effort a request asked for are recorded, so a reader can tell what a turn
// was sent with: Claude Code's display setting, the effort level, and the beta flags.
func TestAnthropicRequestRecordsThinkingAndEffort(t *testing.T) {
	ext := parseAnthropicRequest([]byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}],
		"thinking":{"type":"adaptive","display":"summarized"},"output_config":{"effort":"high"}}`))
	if ext == nil {
		t.Fatal("request not parsed")
	}
	if ext.ThinkingType != "adaptive" || ext.ThinkingDisplay != "summarized" || ext.Effort != "high" {
		t.Fatalf("thinking = %q/%q, effort = %q, want adaptive/summarized, high",
			ext.ThinkingType, ext.ThinkingDisplay, ext.Effort)
	}
}

// Each dialect names effort in its own field; all three land in Effort.
func TestOpenAIDialectsRecordEffort(t *testing.T) {
	chat := parseOpenAIRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"medium"}`))
	if chat == nil || chat.Effort != "medium" {
		t.Fatalf("chat completions Effort = %+v, want medium", chat)
	}
	resp := parseResponsesRequest(nil, []byte(`{"model":"m","input":"hi","reasoning":{"effort":"low"}}`))
	if resp == nil || resp.Effort != "low" {
		t.Fatalf("responses Effort = %+v, want low", resp)
	}
}

// The new fields are recorded for display only. A value of an unexpected JSON type must not
// fail the decode of the whole request — every one of these bodies parsed before the fields
// were read — and leaves only that field empty.
func TestThinkingFieldsOfAnUnexpectedTypeKeepTheRequestParsed(t *testing.T) {
	for name, body := range map[string]string{
		"thinking is a string":    `{"model":"m","messages":[{"role":"user","content":"hi"}],"thinking":"x"}`,
		"display is a number":     `{"model":"m","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"adaptive","display":3}}`,
		"output_config is a list": `{"model":"m","messages":[{"role":"user","content":"hi"}],"output_config":[1]}`,
		"effort is a number":      `{"model":"m","messages":[{"role":"user","content":"hi"}],"output_config":{"effort":5}}`,
		"signature is a number":   `{"model":"m","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":123},{"type":"text","text":"ok"}]}]}`,
		"thinking text is a list": `{"model":"m","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":[1],"signature":"s"},{"type":"text","text":"ok"}]}]}`,
		"redacted data is a bool": `{"model":"m","messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":true},{"type":"text","text":"ok"}]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			ext := parseAnthropicRequest([]byte(body))
			if ext == nil {
				t.Fatal("request not parsed")
			}
			if len(ext.Messages) != 1 {
				t.Fatalf("Messages = %+v, want the one message", ext.Messages)
			}
		})
	}
	chat := parseOpenAIRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":{"x":1}}`))
	if chat == nil || chat.Effort != "" {
		t.Fatalf("chat with object reasoning_effort = %+v, want parsed with Effort empty", chat)
	}
	resp := parseResponsesRequest(nil, []byte(`{"model":"m","input":"hi","reasoning":"high"}`))
	if resp == nil || resp.Effort != "" {
		t.Fatalf("responses with string reasoning = %+v, want parsed with Effort empty", resp)
	}
}

// The thinking blocks a client sends back are counted, with how much of each is text and how
// much is signature. A block that arrives with empty text says the reasoning came back
// encrypted only, which is what a gateway that drops empty blocks needs to be seen against.
func TestAnthropicRequestCountsHistoryThinking(t *testing.T) {
	ext := parseAnthropicRequest([]byte(`{"model":"m","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[
			{"type":"thinking","thinking":"","signature":"abcd"},
			{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]},
		{"role":"assistant","content":[
			{"type":"thinking","thinking":"plan","signature":"ef"},
			{"type":"redacted_thinking","data":"xyz"},
			{"type":"text","text":"done"}]}]}`))
	if ext == nil {
		t.Fatal("request not parsed")
	}
	want := pipeline.InferenceThinking{Blocks: 3, Redacted: 1, TextBytes: 4, SignatureBytes: 6, RedactedBytes: 3}
	if got := ext.HistoryThinking; got == nil || *got != want {
		t.Fatalf("HistoryThinking = %+v, want %+v", got, want)
	}
	// The thinking blocks add no text to the message they sit in.
	if ext.Messages[3].Content != "done" {
		t.Fatalf("Messages[3].Content = %q, want done", ext.Messages[3].Content)
	}
}

// A request with no thinking blocks states nothing, rather than a zero count.
func TestAnthropicRequestWithoutThinkingLeavesHistoryThinkingNil(t *testing.T) {
	ext := parseAnthropicRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if ext == nil || ext.HistoryThinking != nil {
		t.Fatalf("HistoryThinking = %+v, want nil", ext)
	}
}

// The anthropic-beta header is recorded with every value it carried, cleaned of control
// characters and capped.
func TestOnRequestRecordsAnthropicBeta(t *testing.T) {
	p := NewInferenceParser()
	h := http.Header{}
	h.Add("Anthropic-Beta", "interleaved-thinking-2025-05-14")
	h.Add("Anthropic-Beta", "context-1m\x1b[2J-2025-08-07")
	pctx := &pipeline.Context{Path: "/v1/messages", Headers: h,
		Body: []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)}
	p.OnRequest(context.Background(), pctx)
	ext := pctx.Extensions.Inference
	if ext == nil {
		t.Fatal("request not parsed")
	}
	if want := "interleaved-thinking-2025-05-14,context-1m\uFFFD[2J-2025-08-07"; ext.AnthropicBeta != want {
		t.Fatalf("AnthropicBeta = %q, want %q", ext.AnthropicBeta, want)
	}

	long := http.Header{}
	long.Set("Anthropic-Beta", strings.Repeat("a", 1000))
	pctx = &pipeline.Context{Path: "/v1/messages", Headers: long,
		Body: []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)}
	p.OnRequest(context.Background(), pctx)
	if got := len(pctx.Extensions.Inference.AnthropicBeta); got != maxServedModelLen {
		t.Fatalf("AnthropicBeta length = %d, want the cap %d", got, maxServedModelLen)
	}
}

// A buffered response counts its thinking blocks the same way.
func TestAnthropicBufferedResponseCountsThinking(t *testing.T) {
	ext := &pipeline.InferenceExtension{}
	parseAnthropicJSON([]byte(`{"model":"m","stop_reason":"end_turn","content":[
		{"type":"thinking","thinking":"","signature":"sig1"},
		{"type":"redacted_thinking","data":"zz"},
		{"type":"text","text":"answer"}],"usage":{"input_tokens":1,"output_tokens":2}}`), ext)
	want := pipeline.InferenceThinking{Blocks: 2, Redacted: 1, SignatureBytes: 4, RedactedBytes: 2}
	if got := ext.ResponseThinking; got == nil || *got != want {
		t.Fatalf("ResponseThinking = %+v, want %+v", got, want)
	}
	if ext.Completion != "answer" {
		t.Fatalf("Completion = %q, want answer", ext.Completion)
	}
}

// A streamed response builds the same counts from its frames: the block opens on
// content_block_start, its text and signature arrive as deltas, and a redacted block arrives
// whole on its start. Finalizing twice — the buffered hook after the streaming one — must not
// count anything twice.
func TestAnthropicStreamCountsThinking(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{Path: "/v1/messages"}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "m", Stream: true, IsAction: true}
	frames := []string{
		`{"type":"message_start","message":{"model":"m","usage":{"input_tokens":5,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"ab"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"c"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"SIGNATURE"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"RRRR"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"hi"}}`,
		`{"type":"content_block_stop","index":2}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`,
		`{"type":"message_stop"}`,
	}
	for _, f := range frames {
		p.OnResponseFrame(context.Background(), pctx, []byte(f), false)
	}
	p.OnResponseFrame(context.Background(), pctx, nil, true)
	p.OnResponseFrame(context.Background(), pctx, nil, true)
	ext := pctx.Extensions.Inference
	want := pipeline.InferenceThinking{Blocks: 2, Redacted: 1, TextBytes: 3, SignatureBytes: 9, RedactedBytes: 4}
	if got := ext.ResponseThinking; got == nil || *got != want {
		t.Fatalf("ResponseThinking = %+v, want %+v", got, want)
	}
	if ext.Completion != "hi" {
		t.Fatalf("Completion = %q, want hi (thinking text must not reach the completion)", ext.Completion)
	}
}

// A stream with no thinking states nothing, rather than a zero count.
func TestAnthropicStreamWithoutThinkingLeavesResponseThinkingNil(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{Path: "/v1/messages"}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "m", Stream: true, IsAction: true}
	for _, f := range []string{
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
	} {
		p.OnResponseFrame(context.Background(), pctx, []byte(f), false)
	}
	p.OnResponseFrame(context.Background(), pctx, nil, true)
	if got := pctx.Extensions.Inference.ResponseThinking; got != nil {
		t.Fatalf("ResponseThinking = %+v, want nil", got)
	}
}
