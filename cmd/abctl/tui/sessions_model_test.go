package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/table"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// The MODEL column carries the first model the server folded for the session, straight off
// SessionSummary. Three cases live here: the populated cell, the em dash for a session the
// summary carries no model for, and the em dash on a cached-only row — where abctl holds the
// events but the summary is gone, and the model is deliberately NOT re-derived from the cache
// (see sessionModelCell for why: first-in-cache is not first-in-session).
func TestSessionsPane_ModelCell(t *testing.T) {
	now := time.Now()
	m := &model{pane: paneSessions, width: 200, height: 40}
	m.sessionsTbl = newSessionsTable()
	m.sessions = []session.SessionSummary{
		{ID: "with-model", UpdatedAt: now, Model: "claude-opus-5"},
		{ID: "no-model", UpdatedAt: now},
	}
	m.events = map[string][]pipeline.SessionEvent{
		// Events for a session the server no longer lists: the cached-only row. They carry a
		// model, so the em dash below is asserting a choice rather than an empty cache.
		"cached-only": {{Inference: &pipeline.InferenceExtension{Model: "claude-sonnet-5"}}},
	}
	m.rebuildSessionsTable()

	if got := sessionsCell(t, m, titleRow(t, m, "with-model"), "MODEL"); got != "claude-opus-5" {
		t.Errorf("MODEL = %q, want %q", got, "claude-opus-5")
	}
	if got := sessionsCell(t, m, titleRow(t, m, "no-model"), "MODEL"); got != emptyCell {
		t.Errorf("MODEL for an unknown model = %q, want %q — an unknown value must not "+
			"render as a real one", got, emptyCell)
	}
	row := titleRow(t, m, "cached-only")
	if got := sessionsCell(t, m, row, "MODEL"); got != emptyCell {
		t.Errorf("MODEL on a cached-only row = %q, want %q — the cache is not a stand-in for "+
			"the fold the server did", got, emptyCell)
	}
	// Arity, alongside the value: a missing cell anywhere in the row shows up here as one
	// column short, which is the misalignment the header/row contract exists to prevent.
	if got := len(row); got != len(m.sessionsTbl.Columns()) {
		t.Fatalf("cached-only row has %d cells for %d columns: %v", got, len(m.sessionsTbl.Columns()), row)
	}
}

// A long model id is truncated to the column's FITTED width, not left whole for bubbles to cut.
// The prefix problem is the reason the column exists at 18 rather than 14: dated provider ids
// share everything up to the date, and modelColWidth's doc measures the two real prefixes.
func TestSessionsPane_ModelCellTruncatesAtTheFittedWidth(t *testing.T) {
	const dated = "claude-sonnet-4-5-20250929"
	now := time.Now()
	m := &model{pane: paneSessions, width: 200, height: 40}
	m.sessionsTbl = newSessionsTable()
	m.sessions = []session.SessionSummary{{ID: "s1", UpdatedAt: now, Model: dated}}
	m.events = map[string][]pipeline.SessionEvent{}
	// AT THE DECLARED TOTAL, where every column sits at its declared width with no slack —
	// at 200 the table is narrower than the terminal and nothing is squeezed, so this asserts
	// truncation against modelColWidth itself.
	m.width = tableWidth(sessionsColumns())
	m.rebuildSessionsTable()

	row := titleRow(t, m, "s1")
	cell := sessionsCell(t, m, row, "MODEL")
	if got := len([]rune(cell)); got > modelColWidth {
		t.Errorf("MODEL = %q is %d runes in a %d-wide column", cell, got, modelColWidth)
	}
	if !strings.HasPrefix(cell, "claude-sonnet") {
		t.Errorf("MODEL = %q lost the identifying prefix", cell)
	}
	if !strings.HasSuffix(cell, "…") {
		t.Errorf("MODEL = %q has no truncation marker — the cut front is not marked", cell)
	}
}

