package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/pipeline"
)

// TOKENS AND COST ARE AS WIDE AS THE SESSION NEEDS (#1208).
//
// Both are sized for a tool-prune saving appended to the figure — "1,048,576(−12.3k)",
// "<$0.0001(−<$0.0001)" — which most pipelines never produce. At a fixed 17 and 19 that
// reservation was blank on every row, and because both right-align it sat between
// DURATION and the figure, and between TOKENS and COST.

// The two shapes a tool-prune saving arrives in. formatTokensWithSaving renders an
// estimate compact ("−12.3k") and a counted saving exactly ("−12,300"), so the counted
// one is the widest a TOKENS cell gets, and what its cap is sized for.
var (
	// estimatedSaving is how every saving published today arrives.
	estimatedSaving = &event.Saving{Component: "tool-prune", TokensAvoided: 12_300, USD: 0.0037, Estimated: true}
	countedSaving   = &event.Saving{Component: "tool-prune", TokensAvoided: 12_300, USD: 0.0037}
)

// sizingExchange is one inference exchange on host: a request, and the response carrying
// its prompt size and cost record. saving, when not nil, is attached to that record.
func sizingExchange(t *testing.T, id, host string, at time.Time, prompt int, saving *event.Saving) []pipeline.SessionEvent {
	t.Helper()
	rec := event.Event{CostUSD: 0.2767, Settled: true, PromptUSD: 0.2546, OutputUSD: 0.0221}
	if saving != nil {
		rec.Avoided = []event.Saving{*saving}
	}
	return []pipeline.SessionEvent{{
		At: at, SessionID: "s", Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
		Host: host, RequestID: id,
		Inference: &pipeline.InferenceExtension{Model: "claude-sonnet-5"},
	}, {
		At: at.Add(2930 * time.Millisecond), SessionID: "s",
		Direction: pipeline.Outbound, Phase: pipeline.SessionResponse,
		Host: host, StatusCode: 200, Duration: 2930 * time.Millisecond, RequestID: id,
		Inference: &pipeline.InferenceExtension{
			Model: "claude-sonnet-5", InputTokens: prompt, OutputTokens: 208,
		},
		Plugins: costRecord(t, rec),
	}}
}

func sizingModel(t *testing.T, width int, events ...[]pipeline.SessionEvent) *model {
	t.Helper()
	var all []pipeline.SessionEvent
	for _, ex := range events {
		all = append(all, ex...)
	}
	m := &model{
		pane: paneEvents, selectedSess: "s",
		width: width, height: 40, bodyHeight: 12,
		events:       map[string][]pipeline.SessionEvent{"s": all},
		eventColumns: defaultColumnSelection(),
	}
	m.eventsTbl = newEventsTable()
	m.rebuildEventsTable()
	return m
}

// renderedWidth is the width bubbles gave the column with this id, read off the table
// itself rather than off the column definitions, so it is what reaches the screen.
func renderedWidth(t *testing.T, m *model, id eventColumnID) int {
	t.Helper()
	for _, c := range m.eventsTbl.Columns() {
		if strings.TrimRight(headerTitle(c), sortGlyphAsc+sortGlyphDesc) == string(id) {
			return c.Width
		}
	}
	t.Fatalf("column %s is not in the table", id)
	return 0
}

// The issue's case: no tool-prune, so no saving, and each column is exactly as wide as
// its widest figure.
func TestMeasureColumns_FitTheFiguresWithoutASaving(t *testing.T) {
	m := sizingModel(t, 200,
		sizingExchange(t, "r1", "api.anthropic.com", time.Now(), 1_048_576, nil))

	for _, tc := range []struct {
		id   eventColumnID
		want string
	}{
		{colTokens, "1,048,576"},
		{colCost, "$0.2546"},
	} {
		if got := cellAt(t, m, 0, tc.id); got != tc.want {
			t.Errorf("%s request cell = %q, want %q with no padding in front", tc.id, got, tc.want)
		}
		if got, want := renderedWidth(t, m, tc.id), lipgloss.Width(tc.want); got != want {
			t.Errorf("%s is %d wide, want %d — the width of %q", tc.id, got, want, tc.want)
		}
	}

	// And the table bubbles draws is the width the fit was computed for, so the columns
	// the sizing freed are real terminal columns and not an accounting difference.
	cols, _ := layoutColumns(m.eventColumns, m.eventColWidths, m.width)
	header := strings.SplitN(stripANSI(m.eventsTbl.View()), "\n", 2)[0]
	if got, want := lipgloss.Width(header), columnsWidth(cols); got != want {
		t.Errorf("header renders %d wide, layoutColumns says %d", got, want)
	}
}

