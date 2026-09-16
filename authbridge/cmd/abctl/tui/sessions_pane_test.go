package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// costSessionsModel builds a model wide and tall enough that nothing the sessions
// table renders is width- or height-constrained, so a missing cell is a missing
// cell rather than a fitting decision.
func costSessionsModel() *model {
	m := &model{width: 120, height: 40}
	m.sessionsTbl = newSessionsTable()
	return m
}

// sessionsRowCell returns one column's cell from the first row, addressed by the column's
// TITLE PREFIX rather than by an index.
//
// Positional indexing is what makes table.Row dangerous: it is a []string, so
// inserting a column shifts every reader silently instead of failing to compile.
// A test that hardcodes an index would keep passing while asserting about the
// wrong column.
//
// A prefix rather than the whole title because the COST title carries the span its
// figures cover ("COST/1h") and that span follows the server's answer — see
// costColumnTitle. Every call site here passes "COST" and keeps working whatever span the
// fixture's snapshot reports, which is the same reason the production lookup matches by
// prefix.
func sessionsRowCell(t *testing.T, m *model, titlePrefix string) string {
	t.Helper()
	rows := m.sessionsTbl.Rows()
	if len(rows) == 0 {
		t.Fatal("sessions table has no rows")
	}
	for i, c := range m.sessionsTbl.Columns() {
		if !strings.HasPrefix(c.Title, titlePrefix) {
			continue
		}
		if i >= len(rows[0]) {
			t.Fatalf("column %q is at index %d but the row has only %d cells: %v", c.Title, i, len(rows[0]), rows[0])
		}
		return rows[0][i]
	}
	t.Fatalf("no column titled %q* in the sessions table (columns: %v)", titlePrefix, m.sessionsTbl.Columns())
	return ""
}

// A bare "COST" beside a lifetime TOKENS count is two spans on one row with nothing
// saying so: a six-hour live session showed six hours of tokens against one hour of
// dollars, and dividing the two cells fabricated a rate. The header is the only place the
// span can live.
func TestSessionsColumns_CostTitleNamesItsSpan(t *testing.T) {
	title := ""
	for _, c := range sessionsColumns() {
		if strings.HasPrefix(c.Title, costColumnPrefix) {
			title = c.Title
			break
		}
	}
	if title == "" {
		t.Fatalf("no COST column in %v", sessionsColumns())
	}
	if title == costColumnPrefix {
		t.Error("COST title is bare; it promises nothing about the span its figures cover, " +
			"and the TOKENS cell beside it covers a different one")
	}
	// The span the strip actually asks for, so the header cannot drift from the poll
	// window even before a snapshot has answered.
	if want := "COST/" + formatWindowLabel(spendWindow); title != want {
		t.Errorf("COST title = %q, want %q", title, want)
	}
}

// The declared width is pinned arithmetic (sessionsColumns' 102-column note), so the title
// has to fit inside it rather than the other way round.
func TestSessionsColumns_CostTitleFitsTheDeclaredWidth(t *testing.T) {
	for _, c := range sessionsColumns() {
		if !strings.HasPrefix(c.Title, costColumnPrefix) {
			continue
		}
		if got := lipgloss.Width(c.Title); got > c.Width {
			t.Errorf("COST title %q is %d columns in a %d-wide column; bubbles ellipsises the header",
				c.Title, got, c.Width)
		}
		return
	}
	t.Fatal("no COST column found")
}

