package inferenceparser

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/rossoctl/cortex/core/pipeline"
)

func TestInferenceParser_ResponsesAPI_Request(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/backend-api/codex/responses",
		Body: []byte(`{
			"model": "gpt-6-luna",
			"stream": true,
			"tool_choice": "auto",
			"input": [
				{
					"type": "additional_tools",
					"id": "at_1",
					"role": "developer",
					"tools": [
						{
							"type": "namespace",
							"name": "functions",
							"description": "",
							"tools": [
								{"type": "custom", "name": "exec", "description": "run code"},
								{"type": "function", "name": "wait", "description": "wait for output",
									"parameters": {"type": "object", "properties": {}}}
							]
						}
					]
				},
				{
					"type": "message",
					"id": "msg_1",
					"role": "developer",
					"content": [{"type": "input_text", "text": "You are Codex."}]
				},
				{
					"type": "message",
					"id": "msg_2",
					"role": "user",
					"content": [{"type": "input_text", "text": "reply with just the word hello"}]
				}
			]
		}`),
	}

	if action := p.OnRequest(context.Background(), pctx); action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	ext := pctx.Extensions.Inference
	if ext == nil {
		t.Fatal("Extensions.Inference is nil — /backend-api/codex/responses not parsed")
	}
	if ext.Model != "gpt-6-luna" {
		t.Errorf("Model = %q, want gpt-6-luna", ext.Model)
	}
	if !ext.IsAction {
		t.Error("IsAction should be true for an inference request")
	}
	// Only "message" items become Messages — the "additional_tools" item is a
	// tool manifest, not a conversation turn.
	if len(ext.Messages) != 2 || ext.Messages[0].Role != "developer" || ext.Messages[1].Role != "user" {
		t.Fatalf("Messages = %+v, want [developer, user]", ext.Messages)
	}
	if ext.Messages[1].Content != "reply with just the word hello" {
		t.Errorf("user content = %q", ext.Messages[1].Content)
	}
	// "exec" has no parameters object (a custom code-exec tool) and is still
	// surfaced; "wait" carries a real schema.
	if len(ext.Tools) != 2 || ext.Tools[0].Name != "exec" || ext.Tools[1].Name != "wait" {
		t.Fatalf("Tools = %+v, want [exec, wait]", ext.Tools)
	}
	if ext.Tools[0].Parameters != "" {
		t.Errorf("exec Parameters = %q, want empty (no schema on the wire)", ext.Tools[0].Parameters)
	}
	if ext.Tools[1].Parameters == "" {
		t.Error("wait Parameters is empty, want its object schema")
	}
}

// A request whose path ends in "/responses" but whose body carries no input array is not
// an inference request — the same backstop parseOpenAIRequest and parseAnthropicRequest
// have for their own loose suffix match.
func TestInferenceParser_ResponsesAPI_NonInferenceBodyIgnored(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/v1/responses",
		Body: []byte(`{"id": "resp_1", "status": "completed"}`),
	}
	p.OnRequest(context.Background(), pctx)
	if pctx.Extensions.Inference != nil {
		t.Errorf("Extensions.Inference = %+v, want nil for a body with no input array", pctx.Extensions.Inference)
	}
}

