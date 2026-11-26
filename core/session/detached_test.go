package session

import (
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/pipeline"
)

// A detached model call's spend (cortex#2140) lands on its session's PluginSpend figures, and it
// is not activity: no event, no CostMicros, no UpdatedAt, and the active session stays.
func TestAddDetachedSpend_AddsSpendNotActivity(t *testing.T) {
	clk := &procTestClock{t: time.Unix(1_700_000_000, 0)}
	s := New(time.Hour, 0, 0, WithClock(clk.now))
	defer s.Close()
	priced := costRecord(t, event.Event{CostUSD: 0.25, Settled: true, Provenance: "configured",
		Spends: []event.SpendRecord{{Basis: event.BasisUSD, USD: 0.001}}})
	s.Append("s1", pipeline.SessionEvent{Phase: pipeline.SessionResponse, Plugins: priced})
	s.Append("s2", pipeline.SessionEvent{Phase: pipeline.SessionResponse, Plugins: priced})
	before := summaryOf(t, s, "s1")
	clk.advance(4 * time.Minute)

	s.AddDetachedSpend("s1", event.Figures{PluginSpendMicros: 19_500, PluginSpendUnits: 7, AvoidedMicros: 99, KeepAliveMicros: 5})
	s.AddDetachedSpend("gone", event.Figures{PluginSpendMicros: 1})

	got := summaryOf(t, s, "s1")
	if got.PluginSpendMicros != before.PluginSpendMicros+19_500 || got.PluginSpendUnits != before.PluginSpendUnits+7 {
		t.Errorf("plugin spend = %d micros / %d units, want %d / %d", got.PluginSpendMicros, got.PluginSpendUnits,
			before.PluginSpendMicros+19_500, before.PluginSpendUnits+7)
	}
	if got.AvoidedMicros != before.AvoidedMicros || got.KeepAliveMicros != before.KeepAliveMicros {
		t.Errorf("avoided/keep-alive moved: only the PluginSpend* fields are read")
	}
	if got.EventCount != before.EventCount || got.CostMicros != before.CostMicros || !got.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("events/cost/updated moved: a detached call is not activity")
	}
	if s.ActiveSession() != "s2" {
		t.Errorf("active = %q, want s2", s.ActiveSession())
	}
	if n := len(s.ListSessions()); n != 2 {
		t.Errorf("%d sessions, want 2: an unknown session is not created", n)
	}
}