// The title follows the SNAPSHOT, not the request. The server answers with the span it
// actually served — a proxy with a shorter ring answers a one-hour request with what it
// holds — and a header driven off the constant would name an hour over some other span.
func TestSessionsTable_CostTitleFollowsTheSnapshotsWindow(t *testing.T) {
	m := costSessionsModel()
	m.sessions = []session.SessionSummary{{ID: "sess-a", EventCount: 1}}
	m.spend.snap = &usage.Snapshot{
		Window: "30m", Priced: true,
		Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
			"sess-a": {Requests: 1, CostMicros: 250_000, PricedRequests: 1, PriceableRequests: 1},
		}}},
	}

	m.rebuildSessionsTable()

	title := ""
	for _, c := range m.sessionsTbl.Columns() {
		if strings.HasPrefix(c.Title, costColumnPrefix) {
			title = c.Title
		}
	}
	if title != "COST/30m" {
		t.Errorf("COST title = %q, want %q — the figures were summed over the 30m the server served",
			title, "COST/30m")
	}
	// And the figure still lands in that column, which is what the prefix lookup buys.
	if got := sessionsRowCell(t, m, costColumnPrefix); got != "$0.2500" {
		t.Errorf("COST cell = %q, want %q; the re-titled column lost its cell", got, "$0.2500")
	}
}

func TestSessionsTable_ShowsCostPerSession(t *testing.T) {
	m := costSessionsModel()
	m.sessions = []session.SessionSummary{{ID: "sess-a", EventCount: 3, TotalTokens: 100}}
	m.spend.snap = &usage.Snapshot{
		Window: "6h", Priced: true,
		Buckets: []usage.Bucket{{
			Series: map[string]usage.Counts{
				"sess-a": {Requests: 3, CostMicros: 250_000, PricedRequests: 3, PriceableRequests: 3},
			},
		}},
	}

	m.rebuildSessionsTable()

	row := m.sessionsTbl.Rows()[0]
	joined := strings.Join(row, " ")
	if !strings.Contains(joined, "0.25") {
		t.Errorf("row %v does not show the session's $0.25 cost", row)
	}
	// Named-column check as well as the joined one: the joined assertion would pass
	// if the figure landed in the wrong cell.
	if got := sessionsRowCell(t, m, "COST"); !strings.Contains(got, "0.25") {
		t.Errorf("COST cell = %q, want the session's $0.25", got)
	}
}

func TestSessionsTable_UnpricedSessionShowsNoZero(t *testing.T) {
	// The rule the whole feature rests on: an unknown cost is not a zero one, and
	// a table cell has even less room to explain itself than the strip does.
	m := costSessionsModel()
	m.sessions = []session.SessionSummary{{ID: "sess-a", EventCount: 3, TotalTokens: 100}}
	m.spend.snap = &usage.Snapshot{Window: "6h", Priced: false}

	m.rebuildSessionsTable()

	row := m.sessionsTbl.Rows()[0]
	for _, cell := range row {
		if strings.Contains(cell, "$0.00") || cell == "0.0000" {
			t.Errorf("row %v renders a zero cost for an unpriced session", row)
		}
	}
}

// A snapshot that priced SOME traffic still knows nothing about a session that
// contributed none of it. Snapshot.Priced is a window-wide flag, so trusting it
// alone would price every row off another session's figure — and the row would
// read $0.0000, which is the one thing the feature forbids.
func TestSessionsTable_SessionAbsentFromAPricedWindowShowsNoZero(t *testing.T) {
	m := costSessionsModel()
	m.sessions = []session.SessionSummary{{ID: "quiet", EventCount: 1}}
	m.spend.snap = &usage.Snapshot{
		Window: "1h", Priced: true,
		Buckets: []usage.Bucket{{
			Series: map[string]usage.Counts{
				"busy": {Requests: 9, CostMicros: 900_000, PricedRequests: 9, PriceableRequests: 9},
			},
		}},
	}

	m.rebuildSessionsTable()

	if got := sessionsRowCell(t, m, "COST"); got != "" {
		t.Errorf("COST cell = %q for a session the priced window has no figure for, want blank", got)
	}
}

// A session whose requests were all PRICEABLE but none PRICED has an unknown
// cost, not a zero one — the same distinction the strip's coverage figure exists
// to report, applied per row.
func TestSessionsTable_PriceableButUnpricedSessionShowsNoZero(t *testing.T) {
	m := costSessionsModel()
	m.sessions = []session.SessionSummary{{ID: "sess-a", EventCount: 4}}
	m.spend.snap = &usage.Snapshot{
		Window: "1h", Priced: true,
		Buckets: []usage.Bucket{{
			Series: map[string]usage.Counts{
				"sess-a": {Requests: 4, PriceableRequests: 4},
				"other":  {Requests: 1, CostMicros: 5_000, PricedRequests: 1, PriceableRequests: 1},
			},
		}},
	}

	m.rebuildSessionsTable()

	if got := sessionsRowCell(t, m, "COST"); got != "" {
		t.Errorf("COST cell = %q for a priceable-but-unpriced session, want blank", got)
	}
}

