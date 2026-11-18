package chat

import (
	"testing"

	"github.com/rossoctl/cortex/core/translate/pivot"
)

// BenchmarkEncodeReplyStream measures rule K11 on the reply side: a stream
// of 2,000 pivot events, text and tool-input deltas.
func BenchmarkEncodeReplyStream(b *testing.B) {
	evs := []pivot.Event{start(), open(0, pivot.BlockText)}
	for range 1000 {
		evs = append(evs, delta(0, pivot.DeltaText, "some words of text "))
	}
	evs = append(evs, closeBlock(0), openTool(1, "call_1", "shell"))
	for range 994 {
		evs = append(evs, delta(1, pivot.DeltaInputJSON, `"piece",`))
	}
	evs = append(evs, closeBlock(1), usage(pivot.Usage{Input: 10, Output: 5, CacheRead: 20, CacheCreation: 30}), stop("tool_use"))
	for b.Loop() {
		enc := Protocol{}.NewReplyEncoder(pivot.Request{Model: "m"}, true)
		for _, ev := range evs {
			enc.Encode(ev)
		}
		enc.End()
	}
}
