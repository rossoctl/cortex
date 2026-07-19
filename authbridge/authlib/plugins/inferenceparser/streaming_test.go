package inferenceparser

import (
	"context"
	"testing"

	"github.com/rossoctl/rossocortex/authbridge/authlib/pipeline"
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
