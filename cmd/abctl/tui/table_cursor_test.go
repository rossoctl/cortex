package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/cmd/abctl/apiclient"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// hostToken is the per-row marker these fixtures use. Unique per row and short
// enough that the 20-wide HOST column does not truncate it, so its presence in the
// rendered view is a reliable "this row is on screen" — which is the property under
// test. Asserting on the cursor INDEX alone cannot catch this bug: the index was
// always right, it was the rendered window that excluded it.
func hostToken(i int) string { return fmt.Sprintf("h%02d.example", i) }

// markerOf finds the fixture marker among a row's cells, so an assertion can name the row
// the cursor is actually on rather than the row its index would have been before a filter.
func markerOf(row table.Row) (string, bool) {
	for _, cell := range row {
		if c := strings.TrimSpace(cell); strings.HasSuffix(c, ".example") {
			return c, true
		}
	}
	return "", false
}

// cursorRowsFixture builds n request events, each identifiable by its host.
func cursorRowsFixture(n int) []pipeline.SessionEvent {
	events := make([]pipeline.SessionEvent, n)
	for i := range events {
		events[i] = pipeline.SessionEvent{
			Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
			Host:      hostToken(i),
			Inference: &pipeline.InferenceExtension{Model: "m"},
		}
	}
	return events
}

func cursorModel(t *testing.T, n int) *model {
	t.Helper()
	// "Opens at the newest event" is part of this fixture's contract, and that is now
	// a persisted preference (EventSettings.OpenAtOldest) living on a package global —
	// so without this a test elsewhere that pressed `g` decides where this one starts.
	resetSettingsForTest(t)
	m := &model{
		pane: paneEvents, selectedSess: "s", bodyHeight: 12, width: 200,
		events: map[string][]pipeline.SessionEvent{"s": cursorRowsFixture(n)},
	}
	m.eventsTbl = newEventsTable()
	m.rebuildEventsTable()
	if got := len(m.eventsTbl.Rows()); got != n {
		t.Fatalf("fixture built %d rows, want %d", got, n)
	}
	if h := m.eventsTbl.Height(); h >= n {
		t.Fatalf("fixture height %d must be smaller than %d rows to exercise scrolling", h, n)
	}
	return m
}

// assertSelectionVisible fails when the highlighted row is outside the window the
// table actually renders. The highlight is drawn on the cursor row wherever it is,
// so a cursor outside the rendered window means a table with no visible highlight
// at all — which is what an operator sees.
func assertSelectionVisible(t *testing.T, tbl table.Model, label string) {
	t.Helper()
	cur := tbl.Cursor()
	if cur < 0 {
		t.Errorf("%s: no row selected (cursor=%d)", label, cur)
		return
	}
	view := tbl.View()
	// Read the marker off the SELECTED ROW, not from the cursor index. Once a filter is
	// on, row N is no longer event N, so deriving the token from the index would assert
	// that some unrelated row is on screen.
	want, ok := markerOf(tbl.SelectedRow())
	if !ok {
		t.Errorf("%s: selected row %d carries no marker cell: %q", label, cur, tbl.SelectedRow())
		return
	}
	if strings.Contains(view, want) {
		return
	}
	var onScreen []int
	for i := 0; i < len(tbl.Rows()); i++ {
		if strings.Contains(view, hostToken(i)) {
			onScreen = append(onScreen, i)
		}
	}
	win := "nothing"
	if len(onScreen) > 0 {
		win = fmt.Sprintf("rows %d..%d", onScreen[0], onScreen[len(onScreen)-1])
	}
	t.Errorf("%s: selected row %d is off screen; the view shows %s", label, cur, win)
}

// TestSetCursorVisible_LandsOnScreen is the unit-level guard.
//
// table.SetCursor re-windows the rendered rows around the new cursor
// (start = cursor − height) but never reconciles the viewport's YOffset, so for any
// target at or past one screenful the cursor lands exactly one line below the last
// visible row. setCursorVisible must put the cursor where it was asked AND leave it
// on screen, for targets on both sides of that boundary.
func TestSetCursorVisible_LandsOnScreen(t *testing.T) {
	const n = 40
	for _, target := range []int{0, 1, 5, 10, 11, 12, 20, 33, 38, 39} {
		m := cursorModel(t, n)
		setCursorVisible(&m.eventsTbl, target)
		if got := m.eventsTbl.Cursor(); got != target {
			t.Errorf("target %d: cursor=%d", target, got)
		}
		assertSelectionVisible(t, m.eventsTbl, fmt.Sprintf("target %d", target))
	}
}

// Out-of-range targets clamp to the row range rather than leaving the table with a
// cursor pointing at nothing — SetCursor's own clamping behaviour, preserved.
func TestSetCursorVisible_ClampsAndSurvivesEmpty(t *testing.T) {
	m := cursorModel(t, 40)
	setCursorVisible(&m.eventsTbl, 999)
	if got := m.eventsTbl.Cursor(); got != 39 {
		t.Errorf("cursor past the end = %d, want 39", got)
	}
	assertSelectionVisible(t, m.eventsTbl, "target past the end")

	setCursorVisible(&m.eventsTbl, -5)
	if got := m.eventsTbl.Cursor(); got != 0 {
		t.Errorf("negative target = %d, want 0", got)
	}
	assertSelectionVisible(t, m.eventsTbl, "negative target")

	// An empty table is left exactly as it was, which is the most the helper can promise:
	// with no rows there is no row to select, and where a bubbles table parks its cursor
	// when empty depends on how it emptied — a fresh one sits at 0, one whose rows were
	// taken away by SetRows(nil) at -1. Neither index addresses a row, so the helper
	// declines to move rather than picking one, and this pins "unchanged" rather than a
	// value that would only describe one of the two ways in.
	for _, tc := range []struct {
		name  string
		build func() table.Model
	}{
		{"fresh", newEventsTable},
		{"emptied by SetRows(nil)", func() table.Model {
			tbl := newEventsTable()
			tbl.SetRows([]table.Row{make(table.Row, len(tbl.Columns()))})
			tbl.SetRows(nil)
			return tbl
		}},
	} {
		empty := tc.build()
		before := empty.Cursor()
		setCursorVisible(&empty, 3)
		if got := empty.Cursor(); got != before {
			t.Errorf("%s empty table: cursor moved %d -> %d, want it left alone", tc.name, before, got)
		}

		// setTableHeight shares the promise, because it reaches the cursor through
		// GotoTop. table's clamp is min(max(v, low), high) and does NOT swap inverted
		// bounds (viewport's does — different package, and not the one MoveUp uses), so
		// GotoTop on a table with no rows resolves clamp(0, 0, -1) to −1 and would walk
		// a fresh table's cursor off row 0. The height must still be applied.
		resized := tc.build()
		before = resized.Cursor()
		wasHeight := resized.Height()
		setTableHeight(&resized, wasHeight+7)
		if got := resized.Cursor(); got != before {
			t.Errorf("%s empty table: setTableHeight moved the cursor %d -> %d, want it left alone",
				tc.name, before, got)
		}
		if got := resized.Height(); got == wasHeight {
			t.Errorf("%s empty table: setTableHeight did not apply the new height (still %d)",
				tc.name, got)
		}
	}
}

