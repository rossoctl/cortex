package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
	"github.com/rossoctl/cortex/authbridge/cmd/abctl/apiclient"
)

func TestRenderSpendStrip_NeverExceedsTheWidth(t *testing.T) {
	// The strip lives in the chrome. A line that overflows wraps, and a wrapped
	// chrome line costs a row of the table below it -- the same failure
	// fitHintLine exists to prevent.
	s := spendSummary{WindowUSD: 1.12, WindowLabel: "1h", BurnPerMin: 0.0187, Priced: true}
	for _, w := range []int{120, 100, 80, 64, 48, 32, 24, 16, 8, 1} {
		got := renderSpendStrip(s, w)
		if gw := lipgloss.Width(got); gw > w {
			t.Errorf("width %d: rendered %d columns: %q", w, gw, got)
		}
		if strings.Contains(got, "\n") {
			t.Errorf("width %d: strip contains a newline: %q", w, got)
		}
	}
}

func TestRenderSpendStrip_DropsWholeFiguresNeverClipsANumber(t *testing.T) {
	// #953: "no truncated numbers". A half-rendered dollar amount is worse than a
	// missing one -- it reads as a real, smaller figure.
	s := spendSummary{WindowUSD: 1.12, WindowLabel: "1h", BurnPerMin: 0.0187, Priced: true}
	for _, w := range []int{120, 100, 80, 64, 48, 32, 24, 16, 8} {
		got := renderSpendStrip(s, w)
		if got == "" {
			continue
		}
		// Any figure that appears at all must appear IN FULL, as formatUSDCell
		// actually renders it: "$1.1200", not "$1.12". Asserting the short form could
		// not detect clipping -- output truncated to "SPEND  $1.12" contains both
		// "$1.1" and "$1.12", so it passed, and only a clip to "$1.1" or shorter was
		// ever caught. The prefix probes stop just past the "$" so a figure clipped
		// anywhere in its digits still trips them.
		for prefix, whole := range map[string]string{
			"$1.": "$1.1200", // the window figure
			"$0.": "$0.0187", // the burn rate
		} {
			if strings.Contains(got, prefix) && !strings.Contains(got, whole) {
				t.Errorf("width %d: %q has a clipped %q figure (want the whole %q)", w, got, prefix, whole)
			}
		}
		if strings.HasSuffix(strings.TrimSpace(got), "$") {
			t.Errorf("width %d: %q ends mid-figure", w, got)
		}
	}
}

func TestRenderSpendStrip_TheClipAssertionCanActuallyFail(t *testing.T) {
	// Guards the guard above. That test is only worth anything if its assertion
	// fails on clipped input, so feed it clipped input directly. This is the defect
	// class that already bit this branch twice: a test that cannot detect the thing
	// it is named for.
	for _, clipped := range []string{"SPEND  $1.12", "SPEND  $1.1", "SPEND  $1.120"} {
		if strings.Contains(clipped, "$1.") && strings.Contains(clipped, "$1.1200") {
			t.Errorf("%q satisfied the whole-figure assertion; the clip test is blind to it", clipped)
		}
	}
	// ...and passes on the real, unclipped rendering, so it is not vacuously strict.
	if whole := "SPEND  $1.1200 /1h"; strings.Contains(whole, "$1.") && !strings.Contains(whole, "$1.1200") {
		t.Errorf("%q failed the whole-figure assertion; the clip test rejects correct output", whole)
	}
}

func TestRenderSpendStrip_WideEnoughShowsBothFigures(t *testing.T) {
	s := spendSummary{WindowUSD: 1.12, WindowLabel: "1h", BurnPerMin: 0.0187, Priced: true}
	got := renderSpendStrip(s, 120)
	for _, want := range []string{"SPEND", "$1.12", "1h", "/min"} {
		if !strings.Contains(got, want) {
			t.Errorf("strip %q missing %q", got, want)
		}
	}
}

func TestRenderSpendStrip_NarrowKeepsTheHeadlineFigure(t *testing.T) {
	// Degradation drops from the RIGHT: the leftmost figure is the headline and is
	// the last thing to go. (fitHintLine drops from the front for the opposite
	// reason -- its essential hints are last.)
	s := spendSummary{WindowUSD: 1.12, WindowLabel: "1h", BurnPerMin: 0.0187, Priced: true}
	got := renderSpendStrip(s, 24)
	if !strings.Contains(got, "$1.12") {
		t.Errorf("narrow strip %q dropped the headline figure", got)
	}
	if strings.Contains(got, "/min") {
		t.Errorf("narrow strip %q kept the burn rate; it should drop before the headline", got)
	}
}

func TestRenderSpendStrip_UnpricedSaysSoAndNeverShowsZero(t *testing.T) {
	s := spendSummary{WindowLabel: "1h", Priced: false, Unpriced: 12, Priceable: 318}
	got := renderSpendStrip(s, 120)
	if !strings.Contains(got, "unavailable") {
		t.Errorf("strip %q does not say cost is unavailable", got)
	}
	if strings.Contains(got, "$0.00") {
		t.Errorf("strip %q renders $0.00 for an unknown cost", got)
	}
}

func TestRenderSpendStrip_PartiallyPricedDisclosesTheGap(t *testing.T) {
	// A dollar total covering only the priced subset must say so; presenting a
	// subtotal as the whole spend is the failure the coverage counters exist for.
	s := spendSummary{
		WindowUSD: 4.17, WindowLabel: "1h", BurnPerMin: 0.07,
		Priced: true, Unpriced: 12, Priceable: 318,
	}
	got := renderSpendStrip(s, 120)
	if !strings.Contains(got, "12") {
		t.Errorf("strip %q does not disclose the 12 unpriced requests", got)
	}
}

func TestRenderSpendStrip_FullyPricedIsNotAnnotated(t *testing.T) {
	// A correctly configured deployment must not carry a permanent warning; that
	// is what trains an operator to ignore the one signal that matters.
	s := spendSummary{
		WindowUSD: 4.17, WindowLabel: "1h", BurnPerMin: 0.07,
		Priced: true, Unpriced: 0, Priceable: 318,
	}
	got := renderSpendStrip(s, 120)
	if strings.Contains(got, "unpriced") {
		t.Errorf("fully priced strip %q still warns about coverage", got)
	}
}

func TestRenderSpendStrip_NoDataYetRendersNothingUseful(t *testing.T) {
	// Before the first poll returns. An empty strip is honest; "$0.00" is not.
	got := renderSpendStrip(spendSummary{}, 120)
	if strings.Contains(got, "$0.00") {
		t.Errorf("strip %q renders $0.00 before any data arrived", got)
	}
}

func TestRenderSpendStrip_NoSavedFigureWhenUnmeasured(t *testing.T) {
	s := spendSummary{WindowUSD: 1.12, WindowLabel: "1h", BurnPerMin: 0.0187, Priced: true, HasSaved: false}
	if got := renderSpendStrip(s, 120); strings.Contains(got, "saved") {
		t.Errorf("strip %q shows a saved figure with nothing measuring it", got)
	}
}