// Codex's real request arrives zstd-compressed. A corrupt or absent decompression step
// would leave json.Unmarshal failing on compressed bytes, so this pins decode-before-parse
// rather than relying on parseResponsesRequest's nil-on-bad-JSON fallback to mask it.
func TestInferenceParser_ResponsesAPI_ZstdCompressedRequest(t *testing.T) {
	plain := []byte(`{"model":"gpt-6-luna","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	compressed := enc.EncodeAll(plain, nil)
	if err := enc.Close(); err != nil {
		t.Fatalf("enc.Close: %v", err)
	}

	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path:    "/backend-api/codex/responses",
		Headers: http.Header{"Content-Encoding": []string{"zstd"}},
		Body:    compressed,
	}
	p.OnRequest(context.Background(), pctx)
	ext := pctx.Extensions.Inference
	if ext == nil {
		t.Fatal("Extensions.Inference is nil — zstd-compressed body was not decompressed before parsing")
	}
	if ext.Model != "gpt-6-luna" || len(ext.Messages) != 1 || ext.Messages[0].Content != "hi" {
		t.Errorf("ext = %+v, want model gpt-6-luna / one message \"hi\"", ext)
	}
}

// The real event sequence confirmed on live Codex traffic, sanitized to the "hello" fixture
// used throughout this capture. Usage figures match the real response.completed event:
// input_tokens=12862 (of which cached_tokens=11008), output_tokens=5, total_tokens=12867.
func TestInferenceParser_ResponsesAPI_StreamFoldsEvents(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{Path: "/backend-api/codex/responses"}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "gpt-6-luna", Stream: true, IsAction: true}

	frames := [][]byte{
		[]byte(`{"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`),
		[]byte(`{"type":"response.in_progress","response":{"id":"resp_1","status":"in_progress"}}`),
		[]byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message"}}`),
		[]byte(`{"type":"response.content_part.added","item_id":"msg_1","output_index":0,"content_index":0}`),
		[]byte(`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"hello"}`),
		[]byte(`{"type":"response.output_text.done","item_id":"msg_1","output_index":0,"content_index":0,"text":"hello"}`),
		[]byte(`{"type":"response.content_part.done","item_id":"msg_1","output_index":0,"content_index":0}`),
		[]byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_1","type":"message"}}`),
		[]byte(`{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{` +
			`"input_tokens":12862,"input_tokens_details":{"cached_tokens":11008,"cache_write_tokens":0},` +
			`"output_tokens":5,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":12867}}}`),
	}
	for _, f := range frames {
		if action := p.OnResponseFrame(context.Background(), pctx, f, false); action.Type != pipeline.Continue {
			t.Fatalf("frame action = %v, want Continue", action.Type)
		}
	}
	p.OnResponseFrame(context.Background(), pctx, nil, true)

	ext := pctx.Extensions.Inference
	if ext.Completion != "hello" {
		t.Errorf("Completion = %q, want hello", ext.Completion)
	}
	if ext.FinishReason != "completed" {
		t.Errorf("FinishReason = %q, want completed (the terminal event's own status)", ext.FinishReason)
	}
	// Input is reported MINUS the cached+cache-write subsets: 12862 - 11008 - 0 = 1854.
	if ext.InputTokens != 1854 {
		t.Errorf("InputTokens = %d, want 1854 (12862 - 11008 cached)", ext.InputTokens)
	}
	if ext.CacheReadTokens != 11008 {
		t.Errorf("CacheReadTokens = %d, want 11008", ext.CacheReadTokens)
	}
	if ext.CacheWriteTokens != 0 {
		t.Errorf("CacheWriteTokens = %d, want 0", ext.CacheWriteTokens)
	}
	if ext.OutputTokens != 5 {
		t.Errorf("OutputTokens = %d, want 5", ext.OutputTokens)
	}
	if ext.TotalTokens != 12867 {
		t.Errorf("TotalTokens = %d, want 12867 (the provider's own reported total)", ext.TotalTokens)
	}
}

// Codex's response arrives as real SSE wire bytes delivered whole on the terminal call
// (the buffered-fallback shape, same as any other listener-buffered SSE body) but under
// Content-Type: application/json rather than text/event-stream — the mislabeling
// confirmed on live traffic. settle.IsEventStream(pctx) reads that (wrong) header and
// says no; carriesSSEFraming(frame) reads the actual bytes and says yes. This pins that
// the fallback engages: parsing must not silently fail just because the header lied.
func TestInferenceParser_ResponsesAPI_BufferedFallback_MislabeledContentType(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{Path: "/backend-api/codex/responses"}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "gpt-6-luna", Stream: true, IsAction: true}
	// The listener clones the real (wrong) response header onto pctx before dispatch.
	pctx.ResponseHeaders = http.Header{"Content-Type": []string{"application/json"}}

	var body bytes.Buffer
	for _, line := range []string{
		`event: response.output_text.delta` + "\n" + `data: {"type":"response.output_text.delta","delta":"hello"}` + "\n\n",
		`event: response.completed` + "\n" + `data: {"type":"response.completed","response":{"status":"completed",` +
			`"usage":{"input_tokens":10,"output_tokens":1,"total_tokens":11}}}` + "\n\n",
	} {
		body.WriteString(line)
	}

	p.OnResponseFrame(context.Background(), pctx, body.Bytes(), true)

	ext := pctx.Extensions.Inference
	if ext.Completion != "hello" {
		t.Errorf("Completion = %q, want hello — carriesSSEFraming fallback did not engage", ext.Completion)
	}
	if ext.TotalTokens != 11 {
		t.Errorf("TotalTokens = %d, want 11", ext.TotalTokens)
	}
}

