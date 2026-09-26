package tui

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/cmd/abctl/apiclient"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/cost/usage"
)

// drawerSnap is four series whose cost ORDER differs from their request order, which is the
// whole point of the fixture: claude-haiku-4-5 has the most requests and the least spend, so
// a drawer ranking by requests would put it first and a drawer ranking by cost puts it third.
func drawerSnap() *usage.Snapshot {
	return &usage.Snapshot{
		Window: "1h", Group: usage.GroupModel, Priced: true,
		Totals: usage.Counts{Requests: 40, CostMicros: 11_121_980, Tokens: 8_577_000},
		Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
			"claude-opus-5": {Requests: 17, CostMicros: 11_121_400, Tokens: 7_980_000,
				PricedRequests: 17, PriceableRequests: 17, AvoidedMicros: 180_400},
			"claude-sonnet-5": {Requests: 2, CostMicros: 400, Tokens: 549_000,
				PricedRequests: 2, PriceableRequests: 2},
			"claude-haiku-4-5": {Requests: 120, CostMicros: 120, Tokens: 40_000,
				PricedRequests: 120, PriceableRequests: 120},
			"gpt-4o": {Requests: 9, CostMicros: 60, Tokens: 8_000,
				PricedRequests: 4, PriceableRequests: 9},
		}}},
	}
}

// The rows are ranked by COST, and the tail folds into one "(other)" band.
//
// Ranking is the assertion, not membership. This is the spend drawer: the row worth reading
// first is the one that cost the most, and for mixed traffic that is emphatically not the one
// with the most requests — 120 haiku calls cost a hundredth of 17 opus ones. A drawer that
// ranked by requests or tokens would pass a membership test and put the cheapest row on top.
func TestSpendDrawerRows_RanksByCostAndFoldsTheTail(t *testing.T) {
	rows := spendDrawerRows(drawerSnap(), spendDrawerSeries)

	if len(rows) != spendDrawerSeries+1 {
		t.Fatalf("rows = %d, want %d named plus one (other) band: %v",
			len(rows), spendDrawerSeries+1, rowLabels(rows))
	}
	want := []string{"claude-opus-5", "claude-sonnet-5", "claude-haiku-4-5", tailLabel}
	for i, w := range want {
		if rows[i].label != w {
			t.Errorf("row %d = %q, want %q (full order %v)", i, rows[i].label, w, rowLabels(rows))
		}
	}
	// The band carries the folded series' figures, not a placeholder: gpt-4o's 9 requests
	// and its coverage gap have to survive the fold or the drawer loses them silently.
	band := rows[len(rows)-1]
	if band.counts.Requests != 9 {
		t.Errorf("(other) requests = %d, want 9 — the folded series' counters were dropped",
			band.counts.Requests)
	}
	if band.counts.PriceableRequests-band.counts.PricedRequests != 5 {
		t.Errorf("(other) coverage gap = %d, want 5",
			band.counts.PriceableRequests-band.counts.PricedRequests)
	}
}

// "(other)" is always LAST, whatever it sums to.
//
// Reachable and not hypothetical: the folded tail can outweigh a named row, since the fold
// keeps the top three INDIVIDUALLY and the rest collectively. A band sorted by its total
// would then jump above a named series, and a reader would take the band's position as a
// ranking rather than as a remainder.
func TestSpendDrawerRows_TheOtherBandStaysLastEvenWhenItOutweighsANamedRow(t *testing.T) {
	snap := drawerSnap()
	// Four cheap series so the tail is large: each is individually below the named rows,
	// and together they exceed the third of them.
	for _, name := range []string{"m-a", "m-b", "m-c", "m-d"} {
		snap.Buckets[0].Series[name] = usage.Counts{
			Requests: 1, CostMicros: 5_000, PricedRequests: 1, PriceableRequests: 1,
		}
	}
	rows := spendDrawerRows(snap, spendDrawerSeries)

	last := rows[len(rows)-1]
	if last.label != tailLabel {
		t.Fatalf("last row = %q, want %q: %v", last.label, tailLabel, rowLabels(rows))
	}
	// And it really does outweigh a named row, or this test proves nothing.
	third := rows[spendDrawerSeries-1]
	if last.counts.CostMicros <= third.counts.CostMicros {
		t.Fatalf("(other) = %d is not above the third named row (%d), so the ordering rule was "+
			"never exercised", last.counts.CostMicros, third.counts.CostMicros)
	}
}

// Each row's caveats come from ITS OWN counters, never the window's.
//
// A per-series total that is 4-of-9 priced has to say so on its own line. Borrowing the
// window's coverage would attach a caveat measured over other traffic — the misattribution
// moneyFigure exists to prevent — and here the window is fully priced except for that one
// series, so the two answers differ.
func TestRenderSpendDrawer_EachRowCarriesItsOwnCoverage(t *testing.T) {
	lines := renderSpendDrawer(drawerSnap(), nil, usage.GroupModel, "1h", 200)
	joined := strings.Join(lines, "\n")

	// gpt-4o's gap folds into the band, which is where the caveat must appear.
	if !strings.Contains(joined, "unpriced") {
		t.Errorf("no coverage caveat anywhere in the drawer:\n%s", joined)
	}
	// The fully priced rows must NOT carry one.
	for _, line := range lines {
		if strings.Contains(line, "claude-opus-5") && strings.Contains(line, "unpriced") {
			t.Errorf("a fully priced row carries a coverage caveat: %q", line)
		}
	}
}

// The saving rides on the row that earned it, wearing its marker, and never joins the cost.
func TestRenderSpendDrawer_ShowsAPerSeriesSavingWithoutAddingItToCost(t *testing.T) {
	lines := renderSpendDrawer(drawerSnap(), nil, usage.GroupModel, "1h", 200)
	var opus string
	for _, l := range lines {
		if strings.Contains(l, "claude-opus-5") {
			opus = l
		}
	}
	if opus == "" {
		t.Fatal("no row for claude-opus-5")
	}
	// "saved $0.18", in cents and with no marker — the SAVED figure's caveat was unconditional and
	// this panel has no money heading to move it onto, so it was dropped (see renderTierRows). The
	// WORD is asserted along with the figure because it is what keeps the saving apart from the
	// cost now that the marker is gone: a bare "$0.18" beside "$11.12" is two spend figures with
	// nothing telling them apart.
	if !strings.Contains(opus, "saved $0.18") {
		t.Errorf("row %q is missing the saving or the word that identifies it", opus)
	}
	// NO MARKER *ON THIS FIXTURE*, which has IncompleteRequests == 0 — not "no marker ever on this
	// panel". The cost figure's marker is conditional and is still emitted.
	if strings.Contains(opus, inexactMarker) {
		t.Errorf("row %q carries %q with nothing inexact behind it", opus, inexactMarker)
	}
	if !strings.Contains(opus, "$11.12") {
		t.Errorf("row %q lost its cost", opus)
	}
	if strings.Contains(opus, "$11.1214") {
		t.Errorf("row %q kept four decimals; this column reads in cents", opus)
	}
	// 11.1214 + 0.1804, in cents.
	if strings.Contains(opus, "$11.30") {
		t.Errorf("row %q added the saving to the cost", opus)
	}
}

// The hint line names both keys and says which axis is current, because it is the only place
// either is written down. A drawer whose controls are undiscoverable is a drawer nobody
// changes the axis of.
func TestRenderSpendDrawer_HintLineNamesTheKeysAndTheCurrentAxis(t *testing.T) {
	lines := renderSpendDrawer(drawerSnap(), nil, usage.GroupEndpoint, "6h", 200)
	hint := lines[len(lines)-1]
	// "[a]", not "[g]": g is globally "go to top" and the drawer stays open alongside the table,
	// so it must not shadow that motion. See cycleSpendAxis.
	for _, want := range []string{"[a]", "[w]", "6h", "esc"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint %q is missing %q", hint, want)
		}
	}
	// The CURRENT axis is bracketed and the others are not, so the line says where the
	// cycle is as well as what it offers.
	if !strings.Contains(hint, "["+string(usage.GroupEndpoint)+"]") {
		t.Errorf("hint %q does not mark endpoint as current", hint)
	}
	if strings.Contains(hint, "["+string(usage.GroupModel)+"]") {
		t.Errorf("hint %q marks model as current when the axis is endpoint", hint)
	}
}

// The zero value of the state must be the live hour and the model axis — the first entry of
// each cycle, and the span a reader opening the drawer is most likely to want.
//
// THE OFFSET IS GONE, and this test is why it existed. windowStep used to be an offset from a
// default index purely because the old slice had 1h in the MIDDLE: a bare index of zero
// pointed at 15m, so a freshly constructed state would have polled a span nobody asked for —
// which is what the first version did, under a comment claiming it could not. With the band's
// four spans in ascending order the hour is first, so zero is already right and the offset has
// nothing left to correct.
func TestSpendState_ZeroValueRequestsTheLiveHour(t *testing.T) {
	var s spendState
	if got, want := s.window(), spendSpanDefs[spanHour].window; got != want {
		t.Errorf("window() = %q on a zero-value state, want %q", got, want)
	}
	if got := s.axis(); got != usage.GroupModel {
		t.Errorf("axis() = %q on a zero-value state, want %q", got, usage.GroupModel)
	}
	// A ring span asks for a single bucket; a symbolic one omits the resolution entirely.
	if got := s.windowResolution(); got != spendResolution {
		t.Errorf("windowResolution() = %v for the hour, want %v", got, spendResolution)
	}
}

// `w` REACHES EXACTLY THE BAND'S FOUR SPANS, in the band's order.
//
// The cycle used to be {15m, 1h, 6h} — ring diagnostics, none of them a span a budget is read
// against, and it could not reach the day at all. Asserted against spendSpanDefs rather than
// against four literals, so the two cannot drift: a span added to the band is a span `w` must
// be able to point the breakdown at.
func TestSpendDrawerWindows_AreExactlyTheBandsSpans(t *testing.T) {
	if len(spendDrawerWindows) != int(numSpendSpans) {
		t.Fatalf("w cycles %d windows but the band has %d spans", len(spendDrawerWindows), numSpendSpans)
	}
	for span := spendSpan(0); span < numSpendSpans; span++ {
		if got, want := spendDrawerWindows[span], spendSpanDefs[span].window; got != want {
			t.Errorf("cycle position %d is %q, want %q — `w` must reach the band's spans in the "+
				"band's order", span, got, want)
		}
	}
	// And the two spans the old cycle offered are gone: they are ring diagnostics, reachable
	// through `abctl cost --window`, and six stops to reach four useful ones is a worse surface.
	for _, gone := range []string{"15m", "6h"} {
		for _, w := range spendDrawerWindows {
			if w == gone {
				t.Errorf("%q is still in the cycle", gone)
			}
		}
	}
}

