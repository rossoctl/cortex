package session

import (
	"testing"
	"time"

	"github.com/rossoctl/rossocortex/authbridge/authlib/pipeline"
)

// intent fabricates the same shape SessionView.LastIntent recognizes:
// inbound A2A request event. Tests below assert that this exact shape
// survives FIFO eviction so IBAC's LastIntent never returns nil after
// chatty traffic floods the buffer.
func intent(method string) pipeline.SessionEvent {
	return pipeline.SessionEvent{
		At:        time.Now(),
		Direction: pipeline.Inbound,
		Phase:     pipeline.SessionRequest,
		A2A:       &pipeline.A2AExtension{Method: method},
	}
}

// chatter fabricates an outbound non-intent event. Stand-in for the
// OTel exports / inference calls / tool calls that flooded the bucket
// in the GSM8K reproducer.
func chatter(tag string) pipeline.SessionEvent {
	return pipeline.SessionEvent{
		At:        time.Now(),
		Direction: pipeline.Outbound,
		Phase:     pipeline.SessionRequest,
		MCP:       &pipeline.MCPExtension{Method: tag},
	}
}

// TestStore_PinsIntentAcrossEviction reproduces the GSM8K bug at the
// store level: an inbound A2A intent followed by enough chatty
// outbound traffic to overflow maxEvents. Pre-fix, the intent rolled
// out the back of the FIFO and LastIntent returned nil. Post-fix the
// intent must survive — that's the property IBAC depends on.
func TestStore_PinsIntentAcrossEviction(t *testing.T) {
	s := New(5*time.Minute, 5, 0)

	s.Append("sess", intent("solve-math"))
	for i := 0; i < 20; i++ {
		s.Append("sess", chatter("chat"))
	}

	v := s.View("sess")
	if v == nil {
		t.Fatal("View returned nil")
	}
	if len(v.Events) != 5 {
		t.Fatalf("Events len = %d, want 5 (capped at maxEvents)", len(v.Events))
	}
	last := v.LastIntent()
	if last == nil {
		t.Fatal("LastIntent() = nil; intent must survive eviction even when buried under chatter")
	}
	if last.A2A == nil || last.A2A.Method != "solve-math" {
		t.Errorf("LastIntent A2A.Method = %v, want solve-math", last.A2A)
	}
}

// TestStore_NoIntent_FifoEvictionUnchanged is the regression guard:
// a bucket with no intent in it must keep behaving exactly like
// today's FIFO. Pinning logic must not change non-intent buckets.
func TestStore_NoIntent_FifoEvictionUnchanged(t *testing.T) {
	s := New(5*time.Minute, 3, 0)

	for i := 0; i < 5; i++ {
		ev := chatter(string(rune('a' + i)))
		s.Append("sess", ev)
	}

	v := s.View("sess")
	if len(v.Events) != 3 {
		t.Fatalf("Events len = %d, want 3", len(v.Events))
	}
	// Oldest two evicted: surviving methods are c, d, e.
	want := []string{"c", "d", "e"}
	for i, w := range want {
		if v.Events[i].MCP.Method != w {
			t.Errorf("Events[%d].MCP.Method = %q, want %q", i, v.Events[i].MCP.Method, w)
		}
	}
}

// TestStore_OnlyMostRecentIntentPinned: a bucket with multiple
// intents (multi-turn conversation) must pin only the LATEST.
// Older intents evict via normal FIFO so pathological traffic
// (many turns + huge fan-out) can't starve the buffer with stale
// intents.
func TestStore_OnlyMostRecentIntentPinned(t *testing.T) {
	s := New(5*time.Minute, 3, 0)

	s.Append("sess", intent("turn-1"))
	s.Append("sess", chatter("c1"))
	s.Append("sess", intent("turn-2"))
	for i := 0; i < 10; i++ {
		s.Append("sess", chatter("flood"))
	}

	v := s.View("sess")
	if len(v.Events) != 3 {
		t.Fatalf("Events len = %d, want 3", len(v.Events))
	}
	last := v.LastIntent()
	if last == nil || last.A2A == nil || last.A2A.Method != "turn-2" {
		t.Errorf("LastIntent.A2A.Method = %v, want turn-2 (only the latest intent is pinned)", last)
	}
	// Older intent should be gone (evicted by FIFO with only the
	// latest pinned).
	for _, e := range v.Events {
		if e.A2A != nil && e.A2A.Method == "turn-1" {
			t.Errorf("turn-1 intent survived; only the most recent intent should be pinned")
		}
	}
}

