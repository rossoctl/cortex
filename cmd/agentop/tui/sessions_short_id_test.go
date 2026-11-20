package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/rossoctl/cortex/core/session"
)

// SESSION shows the first 8 characters of an id (#2019, change 8), with no "…". default is
// shorter and stays whole.
func TestSessionIDCell_EightChars(t *testing.T) {
	for _, w := range []int{8, 14, noLimit} {
		if got := sessionIDCell("f0a50e1f-1234-4abc-8def-0123456789ab", w); got != "f0a50e1f" {
			t.Errorf("width %d: sessionIDCell = %q, want %q", w, got, "f0a50e1f")
		}
	}
	if got := sessionIDCell(session.DefaultSessionID, 8); got != "default" {
		t.Errorf("sessionIDCell(default) = %q, want %q", got, "default")
	}
}

// A pending bucket's agent name is not cut: SESSION widens to the widest cell, ids counted at 8.
func TestSessionsTable_PendingBucketFullAgentName(t *testing.T) {
	m := uuidPicker(t, 160, uuidOpened, "pending:claude-code")
	if got := m.sessionsTbl.Rows()[rowOf(t, m, "pending:claude-code")][0]; got != "claude-code" {
		t.Errorf("bucket SESSION cell = %q, want %q", got, "claude-code")
	}
	if got := sessionsColumnWidth(m.sessionsTbl.Columns(), "SESSION"); got != 11 {
		t.Errorf("SESSION width = %d, want 11", got)
	}
	// With only ids listed, the column is 8.
	m = uuidPicker(t, 160, uuidOpened, uuidOther)
	if got := sessionsColumnWidth(m.sessionsTbl.Columns(), "SESSION"); got != 8 {
		t.Errorf("ids-only SESSION width = %d, want 8", got)
	}
}

// The columns SESSION gave back go to TITLE: 24 -> 30 at 140 columns with AGENT (and SERVER)
// showing, the layout the issue measured.
func TestSessionsTable_TitleGetsFreedColumns(t *testing.T) {
	m := sessionsServerModel(routerRaw)
	m.width = 140
	m.rebuildSessionsTable()
	cols := m.sessionsTbl.Columns()
	if sessionsColumnWidth(cols, "AGENT") == 0 || sessionsColumnWidth(cols, "SERVER") == 0 {
		t.Fatalf("want AGENT and SERVER at 140 columns: %v", titles(cols))
	}
	if got := sessionsColumnWidth(cols, "TITLE"); got != 30 {
		t.Errorf("TITLE width = %d, want 30", got)
	}
	if got := tableWidth(cols); got > 140 {
		t.Errorf("table is %d wide, over the 140-column terminal", got)
	}
}

// The default bucket is background: every cell of its row draws muted (an em dash stays dim).
func TestSessionsTable_DefaultRowMuted(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	m := uuidPicker(t, 140, uuidOpened, session.DefaultSessionID)
	r := rowOf(t, m, session.DefaultSessionID)
	if r == m.sessionsTbl.Cursor() {
		t.Fatalf("default row is the cursor row")
	}
	rows := m.sessionsTbl.Rows()
	lines, first := tableLines(t, m.sessionsTbl, strings.TrimSpace(rows[0][0]))
	line := lines[first+r]
	if !strings.Contains(ansi.Strip(line), "default") {
		t.Fatalf("line %d is not the default row: %q", first+r, ansi.Strip(line))
	}
	for i, cell := range rows[r] {
		c := strings.TrimSpace(cell)
		if c == "" {
			continue
		}
		want := fgOf(styleMuted)
		if c == "—" {
			want = fgOf(styleDim)
		}
		if p := sgrAt(line, c); !strings.Contains(p, want) || boldIn(p) {
			t.Errorf("cell %d %q has SGR %q, want %q and not bold", i, c, p, want)
		}
	}
}

// The cell is short, but search and the row's id (what drill and the header use) are the full id.
func TestSessionsTable_SearchAndYankUseTheFullID(t *testing.T) {
	m := uuidPicker(t, 140, uuidOpened, uuidOther)
	if got := m.sessionsTbl.Rows()[rowOf(t, m, uuidOpened)][0]; got != uuidOpened[:8] {
		t.Fatalf("SESSION cell = %q, want %q", got, uuidOpened[:8])
	}
	m.search = map[paneID]string{paneSessions: uuidOpened[24:]}
	m.rebuildSessionsTable()
	if got := m.sessionsTbl.Matches(); len(got) != 1 || got[0] != rowOf(t, m, uuidOpened) {
		t.Errorf("search for the id's tail matched rows %v, want only row %d", got, rowOf(t, m, uuidOpened))
	}
	m.sessionsTbl.SetCursor(rowOf(t, m, uuidOpened))
	if got := m.selectedSessionID(); got != uuidOpened {
		t.Errorf("selectedSessionID = %q, want the full id %q", got, uuidOpened)
	}
}
