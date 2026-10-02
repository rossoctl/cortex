package tui

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/pipeline"
)

// BenchmarkRebuildEventsTable is one rebuild of a 2,000-event session in which every
// response carries a cost record: the work the events pane pays on every SSE event.
//
// The cost record is what makes TOKENS and COST the expensive cells — each decodes it —
// so a fixture without one would not show what evaluating them twice per row costs.
// "filtered" keeps one row in ten, and must never cost more than "unfiltered": a filter
// hides rows, it should not add work.
func BenchmarkRebuildEventsTable(b *testing.B) {
	for _, bc := range []struct {
		name   string
		filter string
	}{
		{"unfiltered", ""},
		{"filtered", "h7.example"},
	} {
		b.Run(bc.name, func(b *testing.B) {
			m := rebuildBenchModel(b, 1000)
			m.filter = bc.filter
			b.ReportAllocs()
			for b.Loop() {
				m.rebuildEventsTable()
			}
		})
	}
}

// rebuildBenchModel is an events pane over n inference exchanges, every response priced and
// every other one carrying an estimated tool-prune saving. Hosts cycle through ten values so
// a filter on one of them keeps a tenth of the rows.
func rebuildBenchModel(b *testing.B, n int) *model {
	b.Helper()
	plain, err := json.Marshal(event.Event{CostUSD: 0.2767, Settled: true, PromptUSD: 0.2546, OutputUSD: 0.0221})
	if err != nil {
		b.Fatal(err)
	}
	saved, err := json.Marshal(event.Event{CostUSD: 0.2767, Settled: true, PromptUSD: 0.2546, OutputUSD: 0.0221,
		Avoided: []event.Saving{{Component: "tool-prune", TokensAvoided: 12_300, USD: 0.0037, Estimated: true}}})
	if err != nil {
		b.Fatal(err)
	}

	start := time.Date(2026, 10, 2, 13, 0, 0, 0, time.UTC)
	events := make([]pipeline.SessionEvent, 0, 2*n)
	for i := range n {
		at := start.Add(time.Duration(i) * 3 * time.Second)
		host := fmt.Sprintf("h%d.example", i%10)
		id := fmt.Sprintf("r%d", i)
		rec := plain
		if i%2 == 1 {
			rec = saved
		}
		events = append(events, pipeline.SessionEvent{
			At: at, SessionID: "s", Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
			Host: host, RequestID: id,
			Inference: &pipeline.InferenceExtension{Model: "claude-sonnet-5"},
		}, pipeline.SessionEvent{
			At: at.Add(2 * time.Second), SessionID: "s",
			Direction: pipeline.Outbound, Phase: pipeline.SessionResponse,
			Host: host, StatusCode: 200, Duration: 2 * time.Second, RequestID: id,
			Inference: &pipeline.InferenceExtension{
				Model: "claude-sonnet-5", InputTokens: 200_000 + i*100, OutputTokens: 400,
			},
			Plugins: map[string]json.RawMessage{event.Key: rec},
		})
	}
	m := &model{
		pane: paneEvents, selectedSess: "s",
		width: 200, height: 40, bodyHeight: 30,
		events:       map[string][]pipeline.SessionEvent{"s": events},
		eventColumns: defaultColumnSelection(),
	}
	m.eventsTbl = newEventsTable()
	m.rebuildEventsTable()
	return m
}
