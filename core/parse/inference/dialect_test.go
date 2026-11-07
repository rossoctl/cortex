package inference

import (
	"testing"
)

func TestDialectFor(t *testing.T) {
	for _, tc := range []struct {
		path string
		want dialect
	}{
		// Anthropic Messages at the root and under every prefix seen so far.
		{"/v1/messages", dialectAnthropic},
		{"/zen/v1/messages", dialectAnthropic},
		{"/zen/go/v1/messages", dialectAnthropic},
		{"/anthropic/v1/messages", dialectAnthropic},
		// OpenAI chat completions and legacy completions, likewise.
		{"/v1/chat/completions", dialectOpenAI},
		{"/chat/completions", dialectOpenAI},
		{"/v1/completions", dialectOpenAI},
		{"/completions", dialectOpenAI},
		{"/inference/v1/chat/completions", dialectOpenAI},
		{"/zen/v1/chat/completions", dialectOpenAI},
		{"/zen/go/v1/chat/completions", dialectOpenAI},
		{"/api/v1/chat/completions", dialectOpenAI},
		{"/openai/v1/chat/completions", dialectOpenAI},
		{"/openai/deployments/gpt-4o/chat/completions", dialectOpenAI},
		{"/v1beta/openai/chat/completions", dialectOpenAI},
		// OpenAI Responses API: the public endpoint, and Codex's chatgpt.com-hosted
		// gateway — sharing no prefix, only the "/responses" ending.
		{"/v1/responses", dialectResponses},
		{"/backend-api/codex/responses", dialectResponses},
		// Endpoints beside an inference one, sharing its prefix but not its ending.
		{"/v1/messages/count_tokens", dialectNone},
		{"/v1/messages/batches", dialectNone},
		{"/v1/embeddings", dialectNone},
		{"/v1/rerank", dialectNone},
		{"/inference/v1/model/info", dialectNone},
		{"/v1/autocompletions", dialectNone},
		{"/foov1/messages", dialectNone},                 // the segment boundary, for the suffix checked first
		{"/inference/v1/chat/completions/", dialectNone}, // trailing slash: not trimmed
		{"", dialectNone},
		{"/", dialectNone},
	} {
		if got := dialectFor(tc.path); got != tc.want {
			t.Errorf("dialectFor(%q) = %d, want %d", tc.path, got, tc.want)
		}
	}
}