// TestEventsTable_AutoFollowKeepsSelectionVisible is the reported bug.
//
// The pane rebuilds on every incoming event, and the rebuild restores the cursor —
// to the last row while following the tail. That restore used to lose the highlight,
// so on a live session the highlight vanished on the next event even when the
// operator had scrolled to the bottom with the arrow keys. Pressing up then scrolled
// the rows one at a time with no highlight anywhere, because the cursor stayed
// exactly one row below the rendered window.
func TestEventsTable_AutoFollowKeepsSelectionVisible(t *testing.T) {
	m := cursorModel(t, 40)

	// The first rebuild already follows the tail.
	assertSelectionVisible(t, m.eventsTbl, "after initial rebuild")
	if got, want := m.eventsTbl.Cursor(), 39; got != want {
		t.Fatalf("auto-follow cursor = %d, want %d", got, want)
	}

	// A rebuild while following the tail keeps the highlight on screen.
	m.rebuildEventsTable()
	assertSelectionVisible(t, m.eventsTbl, "rebuild while following")

	// Arrow up, then more rebuilds: the highlight stays visible and the cursor
	// keeps walking up one row at a time.
	for i := 1; i <= 6; i++ {
		m.eventsTbl, _ = m.eventsTbl.Update(tea.KeyMsg{Type: tea.KeyUp})
		assertSelectionVisible(t, m.eventsTbl, fmt.Sprintf("up #%d", i))
		if got, want := m.eventsTbl.Cursor(), 39-i; got != want {
			t.Fatalf("up #%d: cursor = %d, want %d", i, got, want)
		}
		m.rebuildEventsTable()
		assertSelectionVisible(t, m.eventsTbl, fmt.Sprintf("up #%d + rebuild", i))
		if got, want := m.eventsTbl.Cursor(), 39-i; got != want {
			t.Fatalf("up #%d + rebuild: cursor = %d, want %d", i, got, want)
		}
	}
}

// A new event arriving while the operator reads mid-list must not move the
// highlight, and must not lose it either — the other branch of the rebuild.
func TestEventsTable_RebuildMidListKeepsSelectionVisible(t *testing.T) {
	m := cursorModel(t, 40)
	setCursorVisible(&m.eventsTbl, 20)
	assertSelectionVisible(t, m.eventsTbl, "parked mid-list")

	m.events["s"] = append(m.events["s"], cursorRowsFixture(41)[40])
	m.rebuildEventsTable()
	if got := m.eventsTbl.Cursor(); got != 20 {
		t.Errorf("cursor moved on a new event: %d, want 20", got)
	}
	assertSelectionVisible(t, m.eventsTbl, "new event while parked mid-list")
}

// TestGoBottom_KeepsSelectionVisible covers the G/end key, which jumps straight to
// the last row — the fastest way to reproduce this by hand.
func TestGoBottom_KeepsSelectionVisible(t *testing.T) {
	m := cursorModel(t, 40)
	setCursorVisible(&m.eventsTbl, 0)

	m.goBottom()
	if got, want := m.eventsTbl.Cursor(), 39; got != want {
		t.Fatalf("goBottom cursor = %d, want %d", got, want)
	}
	assertSelectionVisible(t, m.eventsTbl, "goBottom")

	for i := 1; i <= 3; i++ {
		m.eventsTbl, _ = m.eventsTbl.Update(tea.KeyMsg{Type: tea.KeyUp})
		assertSelectionVisible(t, m.eventsTbl, fmt.Sprintf("goBottom then up #%d", i))
	}

	m.goTop()
	if got := m.eventsTbl.Cursor(); got != 0 {
		t.Fatalf("goTop cursor = %d, want 0", got)
	}
	assertSelectionVisible(t, m.eventsTbl, "goTop")
}

// A terminal resize re-heights the table mid-session. SetHeight re-windows the rows
// the same way SetCursor does, so the highlight must survive it.
func TestEventsTable_ResizeKeepsSelectionVisible(t *testing.T) {
	m := cursorModel(t, 40)
	setCursorVisible(&m.eventsTbl, 30)
	for _, h := range []int{40, 20, 12, 8, 30} {
		m.bodyHeight = h
		m.rebuildEventsTable()
		assertSelectionVisible(t, m.eventsTbl, fmt.Sprintf("bodyHeight %d", h))
	}
}

