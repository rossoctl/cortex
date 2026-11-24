package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/core/session"
)

// A session that asked not to be recorded (#1923) has "(not recorded) " in front of its title, and
// it wins over the imported and child marks. A recorded session has no mark.
func TestSessionTitleCell_NoRecordPrefix(t *testing.T) {
	m := fitModel(t, paneSessions, 200, 40, nil)
	m.sessions = []session.SessionSummary{
		{ID: "off-1", Title: "fix the build", NoRecord: true},
		{ID: "off-2", Title: "fix the build", NoRecord: true, Source: importedSrc, ParentSession: "parent-123456789"},
		{ID: "live-1", Title: "fix the build"},
	}
	m.rebuildSessionsTable()
	rows := m.sessionsTbl.Rows()
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	title := func(id string) string {
		for _, r := range rows {
			if strings.Contains(strings.Join(r, "|"), id) {
				for _, c := range r {
					if strings.Contains(c, "fix the build") {
						return c
					}
				}
			}
		}
		t.Fatalf("no row for %s in %q", id, rows)
		return ""
	}
	for _, id := range []string{"off-1", "off-2"} {
		if got := title(id); !strings.HasPrefix(got, noRecordMarker+"fix the build") {
			t.Errorf("%s title = %q, want the prefix %q", id, got, noRecordMarker)
		}
	}
	if got := title("live-1"); strings.Contains(got, noRecordMarker) {
		t.Errorf("recorded title = %q, has the not-recorded mark", got)
	}
}

// With no TITLE column, the SESSION cell has the mark's short form, and the whole id still shows
// after it.
func TestSessionIDCell_NoRecordPrefixWithoutTitle(t *testing.T) {
	m := fitModel(t, paneSessions, 60, 40, nil)
	m.sessions = []session.SessionSummary{
		{ID: "off-1", Title: "fix the build", NoRecord: true},
		{ID: "live-1", Title: "fix the build"},
	}
	m.rebuildSessionsTable()
	rows := m.sessionsTbl.Rows()
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	for _, r := range rows {
		cell := r[0]
		switch {
		case strings.Contains(cell, "off-1"):
			if !strings.HasPrefix(cell, noRecordIDMarker+"off-1") {
				t.Errorf("not-recorded SESSION cell = %q, want %q then the id", cell, noRecordIDMarker)
			}
		case strings.Contains(cell, "live-1"):
			if strings.Contains(cell, noRecordIDMarker) {
				t.Errorf("recorded SESSION cell = %q, has the mark", cell)
			}
		default:
			t.Errorf("SESSION cell %q names neither session in full", cell)
		}
	}
}

// At TITLE's narrowest width the full mark leaves no room for the title, so the TITLE cell has
// the short mark and still some of the title.
func TestSessionTitleCell_NoRecordAtTheTitleFloor(t *testing.T) {
	const title = "fix the build"
	sessions := []session.SessionSummary{{ID: "off-1", Title: title, NoRecord: true}}
	for width := 40; width <= 200; width++ {
		m := fitModel(t, paneSessions, width, 40, nil)
		m.sessions = sessions
		m.rebuildSessionsTable()
		rows := m.sessionsTbl.Rows()
		if len(rows) != 1 {
			t.Fatalf("width %d: got %d rows, want 1", width, len(rows))
		}
		for _, c := range rows[0] {
			if !strings.Contains(c, "fix") {
				continue
			}
			// The first width that shows TITLE: the column is at its floor.
			if lipgloss.Width(c) > sessionsTitleWidth {
				t.Fatalf("width %d: TITLE cell %q wider than the floor %d", width, c, sessionsTitleWidth)
			}
			if !strings.HasPrefix(c, noRecordIDMarker+"fix") {
				t.Fatalf("width %d: TITLE cell at the floor = %q, want %q and then some of the title", width, c, noRecordIDMarker)
			}
			return
		}
	}
	t.Fatal("no width from 40 to 200 shows the TITLE column")
}

// The session's header says it is not recorded, in full, also on a narrow terminal, where the
// list shows only the short mark and the header has to clip a long title; a recorded session's
// header does not.
func TestSessionHeader_SaysNotRecorded(t *testing.T) {
	long := strings.Repeat("refactor the session archive writer ", 4)
	for _, width := range []int{200, 60} {
		m := fitModel(t, paneSessions, width, 40, nil)
		m.sessions = []session.SessionSummary{
			{ID: "off-1", Title: long, NoRecord: true},
			{ID: "live-1", Title: long},
		}
		if width == 60 && strings.Contains(m.sessionHeader("off-1", ""), strings.TrimSpace(long)) {
			t.Fatalf("precondition: at %d columns the header should clip the title", width)
		}
		if got := m.sessionHeader("off-1", ""); !strings.Contains(got, "not recorded") {
			t.Errorf("width %d: header = %q, want it to say not recorded", width, got)
		}
		if got := m.sessionHeader("live-1", ""); strings.Contains(got, "not recorded") {
			t.Errorf("width %d: recorded session's header = %q, says not recorded", width, got)
		}
	}
}
