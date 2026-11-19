package tui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	coremodel "github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

const importedSrc = coremodel.ImportedSourcePrefix + "claude-code"

// TestSessionTitleCell_ImportedPrefix: an imported session's TITLE cell starts with importedMarker,
// and its search row has the same prefix. A live session has no prefix.
func TestSessionTitleCell_ImportedPrefix(t *testing.T) {
	m := fitModel(t, paneSessions, 200, 40, nil)
	m.sessions = []session.SessionSummary{
		{ID: "imp-1", Title: "fix the build", Source: importedSrc},
		{ID: "live-1", Title: "fix the build"},
	}
	m.rebuildSessionsTable()
	rows := m.sessionsTbl.Rows()
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	title := func(r []string) string {
		for _, c := range r {
			if strings.Contains(c, "fix the build") {
				return c
			}
		}
		t.Fatalf("no title cell in %q", r)
		return ""
	}
	if got := title(rows[0]); !strings.HasPrefix(got, importedMarker+"fix the build") {
		t.Errorf("imported title = %q, want the prefix %q", got, importedMarker)
	}
	if got := title(rows[1]); strings.HasPrefix(got, importedMarker) {
		t.Errorf("live title = %q, has the imported prefix", got)
	}
	// The search reads the same prefix: it matches the imported row only.
	m.search = map[paneID]string{paneSessions: importedMarker + "fix the build"}
	m.rebuildSessionsTable()
	if got := m.sessionsTbl.Matches(); len(got) != 1 || got[0] != 0 {
		t.Errorf("search matches = %v, want [0]", got)
	}
}

// TestEventCells_ImportedShowNA: the proxy-only columns of an imported exchange say n/a,
// and tokens and cost still show the transcript's values.
func TestEventCells_ImportedShowNA(t *testing.T) {
	req := &pipeline.SessionEvent{Phase: pipeline.SessionRequest, Source: importedSrc, At: time.Now()}
	raw, err := json.Marshal(event.Event{CostUSD: 0.05, Settled: true, Source: event.SourceTranscript})
	if err != nil {
		t.Fatal(err)
	}
	resp := &pipeline.SessionEvent{
		Phase: pipeline.SessionResponse, Source: importedSrc,
		Inference: agentTurn(),
		Plugins:   map[string]json.RawMessage{event.Key: raw},
	}
	cc := cellContext{row: eventRow{event: req, resp: resp}}
	for _, id := range []eventColumnID{colStatus, colDuration, colBytes, colHost, colPlugin} {
		c := eventColumnByID(t, id)
		cc.width = c.width
		if got := c.cell(cc); got != "n/a" {
			t.Errorf("%s = %q, want n/a", id, got)
		}
	}
	if got := eventColumnByID(t, colTokens).cell(cc); got == "" || got == "n/a" {
		t.Errorf("TOKENS = %q, want the transcript's value", got)
	}
	if got := eventColumnByID(t, colCost).cell(cc); got != "$0.0500" {
		t.Errorf("COST = %q, want $0.0500", got)
	}
	// A live exchange keeps its own cells.
	live := cellContext{row: eventRow{event: &pipeline.SessionEvent{Phase: pipeline.SessionRequest, Host: "api.anthropic.com"}}}
	live.width = 20
	if got := eventColumnByID(t, colHost).cell(live); got != "api.anthropic.com" {
		t.Errorf("live HOST = %q", got)
	}
}

// TestToolCells_ImportedShowNA: the tool rows of an imported exchange say n/a in the same
// proxy-only columns.
func TestToolCells_ImportedShowNA(t *testing.T) {
	req := &pipeline.SessionEvent{Phase: pipeline.SessionRequest, Source: importedSrc}
	cols := []eventColumn{eventColumnByID(t, colPlugin), eventColumnByID(t, colStatus), eventColumnByID(t, colDuration)}
	tr := &toolRow{}
	tr.Name = "Bash"
	row, _ := toolCells(cols, cellContext{row: eventRow{event: req}}, eventRow{tool: tr}, time.Now())
	for i, c := range row {
		if strings.TrimSpace(c) != "n/a" {
			t.Errorf("%s = %q, want n/a", cols[i].id, c)
		}
	}
}

