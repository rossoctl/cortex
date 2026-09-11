package tui

import (
	"encoding/json"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// These tests used to cover abctl's own byte-to-token-to-dollar arithmetic. That arithmetic
// moved to the proxy (authlib/costing, authlib/pricing), where it is tested against the real
// parser, and abctl's remaining job is reading a figure off the record. So these now cover
// the reading — including the cases where there is nothing to read, which must render as
// "not reported" rather than as zero.

// recordEvent builds a response event carrying a cost record, as the proxy publishes it.
func recordEvent(t *testing.T, ev costevent.Event) *pipeline.SessionEvent {
	t.Helper()
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return &pipeline.SessionEvent{
		Phase:   pipeline.SessionResponse,
		Plugins: map[string]json.RawMessage{costevent.Key: raw},
	}
}

func TestSavingFor_ReadsTheAttributedComponent(t *testing.T) {
	e := recordEvent(t, costevent.Event{
		CostUSD: 0.04,
		Settled: true,
		Avoided: []costevent.Saving{
			{Component: "some-other-plugin", TokensAvoided: 1, USD: 0.99},
			{Component: "tool-prune", TokensAvoided: 10_577, USD: 0.0041, Tier: "cache_write", Estimated: true},
		},
	})

	got, ok := pruneSavingFor(e)
	if !ok {
		t.Fatal("tool-prune's saving not found")
	}
	// By component, not by position: a pipeline can have several body-shrinking plugins,
	// and attributing one's saving to another is worse than reporting none.
	if got.TokensAvoided != 10_577 || got.USD != 0.0041 {
		t.Errorf("saving = %+v, want tool-prune's own figures", got)
	}
	if got.Tier != "cache_write" {
		t.Errorf("tier = %q; it is what makes the figure checkable, since tiers differ by 12.5x", got.Tier)
	}
	if _, ok := savingFor(e, "not-in-this-record"); ok {
		t.Error("found a saving for a component that did not report one")
	}
}

// Absent is not zero. A saving that cannot be read must leave the cell empty, because a
// rendered 0 reads as "this saved nothing" — a claim nobody made.
func TestSavingFor_AbsentIsNotZero(t *testing.T) {
	for _, tc := range []struct {
		name string
		e    *pipeline.SessionEvent
	}{
		{"nil event", nil},
		{"no plugins", &pipeline.SessionEvent{Phase: pipeline.SessionResponse}},
		{"a record with no savings", recordEvent(t, costevent.Event{CostUSD: 0.04, Settled: true})},
		{"only the prune facts, no record", &pipeline.SessionEvent{
			Plugins: map[string]json.RawMessage{"tool-prune": json.RawMessage(`{"bytesRemoved":900}`)},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := pruneSavingFor(tc.e); ok {
				t.Error("reported a saving where none was published")
			}
		})
	}
}

// A record present but unpriced still yields its saving. That is the whole point of
// costevent.Record answering presence separately from pricedness: a request the table cannot
// price is exactly the one whose token saving is most worth showing.
func TestSavingFor_SurvivesAnUnpricedRecord(t *testing.T) {
	e := recordEvent(t, costevent.Event{
		Avoided: []costevent.Saving{{Component: "tool-prune", TokensAvoided: 500, Tier: "input", Estimated: true}},
	})
	got, ok := pruneSavingFor(e)
	if !ok {
		t.Fatal("saving lost on an unpriced record")
	}
	if got.TokensAvoided != 500 {
		t.Errorf("tokens = %d, want 500", got.TokensAvoided)
	}
	if got.USD != 0 {
		t.Errorf("USD = %v, want 0 — no rate covered it", got.USD)
	}
	// And the cost column must stay empty rather than show $0.00.
	if _, ok := promptCost(e); ok {
		t.Error("promptCost reported a figure for an unpriced record")
	}
}

// The legacy key still decodes, because a proxy older than the rename writes only that one.
func TestSavingFor_ReadsTheLegacyKey(t *testing.T) {
	raw, err := json.Marshal(costevent.Event{
		PromptUSD: 0.0395,
		Avoided:   []costevent.Saving{{Component: "tool-prune", TokensAvoided: 10, USD: 0.001}},
	})
	if err != nil {
		t.Fatal(err)
	}
	e := &pipeline.SessionEvent{
		Phase:   pipeline.SessionResponse,
		Plugins: map[string]json.RawMessage{costevent.PluginName: raw},
	}
	if _, ok := pruneSavingFor(e); !ok {
		t.Error("saving not read from the legacy key")
	}
	if usd, ok := promptCost(e); !ok || usd != 0.0395 {
		t.Errorf("promptCost from the legacy key = %v (ok=%v), want 0.0395", usd, ok)
	}
}

func TestFormatCompact(t *testing.T) {
	for in, want := range map[float64]string{
		0:         "0",
		999:       "999",
		10_577:    "10.6k",
		2_500_000: "2.5M",
	} {
		if got := formatCompact(in); got != want {
			t.Errorf("formatCompact(%v) = %q, want %q", in, got, want)
		}
	}
}

// reqEvent builds a request event carrying tool-prune's facts.
//
// Still useful even though the pane no longer reads them for money: the facts are what the
// plugin publishes, and a request row is where they live.
func reqEvent(t *testing.T, raw string) *pipeline.SessionEvent {
	t.Helper()
	return &pipeline.SessionEvent{
		Phase:   pipeline.SessionRequest,
		Plugins: map[string]json.RawMessage{"tool-prune": json.RawMessage(raw)},
	}
}
