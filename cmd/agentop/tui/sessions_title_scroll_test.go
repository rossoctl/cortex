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
	const long = "/Users/someone/src/cortex/.worktrees/claudesessions/authbridge"
	m := newTitleModel(t, map[string]SessionMetadata{"s1": {Title: long}}, "s1")
	m.width = tableWidth(sessionsColumns()) // TITLE at its floor, so the path does not fit
	m.rebuildSessionsTable()
	static := sessionsCell(t, m, titleRow(t, m, "s1"), "TITLE")

	if m.armTitleScroll() == nil {
		t.Fatal("no tick armed for a title that does not fit")
	}
	if m.armTitleScroll() != nil {
		t.Fatal("a second chain armed while one runs")
	}
	if m.handleTitleScroll(titleScrollMsg{gen: m.titleScroll.gen}) == nil {
		t.Fatal("the chain stopped on the selected row")
	}
	got := sessionsCell(t, m, titleRow(t, m, "s1"), "TITLE")
	if !strings.HasPrefix(got, "/Users") {
		t.Errorf("TITLE after a tick = %q, want the head of the path", got)
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
