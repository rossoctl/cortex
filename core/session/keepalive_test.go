package session

import (
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/pipeline"
)

// A keep-alive ping's spend lands on its session's row, by basis, and counts its pings — and it is
// not activity: no event, no tokens, no CostMicros, no UpdatedAt, and the active session stays.
func TestAddKeepAlive_AddsSpendNotActivity(t *testing.T) {
	clk := &procTestClock{t: time.Unix(1_700_000_000, 0)}
	s := New(time.Hour, 0, 0, WithClock(clk.now))
	defer s.Close()
	priced := costRecord(t, event.Event{CostUSD: 0.25, Settled: true, Provenance: "configured"})
	s.Append("s1", pipeline.SessionEvent{Phase: pipeline.SessionResponse, Plugins: priced})
	s.Append("s2", pipeline.SessionEvent{Phase: pipeline.SessionResponse, Plugins: priced})
	before := summaryOf(t, s, "s1")
	clk.advance(4 * time.Minute)

	s.AddKeepAlive("s1", event.Figures{KeepAliveMicros: 558, AvoidedMicros: 99})
	s.AddKeepAlive("s1", event.Figures{KeepAliveUnits: 7, KeepAliveAPIEquivalentMicros: 40, KeepAliveUnknownMicros: 3})

	got := summaryOf(t, s, "s1")
	if got.KeepAliveMicros != 558 || got.KeepAliveUnits != 7 || got.KeepAliveAPIEquivalentMicros != 40 ||
		got.KeepAliveUnknownMicros != 3 || got.KeepAlivePings != 2 {
		t.Errorf("keep-alive = %d/%d/%d/%d, %d pings; want 558/7/40/3, 2 pings", got.KeepAliveMicros, got.KeepAliveUnits,
			got.KeepAliveAPIEquivalentMicros, got.KeepAliveUnknownMicros, got.KeepAlivePings)
	}
	if got.AvoidedMicros != before.AvoidedMicros {
		t.Errorf("AvoidedMicros = %d, want %d: only the KeepAlive* fields of a ping are read", got.AvoidedMicros, before.AvoidedMicros)
	}
	if got.EventCount != before.EventCount || got.CostMicros != before.CostMicros || got.TotalTokens != before.TotalTokens {
		t.Errorf("events/cost/tokens moved: %d/%d/%d, want %d/%d/%d", got.EventCount, got.CostMicros, got.TotalTokens,
			before.EventCount, before.CostMicros, before.TotalTokens)
	}
	if !got.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("UpdatedAt moved to %v: a ping is not activity", got.UpdatedAt)
	}
	if s.ActiveSession() != "s2" {
		t.Errorf("active = %q, want s2: a ping must not make its session active", s.ActiveSession())
	}
}

// A ping for a session the store does not hold creates none.
func TestAddKeepAlive_UnknownSessionIsDropped(t *testing.T) {
	s := New(time.Hour, 0, 0)
	defer s.Close()
	s.AddKeepAlive("gone", event.Figures{KeepAliveMicros: 1})
	if n := len(s.ListSessions()); n != 0 {
		t.Errorf("%d sessions listed, want 0", n)
	}
}

// A trim sheds the events' figures and leaves the pings' alone: they are not over Events.
func TestAddKeepAlive_SurvivesATrim(t *testing.T) {
	s := New(time.Hour, 3, 0)
	defer s.Close()
	priced := costRecord(t, event.Event{CostUSD: 0.25, Settled: true, Provenance: "configured"})
	s.Append("s1", pipeline.SessionEvent{Phase: pipeline.SessionResponse, Plugins: priced})
	s.AddKeepAlive("s1", event.Figures{KeepAliveMicros: 558})
	for range 10 {
		s.Append("s1", pipeline.SessionEvent{Phase: pipeline.SessionResponse, Plugins: priced})
	}
	if got := summaryOf(t, s, "s1"); got.KeepAliveMicros != 558 || got.KeepAlivePings != 1 {
		t.Errorf("after a trim: %d micros, %d pings; want 558, 1", got.KeepAliveMicros, got.KeepAlivePings)
	}
}