// Cached-only rows are sessions the server no longer lists, so the aggregator's ring
// has been reclaimed too. The COST cell is blank, and the row must still have exactly
// as many cells as there are columns — a short row shifts nothing but renders one
// column of nothing, while a long one panics inside bubbles.
func TestSessionsTable_CachedOnlyRowHasNoCost(t *testing.T) {
	m := costSessionsModel()
	m.events = map[string][]pipeline.SessionEvent{"vanished": make([]pipeline.SessionEvent, 2)}
	m.spend.snap = &usage.Snapshot{
		Window: "1h", Priced: true,
		Buckets: []usage.Bucket{{
			Series: map[string]usage.Counts{
				"vanished": {Requests: 2, CostMicros: 100_000, PricedRequests: 2, PriceableRequests: 2},
			},
		}},
	}

	m.rebuildSessionsTable()

	if got := sessionsRowCell(t, m, "COST"); got != "" {
		t.Errorf("cached-only COST cell = %q, want blank", got)
	}
}

// Every row must carry exactly one cell per column. bubbles indexes m.cols[i] as
// it walks the row, so a row LONGER than the column list panics inside View();
// a shorter one silently renders a blank trailing column. Both live rows —
// server-listed and cached-only — are built in different places, so both need
// pinning or one of them drifts.
func TestSessionsTable_EveryRowHasOneCellPerColumn(t *testing.T) {
	m := costSessionsModel()
	m.sessions = []session.SessionSummary{{ID: "listed", EventCount: 1, CreatedAt: time.Now().Add(-time.Hour), UpdatedAt: time.Now()}}
	m.events = map[string][]pipeline.SessionEvent{"vanished": make([]pipeline.SessionEvent, 2)}

	m.rebuildSessionsTable()

	rows := m.sessionsTbl.Rows()
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want a listed one and a cached-only one", len(rows))
	}
	want := len(m.sessionsTbl.Columns())
	for _, r := range rows {
		if len(r) != want {
			t.Errorf("row %v has %d cells, want %d (one per column)", r, len(r), want)
		}
	}
	// Renders without panicking, which is the failure a long row produces.
	_ = m.sessionsTbl.View()
}

// The cells have to fit the columns they were declared with. A cell wider than
// its column is truncated by bubbles with runewidth.Truncate, which would put an
// ellipsis in the middle of a dollar amount — the one thing #953 rules out.
func TestSessionsTable_CostFitsItsColumn(t *testing.T) {
	// Keyed by the title's PREFIX, not the title: COST carries its span now, and a map
	// keyed on the whole title would look up a missing entry and compare every figure
	// against a width of zero — an assertion that cannot fail is worse than none.
	widths := map[string]int{}
	for _, c := range newSessionsTable().Columns() {
		if strings.HasPrefix(c.Title, costColumnPrefix) {
			widths[costColumnPrefix] = c.Width
			continue
		}
		widths[c.Title] = c.Width
	}
	if widths[costColumnPrefix] == 0 {
		t.Fatal("no COST column in the sessions table")
	}
	// lipgloss.Width, not len: this measures DISPLAY COLUMNS, which is what bubbles
	// compares against. The package's trunc and truncStr helpers are both
	// cell-unaware (one slices runes, the other bytes) and neither may be used for
	// width arithmetic.
	for _, cost := range []float64{0, 0.00001, 0.25, 12.5, 1234.5678, 9999.9999} {
		if got := lipgloss.Width(formatUSDCell(cost)); got > widths["COST"] {
			t.Errorf("formatUSDCell(%v) is %d columns, COST is %d wide", cost, got, widths["COST"])
		}
	}
	// The ceiling, asserted rather than left implicit: five figures of dollars does
	// NOT fit, and bubbles would truncate it into "$10000.00…". Accepted because it
	// means one session spending $10,000 inside the strip's one-hour window, and the
	// two extra columns are paid for on every terminal. If this ever starts failing
	// because the formatter got narrower, widen the column and delete this — a
	// documented ceiling that has silently moved is worse than none.
	if lipgloss.Width(formatUSDCell(10_000)) <= widths["COST"] {
		t.Errorf("formatUSDCell(10000) now fits COST (%d wide); the documented ceiling in newSessionsTable is stale", widths["COST"])
	}
}

