package tui

import (
	"strings"
	"testing"

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

// The session's header says it is not recorded, in full, also on a narrow terminal, where the
// list shows only the short mark; a recorded session's header does not.
func TestSessionHeader_SaysNotRecorded(t *testing.T) {
	for _, width := range []int{200, 60} {
		m := fitModel(t, paneSessions, width, 40, nil)
		m.sessions = []session.SessionSummary{
			{ID: "off-1", Title: "fix the build", NoRecord: true},
			{ID: "live-1", Title: "fix the build"},
		}
		if got := m.sessionHeader("off-1", ""); !strings.Contains(got, "not recorded") {
			t.Errorf("width %d: header = %q, want it to say not recorded", width, got)
		}
		if got := m.sessionHeader("live-1", ""); strings.Contains(got, "not recorded") {
			t.Errorf("width %d: recorded session's header = %q, says not recorded", width, got)
		}
	}
}
