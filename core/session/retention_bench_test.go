package session

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// benchTurns is a session long enough for the quadratic term to dominate, short enough
// to run in a second.
const benchTurns = 300

// benchConversation builds the messages one turn re-sends: the whole conversation so
// far. Rebuilt per turn, as a parser does, so nothing is shared by accident.
func benchConversation(turn int) []pipeline.InferenceMessage {
	out := make([]pipeline.InferenceMessage, 0, turn)
	for i := 0; i < turn; i++ {
		out = append(out, pipeline.InferenceMessage{
			Role:    "user",
			Content: fmt.Sprintf("message %04d: %s", i, strings.Repeat("prompt text ", 40)),
		})
	}
	return out
}

// benchToolManifest builds the tool manifest a client re-sends on every request, sized
// from a live session: 27 tools, ~800 bytes of schema each. Rebuilt per call, as a parser
// does, so nothing is shared by accident.
//
// nonce distinguishes the two sub-benchmarks below. Passing the turn number makes every
// turn's manifest unique, which is what interning cannot collapse.
func benchToolManifest(nonce int) []pipeline.InferenceTool {
	out := make([]pipeline.InferenceTool, 0, 27)
	for i := 0; i < 27; i++ {
		out = append(out, pipeline.InferenceTool{
			Name:        fmt.Sprintf("tool_%02d", i),
			Description: fmt.Sprintf("tool %02d: %s", i, strings.Repeat("what it does ", 20)),
			Parameters: pipeline.RawJSON(fmt.Sprintf(
				`{"type": "object", "nonce": %d, "properties": {"arg_%02d": {"type": "string", "description": "%s"}}}`,
				nonce, i, strings.Repeat("an argument ", 40))),
		})
	}
	return out
}

// BenchmarkRetainedHeapWithTools reports what the tool manifest costs a session, which is
// the term BenchmarkRetainedHeap does not exercise at all — and was the largest one in a
// live proxy before the schemas were interned: measured at 84KB per event, 4.1x the JSON
// text, because they were held as map[string]any.
//
// Two sub-benchmarks, because a single figure cannot show what interning did:
//
//   - shared: every turn re-sends the SAME manifest, which is what a real client does.
//     One copy per session survives.
//   - distinct: every turn's manifest differs by one nonce field, so the table can never
//     match. Standing next to `shared` it prices the duplication that was being paid.
//
// Measured when this was written: shared 2.78MB/session, distinct 7.53MB — the tool term
// collapsing from ~5.3MB to ~0.6MB.
//
// What `distinct` is NOT is a reproduction of the old cost. It prices duplication only,
// and the field it replaced was a map[string]any, which cost 4.1x its JSON text to hold
// on top of being duplicated. The real change is therefore larger than the ratio here,
// and the map figure is not reproducible in this tree by design — the type is gone.
//
// The interleaved tunnel-open in the loop below is load bearing, not incidental colour.
// With it present and InternEvent's contentless-event guard removed, `shared` measures
// 31.33MB/session against 2.79MB — 11.2x, because every turn's table was being cleared
// before the next turn could match it. Any change to the rolling table should be measured
// on THIS shape rather than on a clean run of turns, which is not what live traffic is.
//
// Reported rather than asserted, for the reasons on BenchmarkRetainedHeap:
//
//	go test ./session/ -bench RetainedHeapWithTools -run '^$' -benchtime 1x
func BenchmarkRetainedHeapWithTools(b *testing.B) {
	for _, tc := range []struct {
		name  string
		nonce func(turn int) int
	}{
		{"shared", func(int) int { return 0 }},
		{"distinct", func(turn int) int { return turn }},
	} {
		b.Run(tc.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				runtime.GC()
				var before runtime.MemStats
				runtime.ReadMemStats(&before)

				s := New(0, 0, 100)
				for turn := 1; turn <= benchTurns; turn++ {
					// A tunnel-open between the turns, as the live traffic has: every
					// bridged HTTPS request records one, and it lands between an inference
					// request and its response. It carries nothing to intern, so it is
					// also the shape that used to reset the table — see InternEvent.
					s.Append("s1", pipeline.SessionEvent{Tunnel: true, Host: "gateway:443"})
					s.Append("s1", pipeline.SessionEvent{
						Inference: &pipeline.InferenceExtension{
							Messages: benchConversation(turn),
							Tools:    benchToolManifest(tc.nonce(turn)),
						},
					})
				}

				runtime.GC()
				var after runtime.MemStats
				runtime.ReadMemStats(&after)
				delta := int64(after.HeapAlloc) - int64(before.HeapAlloc)
				b.ReportMetric(float64(delta)/1048576, "MB/session")
				b.ReportMetric(float64(benchTurns), "turns")

				s.Close()
				runtime.KeepAlive(s)
			}
		})
	}
}

// BenchmarkRetainedHeap reports the heap a finished session holds, which is the number
// this package's interning exists to move.
//
// Committed because the figure is otherwise unreproducible after any change to how
// content is stored — and the review of that change found exactly such a regression
// hiding behind a plausible number: keying the intern table on the duplicate rather than
// the canonical string, which reads correctly and silently retains a second copy of the
// conversation.
//
// Reported rather than asserted. A threshold either flakes or is loose enough to miss
// the regressions that matter, so the deterministic properties are pinned by tests
// (TestIntern_TableKeysAreTheStringsTheEventsReference,
// TestAppend_SharesRepeatedMessageContent) and this reports the consequence:
//
//	go test ./session/ -bench RetainedHeap -run '^$' -benchtime 1x
func BenchmarkRetainedHeap(b *testing.B) {
	for i := 0; i < b.N; i++ {
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)

		s := New(0, 0, 100)
		for turn := 1; turn <= benchTurns; turn++ {
			s.Append("s1", pipeline.SessionEvent{
				Inference: &pipeline.InferenceExtension{Messages: benchConversation(turn)},
			})
		}

		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		// Signed: HeapAlloc is uint64, and if a GC nets the heap below the baseline the
		// unsigned difference wraps to ~1.8e19 and reports ~1.7e13 MB/session. A small
		// negative is the honest answer and reads as one.
		delta := int64(after.HeapAlloc) - int64(before.HeapAlloc)
		b.ReportMetric(float64(delta)/1048576, "MB/session")
		b.ReportMetric(float64(benchTurns), "turns")

		s.Close()
		runtime.KeepAlive(s)
	}
}
