package tui

import (
	"math"
	"testing"

	"github.com/rossoctl/cortex/core/cost/event"
)

// The events table shows every saving the proxy attributed to a request, not only tool-prune's:
// context-guru's compaction saving is summed in, tokens and dollars alike.
func TestRowSaving_SumsToolPruneAndContextGuru(t *testing.T) {
	e := recordEvent(t, event.Event{Avoided: []event.Saving{
		{Component: "tool-prune", TokensAvoided: 1_000, USD: 0.01, Estimated: true},
		{Component: "context-guru", TokensAvoided: 4_000, USD: 0.03, Estimated: true},
	}})
	s, ok := rowSavingFor(e)
	if !ok {
		t.Fatal("no saving for a record carrying two")
	}
	if s.TokensAvoided != 5_000 || math.Abs(s.USD-0.04) > 1e-12 || s.Projected || !s.Estimated {
		t.Errorf("saving = %+v, want 5000 tokens, $0.04, applied, estimated", s)
	}
}

// A compaction's dollar figure is shown net of the cache-rewrite penalty it caused, floored at
// zero per saving — the same figure the session totals count.
func TestRowSaving_NetsThePenalty(t *testing.T) {
	e := recordEvent(t, event.Event{Avoided: []event.Saving{
		{Component: "context-guru", TokensAvoided: 4_000, USD: 0.03, PenaltyUSD: 0.01},
		{Component: "tool-prune", TokensAvoided: 100, USD: 0.002},
	}})
	s, _ := rowSavingFor(e)
	if math.Abs(s.USD-0.022) > 1e-12 {
		t.Errorf("USD = %v, want 0.022 (0.03-0.01 + 0.002)", s.USD)
	}
	big := recordEvent(t, event.Event{Avoided: []event.Saving{
		{Component: "context-guru", TokensAvoided: 4_000, USD: 0.03, PenaltyUSD: 0.5},
	}})
	if s, ok := rowSavingFor(big); !ok || s.USD != 0 || s.TokensAvoided != 4_000 {
		t.Errorf("saving = %+v ok=%v, want the tokens kept and $0, not a negative figure", s, ok)
	}
}

// Applied and projected savings are different claims — money not spent against money that was
// spent — so they are never added together. With any applied saving the row shows the applied
// ones; with only projected ones it shows those, marked projected.
func TestRowSaving_NeverMixesAppliedAndProjected(t *testing.T) {
	mixed := recordEvent(t, event.Event{Avoided: []event.Saving{
		{Component: "tool-prune", TokensAvoided: 1_000, USD: 0.01},
		{Component: "context-guru", TokensAvoided: 4_000, USD: 0.03, Projected: true},
	}})
	if s, _ := rowSavingFor(mixed); s.TokensAvoided != 1_000 || s.Projected {
		t.Errorf("mixed = %+v, want only the applied tool-prune saving", s)
	}
	projected := recordEvent(t, event.Event{Avoided: []event.Saving{
		{Component: "context-guru", TokensAvoided: 4_000, USD: 0.03, Projected: true},
	}})
	if s, _ := rowSavingFor(projected); s.TokensAvoided != 4_000 || !s.Projected {
		t.Errorf("projected = %+v, want the projected saving, marked", s)
	}
}

// Only the components agentop knows are shown: a plugin it has never heard of does not get to put
// a figure in the table by publishing one.
func TestRowSaving_IgnoresUnknownComponents(t *testing.T) {
	e := recordEvent(t, event.Event{Avoided: []event.Saving{{Component: "some-other-plugin", TokensAvoided: 9, USD: 9}}})
	if _, ok := rowSavingFor(e); ok {
		t.Error("rendered a saving from a component agentop does not render")
	}
	if _, ok := rowSavingFor(nil); ok {
		t.Error("a nil event reported a saving")
	}
}