func TestRenderSpendStrip_ShowsSavedWhenMeasured(t *testing.T) {
	// Proves the field is wired now so a later commit adds data, not a branch.
	s := spendSummary{
		WindowUSD: 1.12, WindowLabel: "1h", BurnPerMin: 0.0187, Priced: true,
		SavedUSD: 0.24, HasSaved: true,
	}
	got := renderSpendStrip(s, 120)
	if !strings.Contains(got, "saved") || !strings.Contains(got, "$0.24") {
		t.Errorf("strip %q does not show the measured saving", got)
	}
}

func TestRenderSpendStrip_ShowsTodayWhenAvailable(t *testing.T) {
	s := spendSummary{
		WindowUSD: 1.12, WindowLabel: "1h", BurnPerMin: 0.0187, Priced: true,
		TodayUSD: 4.17, HasToday: true,
	}
	got := renderSpendStrip(s, 120)
	if !strings.Contains(got, "today") || !strings.Contains(got, "$4.17") {
		t.Errorf("strip %q does not show today's total", got)
	}
	// Today becomes the headline when present, so it must survive a narrow width.
	if narrow := renderSpendStrip(s, 24); !strings.Contains(narrow, "$4.17") {
		t.Errorf("narrow strip %q dropped today's total, which is the headline", narrow)
	}
}

func TestRenderSpendStrip_WideCharacterSafety(t *testing.T) {
	// footer.go:88-92 records the bug: a budget computed in display columns but
	// sliced by rune index overflowed on any wide character. The strip's own chrome is
	// ASCII, but the width arithmetic must be column-based regardless.
	//
	// It is not hypothetical either: WindowLabel is server-supplied and reaches the line
	// verbatim whenever parseWindowSpan cannot read it as a duration (spend.go), so the wide
	// character can arrive off the wire. The ASCII case alone proved only the loop bound —
	// this test was named for behaviour it never exercised. Each CJK glyph below is TWO
	// display columns and ONE rune, so any len()- or rune-based budget renders wider than it
	// claims and these cases catch it.
	for _, tc := range []struct {
		name string
		s    spendSummary
	}{
		{"ascii", spendSummary{WindowUSD: 1.12, WindowLabel: "1h", BurnPerMin: 0.0187, Priced: true}},
		{"cjk window label", spendSummary{WindowUSD: 1.12, WindowLabel: "一時間", BurnPerMin: 0.0187, Priced: true}},
		{"cjk label under a today headline", spendSummary{
			WindowUSD: 1.12, WindowLabel: "過去一時間", BurnPerMin: 0.0187, Priced: true,
			TodayUSD: 4.17, HasToday: true,
		}},
		{"cjk in the unpriced path", spendSummary{
			WindowLabel: "過去一時間", HasSnapshot: true, Unpriced: 12, Priceable: 318,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for w := 1; w <= 60; w++ {
				got := renderSpendStrip(tc.s, w)
				if gw := lipgloss.Width(got); gw > w {
					t.Fatalf("width %d: rendered %d columns: %q", w, gw, got)
				}
				if strings.Contains(got, "\n") {
					t.Fatalf("width %d: strip contains a newline: %q", w, got)
				}
			}
		})
	}
}

// The strip's width guarantee is expressed in lipgloss.Width, and lipgloss.Width measures
// the WIDEST LINE: lipgloss.Width("abc\nabcdef") is 6, not 10. So a WindowLabel carrying a
// newline sails through fitStripFigures' budget check and renderSpendStrip returns a
// TWO-LINE string — the exact "a wrapped chrome line costs a row of the table below it"
// failure its own godoc opens with.
//
// Reachable, not theoretical: spendSummary copies snap.Window verbatim when parseWindowSpan
// cannot read it as a duration, and that field is server-supplied JSON. Fixed at the
// boundary, in spendSummary, rather than guarded at each use — a wire string is sanitised
// once, where it enters. Asserted here because this is where the consequence shows.
func TestSpendSummary_SanitisesTheServersWindowLabel(t *testing.T) {
	m := &model{}
	m.spend.snap = &usage.Snapshot{
		Window: "1h\nEVIL",
		Totals: usage.Counts{Requests: 1, CostMicros: 1_120_000, PricedRequests: 1, PriceableRequests: 1},
		Priced: true,
	}

	s := m.spendSummary()
	// Errorf, not Fatalf: the label check is the cause and the loop below is the
	// consequence, and a reader of a failure wants both. Stopping at the cause would let
	// someone "fix" this by trimming the label in the renderer and never learn that the
	// two-line output was the thing that mattered.
	if strings.ContainsAny(s.WindowLabel, "\n\r\x1b") {
		t.Errorf("WindowLabel = %q still carries a control character straight off the wire", s.WindowLabel)
	}
	for _, w := range []int{120, 100, 80, 64, 48, 32, 24, 16, 8, 1} {
		got := renderSpendStrip(s, w)
		if strings.Contains(got, "\n") {
			t.Errorf("width %d: strip rendered two lines and stole a row from the table: %q", w, got)
		}
		if gw := lipgloss.Width(got); gw > w {
			t.Errorf("width %d: rendered %d columns: %q", w, gw, got)
		}
	}
}