// A non-streaming (stream:false) Responses API reply, delivered as one plain JSON object
// — not SSE at all. Modeled from the published schema; not exercised by live Codex traffic
// (which always sends stream:true), so this only pins the shape this parser expects.
func TestInferenceParser_ResponsesAPI_NonStreamingResponse(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{Path: "/v1/responses"}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "gpt-6-luna", IsAction: true}

	body := []byte(`{
		"status": "completed",
		"output": [{"content": [{"type": "output_text", "text": "hello"}]}],
		"usage": {"input_tokens": 10, "output_tokens": 1, "total_tokens": 11}
	}`)
	p.OnResponseFrame(context.Background(), pctx, body, true)

	ext := pctx.Extensions.Inference
	if ext.Completion != "hello" {
		t.Errorf("Completion = %q, want hello", ext.Completion)
	}
	if ext.FinishReason != "completed" {
		t.Errorf("FinishReason = %q, want completed", ext.FinishReason)
	}
	if ext.TotalTokens != 11 {
		t.Errorf("TotalTokens = %d, want 11", ext.TotalTokens)
	}
}

// Codex always sends `input` as an array, but the published schema also allows a bare
// string as a shorthand for one user message — the common one-shot usage shown in
// OpenAI's own docs. Pins that this shorthand still produces telemetry instead of
// silently returning nil.
func TestInferenceParser_ResponsesAPI_StringInput(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/v1/responses",
		Body: []byte(`{"model": "gpt-6-luna", "input": "Tell me a joke"}`),
	}
	p.OnRequest(context.Background(), pctx)
	ext := pctx.Extensions.Inference
	if ext == nil {
		t.Fatal("Extensions.Inference is nil — a string-valued input must still parse")
	}
	if len(ext.Messages) != 1 || ext.Messages[0].Role != "user" || ext.Messages[0].Content != "Tell me a joke" {
		t.Errorf("Messages = %+v, want one user message \"Tell me a joke\"", ext.Messages)
	}
}

// zstd.NewReader defaults to a 64 GiB decoded-size limit — far beyond the 32 MiB wire-size
// cap the listener already enforces for the compressed body. Pins that a payload
// decompressing past maxDecodedRequestSize is rejected (treated as unreadable, same as any
// other body this parser can't use) rather than allowed to grow unbounded.
func TestInferenceParser_ResponsesAPI_ZstdDecodeSizeCapped(t *testing.T) {
	// A single repeated byte compresses to a tiny fraction of its decoded size, which is
	// exactly the "small wire body, huge decoded body" shape the cap defends against.
	huge := bytes.Repeat([]byte("a"), maxDecodedRequestSize+1)
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	compressed := enc.EncodeAll(huge, nil)
	if err := enc.Close(); err != nil {
		t.Fatalf("enc.Close: %v", err)
	}
	t.Logf("huge body: %d bytes decoded, %d bytes compressed", len(huge), len(compressed))

	out := maybeDecompressRequest(http.Header{"Content-Encoding": []string{"zstd"}}, compressed)
	if len(out) == len(huge) {
		t.Fatal("decompression succeeded past maxDecodedRequestSize — the cap did not apply")
	}
	// Rejected decompression falls back to the original (still-compressed) bytes per
	// maybeDecompressRequest's documented contract.
	if !bytes.Equal(out, compressed) {
		t.Error("expected the original compressed bytes back when the decoded size cap is exceeded")
	}
}