// Both cycles wrap and return to where they started, and every stop is distinct — a cycle
// that repeated a value would waste a keypress, and one that never returned to the default
// would trap the user away from it.
func TestSpendDrawer_CyclesWrapThroughEveryDistinctStop(t *testing.T) {
	// THROUGH THE REAL CYCLERS, not a copy of their arithmetic. Re-implementing
	// `(step + 1) % len(...)` inline meant the test asserted that its own expression wraps.
	// fetchSpend returns nil on a nil client, so the returned tea.Cmd can be discarded here.
	//
	// TWO FULL LAPS, and the exact sequence at each step. One lap is where this was blind:
	// with the counter incremented without bound, a resolution that CLAMPED out-of-range to the
	// first entry gave the right answer on the step immediately after the end — the only step a
	// single lap checks — and the wrong one on every step after that. Both resolutions wrap now
	// (see wrapIndex), and two laps is what proves it.
	// The expected sequence WRITTEN OUT, not derived from wrapIndex — deriving it from the
	// helper under test would make the assertion agree with whatever that helper does. It starts
	// at the FIRST entry now, which is the live hour: windowStep is a plain index since the
	// cycle became the band's four ascending spans.
	wantSpans := []string{"1h", usage.WindowToday, usage.Window7d, usage.WindowMonth}
	m := &model{}
	for lap := 0; lap < 2; lap++ {
		for i, want := range wantSpans {
			if got := m.spend.window(); got != want {
				t.Errorf("lap %d step %d: window() = %v, want %v", lap, i, got, want)
			}
			_ = m.cycleSpendWindow()
		}
	}
	if got, want := m.spend.window(), "1h"; got != want {
		t.Errorf("after two full laps window() = %q, want back at %q", got, want)
	}

	for lap := 0; lap < 2; lap++ {
		for i, want := range spendDrawerAxes {
			if got := m.spend.axis(); got != want {
				t.Errorf("lap %d step %d: axis() = %q, want %q", lap, i, got, want)
			}
			_ = m.cycleSpendAxis()
		}
	}
	if got := m.spend.axis(); got != usage.GroupModel {
		t.Errorf("after two full laps axis() = %q, want back at %q", got, usage.GroupModel)
	}
}

// A terminal too short refuses to open, and SAYS SO. A key that silently does nothing reads
// as a broken key.
//
// Both directions asserted: refusing everywhere would satisfy the short half on its own and
// silently cost the feature.
func TestToggleSpendDrawer_RefusesOnAShortTerminalAndExplains(t *testing.T) {
	tall := &model{width: 100, height: spendDrawerMinHeight}
	tall.pane = paneEvents
	tall.toggleSpendDrawer()
	if !tall.spend.expanded {
		t.Errorf("a %d-row terminal refused to open the drawer, which is its stated floor",
			spendDrawerMinHeight)
	}
	if !tall.spendDrawerVisible() {
		t.Error("expanded but not visible at the floor height")
	}

	short := &model{width: 100, height: spendDrawerMinHeight - 1}
	short.pane = paneEvents
	short.toggleSpendDrawer()
	if short.spend.expanded {
		t.Error("a terminal one row below the floor opened the drawer")
	}
	if short.flash == "" {
		t.Error("the refusal was silent; a key that does nothing reads as a broken key")
	}

	// And a second press closes it again.
	tall.toggleSpendDrawer()
	if tall.spend.expanded {
		t.Error("a second press did not close the drawer")
	}
}

// AND `$` ON A PANE THAT CANNOT HOST THE DRAWER LEAVES IT ALONE, which is the mirror of the gate
// esc already has.
//
// m.spend.expanded survives a move to the Usage pane, and the close branch tested that flag alone —
// before the host check, so it never ran there. Pressing `$` on Usage therefore closed a drawer the
// operator could not see, said nothing, and the breakdown was missing on the way back to Sessions.
// The flag is left ALONE rather than closed-and-restored: it is a strip expansion and the strip is
// global, so a pane that can host it should find it as the operator left it.
//
// DRIVEN THROUGH handleKey, not toggleSpendDrawer, because half of the defect is the routing:
// paneUsage's own switch handles m/w/b/s and has no default return, so `$` falls past it into the
// global drawer keys. A test calling the toggle directly cannot see that at all.
func TestHandleKey_DollarOnTheUsagePaneKeepsAnOffScreenDrawer(t *testing.T) {
	m := &model{width: 100, height: 40}
	m.pane = paneSessions
	m.handleKey(keyRune('$'))
	if !m.spendDrawerVisible() {
		t.Fatalf("the drawer did not open on the sessions pane (expanded=%v), so this test cannot "+
			"say anything about what $ does to an open one", m.spend.expanded)
	}

	m.pane = paneUsage
	m.flash = ""
	m.handleKey(keyRune('$'))
	if !m.spend.expanded {
		t.Error("$ on the usage pane closed the drawer: it is off screen there, so the keypress " +
			"took away something the operator could neither see nor have meant")
	}
	if m.flash == "" {
		t.Error("$ on the usage pane did nothing and said nothing, which is the broken-key " +
			"reading toggleSpendDrawer's own doc refuses")
	}

	m.pane = paneSessions
	if !m.spendDrawerVisible() {
		t.Error("the drawer did not come back on returning to a pane that hosts it")
	}
}

// The drawer never draws without the strip above it. A breakdown under a bare title, with no
// summary it is breaking down, is a pane — which is the one thing this is not.
func TestSpendDrawerVisible_RequiresTheStrip(t *testing.T) {
	// Tall enough for the drawer, but on a pane the strip does not draw on.
	m := &model{width: 100, height: 40}
	m.pane = panePods
	m.spend.expanded = true
	if m.spendDrawerVisible() {
		t.Error("the drawer draws on the pods picker, where the strip does not")
	}
	m.pane = paneEvents
	if !m.spendDrawerVisible() {
		t.Error("the drawer does not draw on a pane where the strip does")
	}
}

// The drawer's rows appear in the rendered view, under the strip and above the body, with the
// table still on screen — which is the entire argument for a drawer over a pane.
func TestPaneView_DrawsTheDrawerUnderTheStripAndKeepsTheBody(t *testing.T) {
	m := &model{width: 160, height: 40, endpoint: "http://x"}
	m.pane = paneSessions
	m.sessionsTbl = newSessionsTable()
	// BOTH CHAINS, because they are separate now: the band renders from its own spans and
	// the drawer from its own fold. Seeding only one was enough while they shared a poll.
	m.spend.chains[spanHour].snap = drawerSnap()
	m.spend.drawer.snap = drawerSnap()
	m.spend.expanded = true
	m.layout()

	out := m.paneView()
	// The band's label row, which is where the strip's "SPEND" used to be: the column labels
	// are the region's identity now.
	bandAt := strings.Index(out, "LAST 1H")
	rowAt := strings.Index(out, "claude-opus-5")
	if bandAt < 0 {
		t.Fatalf("no band in the view:\n%s", out)
	}
	if rowAt < 0 {
		t.Fatalf("no drawer row in the view:\n%s", out)
	}
	if rowAt < bandAt {
		t.Errorf("the drawer renders above the band it expands")
	}
	// The body survives: the header row of the sessions table must still be there.
	if !strings.Contains(out, "UPDATED") {
		t.Errorf("the table is gone from the view; a drawer that displaces the body has "+
			"become a pane:\n%s", out)
	}
}

// THE VIEW MUST FIT THE TERMINAL, open or closed.
//
// The check this test file was missing, and the defect it let through: layout() reserved a row
// for the strip and nothing for the drawer, so pressing `$` rendered spendDrawerLines lines more
// than the terminal has — measured at 45 lines in a 40-row terminal — and the footer went off the
// bottom on every pane. Substring presence and ordering, which is all the test above checks, is
// blind to it: every line it looks for was present, just not on screen.
//
// Both states and several heights, because the reservation is height-gated: below
// spendDrawerMinHeight the drawer must not draw AND must not reserve, and the floor itself is
// where an off-by-one would show.
func TestPaneView_FitsTheTerminalWithTheDrawerOpen(t *testing.T) {
	// THE SNAPSHOT VARIES TOO, and its absence is the case the first version could not see: it
	// only ever set drawerSnap(), so "the renderer pads a nil snapshot" was covered while "the
	// VIEW fills its reservation for one" was not — which reads as coverage. Before the first poll
	// answers renderSpendBand returns blank lines on purpose, and the reservations are height-gated, so
	// nothing filled them and the footer sat six rows up.
	for _, snap := range []*usage.Snapshot{drawerSnap(), nil} {
		for _, h := range []int{spendDrawerMinHeight - 1, spendDrawerMinHeight, 40, 60} {
			for _, open := range []bool{false, true} {
				m := &model{width: 160, height: h, endpoint: "http://x"}
				m.pane = paneSessions
				m.sessionsTbl = newSessionsTable()
				m.spend.drawer.snap = snap
				m.spend.expanded = open
				m.layout()

				lines := strings.Count(m.paneView(), "\n") + 1
				// EXACTLY the terminal height, not merely within it. "> height" is blind to the
				// other direction, and that direction shipped twice: the reservation is
				// unconditional while the render emitted only what it had, so one model left the
				// footer three rows above the bottom and no snapshot at all left it six.
				if lines != m.height {
					t.Errorf("height %d, open=%v, snapshot=%v: the view is %d lines (%+d) — the "+
						"footer is not at the bottom of the terminal",
						h, open, snap != nil, lines, lines-m.height)
				}
			}
		}
	}
}

// The hint line describes the DATA, not the next request. m.spend.axis() and .window() are what
// the next poll will ask for, so reading them at render time repainted the label the instant `a`
// or `w` was pressed — one poll ahead of rows still grouped and spanned the old way.
//
// The strip already follows this rule for its window figure ("print the window the SERVER
// reported"), and a label describing something other than the figures beside it is the mislabel
// the whole surface is written against.
func TestDrawerLabels_DescribeTheSnapshotNotTheNextRequest(t *testing.T) {
	m := &model{width: 200, height: 60}
	m.pane = paneSessions
	// The snapshot in hand was grouped by model over an hour.
	m.spend.drawer.snap = drawerSnap()
	m.spend.drawer.snap.Window = "1h0m0s"
	m.spend.drawer.snap.Group = usage.GroupModel

	// The operator presses `a` and `w`: the NEXT poll will ask for endpoint over 6h.
	m.spend.groupIdx, m.spend.windowStep = 1, 1
	if m.spend.axis() == usage.GroupModel {
		t.Fatal("setup: the requested axis did not move")
	}

	axis, window := m.drawerLabels()
	if axis != usage.GroupModel {
		t.Errorf("axis label = %q while the rows on screen are grouped by %q: the label leads the "+
			"data by one poll", axis, usage.GroupModel)
	}
	// THE BAND'S OWN LABEL, not "1h": the served window is "1h0m0s" and the band cell directly
	// above this caption says LAST 1H, so anything else here is two spellings of one period in
	// one region. This expectation used to be "1h", which is what spanLabelFor returned when it
	// compared the served string to the requested one with == : the hour is the only span written
	// as a duration, so it was the only one that missed its own label.
	if window != spendSpanDefs[spanHour].label {
		t.Errorf("window label = %q, want %q — the span the answer covers, spelled the way the "+
			"band spells it", window, spendSpanDefs[spanHour].label)
	}

	// With no snapshot there is nothing to describe, so the requested values are the honest
	// fallback: a blank axis would read as a rendering fault.
	m.spend.drawer.snap = nil
	if axis, window := m.drawerLabels(); axis == "" || window == "" {
		t.Errorf("labels = %q/%q with no snapshot; want the requested values rather than blanks",
			axis, window)
	}
}

