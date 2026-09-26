package inferenceparser

import (
	"fmt"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/internal/parsercommon"
)

// checkSplit surfaces every split field in one error message so a
// mismatched counter is obvious.
func checkSplit(t *testing.T, ext *pipeline.InferenceExtension, want pipeline.InferenceExtension) {
	t.Helper()
	got := fmt.Sprintf("input=%d cache_read=%d cache_write=%d output=%d reasoning=%d prompt=%d completion=%d total=%d",
		ext.InputTokens, ext.CacheReadTokens, ext.CacheWriteTokens, ext.OutputTokens, ext.ReasoningTokens,
		ext.PromptTokens, ext.CompletionTokens, ext.TotalTokens)
	expected := fmt.Sprintf("input=%d cache_read=%d cache_write=%d output=%d reasoning=%d prompt=%d completion=%d total=%d",
		want.InputTokens, want.CacheReadTokens, want.CacheWriteTokens, want.OutputTokens, want.ReasoningTokens,
		want.PromptTokens, want.CompletionTokens, want.TotalTokens)
	if got != expected {
		t.Errorf("split token counters mismatch\n got: %s\nwant: %s", got, expected)
	}
}

// Non-streaming Anthropic response with both cache_read and cache_write.
func TestSplitTokens_AnthropicJSON(t *testing.T) {
	ext := &pipeline.InferenceExtension{Model: "claude-opus-4-8"}
	body := []byte(`{
		"content": [{"type": "text", "text": "ok"}],
		"stop_reason": "end_turn",
		"usage": {
			"input_tokens": 50,
			"cache_creation_input_tokens": 100,
			"cache_read_input_tokens": 200,
			"output_tokens": 25
		}
	}`)
	parseAnthropicJSON(body, ext)

	checkSplit(t, ext, pipeline.InferenceExtension{
		InputTokens:      50,
		CacheReadTokens:  200,
		CacheWriteTokens: 100,
		OutputTokens:     25,
		ReasoningTokens:  0,
		PromptTokens:     350, // 50 + 200 + 100
		CompletionTokens: 25,
		TotalTokens:      375,
	})
}

// Non-beta SSE: prompt counts on message_start, output on message_delta.
func TestSplitTokens_AnthropicSSE_NonBeta(t *testing.T) {
	ext := &pipeline.InferenceExtension{Model: "claude-opus-4-8"}
	body := []byte("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":40,\"cache_creation_input_tokens\":10,\"cache_read_input_tokens\":30,\"output_tokens\":0}}}\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":12}}\n")
	parseAnthropicSSE(body, ext)

	checkSplit(t, ext, pipeline.InferenceExtension{
		InputTokens:      40,
		CacheReadTokens:  30,
		CacheWriteTokens: 10,
		OutputTokens:     12,
		ReasoningTokens:  0,
		PromptTokens:     80,
		CompletionTokens: 12,
		TotalTokens:      92,
	})
}

// ?beta=true SSE: message_start carries only input_tokens; cache counts
// arrive on message_delta. Exercises mergeAnthropicUsageMaxSeen.
func TestSplitTokens_AnthropicSSE_Beta(t *testing.T) {
	ext := &pipeline.InferenceExtension{Model: "claude-opus-4-8"}
	body := []byte("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9,\"output_tokens\":0}}}\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":9,\"cache_creation_input_tokens\":1234,\"cache_read_input_tokens\":32529,\"output_tokens\":399}}\n")
	parseAnthropicSSE(body, ext)

	checkSplit(t, ext, pipeline.InferenceExtension{
		InputTokens:      9,
		CacheReadTokens:  32529,
		CacheWriteTokens: 1234,
		OutputTokens:     399,
		ReasoningTokens:  0,
		PromptTokens:     33772,
		CompletionTokens: 399,
		TotalTokens:      34171,
	})
}

// OpenAI JSON: inclusive-prompt normalization (Input = prompt - cached)
// and reasoning pulled from completion_tokens_details.
func TestSplitTokens_OpenAIJSON(t *testing.T) {
	ext := &pipeline.InferenceExtension{Model: "gpt-4o"}
	body := []byte(`{
		"choices": [{"message": {"role": "assistant", "content": "hi"}, "finish_reason": "stop"}],
		"usage": {
			"prompt_tokens": 1500,
			"completion_tokens": 400,
			"total_tokens": 1900,
			"prompt_tokens_details": {"cached_tokens": 200},
			"completion_tokens_details": {"reasoning_tokens": 50}
		}
	}`)
	parseInferenceJSON(body, ext)

	checkSplit(t, ext, pipeline.InferenceExtension{
		InputTokens:      1300,
		CacheReadTokens:  200,
		CacheWriteTokens: 0,
		OutputTokens:     400,
		ReasoningTokens:  50,
		PromptTokens:     1500,
		CompletionTokens: 400,
		TotalTokens:      1900,
	})
}