// TestEventsTable_FilterShrinkKeepsSelectionVisible covers the rows-SHRINK case, the one the
// old `else if prevRow < len(rows)` skipped outright: typing a filter or toggling
// hideInactive can pull the list out from under the cursor, and with no restore the cursor
// was left wherever SetRows had clamped it, with an offset nobody reconciled.
//
// Both shrink paths are exercised, and both directions of the round trip, because the
// interesting state is the one where the stale offset still addresses a line of the new,
// shorter content — a drastic shrink is self-correcting, a moderate one is not.
func TestEventsTable_FilterShrinkKeepsSelectionVisible(t *testing.T) {
	for _, tc := range []struct {
		name     string
		shrink   func(m *model)
		wantRows int
	}{
		// "h3" keeps h30..h39: ten rows, every one of them below the parked cursor.
		{"filter to ten rows", func(m *model) { m.filter = "h3" }, 10},
		// A single row is the drastic case: shorter than the table height.
		{"filter to one row", func(m *model) { m.filter = "h07" }, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := cursorModel(t, 40)
			setCursorVisible(&m.eventsTbl, 30)
			assertSelectionVisible(t, m.eventsTbl, "parked at row 30")

			tc.shrink(m)
			m.rebuildEventsTable()
			if got := len(m.eventsTbl.Rows()); got != tc.wantRows {
				t.Fatalf("filter left %d rows, want %d", got, tc.wantRows)
			}
			assertSelectionVisible(t, m.eventsTbl, "after the filter shrank the list")
			if got, want := m.eventsTbl.Cursor(), tc.wantRows-1; got > want {
				t.Errorf("cursor %d addresses no row in a %d-row list", got, tc.wantRows)
			}
			// The detail pane reads the selection through this, so a cursor the table
			// clamped but the model did not follow shows an empty pane.
			if _, ok := m.selectedEventRow(); !ok {
				t.Error("no selected row resolves after the shrink")
			}

			// Clearing the filter grows the list back; the cursor must still be on screen.
			m.filter = ""
			m.rebuildEventsTable()
			if got := len(m.eventsTbl.Rows()); got != 40 {
				t.Fatalf("clearing the filter left %d rows, want 40", got)
			}
			assertSelectionVisible(t, m.eventsTbl, "after clearing the filter")
			if _, ok := m.selectedEventRow(); !ok {
				t.Error("no selected row resolves after clearing the filter")
			}
		})
	}
}

// hideInactive is the other shrink lever, and it hides rows from the MIDDLE of the list
// rather than the ends, so the surviving rows keep no relationship to their old indices.
func TestEventsTable_HideInactiveShrinkKeepsSelectionVisible(t *testing.T) {
	events := cursorRowsFixture(40)
	// Give a handful of events a plugin invocation so they survive the inactive filter.
	active := map[int]bool{2: true, 9: true, 17: true, 26: true, 33: true, 38: true}
	for i := range events {
		if active[i] {
			events[i].Invocations = &pipeline.Invocations{Outbound: []pipeline.Invocation{
				{Plugin: "tool-prune", Action: pipeline.ActionModify, Reason: "tools_pruned"},
			}}
		}
	}
	m := &model{
		pane: paneEvents, selectedSess: "s", bodyHeight: 12, width: 200,
		events: map[string][]pipeline.SessionEvent{"s": events},
	}
	m.eventsTbl = newEventsTable()
	m.rebuildEventsTable()
	setCursorVisible(&m.eventsTbl, 30)
	assertSelectionVisible(t, m.eventsTbl, "parked at row 30")

	m.hideInactive = true
	m.rebuildEventsTable()
	if got, want := len(m.eventsTbl.Rows()), len(active); got != want {
		t.Fatalf("hideInactive left %d rows, want %d", got, want)
	}
	assertSelectionVisible(t, m.eventsTbl, "after hideInactive shrank the list")
	if _, ok := m.selectedEventRow(); !ok {
		t.Error("no selected row resolves after hideInactive")
	}

	m.hideInactive = false
	m.rebuildEventsTable()
	assertSelectionVisible(t, m.eventsTbl, "after hideInactive off")
}

// sessionsModel builds a sessions pane with n sessions, taller than the table, and the
// cursor parked on one of them. IDs carry the marker so the same visibility assertion works.
func sessionsModel(t *testing.T, n int) *model {
	t.Helper()
	m := &model{pane: paneSessions, width: 200}
	m.sessionsTbl = newSessionsTable()
	m.sessionsTbl.SetHeight(12)
	for i := 0; i < n; i++ {
		m.sessions = append(m.sessions, session.SessionSummary{
			ID: hostToken(i), UpdatedAt: time.Now(), EventCount: 3,
		})
	}
	m.rebuildSessionsTable()
	if got := len(m.sessionsTbl.Rows()); got != n {
		t.Fatalf("fixture built %d session rows, want %d", got, n)
	}
	if h := m.sessionsTbl.Height(); h >= n {
		t.Fatalf("fixture height %d must be smaller than %d rows", h, n)
	}
	return m
}

// The sessions pane restores its cursor BY SESSION ID on every refresh, which arrives on a
// poll rather than a keystroke — so the same lost highlight was one refresh away there too,
// for any session list longer than the pane.
func TestSessionsTable_RestoreByIDKeepsSelectionVisible(t *testing.T) {
	m := sessionsModel(t, 40)
	setCursorVisible(&m.sessionsTbl, 30)
	assertSelectionVisible(t, m.sessionsTbl, "parked at session 30")
	want := m.selectedSessionID()

	// A refresh that changes nothing must not move or lose the selection.
	m.rebuildSessionsTable()
	if got := m.selectedSessionID(); got != want {
		t.Errorf("refresh moved the selection to %q, want %q", got, want)
	}
	assertSelectionVisible(t, m.sessionsTbl, "after a refresh")

	// A refresh that drops the selected session falls back to the first row — the branch
	// that absorbed the old len(rows) > 0 guard, so it must survive an empty list too.
	m.sessions = m.sessions[:20]
	m.rebuildSessionsTable()
	if got, want := m.sessionsTbl.Cursor(), 0; got != want {
		t.Errorf("cursor after the selected session vanished = %d, want %d", got, want)
	}
	assertSelectionVisible(t, m.sessionsTbl, "after the selected session vanished")

	m.sessions = nil
	m.rebuildSessionsTable()
	if got := len(m.sessionsTbl.Rows()); got != 0 {
		t.Fatalf("expected an empty sessions table, got %d rows", got)
	}
	if got := m.selectedSessionID(); got != "" {
		t.Errorf("empty sessions table reports %q selected", got)
	}
}

