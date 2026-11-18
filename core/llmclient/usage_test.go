package llmclient_test

import (
	"encoding/json"
	"testing"

	"github.com/rossoctl/cortex/core/llmclient"
)

// An off-shape usage block leaves Usage nil; it never fails the decode. The judge plugins
// share this client and never read usage (review finding).
func TestChatResponse_LenientUsage(t *testing.T) {
	cases := []struct {
		usage      string
		wantUsage  bool
		wantPrompt int
		wantCached int
	}{
		{`{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":4}}`, true, 10, 4},
		{`null`, false, 0, 0},
		{`{"prompt_tokens":10.0}`, false, 0, 0},
		{`{"prompt_tokens":10,"prompt_tokens_details":[]}`, false, 0, 0},
		{`[]`, false, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.usage, func(t *testing.T) {
			var r llmclient.ChatResponse
			body := `{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":` + c.usage + `}`
			if err := json.Unmarshal([]byte(body), &r); err != nil {
				t.Fatalf("decode failed: %v", err)
			}
			if len(r.Choices) != 1 || r.Choices[0].Message.Content != "ok" {
				t.Errorf("choices = %+v, want the one answer", r.Choices)
			}
			if (r.Usage != nil) != c.wantUsage {
				t.Fatalf("Usage = %+v, want present=%v", r.Usage, c.wantUsage)
			}
			if r.Usage != nil && (r.Usage.PromptTokens != c.wantPrompt || r.Usage.CachedTokens() != c.wantCached) {
				t.Errorf("Usage = %+v, want prompt %d cached %d", r.Usage, c.wantPrompt, c.wantCached)
			}
		})
	}
	// No usage key at all.
	var r llmclient.ChatResponse
	if err := json.Unmarshal([]byte(`{"choices":[]}`), &r); err != nil || r.Usage != nil {
		t.Errorf("no usage: err=%v usage=%+v", err, r.Usage)
	}
}