// MODEL arrives LAST and never leaves: below 73 the table is as it was before the column
// existed, at 97 the money columns return ahead of it, and at 113 — the whole set together —
// it is granted. The property under pin is PRESENCE MONOTONICITY: a reader widening the
// terminal never sees a column vanish, which is what an earlier revision of the gate broke by
// measuring MODEL against the set admitted at that width (it rendered at 89, vanished at 97
// when COST and SAVED returned, came back at 113).
func TestSessionsShowModel_ArrivesLastAndNeverLeaves(t *testing.T) {
	// The two arrivals below MODEL are pinned by their own tests; named here so this test's
	// premise is asserted rather than assumed.
	if hasColumn(sessionsColumnsFor(72), "TITLE") {
		t.Error("premise failed: TITLE renders below 73 — this test's expectations are stale")
	}
	if !hasColumn(sessionsColumnsFor(96), "TITLE") || hasColumn(sessionsColumnsFor(96), "COST") {
		t.Error("premise failed: the TITLE-without-money band moved — see " +
			"TestSessionsShowTitle_RendersAtTheCommonWidth")
	}
	if hasColumn(sessionsColumnsFor(112), "MODEL") {
		t.Error("MODEL is seated below the width that holds every column's floor at once")
	}
	if !hasColumn(sessionsColumnsFor(113), "MODEL") {
		t.Error("MODEL is not seated at 113 — the threshold moved; update this test and the " +
			"README's column-order prose together")
	}
	// AND IT HOLDS ITS FLOOR once seated, which is what distinguishes a granted column from an
	// admitted one: the fitter narrows a freely-admitted MODEL to its global floor of four,
	// where two real model ids render identically.
	for w := 113; w <= 200; w++ {
		if got := sessionsColumnWidth(sessionsColumnsFor(w), "MODEL"); got < sessionsModelCellMin {
			t.Fatalf("width %d: MODEL fitted to %d, under its %d floor", w, got, sessionsModelCellMin)
		}
	}
	// NEVER LEAVES once granted: the presence check across every width above the threshold.
	for w := 113; w <= 200; w++ {
		if !hasColumn(sessionsColumnsFor(w), "MODEL") {
			t.Fatalf("width %d: MODEL vanished — a column that leaves as the terminal widens "+
				"reads as a fault, not a budget", w)
		}
	}
	// AND THE ROW CARRIES IT: the header alone is not the column. At a width past the
	// threshold the model cell is present on a server-listed row, which is the arity contract
	// TestSessionsPicker_RowArityMatchesTheHeaderAtEveryWidth states generally.
	m := &model{pane: paneSessions, width: 120, height: 40}
	m.sessionsTbl = newSessionsTable()
	m.sessions = []session.SessionSummary{{ID: "s1", UpdatedAt: time.Now(), Model: "claude-opus-5"}}
	m.events = map[string][]pipeline.SessionEvent{}
	m.rebuildSessionsTable()
	if !hasColumn(m.sessionsTbl.Columns(), "MODEL") {
		t.Fatal("no MODEL column in the live table at width 120")
	}
	if got := sessionsCell(t, m, titleRow(t, m, "s1"), "MODEL"); got != "claude-opus-5" {
		t.Errorf("MODEL = %q at width 120, want the model", got)
	}
}

// Below MODEL's threshold the table's COLUMN SET is the pre-MODEL one — the property the
// gate's design rests on. Every width-pinned test in this package passed unmodified when the
// column was added; this pins it directly rather than leaving it to that accident.
//
// PRESENCE, NOT WIDTH, and the distinction is deliberate: growSessionsTitle reserves MODEL's
// room below the threshold (exactly as it reserves the money columns'), so a TITLE growing
// into slack grows ~20 columns less than it did before MODEL existed. That is the price of
// keeping TITLE's width monotonic across MODEL's arrival — without the reservation, TITLE
// ballooned below 113 and collapsed to its floor at 113, which is the failure
// TestSessionsTitle_WidthNeverShrinksAsTheTerminalGrows exists to catch. So the columns
// present below 113 are the pre-MODEL set, while TITLE's width among them is bounded by
// MODEL's future claim.
func TestSessionsShowModel_NarrowTableUnchanged(t *testing.T) {
	for w := 1; w < 113; w++ {
		cols := sessionsColumnsFor(w)
		if hasColumn(cols, "MODEL") {
			t.Fatalf("width %d: MODEL is seated below its threshold", w)
		}
		// The pre-MODEL set, derived through the same gates the pre-MODEL code ran: both
		// sessionsShowTitle and sessionsShowMoney exclude MODEL from what they measure, so on
		// a MODEL-less column list they answer exactly what they answered before the column
		// existed. Comparing against that (rather than golden literals) keeps this true by
		// construction when another column's declared width changes on its own merit.
		want := make([]table.Column, 0, len(sessionsColumns()))
		for _, c := range sessionsColumns() {
			if headerTitle(c) != "MODEL" {
				want = append(want, c)
			}
		}
		if !sessionsShowTitle(w) {
			want = want[:0]
			for _, c := range sessionsColumns() {
				if headerTitle(c) != "MODEL" && headerTitle(c) != "TITLE" {
					want = append(want, c)
				}
			}
		}
		if !sessionsShowMoney(w) {
			filtered := want[:0]
			for _, c := range want {
				if t := headerTitle(c); t != "COST" && t != "SAVED" {
					filtered = append(filtered, c)
				}
			}
			want = filtered
		}
		if got, wantTitles := titles(cols), titles(want); !equalStrings(got, wantTitles) {
			t.Fatalf("width %d: columns %v, want the pre-MODEL %v — the narrow table's column "+
				"set moved", w, got, wantTitles)
		}
	}
}

// equalStrings is slices.Equal for strings, spelled out because the tui package's Go version
// predates generics in this codebase's style — every other test here compares by loop or by
// fmt. Joined rather than compared element-wise so the failure message shows both whole sets.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