// AND THROUGH THE VIEW, because the unit above cannot see the call site. Asserting drawerLabels in
// isolation leaves paneView free to go on reading m.spend.axis() directly — verified: that mutation
// passed the unit test. The rendered hint line is what a user reads, so that is what has to be
// pinned.
func TestPaneView_TheHintLineLabelsTheSnapshotNotTheNextRequest(t *testing.T) {
	m := &model{width: 200, height: 60, endpoint: "http://x"}
	m.pane = paneSessions
	m.sessionsTbl = newSessionsTable()
	// The BAND's chain as well as the drawer's: renderSpendDrawer is gated on the band having
	// drawn, because a headless breakdown would be a pane. They are separate polls now, so a
	// fixture seeding one gets a screen with no drawer on it at all.
	m.spend.chains[spanHour].snap = drawerSnap()
	m.spend.drawer.snap = drawerSnap()
	m.spend.drawer.snap.Window = "1h0m0s"
	m.spend.drawer.snap.Group = usage.GroupModel
	m.spend.expanded = true
	// `a` and `w` pressed: the next poll will ask for endpoint over 6h, the rows on screen are
	// still model over an hour.
	m.spend.groupIdx, m.spend.windowStep = 1, 1
	m.layout()

	out := m.paneView()
	if !strings.Contains(out, "["+string(usage.GroupModel)+"]") {
		t.Errorf("the hint line does not bracket %q, the axis the rows on screen are grouped by:\n%s",
			usage.GroupModel, out)
	}
	if !strings.Contains(out, "[w] "+spendSpanDefs[spanHour].label) {
		t.Errorf("the hint line does not report %q, the span the answer covers as the band spells "+
			"it:\n%s", spendSpanDefs[spanHour].label, out)
	}
	// The queued values must not be on screen as though they described the data.
	if strings.Contains(out, "["+string(usage.GroupEndpoint)+"]") || strings.Contains(out, "[w] 6h") {
		t.Errorf("the hint line reports the axis or span the NEXT poll will ask for:\n%s", out)
	}
}

// And the reservation has to be the size the renderer actually emits, or the fit above holds by
// luck. Asserted against renderSpendDrawer's own output rather than against the number 5.
func TestSpendDrawerLines_MatchesWhatTheRendererEmits(t *testing.T) {
	// EVERY shape, not just the full one. The reservation is a fixed maximum, so a snapshot with
	// one series or none at all has to occupy it too — a drawer that emits what it happens to have
	// leaves the footer floating.
	for _, tc := range []struct {
		name string
		snap *usage.Snapshot
	}{
		// More series than the drawer keeps: named rows, the band, and the hint.
		{name: "full", snap: drawerSnap()},
		{name: "one series", snap: &usage.Snapshot{
			Window: "1h", Group: usage.GroupModel, Priced: true,
			Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
				"opus": {Requests: 1, CostMicros: 5000, PricedRequests: 1, PriceableRequests: 1},
			}}},
		}},
		// Before the first poll answers: the hint line and nothing to break down.
		{name: "no snapshot", snap: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := len(renderSpendDrawer(tc.snap, nil, usage.GroupModel, "1h", 200)); got != spendDrawerLines {
				t.Errorf("renderSpendDrawer emits %d lines but layout() reserves %d: the difference "+
					"is either a footer off the bottom or a footer floating above it",
					got, spendDrawerLines)
			}
		})
	}
}

func rowLabels(rows []drawerRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.label)
	}
	return out
}

// An "(other)" the AGGREGATOR produced must merge into the tail, not appear beside it.
//
// usage caps how many labels it tracks per bucket and folds the rest into its own "(other)"
// entry, so that label can arrive in the snapshot's Series map — and rank inside the top three,
// since it is a sum of everything the server dropped. foldTailSeries merges it; the drawer
// truncated the ranked list by hand and appended a band unconditionally, so the row appeared
// TWICE, each copy carrying the same merged total. The rows then summed past the strip's
// headline above them by the whole tail.
//
// THE SUM IS THE ASSERTION, not just the row count. Two bands with the same label is a visible
// oddity; two bands with the same TOTAL is a wrong number, and it is the one a reader would act
// on. Every existing case here exercises a fold-produced "(other)" only, which is why this
// survived.
func TestSpendDrawerRows_AnAggregatorOtherMergesRatherThanDuplicating(t *testing.T) {
	snap := &usage.Snapshot{
		Window: "1h", Group: usage.GroupModel, Priced: true,
		Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
			"opus":   {Requests: 17, CostMicros: 10_000_000, PricedRequests: 17, PriceableRequests: 17},
			"sonnet": {Requests: 2, CostMicros: 8_000_000, PricedRequests: 2, PriceableRequests: 2},
			// The server's own band, ranking SECOND by cost — inside the top three.
			tailLabel: {Requests: 30, CostMicros: 9_000_000, PricedRequests: 30, PriceableRequests: 30},
			// And a genuine tail for the fold to merge into it.
			"haiku":  {Requests: 40, CostMicros: 1_900_000, PricedRequests: 40, PriceableRequests: 40},
			"gpt-4o": {Requests: 9, CostMicros: 1_000_000, PricedRequests: 9, PriceableRequests: 9},
		}}},
	}
	// The snapshot's own total, which the rows must not exceed.
	var snapTotal int64
	for _, b := range snap.Buckets {
		for _, c := range b.Series {
			snapTotal += c.CostMicros
		}
	}

	rows := spendDrawerRows(snap, spendDrawerSeries)

	seen := 0
	var rowTotal int64
	for _, r := range rows {
		rowTotal += r.counts.CostMicros
		if r.label == tailLabel {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("%q appears %d times in %v, want once: two bands both meaning \"the rest\" is "+
			"indefensible, which is why foldTailSeries merges them", tailLabel, seen, rowLabels(rows))
	}
	if rowTotal != snapTotal {
		t.Errorf("the rows sum to %d micros against a snapshot holding %d (delta %+d): the drawer "+
			"is reporting more spend than the strip's headline above it",
			rowTotal, snapTotal, rowTotal-snapTotal)
	}
}

// The refusal must name the REAL reason. A pane that cannot host the drawer has nothing to do
// with height, and a height message there sends the reader to resize a terminal that was never
// the problem — on the file's own standard, "a key that explains the height requirement is a key
// the user stops pressing for a reason".
func TestToggleSpendDrawer_RefusesPerPaneWithTheRightReason(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pane    paneID
		wantSub string
	}{
		// Before a connection exists there is no spend at all, let alone a breakdown.
		{name: "pods picker", pane: panePods, wantSub: "until a pod is connected"},
		{name: "namespaces picker", pane: paneNamespaces, wantSub: "until a pod is connected"},
		// That pane IS the breakdown, and it owns the keys the drawer's hint line would
		// advertise.
		{name: "usage pane", pane: paneUsage, wantSub: "usage pane is the breakdown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A terminal with ample room, so height cannot be the cause of any refusal.
			m := &model{width: 200, height: 60}
			m.pane = tc.pane
			m.toggleSpendDrawer()

			if m.spend.expanded {
				t.Fatalf("the drawer opened on %v", tc.pane)
			}
			if !strings.Contains(m.flash, tc.wantSub) {
				t.Errorf("flash = %q, want it to mention %q — a 60-row terminal is not short, and "+
					"a height complaint here is a wrong reason rather than a missing one",
					m.flash, tc.wantSub)
			}
			if strings.Contains(m.flash, "short") {
				t.Errorf("flash = %q blames the terminal height on a pane that cannot host the "+
					"drawer at any height", m.flash)
			}
		})
	}

	// And a pane that CAN host it still opens, or the refusals above would be indistinguishable
	// from a drawer that never opens anywhere.
	m := &model{width: 200, height: 60}
	m.pane = paneSessions
	m.toggleSpendDrawer()
	if !m.spend.expanded {
		t.Errorf("the drawer refused on the sessions pane too (flash %q)", m.flash)
	}
}

// THE ROW MOST IN NEED OF A CAVEAT IS THE ONE THAT PRICED NOTHING, and it was the only row that
// could not carry one: the money figure was gated on PricedRequests or CostMicros being
// non-zero, so a 0-of-40-priced series rendered its request and token counts with nothing
// anywhere on the line saying its cost was unknown.
//
// "Each row's caveats come from its own counters" held only for rows that managed to price
// something — which is the inverse of what a caveat is for.
func TestRenderSpendDrawer_AFullyUnpricedSeriesSaysItsCostIsUnknown(t *testing.T) {
	snap := &usage.Snapshot{
		Window: "1h", Group: usage.GroupModel, Priced: true,
		Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
			"claude-opus-5": {Requests: 17, CostMicros: 11_121_400, Tokens: 7_980_000,
				PricedRequests: 17, PriceableRequests: 17},
			// Priceable and priced by nothing: a model with no rate in the table.
			"mystery-model": {Requests: 40, Tokens: 900_000, PriceableRequests: 40},
		}}},
	}
	var row string
	for _, l := range renderSpendDrawer(snap, nil, usage.GroupModel, "1h", 200) {
		if strings.Contains(l, "mystery-model") {
			row = l
		}
	}
	if row == "" {
		t.Fatal("no row for the unpriced series")
	}
	if !strings.Contains(row, "cost unavailable") {
		t.Errorf("row %q shows volume with no word about its cost being unknown", row)
	}
	if !strings.Contains(row, "40 of 40 unpriced") {
		t.Errorf("row %q does not name the size of the gap", row)
	}
	// Never a zero: a zero cost and an unknown cost are different answers, and this row is the
	// second kind.
	if strings.Contains(row, "$0.00") {
		t.Errorf("row %q renders $0.00 for a cost nobody produced", row)
	}
	// And a priced row in the same drawer is NOT annotated, or the caveat means nothing.
	for _, l := range renderSpendDrawer(snap, nil, usage.GroupModel, "1h", 200) {
		if strings.Contains(l, "claude-opus-5") && strings.Contains(l, "unavailable") {
			t.Errorf("a fully priced row carries the unknown-cost caveat: %q", l)
		}
	}
}

// A NEGATIVE SERIES TOTAL must not print, on the rule every other money surface in this package
// already follows through negativeCost: spendSummary refuses it for the window figure,
// applyTodayFigure for the day, renderCostSummary for the pane, sessionMoneyCell for the column.
// The drawer was the newest money surface and the only one that did not inherit the guard, so a
// series with priced requests and an impossible sum rendered "$-5.0000" — a credit nobody issued
// in a column of costs.
//
// The sum is what is refused, not each bucket: a positive bucket and a negative one can cancel to
// something plausible, and it is the PUBLISHED figure that has to be refusable.
func TestRenderSpendDrawer_ANegativeSeriesTotalIsUnpricedNotARefund(t *testing.T) {
	snap := &usage.Snapshot{
		Window: "1h", Group: usage.GroupModel, Priced: true,
		Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
			// Priced, and impossible — which the gate on PricedRequests admitted.
			"broken-producer": {Requests: 4, CostMicros: -5_000_000,
				PricedRequests: 4, PriceableRequests: 4},
			"claude-opus-5": {Requests: 17, CostMicros: 11_121_400,
				PricedRequests: 17, PriceableRequests: 17},
		}}},
	}
	lines := renderSpendDrawer(snap, nil, usage.GroupModel, "1h", 200)
	joined := strings.Join(lines, "\n")

	if strings.Contains(joined, "$-5") || strings.Contains(joined, "-$5") {
		t.Errorf("the drawer prints a negative cost:\n%s", joined)
	}
	var row string
	for _, l := range lines {
		if strings.Contains(l, "broken-producer") {
			row = l
		}
	}
	if row == "" {
		t.Fatal("no row for the series with the impossible total")
	}
	if !strings.Contains(row, "cost unavailable") {
		t.Errorf("row %q shows no figure and does not say the cost is unknown either", row)
	}
	// Coverage is NOT the complaint here — every request priced — so naming a gap would send a
	// reader to the rate table for a producer bug.
	if strings.Contains(row, "unpriced") {
		t.Errorf("row %q blames coverage for an impossible figure", row)
	}
	// And the healthy series in the same drawer still shows its figure.
	if !strings.Contains(joined, "$11.12") {
		t.Errorf("the good row lost its figure:\n%s", joined)
	}
}