// pipelineModel builds a pipeline pane with an inbound and an outbound chain either side of
// the divider, long enough that the table scrolls.
func pipelineModel(t *testing.T, inbound, outbound int) *model {
	t.Helper()
	m := &model{pane: panePipeline, width: 200, pipeline: &apiclient.PipelineView{}}
	m.pipelineTbl = newPipelineTable()
	m.pipelineTbl.SetHeight(12)
	for i := 0; i < inbound; i++ {
		m.pipeline.Inbound = append(m.pipeline.Inbound, apiclient.PipelinePlugin{
			Name: fmt.Sprintf("in-%02d", i), Direction: "inbound", Position: i + 1,
		})
	}
	for i := 0; i < outbound; i++ {
		m.pipeline.Outbound = append(m.pipeline.Outbound, apiclient.PipelinePlugin{
			Name: fmt.Sprintf("out-%02d", i), Direction: "outbound", Position: i + 1,
		})
	}
	m.rebuildPipelineTable()
	return m
}

// The divider nudge moves by one row in the direction of travel. As MoveUp/MoveDown that is
// index-equivalent to SetCursor(±1) — including the clamp at both ends — but it also moves
// the viewport offset, which is both the point and a visible change: skipping the divider can
// now scroll the pane by a line. Pinned here so neither half drifts.
func TestPipelineTable_DividerNudgeKeepsSelectionVisible(t *testing.T) {
	m := pipelineModel(t, 20, 20)
	rows := m.pipelineTbl.Rows()
	divider := -1
	for i := range rows {
		if isDividerRow(rows, i) {
			divider = i
			break
		}
	}
	if divider <= 0 || divider >= len(rows)-1 {
		t.Fatalf("divider at %d in %d rows; fixture needs plugins on both sides", divider, len(rows))
	}

	// Travelling DOWN onto the divider skips to the row after it.
	setCursorVisible(&m.pipelineTbl, divider-1)
	m.pipelineTbl, _ = m.pipelineTbl.Update(tea.KeyMsg{Type: tea.KeyDown})
	if isDividerRow(m.pipelineTbl.Rows(), m.pipelineTbl.Cursor()) {
		m.pipelineTbl.MoveDown(1) // what handleKey does for panePipeline
	}
	if got, want := m.pipelineTbl.Cursor(), divider+1; got != want {
		t.Errorf("down onto the divider left cursor %d, want %d", got, want)
	}
	if p := m.selectedPlugin(); p == nil {
		t.Error("cursor rests on the divider after a downward nudge")
	}
	assertPipelineSelectionVisible(t, m, "after a downward nudge")

	// Travelling UP onto it skips to the row before.
	setCursorVisible(&m.pipelineTbl, divider+1)
	m.pipelineTbl, _ = m.pipelineTbl.Update(tea.KeyMsg{Type: tea.KeyUp})
	if isDividerRow(m.pipelineTbl.Rows(), m.pipelineTbl.Cursor()) {
		m.pipelineTbl.MoveUp(1)
	}
	if got, want := m.pipelineTbl.Cursor(), divider-1; got != want {
		t.Errorf("up onto the divider left cursor %d, want %d", got, want)
	}
	if p := m.selectedPlugin(); p == nil {
		t.Error("cursor rests on the divider after an upward nudge")
	}
	assertPipelineSelectionVisible(t, m, "after an upward nudge")

	// A rebuild whose cursor lands on the divider nudges past it and keeps it on screen.
	setCursorVisible(&m.pipelineTbl, divider)
	m.rebuildPipelineTable()
	if isDividerRow(m.pipelineTbl.Rows(), m.pipelineTbl.Cursor()) {
		t.Errorf("rebuild left the cursor on the divider at %d", m.pipelineTbl.Cursor())
	}
	assertPipelineSelectionVisible(t, m, "after a rebuild off the divider")
}

// The clamp at both ends: an inbound-only chain puts the divider last, where nudging DOWN
// has nowhere to go. SetCursor(+1) clamped there and so does MoveDown(1) — the cursor stays
// on the divider, and selectedPlugin returning nil is what keeps the detail pane honest.
func TestPipelineTable_DividerNudgeClampsAtTheEnds(t *testing.T) {
	m := pipelineModel(t, 20, 0)
	rows := m.pipelineTbl.Rows()
	if last := len(rows) - 1; !isDividerRow(rows, last) {
		t.Fatalf("expected the divider last in an inbound-only chain, rows=%d", len(rows))
	}
	last := len(rows) - 1
	m.pipelineTbl.SetCursor(last)
	m.pipelineTbl.MoveDown(1)
	if got := m.pipelineTbl.Cursor(); got != last {
		t.Errorf("nudge past the last row moved cursor to %d, want %d", got, last)
	}
	if p := m.selectedPlugin(); p != nil {
		t.Errorf("divider row reported plugin %q", p.Name)
	}

	// And an outbound-only chain puts it first, where nudging UP has nowhere to go.
	m = pipelineModel(t, 0, 20)
	if !isDividerRow(m.pipelineTbl.Rows(), 0) {
		t.Fatal("expected the divider first in an outbound-only chain")
	}
	m.pipelineTbl.SetCursor(0)
	m.pipelineTbl.MoveUp(1)
	if got := m.pipelineTbl.Cursor(); got != 0 {
		t.Errorf("nudge above the first row moved cursor to %d, want 0", got)
	}
}

