package session

import (
	"fmt"
	"testing"
)

// ViewPage(id, 0, n) has to BE ViewTail(id, n), because that equivalence is what lets a
// paging client use one call for its first request and every later one. ViewTail delegates
// here for the same reason.
func TestViewPage_ZeroCursorIsTheTail(t *testing.T) {
	s := New(0, 0, 100)
	defer s.Close()
	seedEvents(t, s, "s1", 100)

	page := s.ViewPage("s1", 0, 10)
	if page == nil {
		t.Fatal("no view")
	}
	// Asserted against the CONCRETE tail, not against ViewTail's output: ViewTail now
	// delegates here, so comparing the two would be comparing this call to itself and would
	// pass just as well if both returned the wrong ten events.
	want := []string{"h0090", "h0091", "h0092", "h0093", "h0094", "h0095", "h0096", "h0097", "h0098", "h0099"}
	if got := hosts(page); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("page = %v, want %v", got, want)
	}
	if got, want := page.TotalEvents, 100; got != want {
		t.Errorf("TotalEvents = %d, want %d", got, want)
	}
	if page.OldestSeq == 0 {
		t.Error("OldestSeq unset on a truncated view")
	}

	// And the delegation itself still holds.
	if tail := s.ViewTail("s1", 10); fmt.Sprint(hosts(tail)) != fmt.Sprint(want) {
		t.Errorf("ViewTail = %v, want %v", hosts(tail), want)
	}
}

// A cursor ABOVE everything held returns the tail, which is the store being consistent
// rather than a bug: every event it holds does precede that cursor.
//
// Pinned because a client can hold such a cursor, and the consequence is not local. Seq
// restarts at 1 for a re-created session, so a cursor from a previous incarnation asks for
// events "before" a number the new incarnation has not reached — and gets the newest events
// it has. abctl refuses to prepend those by comparing timestamps rather than Seq
// (applyOlderPage); this test records the store behaviour that makes the check necessary.
func TestViewPage_CursorAboveEverythingHeldReturnsTheTail(t *testing.T) {
	s := New(0, 0, 100)
	defer s.Close()
	seedEvents(t, s, "s1", 20)

	v := s.ViewPage("s1", 1_000_000, 5)
	if v == nil {
		t.Fatal("no view")
	}
	want := []string{"h0015", "h0016", "h0017", "h0018", "h0019"}
	if got := hosts(v); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("page = %v, want the tail %v", got, want)
	}
}

// The page ends just BEFORE the cursor: the caller already holds the cursor event, so
// returning it again would duplicate a row in their timeline.
func TestViewPage_ReturnsTheEventsBeforeTheCursor(t *testing.T) {
	s := New(0, 0, 100)
	defer s.Close()
	seedEvents(t, s, "s1", 100)

	tail := s.ViewTail("s1", 10) // h0090..h0099
	older := s.ViewPage("s1", tail.Events[0].Seq, 10)

	want := []string{"h0080", "h0081", "h0082", "h0083", "h0084", "h0085", "h0086", "h0087", "h0088", "h0089"}
	if got := hosts(older); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("page = %v, want %v", got, want)
	}
}

// The property the whole feature rests on: paging backward from the tail reaches every
// event the session holds, exactly once, in order — and terminates.
//
// This is what could not be done before. The API caps one response at 2000 events, so on
// a session past that size every earlier event was unreachable through any request; the
// store held them and nothing could read them.
func TestViewPage_WalksTheWholeSessionExactlyOnce(t *testing.T) {
	const total = 250
	s := New(0, 0, 100)
	defer s.Close()
	seedEvents(t, s, "s1", total)

	var seen []string
	cursor := uint64(0)
	for pages := 0; ; pages++ {
		if pages > total {
			t.Fatal("paging did not terminate")
		}
		v := s.ViewPage("s1", cursor, 7)
		if v == nil {
			t.Fatal("no view")
		}
		if len(v.Events) == 0 {
			break
		}
		// Prepend: pages arrive newest-first, the timeline reads oldest-first.
		seen = append(hosts(v), seen...)
		cursor = v.Events[0].Seq
		// A client stops when it has the oldest event the store holds. Without
		// OldestSeq it could only guess, and would keep asking for empty pages.
		if v.OldestSeq != 0 && v.Events[0].Seq == v.OldestSeq {
			break
		}
	}

	if len(seen) != total {
		t.Fatalf("paged %d events, want %d", len(seen), total)
	}
	for i, host := range seen {
		if want := fmt.Sprintf("h%04d", i); host != want {
			t.Fatalf("event %d = %q, want %q — pages are out of order or overlap", i, host, want)
		}
	}
}