// TestStore_IntentInKeepWindow_TrivialFifo: when the intent is
// already in the keep window (i.e. plain FIFO would preserve it
// anyway), the result is byte-identical to plain FIFO. Catches
// regressions where the pin path mutates or reorders unnecessarily.
func TestStore_IntentInKeepWindow_TrivialFifo(t *testing.T) {
	s := New(5*time.Minute, 3, 0)

	s.Append("sess", chatter("a"))
	s.Append("sess", chatter("b"))
	s.Append("sess", intent("recent-intent"))
	s.Append("sess", chatter("c"))
	s.Append("sess", chatter("d"))

	v := s.View("sess")
	if len(v.Events) != 3 {
		t.Fatalf("Events len = %d, want 3", len(v.Events))
	}
	// Plain FIFO would preserve [intent, c, d] — and the intent is
	// already in the keep window, so the pin path takes the fast
	// branch and produces exactly that.
	if v.Events[0].A2A == nil || v.Events[0].A2A.Method != "recent-intent" {
		t.Errorf("Events[0] = %v, want recent-intent (FIFO order preserved)", v.Events[0])
	}
	if v.Events[1].MCP == nil || v.Events[1].MCP.Method != "c" {
		t.Errorf("Events[1].MCP.Method = %v, want c", v.Events[1].MCP)
	}
	if v.Events[2].MCP == nil || v.Events[2].MCP.Method != "d" {
		t.Errorf("Events[2].MCP.Method = %v, want d", v.Events[2].MCP)
	}
}

// TestStore_PinnedIntentChronologicalOrder: with the intent pinned
// from the eviction prefix, the surviving slice must still be
// chronologically ordered (At ascending). Consumers like abctl
// render events in slice order assuming monotonic time; a
// non-monotonic result here would break the timeline UI.
func TestStore_PinnedIntentChronologicalOrder(t *testing.T) {
	s := New(5*time.Minute, 3, 0)

	// Append intent first so it has the earliest timestamp, then
	// flood with chatter. Pin path keeps intent at index 0 and the
	// most-recent two chatters at indices 1, 2.
	intentEv := intent("first")
	intentEv.At = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	s.Append("sess", intentEv)
	for i := 0; i < 10; i++ {
		ev := chatter("chat")
		ev.At = time.Date(2026, 1, 1, 12, 0, i+1, 0, time.UTC)
		s.Append("sess", ev)
	}

	v := s.View("sess")
	if len(v.Events) != 3 {
		t.Fatalf("Events len = %d, want 3", len(v.Events))
	}
	for i := 1; i < len(v.Events); i++ {
		if !v.Events[i].At.After(v.Events[i-1].At) {
			t.Errorf("Events[%d].At (%v) not after Events[%d].At (%v) — pin must preserve chronological order",
				i, v.Events[i].At, i-1, v.Events[i-1].At)
		}
	}
	if v.Events[0].A2A == nil || v.Events[0].A2A.Method != "first" {
		t.Errorf("Events[0] should be the pinned intent, got %v", v.Events[0])
	}
}

// TestStore_AllIntentsBucket: a pathological bucket made entirely
// of intent events must still respect maxEvents. Only the most
// recent intent is "protected" — the rest evict via plain FIFO.
// This bounds the worst case (intents alone cannot starve the buffer).
func TestStore_AllIntentsBucket(t *testing.T) {
	s := New(5*time.Minute, 3, 0)

	for i := 0; i < 10; i++ {
		s.Append("sess", intent(string(rune('a'+i))))
	}

	v := s.View("sess")
	if len(v.Events) != 3 {
		t.Fatalf("Events len = %d, want 3 (maxEvents must hold even for all-intent buckets)", len(v.Events))
	}
	last := v.LastIntent()
	if last == nil || last.A2A == nil || last.A2A.Method != "j" {
		t.Errorf("LastIntent.Method = %v, want j (most recent intent)", last)
	}
}
