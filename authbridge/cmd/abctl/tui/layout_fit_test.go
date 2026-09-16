package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
	"github.com/rossoctl/cortex/authbridge/cmd/abctl/apiclient"
)

// Nothing in the view sets MaxWidth or MaxHeight: View()'s string goes to the terminal
// verbatim. So the terminal enforces the geometry, and it enforces it badly — a view one line
// too tall scrolls the top away, and a single line one column too wide wraps into a second
// screen line that the height budget never reserved, pushing the bottom row off.
//
// That makes "the view fits the terminal" a real invariant rather than a nicety, and one worth
// asserting mechanically: three separate bugs (a fixed-width sessions table, an unbudgeted
// filter line, an unbounded identity banner) all presented to an operator as rows falling off
// the bottom, which is indistinguishable from the cursor bug this pane already had.

// fitModel builds a model with every table initialised, sized, and populated.
func fitModel(t *testing.T, p paneID, w, h int, events []pipeline.SessionEvent) *model {
	t.Helper()
	m := &model{
		pane: p, selectedSess: "sess-1", endpoint: "http://127.0.0.1:47600",
		width: w, height: h,
		events: map[string][]pipeline.SessionEvent{"sess-1": events},
	}
	// The same construction the real model uses: a zero-value textinput panics on Focus
	// (its cursor has no blink context), and the filter cases below press "/".
	m.filterInput = textinput.New()
	m.detailVp = viewport.New(0, 0)
	m.eventColumns = defaultColumnSelection()
	m.eventsTbl = newEventsTable()
	m.sessionsTbl = newSessionsTable()
	m.pipelineTbl = newPipelineTable()
	m.catalogTbl = newCatalogTable()
	m.namespacesTbl = newNamespacesTable()
	m.podsTbl = newPodsTable()
	for i := 0; i < 40; i++ {
		m.sessions = append(m.sessions, session.SessionSummary{
			// A realistic id: long enough to exercise the ID column's budget.
			ID:        fmt.Sprintf("agent-%02d.team1.svc.cluster.local:8080", i),
			UpdatedAt: time.Now(), EventCount: 12, TotalTokens: 641011,
		})
	}
	// A populated catalog, not just a nil one. Without this the catalog pane rendered
	// its "loading catalog…" line at every size, so the fit invariant never measured
	// the catalog TABLE — which is how it stayed at bubbles' default height of 20 rows
	// with nothing in layout() sizing it, overflowing any terminal shorter than that.
	m.catalog = &apiclient.PluginCatalog{}
	for i := 0; i < 30; i++ {
		m.catalog.Plugins = append(m.catalog.Plugins, apiclient.PluginCatalogEntry{
			Name:        fmt.Sprintf("catalog-plugin-%02d", i),
			Requires:    []string{"some-upstream-plugin"},
			Description: "a one-line operator-facing description of the plugin",
		})
	}
	m.pipeline = &apiclient.PipelineView{}
	for i := 0; i < 8; i++ {
		m.pipeline.Inbound = append(m.pipeline.Inbound, apiclient.PipelinePlugin{
			Name: fmt.Sprintf("inbound-plugin-%02d", i), Direction: "inbound", Position: i + 1})
		m.pipeline.Outbound = append(m.pipeline.Outbound, apiclient.PipelinePlugin{
			Name: fmt.Sprintf("outbound-plugin-%02d", i), Direction: "outbound", Position: i + 1})
	}
	// Rows before layout(), so the geometry layout() computes is the geometry the
	// returned model is actually in. Rebuilding after it left the fixture in a state
	// layout() had never seen — harmless while table heights do not depend on row
	// count, but the fit assertions measure precisely what layout() produced.
	m.rebuildSessionsTable()
	m.rebuildPipelineTable()
	m.rebuildCatalogTable()
	m.layout()
	return m
}

// assertFits is the invariant: no line wider than the terminal, no more lines than it has.
func assertFits(t *testing.T, m *model, label string) {
	t.Helper()
	view := m.View()
	if got := lipgloss.Height(view); got > m.height {
		t.Errorf("%s: view is %d lines for a %d-line terminal (%d too many)",
			label, got, m.height, got-m.height)
	}
	for i, line := range strings.Split(view, "\n") {
		if w := lipgloss.Width(line); w > m.width {
			t.Errorf("%s: line %d is %d columns for a %d-column terminal; it wraps and steals "+
				"%d row(s): %q", label, i, w, m.width, (w+m.width-1)/m.width-1,
				trunc(strings.TrimSpace(lipgloss.NewStyle().Render(line)), 50))
		}
	}
}

var fitSizes = [][2]int{{60, 20}, {80, 24}, {100, 30}, {120, 40}, {200, 50}}

// Every pane, at every size, with and without the filter open.
func TestLayout_EveryPaneFitsTheTerminal(t *testing.T) {
	panes := map[string]paneID{
		"sessions": paneSessions, "events": paneEvents, "pipeline": panePipeline,
		"detail": paneDetail, "catalog": paneCatalog, "usage": paneUsage,
	}
	for _, dim := range fitSizes {
		for name, p := range panes {
			for _, filtering := range []bool{false, true} {
				m := fitModel(t, p, dim[0], dim[1], cursorRowsFixture(60))
				if filtering {
					// Through the real key, not by setting the flag: the budget changes
					// with m.filtering, so the handler has to recompute the layout, and a
					// test that recomputed it itself would pass over a handler that
					// forgot. Opening the filter is the whole point of the case.
					m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
					if !m.filtering {
						t.Fatalf("%s: \"/\" did not open the filter", name)
					}
				}
				assertFits(t, m, fmt.Sprintf("%s %dx%d filter=%v", name, dim[0], dim[1], filtering))
			}
		}
	}
}