// Gateway reports only total_tokens (prompt/completion zero): the wire
// total must be preserved rather than recomputed as 0 from the sub-fields.
func TestSplitTokens_OpenAIJSON_PreservesReportedTotal(t *testing.T) {
	ext := &pipeline.InferenceExtension{Model: "gpt-4o"}
	parseInferenceJSON([]byte(`{
		"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":950}
	}`), ext)

	if ext.TotalTokens != 950 {
		t.Errorf("TotalTokens = %d, want 950 (reported total preserved)", ext.TotalTokens)
	}
}

// prompt_tokens/completion_tokens keys absent (not zero): both bits
// must stay cleared. Contrast with PreservesReportedTotal, where the
// keys are present with value 0 and the bits stay set.
func TestPresentKinds_OpenAI_TotalOnly(t *testing.T) {
	ext := &pipeline.InferenceExtension{Model: "gpt-4o"}
	parseInferenceJSON([]byte(`{
		"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],
		"usage":{"total_tokens":950}
	}`), ext)

	if ext.PresentKinds&uint8(parsercommon.KindInput) != 0 {
		t.Errorf("KindInput set, want cleared (prompt_tokens absent from wire)")
	}
	if ext.PresentKinds&uint8(parsercommon.KindOutput) != 0 {
		t.Errorf("KindOutput set, want cleared (completion_tokens absent from wire)")
	}
	if ext.TotalTokens != 950 {
		t.Errorf("TotalTokens = %d, want 950", ext.TotalTokens)
	}
}

// Malformed OpenAI response where cached_tokens > prompt_tokens must
// clamp InputTokens to 0, not go negative.
func TestSplitTokens_OpenAIJSON_ClampNegativeInput(t *testing.T) {
	ext := &pipeline.InferenceExtension{Model: "gpt-4o"}
	body := []byte(`{
		"choices": [{"message": {"role": "assistant", "content": "ok"}, "finish_reason": "stop"}],
		"usage": {
			"prompt_tokens": 100,
			"completion_tokens": 20,
			"total_tokens": 120,
			"prompt_tokens_details": {"cached_tokens": 250}
		}
	}`)
	parseInferenceJSON(body, ext)

	if ext.InputTokens != 0 {
		t.Errorf("InputTokens = %d, want 0 (clamped)", ext.InputTokens)
	}
	if ext.CacheReadTokens != 250 {
		t.Errorf("CacheReadTokens = %d, want 250", ext.CacheReadTokens)
	}
}

// TestSplitTokens_OpenAISSE covers the OpenAI streaming shape with
// stream_options.include_usage — the usage block arrives on the final
// chunk and must map onto the neutral shape via the same normalization
// as the JSON path.
func TestSplitTokens_OpenAISSE(t *testing.T) {
	ext := &pipeline.InferenceExtension{Model: "gpt-4o", Stream: true}
	body := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":800,\"completion_tokens\":150,\"total_tokens\":950,\"prompt_tokens_details\":{\"cached_tokens\":600},\"completion_tokens_details\":{\"reasoning_tokens\":25}}}\n" +
		"data: [DONE]\n")
	parseInferenceSSE(body, ext)

	checkSplit(t, ext, pipeline.InferenceExtension{
		InputTokens:      200,
		CacheReadTokens:  600,
		CacheWriteTokens: 0,
		OutputTokens:     150,
		ReasoningTokens:  25,
		PromptTokens:     800,
		CompletionTokens: 150,
		TotalTokens:      950,
	})
}

// OpenAI response without the optional _details blocks: only Input
// and Output are on the wire, so no cache-read or reasoning bit is set.
func TestPresentKinds_OpenAI_NoDetailsBlocks(t *testing.T) {
	ext := &pipeline.InferenceExtension{Model: "llama3.1"}
	parseInferenceJSON([]byte(`{
		"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}
	}`), ext)

	want := uint8(parsercommon.KindInput | parsercommon.KindOutput)
	if ext.PresentKinds != want {
		t.Errorf("PresentKinds = %b, want %b (Input|Output only)", ext.PresentKinds, want)
	}
}

