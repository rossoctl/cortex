package cpex

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// --- inferenceToCMFParts (request) ---

func TestInferenceToCMFParts_RequestFlattensMessages(t *testing.T) {
	inf := &pipeline.InferenceExtension{
		Model: "gpt-4o",
		Messages: []pipeline.InferenceMessage{
			{Role: "system", Content: "be terse"},
			{Role: "user", Content: "my ssn is 123-45-6789"},
			{Role: "assistant", Content: ""}, // tool-call turn, no content — dropped
		},
	}
	parts := inferenceToCMFParts(inf, false, nil)
	if len(parts) != 2 {
		t.Fatalf("want 2 text parts (empty-content message dropped), got %d", len(parts))
	}
	for i, want := range []string{"be terse", "my ssn is 123-45-6789"} {
		if parts[i].Kind != cmfPartText || parts[i].Text != want {
			t.Fatalf("part %d = %+v, want text %q", i, parts[i], want)
		}
	}
}

// --- inferenceResponseParts (response phase parses body) ---

func TestInferenceResponseParts_CompletionAndToolCalls(t *testing.T) {
	body := []byte(`{
		"choices": [{
			"message": {
				"role": "assistant",
				"content": "here is the answer",
				"tool_calls": [
					{"id": "call_1", "type": "function", "function": {"name": "lookup", "arguments": "{\"id\":\"E1\"}"}}
				]
			},
			"finish_reason": "stop"
		}]
	}`)
	parts := inferenceResponseParts(body)
	if len(parts) != 2 {
		t.Fatalf("want completion + tool_call = 2 parts, got %d: %+v", len(parts), parts)
	}
	if parts[0].Kind != cmfPartText || parts[0].Text != "here is the answer" {
		t.Fatalf("part0 = %+v, want completion text", parts[0])
	}
	if parts[1].Kind != cmfPartToolCall || parts[1].Name != "lookup" {
		t.Fatalf("part1 = %+v, want tool_call lookup", parts[1])
	}
	if got := parts[1].Arguments["id"]; got != "E1" {
		t.Fatalf("tool_call args id = %v, want E1", got)
	}
	if parts[1].CorrelationID != "call_1" {
		t.Fatalf("tool_call id = %q, want call_1", parts[1].CorrelationID)
	}
}

func TestInferenceResponseParts_StreamingBodyYieldsNil(t *testing.T) {
	// SSE frames don't parse as one JSON object.
	sse := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	if parts := inferenceResponseParts(sse); parts != nil {
		t.Fatalf("streaming body should yield nil parts, got %+v", parts)
	}
}

// --- applyInferenceRequestBodyMod ---