// The IDENTITY banner is rendered above the events table and its height is subtracted from the
// table's. Both halves have to hold for any identity the wire can carry: one long subject, or
// several distinct callers, whose subjects the banner joins into a single unbounded line.
func TestLayout_IdentityBannerFitsTheTerminal(t *testing.T) {
	long := strings.Repeat("service-account-with-a-long-name", 3)
	for _, tc := range []struct {
		name   string
		idents []pipeline.SessionEvent
	}{
		{"one short", []pipeline.SessionEvent{probeIdentity("alice", "cli")}},
		{"one long subject", []pipeline.SessionEvent{probeIdentity(long, "cli")}},
		{"two callers", []pipeline.SessionEvent{
			probeIdentity("alice", "a"), probeIdentity("bob", "b")}},
		{"six callers", []pipeline.SessionEvent{
			probeIdentity("alice", "a"), probeIdentity("bob", "b"), probeIdentity("carol", "c"),
			probeIdentity("dave", "d"), probeIdentity("erin", "e"), probeIdentity("frank", "f")}},
		{"six long callers", []pipeline.SessionEvent{
			probeIdentity(long+"1", "a"), probeIdentity(long+"2", "b"),
			probeIdentity(long+"3", "c"), probeIdentity(long+"4", "d"),
			probeIdentity(long+"5", "e"), probeIdentity(long+"6", "f")}},
	} {
		for _, dim := range fitSizes {
			events := append(cursorRowsFixture(60), tc.idents...)
			m := fitModel(t, paneEvents, dim[0], dim[1], events)
			label := fmt.Sprintf("banner %s %dx%d", tc.name, dim[0], dim[1])
			assertFits(t, m, label)

			// And the height the table gave up must be the height the banner takes, or the
			// table is mis-sized in whichever direction the two disagree.
			banner := identityBanner(m.events["sess-1"], m.width)
			if banner == "" {
				t.Fatalf("%s: fixture produced no banner", label)
			}
			if got, want := lipgloss.Height(banner), identityBannerHeightFor(m.events["sess-1"], m.width); got != want {
				t.Errorf("%s: banner renders %d lines, layout reserved %d", label, got, want)
			}
		}
	}
}

// The sessions table is the first screen an operator sees, and its columns were fixed widths
// summing wider than an 80-column terminal — bubbles pads every cell by one column on each
// side, so a row is the sum of the column widths plus two per column.
func TestLayout_TablesFitTheirTerminal(t *testing.T) {
	for _, dim := range fitSizes {
		m := fitModel(t, paneSessions, dim[0], dim[1], cursorRowsFixture(10))
		for _, tc := range []struct {
			name  string
			width int
			cols  int
		}{
			{"sessions", tableWidth(m.sessionsTbl.Columns()), len(m.sessionsTbl.Columns())},
			{"pipeline", tableWidth(m.pipelineTbl.Columns()), len(m.pipelineTbl.Columns())},
			{"catalog", tableWidth(m.catalogTbl.Columns()), len(m.catalogTbl.Columns())},
		} {
			if tc.width > dim[0] {
				t.Errorf("%s table is %d columns wide (%d columns) at terminal width %d",
					tc.name, tc.width, tc.cols, dim[0])
			}
		}
	}
}

func probeIdentity(subject, client string) pipeline.SessionEvent {
	return pipeline.SessionEvent{
		Direction: pipeline.Inbound, Phase: pipeline.SessionRequest, Host: "h",
		Identity: &pipeline.EventIdentity{Subject: subject, ClientID: client},
	}
}

// The status row is built by unconditional writes, and only the feedback link ever
// checked the width — so the optional state markers could overflow it. layout()
// reserves exactly three rows for title + blank + footer, so a wrapped status row
// costs the hint line below it, which is the row carrying [?] keys and [q] quit.
//
// TestLayout_EveryPaneFitsTheTerminal cannot catch this: it never sets sortCol,
// filter or paused, so the row it measures has none of the optional markers on it.
// Measured before fitStatusLine: paused + a restored filter + an active sort rendered
// 87 columns at width 80.
func TestLayout_StateRichFooterFitsTheTerminal(t *testing.T) {
	for _, dim := range fitSizes {
		for _, tc := range []struct {
			name    string
			sortCol eventColumnID
			filter  string
			paused  bool
			flash   string
		}{
			{name: "sort only", sortCol: colDuration},
			{name: "sort+filter", sortCol: colDuration, filter: "github-tool"},
			{name: "sort+filter+paused", sortCol: colDuration, filter: "github-tool", paused: true},
			// The longest header, so the indicator itself is at its widest.
			{name: "widest sort column", sortCol: colDuration, filter: "api.anthropic.com", paused: true},
			{name: "everything plus a flash", sortCol: colCost, filter: "github-tool", paused: true,
				flash: "yanked → ~/.cortex/abctl-events/evt-20260916-103000.json"},
		} {
			m := fitModel(t, paneEvents, dim[0], dim[1], cursorRowsFixture(60))
			m.sortCol, m.sortDesc = tc.sortCol, true
			m.filter = tc.filter
			m.paused = tc.paused
			if tc.flash != "" {
				m.setStickyFlash(tc.flash)
			}
			m.connState = connStateInfo{phase: connOpen}
			m.rebuildEventsTable()
			assertFits(t, m, fmt.Sprintf("%s %dx%d", tc.name, dim[0], dim[1]))
		}
	}
}