// With a saving the column grows to the whole cell, saving included — for a seven-digit
// prompt with a counted saving too, the widest TOKENS cell there is. At a cap of 17 that
// one rendered "1,048,576(−12,30…".
func TestMeasureColumns_GrowToASaving(t *testing.T) {
	for _, tc := range []struct {
		name   string
		saving *event.Saving
	}{
		{"estimated", estimatedSaving},
		{"counted", countedSaving},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := sizingModel(t, 200,
				sizingExchange(t, "r1", "api.anthropic.com", time.Now(), 1_048_576, tc.saving))

			for _, id := range []eventColumnID{colTokens, colCost} {
				cell := cellAt(t, m, 0, id)
				if !strings.Contains(cell, "(−") {
					t.Fatalf("%s cell %q carries no saving; the fixture is not exercising one", id, cell)
				}
				if got, want := renderedWidth(t, m, id), lipgloss.Width(cell); got != want {
					t.Errorf("%s is %d wide, want %d — the width of %q", id, got, want, cell)
				}
			}
		})
	}
}

// With nothing to measure a column is as narrow as its heading with a sort glyph, and no
// narrower: sorting it later must not clip the name, or widen the column under the rows.
func TestMeasureColumns_FloorIsTheSortedHeading(t *testing.T) {
	widths, _ := measureColumns(eventColumns, nil)
	for _, c := range sizedColumns(eventColumns, widths) {
		if !c.fitsContent {
			if _, ok := widths[c.id]; ok {
				t.Errorf("%s does not fit its content but was measured", c.id)
			}
			continue
		}
		if want := lipgloss.Width(string(c.id) + sortGlyphDesc); c.width != want {
			t.Errorf("%s with no rows is %d wide, want %d — its heading and a glyph", c.id, c.width, want)
		}
		for _, desc := range []bool{true, false} {
			title := tableColumns([]eventColumn{c}, c.id, desc)[0].Title
			if w := lipgloss.Width(title); w > c.width {
				t.Errorf("%s sorted: header %q is %d wide in a %d-wide column", c.id, title, w, c.width)
			}
		}
	}
}

// The declared width is a cap: a cell wider than it is truncated by bubbles, as it always
// was, rather than widening the column past what fitColumns was told.
func TestMeasureColumns_NeverWiderThanTheCap(t *testing.T) {
	col := eventColumn{id: "X", width: 5, fitsContent: true,
		cell: func(cellContext) string { return "1234567890" }}
	widths, _ := measureColumns([]eventColumn{col}, make([]cellContext, 3))
	if got := widths["X"]; got != 5 {
		t.Errorf("measured %d, want the cap of 5", got)
	}
}

// Measured over the whole session, so neither a filter nor the inactive toggle resizes
// the columns. Typing a filter one letter at a time would otherwise reflow the table on
// every keystroke, and hiding the one row with a saving would narrow TOKENS under it.
func TestMeasureColumns_IgnoreTheFilter(t *testing.T) {
	now := time.Now()
	m := sizingModel(t, 200,
		sizingExchange(t, "r1", "wide.example", now, 1_048_576, estimatedSaving),
		sizingExchange(t, "r2", "narrow.example", now.Add(time.Minute), 215_305, nil))
	want := map[eventColumnID]int{
		colTokens: renderedWidth(t, m, colTokens),
		colCost:   renderedWidth(t, m, colCost),
	}

	for _, tc := range []struct {
		name  string
		apply func(*model)
		rows  int
	}{
		{"filter", func(m *model) { m.filter = "narrow.example" }, 2},
		// No plugin acted on these fixture rows, so hiding inactive rows hides all of them.
		{"hide inactive", func(m *model) { m.hideInactive = true }, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m.filter, m.hideInactive = "", false
			tc.apply(m)
			m.rebuildEventsTable()
			if got := len(m.eventsTbl.Rows()); got != tc.rows {
				t.Fatalf("%d rows visible, want %d; the %s did not take effect", got, tc.rows, tc.name)
			}
			for id, w := range want {
				if got := renderedWidth(t, m, id); got != w {
					t.Errorf("%s resized from %d to %d under the %s", id, w, got, tc.name)
				}
			}
		})
	}
}

// A figure wider than any before it widens its column mid-stream, with no keystroke
// behind it, so it must leave the operator where they were. Re-setting columns used to
// clear the table's rows first, which re-anchored the pane.
func TestEventsTable_WiderFigureKeepsScrollPosition(t *testing.T) {
	m := cursorModel(t, 40)
	setCursorVisible(&m.eventsTbl, 39)
	for i := 0; i < 3; i++ {
		m.eventsTbl, _ = m.eventsTbl.Update(tea.KeyMsg{Type: tea.KeyUp})
	}
	wantCursor, wantWindow := m.eventsTbl.Cursor(), renderedWindow(t, m.eventsTbl)
	before := renderedWidth(t, m, colTokens)

	m.events["s"] = append(m.events["s"],
		sizingExchange(t, "wide", hostToken(40), time.Now(), 1_048_576, nil)...)
	m.rebuildEventsTable()

	if after := renderedWidth(t, m, colTokens); after <= before {
		t.Fatalf("TOKENS stayed %d wide; the fixture did not widen it", after)
	}
	if got := m.eventsTbl.Cursor(); got != wantCursor {
		t.Errorf("a wider figure moved the cursor: %d, want %d", got, wantCursor)
	}
	if got := renderedWindow(t, m.eventsTbl); got != wantWindow {
		t.Errorf("a wider figure scrolled the pane: showing rows %s, want %s", got, wantWindow)
	}
}