// A cursor can name an event that has since been evicted. That must page from wherever
// the store now begins rather than erroring or returning nothing: the client's cursor is
// older than the data, which is a normal race between paging and eviction, not a bug.
func TestViewPage_CursorAtAnEvictedEventStillPages(t *testing.T) {
	s := New(0, 5, 100)
	defer s.Close()
	seedEvents(t, s, "s1", 5)

	// Captured while it is still held, then evicted by the appends below: the cap is 5,
	// so after 15 events only the last 5 survive and this Seq names none of them.
	stale := s.ViewPage("s1", 0, 5).Events[2].Seq
	seedEvents(t, s, "s1", 10)

	if held := s.ViewPage("s1", 0, 5); held.Events[0].Seq <= stale {
		t.Fatalf("test setup: cursor Seq %d is still held (oldest held is %d)",
			stale, held.Events[0].Seq)
	}

	v := s.ViewPage("s1", stale, 5)
	if v == nil {
		t.Fatal("no view")
	}
	// Everything the store still holds is NEWER than the cursor, so there is nothing
	// before it to return — but the answer is an empty PAGE carrying the metadata a
	// client needs to see it has reached the bottom, not a 404 and not an error.
	if len(v.Events) != 0 {
		t.Errorf("page = %v, want empty (cursor precedes everything held)", hosts(v))
	}
	if v.OldestSeq == 0 {
		t.Error("OldestSeq unset: a client cannot tell it has reached the bottom")
	}
}

// The bottom of the session reports itself. OldestSeq is the oldest event the store HOLDS,
// so a page containing it tells the client to stop asking.
func TestViewPage_OldestSeqNamesTheOldestHeldEvent(t *testing.T) {
	s := New(0, 0, 100)
	defer s.Close()
	seedEvents(t, s, "s1", 30)

	first := s.ViewPage("s1", 0, 30)
	if first.OldestSeq != 0 {
		t.Errorf("OldestSeq = %d on a view holding the whole session, want 0 (nothing left out)",
			first.OldestSeq)
	}

	partial := s.ViewPage("s1", 0, 5)
	if partial.OldestSeq == 0 {
		t.Fatal("OldestSeq unset on a truncated view")
	}
	oldest := s.ViewPage("s1", partial.Events[0].Seq, 100)
	if got := oldest.Events[0].Seq; got != partial.OldestSeq {
		t.Errorf("walked back to Seq %d, but OldestSeq promised %d", got, partial.OldestSeq)
	}
}

// Seq must stay ascending in the retained slice even when the intent pin moves an old
// event to the front, because ViewPage binary-searches on it. A pinned intent is the one
// case where the kept events are not a contiguous range.
func TestViewPage_SeqStaysOrderedAcrossAPinnedTrim(t *testing.T) {
	s := New(0, 5, 100)
	defer s.Close()

	s.Append("s1", intent("message/send")) // Seq 1, pinned once it falls out of the window
	seedEvents(t, s, "s1", 20)             // forces eviction well past the intent

	v := s.ViewPage("s1", 0, 5)
	if v == nil {
		t.Fatal("no view")
	}
	if len(v.Events) < 2 {
		t.Fatalf("expected the pinned intent plus a tail, got %d events", len(v.Events))
	}
	for i := 1; i < len(v.Events); i++ {
		if v.Events[i-1].Seq >= v.Events[i].Seq {
			t.Fatalf("Seq not ascending at %d: %d then %d — ViewPage's search would misplace a cursor",
				i, v.Events[i-1].Seq, v.Events[i].Seq)
		}
	}
	// And the gap is real: the pinned intent is far older than the event after it.
	if v.Events[1].Seq-v.Events[0].Seq < 2 {
		t.Errorf("expected a Seq gap after the pinned intent, got %d then %d",
			v.Events[0].Seq, v.Events[1].Seq)
	}
}

// Unknown sessions return nil, so the handler 404s rather than serving an empty page that
// looks like a session with no events.
func TestViewPage_UnknownSession(t *testing.T) {
	s := New(0, 0, 100)
	defer s.Close()
	if v := s.ViewPage("nope", 0, 10); v != nil {
		t.Errorf("unknown session returned a view: %+v", v)
	}
}

// Seq counts from 1 and never repeats, including across eviction: a reused number would
// send a client holding an old cursor to the wrong place in the timeline.
func TestAppend_SeqIsMonotonicAcrossEviction(t *testing.T) {
	s := New(0, 3, 100)
	defer s.Close()
	seedEvents(t, s, "s1", 10)

	v := s.ViewPage("s1", 0, 3)
	got := []uint64{v.Events[0].Seq, v.Events[1].Seq, v.Events[2].Seq}
	if want := []uint64{8, 9, 10}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Seq = %v, want %v — numbers must not restart when events are evicted", got, want)
	}
}
