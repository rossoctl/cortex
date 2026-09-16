package tui

import (
	"strings"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/session"
	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// costCellOfFirstRow returns the first sessions row's cost cell.
//
// The column is found by PREFIX rather than by an exact title, because the header is not
// a stable string — it carries the strip's window ("COST/1h") so a reader can tell which
// span the figure covers, and that suffix is a display decision owned by
// sessions_pane.go. Matching the prefix keeps this test about layout() rather than about
// another file's header text.
func costCellOfFirstRow(t *testing.T, m *model) string {
	t.Helper()
	rows := m.sessionsTbl.Rows()
	if len(rows) == 0 {
		t.Fatal("the sessions table has no rows")
	}
	for i, c := range m.sessionsTbl.Columns() {
		if !strings.HasPrefix(c.Title, "COST") {
			continue
		}
		if i >= len(rows[0]) {
			t.Fatalf("the cost column is at index %d but the row has %d cells: %v",
				i, len(rows[0]), rows[0])
		}
		return rows[0][i]
	}
	t.Fatalf("no COST column in the sessions table (columns: %v)", m.sessionsTbl.Columns())
	return ""
}

// TestLayout_ResizeRebuildsTheSessionsRows closes the seam
// fittedSessionsColumnWidth's doc names and leaves to this call site.
//
// The cost cell is width-aware: fitCostCell renders a lone ellipsis rather than let
// bubbles truncate a dollar figure into a smaller-looking number, and it decides that
// against the width the column was actually FITTED to. That width is read at ROW-BUILD
// time, while layout() re-fits the COLUMNS on every WindowSizeMsg — so with no rebuild
// from layout(), the rows kept the formatting chosen for the previous width until the
// next sessionsLoadedMsg or streamed event happened to repaint them. For that interval a
// terminal just narrowed showed one clipped figure and one just widened a needless
// ellipsis.
//
// The assertion that pins the call is the ABSENCE of a load message: nothing between the
// layout() calls delivers new session data, so only layout() can have reformatted the
// cell.
func TestLayout_ResizeRebuildsTheSessionsRows(t *testing.T) {
	// $1234.5678 is 10 columns, the full declared cost width — which the table only gets
	// from a 70-column terminal up. fitTableColumns shrinks it well below that at 40, so
	// the same figure must elide there. Micros rather than a float literal so the expected
	// string comes from the same value the row builder formats.
	const micros = 1_234_567_800
	whole := formatUSDCell(float64(micros) / 1e6)

	m := &model{width: 96, height: 40}
	m.sessionsTbl = newSessionsTable()
	m.sessions = []session.SessionSummary{{ID: "sess-a", EventCount: 1}}
	m.spend.snap = &usage.Snapshot{
		Window: "1h", Priced: true,
		Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
			"sess-a": {Requests: 1, CostMicros: micros, PricedRequests: 1, PriceableRequests: 1},
		}}},
	}

	// Wide: the figure fits and is stated in full. layout() alone must produce the row —
	// there is no rebuildSessionsTable() call here, which is the other half of what is
	// being pinned.
	m.layout()
	if len(m.sessionsTbl.Rows()) == 0 {
		t.Fatal("layout() built no sessions rows at all; it re-fits the columns without " +
			"rebuilding the rows, so the width-aware cost cell is never recomputed on a resize")
	}
	if got := costCellOfFirstRow(t, m); got != whole {
		t.Fatalf("at 96 columns the cost cell is %q, want the whole figure %q; test premise is wrong",
			got, whole)
	}

	// Narrow, with NO load message in between.
	m.width = 40
	m.layout()
	if got := costCellOfFirstRow(t, m); got != costElision {
		t.Errorf("after narrowing to 40 columns the cost cell is %q, want %q — the row still "+
			"carries the formatting chosen for the previous width, and bubbles will truncate "+
			"it into a figure an order of magnitude smaller",
			got, costElision)
	}

	// And back out again, still with no load message: widening must restore the figure,
	// not leave a needless ellipsis where the whole amount now fits.
	m.width = 96
	m.layout()
	if got := costCellOfFirstRow(t, m); got != whole {
		t.Errorf("after widening back to 96 columns the cost cell is %q, want the whole figure %q",
			got, whole)
	}
}

// TestLayout_IsSafeBeforeTheFirstSessionFetch: layout() runs on the first
// WindowSizeMsg, which arrives before any session data. The rebuild it now performs has
// to tolerate that rather than needing the fetch reordered around it.
func TestLayout_IsSafeBeforeTheFirstSessionFetch(t *testing.T) {
	m := &model{width: 96, height: 40}
	m.sessionsTbl = newSessionsTable()
	m.layout()
	if n := len(m.sessionsTbl.Rows()); n != 0 {
		t.Errorf("layout() invented %d rows before any session was fetched", n)
	}
}