// TestDetailTiming_ImportedNA: an imported response has no proxy timing.
func TestDetailTiming_ImportedNA(t *testing.T) {
	e := &pipeline.SessionEvent{Phase: pipeline.SessionResponse, Source: importedSrc,
		Ttfb: time.Second, Duration: 2 * time.Second}
	if got := timingHeader(e); got != "timing: n/a" {
		t.Errorf("timingHeader = %q, want %q", got, "timing: n/a")
	}
}

// TestRawView_ImportedNotCaptured: an imported event has no capture, and the raw node says
// why. A live event with no capture gets no raw node.
func TestRawView_ImportedNotCaptured(t *testing.T) {
	e := &pipeline.SessionEvent{Source: importedSrc}
	var m map[string]any
	if err := json.Unmarshal(withRaw([]byte(`{"phase":"request"}`), nil, e), &m); err != nil {
		t.Fatal(err)
	}
	if got := m["raw"]; got != "not captured: imported from claude-code" {
		t.Errorf("raw = %v, want the not-captured note", got)
	}
	if got := string(withRaw([]byte(`{"phase":"request"}`), nil, &pipeline.SessionEvent{})); got != `{"phase":"request"}` {
		t.Errorf("live event without capture = %s, want unchanged", got)
	}
}

// TestSessionIDCell_ImportedPrefixWithoutTitle: a terminal too narrow for TITLE puts the
// imported mark on the SESSION cell, so the row is still marked.
func TestSessionIDCell_ImportedPrefixWithoutTitle(t *testing.T) {
	m := fitModel(t, paneSessions, 60, 40, nil)
	m.sessions = []session.SessionSummary{
		{ID: "imp-1", Title: "fix the build", Source: importedSrc},
		{ID: "live-1", Title: "fix the build"},
	}
	m.rebuildSessionsTable()
	rows := m.sessionsTbl.Rows()
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	for _, c := range rows[0] {
		if strings.Contains(c, "fix the build") {
			t.Fatalf("TITLE is shown at 60 columns: %q", rows[0])
		}
	}
	if got := rows[0][0]; !strings.HasPrefix(got, importedMarker) {
		t.Errorf("imported SESSION cell = %q, want the prefix %q", got, importedMarker)
	}
	if got := rows[1][0]; strings.HasPrefix(got, importedMarker) {
		t.Errorf("live SESSION cell = %q, has the imported prefix", got)
	}
}

// TestSessionTitleCell_ChildPrefix: a session that another session started has "↳ " in
// front of its title (#1991).
func TestSessionTitleCell_ChildPrefix(t *testing.T) {
	m := fitModel(t, paneSessions, 200, 40, nil)
	m.sessions = []session.SessionSummary{
		{ID: "child-1", Title: "fix the build", ParentSession: "parent-123456789"},
		{ID: "top-1", Title: "fix the build"},
	}
	m.rebuildSessionsTable()
	rows := m.sessionsTbl.Rows()
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	title := func(r []string) string {
		for _, c := range r {
			if strings.Contains(c, "fix the build") {
				return c
			}
		}
		t.Fatalf("no title cell in %q", r)
		return ""
	}
	if got := title(rows[0]); !strings.HasPrefix(got, childMarker+"fix the build") {
		t.Errorf("child title = %q, want the prefix %q", got, childMarker)
	}
	if got := title(rows[1]); strings.Contains(got, childMarker) {
		t.Errorf("top title = %q, has the child prefix", got)
	}
}

// TestSessionIDCell_ChildPrefixWithNoTitleColumn: with no TITLE column, the SESSION cell
// of a child session has the mark.
func TestSessionIDCell_ChildPrefixWithNoTitleColumn(t *testing.T) {
	m := fitModel(t, paneSessions, 60, 40, nil)
	m.sessions = []session.SessionSummary{
		{ID: "child-1", Title: "fix the build", ParentSession: "parent-123456789"},
		{ID: "top-1", Title: "fix the build"},
	}
	m.rebuildSessionsTable()
	rows := m.sessionsTbl.Rows()
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	for _, c := range rows[0] {
		if strings.Contains(c, "fix the build") {
			t.Fatalf("TITLE is shown at 60 columns: %q", rows[0])
		}
	}
	if got := rows[0][0]; !strings.HasPrefix(got, childMarker) {
		t.Errorf("child SESSION cell = %q, want the prefix %q", got, childMarker)
	}
	if got := rows[1][0]; strings.Contains(got, childMarker) {
		t.Errorf("top SESSION cell = %q, has the child prefix", got)
	}
}