// The picker's "(no room)" marker is judged against the widths the table is using. At
// 150 columns the default set fits only because TOKENS and COST are sized to their
// figures; judged at their declared widths, COST would be marked as having no room while
// the table beside it showed it.
func TestColumnPicker_NoRoomAgreesWithTheSizedTable(t *testing.T) {
	const width = 150
	m := sizingModel(t, width,
		sizingExchange(t, "r1", "api.anthropic.com", time.Now(), 1_048_576, nil))
	if m.eventColsDropped != 0 {
		t.Fatalf("%d columns dropped at %d; the sized defaults should all fit", m.eventColsDropped, width)
	}
	if got := cellAt(t, m, 0, colCost); got == "" {
		t.Fatal("COST is visible but blank; the fixture is not pricing the request")
	}

	if out := stripANSI(renderColumnPicker(m.eventColumns, m.eventColWidths, 0, width, 40, "", false)); strings.Contains(out, "(no room)") {
		t.Errorf("picker marks a column the table is showing as having no room:\n%s", out)
	}
	// The control: at declared widths the same terminal is too narrow, so the assertion
	// above is distinguishing something.
	if out := stripANSI(renderColumnPicker(m.eventColumns, nil, 0, width, 40, "", false)); !strings.Contains(out, "(no room)") {
		t.Errorf("at declared widths %d columns should not fit the defaults; the test proves nothing", width)
	}
}

// A widening that crosses a fit boundary changes the column COUNT, not only widths, and
// must not re-anchor the pane either. At 150 columns the fixture's sized defaults fit; the
// first counted saving widens TOKENS and COST past that, a column is dropped, and the
// headings change. Then the reverse: a wider terminal lets the column back on, so the count
// grows. Each direction takes a different SetRows/SetColumns order, and the two together are
// the only ones a toggle or a resize can produce.
func TestEventsTable_WideningPastTheTerminalKeepsScrollPosition(t *testing.T) {
	m := cursorModel(t, 40)
	m.width = 150
	m.rebuildEventsTable()
	if m.eventColsDropped != 0 {
		t.Fatalf("%d columns dropped at %d before any saving; the fixture should start with all of them",
			m.eventColsDropped, m.width)
	}
	setCursorVisible(&m.eventsTbl, 39)
	for i := 0; i < 3; i++ {
		m.eventsTbl, _ = m.eventsTbl.Update(tea.KeyMsg{Type: tea.KeyUp})
	}
	wantCursor, wantWindow := m.eventsTbl.Cursor(), renderedWindow(t, m.eventsTbl)
	before := len(m.eventsTbl.Columns())

	m.events["s"] = append(m.events["s"],
		sizingExchange(t, "wide", hostToken(40), time.Now(), 1_048_576, countedSaving)...)
	for _, step := range []struct {
		name  string
		apply func()
		check func(n int) bool
	}{
		{"a saving pushes a column off", func() {}, func(n int) bool { return n < before }},
		{"a wider terminal lets it back", func() { m.width = 200 }, func(n int) bool { return n == before }},
	} {
		step.apply()
		m.rebuildEventsTable()
		if n := len(m.eventsTbl.Columns()); !step.check(n) {
			t.Fatalf("%s: %d columns, from %d; the fixture did not cross the fit boundary", step.name, n, before)
		}
		if got := m.eventsTbl.Cursor(); got != wantCursor {
			t.Errorf("%s: moved the cursor: %d, want %d", step.name, got, wantCursor)
		}
		if got := renderedWindow(t, m.eventsTbl); got != wantWindow {
			t.Errorf("%s: scrolled the pane: showing rows %s, want %s", step.name, got, wantWindow)
		}
	}
}

// A fitsContent cell is evaluated once, before its column's width is known, and that string
// is what the row shows. So it must not depend on cc.width — checked here at the cap and at
// one column, for every fitsContent column, on rows with and without a saving.
func TestFitsContentCellsIgnoreWidth(t *testing.T) {
	m := sizingModel(t, 200,
		sizingExchange(t, "r1", "a.example", time.Now(), 215_305, nil),
		sizingExchange(t, "r2", "b.example", time.Now(), 1_048_576, estimatedSaving),
		sizingExchange(t, "r3", "c.example", time.Now(), 1_048_576, countedSaving))
	rows := buildEventRows(m.events["s"])
	ids, partner := computeEventPairs(rows)

	checked := 0
	for _, c := range eventColumns {
		if !c.fitsContent {
			continue
		}
		for i, er := range rows {
			cc := cellContext{m: m, rows: rows, partner: partner, i: i, row: er, ids: ids}
			cc.width = c.width
			wide := c.cell(cc)
			cc.width = 1
			if narrow := c.cell(cc); narrow != wide {
				t.Errorf("%s row %d: %q at width %d, %q at width 1", c.id, i, wide, c.width, narrow)
			}
			if wide != "" {
				checked++
			}
		}
	}
	if checked == 0 {
		t.Fatal("no fitsContent cell had a value, so nothing was compared")
	}
}