func TestInferenceRequestBodyMod_RedactsPositionally(t *testing.T) {
	pctx := &pipeline.Context{
		Body: []byte(`{"model":"gpt-4o","messages":[{"role":"system","content":"sys"},{"role":"user","content":"ssn 123-45-6789"}],"temperature":0.2}`),
	}
	mutated, err := applyInferenceRequestBodyMod(pctx, []string{"sys", "ssn [REDACTED]"})
	if err != nil || !mutated {
		t.Fatalf("mutated=%v err=%v", mutated, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(pctx.Body, &decoded); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	msgs := decoded["messages"].([]any)
	if got := msgs[1].(map[string]any)["content"]; got != "ssn [REDACTED]" {
		t.Fatalf("user content not redacted: %v", got)
	}
	// Untouched fields survive.
	if decoded["temperature"].(float64) != 0.2 {
		t.Fatalf("temperature lost: %v", decoded["temperature"])
	}
}

func TestInferenceRequestBodyMod_CountDriftFailsClosed(t *testing.T) {
	pctx := &pipeline.Context{
		Body: []byte(`{"messages":[{"role":"user","content":"a"},{"role":"user","content":"b"}]}`),
	}
	// Only one redacted text for two string-content messages → drift.
	mutated, err := applyInferenceRequestBodyMod(pctx, []string{"a"})
	if err == nil {
		t.Fatal("expected count-drift error (fail closed), got nil")
	}
	if mutated {
		t.Fatal("must not mutate on drift")
	}
}

func TestInferenceRequestBodyMod_NoChangeReportsNoOp(t *testing.T) {
	pctx := &pipeline.Context{
		Body: []byte(`{"messages":[{"role":"user","content":"unchanged"}]}`),
	}
	mutated, err := applyInferenceRequestBodyMod(pctx, []string{"unchanged"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if mutated {
		t.Fatal("identical rewrite should report mutated=false")
	}
}

// --- applyInferenceResponseBodyMod ---

func TestInferenceResponseBodyMod_RewritesChoiceContent(t *testing.T) {
	pctx := &pipeline.Context{
		ResponseBody: []byte(`{"choices":[{"message":{"role":"assistant","content":"leaked 123-45-6789"},"finish_reason":"stop"}],"usage":{"total_tokens":9}}`),
	}
	mutated, err := applyInferenceResponseBodyMod(pctx, "leaked [REDACTED]")
	if err != nil || !mutated {
		t.Fatalf("mutated=%v err=%v", mutated, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(pctx.ResponseBody, &decoded); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	msg := decoded["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "leaked [REDACTED]" {
		t.Fatalf("completion not redacted: %v", msg["content"])
	}
}

func TestInferenceResponseBodyMod_MultiChoiceFailsClosed(t *testing.T) {
	// n>1: two choices carry string content, but only one redacted
	// completion is available. Stamping it over both would collapse
	// distinct completions, so the rewrite must fail closed.
	pctx := &pipeline.Context{
		ResponseBody: []byte(`{"choices":[{"message":{"role":"assistant","content":"first 123-45-6789"}},{"message":{"role":"assistant","content":"second answer"}}]}`),
	}
	original := append([]byte(nil), pctx.ResponseBody...)
	mutated, err := applyInferenceResponseBodyMod(pctx, "first [REDACTED]")
	if err == nil {
		t.Fatal("multi-choice response rewrite should fail closed with an error")
	}
	if mutated {
		t.Fatal("must not mutate a multi-choice response")
	}
	if string(pctx.ResponseBody) != string(original) {
		t.Fatalf("response body must be left untouched on fail-closed, got %s", pctx.ResponseBody)
	}
}

func TestInferenceResponseBodyMod_StreamingFailsClosed(t *testing.T) {
	pctx := &pipeline.Context{
		ResponseBody: []byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"),
	}
	mutated, err := applyInferenceResponseBodyMod(pctx, "redacted")
	if err == nil {
		t.Fatal("streaming response rewrite should fail closed with an error")
	}
	if mutated {
		t.Fatal("must not mutate a streaming response")
	}
}

// --- parseToolArgs / inferProvider ---

func TestParseToolArgs(t *testing.T) {
	if got := parseToolArgs(`{"a":1}`); !reflect.DeepEqual(got, map[string]any{"a": float64(1)}) {
		t.Fatalf("valid json: %v", got)
	}
	if got := parseToolArgs("not json"); got["_raw"] != "not json" {
		t.Fatalf("malformed should wrap as _raw: %v", got)
	}
	if got := parseToolArgs(""); got != nil {
		t.Fatalf("empty should be nil: %v", got)
	}
}

func TestInferProvider(t *testing.T) {
	cases := map[string]string{
		"gpt-4o":            "openai",
		"o1-preview":        "openai",
		"claude-3-5-sonnet": "anthropic",
		"gemini-1.5-pro":    "google",
		"llama3":            "",
		"granite-3":         "",
	}
	for model, want := range cases {
		if got := inferProvider(model); got != want {
			t.Errorf("inferProvider(%q) = %q, want %q", model, got, want)
		}
	}
}