// fittedCostWidth returns the width fitTableColumns leaves the COST column at terminal
// width w, failing the test if the column is not in the fitted set at all.
//
// The absence check is the invariant the sessions godoc used to have backwards: the fitter
// SHRINKS the widest column and never drops one, which is what keeps the positional-index
// trap closed (event_retention_test.go asserts row[5]). A fitter that started dropping
// columns would spring it, and a shifted index still compiles and still asserts.
func fittedCostWidth(t *testing.T, w int) int {
	t.Helper()
	fitted := fitTableColumns(sessionsColumns(), w)
	if len(fitted) != len(sessionsColumns()) {
		t.Fatalf("width %d: fitTableColumns returned %d of %d columns; a dropped column shifts every positional row reader",
			w, len(fitted), len(sessionsColumns()))
	}
	for _, c := range fitted {
		// By prefix: the title carries the span its figures cover, so an exact match would
		// stop finding the column the first time that span changed.
		if strings.HasPrefix(c.Title, costColumnPrefix) {
			return c.Width
		}
	}
	t.Fatalf("width %d: no COST column in the fitted set: %v", w, fitted)
	return 0
}

// The FITTER'S OUTPUT, which is what nothing checked.
//
// TestSessionsTable_CostFitsItsColumn above pins formatUSDCell against the 10 columns
// sessionsColumns DECLARES — a width the table only has from a 70-column terminal up.
// fitTableColumns shrinks COST to 5 at 40, 7 at 50 and 8 at 60, and bubbles renders a cell
// as runewidth.Truncate(value, col.Width, "…"), which cuts from the right and keeps the
// leading digits: $1234.5678 in a 5-wide column comes out "$123…", ten times smaller and
// still readable as an amount. That is exactly the "never render a partial number" rule the
// COST column's own comment says #953 forbids, and a constructor-only assertion could not
// see it.
//
// Every cell must therefore be one of exactly two things: the whole figure, or the elision
// marker. Nothing in between, and NOT blank — blank means "unpriced" in this table.
func TestSessionsTable_CostCellIsNeverATruncatedNumber(t *testing.T) {
	// Micros rather than dollars so the expected string is derived from the same value the
	// row builder formats; a float literal round-tripped through 1e6 could disagree in the
	// fourth decimal and make the test about arithmetic instead of about width.
	amounts := []int64{
		30,            // "<$0.0001", the sub-floor form: 8 columns
		120_000,       // "$0.1200": 7 columns, fits from a 50-column terminal up
		12_500_000,    // "$12.5000": 8 columns
		1_234_567_800, // "$1234.5678": 10 columns, needs the full declared width
		9_999_999_900, // "$9999.9999": 10 columns, the documented ceiling
	}
	for _, w := range []int{40, 50, 60, 70, 80, 96} {
		costW := fittedCostWidth(t, w)
		for _, micros := range amounts {
			usd := float64(micros) / 1e6
			whole := formatUSDCell(usd)

			m := &model{width: w, height: 40}
			m.sessionsTbl = newSessionsTable()
			m.sessions = []session.SessionSummary{{ID: "sess-a", EventCount: 1}}
			m.spend.snap = &usage.Snapshot{
				Window: "1h", Priced: true,
				Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
					"sess-a": {Requests: 1, CostMicros: micros, PricedRequests: 1, PriceableRequests: 1},
				}}},
			}
			// layout() is the production path — it is the only caller of
			// SetColumns(fitTableColumns(...)) — so this exercises the real fitted widths
			// rather than a width the test computed for itself.
			m.layout()
			m.rebuildSessionsTable()

			got := sessionsRowCell(t, m, "COST")
			switch got {
			case whole:
				// Stated in full, so it must actually fit or bubbles will clip it anyway.
				if gw := lipgloss.Width(got); gw > costW {
					t.Errorf("term %d, %s: COST cell is %d columns in a %d-wide column; bubbles truncates it to a smaller-looking figure",
						w, whole, gw, costW)
				}
			case costElision:
				// Elided, so the whole figure must genuinely not have fitted — otherwise
				// the fix is hiding a figure it could have shown.
				if lipgloss.Width(whole) <= costW {
					t.Errorf("term %d, %s: COST elided although %d columns fit a %d-wide column",
						w, whole, lipgloss.Width(whole), costW)
				}
			case "":
				t.Errorf("term %d, %s: COST cell is blank, which in this table means UNPRICED; a known cost that will not fit must say %q",
					w, whole, costElision)
			default:
				t.Errorf("term %d, %s: COST cell = %q, want the whole figure or %q — never a partial number",
					w, whole, got, costElision)
			}
		}
	}
}

