package session

import (
	"fmt"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// TestNoExpiryByDefault: ttl <= 0 means sessions are never dropped for being idle.
// Time-based expiry read as data loss — traffic vanished because someone stepped away,
// not because anything overflowed.
func TestNoExpiryByDefault(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Minute} {
		s := New(ttl, 500, 100)
		defer s.Close()
		s.Append("s1", pipeline.SessionEvent{})

		// Pretend a very long idle period by backdating the entry.
		s.mu.Lock()
		s.sessions["s1"].UpdatedAt = time.Now().Add(-72 * time.Hour)
		s.mu.Unlock()

		if v := s.View("s1"); v == nil {
			t.Errorf("ttl=%v: session gone after 72h idle; it must not expire on time", ttl)
		}
		if got := len(s.ListSessions()); got != 1 {
			t.Errorf("ttl=%v: List() = %d sessions, want 1", ttl, got)
		}
		s.Cleanup() // explicit sweep must also spare it
		if v := s.View("s1"); v == nil {
			t.Errorf("ttl=%v: Cleanup() deleted a session that cannot expire", ttl)
		}
	}
}

// TestExplicitTTLStillExpires: the capability is not gone, only the default.
func TestExplicitTTLStillExpires(t *testing.T) {
	s := New(30*time.Minute, 500, 100)
	defer s.Close()
	s.Append("s1", pipeline.SessionEvent{})

	s.mu.Lock()
	s.sessions["s1"].UpdatedAt = time.Now().Add(-31 * time.Minute)
	s.mu.Unlock()

	if v := s.View("s1"); v != nil {
		t.Error("an explicitly configured ttl no longer expires idle sessions")
	}
}

// TestNoReaperWhenNothingCanExpire: with ttl <= 0 the interval clamps to one second, so
// a reaper would wake every second forever to find nothing.
func TestNoReaperWhenNothingCanExpire(t *testing.T) {
	s := New(0, 500, 100)
	// Close() closes s.stop; a running backgroundCleanup would return on it. The point
	// here is simply that New with ttl=0 does not panic on a non-positive ticker and
	// that Close is safe whether or not the goroutine was started.
	s.Close()
	s.Close() // idempotent: must not panic on a double close
}

// TestSizeCapsBoundWhenSet: a cap that is set is honoured, in both dimensions.
//
// Named for what it asserts. It used to be TestSizeCapsStillBound, "removing time
// expiry must not remove the memory bound" — the same premise this branch retired from
// isExpired, the TTL field comment, Limits and CLAUDE.md, and the last copy of it. It
// is no longer true as stated: with max_events unset by default, removing time expiry
// does leave one long-lived session unbounded, which is why Limits argues for setting
// max_events or a ttl on that shape.
//
// The body was always narrower than the name. It passes explicit caps, so what it
// proves is that the store honours them when an operator asks — the case that still
// matters, and the one the new default makes reachable only on purpose.
func TestSizeCapsBoundWhenSet(t *testing.T) {
	s := New(0, 10, 3)
	defer s.Close()

	for i := 0; i < 25; i++ {
		s.Append("s1", pipeline.SessionEvent{})
	}
	if v := s.View("s1"); v != nil && len(v.Events) > 10 {
		t.Errorf("maxEvents ignored: %d events retained, cap is 10", len(v.Events))
	}
	for _, id := range []string{"s2", "s3", "s4", "s5"} {
		s.Append(id, pipeline.SessionEvent{})
	}
	if got := len(s.ListSessions()); got > 3 {
		t.Errorf("maxSessions ignored: %d sessions retained, cap is 3", got)
	}
}

// TestUnsetMaxEventsKeepsEveryEvent pins what the binaries now rely on: they pass
// cfg.Session.MaxEvents straight through, so an operator who has not set
// session.max_events gets a store that never evicts an event from a live session.
//
// The first event is asserted as well as the count, because FIFO eviction takes the
// BEGINNING of a session — on a long agent run, the inbound request that started it.
// That is what made a defaulted cap the wrong trade: the trim was invisible from the
// timeline, so the story just began later than it really had.
func TestUnsetMaxEventsKeepsEveryEvent(t *testing.T) {
	s := New(0, 0, 100) // maxEvents unset, as the binaries pass it by default
	defer s.Close()

	const n = 2000
	for i := 0; i < n; i++ {
		s.Append("s1", pipeline.SessionEvent{Host: fmt.Sprintf("h%04d", i)})
	}

	v := s.View("s1")
	if v == nil {
		t.Fatal("no session recorded")
	}
	if len(v.Events) != n {
		t.Errorf("retained %d events with maxEvents unset, want all %d", len(v.Events), n)
	}
	if len(v.Events) > 0 {
		if got, want := v.Events[0].Host, "h0000"; got != want {
			t.Errorf("oldest retained event is %q, want the very first %q", got, want)
		}
	}
}
