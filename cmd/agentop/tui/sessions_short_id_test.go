package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/rossoctl/cortex/core/session"
)

// SESSION shows the last 8 characters of an id, with no "…". default is shorter and stays whole.
func TestSessionIDCell_EightChars(t *testing.T) {
	for _, w := range []int{8, 14, noLimit} {
		if got := sessionIDCell("f0a50e1f-1234-4abc-8def-0123456789ab", w); got != "456789ab" {
			t.Errorf("width %d: sessionIDCell = %q, want %q", w, got, "456789ab")
		}
	}
	if got := sessionIDCell(session.DefaultSessionID, 8); got != "default" {
		t.Errorf("sessionIDCell(default) = %q, want %q", got, "default")
	}
}

// Ids that start with a timestamp share their head with every session started near them: a
// Codex UUIDv7 for about 65 s, an OpenCode ses_ id for about 17 minutes. Their cells differ.
func TestSessionIDCell_TimeOrderedIDsDiffer(t *testing.T) {
	for _, pair := range [][2]string{
		{"019a8b2c-1d3e-7f40-8a1b-2c3d4e5f6a7b", "019a8b2c-4f10-7c22-9d3e-8a7b6c5d4e3f"}, // UUIDv7, 1 s apart
		{"ses_f00ff1689ffeGD4X9nXaHggqh1", "ses_f00ff3a01c22kP7wQm2Rt5Ve8Z"},             // OpenCode
	} {
		a, b := sessionIDCell(pair[0], noLimit), sessionIDCell(pair[1], noLimit)
		if a == b {
			t.Errorf("%q and %q both show %q", pair[0], pair[1], a)
		}
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
// showing, the layout the issue measured. 140 is the TABLE's width; the terminal is
// boxSideCols wider, for the body box.
func TestSessionsTable_TitleGetsFreedColumns(t *testing.T) {
	m := sessionsServerModel(routerRaw)
	m.width = 140 + boxSideCols
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
func TestSessionsTable_SearchAndSelectUseTheFullID(t *testing.T) {
	m := uuidPicker(t, 140, uuidOpened, uuidOther)
	if got := m.sessionsTbl.Rows()[rowOf(t, m, uuidOpened)][0]; got != uuidOpened[28:] {
		t.Fatalf("SESSION cell = %q, want %q", got, uuidOpened[28:])
	}
	m.search = map[paneID]string{paneSessions: uuidOpened[:8]}
	m.rebuildSessionsTable()
	if got := m.sessionsTbl.Matches(); len(got) != 1 || got[0] != rowOf(t, m, uuidOpened) {
		t.Errorf("search for the id's head matched rows %v, want only row %d", got, rowOf(t, m, uuidOpened))
	}
	m.sessionsTbl.SetCursor(rowOf(t, m, uuidOpened))
	if got := m.selectedSessionID(); got != uuidOpened {
		t.Errorf("selectedSessionID = %q, want the full id %q", got, uuidOpened)
	}
}

// A pending bucket widens SESSION, and the width gates count that: from 60 columns up, the
// bucket's agent name is not cut, and TITLE is not shown below its floor.
func TestSessionsTable_PendingBucketWidthGates(t *testing.T) {
	for w := 60; w <= 120; w++ {
		m := uuidPicker(t, w, uuidOpened, "pending:claude-code")
		cols := m.sessionsTbl.Columns()
		if got := m.sessionsTbl.Rows()[rowOf(t, m, "pending:claude-code")][0]; got != "claude-code" {
			t.Errorf("width %d: bucket SESSION cell = %q, want %q", w, got, "claude-code")
		}
		if tw := sessionsColumnWidth(cols, "TITLE"); tw != 0 && tw < sessionsTitleWidth {
			t.Errorf("width %d: TITLE is %d, below its floor %d", w, tw, sessionsTitleWidth)
		}
		if got := tableWidth(cols); got > w {
			t.Errorf("width %d: table is %d wide", w, got)
		}
	}
}

// On a terminal narrower than the bucket's extra width, the gates still see a width: the
// bucket does not bring back the columns that width drops.
func TestSessionsTable_PendingBucketOnATinyTerminal(t *testing.T) {
	for _, w := range []int{1, 2, 3} {
		plain := len(uuidPicker(t, w, uuidOpened).sessionsTbl.Columns())
		bucket := len(uuidPicker(t, w, uuidOpened, "pending:claude-code").sessionsTbl.Columns())
		if bucket != plain {
			t.Errorf("width %d: %d columns with a bucket, %d without", w, bucket, plain)
		}
	}
}