// runeKey builds the KeyMsg handleKey sees for an ordinary character.
func runeKey(r rune) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}} }

// THROUGH handleKey, which nothing in this suite did — and that is the gap the esc defect
// slipped through. Every other drawer test calls toggleSpendDrawer, cycleSpendAxis and the
// renderer directly, so the bindings themselves, their guards and their ORDER against the rest of
// the key handler were untested.
func TestHandleKey_TheDrawersBindings(t *testing.T) {
	newModel := func(pane paneID) *model {
		m := &model{width: 200, height: 60}
		m.pane = pane
		m.sessionsTbl = newSessionsTable()
		m.spend.drawer.snap = drawerSnap()
		return m
	}

	t.Run("$ toggles", func(t *testing.T) {
		m := newModel(paneSessions)
		m.handleKey(runeKey('$'))
		if !m.spendDrawerVisible() {
			t.Fatal("$ did not open the drawer")
		}
		m.handleKey(runeKey('$'))
		if m.spend.expanded {
			t.Error("a second $ did not close it")
		}
	})

	t.Run("a and w only act while it is open", func(t *testing.T) {
		m := newModel(paneSessions)
		// Closed: both must be inert, or they take letters from the rest of the UI.
		before := m.spend
		m.handleKey(runeKey('a'))
		m.handleKey(runeKey('w'))
		if m.spend.groupIdx != before.groupIdx || m.spend.windowStep != before.windowStep {
			t.Error("a or w changed the drawer's state while it was closed")
		}

		m.handleKey(runeKey('$'))
		m.handleKey(runeKey('a'))
		if m.spend.axis() != spendDrawerAxes[1] {
			t.Errorf("axis = %q after one `a`, want %q", m.spend.axis(), spendDrawerAxes[1])
		}
		m.handleKey(runeKey('w'))
		if m.spend.window() == spendSpanDefs[spanHour].window {
			t.Errorf("window = %v after one `w`, want it moved off the default", m.spend.window())
		}
	})

	// THE REGRESSION. esc used to be gated on the flag rather than on what is on screen, so a
	// drawer left open on one pane swallowed esc on a pane that cannot host it — the Usage pane
	// then needed a second press to exit.
	t.Run("esc is not swallowed where the drawer cannot show", func(t *testing.T) {
		m := newModel(paneSessions)
		m.handleKey(runeKey('$'))
		if !m.spend.expanded {
			t.Fatal("setup: the drawer did not open")
		}
		// Move to a pane that cannot host it. The flag survives, deliberately.
		m.pane = paneUsage
		if m.spendDrawerVisible() {
			t.Fatal("setup: the drawer is visible on the usage pane")
		}
		m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
		if !m.spend.expanded {
			t.Error("esc closed a drawer that is not on screen, so it never reached the pane — " +
				"the Usage pane needs a second press to exit")
		}
		// THE POSITIVE HALF, and the reason this subtest was not a control for its own name.
		// Asserting only that the flag survived says nothing about where esc went: a handler
		// that swallowed the key entirely, or returned before the pane got it, passed. What the
		// regression was actually about is the back-out, so assert the back-out.
		if m.pane == paneUsage {
			t.Errorf("esc did not leave the usage pane (pane = %v) — the flag surviving only "+
				"means the drawer ignored the key, not that the pane received it", m.pane)
		}
	})

	t.Run("esc closes it where it does show", func(t *testing.T) {
		m := newModel(paneSessions)
		m.handleKey(runeKey('$'))
		m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
		if m.spend.expanded {
			t.Error("esc did not close a drawer that was on screen")
		}
	})

	// A resize below the floor is the other way the flag and the screen part company.
	//
	// ON paneEvents, whose esc backs out to Sessions, so the key's arrival at the pane is
	// observable without disturbing the handler under test. An open filter looked like the
	// cheaper signal and was inert: keys.go gates the whole spend block on `!m.filtering`, so
	// `m.filtering = true` skipped the esc case entirely — which made the flag assertion below
	// vacuous too, since nothing could have cleared it. The first version of this subtest
	// asserted less than the one it replaced.
	t.Run("esc is not swallowed below the height floor", func(t *testing.T) {
		m := newModel(paneEvents)
		m.handleKey(runeKey('$'))
		m.height = spendDrawerMinHeight - 1
		if m.spendDrawerVisible() {
			t.Fatal("setup: still visible below the floor")
		}
		m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
		if m.pane == paneEvents {
			t.Error("esc never reached the pane below the height floor — still on Events, so the " +
				"off-screen drawer swallowed the key rather than passing it on")
		}
		if !m.spend.expanded {
			t.Error("esc closed an off-screen drawer after a resize")
		}
	})
}

// EVERY pane, with its host decision stated — so adding a pane forces a choice here rather than
// inheriting one, and so the README's scope column and the `?` overlay have a single list to be
// checked against. Both have already drifted from it once.
func TestSpendDrawerHost_CoversEveryPane(t *testing.T) {
	// Keyed by pane, and the switch in spendDrawerHost lists the refusals — so a new paneID
	// appears here as a missing map entry rather than silently defaulting to "hosts it". It
	// caught panePluginDetail missing from this list on the first run, which is the same service
	// TestPaneKeysCoverAllPanes performs for the help overlay and for the same reason: paneUsage
	// once shipped reachable by `u` and named nowhere.
	want := map[paneID]bool{
		paneNamespaces:   false, // nothing connected yet
		panePods:         false, // likewise
		paneUsage:        false, // already a breakdown, and it owns w/b/m
		paneSessions:     true,
		paneEvents:       true,
		paneDetail:       true,
		panePipeline:     true,
		panePluginDetail: true,
		paneCatalog:      true,
	}
	for p := paneID(0); p <= lastPaneID; p++ {
		expect, listed := want[p]
		if !listed {
			t.Errorf("pane %d is not listed here: a new pane must have its drawer decision made "+
				"deliberately, and the README's scope column has to be updated with it", p)
			continue
		}
		if ok, why := m0().withPane(p).spendDrawerHost(); ok != expect {
			t.Errorf("pane %d: hosts = %v, want %v (reason %q)", p, ok, expect, why)
		}
	}
	// A refusal without a reason is what produced the wrong-reason bug; none may be silent.
	for p, expect := range want {
		if expect {
			continue
		}
		if _, why := m0().withPane(p).spendDrawerHost(); why == "" {
			t.Errorf("pane %d refuses with no reason to show the user", p)
		}
	}
}

func m0() *model { return &model{width: 200, height: 60} }

func (m *model) withPane(p paneID) *model { m.pane = p; return m }

// AN UNPRICED TAIL MUST STILL APPEAR. foldTailSeries appends its "(other)" to the kept list only
// when the tail's METRIC total is positive, and this drawer's metric is COST — so a tail of models
// with no rate produced folded buckets carrying the band and a ranked list that never named it,
// and the row was dropped. 100 requests and 2.7M tokens went missing with every figure above them
// unchanged.
//
// The inverse of TestSpendDrawerRows_AnAggregatorOtherMergesRatherThanDuplicating: that one
// catches rows summing PAST the snapshot, this one catches them summing under it. The quiet
// direction needed its own test, which is why it shipped.
//
// Reachable rather than exotic: a gateway without a full rate card leaves models permanently
// unpriced, and the Usage pane never met this because its metric is tokens or requests — positive
// whenever the tail exists at all.
func TestSpendDrawerRows_AnUnpricedTailStillGetsItsBand(t *testing.T) {
	snap := &usage.Snapshot{
		Window: "1h", Group: usage.GroupModel, Priced: true,
		Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
			"opus":   {Requests: 1, CostMicros: 5000, Tokens: 1000, PricedRequests: 1, PriceableRequests: 1},
			"sonnet": {Requests: 1, CostMicros: 4000, Tokens: 1000, PricedRequests: 1, PriceableRequests: 1},
			"haiku":  {Requests: 1, CostMicros: 3000, Tokens: 1000, PricedRequests: 1, PriceableRequests: 1},
			// The tail: real traffic, no rate, so zero cost.
			"unpriced-a": {Requests: 60, Tokens: 1_000_000, PriceableRequests: 60},
			"unpriced-b": {Requests: 40, Tokens: 700_000, PriceableRequests: 40},
		}}},
	}
	// What the snapshot holds, so the assertion is against the data and not a copied constant.
	var wantReq, wantTok int64
	for _, b := range snap.Buckets {
		for _, c := range b.Series {
			wantReq += c.Requests
			wantTok += c.Tokens
		}
	}

	rows := spendDrawerRows(snap, spendDrawerSeries)

	if !func() bool {
		for _, r := range rows {
			if r.label == tailLabel {
				return true
			}
		}
		return false
	}() {
		t.Fatalf("no %q row in %v: the tail priced nothing, so its requests and tokens are "+
			"invisible while the figures above are unchanged", tailLabel, rowLabels(rows))
	}

	var gotReq, gotTok int64
	for _, r := range rows {
		gotReq += r.counts.Requests
		gotTok += r.counts.Tokens
	}
	if gotReq != wantReq || gotTok != wantTok {
		t.Errorf("rows account for %d requests and %d tokens, snapshot holds %d and %d — the "+
			"breakdown sums UNDER the answer it breaks down", gotReq, gotTok, wantReq, wantTok)
	}
	// The band carries no cost, and must not invent one.
	for _, r := range rows {
		if r.label == tailLabel && r.counts.CostMicros != 0 {
			t.Errorf("%q reports %d micros for a tail that priced nothing", tailLabel, r.counts.CostMicros)
		}
	}
}

// Ranking is money, so it saturates like money.
//
// The drawer displays Counts.Add's saturating totals and used to RANK on a raw `+=`, which
// makes the two disagree exactly where int64 runs out. The consequence is not a wrong figure
// but a missing row: a negative rank sorts below every real series, foldTailSeries folds the
// window's most expensive model into (other), and the reader sees four cheap models and a
// band. Unreachable in practice at $9.2T per label — pinned because the fix is the package's
// own accumulator and the next money surface should copy this one, not the old one.
func TestRankSeriesByCost_SaturatesInsteadOfWrapping(t *testing.T) {
	half := int64(math.MaxInt64/2) + 100
	buckets := []usage.Bucket{
		{Series: map[string]usage.Counts{"whale": {Requests: 1, CostMicros: half}}},
		{Series: map[string]usage.Counts{"whale": {Requests: 1, CostMicros: half}}},
		{Series: map[string]usage.Counts{"minnow": {Requests: 1, CostMicros: 10}}},
	}
	ranked := rankSeriesByCost(buckets)
	if len(ranked) == 0 {
		t.Fatal("no series ranked")
	}
	if ranked[0].label != "whale" {
		t.Errorf("ranked first = %q (total %d), want whale — the overflowing series must rank "+
			"above a 10-micro one, not below it", ranked[0].label, ranked[0].total)
	}
	for _, s := range ranked {
		if s.total < 0 {
			t.Errorf("series %q ranks at %d: a money total wrapped negative", s.label, s.total)
		}
	}
}

