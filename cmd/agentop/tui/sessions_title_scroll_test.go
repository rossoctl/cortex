package tui

import (
	"strings"
	"testing"
)

func TestScrollWindow_HoldsAtTheHeadThenReachesTheTail(t *testing.T) {
	const title = "abcdefghij" // 10 columns into 6: offsets 0..4
	if got := scrollWindow(title, 6, 0); got != "abcde…" {
		t.Errorf("step 0 = %q, want the head", got)
	}
	if got := scrollWindow(title, 6, titleScrollHold+2); got != "cdefg…" {
		t.Errorf("two steps after the hold = %q, want offset 2", got)
	}
	if got := scrollWindow(title, 6, titleScrollHold+4); got != "efghij" {
		t.Errorf("at the end = %q, want the tail with no ellipsis", got)
	}
	if got := scrollWindow(title, 6, 4+2*titleScrollHold); got != "abcde…" {
		t.Errorf("after one cycle = %q, want the head again", got)
	}
}

func TestTitleScroll_ScrollsTheSelectedRowAndStopsOffThePane(t *testing.T) {
	const long = "Fix the flaky reload test in the session archive"
	m := newTitleModel(t, map[string]SessionMetadata{"s1": {Title: long}}, "s1")
	m.width = tableWidth(sessionsColumns()) // TITLE at its floor, so the title does not fit
	m.rebuildSessionsTable()
	static := sessionsCell(t, m, titleRow(t, m, "s1"), "TITLE")

	if m.armTitleScroll() == nil {
		t.Fatal("no tick armed for a title that does not fit")
	}
	if m.armTitleScroll() != nil {
		t.Fatal("a second chain armed while one runs")
	}
	// Past the hold at the head, so that the scrolled cell is not the static cell. At the head the
	// two are the same, and the rebuild and off-pane checks below could not fail.
	for range titleScrollHold + 2 {
		if m.handleTitleScroll(titleScrollMsg{gen: m.titleScroll.gen}) == nil {
			t.Fatal("the chain stopped on the selected row")
		}
	}
	got := sessionsCell(t, m, titleRow(t, m, "s1"), "TITLE")
	if got == static || !strings.HasPrefix(got, "x the") {
		t.Errorf("TITLE two steps after the hold = %q, want the title from offset 2", got)
	}
	m.rebuildSessionsTable()
	if again := sessionsCell(t, m, titleRow(t, m, "s1"), "TITLE"); again != got {
		t.Errorf("a rebuild put back %q, want the scrolled %q", again, got)
	}
	if m.handleTitleScroll(titleScrollMsg{gen: m.titleScroll.gen - 1}) != nil {
		t.Error("a tick from an earlier chain was kept")
	}

	m.pane = paneEvents
	m.armTitleScroll()
	if back := sessionsCell(t, m, titleRow(t, m, "s1"), "TITLE"); back != static {
		t.Errorf("TITLE off the pane = %q, want the static %q", back, static)
	}
	if m.handleTitleScroll(titleScrollMsg{gen: m.titleScroll.gen}) != nil {
		t.Error("the chain kept running off the pane")
	}
}

// The static cell of a path keeps the tail, the part that identifies the session. A scroll would
// show the head, which all sessions share, so a path does not scroll.
func TestTitleScroll_LeavesAPathTitleAlone(t *testing.T) {
	const long = "/Users/someone/src/cortex/.worktrees/claudesessions/authbridge"
	m := newTitleModel(t, map[string]SessionMetadata{"s1": {Title: long}}, "s1")
	m.width = tableWidth(sessionsColumns())
	m.rebuildSessionsTable()
	if m.armTitleScroll() != nil {
		t.Error("a tick armed for a path title")
	}
}

// At TITLE's floor the full "(not recorded) " mark is wider than the column, so the static cell
// has the short mark. The scroll must use the same mark, and must still scroll.
func TestTitleScroll_UsesTheShortMarkOfTheStaticCell(t *testing.T) {
	const long = "Fix the flaky reload test in the session archive"
	m := newTitleModel(t, map[string]SessionMetadata{"s1": {Title: long}}, "s1")
	m.sessions[0].NoRecord = true
	m.width = tableWidth(sessionsColumns())
	m.rebuildSessionsTable()
	if m.armTitleScroll() == nil {
		t.Fatal("no tick armed for a not-recorded row at TITLE's floor")
	}
	m.handleTitleScroll(titleScrollMsg{gen: m.titleScroll.gen})
	got := sessionsCell(t, m, titleRow(t, m, "s1"), "TITLE")
	if !strings.HasPrefix(got, noRecordIDMarker+"Fix") {
		t.Errorf("TITLE after a tick = %q, want the short mark %q and the head", got, noRecordIDMarker)
	}
}