// An escape sequence is the same defect with a worse payload: it can reposition the cursor,
// recolour the pane, or erase the coverage warning it is rendered beside. CWE-150, the same
// reason sanitizeLabel exists for the usage pane's model keys.
func TestSpendSummary_NeutralisesAnEscapeSequenceInTheWindowLabel(t *testing.T) {
	m := &model{}
	m.spend.snap = &usage.Snapshot{
		Window: "1h\x1b[2J\x1b[H",
		Totals: usage.Counts{Requests: 1, CostMicros: 1_120_000, PricedRequests: 1, PriceableRequests: 1},
		Priced: true,
	}

	s := m.spendSummary()
	if strings.Contains(s.WindowLabel, "\x1b") {
		t.Errorf("WindowLabel = %q carries an ESC; it will be written straight to the terminal", s.WindowLabel)
	}
	if got := renderSpendStrip(s, 120); strings.Contains(got, "\x1b[2J") {
		t.Errorf("strip %q clears the screen on behalf of the server", got)
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

func TestLayout_ReservesExactlyOneRowForTheStrip(t *testing.T) {
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

	if want := 40 - 4; tall.bodyHeight != want {
		t.Errorf("bodyHeight with strip = %d, want %d (height - title - strip - 2 footer rows)",
			tall.bodyHeight, want)
	}
	if want := 19 - 3; short.bodyHeight != want {
		t.Errorf("bodyHeight below the fold = %d, want %d (no strip row)", short.bodyHeight, want)
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

	if want := 40 - 4; picker.bodyHeight != want {
		t.Errorf("picker bodyHeight = %d, want %d: the reservation must not depend on the pane",
			picker.bodyHeight, want)
	}
	if picker.spendStripVisible() {
		t.Error("the picker reserves the row but must not DRAW the strip")
	}
}

func TestPaneView_DrawsTheStrip(t *testing.T) {
	// The mutation guard for task 3 step 7. A renderSpendStrip call that nothing
	// asserts on is the failure mode here: the strip would be written, reviewed,
	// and never reach the screen. Deleting the append in paneView must fail this.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := New(ctx, apiclient.New("http://127.0.0.1:1")).(*model)
	m.width, m.height = 100, 40
	m.layout()
	m.spend.snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{
			Requests: 10, CostMicros: 1_120_000,
			PricedRequests: 10, PriceableRequests: 10,
		},
		Priced: true,
	}

	got := m.paneView()
	if !strings.Contains(got, stripLabel) {
		t.Errorf("paneView output has no %q row; the strip is not wired to the screen", stripLabel)
	}
	if !strings.Contains(got, "$1.12") {
		t.Error("paneView output has no spend figure; the strip row is rendered from something other than spendSummary")
	}
	// The strip must be the SECOND row, directly under the title bar. "In the
	// chrome, read before the data" is the whole requirement -- a strip rendered
	// below the table is just the per-row cost figure again, one pane over.
	lines := strings.Split(got, "\n")
	if len(lines) < 2 {
		t.Fatalf("paneView rendered %d lines; expected at least a title and a strip", len(lines))
	}
	if !strings.Contains(lines[1], stripLabel) {
		t.Errorf("row 1 is %q, want the %q strip directly under the title", lines[1], stripLabel)
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
	m.spend.snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{Requests: 1, CostMicros: 1_120_000, PricedRequests: 1, PriceableRequests: 1},
		Priced: true,
	}

	if got := m.paneView(); strings.Contains(got, stripLabel) {
		t.Errorf("19-row terminal drew the strip row it did not reserve: %q", got)
	}
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
		m.spend.snap = &usage.Snapshot{
			Window: "1h",
			Totals: usage.Counts{Requests: 1, CostMicros: 1_120_000, PricedRequests: 1, PriceableRequests: 1},
			Priced: true,
		}
		if got := m.paneView(); strings.Contains(got, stripLabel) {
			t.Errorf("pane %v drew the strip: %q", p, got)
		}
		cancel()
	}
}

func TestRenderSpendStrip_FailedPollSaysUnavailableNotNothing(t *testing.T) {
	// Finding 2. A failing /v1/usage used to render "" on every poll forever, while
	// layout() went on reserving the row: a permanent blank line above the footer
	// and no diagnostic anywhere on screen. Silence is the one unacceptable answer,
	// because the row is spent either way.
	s := spendSummary{Failed: true}
	got := renderSpendStrip(s, 120)

	if got == "" {
		t.Fatal("a failed poll rendered nothing; the reserved row becomes a permanent blank line")
	}
	if !strings.Contains(got, "unavailable") {
		t.Errorf("strip %q does not say cost is unavailable", got)
	}
	if strings.Contains(got, "$") {
		t.Errorf("strip %q shows a dollar amount for a poll that never answered", got)
	}
}

func TestRenderSpendStrip_FailedPollFitsEveryWidth(t *testing.T) {
	// The failure path is the one an operator sees for as long as the endpoint is
	// broken, so it must obey the width contract like any other.
	s := spendSummary{Failed: true}
	for w := 1; w <= 80; w++ {
		got := renderSpendStrip(s, w)
		if gw := lipgloss.Width(got); gw > w {
			t.Fatalf("width %d: rendered %d columns: %q", w, gw, got)
		}
		if strings.Contains(got, "\n") {
			t.Fatalf("width %d: strip contains a newline: %q", w, got)
		}
	}
}

func TestRenderSpendStrip_FailedPollOutranksTheNoDataPath(t *testing.T) {
	// Failed must not be inferred from the counters. A failed poll has none, so if
	// the renderer reached the Priceable == 0 branch it would return "" -- which is
	// exactly the bug. Pin the precedence.
	failed := renderSpendStrip(spendSummary{Failed: true}, 120)
	quiet := renderSpendStrip(spendSummary{}, 120)

	if failed == quiet {
		t.Errorf("a failed poll renders identically to no-data-yet (%q); the two are different answers", failed)
	}
	if quiet != "" {
		t.Errorf("no-data-yet rendered %q, want the empty string", quiet)
	}
}

func TestRenderSpendStrip_SettledZeroShowsZeroNotUnavailable(t *testing.T) {
	// Finding 3, the half that was unpinned. A window that WAS priced and cost
	// exactly nothing is a known answer: the gateway declared the traffic free.
	// authlib/usage asserts "a settled zero IS priced", and formatUSDCell renders
	// "<$0.0001" for any positive amount under the floor -- so "$0.0000" in the
	// strip can only ever mean an exact, settled zero.
	//
	// Without this test, someone could "fix" the zero into an unavailable branch
	// and the suite would stay green, silently conflating free traffic with
	// unmeasured traffic -- the precise distinction the strip exists to draw.
	s := spendSummary{WindowUSD: 0, WindowLabel: "1h", Priced: true, Priceable: 10}
	got := renderSpendStrip(s, 120)

	if got == "" {
		t.Fatal("a settled zero rendered nothing; free traffic is a real, knowable answer")
	}
	if strings.Contains(got, "unavailable") {
		t.Errorf("strip %q reports a SETTLED zero as unavailable; that conflates free with unknown", got)
	}
	if !strings.Contains(got, "$0.0000") {
		t.Errorf("strip %q does not state the settled zero as an amount", got)
	}
	// And it must be distinguishable from the unknown case, which is the whole point.
	if unknown := renderSpendStrip(spendSummary{WindowLabel: "1h", Priceable: 10}, 120); got == unknown {
		t.Errorf("a settled zero and an unknown cost render identically as %q", got)
	}
}

func TestRenderSpendStrip_NoPriceableTrafficSaysSoRatherThanNothing(t *testing.T) {
	// A poll answered and found no inference traffic at all. That is a finding, not
	// an absence: the row is reserved on height alone, so rendering "" here buys a
	// blank line above the footer that reads as a broken UI.
	s := spendSummary{WindowLabel: "1h", HasSnapshot: true, Priceable: 0}
	got := renderSpendStrip(s, 120)

	if got == "" {
		t.Fatal("a window with no priceable traffic rendered nothing, wasting its reserved row")
	}
	if !strings.Contains(got, "no priceable traffic") {
		t.Errorf("strip %q does not say there was no priceable traffic", got)
	}
	if strings.Contains(got, "$") {
		t.Errorf("strip %q shows a dollar amount for a window with nothing to price", got)
	}
	if strings.Contains(got, "unavailable") {
		t.Errorf("strip %q says cost is unavailable; the cost is knowable and there simply was none to price", got)
	}
}

func TestRenderSpendStrip_BeforeTheFirstPollStaysSilent(t *testing.T) {
	// The one case where "" is right. "We have not looked" is honest, brief and
	// self-correcting within a poll interval -- and it must stay distinguishable
	// from "we looked and found nothing to price", which is the test above.
	quiet := renderSpendStrip(spendSummary{}, 120)
	if quiet != "" {
		t.Errorf("pre-first-poll rendered %q, want the empty string", quiet)
	}

	looked := renderSpendStrip(spendSummary{WindowLabel: "1h", HasSnapshot: true}, 120)
	if looked == quiet {
		t.Error("'not looked yet' and 'looked, nothing priceable' render identically")
	}
}

func TestRenderSpendStrip_NoPriceableTrafficFitsEveryWidth(t *testing.T) {
	// It is a steady state for a proxy handling only non-LLM traffic, so it obeys
	// the width contract like every other branch.
	s := spendSummary{WindowLabel: "1h", HasSnapshot: true}
	for w := 1; w <= 80; w++ {
		got := renderSpendStrip(s, w)
		if gw := lipgloss.Width(got); gw > w {
			t.Fatalf("width %d: rendered %d columns: %q", w, gw, got)
		}
		if strings.Contains(got, "\n") {
			t.Fatalf("width %d: strip contains a newline: %q", w, got)
		}
	}
}

func TestRenderSpendStrip_UnpricedGapStillOutranksTheNoTrafficLine(t *testing.T) {
	// Priceable > 0 with nothing priced is a COVERAGE problem and must keep saying
	// so; the no-traffic line is only for Priceable == 0. Pins the precedence so
	// the new branch cannot swallow the coverage warning.
	s := spendSummary{WindowLabel: "1h", HasSnapshot: true, Unpriced: 12, Priceable: 318}
	got := renderSpendStrip(s, 120)

	if strings.Contains(got, "no priceable traffic") {
		t.Errorf("strip %q claims no priceable traffic while reporting 318 priceable requests", got)
	}
	if !strings.Contains(got, "unavailable") {
		t.Errorf("strip %q lost the coverage warning", got)
	}
}

// A LATENT bug, armed by the commit that first sets HasToday. The "nothing was
// priced" branch is guarded on `!s.Priced && !s.HasToday`, so a summary carrying a
// today figure over a window that priced nothing falls through to the figures
// path — where the window figure was rendered unconditionally and read "$0.0000
// /1h". That states a settled zero for a cost nobody knows, which is the one thing
// this whole feature forbids, and it is exactly the misreading formatUSDCell's
// floor and the strip's "cost unavailable" branch both exist to prevent.
//
// Unreachable while HasToday is never set, which is why it survived review twice.
// Pinned here rather than left for the ledger commit to trip over: a test is the
// only artefact that a later author cannot skip reading.
func TestRenderSpendStrip_TodayWithAnUnpricedWindowStatesNoWindowZero(t *testing.T) {
	s := spendSummary{
		TodayUSD: 4.17, HasToday: true,
		WindowUSD: 0, WindowLabel: "1h", Priced: false,
		HasSnapshot: true, Priceable: 40, Unpriced: 40,
	}
	got := renderSpendStrip(s, 120)

	if strings.Contains(got, "$0.0000 /1h") {
		t.Errorf("strip %q reports an UNPRICED window as a settled $0.0000", got)
	}
	// The today figure is the one thing here that IS known, so it must survive.
	if !strings.Contains(got, "4.17") {
		t.Errorf("strip %q dropped the today figure, which is the only known cost", got)
	}
	// And the coverage gap still qualifies the total.
	if !strings.Contains(got, "40 of 40 unpriced") {
		t.Errorf("strip %q lost the coverage warning for an unpriced window", got)
	}
}

// THE critical finding: a partial day published as a complete total.
//
// One priced request out of four hundred. The dollar figure is real and the day's real
// cost is unknown and far larger, and the strip printed "$0.0031 today" with no marker
// anywhere on the line — the branch's headline figure, presented as settled. Asserted at
// EVERY width, because the marker's whole justification is that it is one column and so
// cannot be squeezed out: wherever the figure appears, the fact that it is partial
// appears with it.
func TestRenderSpendStrip_APartialTodayIsNeverPublishedAsComplete(t *testing.T) {
	s := spendSummary{
		TodayUSD: 0.0031, HasToday: true, TodayUnpriced: 399, TodayPriceable: 400,
		WindowUSD: 1.12, WindowLabel: "1h", BurnPerMin: 0.0187, Priced: true,
		HasSnapshot: true, Priceable: 400,
	}
	for w := 1; w <= 200; w++ {
		got := renderSpendStrip(s, w)
		if !strings.Contains(got, "$0.0031") {
			// Dropped whole, which is the honest degradation. It is the figure appearing
			// UNQUALIFIED that is forbidden.
			continue
		}
		if !strings.Contains(got, "$0.0031"+partialMarker) {
			t.Fatalf("width %d: %q states today's partial total as a complete one", w, got)
		}
	}
	// Where there is room, the caveat is spelled out — and in TODAY's own numbers, not
	// the hour's.
	wide := renderSpendStrip(s, 200)
	if !strings.Contains(wide, "399 of 400 unpriced") {
		t.Errorf("wide strip %q does not disclose the day's own coverage gap", wide)
	}
}

// The second verified misread, at the renderer: "SPEND $4.1700 today  $1.1200 /1h
// 40 of 40 unpriced" — a warning that reads as qualifying the DAY and describes the
// HOUR. Here the day is complete and the hour has the gap, so the day must carry no
// marker and the gap must be attached to the figure it is about.
func TestRenderSpendStrip_TheHoursGapDoesNotQualifyTheDay(t *testing.T) {
	s := spendSummary{
		TodayUSD: 4.17, HasToday: true, TodayUnpriced: 0, TodayPriceable: 318,
		WindowUSD: 1.12, WindowLabel: "1h", Priced: true,
		HasSnapshot: true, Unpriced: 40, Priceable: 40,
	}
	got := renderSpendStrip(s, 200)

	if !strings.Contains(got, "$4.1700 today") {
		t.Errorf("strip %q does not state the day's complete total plainly", got)
	}
	if strings.Contains(got, "$4.1700"+partialMarker) {
		t.Errorf("strip %q marks a fully priced day as partial", got)
	}
	if !strings.Contains(got, "$1.1200"+partialMarker+" /1h (40 of 40 unpriced)") {
		t.Errorf("strip %q does not attach the hour's gap to the hour's own figure", got)
	}
	// And the old shape must be gone: an unlabelled coverage note at the end of the line
	// is the misattribution itself.
	if strings.HasSuffix(got, "40 of 40 unpriced") {
		t.Errorf("strip %q still trails an unlabelled coverage note after the day's figure", got)
	}
}

// A window that priced NOTHING has no figure for its gap to ride on, and the gap still
// has to be stated. It wears the window's label, so it cannot be read as qualifying the
// today figure beside it.
func TestRenderSpendStrip_ASuppressedWindowsGapWearsTheWindowsLabel(t *testing.T) {
	s := spendSummary{
		TodayUSD: 4.17, HasToday: true, TodayPriceable: 318,
		WindowLabel: "1h", Priced: false,
		HasSnapshot: true, Unpriced: 40, Priceable: 40,
	}
	got := renderSpendStrip(s, 200)

	if !strings.Contains(got, "40 of 40 unpriced /1h") {
		t.Errorf("strip %q does not name the window the 40-request gap belongs to", got)
	}
	if strings.Contains(got, "$4.1700"+partialMarker) {
		t.Errorf("strip %q marked the day partial from the HOUR's gap", got)
	}
}

// A truncated stream's floor must never be published as an exact total.
//
// That is the title of a commit on this branch, and the strip ignored it: the figure
// went out as "$4.1700 today" and "$1.1200 /1h" to four decimal places, with no
// annotation, for a total the aggregator itself reports as a lower bound. Asserted at
// every width for the reason the partial marker is: one column is exactly what it takes
// to make the qualification undroppable.
func TestRenderSpendStrip_AnInexactTotalIsMarkedAtEveryWidth(t *testing.T) {
	s := spendSummary{
		TodayUSD: 4.17, HasToday: true, TodayPriceable: 318, TodayIncomplete: 4,
		WindowUSD: 1.12, WindowLabel: "1h", BurnPerMin: 0.0187, Priced: true,
		HasSnapshot: true, Priceable: 10, Incomplete: 3,
	}
	for w := 1; w <= 200; w++ {
		got := renderSpendStrip(s, w)
		for amount, label := range map[string]string{"$4.1700": "today", "$1.1200": "the hour"} {
			if !strings.Contains(got, amount) {
				continue
			}
			if !strings.Contains(got, inexactMarker+amount) {
				t.Fatalf("width %d: %q states %s's inexact total as an exact figure", w, got, label)
			}
		}
	}
	wide := renderSpendStrip(s, 200)
	if !strings.Contains(wide, "4 inexact") {
		t.Errorf("wide strip %q does not say how many of the day's figures are inexact", wide)
	}
	if !strings.Contains(wide, "3 inexact") {
		t.Errorf("wide strip %q does not say how many of the hour's figures are inexact", wide)
	}
}

// The other half: an exact total carries no marker. Without this the fix could be
// "made to pass" by marking everything, which is the same as marking nothing.
func TestRenderSpendStrip_AnExactTotalCarriesNoInexactMarker(t *testing.T) {
	s := spendSummary{
		TodayUSD: 4.17, HasToday: true, TodayPriceable: 318,
		WindowUSD: 1.12, WindowLabel: "1h", Priced: true, HasSnapshot: true, Priceable: 10,
	}
	got := renderSpendStrip(s, 200)

	if strings.Contains(got, inexactMarker+"$4.1700") || strings.Contains(got, inexactMarker+"$1.1200") {
		t.Errorf("strip %q marks an exact total as inexact", got)
	}
	if strings.Contains(got, "inexact") {
		t.Errorf("strip %q carries an exactness caveat with nothing to act on", got)
	}
}

// Exactness and coverage are different claims about the same number and both can be
// true at once, so a figure that is both must say both — and in the order cmd_cost.go
// fixed: the figure itself first, then what it covers.
func TestRenderSpendStrip_AFigureCanBeBothInexactAndPartial(t *testing.T) {
	s := spendSummary{
		TodayUSD: 0.0031, HasToday: true, TodayUnpriced: 399, TodayPriceable: 400, TodayIncomplete: 2,
		HasSnapshot: true,
	}
	got := renderSpendStrip(s, 200)

	if !strings.Contains(got, inexactMarker+"$0.0031"+partialMarker+" today") {
		t.Errorf("strip %q does not carry both markers on the figure they qualify", got)
	}
	if !strings.Contains(got, "(2 inexact, 399 of 400 unpriced)") {
		t.Errorf("strip %q does not state both caveats, exactness first", got)
	}
	// And both markers survive the compact form, which is what a narrow terminal gets.
	narrow := renderSpendStrip(s, 24)
	if !strings.Contains(narrow, inexactMarker+"$0.0031"+partialMarker) {
		t.Errorf("narrow strip %q dropped a marker; the figure now reads as settled", narrow)
	}
}

// The width matrix, with both caveats live. The strip's contract is one line, never
// wider than its budget, and no clipped number — and a caveat is the newest thing that
// can break it. The CJK label is two display columns per rune and one rune, so any
// len()- or rune-based arithmetic in the caveat path renders wider than it claims.
func TestRenderSpendStrip_CaveatsObeyTheWidthContract(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    spendSummary
	}{
		{"partial day and partial hour", spendSummary{
			TodayUSD: 0.0031, HasToday: true, TodayUnpriced: 399, TodayPriceable: 400,
			WindowUSD: 1.12, WindowLabel: "1h", BurnPerMin: 0.0187, Priced: true,
			HasSnapshot: true, Unpriced: 12, Priceable: 318,
			SavedUSD: 0.24, HasSaved: true,
		}},
		{"every figure both inexact and partial", spendSummary{
			TodayUSD: 0.0031, HasToday: true, TodayUnpriced: 399, TodayPriceable: 400, TodayIncomplete: 7,
			WindowUSD: 1.12, WindowLabel: "1h", BurnPerMin: 0.0187, Priced: true,
			HasSnapshot: true, Unpriced: 12, Priceable: 318, Incomplete: 3,
			SavedUSD: 0.24, HasSaved: true,
		}},
		{"every caveat at once, plus a stale age", spendSummary{
			TodayUSD: 0.0031, HasToday: true, TodayUnpriced: 399, TodayPriceable: 400, TodayIncomplete: 7,
			WindowUSD: 1.12, WindowLabel: "1h", BurnPerMin: 0.0187, Priced: true,
			HasSnapshot: true, Unpriced: 12, Priceable: 318, Incomplete: 3,
			SavedUSD: 0.24, HasSaved: true,
			Age: 3 * time.Minute, Stale: true,
		}},
		{"cjk window label under a partial day", spendSummary{
			TodayUSD: 0.0031, HasToday: true, TodayUnpriced: 399, TodayPriceable: 400,
			WindowUSD: 1.12, WindowLabel: "過去一時間", BurnPerMin: 0.0187, Priced: true,
			HasSnapshot: true, Unpriced: 12, Priceable: 318,
		}},
		{"suppressed window whose gap wears a cjk label", spendSummary{
			TodayUSD: 4.17, HasToday: true, TodayUnpriced: 2, TodayPriceable: 318,
			WindowLabel: "過去一時間", Priced: false,
			HasSnapshot: true, Unpriced: 40, Priceable: 40,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, w := range []int{40, 60, 80, 96, 120, 200} {
				assertStripFits(t, tc.s, w)
			}
			// Every width from 1 up, because the interesting failures are at the seams
			// where a form stops fitting.
			for w := 1; w <= 200; w++ {
				assertStripFits(t, tc.s, w)
			}
		})
	}
}

