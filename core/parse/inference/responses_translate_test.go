package inference_test

import (
	"testing"

	"github.com/rossoctl/cortex/core/parse/inference"
	"github.com/rossoctl/cortex/core/translate/pivot"
	"github.com/rossoctl/cortex/core/translate/responses"
)

// TestTranslatedResponsesUsage checks rule R11 (decision 14): a reply that
// the hub encodes as Responses, parsed back for the session store, gives
// the pivot usage counts.
func TestTranslatedResponsesUsage(t *testing.T) {
	enc := responses.Protocol{}.NewReplyEncoder(pivot.Request{Model: "claude"}, true)
	var out []byte
	for _, ev := range []pivot.Event{
		{Kind: pivot.EventStart, ID: "resp-1", Model: "claude"},
		{Kind: pivot.EventBlockStart, Index: 0, Block: pivot.BlockText},
		{Kind: pivot.EventDelta, Index: 0, Delta: pivot.DeltaText, Value: "ok"},
		{Kind: pivot.EventBlockStop, Index: 0},
		{Kind: pivot.EventUsage, Usage: pivot.Usage{Input: 100, Output: 20, CacheRead: 300, CacheCreation: 50}},
		{Kind: pivot.EventStop, StopReason: "end_turn"},
	} {
		out = append(out, enc.Encode(ev)...)
	}
	out = append(out, enc.End()...)
	ext := inference.ParseResponse("/backend-api/codex/responses", out)
	if ext == nil {
		t.Fatal("no extension")
	}
	if ext.InputTokens != 100 || ext.CacheReadTokens != 300 || ext.CacheWriteTokens != 50 || ext.OutputTokens != 20 {
		t.Errorf("tokens: input %d, cache read %d, cache write %d, output %d; want 100, 300, 50, 20\n%s",
			ext.InputTokens, ext.CacheReadTokens, ext.CacheWriteTokens, ext.OutputTokens, out)
	}
	if ext.Completion != "ok" {
		t.Errorf("completion %q", ext.Completion)
	}
}