// Traffic that can never carry a price says so, in its own words.
//
// Requests with no priceable one among them is what an MCP-only endpoint or agent looks like,
// and it took the branch nothing matched: no cost cell, no caveat. "cost unavailable" is the
// wrong word for it — the cost is known to be nothing — so the row needs a third spelling, and
// this test pins both halves of that: the row says something, and it does not say the thing
// that would send a reader to the rate table.
func TestDrawerFigures_NamesTrafficThatCannotBePriced(t *testing.T) {
	snap := &usage.Snapshot{
		Window: "1h", Group: usage.GroupEndpoint, Priced: true,
		Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
			"inference.svc": {Requests: 5, Tokens: 100, CostMicros: 900,
				PricedRequests: 5, PriceableRequests: 5},
			"mcp-tools.svc": {Requests: 9, Tokens: 8000},
		}}},
	}
	var row string
	for _, line := range renderSpendDrawer(snap, nil, usage.GroupEndpoint, "1h", 200) {
		if strings.Contains(line, "mcp-tools.svc") {
			row = line
		}
	}
	if row == "" {
		t.Fatal("the unpriceable series has no row at all")
	}
	if !strings.Contains(row, "not priceable") {
		t.Errorf("row %q leaves the cost slot blank for traffic that cannot carry a price", row)
	}
	if strings.Contains(row, "cost unavailable") {
		t.Errorf("row %q says the cost is unavailable, but nothing here is priceable — that "+
			"sends a reader to the rate table for traffic that has no rate", row)
	}
	// The priced row beside it is untouched: this branch is reached only when nothing priced
	// AND nothing could have.
	for _, line := range renderSpendDrawer(snap, nil, usage.GroupEndpoint, "1h", 200) {
		if strings.Contains(line, "inference.svc") && strings.Contains(line, "not priceable") {
			t.Errorf("priced row %q picked up the unpriceable caveat", line)
		}
	}
}

// tierSnap is drawerSnap with a modelled mix on the totals, so the tier column has something
// to say.
func tierSnap() *usage.Snapshot {
	s := drawerSnap()
	s.Totals.InputCostMicros = 3000
	s.Totals.CacheWriteCostMicros = 7500
	s.Totals.CacheReadCostMicros = 30000
	s.Totals.OutputCostMicros = 45000
	return s
}

// Two columns, headed, tiers left and series right.
func TestRenderSpendDrawer_ShowsBothColumnsWithHeaders(t *testing.T) {
	lines := renderSpendDrawer(tierSnap(), nil, usage.GroupModel, "1h", 100)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"WHERE IT WENT", "BY MODEL", "cache-read", "claude-opus-5"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the panel is missing %q:\n%s", want, joined)
		}
	}
	// The header names the CURRENT axis, which is what the dropped tree glyphs were
	// gesturing at and what the hint line otherwise says only in brackets.
	endpoint := strings.Join(renderSpendDrawer(tierSnap(), nil, usage.GroupEndpoint, "1h", 100), "\n")
	if !strings.Contains(endpoint, "BY ENDPOINT") {
		t.Errorf("the header does not follow the axis:\n%s", endpoint)
	}
}

// A CLAMPED SERIES SAYS SO ON THE ROW, in words at a width that has room for them and in the
// marker at every width.
//
// THIS IS THE LIVE HALF of the two notes moneyFigureFrom can emit, and it had no test through a
// screen: drawerFigures passes r.counts.Saturated straight through, so saturatedNote's wording is
// what an operator actually reads when a series total hits the int64 ceiling. The wording was
// asserted only through moneyFigure, which has no production caller at all — so the vocabulary was
// pinned on a path nothing renders while the rendered path was pinned by nothing.
//
// damagedNote is the other half and stays unreachable from here by construction: this call site
// passes nil for degraded, since a per-series damage figure is not something the aggregate carries.
// Its wording keeps its unit test and its comment says which side of the line it is on.
func TestRenderSpendDrawer_AClampedSeriesRowSaysItIsAFloor(t *testing.T) {
	snap := drawerSnap()
	for k, c := range snap.Buckets[0].Series {
		if k == "claude-opus-5" {
			c.Saturated = true
			snap.Buckets[0].Series[k] = c
		}
	}

	// THE MARKER AT EVERY WIDTH the drawer renders at, because it is one column and the fitter's
	// compact form keeps it — that is what makes it the disclosure of last resort.
	for _, width := range []int{90, 120, 200} {
		joined := strings.Join(renderSpendDrawer(snap, nil, usage.GroupModel, "TODAY", width), "\n")
		if !strings.Contains(joined, damagedMarker+"$11.12") {
			t.Errorf("width %d: the clamped row carries no %q on its figure:\n%s",
				width, damagedMarker, joined)
		}
	}

	// AND THE WORDS where there is room: 200 columns is the measured width at which the fitter
	// keeps the full form. Below that the marker carries it alone, which is the documented
	// degradation rather than a loss.
	joined := strings.Join(renderSpendDrawer(snap, nil, usage.GroupModel, "TODAY", 200), "\n")
	if !strings.Contains(joined, saturatedNote) {
		t.Errorf("a clamped series row does not say %q anywhere:\n%s", saturatedNote, joined)
	}
}

// CYCLING DROPS THE PREVIOUS SPAN'S ERROR, so no failure is ever captioned with a span that was
// not asked for.
//
// The error and the snapshot are stored together by applySpendLoaded, so a failed poll leaves err
// set and snap nil — and the diagnostic prints the CURRENT label, which `w` has already moved.
// Measured before the fix: a failed month poll followed by `w` printed "breakdown unavailable for
// LAST 1H: <the month's error>" for the whole round trip.
func TestCycleSpendDrawer_DropsThePreviousSpansError(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cycle func(m *model) tea.Cmd
	}{
		{"w", (*model).cycleSpendWindow},
		{"a", (*model).cycleSpendAxis},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &model{width: 200, height: 60, pane: paneSessions}
			m.sessionsTbl = newSessionsTable()
			m.spend.expanded = true
			m.spend.chains[spanHour].snap = drawerSnap()
			// The month's poll failed: snap nil and err set, which is what applySpendLoaded stores.
			m.spend.drawer.snap, m.spend.drawer.err = nil, errors.New("month poll exploded")
			m.layout()
			if !strings.Contains(m.paneView(), "month poll exploded") {
				t.Fatalf("setup: the failure is not on screen to begin with:\n%s", m.paneView())
			}

			tc.cycle(m)

			if m.spend.drawer.err != nil {
				t.Errorf("drawer.err survived the cycle: %v", m.spend.drawer.err)
			}
			if out := m.paneView(); strings.Contains(out, "month poll exploded") {
				t.Errorf("the previous span's failure is still captioned with the new span:\n%s", out)
			}
		})
	}
}

// THE DRAWER'S CADENCE IS ITS SPAN'S, and the two lists that make that possible stay in step.
//
// The breakdown sits directly under the band cell it breaks down, so a slower cadence means the two
// disagree about the same span for the difference between them. On one fixed five-minute interval
// the hour's breakdown could lag the hour's total — twenty seconds — by five minutes, which is the
// failure bandSpanCell's doc argues against, with nothing on screen saying so.
func TestSpendDrawer_PollsAtTheCadenceOfTheSpanItShows(t *testing.T) {
	// The two parallel lists first: windowSpan indexes into one and the request carries the other,
	// so a span inserted into either alone would point the cadence at a different window than the
	// one being fetched.
	if len(spendDrawerSpans) != len(spendDrawerWindows) {
		t.Fatalf("%d spans against %d windows", len(spendDrawerSpans), len(spendDrawerWindows))
	}
	for i, span := range spendDrawerSpans {
		if got := spendSpanDefs[span].window; got != spendDrawerWindows[i] {
			t.Errorf("index %d: span %d's window is %q, the window list says %q",
				i, span, got, spendDrawerWindows[i])
		}
	}

	// AND EVERY SELECTION REPORTS ITS OWN SPAN'S INTERVAL. Stepped through with the same keypress
	// an operator uses, so the wrap and the index resolution are covered too.
	m := &model{}
	for i := range spendDrawerWindows {
		want := spendSpanDefs[spendDrawerSpans[i]].pollInterval()
		if got := m.spend.pollInterval(); got != want {
			t.Errorf("step %d (%s): cadence %v, want its span's %v",
				i, m.spend.window(), got, want)
		}
		m.cycleSpendWindow()
	}

	// The hour and the month must not be the same number, or the assertion above would hold for one
	// constant applied to all four — which is the state this replaced.
	if spendSpanDefs[spanHour].pollInterval() == spendSpanDefs[spanMonth].pollInterval() {
		t.Error("the hour and the month poll at the same rate, so this test cannot see the defect")
	}
}

// A LATE DRAWER SAYS SO, on its own label, at its own span's threshold.
func TestDrawerLabels_ALateBreakdownCarriesItsAge(t *testing.T) {
	for _, tc := range []struct {
		name    string
		since   time.Duration
		wantAge bool
	}{
		{"just answered", time.Second, false},
		{"inside the threshold", 2*spendSpanDefs[spanHour].pollInterval() - time.Second, false},
		{"past it", 6 * time.Minute, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &model{width: 200, height: 60, pane: paneSessions}
			m.spend.drawer.snap = drawerSnap()
			m.spend.drawer.snap.Window = spendSpanDefs[spanHour].window
			m.spend.drawer.lastFetch = time.Now().Add(-tc.since)

			_, window := m.drawerLabels()
			base := spendSpanDefs[spanHour].label
			if got := window != base; got != tc.wantAge {
				t.Errorf("label = %q (base %q): carries an age = %v, want %v",
					window, base, got, tc.wantAge)
			}
		})
	}

	// AND NEVER BEFORE THE FIRST ANSWER: a zero lastFetch is "nothing has answered", which the
	// empty drawer already says. Rendered as an age it would read as a wedged chain on a drawer
	// that has simply just been opened.
	m := &model{width: 200, height: 60, pane: paneSessions}
	m.spend.drawer.snap = drawerSnap()
	m.spend.drawer.snap.Window = spendSpanDefs[spanHour].window
	if _, window := m.drawerLabels(); window != spendSpanDefs[spanHour].label {
		t.Errorf("label = %q before any poll answered, want the bare %q",
			window, spendSpanDefs[spanHour].label)
	}
}

// indentOf counts the leading spaces of a rendered line, in display columns.
func indentOf(line string) int {
	return len([]rune(line)) - len([]rune(strings.TrimLeft(line, " ")))
}