// A wedged poll chain must be visible. The strip is always on, so the failure mode is
// silent: the last good figure keeps rendering and nothing says when it was fetched.
func TestRenderSpendStrip_AStaleFigureIsDated(t *testing.T) {
	s := spendSummary{
		WindowUSD: 1.12, WindowLabel: "1h", Priced: true, HasSnapshot: true, Priceable: 10,
		Age: 3 * time.Minute, Stale: true,
	}
	got := renderSpendStrip(s, 200)

	if !strings.Contains(got, "3m ago") {
		t.Errorf("strip %q does not date a figure fetched 3m ago", got)
	}
	// The figure stays: it is old, not wrong.
	if !strings.Contains(got, "$1.1200 /1h") {
		t.Errorf("strip %q withheld a stale figure instead of dating it", got)
	}
}

// And a fresh one is not dated, which is the half that keeps the age worth reading.
func TestRenderSpendStrip_AFreshFigureIsNotDated(t *testing.T) {
	s := spendSummary{WindowUSD: 1.12, WindowLabel: "1h", Priced: true, HasSnapshot: true, Priceable: 10}
	got := renderSpendStrip(s, 200)

	if strings.Contains(got, "ago") {
		t.Errorf("strip %q dates a current figure; a permanent timestamp is noise", got)
	}
}

