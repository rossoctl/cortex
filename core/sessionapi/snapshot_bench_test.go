package sessionapi

import (
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// The live session this is shaped after: a laptop proxy's busiest session, measured over
// its own HTTP API. 226 events carrying an inference extension, ~948 messages each at
// ~476 bytes of content, and a 140MB response for one snapshot request.
const (
	benchEvents      = 226
	benchMsgPerEvent = 948
	benchMsgBytes    = 476
)

// benchView builds the fixture THROUGH the store, not as a bare SessionView.
//
// That matters more than it looks. The store interns repeated message content, so a real
// proxy serving this document holds ~9MB of live heap behind it, not 105MB — and live heap
// sets the GC goal, which sets how much garbage the streamed path may accumulate before a
// collection. A hand-built view retains every copy, inflating the goal to the point that
// the streamed path's own churn looks like the thing being measured: 112MB against 20MB
// for the identical code. The fixture has to be interned to answer the question.
func benchView(b *testing.B) *pipeline.SessionView {
	b.Helper()
	store := session.New(0, 0, 100)
	b.Cleanup(store.Close)

	body := strings.Repeat("prompt text ", benchMsgBytes/12)
	for e := 0; e < benchEvents; e++ {
		msgs := make([]pipeline.InferenceMessage, benchMsgPerEvent)
		for i := range msgs {
			// Allocated fresh per event, as a parser's json.Unmarshal produces them, so
			// the sharing measured here is the store's and not the fixture's.
			msgs[i] = pipeline.InferenceMessage{Role: "user", Content: fmt.Sprintf("message %06d: %s", i, body)}
		}
		store.Append("s1", pipeline.SessionEvent{
			Phase:     pipeline.SessionRequest,
			Inference: &pipeline.InferenceExtension{Model: "m", Messages: msgs},
		})
	}
	return store.ViewTail("s1", defaultEventLimit)
}

// BenchmarkSnapshotHeap reports the heap one snapshot response costs the proxy, buffered
// against streamed. It is the number writeSessionView exists to move.
//
// HeapSys, not HeapAlloc: the cost here is not live data — the document is written and
// dropped, so HeapAlloc is unchanged either way. It is the peak the allocator had to
// reach, which Go then keeps as idle heap rather than returning promptly, so it is what
// RSS reflects and what an operator watching the proxy sees.
//
// RUN THE TWO SEPARATELY. HeapSys does not shrink within a process, so whichever runs
// second measures a heap the first already grew and reports a flattering ~0 — a trap this
// benchmark was written into once already:
//
//	go test ./sessionapi/ -bench 'SnapshotHeap/buffered' -run '^$' -benchtime 1x
//	go test ./sessionapi/ -bench 'SnapshotHeap/streamed' -run '^$' -benchtime 1x
//
// Reported rather than asserted, for the reason session.BenchmarkRetainedHeap gives: a
// threshold on a heap figure either flakes or is loose enough to miss what matters. The
// deterministic property — that the document reaches the writer in per-event pieces — is
// pinned by TestWriteSessionView_NeverBuffersTheWholeDocument instead.
//
// Measured on an M-series laptop, each in its own process, three runs each, on a 105MB
// document:
//
//	buffered   +243.9 .. +247.9 MB HeapSys
//	streamed    +11.9 ..  +16.0 MB HeapSys
func BenchmarkSnapshotHeap(b *testing.B) {
	b.Run("buffered", func(b *testing.B) {
		benchSnapshot(b, func(w io.Writer, v *pipeline.SessionView) error {
			return json.NewEncoder(w).Encode(v)
		})
	})
	b.Run("streamed", func(b *testing.B) {
		benchSnapshot(b, writeSessionView)
	})
}

func benchSnapshot(b *testing.B, write func(io.Writer, *pipeline.SessionView) error) {
	view := benchView(b)
	var counted countingWriter

	runtime.GC()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := write(&counted, view); err != nil {
			b.Fatal(err)
		}
	}

	runtime.GC()
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	b.ReportMetric(float64(int64(after.HeapSys)-int64(before.HeapSys))/1048576, "MB-HeapSys")
	b.ReportMetric(float64(counted.n)/float64(b.N)/1048576, "MB-response")
	runtime.KeepAlive(view)
}

type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }
