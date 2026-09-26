package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/cmd/abctl/apiclient"
)

// figureProbe is the short prefix every "a surviving figure is never half a figure" assertion in
// this package probes with — "$1." for "$1.12".
//
// A NAMED FUNCTION rather than whole[:3] written out at each call site, because the guard below
// tests the detector itself: a probe shape inlined at four call sites is four things to keep in
// step, and the one that drifts is the one nobody notices.
func figureProbe(whole string) string {
	if len(whole) < 3 {
		return whole
	}
	return whole[:3]
}

// clipsFigure reports whether line shows a HALF-RENDERED figure: the probe shape is present and
// the whole figure is not.
//
// This is the detector the band's and the drawer's clip assertions call. It exists as a function
// so that it can be held to its own contract — see TestClipsFigure_CanActuallyFail.
func clipsFigure(line, whole string) bool {
	return strings.Contains(line, figureProbe(whole)) && !strings.Contains(line, whole)
}

// TestClipsFigure_CanActuallyFail guards the detector every clip assertion in this package uses.
// A clip test that cannot fail is worse than no clip test, because the suite reports it as
// coverage.
//
// RENAMED TWICE, and the second rename is the point. It began as
// TestRenderSpendStrip_TheClipAssertionCanActuallyFail, whose subject is gone — paneView stopped
// calling renderSpendStrip when renderSpendBand replaced it — and became
// TestFormatUSDCell_IsPrefixProbeable, which asserted the wrong thing:
//
//	clipped := whole[:i]                                   // a strict prefix of whole
//	if strings.Contains(clipped, prefix) && strings.Contains(clipped, whole) { ... }
//
// A strict prefix can never contain the whole, so that branch was unreachable; and the companion
// check on the correct line — !Contains("TODAY  $1.12", "$1.12") — was unreachable the other way.
// Both t.Errorf bodies were dead code, leaving only the premise Fatalf able to fire. Deriving both
// literals from formatUSDCell had removed ONE vacuity (the `x && !x` pair) and introduced another,
// which is what happens when a test asserts a property of its own fixture arithmetic instead of a
// property of the code.
//
// So the subject is now the DETECTOR, whose behaviour can be wrong: it must fire on every
// truncation, stay silent on the whole figure, and stay silent on a cell that shows no figure at
// all. Verified by mutation — dropping the "!" and swapping "&&" for "||" each fail here.
func TestClipsFigure_CanActuallyFail(t *testing.T) {
	whole := formatUSDCell(1.12)
	probe := figureProbe(whole)
	// THE PREMISE, and the original reason this test exists: the probe and the whole figure have
	// to be different strings with the whole one longer, or the detector has nothing to compare.
	if !strings.HasPrefix(whole, probe) || len(whole) <= len(probe) {
		t.Fatalf("formatUSDCell(1.12) = %q, which the %q probe cannot describe: every clip "+
			"assertion in this package needs rewriting alongside the formatter", whole, probe)
	}

	// IT FIRES ON EVERY TRUNCATION that still shows the probe, rather than on a few hand-picked
	// ones: a formatter with more digits gains more clipped forms and they all have to be caught.
	for i := len(probe); i < len(whole); i++ {
		if line := "TODAY  " + whole[:i]; !clipsFigure(line, whole) {
			t.Errorf("clipsFigure(%q, %q) = false: a clipped figure reads as a real, smaller "+
				"one, and this is the only thing looking for it", line, whole)
		}
	}
	// AND NOT ON THE WHOLE FIGURE, or every correct render is reported as a clip.
	if line := "TODAY  " + whole; clipsFigure(line, whole) {
		t.Errorf("clipsFigure(%q, %q) = true for unclipped output", line, whole)
	}
	// AND NOT ON A CELL THAT SHOWS NO FIGURE AT ALL. The band has three outcomes per span — a
	// figure, an em dash, and a dropped cell — and "not known here" is not a clipped figure.
	if line := "TODAY  " + emptyCell; clipsFigure(line, whole) {
		t.Errorf("clipsFigure(%q, %q) = true for a cell carrying no figure", line, whole)
	}
}

func TestSpendStripVisible_HiddenOnPreConnectionPickers(t *testing.T) {
	// The namespace and pod pickers run before any session exists, so there is no
	// cost to show. They also return early from paneView with their own layout.
	for _, p := range []paneID{paneNamespaces, panePods} {
		m := &model{height: 40}
		m.pane = p
		if m.spendStripVisible() {
			t.Errorf("pane %v: strip visible on a pre-connection picker", p)
		}
	}
}