// BOTH headers sit over their own column.
//
// Measured, not eyeballed. The right one: fitStripFigures prepends its own `label + "  "`
// indent, so the series text sat three columns right of the header naming it — "BY MODEL" at
// column 36 against "claude-opus-5" at 39. The left one had the mirror defect and no test, so it
// outlived the fix — "WHERE IT WENT" at column 2 over tier rows starting at 0. A header over the
// wrong column is worse than none, on either side.
func TestRenderSpendDrawer_BothHeadersSitOverTheirColumns(t *testing.T) {
	for _, width := range []int{80, 100, 160, 200} {
		lines := renderSpendDrawer(tierSnap(), nil, usage.GroupModel, "1h", width)
		hdr, row := lines[0], lines[1]
		// THE LEFT COLUMN BY ITS INDENT, not by a tier name: the tier rows are ranked by cost,
		// so which label lands on the first row depends on the fixture's figures.
		if hi, ri := indentOf(hdr), indentOf(row); hi != ri {
			t.Errorf("width %d: the header is indented %d columns, the tier rows %d:\n%s",
				width, hi, ri, strings.Join(lines, "\n"))
		}
		for _, c := range []struct{ heading, value string }{
			{"BY MODEL", "claude-opus-5"},
		} {
			hi, ri := strings.Index(hdr, c.heading), strings.Index(row, c.value)
			if hi < 0 || ri < 0 {
				t.Fatalf("width %d: %q or %q missing:\n%s",
					width, c.heading, c.value, strings.Join(lines, "\n"))
			}
			hcol := len([]rune(hdr[:hi]))
			rcol := len([]rune(row[:ri]))
			if hcol != rcol {
				t.Errorf("width %d: %q starts at column %d, %q at %d:\n%s",
					width, c.heading, hcol, c.value, rcol, strings.Join(lines, "\n"))
			}
		}
	}
}

// The figures the band already shows are not repeated here.
//
// With one model in the window the old drawer restated the window total and the saving
// verbatim, which is what made it useless on a single-model deployment — the common case.
func TestRenderSpendDrawer_DoesNotRestateTheBandsFigures(t *testing.T) {
	snap := &usage.Snapshot{
		Window: "1h", Group: usage.GroupModel, Priced: true,
		Totals: usage.Counts{
			Requests: 35, CostMicros: 4_546_200, AvoidedMicros: 209_100,
			InputCostMicros: 3000, OutputCostMicros: 45000,
		},
		Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
			"claude-opus-5": {Requests: 35, CostMicros: 4_546_200, AvoidedMicros: 209_100,
				PricedRequests: 35, PriceableRequests: 35},
		}}},
	}
	joined := strings.Join(renderSpendDrawer(snap, nil, usage.GroupModel, "1h", 100), "\n")
	// The window total appears once — on the model row that earned it — and the tier column
	// carries shares of it rather than the figure again.
	if n := strings.Count(joined, "$4.55"); n > 1 {
		t.Errorf("the window total appears %d times in the panel:\n%s", n, joined)
	}
}

// No ORPHAN tree glyph. The original rule was "no glyphs at all", because every row
// they prefixed was top-level and the glyph implied a parent none of them had. The
// reasoning row is the first row in this panel that genuinely has one — output,
// directly above it — so the glyph is now allowed exactly where it tells the truth.
// The invariant is unchanged: a glyph must have its parent on the preceding line.
//
// "├" stays banned outright. It means "more siblings follow", and reasoning is the
// only child this panel has.
// BOTH FIXTURES, which is what lets this subsume the drawer's adjacency check: the glyph
// row's parent must be output whether or not a split was reported, and a regression that
// inserted the child at a fixed index passes on one fixture and fails on the other.
func TestRenderSpendDrawer_HasNoOrphanTreeGlyph(t *testing.T) {
	for _, tc := range []struct {
		name string
		snap *usage.Snapshot
	}{{"unreported split", tierSnap()}, {"reported split", reasoningSnap()}} {
		t.Run(tc.name, func(t *testing.T) {
			lines := renderSpendDrawer(tc.snap, nil, usage.GroupModel, "1h", 100)
			joined := strings.Join(lines, "\n")
			if strings.Contains(joined, "├") {
				t.Errorf("the panel draws \"├\", which claims a sibling follows:\n%s", joined)
			}
			// THE GLYPH MUST BE PRESENT BEFORE ITS PARENT IS CHECKED. The loop below skips
			// any row without a "└", so flattening childTierLabel to no glyph would make
			// every iteration skip and this test go green — the same dead-assertion shape
			// as drawnBarGlyphs' inverted rune range. Count first, then check.
			glyphRows := 0
			for _, l := range lines {
				if strings.Contains(l, "└") {
					glyphRows++
				}
			}
			if glyphRows == 0 {
				t.Fatalf("no row carries \"└\", so the parent check below cannot fail:\n%s", joined)
			}
			for i, l := range lines {
				if !strings.Contains(l, "└") {
					continue
				}
				if i == 0 {
					t.Errorf("row 0 carries \"└\" with nothing above it to be a child of:\n%s", joined)
					continue
				}
				if !strings.Contains(lines[i-1], "output") {
					t.Errorf("row %d carries \"└\" but the line above it is not output:\n%s", i, joined)
				}
			}
		})
	}
}

// Too narrow for two columns and the TIER column yields, so the panel degrades to exactly
// the per-model drawer that shipped before this feature. The addition gives way to the
// existing contract, never the reverse.
func TestRenderSpendDrawer_NarrowDropsTheTierColumnNotTheModels(t *testing.T) {
	joined := strings.Join(
		renderSpendDrawer(tierSnap(), nil, usage.GroupModel, "1h", spendDrawerTwoColumnMin-1), "\n")
	if !strings.Contains(joined, "claude-opus-5") {
		t.Errorf("the model column dropped below the two-column width:\n%s", joined)
	}
	if strings.Contains(joined, "cache-read") {
		t.Errorf("both columns drawn below the two-column width:\n%s", joined)
	}
}

// THE DRAWER'S CHAIN MUST OUTLIVE GOING OFF SCREEN, because m.spend.expanded does.
//
// The tick handler used to stop on !spendDrawerVisible(), conflating "closed" with "not on
// screen right now". They are different questions: expanded deliberately survives a move to a
// pane that cannot host the drawer and a resize below spendDrawerMinHeight — see the esc
// handler, which leaves the flag alone so returning finds the drawer as the operator left it.
//
// So: open on Sessions, press `u`, and the next tick returned without rescheduling. Nothing
// re-arms the chain but `$` itself, so coming back rendered the pre-switch snapshot forever —
// and the drawer has no age indicator, so it was silent.
func TestSpendDrawerTick_SurvivesGoingOffScreen(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(*model)
	}{
		{"moved to a pane that cannot host it", func(m *model) { m.pane = paneUsage }},
		{"resized below the drawer's height floor", func(m *model) { m.height = spendDrawerMinHeight - 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &model{width: 200, height: 60, pane: paneSessions}
			m.spend.expanded = true
			gen := m.spend.drawer.tickGen
			tc.break_(m)

			if m.spendDrawerVisible() {
				t.Fatal("the drawer is still visible, so this fixture asserts nothing")
			}
			if !m.spend.expanded {
				t.Fatal("expanded was cleared, so the chain SHOULD stop; fixture is wrong")
			}
			// The tick must still be accepted and rescheduled: it is the flag that says the
			// drawer is open, not whether it happens to be drawable this frame.
			_, cmd := m.Update(spendDrawerTickMsg{gen: gen})
			if cmd == nil {
				t.Error("the drawer's poll chain stopped and nothing re-arms it but `$`, so " +
					"returning to a hosting pane renders the pre-switch snapshot forever")
			}
		})
	}
	// And it DOES stop once the drawer is actually closed, or the off-screen case would keep a
	// ledger walk alive for the session.
	m := &model{width: 200, height: 60, pane: paneSessions}
	m.spend.expanded = false
	if _, cmd := m.Update(spendDrawerTickMsg{gen: m.spend.drawer.tickGen}); cmd != nil {
		t.Error("a closed drawer kept polling; its span can be a month of day files")
	}
}

// A POD SWITCH MUST NOT LEAVE AN EXPANDED DRAWER WITH NOTHING TO SHOW AND NOTHING COMING.
//
// startSpendPolling calls invalidate(), which walks every chain INCLUDING the drawer's — a
// different pod is a different breakdown just as much as a different total — and then restarted
// only the band's. So: open the drawer, esc to the pods picker, pick another pod, and the drawer
// came back expanded with snap == nil and nothing scheduled to fill it. Headers over blank rows,
// revivable only by closing and reopening.
func TestStartSpendPolling_ReArmsAnOpenDrawer(t *testing.T) {
	// THROUGH THE RETURNED BATCH, not by fabricating a tick. A hand-made spendDrawerTickMsg is
	// accepted and rescheduled by the handler whether or not anything ever scheduled one — so
	// asserting on it passed with the re-arm removed, which is the mutation this test exists to
	// catch. What has to be true is that startSpendPolling ITSELF issues the drawer's fetch.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"window":"1h","group":"model","buckets":[],"totals":{}}`))
	}))
	defer ts.Close()

	drawerFetched := func(m *model) bool {
		t.Helper()
		cmd := m.startSpendPolling()
		if cmd == nil {
			t.Fatal("startSpendPolling returned no command at all")
		}
		batch, ok := cmd().(tea.BatchMsg)
		if !ok {
			t.Fatalf("startSpendPolling produced %T, want a tea.BatchMsg", cmd())
		}
		// Every command at once, then ONE deadline. The batch holds fetches and tea.Tick
		// closures; the fetches answer against a loopback server in milliseconds while the
		// ticks block for their whole interval, so anything that has not answered by the
		// deadline is a tick and not the fetch under test. Fired concurrently rather than in
		// sequence because waiting out each tick in turn took sixteen seconds.
		msgs := make(chan tea.Msg, len(batch))
		for _, c := range batch {
			if c == nil {
				continue
			}
			go func(c tea.Cmd) {
				defer func() { _ = recover() }()
				msgs <- c()
			}(c)
		}
		deadline := time.After(3 * time.Second)
		for {
			select {
			case msg := <-msgs:
				if _, isDrawer := msg.(spendDrawerLoadedMsg); isDrawer {
					return true
				}
			case <-deadline:
				return false
			}
		}
	}

	open := &model{width: 200, height: 60, pane: paneSessions, client: apiclient.New(ts.URL)}
	open.spend.expanded = true
	open.spend.drawer.snap = drawerSnap()
	if !drawerFetched(open) {
		t.Error("startSpendPolling issued no drawer fetch for an OPEN drawer: invalidate() has " +
			"already cleared drawer.snap, so the drawer renders headers over blank rows " +
			"indefinitely and only `$` twice revives it")
	}
	// invalidate cleared the previous pod's breakdown, which is correct — it belonged to the
	// pod we just left.
	if open.spend.drawer.snap != nil {
		t.Error("the previous pod's breakdown survived the switch")
	}

	// A CLOSED drawer is not armed, or every session would pay for a chain nobody opened.
	closed := &model{width: 200, height: 60, pane: paneSessions, client: apiclient.New(ts.URL)}
	if drawerFetched(closed) {
		t.Error("startSpendPolling fetched a breakdown for a drawer nobody opened")
	}
}

// A FAILED DRAWER FETCH MUST NOT RENDER AS SILENCE, which is the rule the band already follows.
//
// applySpendDrawerLoaded clears snap on failure — deliberately, since a stale breakdown drawn as
// a current one is the worse error — so without an error path the drawer showed headers over
// blank rows and said nothing about why. drawer.err was written and had no reader at all.
func TestRenderSpendDrawer_AFailedFetchSaysSo(t *testing.T) {
	lines := renderSpendDrawer(nil, context.DeadlineExceeded, usage.GroupModel, "MONTH", 100)
	if len(lines) != spendDrawerLines {
		t.Fatalf("%d lines, want %d: the reservation has to be filled either way",
			len(lines), spendDrawerLines)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "unavailable") {
		t.Errorf("a failed drawer fetch renders no diagnostic:\n%s", joined)
	}
	if !strings.Contains(joined, "MONTH") {
		t.Errorf("the diagnostic does not name the span that failed:\n%s", joined)
	}
	// The keys still work, so they are still advertised: a failed span is when an operator most
	// wants to try another.
	if !strings.Contains(joined, "[w]") || !strings.Contains(joined, "esc") {
		t.Errorf("the hint line is gone, so the keys that recover from this are undiscoverable:\n%s",
			joined)
	}
	// And an empty drawer before the first poll is NOT the same state: no diagnostic there.
	fresh := strings.Join(renderSpendDrawer(nil, nil, usage.GroupModel, "MONTH", 100), "\n")
	if strings.Contains(fresh, "unavailable") {
		t.Errorf("a drawer awaiting its first answer reports a failure:\n%s", fresh)
	}
}

