package session

import (
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// clearSpy is a Recorder that is also a Clearer. It notes what the store still held at the
// moment it was told, which must be nothing: a Clearer resets state keyed by session ids, and
// an id still resident could be appended to between the two halves.
type clearSpy struct {
	s           *Store
	cleared     int
	heldAtClear int
}

func (c *clearSpy) Record(string, *pipeline.SessionEvent) {}
func (c *clearSpy) Cleared() {
	c.cleared++
	c.heldAtClear = len(c.s.sessions) // s.mu is held: Cleared runs inside Clear's critical section
}

// Clear removes every session and the active-session mark: a mark left behind would file the
// next header-less request under a session the user just erased.
func TestClear_RemovesEverySession(t *testing.T) {
	s, clk := newProcStore()
	defer s.Close()
	s.Append("pending:bob-shell", ev())
	recordIn(s, clk, "s1")
	recordIn(s, clk, "s2")

	if n := s.Clear(); n != 3 {
		t.Fatalf("Clear() = %d, want 3 (the bucket, s1, s2)", n)
	}
	if l := s.ListSessions(); len(l) != 0 {
		t.Fatalf("ListSessions after Clear = %d rows", len(l))
	}
	// The field, not ActiveSession(): that already hides an id with no entry, while eviction and
	// Rekey read the field itself.
	if s.activeID != "" {
		t.Fatalf("activeID after Clear = %q", s.activeID)
	}
}

func TestClear_NotifiesClearers(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	spy := &clearSpy{s: s}
	s.AddRecorder(spy)
	s.Append("a", ev())
	s.Append("b", ev())
	s.Clear()
	if spy.cleared != 1 {
		t.Fatalf("Cleared called %d times, want 1", spy.cleared)
	}
	if spy.heldAtClear != 0 {
		t.Fatalf("the store still held %d sessions when it told its Clearers", spy.heldAtClear)
	}
}

// With every SeqSeeder reset, a session re-created after a clear numbers from 1 — the clear is
// the one boundary across which a session's numbering is meant to restart.
func TestClear_TheNextAppendStartsAtSeqOne(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	for range 3 {
		s.Append("a", ev())
	}
	s.Clear()
	s.Append("a", ev())
	v := s.View("a")
	if v == nil || len(v.Events) != 1 || v.Events[0].Seq != 1 {
		t.Fatalf("after Clear, a = %+v, want one event numbered 1", v)
	}
}

// A clear is not a disconnect: agentop's live stream stays attached and sees what comes next.
func TestClear_LeavesSubscribersConnected(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	sub, cancel := s.Subscribe()
	defer cancel()
	s.Clear()
	s.Append("a", ev())
	select {
	case e, ok := <-sub.Events():
		if !ok || e.SessionID != "a" {
			t.Fatalf("after Clear the subscriber got %+v (open=%v)", e, ok)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the subscriber saw nothing appended after Clear")
	}
}