// The age is the LAST figure, so it is the first thing a narrow terminal gives up — unlike
// a partiality marker, which is undroppable. It is recoverable information: the next poll
// either lands or the age keeps growing.
func TestRenderSpendStrip_TheAgeYieldsBeforeAFigure(t *testing.T) {
	s := spendSummary{
		TodayUSD: 4.17, HasToday: true, TodayPriceable: 318,
		WindowUSD: 1.12, WindowLabel: "1h", Priced: true, HasSnapshot: true, Priceable: 10,
		Age: 3 * time.Minute, Stale: true,
	}
	narrow := renderSpendStrip(s, 24)

	if !strings.Contains(narrow, "$4.1700") {
		t.Errorf("narrow strip %q dropped the headline figure before the age", narrow)
	}
	if strings.Contains(narrow, "ago") {
		t.Errorf("narrow strip %q kept the age at the expense of a reading", narrow)
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

// assertStripFits is the strip's whole contract in one place: one line, inside the
// budget, and no half-rendered dollar amount.
func assertStripFits(t *testing.T, s spendSummary, w int) {
	t.Helper()
	got := renderSpendStrip(s, w)
	if gw := lipgloss.Width(got); gw > w {
		t.Fatalf("width %d: rendered %d columns: %q", w, gw, got)
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("width %d: strip contains a newline and costs the table a row: %q", w, got)
	}
	// A dollar sign that is not followed by a whole four-decimal figure is a clipped
	// amount. formatUSDCell is the only producer of a "$" on this line.
	for _, tail := range []string{"$", "$0", "$0.", "$0.0"} {
		if strings.HasSuffix(got, tail) {
			t.Fatalf("width %d: %q ends mid-figure", w, got)
		}
	}
}

// A newline in the server's window label, with a caveat live beside it. The label is
// sanitised in spendSummary, and this is the assertion that the caveats did not open a
// second path for a wire string to reach the terminal unmeasured: lipgloss.Width
// measures the WIDEST LINE, so a two-line result passes a budget check and silently
// steals a row from the table below.
func TestSpendSummary_CaveatsSurviveANewlineBearingWindowLabel(t *testing.T) {
	m := &model{}
	m.spend.snap = &usage.Snapshot{
		Window: "1h\nEVIL",
		Totals: usage.Counts{
			Requests: 318, CostMicros: 1_120_000,
			PricedRequests: 306, PriceableRequests: 318,
		},
		Priced: true,
	}
	m.spend.todaySnap = &usage.Snapshot{
		Window: usage.WindowToday,
		Totals: usage.Counts{
			Requests: 400, CostMicros: 3_100,
			PricedRequests: 1, PriceableRequests: 400,
		},
		Priced: true,
	}

	s := m.spendSummary()
	for _, w := range []int{40, 60, 80, 96, 120, 200} {
		assertStripFits(t, s, w)
	}
	for w := 1; w <= 200; w++ {
		assertStripFits(t, s, w)
	}
}

// The mirror of the above: a PRICED window keeps its figure when today is present.
// Without this the guard could be "fixed" by dropping the window figure whenever
// HasToday is set, which would silently delete a correct reading.
func TestRenderSpendStrip_TodayWithAPricedWindowKeepsBoth(t *testing.T) {
	s := spendSummary{
		TodayUSD: 4.17, HasToday: true,
		WindowUSD: 1.12, WindowLabel: "1h", Priced: true,
		HasSnapshot: true, Priceable: 40,
	}
	got := renderSpendStrip(s, 120)

	if !strings.Contains(got, "$4.1700 today") {
		t.Errorf("strip %q lost the today figure", got)
	}
	if !strings.Contains(got, "$1.1200 /1h") {
		t.Errorf("strip %q lost the priced window figure", got)
	}
}

// TestRenderSpendStrip_NoFigureCarriesAMinusSign.
//
// The rendered end of the negative-total refusal, on the line that is always on screen.
// Both figure paths at once, because they reach the strip through different code: the
// window figure through spendSummary and the day figure through applyTodayFigure.
//
// "$-5.0000" is not a smaller number, it is a claim that money came back. The strip says
// "cost unavailable" instead, which is its established spelling for a figure nobody can
// vouch for.
func TestRenderSpendStrip_NoFigureCarriesAMinusSign(t *testing.T) {
	m := &model{}
	m.spend.snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{Requests: 10, CostMicros: -5_000_000,
			PricedRequests: 10, PriceableRequests: 10},
		Priced: true,
	}
	m.spend.todaySnap = &usage.Snapshot{
		Window: usage.WindowToday,
		Totals: usage.Counts{Requests: 400, CostMicros: -5_000_000,
			PricedRequests: 400, PriceableRequests: 400},
		Priced: true,
	}

	got := renderSpendStrip(m.spendSummary(), 200)
	if strings.Contains(got, "$-") {
		t.Errorf("strip %q renders a negative amount — a refund nobody issued", got)
	}
	// And it is not silence either: the row is reserved on height alone, so a blank line
	// above the footer is the one outcome worse than saying "unavailable".
	if !strings.Contains(got, "cost unavailable") {
		t.Errorf("strip %q neither showed a figure nor declined one", got)
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
	m.spend.todaySnap = &usage.Snapshot{
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
	m.spend.todaySnap = &usage.Snapshot{
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
	m.spend.snap = &usage.Snapshot{Window: "1h", Totals: clamped, Priced: true,
		Buckets: []usage.Bucket{{Counts: clamped}}}
	m.spend.todaySnap = &usage.Snapshot{Window: usage.WindowToday, Totals: clamped, Priced: true}

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
	m.spend.snap = &usage.Snapshot{Window: "1h", Totals: clean, Priced: true,
		Buckets: []usage.Bucket{{Counts: clean}}}
	m.spend.todaySnap = &usage.Snapshot{Window: usage.WindowToday, Totals: clean, Priced: true}

	out := m.spendSummary()
	if out.Clamped || out.TodayClamped {
		t.Errorf("Clamped=%v TodayClamped=%v for an aggregate that never clamped",
			out.Clamped, out.TodayClamped)
	}
}

// TestRenderSpendStrip_ADamagedDayWearsItsOwnMarker.
//
// The rendered half. The marker rides on the FIGURE, so the fitter can drop the words and
// never the fact — the discipline partialMarker's own doc sets out. And it is a THIRD glyph:
// usage.Snapshot.Degraded's doc forbids showing it under the same marker as
// IncompleteRequests, because one says a figure in the sum is a floor and the other says
// rows are missing from the sum.
func TestRenderSpendStrip_ADamagedDayWearsItsOwnMarker(t *testing.T) {
	s := spendSummary{
		TodayUSD: 4.17, HasToday: true, TodayPriceable: 400,
		TodayDegraded: &usage.Degraded{SkippedLines: 3},
		WindowUSD:     1.12, WindowLabel: "1h", Priced: true, HasSnapshot: true, Priceable: 10,
	}
	got := renderSpendStrip(s, 200)

	if !strings.Contains(got, damagedMarker+"$4.1700 today") {
		t.Errorf("strip %q publishes a short day figure with no marker on it", got)
	}
	// The words too, while there is room for them.
	if !strings.Contains(got, "3 lines lost") {
		t.Errorf("strip %q does not say what the day lost", got)
	}
	// The rolling window figure is ring-backed and cannot be damaged, so it must NOT wear
	// the marker: a caveat on the wrong figure is a misattribution, which is the defect
	// moneyFigure was built to end.
	if strings.Contains(got, damagedMarker+"$1.1200") {
		t.Errorf("strip %q marks the ring-backed window figure as damaged", got)
	}
}

// TestRenderSpendStrip_TheDamageMarkerSurvivesNarrowing.
//
// The words are droppable, the marker is not. fitStripFigures gives up every figure's
// explanation before it gives up a reading, so a narrow terminal loses "3 lines lost" — and
// if it also lost the "!" the strip would publish a short total as a complete one, the worst
// outcome available on this line.
func TestRenderSpendStrip_TheDamageMarkerSurvivesNarrowing(t *testing.T) {
	s := spendSummary{
		TodayUSD: 4.17, HasToday: true, TodayPriceable: 400, TodayIncomplete: 7,
		TodayUnpriced: 100,
		TodayDegraded: &usage.Degraded{SkippedLines: 3, TruncatedDays: 1},
		HasSnapshot:   true,
	}
	// Every width from the point one whole figure fits. Below that the strip renders ""
	// rather than clip, which is its documented contract.
	for w := 1; w <= 200; w++ {
		got := renderSpendStrip(s, w)
		if got == "" {
			continue
		}
		if !strings.Contains(got, damagedMarker) {
			t.Errorf("width %d: %q dropped the damage marker: a short total now reads as complete", w, got)
		}
		// All three claims, all one cell each, none crowding out another.
		if !strings.Contains(got, damagedMarker+inexactMarker+"$4.1700"+partialMarker) {
			t.Errorf("width %d: %q lost one of the three markers", w, got)
		}
	}
}

// The width matrix again, with the damage caveat live — the newest thing that can break the
// strip's one-line, within-budget, never-clipped contract. The CJK label is two display
// columns per rune, so any len()- or rune-based arithmetic in the caveat path renders wider
// than it claims.
func TestRenderSpendStrip_TheDamageCaveatObeysTheWidthContract(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    spendSummary
	}{
		{"damaged day beside a partial hour", spendSummary{
			TodayUSD: 0.0031, HasToday: true, TodayUnpriced: 399, TodayPriceable: 400,
			TodayDegraded: &usage.Degraded{SkippedLines: 3},
			WindowUSD:     1.12, WindowLabel: "1h", BurnPerMin: 0.0187, Priced: true,
			HasSnapshot: true, Unpriced: 12, Priceable: 318,
			SavedUSD: 0.24, HasSaved: true,
		}},
		{"every caveat the day can carry, plus a stale age", spendSummary{
			TodayUSD: 0.0031, HasToday: true, TodayUnpriced: 399, TodayPriceable: 400,
			TodayIncomplete: 7,
			TodayDegraded:   &usage.Degraded{SkippedLines: 1_234_567, TruncatedDays: 89},
			WindowUSD:       1.12, WindowLabel: "1h", BurnPerMin: 0.0187, Priced: true,
			HasSnapshot: true, Unpriced: 12, Priceable: 318, Incomplete: 3,
			SavedUSD: 0.24, HasSaved: true,
			Age: 3 * time.Minute, Stale: true,
		}},
		{"damaged day under a cjk window label", spendSummary{
			TodayUSD: 0.0031, HasToday: true, TodayUnpriced: 399, TodayPriceable: 400,
			TodayDegraded: &usage.Degraded{SkippedLines: 3, TruncatedDays: 1},
			WindowUSD:     1.12, WindowLabel: "過去一時間", BurnPerMin: 0.0187, Priced: true,
			HasSnapshot: true, Unpriced: 12, Priceable: 318,
		}},
		{"a disclosure carrying no counters", spendSummary{
			TodayUSD: 4.17, HasToday: true, TodayPriceable: 400,
			TodayDegraded: &usage.Degraded{},
			HasSnapshot:   true,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, w := range []int{40, 60, 80, 96, 120, 200} {
				assertStripFits(t, tc.s, w)
			}
			// Every width from 1 up, because the interesting failures are at the seams where a
			// form stops fitting.
			for w := 1; w <= 200; w++ {
				assertStripFits(t, tc.s, w)
			}
		})
	}
}

// TestRenderSpendStrip_TheDamageCaveatIsNotWhatDropsAFigure.
//
// The words go in the figure's FULL form only, and fitStripFigures tries every figure's
// compact form before it drops a reading — so a longer caveat can cost an explanation and
// never a number. Pinned against the same summary with a clean read, which is the only way
// to tell "the caveat took a figure" from "the terminal was always too narrow".
func TestRenderSpendStrip_TheDamageCaveatIsNotWhatDropsAFigure(t *testing.T) {
	clean := spendSummary{
		TodayUSD: 4.17, HasToday: true, TodayPriceable: 400,
		WindowUSD: 1.12, WindowLabel: "1h", BurnPerMin: 0.0187, Priced: true,
		HasSnapshot: true, Priceable: 318,
	}
	damaged := clean
	damaged.TodayDegraded = &usage.Degraded{SkippedLines: 1_234_567, TruncatedDays: 89}

	for w := 1; w <= 200; w++ {
		gotClean, gotDamaged := renderSpendStrip(clean, w), renderSpendStrip(damaged, w)
		// Count the readings, not the characters: a figure is a "$" on this line.
		nClean, nDamaged := strings.Count(gotClean, "$"), strings.Count(gotDamaged, "$")
		// The marker costs the day figure one column, so the damaged line may hold one fewer
		// reading at the seams — that is the marker, which is undroppable by design, not the
		// caveat's words. More than one behind means the words are costing numbers.
		if nDamaged < nClean-1 {
			t.Errorf("width %d: damaged line holds %d figures where clean holds %d\n  clean:   %q\n  damaged: %q",
				w, nDamaged, nClean, gotClean, gotDamaged)
		}
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

// TestRenderSpendStrip_ADisclosureWithNoCountersStillMarksTheFigure.
//
// PRESENCE is the claim, not the counters. usage.Snapshot.Degraded is a pointer precisely so
// a clean read serialises nothing, so a producer that sent the object is saying it found
// damage — and reading its zeros as "checked, fine" is the same class of false reassurance
// as $0.00 over unpriced traffic. Tightening snapshotDamaged to require a non-zero counter
// survived every other test in this file.
func TestRenderSpendStrip_ADisclosureWithNoCountersStillMarksTheFigure(t *testing.T) {
	s := spendSummary{
		TodayUSD: 4.17, HasToday: true, TodayPriceable: 400,
		TodayDegraded: &usage.Degraded{},
		HasSnapshot:   true,
	}
	got := renderSpendStrip(s, 200)
	if !strings.Contains(got, damagedMarker+"$4.1700") {
		t.Errorf("strip %q reads a counterless disclosure as a clean read", got)
	}
	if !strings.Contains(got, "rows lost") {
		t.Errorf("strip %q says nothing about a disclosure it was sent", got)
	}
}

// TestRenderSpendStrip_AClampedFigureWearsTheShortMarkerOnEitherReading.
//
// usage.Counts.Saturated says an addition into a window's totals reached the int64 ceiling and
// was CLAMPED rather than allowed to wrap, so the figure is a floor by an amount nothing in the
// response can state. The server aggregated it and no client in cmd/abctl read it, which is the
// same defect usage.Snapshot.Degraded shipped with — and here it is worse than a missing caveat,
// because a clamped figure is not merely low but absurd, and an absurd number with no marker
// reads as a real one.
//
// BOTH READINGS, which is the asymmetry with damage. Degraded is a property of a LEDGER READ, so
// only the day figure can carry one. Saturated is on usage.Counts, and the ring's Add clamps
// exactly like the ledger's fold does — so a rolling hour can overflow with no ledger anywhere
// near it, and a strip that marked only the day would publish the other figure bare.
func TestRenderSpendStrip_AClampedFigureWearsTheShortMarkerOnEitherReading(t *testing.T) {
	t.Run("the day", func(t *testing.T) {
		s := spendSummary{
			TodayUSD: 4.17, HasToday: true, TodayPriceable: 400, TodayClamped: true,
			WindowUSD: 1.12, WindowLabel: "1h", Priced: true, HasSnapshot: true, Priceable: 10,
		}
		got := renderSpendStrip(s, 200)
		if !strings.Contains(got, damagedMarker+"$4.1700 today") {
			t.Errorf("strip %q publishes a clamped day figure with no marker on it", got)
		}
		if !strings.Contains(got, saturatedNote) {
			t.Errorf("strip %q does not say the day's figures are floors", got)
		}
		// The clean rolling figure must NOT be marked: a caveat on the wrong figure is the
		// misattribution moneyFigure was built to end.
		if strings.Contains(got, damagedMarker+"$1.1200") {
			t.Errorf("strip %q marks an unclamped window figure as short", got)
		}
	})
	t.Run("the rolling window", func(t *testing.T) {
		s := spendSummary{
			TodayUSD: 4.17, HasToday: true, TodayPriceable: 400,
			WindowUSD: 1.12, WindowLabel: "1h", Priced: true, HasSnapshot: true, Priceable: 10,
			Clamped: true,
		}
		got := renderSpendStrip(s, 200)
		if !strings.Contains(got, damagedMarker+"$1.1200 /1h") {
			t.Errorf("strip %q publishes a clamped rolling figure with no marker on it", got)
		}
		if strings.Contains(got, damagedMarker+"$4.1700") {
			t.Errorf("strip %q marks an unclamped day figure as short", got)
		}
	})
}

// TestRenderSpendStrip_ACleanAggregateCarriesNoClampCaveat is the mirror. A permanent "figures
// are floors" note over an aggregate that never clamped is the same false signal as a coverage
// warning that never clears — and it would appear on every strip there is.
func TestRenderSpendStrip_ACleanAggregateCarriesNoClampCaveat(t *testing.T) {
	s := spendSummary{
		TodayUSD: 4.17, HasToday: true, TodayPriceable: 400,
		WindowUSD: 1.12, WindowLabel: "1h", Priced: true, HasSnapshot: true, Priceable: 10,
	}
	got := renderSpendStrip(s, 200)
	if strings.Contains(got, saturatedNote) || strings.Contains(got, damagedMarker) {
		t.Errorf("strip %q qualifies a clean aggregate", got)
	}
}

// TestRenderSpendStrip_TheClampMarkerSurvivesNarrowing.
//
// The words are droppable, the marker is not — fitStripFigures gives up every figure's
// explanation before it gives up a reading. A narrow terminal losing "clamped, figures are
// floors" is a cost; losing the "!" would publish a clamped total as a settled one, which is the
// worst outcome available on this line.
func TestRenderSpendStrip_TheClampMarkerSurvivesNarrowing(t *testing.T) {
	s := spendSummary{
		TodayUSD: 4.17, HasToday: true, TodayPriceable: 400, TodayIncomplete: 7,
		TodayUnpriced: 100, TodayClamped: true,
		HasSnapshot: true,
	}
	for w := 1; w <= 200; w++ {
		got := renderSpendStrip(s, w)
		if got == "" {
			continue
		}
		if !strings.Contains(got, damagedMarker+inexactMarker+"$4.1700"+partialMarker) {
			t.Errorf("width %d: %q lost one of the three markers", w, got)
		}
	}
}

// TestMoneyFigure_OneMarkerForBothWaysAFigureCanBeShort.
//
// damagedMarker's two causes take ONE cell between them, and each keeps its own words. "!!" on a
// figure that is short twice over reads as emphasis rather than as two facts, and a fourth glyph
// would deepen the strip's vocabulary to draw a distinction that changes nothing about how the
// number must be read — where the WORDS do change what an operator goes and looks at: a day file
// for one, whatever produced 9.2e18 micros of traffic for the other.
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

// The width matrix again, with the clamp caveat live. Same contract as ever: one line, never
// wider than the budget, never a clipped figure. The CJK label is two display columns per rune,
// so any len()- or rune-based arithmetic in the new caveat path renders wider than it claims.
func TestRenderSpendStrip_TheClampCaveatObeysTheWidthContract(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    spendSummary
	}{
		{"a clamped day beside a clamped hour", spendSummary{
			TodayUSD: 4.17, HasToday: true, TodayPriceable: 400, TodayClamped: true,
			WindowUSD: 1.12, WindowLabel: "1h", BurnPerMin: 0.0187, Priced: true,
			HasSnapshot: true, Priceable: 318, Clamped: true,
		}},
		{"every caveat a day can carry at once", spendSummary{
			TodayUSD: 0.0031, HasToday: true, TodayUnpriced: 399, TodayPriceable: 400,
			TodayIncomplete: 7, TodayClamped: true,
			TodayDegraded: &usage.Degraded{SkippedLines: 1_234_567, TruncatedDays: 89},
			WindowUSD:     1.12, WindowLabel: "過去一時間", BurnPerMin: 0.0187, Priced: true,
			HasSnapshot: true, Unpriced: 12, Priceable: 318, Incomplete: 3, Clamped: true,
			SavedUSD: 0.24, HasSaved: true,
			Age: 3 * time.Minute, Stale: true,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for w := 1; w <= 200; w++ {
				assertStripFits(t, tc.s, w)
			}
		})
	}
}
