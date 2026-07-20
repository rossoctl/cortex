package cpex

import (
	"encoding/json"
	"testing"

	"github.com/rossoctl/rossocortex/authbridge/authlib/pipeline"
)

// --- a2aToCMFParts (request) ---

func TestA2AToCMFParts_RequestTextPartsOnly(t *testing.T) {
	a2a := &pipeline.A2AExtension{
		Method: "message/send",
		Role:   "user",
		Parts: []pipeline.A2APart{
			{Kind: "text", Content: "hello"},
			{Kind: "data", Content: `{"x":1}`}, // excluded — structured data
			{Kind: "text", Content: "world"},
			{Kind: "file", Content: "file:///x"}, // excluded
		},
	}
	parts := a2aToCMFParts(a2a, false, nil)
	if len(parts) != 2 {
		t.Fatalf("want 2 text parts (data/file excluded), got %d: %+v", len(parts), parts)
	}
	if parts[0].Text != "hello" || parts[1].Text != "world" {
		t.Fatalf("unexpected parts: %+v", parts)
	}
}

// --- a2aResponseParts (response phase parses body) ---

func TestA2AResponseParts_ArtifactText(t *testing.T) {
	body := []byte(`{"result":{"taskId":"t1","artifacts":[{"parts":[{"kind":"text","text":"final answer"},{"kind":"data","data":{"k":1}}]}]}}`)
	parts := a2aResponseParts(body)
	if len(parts) != 1 {
		t.Fatalf("want 1 artifact text part, got %d: %+v", len(parts), parts)
	}
	if parts[0].Kind != cmfPartText || parts[0].Text != "final answer" {
		t.Fatalf("part = %+v", parts[0])
	}
}

func TestA2AResponseParts_StreamingYieldsNil(t *testing.T) {
	sse := []byte("data: {\"result\":{\"kind\":\"status-update\"}}\n\n")
	if parts := a2aResponseParts(sse); parts != nil {
		t.Fatalf("streaming should yield nil, got %+v", parts)
	}
}

// --- applyA2ARequestBodyMod ---

func TestA2ARequestBodyMod_RedactsTextParts(t *testing.T) {
	pctx := &pipeline.Context{
		Body: []byte(`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"message":{"role":"user","parts":[{"kind":"text","text":"ssn 123-45-6789"},{"kind":"data","data":{"k":1}}]}}}`),
	}
	mutated, err := applyA2ARequestBodyMod(pctx, []string{"ssn [REDACTED]"})
	if err != nil || !mutated {
		t.Fatalf("mutated=%v err=%v", mutated, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(pctx.Body, &decoded); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	parts := decoded["params"].(map[string]any)["message"].(map[string]any)["parts"].([]any)
	if got := parts[0].(map[string]any)["text"]; got != "ssn [REDACTED]" {
		t.Fatalf("text part not redacted: %v", got)
	}
	// Data part survives untouched.
	if _, ok := parts[1].(map[string]any)["data"]; !ok {
		t.Fatalf("data part corrupted: %v", parts[1])
	}
}

func TestA2ARequestBodyMod_CountDriftFailsClosed(t *testing.T) {
	pctx := &pipeline.Context{
		Body: []byte(`{"params":{"message":{"parts":[{"kind":"text","text":"a"},{"kind":"text","text":"b"}]}}}`),
	}
	mutated, err := applyA2ARequestBodyMod(pctx, []string{"only-one"})
	if err == nil || mutated {
		t.Fatalf("expected drift fail-closed; mutated=%v err=%v", mutated, err)
	}
}

func TestA2ARequestBodyMod_EmptyTextPartNotCounted(t *testing.T) {
	// An empty text-kind part is dropped by the read side (a2aToCMFParts),
	// so the write side must not count it either — otherwise it drifts
	// against newTexts and trips a false count-mismatch error.
	pctx := &pipeline.Context{
		Body: []byte(`{"params":{"message":{"parts":[{"kind":"text","text":"ssn 123-45-6789"},{"kind":"text","text":""}]}}}`),
	}
	mutated, err := applyA2ARequestBodyMod(pctx, []string{"ssn [REDACTED]"})
	if err != nil || !mutated {
		t.Fatalf("empty text part should not cause drift; mutated=%v err=%v", mutated, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(pctx.Body, &decoded); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	parts := decoded["params"].(map[string]any)["message"].(map[string]any)["parts"].([]any)
	if got := parts[0].(map[string]any)["text"]; got != "ssn [REDACTED]" {
		t.Fatalf("non-empty text part not redacted: %v", got)
	}
	if got := parts[1].(map[string]any)["text"]; got != "" {
		t.Fatalf("empty text part must stay untouched, got %v", got)
	}
}

// --- applyA2AResponseBodyMod ---

func TestA2AResponseBodyMod_RewritesArtifact(t *testing.T) {
	pctx := &pipeline.Context{
		ResponseBody: []byte(`{"result":{"artifacts":[{"parts":[{"kind":"text","text":"leaked 123-45-6789"}]}]}}`),
	}
	mutated, err := applyA2AResponseBodyMod(pctx, "leaked [REDACTED]")
	if err != nil || !mutated {
		t.Fatalf("mutated=%v err=%v", mutated, err)
	}
	if want := "leaked [REDACTED]"; !jsonContains(t, pctx.ResponseBody, want) {
		t.Fatalf("artifact not redacted in %s", pctx.ResponseBody)
	}
	// Assert the original secret is GONE, not merely that the redacted
	// string is present (a partial rewrite would satisfy the latter).
	if contains(string(pctx.ResponseBody), "123-45-6789") {
		t.Fatalf("original secret still present after redaction: %s", pctx.ResponseBody)
	}
}

func TestA2AResponseBodyMod_MultiPartFailsClosed(t *testing.T) {
	// Two text-kind artifact parts → ambiguous single-value rewrite → error.
	pctx := &pipeline.Context{
		ResponseBody: []byte(`{"result":{"artifacts":[{"parts":[{"kind":"text","text":"part one"},{"kind":"text","text":"part two"}]}]}}`),
	}
	mutated, err := applyA2AResponseBodyMod(pctx, "redacted")
	if err == nil {
		t.Fatal("expected fail-closed error for multi-part response, got nil")
	}
	if mutated {
		t.Fatal("mutated=true on multi-part fail-closed")
	}
}

func TestA2AResponseBodyMod_MultiArtifactMultiPartFailsClosed(t *testing.T) {
	// Text parts spread across multiple artifacts still triggers the guard.
	pctx := &pipeline.Context{
		ResponseBody: []byte(`{"result":{"artifacts":[{"parts":[{"kind":"text","text":"first"}]},{"parts":[{"kind":"text","text":"second"}]}]}}`),
	}
	mutated, err := applyA2AResponseBodyMod(pctx, "redacted")
	if err == nil {
		t.Fatal("expected fail-closed error for multi-artifact text parts, got nil")
	}
	if mutated {
		t.Fatal("mutated=true on multi-artifact fail-closed")
	}
}

func TestA2AResponseBodyMod_StreamingFailsClosed(t *testing.T) {
	pctx := &pipeline.Context{
		ResponseBody: []byte("data: {\"result\":{}}\n\n"),
	}
	mutated, err := applyA2AResponseBodyMod(pctx, "x")
	if err == nil || mutated {
		t.Fatalf("streaming response should fail closed; mutated=%v err=%v", mutated, err)
	}
}

func jsonContains(t *testing.T, body []byte, substr string) bool {
	t.Helper()
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("body not valid JSON: %v", err)
	}
	return contains(string(body), substr)
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