func TestSpendStripVisible_ShownOnTheDataPanes(t *testing.T) {
	for _, p := range []paneID{paneSessions, paneEvents, paneDetail, panePipeline, paneUsage, paneCatalog} {
		m := &model{height: 40}
		m.pane = p
		if !m.spendStripVisible() {
			t.Errorf("pane %v: strip hidden on a data pane", p)
		}
	}
}

func TestSpendStripVisible_FoldsAwayOnAShortTerminal(t *testing.T) {
	// The spec's rule: below 20 rows the strip yields its row to the table, which
	// needs it more than the chrome does.
	m := &model{height: 19}
	m.pane = paneEvents
	if m.spendStripVisible() {
		t.Error("strip took a row on a 19-row terminal")
	}
	m.height = 20
	if !m.spendStripVisible() {
		t.Error("strip hidden at 20 rows, the documented threshold")
	}
}

func TestLayout_ReservesExactlyTheBandsRowsForTheStrip(t *testing.T) {
	// Get this wrong and every table renders one row too tall, pushing the footer
	// off-screen. The row is ADDED to the existing title(1) + footer(2) budget --
	// layout's old comment said "title + blank + footer" but there was never a
	// blank row to borrow.
	tall := &model{width: 100, height: 40}
	tall.pane = paneEvents
	tall.layout()

	short := &model{width: 100, height: 19} // below the fold threshold
	short.pane = paneEvents
	short.layout()

	// dividerLines is in both expectations, and in the short one it is the ONLY new term: the
	// rule closes the top block on every pane and at every height, so unlike the band it is
	// reserved below the fold too.
	if want := 40 - 3 - dividerLines - spendBandLines; tall.bodyHeight != want {
		t.Errorf("bodyHeight with the band = %d, want %d (height - title - 2 footer rows - "+
			"dividerLines - spendBandLines)", tall.bodyHeight, want)
	}
	if want := 19 - 3 - dividerLines; short.bodyHeight != want {
		t.Errorf("bodyHeight below the fold = %d, want %d (the divider's row, no band rows)",
			short.bodyHeight, want)
	}
}

func TestLayout_PickerPanesStillReserveTheStripRow(t *testing.T) {
	// The ruling: reserve by HEIGHT, not per pane. layout() runs on resize, so a
	// pane-aware reservation would need re-running on every pane transition -- and
	// a picker one row shorter than it could be is invisible next to an events
	// table that is wrong by a row and pushes the footer off the bottom.
	picker := &model{width: 100, height: 40}
	picker.pane = panePods
	picker.layout()

	if want := 40 - 3 - dividerLines - spendBandLines; picker.bodyHeight != want {
		t.Errorf("picker bodyHeight = %d, want %d: the reservation must not depend on the pane",
			picker.bodyHeight, want)
	}
	if picker.spendStripVisible() {
		t.Error("the picker reserves the row but must not DRAW the strip")
	}
}

func TestPaneView_DrawsTheStrip(t *testing.T) {
	// The mutation guard for task 3 step 7. A renderSpendBand call that nothing
	// asserts on is the failure mode here: the strip would be written, reviewed,
	// and never reach the screen. Deleting the append in paneView must fail this.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := New(ctx, apiclient.New("http://127.0.0.1:1")).(*model)
	m.width, m.height = 100, 40
	m.layout()
	m.spend.chains[spanHour].snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{
			Requests: 10, CostMicros: 1_120_000,
			PricedRequests: 10, PriceableRequests: 10,
		},
		Priced: true,
	}

	got := m.paneView()
	// The band carries no "SPEND" label: the column labels are the identity now, which is
	// why they sit above the values rather than beside them.
	if !strings.Contains(got, "LAST 1H") {
		t.Errorf("paneView output has no band label row; the band is not wired to the screen:\n%s", got)
	}
	if !strings.Contains(got, "$1.12") {
		t.Error("paneView output has no spend figure; the band is rendered from something other than spendSummary")
	}
	// The strip must be the SECOND row, directly under the title bar. "In the
	// chrome, read before the data" is the whole requirement -- a strip rendered
	// below the table is just the per-row cost figure again, one pane over.
	lines := strings.Split(got, "\n")
	if len(lines) < 2 {
		t.Fatalf("paneView rendered %d lines; expected at least a title and a strip", len(lines))
	}
	// ROW 1 IS THE WHOLE BAND, label and figure inline, which is what the fold means here. This
	// used to assert a label row at 1 and a value row at 2 — "labels above values is the whole
	// point" — and that point moved: the pair travels together now and row 2 belongs to whatever
	// follows the band. See spendBandLines.
	if !strings.Contains(lines[1], "LAST 1H") {
		t.Errorf("row 1 is %q, want the band directly under the title", lines[1])
	}
	if !strings.Contains(lines[1], "$1.12") {
		t.Errorf("row 1 is %q, want the hour's figure on the same row as its own label", lines[1])
	}
}

