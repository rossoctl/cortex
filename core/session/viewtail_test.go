package session

import (
	"fmt"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// seedEvents appends n events whose Host names their index, so a tail can be
// identified by which indices it carries rather than only by its length.
func seedEvents(t *testing.T, s *Store, id string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		s.Append(id, pipeline.SessionEvent{Host: fmt.Sprintf("h%04d", i)})
	}
}

func hosts(v *pipeline.SessionView) []string {
	out := make([]string, 0, len(v.Events))
	for i := range v.Events {
		out = append(out, v.Events[i].Host)
	}
	return out
}

// ViewTail returns the END of the session, because that is the part an operator is
// looking at: a timeline is read from the most recent event backwards.
func TestViewTail_ReturnsTheMostRecentEvents(t *testing.T) {
	s := New(0, 0, 100)
	defer s.Close()
	seedEvents(t, s, "s1", 100)

	v := s.ViewTail("s1", 3)
	if v == nil {
		t.Fatal("no view")
	}
	if got, want := hosts(v), []string{"h0097", "h0098", "h0099"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("tail = %v, want %v", got, want)
	}
	if got, want := v.TotalEvents, 100; got != want {
		t.Errorf("TotalEvents = %d, want %d — a truncated view has to say so", got, want)
	}
}

// A tail long enough to cover the session is indistinguishable from View, TotalEvents
// included: a consumer that ignores the field must see exactly what it saw before the
// field existed.
func TestViewTail_WholeSessionLooksUntruncated(t *testing.T) {
	s := New(0, 0, 100)
	defer s.Close()
	seedEvents(t, s, "s1", 5)

	for _, limit := range []int{5, 6, 500} {
		v := s.ViewTail("s1", limit)
		if v == nil {
			t.Fatalf("limit %d: no view", limit)
		}
		if len(v.Events) != 5 {
			t.Errorf("limit %d: %d events, want 5", limit, len(v.Events))
		}
		if v.TotalEvents != 0 {
			t.Errorf("limit %d: TotalEvents = %d, want 0 (nothing was left out)", limit, v.TotalEvents)
		}
	}
}

// limit <= 0 must not mean "unlimited". A caller that forgot to set it gets the
// smallest answer, not the 1.1GB one — the same reasoning that made max_sessions: 0
// resolve to a default rather than to no cap.
func TestViewTail_NonPositiveLimitIsNotUnlimited(t *testing.T) {
	s := New(0, 0, 100)
	defer s.Close()
	seedEvents(t, s, "s1", 50)

	for _, limit := range []int{0, -1, -1000} {
		v := s.ViewTail("s1", limit)
		if v == nil {
			t.Fatalf("limit %d: no view", limit)
		}
		if len(v.Events) != 1 {
			t.Errorf("limit %d returned %d events, want 1", limit, len(v.Events))
		}
		if v.TotalEvents != 50 {
			t.Errorf("limit %d: TotalEvents = %d, want 50", limit, v.TotalEvents)
		}
	}
}

// The tail is a copy: a caller holding it must not see later appends, and must not be
// able to mutate what the store keeps. View already promises this; so does ViewTail.
func TestViewTail_IsACopy(t *testing.T) {
	s := New(0, 0, 100)
	defer s.Close()
	seedEvents(t, s, "s1", 3)

	v := s.ViewTail("s1", 3)
	v.Events[0].Host = "mutated"
	s.Append("s1", pipeline.SessionEvent{Host: "h0003"})

	if len(v.Events) != 3 {
		t.Errorf("the held view grew to %d events", len(v.Events))
	}
	if got := s.ViewTail("s1", 4).Events[0].Host; got != "h0000" {
		t.Errorf("store event was mutated through the view: %q", got)
	}
}

// Unknown and expired sessions 404 the same way View makes them.
func TestViewTail_UnknownSession(t *testing.T) {
	s := New(0, 0, 100)
	defer s.Close()
	if v := s.ViewTail("nope", 10); v != nil {
		t.Errorf("unknown session returned a view: %+v", v)
	}
}
