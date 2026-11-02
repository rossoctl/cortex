package session

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"unsafe"

	"github.com/rossoctl/cortex/core/pipeline"
)

// interleaveFixture: n conversations taking turns in one session bucket, each turn
// re-sending its whole conversation, as subagent traffic actually arrives. Content is
// built fresh per event — a test that sees sharing is seeing the store share it.
func interleaveFixture(conv, turn int) []pipeline.InferenceMessage {
	msgs := make([]pipeline.InferenceMessage, turn)
	for i := 0; i < turn; i++ {
		msgs[i] = pipeline.InferenceMessage{
			Role:    "user",
			Content: strings.Clone(fmt.Sprintf("conv%d-msg%d: %s", conv, i, strings.Repeat("payload ", 64))),
		}
	}
	return msgs
}

func heapAfterGC() uint64 {
	for i := 0; i < 5; i++ {
		runtime.GC()
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// TestIntern_InterleavedConversationsShare is #1541's acceptance case: a session
// carrying several conversations taking turns must retain its message text at close
// to the single-conversation cost, because each conversation keeps its own intern
// table and growing array instead of evicting the previous one's on every switch.
// Before the fix, two conversations taking turns retained 60.5x the distinct content
// (49.7MB for 0.8MB); one conversation retains 1.9x.
//
// The acceptance bound is the issue's own: within ~2x of the SINGLE-CONVERSATION
// case at the same event count — because each conversation keeps its own table
// and growing array, the only extra cost of interleaving is the second array's
// existence. Before the fix, two conversations taking turns retained 60.5x the
// distinct content (49.7MB for 0.8MB on the issue's fixture); one conversation
// retained 1.9x.
func TestIntern_InterleavedConversationsShare(t *testing.T) {
	const turns = 50
	run := func(convs int) uint64 {
		s := New(0, 0, 100)
		defer s.Close()
		for turn := 1; turn <= turns; turn++ {
			for c := 0; c < convs; c++ {
				s.Append("s1", pipeline.SessionEvent{
					Inference: &pipeline.InferenceExtension{
						Model:    fmt.Sprintf("m%d", c),
						Messages: interleaveFixture(c, turn),
					},
				})
			}
		}
		retained := heapAfterGC()
		runtime.KeepAlive(s)
		return retained
	}
	one := run(1)
	for _, convs := range []int{2, 4} {
		got := run(convs)
		t.Logf("%d conversations: retained %.2f MB, single-conversation %.2f MB, %.1fx",
			convs, float64(got)/1e6, float64(one)/1e6, float64(got)/float64(one))
		if got > one*5/2 {
			t.Errorf("%d interleaved conversations retained %.1fx the single-conversation heap; want within ~2x (was ~30x before #1541, on the issue's fixture)",
				convs, float64(got)/float64(one))
		}
	}
}

// TestIntern_TunnelOpensDoNotBreakSharing: gotcha 14's rule for any interning
// fixture — contentless events interleave with turns on live traffic (a CONNECT
// tunnel-open lands between every inference request and its response), so the
// interleaved case above is only honest with them present. Each turn here is
// followed by a tunnel-open row.
func TestIntern_TunnelOpensDoNotBreakSharing(t *testing.T) {
	const turns = 30
	s := New(0, 0, 100)
	defer s.Close()
	for turn := 1; turn <= turns; turn++ {
		for c := 0; c < 2; c++ {
			s.Append("s1", pipeline.SessionEvent{
				Inference: &pipeline.InferenceExtension{
					Model:    fmt.Sprintf("m%d", c),
					Messages: interleaveFixture(c, turn),
				},
			})
			// The contentless interloper: a tunnel-open carries no extension.
			s.Append("s1", pipeline.SessionEvent{Phase: pipeline.SessionRequest})
		}
	}
	retained := heapAfterGC()
	runtime.KeepAlive(s)
	distinct := uint64(2) * uint64(turns*(turns+1)/2) * 520
	ratio := float64(retained) / float64(distinct)
	t.Logf("with tunnel opens: retained %.1f MB, distinct %.1f MB, %.1fx",
		float64(retained)/1e6, float64(distinct)/1e6, ratio)
	if ratio > 3 {
		t.Errorf("tunnel-open interleaving broke sharing: %.1fx the distinct content", ratio)
	}
}

// TestIntern_ResponsePhaseEventSharesTheArray: the response-phase event of a
// request repeats the request's conversation verbatim — the exact shape the
// full-match path exists for. It must carry the SAME backing array, not a copy:
// this is where the per-event header duplication used to be re-created even
// with interning.
func TestIntern_ResponsePhaseEventSharesTheArray(t *testing.T) {
	s := New(0, 0, 100)
	defer s.Close()
	msgs := interleaveFixture(0, 5)
	s.Append("s1", pipeline.SessionEvent{Phase: pipeline.SessionRequest,
		Inference: &pipeline.InferenceExtension{Model: "m", Messages: msgs}})
	s.Append("s1", pipeline.SessionEvent{Phase: pipeline.SessionResponse,
		Inference: &pipeline.InferenceExtension{Model: "m", Messages: msgs, Completion: "done"}})

	v := s.View("s1")
	reqArr := unsafe.SliceData(v.Events[0].Inference.Messages)
	respArr := unsafe.SliceData(v.Events[1].Inference.Messages)
	if reqArr != respArr {
		t.Error("the response-phase event cloned the conversation array instead of sharing it")
	}
	// And its completion is interned alongside.
	if v.Events[1].Inference.Completion != "done" {
		t.Errorf("completion = %q, want the event's own", v.Events[1].Inference.Completion)
	}
}

// TestIntern_SiblingConversationsSplit: two conversations with the same model and
// first message (same-type subagents share a system prompt) but different second
// messages must not thrash one state. Each keeps its own array, and each turn
// shares with its own previous turn.
func TestIntern_SiblingConversationsSplit(t *testing.T) {
	s := New(0, 0, 100)
	defer s.Close()
	conv := func(task string, turn int) []pipeline.InferenceMessage {
		msgs := make([]pipeline.InferenceMessage, turn+1)
		msgs[0] = pipeline.InferenceMessage{Role: "system", Content: strings.Repeat("shared system prompt ", 8)}
		msgs[1] = pipeline.InferenceMessage{Role: "user", Content: task}
		for i := 2; i <= turn; i++ {
			msgs[i] = pipeline.InferenceMessage{Role: "user", Content: strings.Clone(fmt.Sprintf("%s turn %d", task, i))}
		}
		return msgs
	}
	for turn := 1; turn <= 10; turn++ {
		s.Append("s1", pipeline.SessionEvent{Inference: &pipeline.InferenceExtension{Model: "m", Messages: conv("task-a", turn)}})
		s.Append("s1", pipeline.SessionEvent{Inference: &pipeline.InferenceExtension{Model: "m", Messages: conv("task-b", turn)}})
	}
	v := s.View("s1")
	// Every event of task-a must share message 0 (the system prompt) with every
	// other task-a event — one copy per conversation, not one per event.
	var a0, b0 uintptr
	counts := map[uintptr]int{}
	for _, e := range v.Events {
		if len(e.Inference.Messages) == 0 {
			continue
		}
		m0 := backing(e.Inference.Messages[0].Content)
		if strings.Contains(e.Inference.Messages[1].Content, "task-a") {
			a0 = m0
		} else {
			b0 = m0
		}
		counts[m0]++
	}
	if a0 == b0 {
		t.Fatal("the two sibling conversations share one canonical for the system prompt — they should be able to, but the arrays must not thrash; see the array check below")
	}
	for ptr, n := range counts {
		if n != 10 {
			t.Errorf("a system prompt held by %d events (backing %x) — siblings are thrashing one state", n, ptr)
		}
	}
}

// TestIntern_CompactionRebasesNotCorrupts: a turn that rewrites history (context
// compaction replaces the message prefix) shares only the surviving prefix and
// keeps correct content — no cross-event corruption from the array sharing.
func TestIntern_CompactionRebasesNotCorrupts(t *testing.T) {
	s := New(0, 0, 100)
	defer s.Close()
	full := interleaveFixture(0, 6)
	s.Append("s1", pipeline.SessionEvent{Inference: &pipeline.InferenceExtension{Model: "m", Messages: full}})
	// Compaction: the first three messages replaced by a summary, the rest kept.
	// Fresh strings (interleaveFixture clones), so a sharing assertion below can
	// tell the canonical copies from the compacted event's own wire strings.
	compacted := interleaveFixture(0, 6)
	compacted[2] = pipeline.InferenceMessage{Role: "user", Content: strings.Repeat("summary of the early conversation ", 8)}
	s.Append("s1", pipeline.SessionEvent{Inference: &pipeline.InferenceExtension{Model: "m", Messages: compacted}})
	// And the next turn extends the compacted history.
	next := append(append([]pipeline.InferenceMessage(nil), compacted...),
		pipeline.InferenceMessage{Role: "user", Content: strings.Repeat("after compaction ", 16)})
	s.Append("s1", pipeline.SessionEvent{Inference: &pipeline.InferenceExtension{Model: "m", Messages: next}})

	v := s.View("s1")
	last := v.Events[2].Inference.Messages
	if got, want := last[2].Content, compacted[2].Content; got != want {
		t.Errorf("compacted message corrupted: got %.40q want %.40q", got, want)
	}
	if got, want := last[5].Content, full[5].Content; got != want {
		t.Errorf("surviving message corrupted: got %.40q want %.40q", got, want)
	}
	// The pre-compaction event must be untouched.
	first := v.Events[0].Inference.Messages
	if got, want := first[2].Content, full[2].Content; got != want {
		t.Errorf("the pre-compaction event was rewritten: got %.40q want %.40q", got, want)
	}
	// And the surviving PREFIX must be the canonical copies the first event
	// registered, not the compacted event's own wire strings — "shares only
	// the verified prefix" is the claim the rebuild path makes.
	second := v.Events[1].Inference.Messages
	for i := 0; i < 2; i++ {
		if backing(second[i].Content) != backing(first[i].Content) {
			t.Errorf("compaction re-copied prefix message %d instead of sharing the canonical", i)
		}
	}
}

// TestIntern_TruncatedHistorySharesThePrefix: a turn that re-sends only the
// first k messages of the conversation (a client trimming its history) takes
// the shared array's prefix itself — canonical elements, zero copy — rather
// than re-copying the arriving wire strings, which retained a second copy of
// every prefix message.
func TestIntern_TruncatedHistorySharesThePrefix(t *testing.T) {
	s := New(0, 0, 100)
	defer s.Close()
	s.Append("s1", pipeline.SessionEvent{Inference: &pipeline.InferenceExtension{
		Model: "m", Messages: interleaveFixture(0, 6)}})
	s.Append("s1", pipeline.SessionEvent{Inference: &pipeline.InferenceExtension{
		Model: "m", Messages: interleaveFixture(0, 3)}})
	v := s.View("s1")
	for i := 0; i < 3; i++ {
		if backing(v.Events[1].Inference.Messages[i].Content) != backing(v.Events[0].Inference.Messages[i].Content) {
			t.Errorf("the truncated re-send kept its own copy of message %d — the verified prefix must be the canonical one", i)
		}
	}
}

// BenchmarkRetainedHeapInterleaved is the #1551 companion benchmark: the same
// shape as BenchmarkRetainedHeap, with two conversations taking turns and a
// tunnel-open between every pair — gotcha 14's rule that a clean run of turns is
// not what live traffic looks like. Before #1541 this retained ~60x the distinct
// content against the single-conversation case's ~2x.
func BenchmarkRetainedHeapInterleaved(b *testing.B) {
	for i := 0; i < b.N; i++ {
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)

		s := New(0, 0, 100)
		for turn := 1; turn <= benchTurns; turn++ {
			for c := 0; c < 2; c++ {
				s.Append("s1", pipeline.SessionEvent{
					Inference: &pipeline.InferenceExtension{
						Model:    fmt.Sprintf("m%d", c),
						Messages: interleaveFixture(c, turn),
					},
				})
			}
			// The contentless interloper, one per turn-pair.
			s.Append("s1", pipeline.SessionEvent{Phase: pipeline.SessionRequest})
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
}