// CLOSING THE DRAWER DISOWNS ITS POLL CHAIN, BY EITHER KEY — and the two keys are asserted
// together because the bug was that they had drifted apart.
//
// `$` invalidated and esc did not. That was harmless only because the OPEN path also invalidates,
// for snapshot freshness, which is not about closing at all: a reply already in the air outlives
// the keypress by up to spendFetchTimeout, and without reqSeq moving it passes
// applySpendDrawerLoaded's guard and is stored against a closed drawer, so the next open renders a
// stale breakdown before its own first poll lands.
//
// reqSeq IS THE ASSERTION, not just snap. Clearing the snapshot without bumping the sequence leaves
// exactly that in-flight reply admissible, which is the half a "did it clear?" test would miss —
// and removing the esc-path call left the entire package green before this existed.
func TestClosingTheDrawer_DisownsItsPollChainByEitherKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  tea.KeyMsg
	}{
		{"dollar", keyRune('$')},
		{"esc", tea.KeyMsg{Type: tea.KeyEsc}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &model{width: 100, height: 40}
			m.pane = paneSessions
			m.handleKey(keyRune('$'))
			if !m.spendDrawerVisible() {
				t.Fatalf("the drawer did not open, so this says nothing about closing it")
			}
			// A breakdown on screen and a request in flight.
			m.spend.drawer.snap = &usage.Snapshot{Window: "month", Priced: true}
			m.spend.drawer.lastFetch = time.Now()
			seq := m.spend.drawer.reqSeq

			m.handleKey(tc.key)

			if m.spend.expanded {
				t.Fatalf("%q did not close the drawer", tc.name)
			}
			if m.spend.drawer.snap != nil {
				t.Errorf("%q left the breakdown behind, so the next open draws a stale one before "+
					"its own poll lands", tc.name)
			}
			if m.spend.drawer.reqSeq == seq {
				t.Errorf("%q closed the drawer without bumping reqSeq (%d): a reply already in "+
					"flight is still admissible and will be stored against a closed drawer",
					tc.name, seq)
			}
		})
	}
}

// THE TWO COLUMNS DO NOT TOUCH, at any width that draws both.
//
// There was never a gutter in this layout — `"  " + %-*s(tierColumnWidth) + series` — and it only
// looked like there was one because tierColumnWidth was 34 against tier rows that rendered 25
// columns. Nine columns of accidental padding. Deriving tierColumnWidth from the row's own parts
// removed the slack and the panel rendered "$56.51claude-opus-5": a money figure and a model name
// welded into one token, on a money surface.
//
// FOUND BY RENDERING THE PANEL, not by a test — every existing assertion looked at one column or
// the other, and the two were only ever composed in a code path nothing measured across. Hence this
// asserts the SEAM specifically, and sweeps widths so it cannot pass on one lucky terminal size.
func TestRenderSpendDrawer_TheColumnsDoNotTouch(t *testing.T) {
	snap := &usage.Snapshot{
		Window: "today", Priced: true,
		Totals: usage.Counts{
			Requests: 1755, CostMicros: 99_380_000,
			InputCostMicros: 2110, CacheWriteCostMicros: 25340,
			CacheReadCostMicros: 56510, OutputCostMicros: 15420,
		},
		Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
			"claude-opus-5": {
				Requests: 1187, Tokens: 137_000_000, CostMicros: 89_950_000,
				PricedRequests: 1187, PriceableRequests: 1187,
			},
		}}},
	}
	swept := 0
	for w := spendDrawerTwoColumnMin; w <= 200; w++ {
		for i, line := range renderSpendDrawer(snap, nil, usage.GroupModel, "TODAY", w) {
			// The seam is where the tier column ends. Only rows that actually carry a tier figure
			// can weld, so find one and check the columns either side of the boundary.
			at := strings.Index(line, "claude-opus-5")
			if at <= 0 {
				continue
			}
			swept++
			if line[at-1] != ' ' {
				t.Fatalf("width %d row %d: the series column runs straight into the tier column "+
					"at %d:\n%s", w, i, at, line)
			}
		}
	}
	if swept == 0 {
		t.Fatal("no row put the two columns together, so this asserted nothing")
	}
}

// THE SERIES COLUMN IS A TABLE: every row puts its figures in the same columns.
//
// It was a list of figures joined by a fixed gap, so each row's columns began wherever the previous
// figure happened to end — and the model name is the widest variable on the row. Measured on a live
// panel, "claude-opus-5" against "claude-sonnet-5" (two columns longer) put the two rows' money,
// request counts and token counts in six different places:
//
//	claude-opus-5   ~$89.95 (3 inexact)   1187 req   137M tokens   saved $4.40
//	claude-sonnet-5   $9.43   568 req   45M tokens
//
// Nothing there can be read down a column, which is the whole reason to stack rows.
//
// ASSERTED IN DISPLAY COLUMNS off the END of each figure, because right-aligned money is the point
// and because a byte offset is not a column — see TestRenderTierRows_MoneyIsRightAligned, where
// exactly that confusion made an already-aligned renderer look broken.
func TestDrawerFigures_RowsShareTheirColumns(t *testing.T) {
	row := func(label string, req, tokens int64) string {
		return fitStripFigures(" ", drawerFigures(drawerRow{
			label: label,
			counts: usage.Counts{
				Requests: req, Tokens: tokens, CostMicros: 11_121_400,
				PricedRequests: req, PriceableRequests: req,
			},
		}), 140)
	}
	// Labels of different widths, which is the variable that broke the alignment.
	short, long := row("claude-opus-5", 1187, 137_000_000), row("claude-sonnet-5", 568, 45_000_000)
	if short == long {
		t.Fatal("both rows rendered identically, so this test asserts nothing")
	}

	// endOf is the display column one past a substring.
	endOf := func(t *testing.T, line, sub string) int {
		t.Helper()
		at := strings.Index(line, sub)
		if at < 0 {
			t.Fatalf("row %q does not contain %q", line, sub)
		}
		return lipgloss.Width(line[:at]) + lipgloss.Width(sub)
	}

	// The money figure is identical on both rows by construction, so its column is comparable.
	money := formatUSDTotal(11.1214)
	if a, b := endOf(t, short, money), endOf(t, long, money); a != b {
		t.Errorf("the money column ends at %d on one row and %d on the other, so the figures do "+
			"not line up:\n%s\n%s", a, b, short, long)
	}
	// And so does everything after it: a label two columns longer must not push the rest along.
	for _, sub := range []string{"req", "tokens"} {
		if a, b := endOf(t, short, sub), endOf(t, long, sub); a != b {
			t.Errorf("%q ends at %d on one row and %d on the other:\n%s\n%s", sub, a, b, short, long)
		}
	}
}

// AN EMPTY MIDDLE COLUMN HOLDS ITS PLACE, which is the whole reason drawerFigures is positional.
//
// A row can be missing a middle figure and carry a later one: tool-prune removes prompt tokens from
// a request whose response could not be parsed, so a saving with no token count is a real row rather
// than a constructed one. If its empty slot collapses, the saving moves left into the tokens column
// and no longer lines up with the savings above it.
//
// FOUND AFTER THE COLUMNS SHIPPED, by measuring what a review comment about the note's width
// actually recovered — 3 columns where 14 were expected. The cause was that padLeft and padRight
// both return "" unchanged, so stripFigure.pad was holding no column open at all for an empty
// figure; every earlier test filled every slot, so nothing saw it.
func TestDrawerFigures_AnEmptyMiddleColumnHoldsItsPlace(t *testing.T) {
	row := func(label string, tokens int64) string {
		return fitStripFigures(" ", drawerFigures(drawerRow{
			label: label,
			counts: usage.Counts{
				Requests: 100, Tokens: tokens, CostMicros: 1_000_000,
				PricedRequests: 100, PriceableRequests: 100, AvoidedMicros: 500_000,
			},
		}), 140)
	}
	withTokens, without := row("model-a", 900_000), row("model-b", 0)

	saved := "saved " + formatUSDTotalMicros(500_000)
	at := func(t *testing.T, line string) int {
		t.Helper()
		i := strings.Index(line, saved)
		if i < 0 {
			t.Fatalf("row %q does not carry %q", line, saved)
		}
		return lipgloss.Width(line[:i])
	}
	if a, b := at(t, withTokens), at(t, without); a != b {
		t.Errorf("the saving starts at column %d with a token count and %d without it — the empty "+
			"tokens column collapsed instead of holding its place:\n%s\n%s", a, b, withTokens, without)
	}
	// Not vacuous: the two rows really do differ in whether the tokens column is filled.
	if !strings.Contains(withTokens, "tokens") || strings.Contains(without, "tokens") {
		t.Fatalf("the fixture no longer varies the tokens column:\n%s\n%s", withTokens, without)
	}

	// THE OTHER MIDDLE-EMPTY SHAPE: a priced row with no request count, so the REQUEST column is the
	// one held open under a token figure. Raised in review against the commit before pad learned to
	// pad an empty figure, where it rendered
	// "mcp-tool  $9.43       45M tokens" against "claude-sonnet-5  $9.43  568 req  45M tokens".
	priced := func(label string, req int64) string {
		return fitStripFigures(" ", drawerFigures(drawerRow{
			label: label,
			counts: usage.Counts{
				Requests: req, Tokens: 45_000_000, CostMicros: 9_430_000,
				PricedRequests: 568, PriceableRequests: req,
			},
		}), 140)
	}
	withReq, noReq := priced("claude-sonnet-5", 568), priced("mcp-tool", 0)
	tok := "45M tokens"
	tokAt := func(t *testing.T, line string) int {
		t.Helper()
		i := strings.Index(line, tok)
		if i < 0 {
			t.Fatalf("row %q does not carry %q", line, tok)
		}
		return lipgloss.Width(line[:i])
	}
	if a, b := tokAt(t, withReq), tokAt(t, noReq); a != b {
		t.Errorf("the token figure starts at column %d with a request count and %d without it — the "+
			"empty request column collapsed:\n%s\n%s", a, b, withReq, noReq)
	}
	if !strings.Contains(withReq, "568 req") || strings.Contains(noReq, "req") {
		t.Fatalf("the fixture no longer varies the request column:\n%s\n%s", withReq, noReq)
	}
}