// assertPipelineSelectionVisible is assertSelectionVisible for the pipeline table, whose rows
// carry plugin names rather than the host marker.
func assertPipelineSelectionVisible(t *testing.T, m *model, label string) {
	t.Helper()
	row := m.pipelineTbl.SelectedRow()
	if len(row) < 3 {
		t.Errorf("%s: no row selected", label)
		return
	}
	if name := strings.TrimSpace(row[2]); !strings.Contains(m.pipelineTbl.View(), name) {
		t.Errorf("%s: selected row %d (%q) is off screen", label, m.pipelineTbl.Cursor(), name)
	}
}

// TestEventsTable_TailWithArrivingEventsKeepsSelectionVisible is the live-session shape:
// parked on the last row while the pane keeps rebuilding as events arrive.
//
// Reported as "it highlights the last row, then a second later deselects it, goes one line
// up, and the highlight disappears". The cursor does NOT actually move — what moves is the
// rendered window, by exactly one row, which takes the selected row off the bottom edge. So
// the index stays right while the screen looks deselected, and both halves are asserted here.
func TestEventsTable_TailWithArrivingEventsKeepsSelectionVisible(t *testing.T) {
	m := cursorModel(t, 40)
	for i := 0; i < 40; i++ { // arrive at the tail the way an operator does
		m.eventsTbl, _ = m.eventsTbl.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	assertSelectionVisible(t, m.eventsTbl, "at the tail by arrow key")

	// Each tick: one more event, then a rebuild. Auto-follow must keep the selection on
	// the new last row AND on screen.
	for tick := 1; tick <= 4; tick++ {
		next := len(m.events["s"])
		m.events["s"] = append(m.events["s"], pipeline.SessionEvent{
			Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
			Host:      hostToken(next),
			Inference: &pipeline.InferenceExtension{Model: "m"},
		})
		m.rebuildEventsTable()
		label := fmt.Sprintf("tick %d", tick)
		if got, want := m.eventsTbl.Cursor(), len(m.eventsTbl.Rows())-1; got != want {
			t.Errorf("%s: cursor = %d, want the new last row %d", label, got, want)
		}
		assertSelectionVisible(t, m.eventsTbl, label)
	}

	// And a tick that adds nothing still must not drop the highlight.
	m.rebuildEventsTable()
	assertSelectionVisible(t, m.eventsTbl, "idle tick at the tail")
}

// renderedWindow reports the span of fixture rows the table currently shows, as
// "first..last". That span IS the scroll position in the only terms an operator can
// see, and it is what the tests below pin: the cursor INDEX was never wrong
// here, the window under it moved.
func renderedWindow(t *testing.T, tbl table.Model) string {
	t.Helper()
	view := tbl.View()
	first, last := -1, -1
	for i := 0; i < len(tbl.Rows()); i++ {
		if strings.Contains(view, hostToken(i)) {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	// Fails rather than returning a sentinel. Every caller compares one window against
	// another, so a "nothing" would have matched a "nothing" and passed vacuously — the
	// exact shape of failure these tests exist to catch.
	if first < 0 {
		t.Fatalf("no fixture row is rendered at all; the window assertions would be vacuous")
	}
	return fmt.Sprintf("%d..%d", first, last)
}

// TestEventsTable_PollRebuildKeepsScrollPosition is the reported bug: scroll to the
// last message, arrow back up a few rows, and a few seconds later the row under the
// cursor is the bottom row on screen.
//
// The few seconds are the two-second sessions poll: refreshTickMsg → loadSessionsCmd →
// sessionsLoadedMsg calls rebuildEventsTable while the events pane is open, whether or
// not any event arrived. That rebuild restored the cursor index correctly but re-anchored
// the window under it, so every poll slid the rows the operator had scrolled back to off
// the bottom edge.
//
// A rebuild that changes no data must change no pixel: same cursor, same window.
func TestEventsTable_PollRebuildKeepsScrollPosition(t *testing.T) {
	for _, tc := range []struct {
		name string
		park int
	}{
		// Reading back from the tail — the reported path.
		{"scrolled back from the tail", 39},
		// And well away from either end, where nothing clamps the answer for us.
		{"scrolled back mid-list", 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := cursorModel(t, 40)
			setCursorVisible(&m.eventsTbl, tc.park)
			for i := 0; i < 3; i++ {
				m.eventsTbl, _ = m.eventsTbl.Update(tea.KeyMsg{Type: tea.KeyUp})
			}
			wantCursor, wantWindow := m.eventsTbl.Cursor(), renderedWindow(t, m.eventsTbl)
			if wantCursor != tc.park-3 {
				t.Fatalf("three arrows up from %d left cursor %d", tc.park, wantCursor)
			}

			// Two polls, because one that merely delays the jerk is not a fix.
			for poll := 1; poll <= 2; poll++ {
				m.rebuildEventsTable()
				if got := m.eventsTbl.Cursor(); got != wantCursor {
					t.Errorf("poll %d moved the cursor: %d, want %d", poll, got, wantCursor)
				}
				if got := renderedWindow(t, m.eventsTbl); got != wantWindow {
					t.Errorf("poll %d scrolled the pane: showing rows %s, want %s", poll, got, wantWindow)
				}
			}
		})
	}
}

// The same guarantee when the poll actually brings something: an event appended while
// the operator reads mid-list must not scroll the rows out from under them either.
func TestEventsTable_NewEventWhileScrolledBackKeepsScrollPosition(t *testing.T) {
	m := cursorModel(t, 40)
	setCursorVisible(&m.eventsTbl, 39)
	for i := 0; i < 3; i++ {
		m.eventsTbl, _ = m.eventsTbl.Update(tea.KeyMsg{Type: tea.KeyUp})
	}
	wantCursor, wantWindow := m.eventsTbl.Cursor(), renderedWindow(t, m.eventsTbl)

	m.events["s"] = cursorRowsFixture(41)
	m.rebuildEventsTable()
	if got := m.eventsTbl.Cursor(); got != wantCursor {
		t.Errorf("a new event moved the cursor: %d, want %d", got, wantCursor)
	}
	if got := renderedWindow(t, m.eventsTbl); got != wantWindow {
		t.Errorf("a new event scrolled the pane: showing rows %s, want %s", got, wantWindow)
	}
}

// The other half of the contract, and the reason this cannot be fixed by simply never
// scrolling: an operator sitting ON the last row is following the tail, and the tail
// must keep advancing under them.
func TestEventsTable_TailStillFollowsOnNewEvent(t *testing.T) {
	m := cursorModel(t, 40)
	if got, want := m.eventsTbl.Cursor(), 39; got != want {
		t.Fatalf("fixture should open following the tail: cursor %d, want %d", got, want)
	}
	before := renderedWindow(t, m.eventsTbl)

	m.events["s"] = cursorRowsFixture(41)
	m.rebuildEventsTable()
	if got, want := m.eventsTbl.Cursor(), 40; got != want {
		t.Errorf("tail-follow left the cursor at %d, want the new last row %d", got, want)
	}
	if got := renderedWindow(t, m.eventsTbl); got == before {
		t.Errorf("tail-follow did not scroll: still showing rows %s", got)
	}
	assertSelectionVisible(t, m.eventsTbl, "tail-follow onto a new event")
}

// The sessions picker restores its cursor BY ID on the same two-second poll, so it had
// the same jerk — and unlike the events pane it is the first thing an operator lands on.
func TestSessionsTable_PollRebuildKeepsScrollPosition(t *testing.T) {
	m := sessionsModel(t, 40)
	setCursorVisible(&m.sessionsTbl, 30)
	for i := 0; i < 3; i++ {
		m.sessionsTbl, _ = m.sessionsTbl.Update(tea.KeyMsg{Type: tea.KeyUp})
	}
	wantID := m.selectedSessionID()
	wantCursor, wantWindow := m.sessionsTbl.Cursor(), renderedWindow(t, m.sessionsTbl)

	for poll := 1; poll <= 2; poll++ {
		m.rebuildSessionsTable()
		if got := m.selectedSessionID(); got != wantID {
			t.Errorf("poll %d moved the selection to %q, want %q", poll, got, wantID)
		}
		if got := m.sessionsTbl.Cursor(); got != wantCursor {
			t.Errorf("poll %d moved the cursor: %d, want %d", poll, got, wantCursor)
		}
		if got := renderedWindow(t, m.sessionsTbl); got != wantWindow {
			t.Errorf("poll %d scrolled the picker: showing rows %s, want %s", poll, got, wantWindow)
		}
	}
}

// TestTables_ResizeKeepsSelectionVisible guards the sharp edge of that fix.
//
// Restoring the cursor to the row it is already on no longer touches the scroll
// offset — that is the fix — so the blanket re-anchor that used to happen on every
// poll is gone, and with it the accidental repair of an offset invalidated by a
// resize. SetHeight re-windows the rendered rows (start = cursor − height) while the
// viewport keeps the offset it computed for the old height, and the pane can end up
// rendering a window entirely below the row it highlights.
//
// Arrow keys are what make this reachable: each MoveUp raises the offset, so the
// deeper the operator has scrolled up, the further the stale offset is from anything
// true. Driven through layout() rather than SetHeight, because layout() is where the
// tables it does not rebuild get their height — and therefore the only place that can
// reconcile them.
func TestTables_ResizeKeepsSelectionVisible(t *testing.T) {
	// Not every combination exercises both halves, and it is worth knowing which:
	// termH 15 lands on the height sessionsModel already set, so setTableHeight takes
	// its early return and nothing resizes — that row checks only that the selection
	// survives a poll. termH 60 gives the 40-row fixture more rows than it has, so
	// every row renders and the visibility half holds at any offset. 7, 9, 11 and 33
	// all resize AND leave rows off screen, which is where the assertion has teeth.
	for _, ups := range []int{0, 3, 8, 15, 25} {
		for _, termH := range []int{7, 9, 11, 15, 33, 60} {
			m := sessionsModel(t, 40)
			m.width, m.height = 200, 15
			setCursorVisible(&m.sessionsTbl, 39)
			for i := 0; i < ups; i++ {
				m.sessionsTbl, _ = m.sessionsTbl.Update(tea.KeyMsg{Type: tea.KeyUp})
			}
			want := m.selectedSessionID()

			m.height = termH
			m.layout()
			label := fmt.Sprintf("%d ups, terminal height %d", ups, termH)
			assertSelectionVisible(t, m.sessionsTbl, label)
			if got := m.selectedSessionID(); got != want {
				t.Errorf("%s: resize moved the selection to %q, want %q", label, got, want)
			}
			// And the poll that follows must not undo the repair.
			m.rebuildSessionsTable()
			assertSelectionVisible(t, m.sessionsTbl, label+", after the next poll")
		}
	}
}

// TestViewports_GrowingTheTerminalDoesNotStrandTheOffset covers the two scrollable
// viewports — the shared detail pane and the help overlay.
//
// viewport.Height is a plain field, so assigning it on resize moves maxYOffset while
// YOffset stays put, and viewport.SetContent only clamps against the line COUNT. Grow
// the terminal under a viewport scrolled near its end and the offset is left past the
// bottom: the body renders high with dead space beneath it and no key but a scroll
// brings it back. PastBottom is the viewport's own name for that state.
//
// Both are asserted after a GotoBottom, which is where the gap between YOffset and
// maxYOffset is widest.
func TestViewports_GrowingTheTerminalDoesNotStrandTheOffset(t *testing.T) {
	t.Run("detail pane", func(t *testing.T) {
		m := fitModel(t, paneEvents, 80, 24, []pipeline.SessionEvent{fatInferenceEvent()})
		m.rebuildEventsTable()
		er, ok := m.selectedEventRow()
		if !ok {
			t.Fatal("fixture has no selectable event row")
		}
		m.showDetail(er, true)
		m.pane = paneDetail
		m.detailVp.GotoBottom()
		// Without this the case is vacuous: GotoBottom on content that fits leaves the
		// offset at 0, and "not past the bottom" then holds however the resize behaves.
		if m.detailVp.YOffset == 0 {
			t.Fatal("fixture is not scrollable at 80x24")
		}

		m.width, m.height = 120, 60
		m.layout()
		if m.detailVp.PastBottom() {
			t.Errorf("detail viewport left past the bottom: YOffset %d, height %d",
				m.detailVp.YOffset, m.detailVp.Height)
		}
		// PastBottom is the library's own name for the state, but the symptom is what
		// the pane draws, so assert that too: a stranded offset renders the body high
		// with dead space under it, and at the far end nothing at all.
		if strings.TrimSpace(m.detailVp.View()) == "" {
			t.Error("detail viewport renders nothing after the resize")
		}
	})

	// The plugin detail pane shares detailVp but is NOT re-rendered by layout(), so
	// its offset can only be reconciled where the height is assigned.
	t.Run("plugin detail pane", func(t *testing.T) {
		m := pluginDetailModel(t)
		m.detailVp.GotoBottom()
		if m.detailVp.YOffset == 0 {
			t.Fatal("fixture is not scrollable")
		}
		// AMPLY past the content, not barely. At height 60 this premise held by one row
		// until the spend band took a second row from the body, after which the viewport
		// was one line shorter than the fixture, offset 1 was legitimately not
		// PastBottom, and the pane opened on its second line — a failure about the
		// fixture's headroom rather than about stranded offsets.
		m.width, m.height = 100, 80
		m.layout()
		if m.detailVp.PastBottom() {
			t.Errorf("plugin detail viewport left past the bottom: YOffset %d, height %d",
				m.detailVp.YOffset, m.detailVp.Height)
		}
		// Grown past its own content, so the offset must be back at the top and the
		// pane must be showing the plugin from its first line — the rendered form of
		// "not stranded". This is also the case that would catch layout() re-rendering
		// somebody else's content into this pane (see keys.go's showDetail guard).
		if view := m.detailVp.View(); !strings.Contains(view, "tool-prune") {
			t.Errorf("plugin detail pane does not show its plugin after the resize:\n%s", view)
		}
	})

	t.Run("help overlay", func(t *testing.T) {
		m := fitModel(t, paneEvents, 80, 24, cursorRowsFixture(60))
		m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
		if !m.helpVisible {
			t.Fatal("\"?\" did not open the help overlay")
		}
		m.helpVp.GotoBottom()
		// The same guard as the two cases above, and this one is the closest to the
		// line: the help body for paneEvents runs only a handful of lines past the
		// overlay's height, so deleting a few entries from globalKeys/paneKeys would
		// turn this into an unconditional pass.
		if m.helpVp.YOffset == 0 {
			t.Fatal("help body is not scrollable at 80x24; the case would be vacuous")
		}

		next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 60})
		mm, ok := next.(*model)
		if !ok {
			t.Fatalf("Update returned %T, want *model", next)
		}
		if mm.helpVp.PastBottom() {
			t.Errorf("help viewport left past the bottom: YOffset %d, height %d",
				mm.helpVp.YOffset, mm.helpVp.Height)
		}
		if strings.TrimSpace(mm.helpVp.View()) == "" {
			t.Error("help overlay renders nothing after the resize")
		}
	})
}