// response.incomplete and response.failed are documented terminal events, distinct from
// response.completed, that this parser must also fold usage/status from — a request that
// hits a token limit or fails outright must not silently report zero usage just because
// our one live sample happened to end cleanly.
func TestInferenceParser_ResponsesAPI_IncompleteEventFoldsUsage(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{Path: "/backend-api/codex/responses"}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "gpt-6-luna", Stream: true, IsAction: true}

	frames := [][]byte{
		[]byte(`{"type":"response.output_text.delta","delta":"partial"}`),
		[]byte(`{"type":"response.incomplete","response":{"status":"incomplete",` +
			`"usage":{"input_tokens":10,"output_tokens":1,"total_tokens":11}}}`),
	}
	for _, f := range frames {
		p.OnResponseFrame(context.Background(), pctx, f, false)
	}
	p.OnResponseFrame(context.Background(), pctx, nil, true)

	ext := pctx.Extensions.Inference
	if ext.Completion != "partial" {
		t.Errorf("Completion = %q, want partial", ext.Completion)
	}
	if ext.FinishReason != "incomplete" {
		t.Errorf("FinishReason = %q, want incomplete", ext.FinishReason)
	}
	if ext.TotalTokens != 11 {
		t.Errorf("TotalTokens = %d, want 11 — usage on response.incomplete must still be folded", ext.TotalTokens)
	}
}

// Confirmed on a live Codex turn that ran its `exec` tool: response.output_item.added
// announces the call_id/name first (no arguments yet), and the completed input arrives
// whole on the matching response.output_item.done — unlike Anthropic's tool_use blocks,
// which split id/name and arguments across separate events, this API restates id and name
// again on "done" rather than only filling in arguments. Also pins that seeing BOTH events
// for the same call_id does not double the entry: added creates one placeholder, done fills
// it in, finalize emits exactly one.
func TestInferenceParser_ResponsesAPI_StreamFoldsCustomToolCall(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{Path: "/backend-api/codex/responses"}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "gpt-6-luna", Stream: true, IsAction: true}

	frames := [][]byte{
		[]byte(`{"type":"response.output_text.delta","delta":"I'll inspect the directory."}`),
		[]byte(`{"type":"response.output_item.added","item":{"type":"custom_tool_call",` +
			`"call_id":"call_700a03208f0f45d595453da88151178d","name":"exec"}}`),
		[]byte(`{"type":"response.output_item.done","item":{"type":"custom_tool_call",` +
			`"call_id":"call_700a03208f0f45d595453da88151178d","name":"exec",` +
			`"input":"const r = await tools.exec_command({cmd:\"ls\"});\ntext(r.output);\n",` +
			`"status":"completed"}}`),
		[]byte(`{"type":"response.completed","response":{"status":"completed",` +
			`"usage":{"input_tokens":10,"output_tokens":1,"total_tokens":11}}}`),
	}
	for _, f := range frames {
		p.OnResponseFrame(context.Background(), pctx, f, false)
	}
	p.OnResponseFrame(context.Background(), pctx, nil, true)

	ext := pctx.Extensions.Inference
	if ext.Completion != "I'll inspect the directory." {
		t.Errorf("Completion = %q", ext.Completion)
	}
	if len(ext.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %+v, want exactly 1 — added+done for the same call_id must not double count", ext.ToolCalls)
	}
	tc := ext.ToolCalls[0]
	if tc.ID != "call_700a03208f0f45d595453da88151178d" || tc.Name != "exec" {
		t.Errorf("ToolCalls[0] = %+v, want id=call_700a03208f0f45d595453da88151178d name=exec", tc)
	}
	if !strings.Contains(tc.Arguments, "exec_command") {
		t.Errorf("Arguments = %q, want it to contain the exec_command call", tc.Arguments)
	}
}

