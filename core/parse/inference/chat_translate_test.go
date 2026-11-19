package inference_test

import (
	"encoding/json"
	"testing"

	"github.com/rossoctl/cortex/core/parse/inference"
	"github.com/rossoctl/cortex/core/translate/chat"
	"github.com/rossoctl/cortex/core/translate/pivot"
)

// TestTranslatedChatUsage checks rules K6 and K9: a reply that the hub
// encodes as Chat Completions, parsed back for the session store, gives
// the pivot usage counts in each reply form.
func TestTranslatedChatUsage(t *testing.T) {
	withUsage := pivot.Extra{"chat-completions.include_usage": json.RawMessage("true")}
	for _, tc := range []struct {
		name   string
		extra  pivot.Extra
		stream bool
	}{
		{"stream include_usage", withUsage, true},
		{"stream", nil, true},
		{"not streamed", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enc := chat.Protocol{}.NewReplyEncoder(pivot.Request{Model: "claude", Extra: tc.extra}, tc.stream)
			var out []byte
			for _, ev := range []pivot.Event{
				{Kind: pivot.EventStart, ID: "msg-1", Model: "claude"},
				{Kind: pivot.EventBlockStart, Index: 0, Block: pivot.BlockText},
				{Kind: pivot.EventDelta, Index: 0, Delta: pivot.DeltaText, Value: "ok"},
				{Kind: pivot.EventBlockStop, Index: 0},
				{Kind: pivot.EventUsage, Usage: pivot.Usage{Input: 100, Output: 20, CacheRead: 300, CacheCreation: 50}},
				{Kind: pivot.EventStop, StopReason: "end_turn"},
			} {
				out = append(out, enc.Encode(ev)...)
			}
			out = append(out, enc.End()...)
			ext := inference.ParseResponse("/v1/chat/completions", out)
			if ext == nil {
				t.Fatalf("no extension\n%s", out)
			}
			if ext.InputTokens != 100 || ext.CacheReadTokens != 300 || ext.CacheWriteTokens != 50 || ext.OutputTokens != 20 {
				t.Errorf("tokens: input %d, cache read %d, cache write %d, output %d; want 100, 300, 50, 20\n%s",
					ext.InputTokens, ext.CacheReadTokens, ext.CacheWriteTokens, ext.OutputTokens, out)
			}
			if ext.Completion != "ok" {
				t.Errorf("completion %q\n%s", ext.Completion, out)
			}
		})
	}
}
