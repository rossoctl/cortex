package archive

import (
	"fmt"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// A segment must rotate on size, not only on idleness (#1566): a session
// busy all day kept one segment growing without limit, and every backward
// page decoded it from the first event.
func TestSegment_RotatesOnEventCount(t *testing.T) {
	base := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	a, err := Open(t.TempDir(), WithClock(func() time.Time { return base }),
		func(a *Archive) { a.tickEvery = time.Hour })
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	// Written on the writer goroutine, as the queue would: 10.5k events is
	// past defaultQueueDepth, so Record would drop most of them — the
	// rotation itself is what is under test, not the queue.
	const total = maxSegmentEvents + 500
	a.inWriter(func() {
		for i := 1; i <= total; i++ {
			a.write("s", &pipeline.SessionEvent{
				At: base, Seq: uint64(i), SessionID: "s",
				Phase: pipeline.SessionRequest, Host: "h",
				Inference: &pipeline.InferenceExtension{Model: "m", Messages: []pipeline.InferenceMessage{
					{Role: "user", Content: fmt.Sprintf("event %d", i)},
				}},
			})
		}
	})
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.sessions["s"]
	if s.w == nil {
		t.Fatal("no open segment after writes")
	}
	openInfo := s.w.info()
	closed := s.meta.Segments
	if len(closed) == 0 {
		t.Fatalf("one segment held all %d events — no rotation on size", total)
	}
	// Every event is in exactly one segment: the closed ones plus the open one.
	sum := openInfo.Events
	for _, seg := range closed {
		sum += seg.Events
	}
	if sum != total {
		t.Errorf("segments hold %d events, want %d — a rotation lost or duplicated events", sum, total)
	}
	// Seq continuity across the boundary: each closed segment's LastSeq is
	// followed by the next's FirstSeq.
	for i := 1; i < len(closed); i++ {
		if closed[i].FirstSeq != closed[i-1].LastSeq+1 {
			t.Errorf("segment %d starts at seq %d, want %d — a gap or overlap at a rotation",
				i, closed[i].FirstSeq, closed[i-1].LastSeq+1)
		}
	}
	if closed[0].FirstSeq != 1 {
		t.Errorf("first segment starts at seq %d, want 1", closed[0].FirstSeq)
	}
	if openInfo.FirstSeq != closed[len(closed)-1].LastSeq+1 {
		t.Errorf("open segment starts at seq %d, want %d", openInfo.FirstSeq, closed[len(closed)-1].LastSeq+1)
	}
}