// function_call is NOT confirmed on live traffic — Codex's one real tool call used the
// custom_tool_call shape instead (see the test above). This pins our handling of the
// documented function_call shape, including the added+done sequence and the resulting
// ID, so a regression here is caught even without a live sample to catch it; see
// responsesOutputItem's doc comment for the caveat.
func TestInferenceParser_ResponsesAPI_StreamFoldsFunctionCall(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{Path: "/v1/responses"}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "gpt-6-luna", Stream: true, IsAction: true}

	frames := [][]byte{
		[]byte(`{"type":"response.output_item.added","item":{"type":"function_call",` +
			`"call_id":"call_abc","name":"get_weather"}}`),
		[]byte(`{"type":"response.output_item.done","item":{"type":"function_call",` +
			`"call_id":"call_abc","name":"get_weather","arguments":"{\"city\":\"NYC\"}"}}`),
		[]byte(`{"type":"response.completed","response":{"status":"completed"}}`),
	}
	for _, f := range frames {
		p.OnResponseFrame(context.Background(), pctx, f, false)
	}
	p.OnResponseFrame(context.Background(), pctx, nil, true)

	ext := pctx.Extensions.Inference
	if len(ext.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %+v, want exactly 1", ext.ToolCalls)
	}
	tc := ext.ToolCalls[0]
	if tc.ID != "call_abc" || tc.Name != "get_weather" || tc.Arguments != `{"city":"NYC"}` {
		t.Errorf("ToolCalls[0] = %+v, want id=call_abc name=get_weather arguments={\"city\":\"NYC\"}", tc)
	}
}

// A stream that announces a call via output_item.added but is cut short before the matching
// output_item.done ever arrives — a client-cancelled Codex turn is the real-world case this
// models. Without tracking from "added", this call would be silently lost: finalize must
// still emit it, call_id and name known, arguments empty, rather than reporting no tool
// calls at all for a turn that clearly started one.
func TestInferenceParser_ResponsesAPI_StreamToolCallCancelledMidCall(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{Path: "/backend-api/codex/responses"}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "gpt-6-luna", Stream: true, IsAction: true}

	frames := [][]byte{
		[]byte(`{"type":"response.output_item.added","item":{"type":"custom_tool_call",` +
			`"call_id":"call_cancelled","name":"exec"}}`),
		// Stream ends here — no matching output_item.done, no response.completed.
	}
	for _, f := range frames {
		p.OnResponseFrame(context.Background(), pctx, f, false)
	}
	p.OnResponseFrame(context.Background(), pctx, nil, true)

	ext := pctx.Extensions.Inference
	if len(ext.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %+v, want exactly 1 — a cancelled call must still be reported", ext.ToolCalls)
	}
	tc := ext.ToolCalls[0]
	if tc.ID != "call_cancelled" || tc.Name != "exec" {
		t.Errorf("ToolCalls[0] = %+v, want id=call_cancelled name=exec", tc)
	}
	if tc.Arguments != "" {
		t.Errorf("Arguments = %q, want empty — done never arrived to fill it in", tc.Arguments)
	}
}

// response.output_item.done can mark an item "incomplete" (hit a limit) or "in_progress"
// (a cancelled turn's own partial snapshot) rather than "completed" — either way its
// Input/Arguments were cut off mid-write, and recording them would hand a consumer a call
// that looks complete but silently isn't. This pins that such a call is excluded from
// ToolCalls entirely, not recorded with truncated arguments.
func TestInferenceParser_ResponsesAPI_StreamToolCallIncompleteExcluded(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{Path: "/backend-api/codex/responses"}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "gpt-6-luna", Stream: true, IsAction: true}

	frames := [][]byte{
		[]byte(`{"type":"response.output_item.added","item":{"type":"custom_tool_call",` +
			`"call_id":"call_cut_short","name":"exec"}}`),
		[]byte(`{"type":"response.output_item.done","item":{"type":"custom_tool_call",` +
			`"call_id":"call_cut_short","name":"exec","input":"const r = await tools.ex",` +
			`"status":"incomplete"}}`),
		[]byte(`{"type":"response.incomplete","response":{"status":"incomplete"}}`),
	}
	for _, f := range frames {
		p.OnResponseFrame(context.Background(), pctx, f, false)
	}
	p.OnResponseFrame(context.Background(), pctx, nil, true)

	ext := pctx.Extensions.Inference
	if len(ext.ToolCalls) != 0 {
		t.Errorf("ToolCalls = %+v, want none — an incomplete item's cut-off arguments must not be recorded", ext.ToolCalls)
	}
}

// The non-streaming path reads output_item entries whole too, same as the streaming
// path's response.output_item.done — this pins that parseResponsesJSON extracts both the
// text and the tool call from the same output array, for both tool-call item types.
func TestInferenceParser_ResponsesAPI_NonStreamingToolCall(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{Path: "/v1/responses"}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "gpt-6-luna", IsAction: true}

	body := []byte(`{
		"status": "completed",
		"output": [
			{"type": "message", "content": [{"type": "output_text", "text": "checking"}]},
			{"type": "custom_tool_call", "call_id": "call_1", "name": "exec", "input": "ls"}
		],
		"usage": {"input_tokens": 10, "output_tokens": 1, "total_tokens": 11}
	}`)
	p.OnResponseFrame(context.Background(), pctx, body, true)

	ext := pctx.Extensions.Inference
	if ext.Completion != "checking" {
		t.Errorf("Completion = %q, want checking", ext.Completion)
	}
	if len(ext.ToolCalls) != 1 || ext.ToolCalls[0].Name != "exec" || ext.ToolCalls[0].Arguments != "ls" {
		t.Errorf("ToolCalls = %+v, want one call: exec(ls)", ext.ToolCalls)
	}
}

// function_call's non-streaming shape — JSON-schema arguments in the Arguments field
// rather than custom_tool_call's freeform Input — was previously only exercised on the
// streaming path. Pins that parseResponsesJSON reads it too.
func TestInferenceParser_ResponsesAPI_NonStreamingFunctionCall(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{Path: "/v1/responses"}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "gpt-6-luna", IsAction: true}

	body := []byte(`{
		"status": "completed",
		"output": [
			{"type": "function_call", "call_id": "call_abc", "name": "get_weather", "arguments": "{\"city\":\"NYC\"}"}
		],
		"usage": {"input_tokens": 10, "output_tokens": 1, "total_tokens": 11}
	}`)
	p.OnResponseFrame(context.Background(), pctx, body, true)

	ext := pctx.Extensions.Inference
	if len(ext.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %+v, want exactly 1", ext.ToolCalls)
	}
	tc := ext.ToolCalls[0]
	if tc.ID != "call_abc" || tc.Name != "get_weather" || tc.Arguments != `{"city":"NYC"}` {
		t.Errorf("ToolCalls[0] = %+v, want id=call_abc name=get_weather arguments={\"city\":\"NYC\"}", tc)
	}
}

// Mirrors the streaming incomplete-exclusion test above for the non-streaming path: an
// output item marked "incomplete" or "in_progress" has no "added"/"done" split to track —
// it arrives whole — so parseResponsesJSON must skip it directly rather than recording
// cut-off arguments.
func TestInferenceParser_ResponsesAPI_NonStreamingToolCallIncompleteExcluded(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{Path: "/v1/responses"}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "gpt-6-luna", IsAction: true}

	body := []byte(`{
		"status": "incomplete",
		"output": [
			{"type": "custom_tool_call", "call_id": "call_cut_short", "name": "exec", "input": "const r = await tools.ex", "status": "incomplete"}
		],
		"usage": {"input_tokens": 10, "output_tokens": 1, "total_tokens": 11}
	}`)
	p.OnResponseFrame(context.Background(), pctx, body, true)

	ext := pctx.Extensions.Inference
	if len(ext.ToolCalls) != 0 {
		t.Errorf("ToolCalls = %+v, want none — an incomplete item's cut-off arguments must not be recorded", ext.ToolCalls)
	}
}