// THE CAVEAT PROSE LEAVES THE MONEY COLUMN AND BECOMES THE ROW'S LAST FIGURE.
//
// "~$89.95 (3 inexact)" put a parenthesised sentence of variable length INSIDE a column every other
// row aligns against, so one inexact row knocked the whole table out of line — and the prose is
// what width pressure drops first anyway, which is precisely what a trailing figure is for. The
// band states the same rule for itself as MARKERS, NOT PROSE; the difference here is that the
// drawer has room to keep the words, just not in the middle of a column.
//
// The count is NOT lost, which is the half worth pinning: see
// TestSpendDrawerRows_StateHowManyFiguresAreInexact.
func TestDrawerFigures_TheCaveatIsTheLastFigure(t *testing.T) {
	figs := drawerFigures(drawerRow{
		label: "claude-opus-5",
		counts: usage.Counts{
			Requests: 40, Tokens: 900_000, CostMicros: 11_121_400,
			PricedRequests: 40, PriceableRequests: 40, IncompleteRequests: 3,
		},
	})
	if len(figs) < 2 {
		t.Fatalf("row produced %d figures", len(figs))
	}
	last := figs[len(figs)-1]
	if !strings.Contains(last.full, "3 inexact") {
		t.Errorf("the last figure is %q, want the caveat list", last.full)
	}
	// And no EARLIER figure carries it, or the prose is still inside a column.
	for i, f := range figs[:len(figs)-1] {
		if strings.Contains(f.full, "inexact") {
			t.Errorf("figure %d (%q) still carries the caveat prose", i, f.full)
		}
	}
	// The marker stays on the money figure: the glyph is the fact, the words are the explanation.
	if !strings.Contains(figs[1].full, inexactMarker) {
		t.Errorf("the money figure %q lost its marker along with the prose", figs[1].full)
	}
}

// THE ROW SAYS HOW MANY FIGURES ARE INEXACT, not just that some are.
//
// The glyph and the count are different claims: markMoney's "~" says "this total is a lower bound",
// the caveat says how much of the row is behind it — "3 inexact" on a series of forty requests is a
// blip, on a series of three it is the whole row. The drawer is the only surface with room for both,
// and it passes r.counts.IncompleteRequests for exactly that.
//
// UNASSERTED UNTIL NOW: the base covered it through two strip tests that went with the strip, and
// the band's tests see only the marker. Verified — dropping the `if incomplete > 0` block left the
// whole package green.
func TestSpendDrawerRows_StateHowManyFiguresAreInexact(t *testing.T) {
	figs := drawerFigures(drawerRow{
		label: "claude-opus-5",
		counts: usage.Counts{
			Requests: 40, CostMicros: 11_121_400,
			PricedRequests: 40, PriceableRequests: 40, IncompleteRequests: 3,
		},
	})
	var joined string
	for _, f := range figs {
		joined += f.full + " "
	}
	if !strings.Contains(joined, "3 inexact") {
		t.Errorf("row %q does not say how many of its figures are lower bounds", joined)
	}
	// And the marker is there too — they are not alternatives.
	if !strings.Contains(joined, inexactMarker) {
		t.Errorf("row %q carries the count without the marker on the figure", joined)
	}
	// A clean row says neither, so the count is a signal rather than furniture.
	clean := drawerFigures(drawerRow{
		label:  "claude-opus-5",
		counts: usage.Counts{Requests: 40, CostMicros: 11_121_400, PricedRequests: 40, PriceableRequests: 40},
	})
	var cleanJoined string
	for _, f := range clean {
		cleanJoined += f.full + " "
	}
	if strings.Contains(cleanJoined, "inexact") {
		t.Errorf("a row with nothing inexact still says so: %q", cleanJoined)
	}
}

// AND paneView READS drawer.err, which is the half no test reached.
//
// Every other test here passes an error straight to renderSpendDrawer, so they pin the RENDERER and
// say nothing about whether anything hands it the stored error. Verified: changing the call site to
// pass nil left the package green — the same data-half/renderer-half split that let a failed poll
// render as silence in the first place.
func TestPaneView_DrawsTheDrawersStoredError(t *testing.T) {
	m := &model{width: 120, height: 40}
	m.pane = paneSessions
	m.sessionsTbl = newSessionsTable()
	m.handleKey(keyRune('$'))
	if !m.spendDrawerVisible() {
		t.Fatalf("the drawer did not open, so this cannot say what paneView does with its error")
	}
	m.spend.drawer.snap = nil
	m.spend.drawer.err = errors.New("dial tcp: connection refused")
	m.layout()

	out := m.paneView()
	if !strings.Contains(out, "unavailable") {
		t.Errorf("paneView drew no diagnostic for a drawer whose poll failed — a stored error with "+
			"no reader is the defect renderSpendDrawer's error path exists to end:\n%s", out)
	}
}

// reasoningSnap is tierSnap with a reported reasoning split, so the drawer is
// exercised with a real child FIGURE rather than only the not-known cell. No other
// drawer fixture sets one, which is why the populated child was never rendered here.
func reasoningSnap() *usage.Snapshot {
	s := tierSnap()
	s.Totals.OutputTokens = 1593
	s.Totals.ReasoningTokens = 948
	s.Totals.PresentKinds = uint8(usage.KindOutput | usage.KindReasoning)
	return s
}

// TestRenderSpendDrawer_EmitsEveryTierPlusTheChild is the regression test for the
// bug this feature shipped and nothing caught: the assembly loop was bounded by
// numTierRows while the tier column had grown to tierPanelLines, so inserting the
// child DISPLACED a row instead of adding one. The ranking is by cost descending, so
// what fell off was the cheapest tier — `input` simply vanished from a panel still
// claiming to break down the whole bill.
//
// It was found by rendering the panel and reading it, not by a test. Only the line
// COUNT was pinned, and the count was still right: five left rows either way. Pinning
// the labels is what makes the next such regression fail here.
func TestRenderSpendDrawer_EmitsEveryTierPlusTheChild(t *testing.T) {
	for _, tc := range []struct {
		name string
		snap *usage.Snapshot
	}{
		{"reasoning reported", reasoningSnap()},
		{"reasoning unreported", tierSnap()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			joined := strings.Join(renderSpendDrawer(tc.snap, nil, usage.GroupModel, "1h", 100), "\n")
			// Every rate tier, by name. "input" last in the cost ranking is the one the
			// old bound dropped.
			for _, label := range []string{"input", "cache-write", "cache-read", "output"} {
				if !strings.Contains(joined, label) {
					t.Errorf("the panel omits the %q tier:\n%s", label, joined)
				}
			}
			// And the child, whatever it renders.
			if !strings.Contains(joined, "reasoning") {
				t.Errorf("the panel omits the reasoning child:\n%s", joined)
			}
		})
	}
}

// With a split reported, the drawer must show the child's FIGURE — the populated
// path, which no other drawer fixture reaches.
func TestRenderSpendDrawer_ChildCarriesItsFigure(t *testing.T) {
	var child string
	for _, l := range renderSpendDrawer(reasoningSnap(), nil, usage.GroupModel, "1h", 100) {
		if strings.Contains(l, "reasoning") {
			child = l
		}
	}
	if child == "" {
		t.Fatal("no reasoning row")
	}
	if strings.Contains(child, emptyCell) {
		t.Errorf("child row = %q shows the not-known cell despite a reported split", child)
	}
	// THE MICROS, not the rendered cents. `Contains(child, "$3.48")` passes on any figure
	// within ~5,000 micros of the right one — including one apportioned from
	// OutputCostMicros instead of output's DISPLAYED figure, which is the distinction
	// ApportionReasoning exists to make.
	//
	// 3_483_542 is read off the code, not derived here: two hand-derived versions of this
	// number were wrong by 479 and 740 micros, both invisible behind the rounded string.
	// Regenerate with ApportionReasoning(tiers[TierOutput]) on drawerTotals(reasoningSnap()).
	const wantMicros = 3_483_542
	tiers, ok := drawerTotals(reasoningSnap()).ApportionTiers()
	if !ok {
		t.Fatal("the fixture apportions to nothing")
	}
	got, has := drawerTotals(reasoningSnap()).ApportionReasoning(tiers[pricing.TierOutput])
	if !has || got != wantMicros {
		t.Errorf("ApportionReasoning = %d (has=%v), want %d", got, has, wantMicros)
	}
	if want := formatUSDTotalMicros(wantMicros); !strings.Contains(child, want) {
		t.Errorf("child row = %q, want the apportioned %s", child, want)
	}
}

// TestRenderSpendDrawer_NarrowHeightIsUnchangedByTheChildRow pins the claim the
// narrow path's own doc comment makes — "degrades to exactly the per-model drawer
// that shipped before" — as a LINE COUNT, which nothing checked.
//
// The reasoning child took the tier column from 4 rows to 5. Reserved
// unconditionally, that grew the one-column drawer too, which has no tier column at
// all: a narrow terminal permanently lost a body row to a child that cannot render
// there. The reservation is width-aware for exactly this reason.
func TestRenderSpendDrawer_NarrowHeightIsUnchangedByTheChildRow(t *testing.T) {
	narrow := spendDrawerTwoColumnMin - 1
	// The series column plus "(other)", the header, and the hint line — what shipped
	// before the tier column existed, and what must still ship at this width.
	want := spendDrawerSeries + 1 + 2
	for _, snap := range []*usage.Snapshot{reasoningSnap(), tierSnap()} {
		got := renderSpendDrawer(snap, nil, usage.GroupModel, "1h", narrow)
		if len(got) != want {
			t.Errorf("one-column drawer is %d lines, want %d:\n%s",
				len(got), want, strings.Join(got, "\n"))
		}
		// NO SECOND CHECK AGAINST spendDrawerLinesFor(narrow). The renderer pads to
		// exactly that, so it compares the renderer with its own padding rule and cannot
		// fail — the pattern the sanitize test rejects by name. `want` above is the
		// independent witness.
		// And no tier or child content leaked into the one-column form.
		if joined := strings.Join(got, "\n"); strings.Contains(joined, "reasoning") {
			t.Errorf("the one-column drawer draws the reasoning child:\n%s", joined)
		}
	}
	// Two columns still get the taller reservation, or the fix traded one bug for another.
	if spendDrawerLinesFor(spendDrawerTwoColumnMin) <= want {
		t.Errorf("two-column reservation %d is not taller than the one-column %d",
			spendDrawerLinesFor(spendDrawerTwoColumnMin), want)
	}
}

// THE README'S TWO-COLUMN THRESHOLD IS PINNED TO THE CONSTANT.
//
// It is a hand-written literal derived from seven constants, and it had already drifted
// once before this PR — the prose said 72 against an actual 84 — then this PR moved the
// real value to 86 by widening tierLabelWidth AND tierMoneyWidth for " └ reasoning". A
// number nothing checks will drift again on the next width change.
//
// MATCHED IN CONTEXT AND COMPARED, not searched for as a substring. A
// strings.Contains(readme, "86") version of this test is blind at the CURRENT value:
// "8693" and "186" appear elsewhere in the file and supply those digits, so the sentence
// could say anything and the test would still pass. A substring check closed this finding
// once without closing the gap, under a comment claiming it would fail when the constant
// moved.
func TestREADME_StatesTheCurrentTwoColumnThreshold(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	m := regexp.MustCompile(`Below (\d+) columns`).FindSubmatch(raw)
	if m == nil {
		t.Fatalf("cmd/abctl/README.md no longer says \"Below N columns\"; this test pins that " +
			"sentence against spendDrawerTwoColumnMin and cannot find it")
	}
	if got, want := string(m[1]), strconv.Itoa(spendDrawerTwoColumnMin); got != want {
		t.Errorf("README documents a %s-column threshold; spendDrawerTwoColumnMin is %s — "+
			"the drawer's documented width has drifted from the code", got, want)
	}
}