// sortCursorFixture is a distinguishable version of cursorRowsFixture: distinct
// timestamps and request ids, so keyOf gives every event its OWN eventKey, plus a
// duration that is deliberately NOT in arrival order so a DURATION sort has to move
// rows. cursorRowsFixture leaves At and RequestID at their zero values, which makes
// every one of its events share a single eventKey — fine for the scroll-geometry
// tests it was written for, useless for asking which event the cursor is on.
func sortCursorFixture(n int) []pipeline.SessionEvent {
	base := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	events := make([]pipeline.SessionEvent, n)
	for i := range events {
		events[i] = pipeline.SessionEvent{
			Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
			Host:      hostToken(i),
			RequestID: fmt.Sprintf("req-%d", i),
			At:        base.Add(time.Duration(i) * time.Second),
			// Durations that agree with arrival order nowhere, and specifically put
			// NEITHER the first nor the last-arriving event at either end of the sorted
			// list. That is what lets a test tell tail-follow apart from the identity
			// pin: if the newest event were also the smallest, "follow the tail" and
			// "stay on my event" would name the same row and the two would be
			// indistinguishable.
			Duration:  time.Duration(1+(i*37+11)%(n*3)) * time.Millisecond,
			Inference: &pipeline.InferenceExtension{Model: "m"},
		}
	}
	return events
}