// The widths the sessions godoc and fitCostCell's comment both quote. Asserted so the
// prose cannot go stale silently the way the "106 columns / drops trailing columns" note
// it replaced did.
func TestFitTableColumns_SessionsCostWidthsAreWhatTheCommentsClaim(t *testing.T) {
	if got := tableWidth(sessionsColumns()); got != 102 {
		t.Errorf("declared sessions table width = %d, want 102 (90 declared + 6 columns × cellPadding)", got)
	}
	for _, tc := range []struct{ term, cost int }{{40, 5}, {50, 7}, {60, 8}, {70, 10}, {80, 10}, {96, 10}} {
		if got := fittedCostWidth(t, tc.term); got != tc.cost {
			t.Errorf("terminal %d: fitted COST width = %d, want %d", tc.term, got, tc.cost)
		}
	}
}

// The COST cell republished a truncated stream's floor as "$0.2546" — a figure exact to
// four decimal places for an amount the aggregator itself reports as a lower bound. The
// cell has no room for a sentence, so it carries the one-column marker every other money
// surface in abctl uses.
func TestSessionsTable_InexactCostCellSaysSo(t *testing.T) {
	m := costSessionsModel()
	m.sessions = []session.SessionSummary{{ID: "sess-a", EventCount: 3, TotalTokens: 100}}
	m.spend.snap = &usage.Snapshot{
		Window: "1h", Priced: true,
		Buckets: []usage.Bucket{{
			Series: map[string]usage.Counts{
				"sess-a": {
					Requests: 3, CostMicros: 254_600,
					PricedRequests: 3, PriceableRequests: 3, IncompleteRequests: 1,
				},
			},
		}},
	}

	m.rebuildSessionsTable()

	got := sessionsRowCell(t, m, "COST")
	if want := inexactMarker + "$0.2546"; got != want {
		t.Errorf("COST cell = %q, want %q — a lower bound stated as an exact figure is the "+
			"claim IncompleteRequests exists to withdraw", got, want)
	}
}

// And an exact figure carries no marker, so the annotation stays worth reading.
func TestSessionsTable_ExactCostCellCarriesNoMarker(t *testing.T) {
	m := costSessionsModel()
	m.sessions = []session.SessionSummary{{ID: "sess-a", EventCount: 3}}
	m.spend.snap = &usage.Snapshot{
		Window: "1h", Priced: true,
		Buckets: []usage.Bucket{{
			Series: map[string]usage.Counts{
				"sess-a": {Requests: 3, CostMicros: 254_600, PricedRequests: 3, PriceableRequests: 3},
			},
		}},
	}

	m.rebuildSessionsTable()

	if got := sessionsRowCell(t, m, "COST"); got != "$0.2546" {
		t.Errorf("COST cell = %q, want the bare %q", got, "$0.2546")
	}
}