func TestPaneView_NoStripRowOnAShortTerminal(t *testing.T) {
	// Below the fold the row is not drawn AND not reserved; drawing it without the
	// reservation is what pushes the footer off the bottom of the terminal.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := New(ctx, apiclient.New("http://127.0.0.1:1")).(*model)
	m.width, m.height = 100, 19
	m.layout()
	m.spend.chains[spanHour].snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{Requests: 1, CostMicros: 1_120_000, PricedRequests: 1, PriceableRequests: 1},
		Priced: true,
	}

	// AGAINST THE BAND'S OWN LABELS, not against a "SPEND" prefix. This used to assert the absence
	// of the constant the old strip put at the head of its line — and the band has no such prefix,
	// so the condition became unfalsifiable the moment the band replaced the strip: it could not
	// have failed on a band drawn in full. The labels ARE the band's identity now.
	if got := m.paneView(); bandIsDrawn(got) {
		t.Errorf("19-row terminal drew the band rows it did not reserve: %q", got)
	}
}

// bandIsDrawn reports whether any band cell reached the output.
//
// ANY of the four rather than all: the fitter drops cells as the terminal narrows, so a test
// asserting the band is ABSENT has to fail on a single surviving cell — and one asserting it is
// present must not depend on which cells fitted.
func bandIsDrawn(view string) bool {
	for span := spendSpan(0); span < numSpendSpans; span++ {
		if strings.Contains(view, spendSpanDefs[span].label) {
			return true
		}
	}
	return false
}

func TestPaneView_PickerPanesDrawNoStrip(t *testing.T) {
	// paneNamespaces and panePods return early from paneView with their own
	// JoinVertical, so this also guards against the strip being added there.
	for _, p := range []paneID{paneNamespaces, panePods} {
		ctx, cancel := context.WithCancel(context.Background())
		m := New(ctx, apiclient.New("http://127.0.0.1:1")).(*model)
		m.width, m.height = 100, 40
		m.layout()
		m.pane = p
		m.spend.chains[spanHour].snap = &usage.Snapshot{
			Window: "1h",
			Totals: usage.Counts{Requests: 1, CostMicros: 1_120_000, PricedRequests: 1, PriceableRequests: 1},
			Priced: true,
		}
		if got := m.paneView(); bandIsDrawn(got) {
			t.Errorf("pane %v drew the band: %q", p, got)
		}
		cancel()
	}
}