// _details blocks present but zero: bits set ("reported zero"), not
// absent — the distinction from the previous test.
func TestPresentKinds_OpenAI_WithDetailsBlocks(t *testing.T) {
	ext := &pipeline.InferenceExtension{Model: "gpt-4o"}
	parseInferenceJSON([]byte(`{
		"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],
		"usage":{
			"prompt_tokens":12,"completion_tokens":3,"total_tokens":15,
			"prompt_tokens_details":{"cached_tokens":0},
			"completion_tokens_details":{"reasoning_tokens":0}
		}
	}`), ext)

	want := uint8(parsercommon.KindInput | parsercommon.KindOutput | parsercommon.KindCacheRead | parsercommon.KindReasoning)
	if ext.PresentKinds != want {
		t.Errorf("PresentKinds = %b, want %b (all four)", ext.PresentKinds, want)
	}
}

// No usage block on the wire: PresentKinds must stay 0 so the
// log renders -1 (not exposed) rather than 0 (reported zero).
func TestPresentKinds_OpenAI_NoUsageBlock(t *testing.T) {
	ext := &pipeline.InferenceExtension{Model: "gpt-4o"}
	parseInferenceJSON([]byte(`{
		"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]
	}`), ext)

	if ext.PresentKinds != 0 {
		t.Errorf("PresentKinds = %b, want 0", ext.PresentKinds)
	}
}

// Anthropic response: Input/CacheRead/CacheWrite/Output always
// present (Messages API emits all four); Reasoning never present.
func TestPresentKinds_Anthropic(t *testing.T) {
	ext := &pipeline.InferenceExtension{Model: "claude-haiku-4-5"}
	parseAnthropicJSON([]byte(`{
		"content":[{"type":"text","text":"ok"}],
		"stop_reason":"end_turn",
		"usage":{
			"input_tokens":8,"output_tokens":2,
			"cache_creation_input_tokens":0,"cache_read_input_tokens":0
		}
	}`), ext)

	want := uint8(parsercommon.KindInput | parsercommon.KindCacheRead | parsercommon.KindCacheWrite | parsercommon.KindOutput)
	if ext.PresentKinds != want {
		t.Errorf("PresentKinds = %b, want %b (four kinds, reasoning absent)", ext.PresentKinds, want)
	}
}

// Anthropic non-streaming response WITHOUT the cache_* fields on the wire:
// only Input and Output bits should be set.
func TestPresentKinds_Anthropic_NoCacheFields(t *testing.T) {
	ext := &pipeline.InferenceExtension{Model: "claude-haiku-4-5"}
	parseAnthropicJSON([]byte(`{
		"content":[{"type":"text","text":"ok"}],
		"stop_reason":"end_turn",
		"usage":{"input_tokens":8,"output_tokens":2}
	}`), ext)

	want := uint8(parsercommon.KindInput | parsercommon.KindOutput)
	if ext.PresentKinds != want {
		t.Errorf("PresentKinds = %b, want %b (Input|Output only)", ext.PresentKinds, want)
	}
}

// Anthropic ?beta=true SSE interrupted after message_start: no cache fields
// ever land on the wire, so their bits must stay cleared.
func TestPresentKinds_AnthropicSSE_Beta_MessageStartOnly(t *testing.T) {
	ext := &pipeline.InferenceExtension{Model: "claude-opus-4-8"}
	body := []byte("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9,\"output_tokens\":0}}}\n")
	parseAnthropicSSE(body, ext)

	want := uint8(parsercommon.KindInput | parsercommon.KindOutput)
	if ext.PresentKinds != want {
		t.Errorf("PresentKinds = %b, want %b (no cache fields observed)", ext.PresentKinds, want)
	}
}

// OpenAI streaming: usage is cumulative, so state.usage is replaced on
// each usage-bearing chunk. A later chunk that omits _details must
// therefore drop CacheRead — this pins that behavior against a future
// refactor that turns usage merging back into a merge.
func TestFoldOpenAIFrame_UsageIsCumulative(t *testing.T) {
	ext := &pipeline.InferenceExtension{Model: "gpt-4o", Stream: true}
	body := []byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":null}],\"usage\":{\"prompt_tokens\":800,\"completion_tokens\":100,\"total_tokens\":900,\"prompt_tokens_details\":{\"cached_tokens\":600}}}\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":800,\"completion_tokens\":150,\"total_tokens\":950}}\n" +
		"data: [DONE]\n")
	parseInferenceSSE(body, ext)

	if ext.CacheReadTokens != 0 {
		t.Errorf("CacheReadTokens = %d, want 0 (later cumulative chunk without _details)", ext.CacheReadTokens)
	}
	if ext.OutputTokens != 150 {
		t.Errorf("OutputTokens = %d, want 150 (last chunk wins)", ext.OutputTokens)
	}
}