// The marker costs a column, so it must obey the same rule the figure does: whole, or
// elided, never a partial number. It cannot be dropped to make a figure fit — that would
// turn a lower bound back into an exact-looking total, which is the defect.
func TestSessionsTable_InexactCostCellIsNeverATruncatedNumber(t *testing.T) {
	amounts := []int64{120_000, 12_500_000, 1_234_567_800}
	for _, w := range []int{40, 50, 60, 70, 80, 96} {
		costW := fittedCostWidth(t, w)
		for _, micros := range amounts {
			usd := float64(micros) / 1e6
			whole := inexactMarker + formatUSDCell(usd)

			m := &model{width: w, height: 40}
			m.sessionsTbl = newSessionsTable()
			m.sessions = []session.SessionSummary{{ID: "sess-a", EventCount: 1}}
			m.spend.snap = &usage.Snapshot{
				Window: "1h", Priced: true,
				Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
					"sess-a": {
						Requests: 1, CostMicros: micros,
						PricedRequests: 1, PriceableRequests: 1, IncompleteRequests: 1,
					},
				}}},
			}
			m.layout()
			m.rebuildSessionsTable()

			switch got := sessionsRowCell(t, m, "COST"); got {
			case whole:
				if gw := lipgloss.Width(got); gw > costW {
					t.Errorf("term %d, %s: cell is %d columns in a %d-wide column; bubbles clips it",
						w, whole, gw, costW)
				}
			case costElision:
				if lipgloss.Width(whole) <= costW {
					t.Errorf("term %d, %s: elided although %d columns fit a %d-wide column",
						w, whole, lipgloss.Width(whole), costW)
				}
			case formatUSDCell(usd):
				t.Errorf("term %d: cell = %q — the marker was dropped to make the figure fit, "+
					"which restates a lower bound as an exact total", w, got)
			default:
				t.Errorf("term %d, %s: cell = %q, want the whole marked figure or %q",
					w, whole, got, costElision)
			}
		}
	}
}

func TestSessionCost_ReportsWhetherTheFigureIsExact(t *testing.T) {
	// IncompleteRequests is summed over the same buckets as the dollars, and one
	// inexact request is enough to make the session's total a lower bound.
	m := costSessionsModel()
	m.spend.snap = &usage.Snapshot{
		Window: "1h", Priced: true,
		Buckets: []usage.Bucket{
			{Series: map[string]usage.Counts{"sess-a": {CostMicros: 100_000, PricedRequests: 1, PriceableRequests: 1}}},
			{Series: map[string]usage.Counts{"sess-a": {
				CostMicros: 150_000, PricedRequests: 1, PriceableRequests: 1, IncompleteRequests: 1,
			}}},
			{Series: map[string]usage.Counts{"sess-b": {CostMicros: 9_000, PricedRequests: 1, PriceableRequests: 1}}},
		},
	}

	usd, priced, inexact := m.sessionCost("sess-a")
	if !priced || usd != 0.25 {
		t.Fatalf("sessionCost = (%v, %v), want (0.25, true)", usd, priced)
	}
	if !inexact {
		t.Error("inexact = false although one of the session's buckets reports an incomplete figure")
	}
	// A session with no incomplete request of its own must not inherit another's.
	if _, _, inexactB := m.sessionCost("sess-b"); inexactB {
		t.Error("inexact = true for a session whose own figures are all exact")
	}
}

func TestSessionCost_SumsEveryBucketForTheSession(t *testing.T) {
	// The strip asks for one bucket, but nothing guarantees the server folds to one
	// — resolution is negotiated, not dictated. Reading Buckets[0] alone would
	// report the first slice of the window as the whole of it.
	m := costSessionsModel()
	m.spend.snap = &usage.Snapshot{
		Window: "1h", Priced: true,
		Buckets: []usage.Bucket{
			{Series: map[string]usage.Counts{"sess-a": {CostMicros: 100_000, PricedRequests: 1, PriceableRequests: 1}}},
			{Series: map[string]usage.Counts{"sess-a": {CostMicros: 150_000, PricedRequests: 1, PriceableRequests: 1}}},
			{Series: nil},
		},
	}

	usd, priced, inexact := m.sessionCost("sess-a")
	if !priced {
		t.Fatal("priced = false for a session with two priced buckets")
	}
	if usd != 0.25 {
		t.Errorf("sessionCost = %v, want 0.25 (both buckets summed)", usd)
	}
	if inexact {
		t.Error("inexact = true although neither bucket reports an incomplete figure")
	}
}