func sortCursorModel(t *testing.T, n int) *model {
	t.Helper()
	resetSettingsForTest(t) // same reasoning as cursorModel
	m := &model{
		pane: paneEvents, selectedSess: "s", bodyHeight: 12, width: 200,
		events: map[string][]pipeline.SessionEvent{"s": sortCursorFixture(n)},
	}
	m.eventsTbl = newEventsTable()
	m.rebuildEventsTable()
	if got := len(m.eventsTbl.Rows()); got != n {
		t.Fatalf("fixture built %d rows, want %d", got, n)
	}
	return m
}

// Tail-follow is chronological-only (#865). "Stay at the bottom" means "follow the
// newest event" only because the last row IS the newest; under a DURATION sort the
// last row is the SMALLEST value, so following it would drag the cursor to a
// different event on every streamed message — and away from the large-value end the
// operator sorted to look at.
func TestEventsTable_SortSuppressesTailFollow(t *testing.T) {
	m := sortCursorModel(t, 40)

	// Chronological: the cursor rides the tail as new events land.
	if got := m.eventsTbl.Cursor(); got != 39 {
		t.Fatalf("chronological cursor = %d, want 39 (tail)", got)
	}
	m.events["s"] = append(m.events["s"], sortCursorFixture(41)[40])
	m.rebuildEventsTable()
	if got := m.eventsTbl.Cursor(); got != 40 {
		t.Fatalf("chronological: cursor = %d after a new event, want 40 (followed the tail)", got)
	}

	// Pin the event the operator is on — the newest one, since they were following
	// the live tail. By eventKey, not by pointer: appending to m.events reallocates
	// the backing slice, so the same event legitimately gets a new address.
	m.selectedEventKey = keyOf(m.selectedEvent())
	before := m.selectedEventKey
	if before == (eventKey{}) {
		t.Fatal("no event under the cursor")
	}

	// Turn the sort on. wasAtEnd is computed from the table as it stood BEFORE this
	// rebuild, and the cursor is still on the last row from the follow above — so the
	// very rebuild that applies the sort is a wasAtEnd rebuild. That is exactly the
	// case the gate exists for: an operator following the live tail turns a sort on,
	// and "the tail" silently stops meaning "the newest event". Ungated, the cursor
	// re-anchors to whichever event now sorts last, which is a different one (the
	// fixture guarantees the newest event is at neither extreme).
	m.sortCol, m.sortDesc = colDuration, true
	m.rebuildEventsTable()
	if got := keyOf(m.selectedEvent()); got != before {
		t.Errorf("turning a sort on moved the cursor off the operator's event:\n got  %+v\n want %+v",
			got, before)
	}
	assertSelectionVisible(t, m.eventsTbl, "sort applied while following the tail")

	// And a further streamed event must not drag it either.
	incoming := sortCursorFixture(42)[41]
	incoming.Duration = time.Hour // sorts to row 0 descending, shifting every row down
	incoming.RequestID = "req-incoming"
	m.events["s"] = append(m.events["s"], incoming)
	m.rebuildEventsTable()
	if got := keyOf(m.selectedEvent()); got != before {
		t.Errorf("a streamed event moved the cursor off its event while sorted:\n got  %+v\n want %+v",
			got, before)
	}
	assertSelectionVisible(t, m.eventsTbl, "streamed event while sorted")
}

