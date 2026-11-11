package tui

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session/timeline"
)

// BenchmarkRebuildEventsTable is one rebuild of a 2,000-event session in which every
// response carries a cost record: the work the events pane pays on every SSE event.
//
// The cost record is what makes TOKENS and COST the expensive cells — each decodes it —
// so a fixture without one would not show what evaluating them twice per row costs.
// "searched" marks one row in ten, which costs one match per row on top of "plain": a search
// hides nothing, so every row is still rendered. Each response also has tool rows from the
// timeline, and "sorted" sorts by DURATION, which ranks the tool rows among the exchanges.
func BenchmarkRebuildEventsTable(b *testing.B) {
	for _, bc := range []struct {
		name   string
		search string
		sort   eventColumnID
	}{
		{"plain", "", ""},
		{"searched", "h7.example", ""},
		{"sorted", "", colDuration},
	} {
		b.Run(bc.name, func(b *testing.B) {
			m := rebuildBenchModel(b, 1000)
			m.search = map[paneID]string{paneEvents: bc.search}
			m.sortCol = bc.sort
			b.ReportAllocs()
			for b.Loop() {
				m.rebuildEventsTable()
			}
		})
	}
}

// rebuildBenchModel is an events pane over n inference exchanges, every response priced and
// every other one carrying an estimated tool-prune saving. Hosts cycle through ten values so
// a search for one of them matches a tenth of the rows. Each response asks for one tool, and
// every 20th response asks for two parallel tools; each tool ends 1s after its response.
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
	tl := &timeline.Timeline{Tools: make([]timeline.Tool, 0, n+n/20)}
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
			Host: host, RequestID: id, Seq: uint64(2*i + 1),
			Inference: &pipeline.InferenceExtension{Model: "claude-sonnet-5"},
		}, pipeline.SessionEvent{
			At: at.Add(2 * time.Second), SessionID: "s",
			Direction: pipeline.Outbound, Phase: pipeline.SessionResponse,
			Host: host, StatusCode: 200, Duration: 2 * time.Second, RequestID: id, Seq: uint64(2*i + 2),
			Inference: &pipeline.InferenceExtension{
				Model: "claude-sonnet-5", InputTokens: 200_000 + i*100, OutputTokens: 400,
			},
			Plugins: map[string]json.RawMessage{event.Key: rec},
		})
		toolStart := at.Add(2 * time.Second)
		toolEnd := toolStart.Add(time.Second)
		tools := 1
		if i%20 == 0 {
			tools = 2
		}
		for k := range tools {
			tl.Tools = append(tl.Tools, timeline.Tool{
				Agent: "main", ID: fmt.Sprintf("t%d-%d", i, k), Name: "Bash",
				Start: toolStart, End: &toolEnd, Preview: "git -C /repo status", Seq: uint64(2*i + 2),
			})
		}
	}
	m := &model{
		pane: paneEvents, selectedSess: "s",
		width: 200, height: 40, bodyHeight: 30,
		events:       map[string][]pipeline.SessionEvent{"s": events},
		eventColumns: defaultColumnSelection(),
	}
	m.timeline.session, m.timeline.tl, m.timeline.tools = "s", tl, indexTools(tl)
	m.eventsTbl = newEventsTable()
	m.rebuildEventsTable()
	if got, want := len(m.eventsTbl.Rows()), n+len(tl.Tools); got != want {
		b.Fatalf("rows = %d, want %d (each exchange and each tool)", got, want)
	}
	return m
}