func TestSessionCost_UnknownCases(t *testing.T) {
	priced := &usage.Snapshot{
		Window: "1h", Priced: true,
		Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
			"sess-a": {CostMicros: 100_000, PricedRequests: 1, PriceableRequests: 1},
		}}},
	}
	cases := []struct {
		name string
		snap *usage.Snapshot
		id   string
	}{
		{"no snapshot yet", nil, "sess-a"},
		{"window priced nothing", &usage.Snapshot{Window: "1h", Priced: false}, "sess-a"},
		{"empty session id", priced, ""},
		{"session not in the series", priced, "sess-b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := costSessionsModel()
			m.spend.snap = tc.snap
			usd, ok, inexact := m.sessionCost(tc.id)
			if ok {
				t.Errorf("priced = true; an unknown cost must be reported as unknown, not as %v", usd)
			}
			if usd != 0 {
				t.Errorf("usd = %v alongside priced=false; a caller trusting the figure would render it", usd)
			}
			// An unknown cost cannot be inexact: there is no figure for the claim to be
			// about, and a marker on a blank cell would be furniture.
			if inexact {
				t.Error("inexact = true for a cost that does not exist")
			}
		})
	}
}

// TestFitCostCell_ANegativeAmountRendersBlank.
//
// The last step before a figure reaches the screen, and the fourth money surface that was
// inheriting the server's no-negative-cost guarantee without restating it. formatUSDCell
// is faithful about the sign, so this cell printed "$-5.0000".
//
// Blank, not costElision: the ellipsis means "a figure exists and did not fit", which
// would be a claim that an impossible number is a real one. Blank means unpriced, which is
// what this is — the same reading sessionCost's priced=false already has in this table.
//
// Every width, including the ones where a positive figure of the same magnitude elides:
// the refusal must not depend on the fitting decision.
func TestFitCostCell_ANegativeAmountRendersBlank(t *testing.T) {
	for _, w := range []int{0, 4, 5, 7, 10, 20} {
		if got := fitCostCell(-5, false, w); got != "" {
			t.Errorf("fitCostCell(-5, false, %d) = %q, want blank", w, got)
		}
		if got := fitCostCell(-5, true, w); got != "" {
			t.Errorf("fitCostCell(-5, true, %d) = %q, want blank", w, got)
		}
	}
	// The mirror: the same magnitude, priced, still renders. Without it the guard could be
	// "fixed" by blanking the cell whenever the column is wide enough to notice.
	if got := fitCostCell(5, false, 10); got != "$5.0000" {
		t.Errorf("fitCostCell(5, false, 10) = %q, want $5.0000", got)
	}
}

// TestRebuildSessionsTable_ANegativeSessionCostLeavesTheCellBlank drives the same refusal
// through the pane, because that is where the reviewer saw "$-5.0000": in the COST column
// of a real table, beside sessions whose figures were fine.
func TestRebuildSessionsTable_ANegativeSessionCostLeavesTheCellBlank(t *testing.T) {
	m := costSessionsModel()
	m.sessions = []session.SessionSummary{{ID: "s1", EventCount: 1}}
	m.spend.snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{Requests: 1, CostMicros: -5_000_000, PricedRequests: 1, PriceableRequests: 1},
		Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
			"s1": {Requests: 1, CostMicros: -5_000_000, PricedRequests: 1},
		}}},
		Priced: true,
	}

	m.rebuildSessionsTable()
	rows := m.sessionsTbl.Rows()
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1; test premise is wrong", len(rows))
	}
	if got := rows[0][4]; got != "" {
		t.Errorf("COST cell = %q, want blank — a minus sign here reads as a refund", got)
	}
}
