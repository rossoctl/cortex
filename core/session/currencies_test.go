package session

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/pipeline"
)

func summaryOf(t *testing.T, st *Store, id string) SessionSummary {
	t.Helper()
	for _, s := range st.ListSessions() {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no session %q", id)
	return SessionSummary{}
}

// A dollars-only session says nothing about units, so its summary is byte-identical to one
// written before units existed.
//
// ASSERTED ON THE JSON, because "no currencies key" is the claim and a decoded nil slice cannot
// tell an absent key from an empty list.
func TestSessionSummary_ADollarsOnlySessionNamesNoUnit(t *testing.T) {
	st := New(5*time.Minute, 0, 0)
	defer st.Close()
	st.Append("s1", pipeline.SessionEvent{Phase: pipeline.SessionResponse,
		Plugins: costRecord(t, event.Event{CostUSD: 0.25, Settled: true})})

	raw, err := json.Marshal(summaryOf(t, st, "s1"))
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	if v, ok := keys["currencies"]; ok {
		t.Errorf("a dollars-only session carries currencies=%s; every existing reader's bytes change", v)
	}
}

// A session priced in credits says so, and a mixed one names both units, dollars spelled out.
func TestSessionSummary_NamesEveryUnitBehindItsCost(t *testing.T) {
	st := New(5*time.Minute, 0, 0)
	defer st.Close()
	credits := costRecord(t, event.Event{CostUSD: 0.03, Settled: true, Currency: "Bobcoins"})
	dollars := costRecord(t, event.Event{CostUSD: 0.25, Settled: true})

	st.Append("bob", pipeline.SessionEvent{Phase: pipeline.SessionResponse, Plugins: credits})
	st.Append("mixed", pipeline.SessionEvent{Phase: pipeline.SessionResponse, Plugins: credits})
	st.Append("mixed", pipeline.SessionEvent{Phase: pipeline.SessionResponse, Plugins: dollars})

	if got := summaryOf(t, st, "bob").Currencies; !slices.Equal(got, []string{"Bobcoins"}) {
		t.Errorf("bob Currencies = %v, want [Bobcoins]", got)
	}
	if got := summaryOf(t, st, "mixed").Currencies; !slices.Equal(got, []string{"Bobcoins", "USD"}) {
		t.Errorf("mixed Currencies = %v, want [Bobcoins USD]; a mixed session must not read as one unit", got)
	}
}

// An unpriced record names no unit: its figure is not in CostMicros, so neither is its unit.
func TestSessionSummary_AnUnpricedRecordAddsNoUnit(t *testing.T) {
	st := New(5*time.Minute, 0, 0)
	defer st.Close()
	st.Append("s1", pipeline.SessionEvent{Phase: pipeline.SessionResponse,
		Plugins: costRecord(t, event.Event{CostUSD: 0.25, Settled: true})})
	st.Append("s1", pipeline.SessionEvent{Phase: pipeline.SessionResponse,
		Plugins: costRecord(t, event.Event{Currency: "Bobcoins"})})

	if got := summaryOf(t, st, "s1").Currencies; got != nil {
		t.Errorf("Currencies = %v, want none; the credits record carried no figure", got)
	}
}

// The units follow the events through a trim, like CostMicros does.
//
// Alternating units under a small cap, so every trim evicts some of each and the last one leaves
// exactly one unit behind. A running tally that only ever added would still report both.
func TestSessionSummary_UnitsShedOnTrimLikeTheCost(t *testing.T) {
	st := New(5*time.Minute, 3, 0)
	defer st.Close()
	credits := costRecord(t, event.Event{CostUSD: 0.03, Settled: true, Currency: "Bobcoins"})
	dollars := costRecord(t, event.Event{CostUSD: 0.25, Settled: true})

	for i := 0; i < 6; i++ {
		rec := credits
		if i%2 == 1 {
			rec = dollars
		}
		st.Append("s1", pipeline.SessionEvent{Phase: pipeline.SessionResponse, Plugins: rec})
	}
	if got := summaryOf(t, st, "s1").Currencies; !slices.Equal(got, []string{"Bobcoins", "USD"}) {
		t.Fatalf("after interleaving, Currencies = %v, want [Bobcoins USD]", got)
	}
	for i := 0; i < 3; i++ {
		st.Append("s1", pipeline.SessionEvent{Phase: pipeline.SessionResponse, Plugins: dollars})
	}
	if got := summaryOf(t, st, "s1").Currencies; got != nil {
		t.Errorf("every credits event was trimmed, but Currencies = %v; the unit outlived its figure", got)
	}
}

// A saving on an unpriced request is still a figure on this session, so it names its unit, and
// the unit goes when the saving is trimmed.
func TestSessionSummary_ASavingNamesItsUnit(t *testing.T) {
	st := New(5*time.Minute, 1, 0)
	defer st.Close()
	st.Append("bob", pipeline.SessionEvent{Phase: pipeline.SessionResponse, Plugins: costRecord(t,
		event.Event{Currency: "Bobcoins", Avoided: []event.Saving{{Basis: event.BasisUSD, Component: "tool-prune", USD: 0.5}}})})
	if got := summaryOf(t, st, "bob").Currencies; !slices.Equal(got, []string{"Bobcoins"}) {
		t.Errorf("Currencies = %v, want [Bobcoins]", got)
	}
	st.Append("bob", pipeline.SessionEvent{Phase: pipeline.SessionResponse,
		Plugins: costRecord(t, event.Event{CostUSD: 0.25, Settled: true})})
	if got := summaryOf(t, st, "bob").Currencies; got != nil {
		t.Errorf("the saving was trimmed, but Currencies = %v; its unit outlived it", got)
	}
}