func TestFormatSpendAge_RoundsToTheCoarsestUsefulUnit(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{45 * time.Second, "45s"},
		{90 * time.Second, "1m"},
		{3 * time.Minute, "3m"},
		{59*time.Minute + 59*time.Second, "59m"},
		{2 * time.Hour, "2h"},
	} {
		if got := formatSpendAge(tc.in); got != tc.want {
			t.Errorf("formatSpendAge(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestApplyTodayFigure_CarriesTheLedgersDamageDisclosure.
//
// The data half of the fix. usage.Snapshot.Degraded had ZERO non-test consumers in
// cmd/abctl: the server populated it, logged a warning, and nothing downstream read it — so
// a day that lost rows produced a figure indistinguishable from a clean one.
//
// The strip's today poll is the only ledger-backed request the TUI's chrome makes, which
// makes this the one field on the path that can be populated at all.
func TestApplyTodayFigure_CarriesTheLedgersDamageDisclosure(t *testing.T) {
	m := &model{}
	m.spend.chains[spanToday].snap = &usage.Snapshot{
		Window: usage.WindowToday,
		Totals: usage.Counts{Requests: 400, CostMicros: 4_170_000,
			PricedRequests: 400, PriceableRequests: 400},
		Priced:   true,
		Degraded: &usage.Degraded{SkippedLines: 3, TruncatedDays: 1},
	}

	var out spendSummary
	m.applyTodayFigure(&out)
	if out.TodayDegraded == nil {
		t.Fatal("TodayDegraded is nil; the ledger's damage disclosure reaches no renderer")
	}
	if out.TodayDegraded.SkippedLines != 3 || out.TodayDegraded.TruncatedDays != 1 {
		t.Errorf("TodayDegraded = %+v, want 3 lines and 1 day", *out.TodayDegraded)
	}
	// And the figure is still published: it is short, not unknown.
	if !out.HasToday || out.TodayUSD != 4.17 {
		t.Errorf("HasToday=%v TodayUSD=%v; a short figure was withheld rather than qualified",
			out.HasToday, out.TodayUSD)
	}
}

// TestApplyTodayFigure_ACleanReadLeavesNoDisclosure.
//
// The pointer's whole point: absence means the read was clean, so nothing may be rendered
// for it. Zeros in an always-present object would read as "checked, fine" from a producer
// that never checked.
func TestApplyTodayFigure_ACleanReadLeavesNoDisclosure(t *testing.T) {
	m := &model{}
	m.spend.chains[spanToday].snap = &usage.Snapshot{
		Window: usage.WindowToday,
		Totals: usage.Counts{Requests: 400, CostMicros: 4_170_000,
			PricedRequests: 400, PriceableRequests: 400},
		Priced: true,
	}

	var out spendSummary
	m.applyTodayFigure(&out)
	if out.TodayDegraded != nil {
		t.Errorf("TodayDegraded = %+v for a clean read", *out.TodayDegraded)
	}
}

// TestSpendSummary_CarriesTheClampDisclosureOnBothSpans.
//
// The data half. usage.Counts.Saturated had ZERO non-test consumers in cmd/abctl, so a clamped
// aggregate produced a figure indistinguishable from a settled one on every money surface at
// once. Both spans are asserted because the flag is on usage.Counts rather than on a ledger
// read: the ring's Add clamps too, so carrying it for the day alone would leave the strip's
// rolling reading able to publish a clamped figure bare.
func TestSpendSummary_CarriesTheClampDisclosureOnBothSpans(t *testing.T) {
	clamped := usage.Counts{Requests: 400, CostMicros: 4_170_000,
		PricedRequests: 400, PriceableRequests: 400, Saturated: true}

	m := &model{}
	m.spend.chains[spanHour].snap = &usage.Snapshot{Window: "1h", Totals: clamped, Priced: true,
		Buckets: []usage.Bucket{{Counts: clamped}}}
	m.spend.chains[spanToday].snap = &usage.Snapshot{Window: usage.WindowToday, Totals: clamped, Priced: true}

	out := m.spendSummary()
	if !out.Clamped {
		t.Error("Clamped is false; a clamped rolling window reaches no renderer")
	}
	if !out.TodayClamped {
		t.Error("TodayClamped is false; a clamped day reaches no renderer")
	}
	// And the figures are still published: they are floors, not unknowns, and withholding them
	// would report measured spend as unavailable.
	if !out.HasToday || out.WindowUSD == 0 {
		t.Errorf("HasToday=%v WindowUSD=%v; clamped figures were withheld rather than qualified",
			out.HasToday, out.WindowUSD)
	}
}

// TestSpendSummary_ACleanAggregateLeavesTheClampUnset is the mirror: false must mean "the
// arithmetic held", so nothing may be rendered for it.
func TestSpendSummary_ACleanAggregateLeavesTheClampUnset(t *testing.T) {
	clean := usage.Counts{Requests: 400, CostMicros: 4_170_000,
		PricedRequests: 400, PriceableRequests: 400}

	m := &model{}
	m.spend.chains[spanHour].snap = &usage.Snapshot{Window: "1h", Totals: clean, Priced: true,
		Buckets: []usage.Bucket{{Counts: clean}}}
	m.spend.chains[spanToday].snap = &usage.Snapshot{Window: usage.WindowToday, Totals: clean, Priced: true}

	out := m.spendSummary()
	if out.Clamped || out.TodayClamped {
		t.Errorf("Clamped=%v TodayClamped=%v for an aggregate that never clamped",
			out.Clamped, out.TodayClamped)
	}
}

// TestMoneyMarkers_AreThreeDistinctOneColumnClaims.
//
// usage.Snapshot.Degraded's doc is explicit that its claim must not be merged with
// usage.Counts.IncompleteRequests', nor shown under one marker; partialMarker is a third
// claim again. Spelling any two of them the same character would collapse two facts into one
// glyph WITHOUT A SINGLE RENDER TEST NOTICING — the figure would still carry "a marker", and
// every assertion phrased in terms of the constants would still hold. That is exactly what
// happened when this was mutated, which is why the distinctness is pinned directly.
//
// One display column each, which is what makes them survivable at every width the strip's
// fitter and fitTableColumns can produce. Not a rune count and not len(): both lie about a
// glyph, and the strip's whole budget is expressed in display cells.
func TestMoneyMarkers_AreThreeDistinctOneColumnClaims(t *testing.T) {
	for _, m := range []struct{ name, glyph string }{
		{"damagedMarker", damagedMarker},
		{"inexactMarker", inexactMarker},
		{"partialMarker", partialMarker},
	} {
		if w := lipgloss.Width(m.glyph); w != 1 {
			t.Errorf("%s = %q is %d display columns, want 1", m.name, m.glyph, w)
		}
	}
	seen := map[string]string{}
	for _, m := range []struct{ name, glyph string }{
		{"damagedMarker", damagedMarker},
		{"inexactMarker", inexactMarker},
		{"partialMarker", partialMarker},
	} {
		if other, dup := seen[m.glyph]; dup {
			t.Errorf("%s and %s are both %q — two claims under one marker", m.name, other, m.glyph)
		}
		seen[m.glyph] = m.name
	}
}

// TestMoneyFigure_OneMarkerForBothWaysAFigureCanBeShort.
//
// damagedMarker's two causes take ONE cell between them, and each keeps its own words. "!!" on a
// figure that is short twice over reads as emphasis rather than as two facts, and a fourth glyph
// would deepen the strip's vocabulary to draw a distinction that changes nothing about how the
// number must be read — where the WORDS do change what an operator goes and looks at: a day file
// for one, whatever produced 9.2e18 micros of traffic for the other.
//
// THROUGH moneyFigure, WHICH NO SCREEN REACHES, and that is deliberate here rather than an
// oversight: this test's subject is the RELATIONSHIP between one marker and two explanations, and
// moneyFigure is the only entry point that can carry both at once. The live path —
// moneyFigureTotal, from the drawer's per-model rows — passes nil for degraded, so it can only ever
// produce one of the two. The half of this that an operator can actually read is asserted through
// the rendered drawer in TestRenderSpendDrawer_AClampedSeriesRowSaysItIsAFloor; without that, this
// file pinned the vocabulary of a path nothing renders and the rendered path was pinned by nothing.
func TestMoneyFigure_OneMarkerForBothWaysAFigureCanBeShort(t *testing.T) {
	fig := moneyFigure(4.17, "today", 0, 400, 0, &usage.Degraded{SkippedLines: 3}, true)
	if n := strings.Count(fig.compact, damagedMarker); n != 1 {
		t.Errorf("compact form %q carries %d %q cells, want exactly 1", fig.compact, n, damagedMarker)
	}
	for _, want := range []string{saturatedNote, "3 lines lost"} {
		if !strings.Contains(fig.full, want) {
			t.Errorf("full form %q is missing %q — one marker must not collapse two causes into "+
				"one explanation", fig.full, want)
		}
	}
	// The clamp leads: it is short in every column of the aggregate, where a damaged read is
	// short in the dollars. Same order as the Cost pane and `abctl cost`.
	if clamp, damaged := strings.Index(fig.full, saturatedNote), strings.Index(fig.full, "3 lines lost"); clamp > damaged {
		t.Errorf("the damaged-read note outranks the clamp in %q", fig.full)
	}
}

// A wedged poll must be visible on the figure it belongs to, and the day figure is the one that
// could go hours stale unnoticed: it outranks the window, polls twelve times more slowly, and is
// the last thing the fitter drops.
//
// The age is ONE reading for the line, taken from the OLDER chain — so a fresh window poll
// cannot vouch for a wedged day poll.
func TestSpendSummary_TheAgeComesFromTheOlderChain(t *testing.T) {
	now := time.Now()
	m := &model{}
	m.spend.chains[spanHour].snap = &usage.Snapshot{
		Window: "1h", Priced: true,
		Totals: usage.Counts{Requests: 1, CostMicros: 1, PricedRequests: 1, PriceableRequests: 1},
	}
	// The window answered a moment ago; the day chain has been wedged for an hour.
	m.spend.chains[spanHour].lastFetch = now
	m.spend.chains[spanToday].lastFetch = now.Add(-time.Hour)

	got := m.spendSummary()
	if !got.Stale {
		t.Fatal("a day poll wedged for an hour reports no staleness, because a window poll from " +
			"one second ago was the only clock consulted — which puts the gap on the most " +
			"prominent figure on the line")
	}
	if got.Age < 59*time.Minute {
		t.Errorf("Age = %v, want ~1h: the age must be the OLDER chain's, not the fresher one's",
			got.Age)
	}

	// And a chain that has NEVER answered is not infinitely stale: today is unavailable on a
	// proxy with no ledger, and reporting that as staleness would mark every Kubernetes
	// deployment's strip permanently old.
	m.spend.chains[spanToday].lastFetch = time.Time{}
	if fresh := m.spendSummary(); fresh.Stale {
		t.Errorf("Stale = true with a today chain that never answered (age %v); an absent ledger "+
			"is not a wedged poll", fresh.Age)
	}
}
