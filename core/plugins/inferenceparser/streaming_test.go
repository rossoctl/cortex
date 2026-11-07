package inferenceparser

import (
	"context"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

func TestInferenceParser_OnResponseFrame_StreamFoldsDeltas(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Extensions: pipeline.Extensions{
			Inference: &pipeline.InferenceExtension{
				Model:  "gpt-4",
				Stream: true,
			},
		},
	}

	frames := [][]byte{
		[]byte(`{"choices":[{"delta":{"content":"Hello"}}]}`),
		[]byte(`{"choices":[{"delta":{"content":" world"}}]}`),
		[]byte(`{"choices":[{"delta":{"content":"!"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`),
	}
	for _, f := range frames {
		if action := p.OnResponseFrame(context.Background(), pctx, f, false); action.Type != pipeline.Continue {
			t.Fatalf("frame action = %v, want Continue", action.Type)
		}
	}
	// Mid-stream nothing finalized yet.
	if pctx.Extensions.Inference.Completion != "" {
		t.Errorf("Completion populated mid-stream = %q; finalize should wait for last=true", pctx.Extensions.Inference.Completion)
	}

	// Finalize on last=true.
	p.OnResponseFrame(context.Background(), pctx, nil, true)
	ext := pctx.Extensions.Inference
	if ext.Completion != "Hello world!" {
		t.Errorf("Completion = %q, want %q", ext.Completion, "Hello world!")
	}
	if ext.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", ext.FinishReason)
	}
	if ext.PromptTokens != 5 || ext.CompletionTokens != 3 || ext.TotalTokens != 8 {
		t.Errorf("usage = (%d,%d,%d), want (5,3,8)", ext.PromptTokens, ext.CompletionTokens, ext.TotalTokens)
	}
}

func TestInferenceParser_OnResponseFrame_DONESkipped(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Extensions: pipeline.Extensions{
			Inference: &pipeline.InferenceExtension{Model: "m", Stream: true},
		},
	}
	p.OnResponseFrame(context.Background(), pctx, []byte(`{"choices":[{"delta":{"content":"hi"}}]}`), false)
	p.OnResponseFrame(context.Background(), pctx, []byte("[DONE]"), false)
	p.OnResponseFrame(context.Background(), pctx, nil, true)
	if pctx.Extensions.Inference.Completion != "hi" {
		t.Errorf("Completion = %q, want %q ([DONE] should not be parsed as JSON)", pctx.Extensions.Inference.Completion, "hi")
	}
}

func TestInferenceParser_OnResponseFrame_ApplicationJSONOneShot(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Extensions: pipeline.Extensions{
			Inference: &pipeline.InferenceExtension{Model: "m", Stream: false},
		},
	}
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"non-streaming reply"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
	p.OnResponseFrame(context.Background(), pctx, body, true)
	ext := pctx.Extensions.Inference
	if ext.Completion != "non-streaming reply" {
		t.Errorf("Completion = %q", ext.Completion)
	}
	if ext.TotalTokens != 3 {
		t.Errorf("TotalTokens = %d, want 3", ext.TotalTokens)
	}
}

func TestInferenceParser_OnResponseFrame_NoExtensionMeansNoOp(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{}
	action := p.OnResponseFrame(context.Background(), pctx, []byte(`{}`), true)
	if action.Type != pipeline.Continue {
		t.Errorf("action = %v, want Continue", action.Type)
	}
}

func TestInferenceParser_OnResponseFrame_EmptyStreamRecordsSkip(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Extensions: pipeline.Extensions{
			Inference: &pipeline.InferenceExtension{Model: "m", Stream: true},
		},
	}
	pctx.SetCurrentPlugin("inference-parser", pipeline.InvocationPhaseResponse)
	p.OnResponseFrame(context.Background(), pctx, nil, true)
	pctx.ClearCurrentPlugin()
	if pctx.Extensions.Invocations == nil {
		t.Fatal("no invocation recorded for empty stream")
	}
}

// TestInferenceParser_OnResponseFrame_StreamAssemblesToolCalls pins the OpenAI
// streaming tool-call shape: each call arrives as fragments across chunks, keyed by
// the index the provider stamps on each delta.tool_calls entry — id and function name
// on the first fragment, arguments as a series of JSON-string fragments that are only
// valid concatenated. Two calls pin the index keying and emission order.
func TestInferenceParser_OnResponseFrame_StreamAssemblesToolCalls(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Extensions: pipeline.Extensions{
			Inference: &pipeline.InferenceExtension{Model: "gpt-4", Stream: true},
		},
	}

	frames := [][]byte{
		[]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`),
		[]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`),
		[]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]}}]}`),
		[]byte(`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"get_time","arguments":""}}]}}]}`),
		[]byte(`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{}"}}]}}]}`),
		[]byte(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`),
	}
	for _, f := range frames {
		if action := p.OnResponseFrame(context.Background(), pctx, f, false); action.Type != pipeline.Continue {
			t.Fatalf("frame action = %v, want Continue", action.Type)
		}
	}
	p.OnResponseFrame(context.Background(), pctx, nil, true)

	ext := pctx.Extensions.Inference
	want := []pipeline.InferenceToolCall{
		{ID: "call_1", Name: "get_weather", Arguments: `{"city":"Paris"}`},
		{ID: "call_2", Name: "get_time", Arguments: `{}`},
	}
	if len(ext.ToolCalls) != len(want) {
		t.Fatalf("ToolCalls = %+v, want %+v", ext.ToolCalls, want)
	}
	for i := range want {
		if ext.ToolCalls[i] != want[i] {
			t.Errorf("ToolCalls[%d] = %+v, want %+v", i, ext.ToolCalls[i], want[i])
		}
	}
	if ext.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want tool_calls", ext.FinishReason)
	}
}

// A stream of only empty mid-stream frames is still a stream. StreamedResponse must be
// set, because pricing.IncompleteReason reads it to decide whether an output tally is
// final. The mid-stream arm must call Stream.Fold for an empty frame too: a length guard
// there leaves no stream state, and the terminal frame then reads as a buffered body.
func TestInferenceParser_OnResponseFrame_EmptyFramesMarkStreamed(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/v1/chat/completions",
		Extensions: pipeline.Extensions{
			Inference: &pipeline.InferenceExtension{Model: "m"},
		},
	}
	pctx.SetCurrentPlugin("inference-parser", pipeline.InvocationPhaseResponse)
	p.OnResponseFrame(context.Background(), pctx, nil, false)
	p.OnResponseFrame(context.Background(), pctx, []byte{}, false)
	p.OnResponseFrame(context.Background(), pctx, nil, true)
	pctx.ClearCurrentPlugin()
	if !pctx.Extensions.Inference.StreamedResponse {
		t.Error("StreamedResponse = false after only empty mid-stream frames, want true")
	}
}