// The selectedEventKey pin is order-agnostic — findByKey searches by identity — so
// turning a sort on, flipping it, and turning it off must all leave the cursor on
// the same event, wherever that event now sits.
func TestEventsTable_SortKeepsCursorOnItsEvent(t *testing.T) {
	m := sortCursorModel(t, 40)
	setCursorVisible(&m.eventsTbl, 17)
	m.selectedEventKey = keyOf(m.selectedEvent())
	want := m.selectedEventKey
	if want == (eventKey{}) {
		t.Fatal("no event under the cursor")
	}

	for _, step := range []struct {
		name string
		col  eventColumnID
		desc bool
	}{
		{"sort desc", colDuration, true},
		{"flip to asc", colDuration, false},
		{"sort by host", colHost, false},
		{"back to chronological", "", false},
	} {
		m.sortCol, m.sortDesc = step.col, step.desc
		m.rebuildEventsTable()
		if got := keyOf(m.selectedEvent()); got != want {
			t.Errorf("%s: cursor landed on a different event", step.name)
		}
		assertSelectionVisible(t, m.eventsTbl, step.name)
	}
}

// A sort restored from the config file is active on the very first rebuild, with no
// keypress behind it and nothing on screen announcing it. Suppressing tail-follow
// removes the only restore arm that fires with no pin, so without a seeded pin the
// cursor falls back to a positional index while arriving events reshuffle every row —
// landing on a different event each time.
func TestEventsTable_PersistedSortPinsWithoutAKeypress(t *testing.T) {
	m := sortCursorModel(t, 12)
	// Discard the state sortCursorModel's own rebuild established, and start over the
	// way a fresh process does: a sort from the config file, nothing pinned.
	m.sortCol, m.sortDesc = colDuration, true
	m.selectedEventKey = eventKey{}
	m.rebuildEventsTable()

	if m.selectedEventKey == (eventKey{}) {
		t.Fatal("an active sort left the pin unseeded, so restore has nothing to follow")
	}
	want := keyOf(m.selectedEvent())
	if want == (eventKey{}) {
		t.Fatal("no event under the cursor after the first sorted rebuild")
	}

	// Events arrive, each slower than everything before it — so descending puts each
	// at row 0 and pushes every existing row down.
	for i := 1; i <= 3; i++ {
		next := sortCursorFixture(12 + i)[11+i]
		next.Duration = time.Duration(i) * time.Hour
		next.RequestID = fmt.Sprintf("req-incoming-%d", i)
		m.events["s"] = append(m.events["s"], next)
		m.rebuildEventsTable()
		if got := keyOf(m.selectedEvent()); got != want {
			t.Fatalf("event %d: cursor drifted off its event with no keypress:\n got  %+v\n want %+v",
				i, got, want)
		}
		assertSelectionVisible(t, m.eventsTbl, fmt.Sprintf("after event %d", i))
	}
}

// The seed must not overwrite a real pin, including one the eviction branch
// deliberately cleared to signal "the pinned event is gone".
func TestEventsTable_SortSeedDoesNotClobberAnExistingPin(t *testing.T) {
	m := sortCursorModel(t, 12)
	setCursorVisible(&m.eventsTbl, 5)
	m.selectedEventKey = keyOf(m.selectedEvent())
	pinned := m.selectedEventKey

	m.sortCol, m.sortDesc = colDuration, true
	m.rebuildEventsTable()
	if m.selectedEventKey != pinned {
		t.Errorf("turning a sort on replaced the operator's pin:\n got  %+v\n want %+v",
			m.selectedEventKey, pinned)
	}
	if got := keyOf(m.selectedEvent()); got != pinned {
		t.Errorf("cursor is not on the pinned event after sorting")
	}
}
