package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/rossoctl/cortex/core/usage"
)

// bandSummary is a healthy four-span answer, with figures of four different magnitudes so the
// alignment assertions have something to line up. The numbers are the shape a live proxy
// produced: an hour inside a day inside a week inside a month.
func bandSummary() spendSummary {
	var s spendSummary
	for span, usd := range map[spendSpan]float64{
		spanHour:  4.04,
		spanToday: 18.7994,
		span7d:    216.44,
		spanMonth: 703.18,
	} {
		s.Spans[span] = spanReading{

			USD:       usd,
			Priced:    true,
			Priceable: 100,
		}
	}
	return s
}

// bandSpan pulls one cell's reading out for mutation in a test.
func withSpan(s spendSummary, span spendSpan, mutate func(*spanReading)) spendSummary {
	r := s.Spans[span]
	mutate(&r)
	s.Spans[span] = r
	return s
}

// EVERY SPAN IS ON THE BAND, AND EVERY ONE NAMES ITSELF. That is the invariant the band exists
// to hold: the defect it replaces had CACHE HIT and TOKENS read off the rolling-hour snapshot
// while sitting in a row that opened with TODAY, so an hour's figures read as a day's.
func TestRenderSpendBand_EverySpanIsLabelledWithItsPeriod(t *testing.T) {
	lines := renderSpendBand(bandSummary(), 200)
	if len(lines) != spendBandLines {
		t.Fatalf("%d lines, want %d", len(lines), spendBandLines)
	}
	for span := spendSpan(0); span < numSpendSpans; span++ {
		label := spendSpanDefs[span].label
		if !strings.Contains(lines[0], label) {
			t.Errorf("band %q is missing %q", lines[0], label)
		}
	}
	// And the figures are all there, under them.
	for span := spendSpan(0); span < numSpendSpans; span++ {
		want := formatUSDTotal(bandSummary().Spans[span].USD)
		if !strings.Contains(lines[0], want) {
			t.Errorf("band %q is missing %s's figure %q",
				lines[0], spendSpanDefs[span].label, want)
		}
	}
}

// ASCENDING SPAN, LEFT TO RIGHT, because that is how a reader zooms out from "right now" — and
// because the drop order depends on it being the visual order and nothing else.
func TestRenderSpendBand_ReadsOutwardsFromTheLiveHour(t *testing.T) {
	lines := renderSpendBand(bandSummary(), 200)
	at := -1
	for span := spendSpan(0); span < numSpendSpans; span++ {
		label := spendSpanDefs[span].label
		i := strings.Index(lines[0], label)
		if i < 0 {
			t.Fatalf("band %q is missing %q", lines[0], label)
		}
		if i <= at {
			t.Errorf("%q appears at %d, not after the previous span at %d — the band must read "+
				"in ascending span order:\n%s", label, i, at, lines[0])
		}
		at = i
	}
}

// EVERY LABEL SITS IMMEDIATELY BESIDE ITS OWN FIGURE, which is what makes one line legible.
//
// THIS REPLACES TestRenderSpendBand_FiguresShareAColumnStride, and the swap is the point of
// folding the band, so it is worth recording rather than leaving as a deleted test. The old band
// stacked labels over values and right-aligned both in cells of ONE shared width, because four
// readings of the same quantity left-flushed in cells of their own widths put "$4.04" and
// "$703.18" four columns apart — a reader scanning the column could not compare them.
//
// On one line there is no column to scan, so that property is not weakened, it is INAPPLICABLE:
// nothing sits above or below a figure to be aligned with. What replaces it is adjacency —
// "TODAY $18.80" reads as a pair whatever its neighbours are — and adjacency is a stronger
// guarantee than a shared stride, because a stride can be right while the label above it is the
// wrong one. Asserted as a CONTIGUOUS SUBSTRING for exactly that reason: it cannot pass for a
// band that pairs a label with a different span's money.
//
// The cost is real and is not hidden: two cells of unequal width no longer put their decimal
// points anywhere near each other. That is accepted, because the row is read left to right now
// rather than scanned top to bottom, and because the row it gives back goes to the sessions
// table — see spendBandLines.
func TestRenderSpendBand_EveryLabelSitsBesideItsOwnFigure(t *testing.T) {
	line := renderSpendBand(bandSummary(), 200)[0]
	at := -1
	for span := spendSpan(0); span < numSpendSpans; span++ {
		pair := spendSpanDefs[span].label + " " + formatUSDTotal(bandSummary().Spans[span].USD)
		i := strings.Index(line, pair)
		if i < 0 {
			t.Errorf("%q does not appear as a unit in the band — a label must travel with its own "+
				"figure and nothing may come between them:\n%s", pair, line)
			continue
		}
		// AND IN ASCENDING SPAN ORDER, checked here rather than left to
		// TestRenderSpendBand_ReadsOutwardsFromTheLiveHour: that test finds each label
		// independently, so it would stay green for a band that paired every label with the
		// WRONG figure as long as the labels themselves were in order.
		if i <= at {
			t.Errorf("%q appears at %d, not after the previous pair at %d:\n%s", pair, i, at, line)
		}
		at = i
	}
}

// A WIDE CELL COSTS ONLY ITSELF, which the two-line band could not promise.
//
// bandCell.width() used to give every surviving cell max(width) so their figures shared a stride,
// so one five-figure month inflated all four columns — the effect
// TestRenderSpendBand_HealthySlowSpansDoNotWidenTheBand measures from the other side. Folded, a
// cell occupies its own label plus its own figure, so widening one span's number cannot move
// another span's cell at all.
//
// This is what makes "THIS MONTH" affordable: five columns longer than "MONTH" and it lands on one
// cell instead of four.
func TestRenderSpendBand_AWideCellDoesNotWidenTheOthers(t *testing.T) {
	base := renderSpendBand(bandSummary(), 200)[0]
	// A month two orders of magnitude larger, nothing else touched.
	fat := renderSpendBand(withSpan(bandSummary(), spanMonth, func(r *spanReading) {
		r.USD = 70318.42
	}), 200)[0]

	// Everything up to the month's own cell is byte-identical.
	cut := strings.Index(base, spendSpanDefs[spanMonth].label)
	if cut < 0 {
		t.Fatalf("band is missing %q:\n%s", spendSpanDefs[spanMonth].label, base)
	}
	if got, want := fat[:cut], base[:cut]; got != want {
		t.Errorf("widening the month moved the cells before it:\n got %q\nwant %q", got, want)
	}
}

// Each of the three disclosure glyphs still rides on the figure it qualifies.
func TestRenderSpendBand_CarriesTheFigureMarkers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*spanReading)
		want   string
	}{
		{"inexact", func(r *spanReading) { r.Incomplete = 3 }, inexactMarker},
		{"partial", func(r *spanReading) { r.Unpriced, r.Priceable = 40, 100 }, partialMarker},
		{"damaged", func(r *spanReading) { r.Clamped = true }, damagedMarker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := withSpan(bandSummary(), spanToday, tc.mutate)
			// AT EVERY WIDTH THAT DRAWS TODAY AT ALL, not 200 alone. A marker is a column, so
			// marking a cell widens it — which is exactly where a marker is most likely to be
			// dropped and least likely to be noticed. The strip this band replaced had a
			// narrow-width marker test (its fitter fell back to a compact form); nothing restored
			// that coverage here, so the whole marker suite was asserting one roomy terminal.
			//
			// GATED ON THE CELL BEING PRESENT, per width, rather than swept upward from the
			// narrowest width that shows it. The upward sweep was sound only while every cell
			// shared one width: inline, "TODAY ~$18.80" (13 columns) fits alone at 13 while
			// "THIS MONTH $703.18" (18) does not, so TODAY is drawn at 13 and then correctly
			// REPLACED by the month at 18 — the month outranks it in bandDropOrder, and trading a
			// cell for a more valuable one as the terminal widens is what "chosen by value, not by
			// count" means. Swept blindly, that legitimate swap reads as a lost marker.
			//
			// The property that actually matters is unchanged and is what is asserted: wherever the
			// cell appears, its disclosure appears with it. Never a silent figure.
			drawn := 0
			for width := 1; width <= 200; width++ {
				joined := strings.Join(renderSpendBand(s, width), "\n")
				if !strings.Contains(joined, spendSpanDefs[spanToday].label) {
					continue
				}
				drawn++
				if !strings.Contains(joined, tc.want) {
					t.Fatalf("width %d: TODAY is on screen without its %q marker:\n%s",
						width, tc.want, joined)
				}
			}
			if drawn == 0 {
				t.Fatalf("TODAY is drawn at no width up to 200, so this asserted nothing")
			}
		})
	}
}

// A SPAN THIS DEPLOYMENT CANNOT ANSWER READS AS AN EM DASH, never as a number.
//
// With no cost ledger the server answers a symbolic window from the six-hour ring instead and
// reports the window it actually served. Drawing that under a MONTH label would understate the
// month by about 120x while looking perfectly well-formed, which is the one thing every money
// surface here refuses.
func TestRenderSpendBand_AnUnanswerableSpanIsNotANumber(t *testing.T) {
	s := withSpan(bandSummary(), spanMonth, func(r *spanReading) {
		r.Unanswerable = true
		r.USD, r.Priced = 703.18, true // the figure is there and must NOT be drawn
	})
	lines := renderSpendBand(s, 200)
	if !strings.Contains(lines[0], "MONTH") {
		t.Errorf("band %q dropped MONTH; silence about a span is worse than saying it is "+
			"unavailable:\n%s", lines[0], strings.Join(lines, "\n"))
	}
	if strings.Contains(lines[0], "$703.18") {
		t.Errorf("band %q drew a figure for a span the deployment cannot answer:\n%s",
			lines[0], strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[0], emptyCell) {
		t.Errorf("band %q has no %q for the unanswerable span", lines[0], emptyCell)
	}
	// The spans that CAN be answered are untouched: one degraded cell must not blank the band.
	if !strings.Contains(lines[0], formatUSDTotal(4.04)) {
		t.Errorf("band %q lost the hour figure over the month's degradation", lines[0])
	}
}

// A failed poll and an unpriced span read the same way, and neither blanks its neighbours —
// which is what the per-span chains were split for.
func TestRenderSpendBand_OneChainsFailureLeavesTheOthers(t *testing.T) {
	s := withSpan(bandSummary(), span7d, func(r *spanReading) {
		r.Failed, r.Priced = true, false
	})
	lines := renderSpendBand(s, 200)
	if !strings.Contains(lines[0], emptyCell) {
		t.Errorf("band %q has no %q for the failed span", lines[0], emptyCell)
	}
	for _, span := range []spendSpan{spanHour, spanToday, spanMonth} {
		want := formatUSDTotal(bandSummary().Spans[span].USD)
		if !strings.Contains(lines[0], want) {
			t.Errorf("band %q lost %s over another span's failure",
				lines[0], spendSpanDefs[span].label)
		}
	}
}

// A WEDGED CHAIN SAYS SO, on the label of the span that wedged.
//
// This is the disclosure that went missing when the band replaced the strip: spendSummary
// computed Age and Stale and no renderer read either, so a poll chain that stopped answering
// looked exactly like a current reading. It is per span because the four poll fifteen times
// apart — one age for the band would alarm on a healthy month or stay silent on a wedged hour.
func TestRenderSpendBand_AStaleSpanIsDated(t *testing.T) {
	s := withSpan(bandSummary(), spanToday, func(r *spanReading) {
		r.Stale, r.Age = true, 7*time.Minute
	})
	lines := renderSpendBand(s, 200)
	if !strings.Contains(lines[0], "TODAY "+formatSpendAge(7*time.Minute)) {
		t.Errorf("band %q does not date the stale span; a wedged chain is indistinguishable "+
			"from a current reading without it:\n%s", lines[0], strings.Join(lines, "\n"))
	}
	// The figure itself is unchanged: the answer is old, not wrong.
	if !strings.Contains(lines[0], formatUSDTotal(18.7994)) {
		t.Errorf("band %q dropped a figure that was merely stale", lines[0])
	}
	// And a fresh band carries no timestamp at all — a permanent one is noise.
	fresh := strings.Join(renderSpendBand(bandSummary(), 200), "\n")
	if strings.Contains(fresh, "ago") || strings.Contains(fresh, formatSpendAge(7*time.Minute)) {
		t.Errorf("a healthy band carries an age:\n%s", fresh)
	}
}

// Height is constant, nothing is ever clipped, and no line exceeds the terminal.
func TestRenderSpendBand_HeightIsConstantAndFiguresDropWhole(t *testing.T) {
	for _, w := range []int{200, 120, 78, 60, 40, 34, 30, 20, 8, 1} {
		lines := renderSpendBand(bandSummary(), w)
		if len(lines) != spendBandLines {
			t.Fatalf("width %d: %d lines, want %d", w, len(lines), spendBandLines)
		}
		for i, line := range lines {
			if n := lipgloss.Width(line); n > w {
				t.Errorf("width %d: line %d is %d columns: %q", w, i, n, line)
			}
		}
		// A figure that survives is never half a figure.
		//
		// THROUGH clipsFigure rather than spelled out here, because the detector is the part
		// that can be silently wrong: this assertion once read Contains("$3.84") &&
		// !Contains("$3.8402"), and retuning the literals for a two-decimal formatter collapsed
		// both sides onto "$3.84" — an `x && !x` that could never fire. The detector has its own
		// guard now; see TestClipsFigure_CanActuallyFail.
		//
		// IT CANNOT FIRE AS THE BAND IS WRITTEN TODAY, and that is deliberate rather than
		// overlooked. renderSpendBand emits every cell through padLeft and then trims trailing
		// spaces, so it only ever PADS: under width pressure it gives up whole cells and has no
		// path that shortens one. This is a guard against a CHANGE, not a claim about current
		// behaviour — and the change it guards against is one this package has already been bitten
		// by, where a width budget met by truncation ate the end of a cell (the table layer's
		// runewidth.Truncate hazard, which is not ANSI-aware either).
		//
		// Note how this differs from the assertions this branch deleted for being unreachable:
		// those could not fail because of their own arithmetic, where this cannot fail because the
		// subject is currently correct. The first is a broken test; the second is an invariant.
		for span := spendSpan(0); span < numSpendSpans; span++ {
			whole := formatUSDTotal(bandSummary().Spans[span].USD)
			if clipsFigure(lines[0], whole) {
				t.Errorf("width %d: %s's figure was clipped: %q (probe %q, want whole %q)",
					w, spendSpanDefs[span].label, lines[0], figureProbe(whole), whole)
			}
		}
	}
}

// THE THREE ANSWERS A MONEY CELL CAN GIVE, pinned together because each one is a different truth
// and two of them look alike.
//
//	unpriced            an em dash      nothing here carried a cost figure
//	priced at zero      "$0.00"         every request was settled free
//	priced below a cent "<$0.01"        a real charge, too small to state to the cent
//
// THE MIDDLE ONE IS DELIBERATE AND IS NOT THE $0.00 THIS PACKAGE REFUSES ELSEWHERE.
// usage.Snapshot.Priced is PricedRequests > 0 and explicitly NOT CostMicros > 0, because a window
// whose every request the gateway SETTLED AT ZERO has priced requests and no dollars: snapshot.go
// requires that to render as a zero figure rather than as "cost unavailable", and they are
// different answers. sessionMoneyCell and the tier column do use the em dash for their own zeros,
// but those are per-session and per-tier apportionments where a zero means "absent from the mix" —
// an aggregate of settled-free traffic means the traffic was free, and withholding it would report
// an answer we have as one we do not.
//
// The reading that WOULD be a lie is the third row, and it is refused one layer down:
// formatUSDCell floors anything positive under half a cent to "<$0.01". That is why the cell can
// print $0.00 only for an exact zero — and why both are asserted here, since a reader probing a
// $0.00 cell cannot otherwise tell which of the two produced it.
//
// The expected strings are LITERALS, not formatUSDCell calls: this is a contract about what an
// operator reads, and deriving the expectation from the formatter would let a formatter change
// take both sides with it. That is the vacuity TestClipsFigure_CanActuallyFail documents.
func TestRenderSpendBand_UnpricedZeroAndSubCentAreThreeDifferentCells(t *testing.T) {
	cell := func(s spendSummary) string { return renderSpendBand(s, 200)[0] }

	unpriced := cell(withSpan(bandSummary(), spanHour, func(r *spanReading) {
		r.USD, r.Priced = 0, false
	}))
	if !strings.Contains(unpriced, emptyCell) {
		t.Errorf("an unpriced hour rendered %q with no %q: priced:false means cost unavailable, "+
			"and a figure in its place claims knowledge the snapshot disclaims",
			unpriced, emptyCell)
	}
	if strings.Contains(unpriced, "$0.00") {
		t.Errorf("an unpriced hour rendered %q, which reads as free traffic", unpriced)
	}

	free := cell(withSpan(bandSummary(), spanHour, func(r *spanReading) {
		r.USD, r.Priced = 0, true
	}))
	if !strings.Contains(free, "$0.00") {
		t.Errorf("a settled-free hour rendered %q, want $0.00: the gateway priced every request "+
			"at nothing, which is an answer and not an absence", free)
	}

	subCent := cell(withSpan(bandSummary(), spanHour, func(r *spanReading) {
		r.USD, r.Priced = 0.003, true
	}))
	if !strings.Contains(subCent, "<$0.01") {
		t.Errorf("a $0.003 hour rendered %q, want <$0.01", subCent)
	}
	if strings.Contains(subCent, "$0.00") {
		t.Errorf("a $0.003 hour rendered %q: a known non-zero charge shown as free is the one "+
			"rounding this band is not allowed to do", subCent)
	}
}

// TODAY AND MONTH OUTLIVE THE OTHER TWO, which is the drop order the band needs and NOT the
// order it reads in.
//
// Dropping right-to-left would give up MONTH first — the budget figure, and the reason three of
// these spans exist. Dropping left-to-right would give up the live hour. So the middle yields:
// the week first, then the hour.
func TestRenderSpendBand_TodayAndMonthSurviveLongest(t *testing.T) {
	// EVERY RUNG OF THE LADDER, not just the bottom one. This used to narrow from 60, stop at the
	// first width leaving exactly two cells, and assert on those — so the whole of counts four and
	// three went unexamined, and the inversion that put the week on screen while hiding the month
	// lived entirely at three. A test that looks at one rung of a four-rung ladder reports the
	// ladder as sound.
	//
	// The order is the one bandDropOrder states: the week goes first, then the hour, leaving TODAY
	// and MONTH, and MONTH alone last of all.
	for _, rung := range []struct {
		cells int
		want  []spendSpan
	}{
		{4, []spendSpan{spanHour, spanToday, span7d, spanMonth}},
		{3, []spendSpan{spanHour, spanToday, spanMonth}},
		{2, []spendSpan{spanToday, spanMonth}},
		{1, []spendSpan{spanMonth}},
	} {
		// The WIDEST width that yields this many cells, so each rung is judged where it is the
		// band's own choice rather than an artefact of the next rung's boundary.
		found := ""
		// The ceiling has to clear the WHOLE band. Four cells of bandSummary() plus three
		// separators is 66 columns once labels sit inline with their figures ("LAST 1H $4.04" is
		// 13 where the old stacked cell was 7), so the 60 this scanned before the fold could not
		// reach the four-cell rung at all and that rung silently asserted nothing.
		for w := 120; w >= 1; w-- {
			line := renderSpendBand(bandSummary(), w)[0]
			n := 0
			for span := spendSpan(0); span < numSpendSpans; span++ {
				if strings.Contains(line, spendSpanDefs[span].label) {
					n++
				}
			}
			if n == rung.cells {
				found = line
				break
			}
		}
		if found == "" {
			t.Errorf("no width leaves exactly %d cells, so that rung asserted nothing", rung.cells)
			continue
		}
		keep := map[spendSpan]bool{}
		for _, span := range rung.want {
			keep[span] = true
		}
		for span := spendSpan(0); span < numSpendSpans; span++ {
			label := spendSpanDefs[span].label
			if got := strings.Contains(found, label); got != keep[span] {
				verb := "is missing"
				if got {
					verb = "still carries"
				}
				t.Errorf("at %d cells the band %s %q: %q", rung.cells, verb, label, found)
			}
		}
	}
}

// An empty summary still fills its reserved height, because layout() has already held the rows
// back — see spendBandLines. Blank lines, never zero lines.
func TestRenderSpendBand_EmptySummaryStillFillsItsHeight(t *testing.T) {
	lines := renderSpendBand(spendSummary{}, 200)
	if len(lines) != spendBandLines {
		t.Fatalf("%d lines, want %d", len(lines), spendBandLines)
	}
	// Every span reports "not known here" rather than a zero, and still names itself: a band
	// of em dashes is an honest answer, a band of "$0.00" is a claim that nothing was spent.
	for span := spendSpan(0); span < numSpendSpans; span++ {
		if !strings.Contains(lines[0], spendSpanDefs[span].label) {
			t.Errorf("band %q dropped %q on an empty summary", lines[0],
				spendSpanDefs[span].label)
		}
	}
	if strings.Contains(lines[0], "$0.00") {
		t.Errorf("band %q renders $0.00 for spans nothing has answered yet", lines[0])
	}
}

// spanReadings is where the disclosure is decided, so it is asserted directly too: the renderer
// can only draw an em dash if this says the span is unanswerable.
func TestSpanReadings_DetectTheNoLedgerDegradation(t *testing.T) {
	m := &model{}
	// A proxy with no cost ledger: every symbolic window comes back as the ring's span.
	for _, span := range []spendSpan{spanToday, span7d, spanMonth} {
		m.spend.chains[span].snap = &usage.Snapshot{
			Window: "6h0m0s",
			Totals: usage.Counts{Requests: 10, CostMicros: 1_120_000, PricedRequests: 10, PriceableRequests: 10},
			Priced: true,
		}
	}
	// The hour is ring-served and comes back stringified, which is the SAME span and must not
	// be mistaken for a degradation.
	m.spend.chains[spanHour].snap = &usage.Snapshot{
		Window: "1h0m0s",
		Totals: usage.Counts{Requests: 10, CostMicros: 404_000, PricedRequests: 10, PriceableRequests: 10},
		Priced: true,
	}

	got := m.spanReadings()

	for _, span := range []spendSpan{spanToday, span7d, spanMonth} {
		if !got[span].Unanswerable {
			t.Errorf("%s: Unanswerable = false though the server served 6h — a six-hour figure "+
				"under this label understates the span", spendSpanDefs[span].label)
		}
	}
	if got[spanHour].Unanswerable {
		t.Error("the hour is marked unanswerable, but \"1h\" and \"1h0m0s\" are the same span " +
			"spelled by the caller and by Go")
	}
	if !got[spanHour].Priced || got[spanHour].USD != 0.404 {
		t.Errorf("hour reading = %+v, want the priced 0.404 figure", got[spanHour])
	}
}

// servedAsRequested is the whole basis of that disclosure, so its boundary is pinned directly.
func TestServedAsRequested(t *testing.T) {
	for _, tc := range []struct {
		want, served string
		ok           bool
	}{
		{"1h", "1h0m0s", true},   // a duration window, stringified by Go
		{"1h", "1h", true},       // and spelled back verbatim
		{"today", "today", true}, // symbolic, answered
		{"month", "month", true},
		{"7d", "7d", true},
		{"month", "6h0m0s", false}, // the no-ledger degradation
		{"today", "6h0m0s", false},
		{"7d", "6h0m0s", false},
		{"1h", "6h0m0s", false},   // a duration served as a DIFFERENT duration
		{"month", "today", false}, // never equal across symbolic windows
	} {
		if got := servedAsRequested(tc.want, tc.served); got != tc.ok {
			t.Errorf("servedAsRequested(%q, %q) = %v, want %v", tc.want, tc.served, got, tc.ok)
		}
	}
}

// TestSpanReadings_StalenessIsMeasuredAgainstEachSpansOwnCadence.
//
// THE THRESHOLD HAS TO BE PER SPAN, exactly like the readings are, and the plumbing landed
// without it: spanReadings compared every chain's age to the global spendStaleAfter, which is
// twice the HOUR's twenty-second cadence — forty seconds. The month and the week poll every
// five minutes, so for about 87% of every healthy polling cycle they were dated "MONTH 3m",
// which reads as a wedged chain on a chain that answered three minutes ago and is not due for
// another two.
//
// It also cost width. The age widens the label, the band is one uniform cell width, so two
// permanently-dated cells pushed the whole band from 34 columns to 42 — dropping 7 DAYS at a
// width where all four had fit.
//
// bandSpanCell's own doc already argued for this ("PER SPAN, because the four poll fifteen
// times apart. One age for the whole band would … alarm on a healthy month chain between its
// own five-minute polls"). This is that rationale made true.
func TestSpanReadings_StalenessIsMeasuredAgainstEachSpansOwnCadence(t *testing.T) {
	now := time.Now()
	for span := spendSpan(0); span < numSpendSpans; span++ {
		def := spendSpanDefs[span]
		t.Run(def.label, func(t *testing.T) {
			// One interval old: a chain answering on schedule, never stale.
			m := &model{}
			m.spend.chains[span].snap = &usage.Snapshot{
				Window: def.window,
				Totals: usage.Counts{Requests: 1, CostMicros: 1_000_000, PricedRequests: 1, PriceableRequests: 1},
				Priced: true,
			}
			m.spend.chains[span].lastFetch = now.Add(-def.interval)
			if got := m.spanReadings()[span]; got.Stale {
				t.Errorf("%s: stale after one poll interval (%v) — a chain answering on its own "+
					"cadence is healthy, and dating it reads as wedged", def.label, def.interval)
			}
			// Just inside twice its own interval: still healthy, so one dropped reply is not
			// an alarm.
			//
			// HALF AN INTERVAL OF MARGIN, not one second. `now` is captured once above and reused
			// across four subtests, and the threshold is an inequality against wall time — so a
			// second of margin is a second of budget for the race detector, a loaded CI runner or
			// a GC pause, after which "just inside" silently becomes "past" and the assertion
			// fails for a reason that has nothing to do with staleness. The margin scales with the
			// cadence, so the hour keeps twenty seconds and the slow spans keep two and a half
			// minutes, and the property under test is unchanged: the threshold is 2x the interval.
			m.spend.chains[span].lastFetch = now.Add(-2*def.interval + def.interval/2)
			if got := m.spanReadings()[span]; got.Stale {
				t.Errorf("%s: stale just inside 2x its %v interval; one missed reply must not "+
					"alarm", def.label, def.interval)
			}
			// Past twice its own interval: now it is worth saying.
			m.spend.chains[span].lastFetch = now.Add(-2*def.interval - def.interval/2)
			got := m.spanReadings()[span]
			if !got.Stale {
				t.Errorf("%s: not stale past 2x its %v interval — a wedged chain is "+
					"indistinguishable from a current reading without this", def.label, def.interval)
			}
			if got.Age < 2*def.interval {
				t.Errorf("%s: Age = %v, want at least 2x the %v interval", def.label, got.Age, def.interval)
			}
		})
	}
}

// And the consequence the global threshold had on the band: a healthy month and week must not
// widen it, because the age rides on the label and the cells share one width.
func TestRenderSpendBand_HealthySlowSpansDoNotWidenTheBand(t *testing.T) {
	now := time.Now()
	m := &model{}
	for span := spendSpan(0); span < numSpendSpans; span++ {
		def := spendSpanDefs[span]
		m.spend.chains[span].snap = &usage.Snapshot{
			Window: def.window,
			Totals: usage.Counts{Requests: 1, CostMicros: 4_040_000, PricedRequests: 1, PriceableRequests: 1},
			Priced: true,
		}
		// Three minutes since each answered: overdue for the hour and today, well inside the
		// five-minute cadence of the week and the month.
		m.spend.chains[span].lastFetch = now.Add(-3 * time.Minute)
	}
	got := m.spanReadings()
	if !got[spanHour].Stale {
		t.Error("the hour is not dated three minutes after a 20s-cadence poll; it IS wedged")
	}
	for _, span := range []spendSpan{span7d, spanMonth} {
		if got[span].Stale {
			t.Errorf("%s is dated three minutes after its own five-minute-cadence poll, which "+
				"reads as wedged on a chain that is not even due yet", spendSpanDefs[span].label)
		}
	}
	// AND THE WIDTH CONSEQUENCE, on a band where nothing is wedged. The fixture above dates the
	// hour and the day legitimately — three minutes IS overdue on a 20s and a 60s cadence — and
	// two genuinely stale cells SHOULD widen the band. What must not widen it is the pair that
	// answered on schedule, so here every chain is inside its own interval.
	healthy := &model{}
	for span := spendSpan(0); span < numSpendSpans; span++ {
		def := spendSpanDefs[span]
		healthy.spend.chains[span].snap = &usage.Snapshot{
			Window: def.window,
			Totals: usage.Counts{Requests: 1, CostMicros: 4_040_000, PricedRequests: 1, PriceableRequests: 1},
			Priced: true,
		}
		healthy.spend.chains[span].lastFetch = now.Add(-def.interval)
	}
	spans := healthy.spanReadings()
	for span := spendSpan(0); span < numSpendSpans; span++ {
		if spans[span].Stale {
			t.Fatalf("%s is dated one interval after its own poll, so this width check is "+
				"measuring the wrong thing", spendSpanDefs[span].label)
		}
	}
	// THE WIDTH CONSEQUENCE, ASSERTED AGAINST A BAND WITH NO AGES AT ALL rather than at a fixed
	// column count. It used to assert that all four cells survived at width 40, which held only
	// because the stacked band charged every cell max(label, value) — seven columns, so four cells
	// and three gutters came to 34. Inline, the same four spans need 61, and a hardcoded 40 turned a
	// statement about staleness into a statement about arithmetic that the fold changed.
	//
	// What the test is actually for is that an age on a healthy chain costs nothing, and comparing
	// against the same summary with every Stale flag cleared says exactly that — at any width, and
	// without a number in the test that has to be re-derived every time a label changes.
	clean := spendSummary{Spans: spans}
	for span := spendSpan(0); span < numSpendSpans; span++ {
		clean.Spans[span].Stale, clean.Spans[span].Age = false, 0
	}
	wide, plain := renderSpendBand(spendSummary{Spans: spans}, 200), renderSpendBand(clean, 200)
	if got, want := lipgloss.Width(wide[0]), lipgloss.Width(plain[0]); got != want {
		t.Errorf("a band whose chains all answered on schedule is %d columns against %d with the "+
			"staleness explicitly cleared — a healthy chain is paying for an age it should not "+
			"carry:\n%s\n%s", got, want, wide[0], plain[0])
	}
	// And every cell really is there, or an equal width would be two equally empty bands.
	for span := spendSpan(0); span < numSpendSpans; span++ {
		if !strings.Contains(wide[0], spendSpanDefs[span].label) {
			t.Errorf("%s is missing from a band where every chain answered on schedule:\n%s",
				spendSpanDefs[span].label, wide[0])
		}
	}
}

// bandDropOrder MUST BE A PERMUTATION of the spans, and that is a structural hazard rather than a
// style point: it is a POSITIONAL array literal, so raising numSpendSpans to five leaves element
// four at the zero value — spanHour, a duplicate — and the new span is then never dropped at all.
//
// The fit loop walks this order exactly numSpendSpans times, so a duplicate costs it one of its
// only chances to shed a cell: it can exit with bandWidth still above the terminal width, the band
// renders wider than its column budget, wraps to three lines against layout()'s two-line
// reservation, and pushes the footer off the bottom. That is the failure the whole drop ladder
// exists to prevent, reintroduced by an array literal that compiled.
func TestBandDropOrder_IsAPermutationOfEverySpan(t *testing.T) {
	var seen [numSpendSpans]int
	for i, span := range bandDropOrder {
		if !span.valid() {
			t.Fatalf("bandDropOrder[%d] = %d, which is not a span", i, span)
		}
		seen[span]++
	}
	for span := spendSpan(0); span < numSpendSpans; span++ {
		switch seen[span] {
		case 1:
		case 0:
			t.Errorf("%s is missing from bandDropOrder, so it can never be dropped and the band "+
				"can render wider than the terminal", spendSpanDefs[span].label)
		default:
			t.Errorf("%s appears %d times in bandDropOrder, which spends another span's turn to "+
				"drop", spendSpanDefs[span].label, seen[span])
		}
	}
}

// THE FOUR FIXTURE PROBES MUST BE PAIRWISE DISTINCT, or the clip assertion above can be satisfied
// by the wrong cell.
//
// clipsFigure looks for a three-rune prefix anywhere in the value row — "$21" for $216.44 — so two
// fixture amounts sharing a prefix ("$216.44" and "$21.50") would let one cell's text answer for
// the other, and a genuinely clipped figure could pass because its neighbour happened to contain
// the probe. The current amounts are distinct by luck of choosing four magnitudes; this makes it a
// requirement, so a future fixture edit fails here rather than quietly weakening the clip check.
func TestBandFixtureProbes_DoNotCrossMatch(t *testing.T) {
	s := bandSummary()
	seen := map[string]spendSpan{}
	for span := spendSpan(0); span < numSpendSpans; span++ {
		probe := figureProbe(formatUSDTotal(s.Spans[span].USD))
		if other, dup := seen[probe]; dup {
			t.Errorf("%s and %s both probe as %q, so either cell can satisfy the other's "+
				"whole-figure assertion", spendSpanDefs[other].label, spendSpanDefs[span].label, probe)
		}
		seen[probe] = span
	}
	// And no probe is a substring of another whole figure, which is the same hazard by a longer
	// route: "$70" would match inside "$1,708.20" if the fixtures ever grew a thousands separator.
	for span := spendSpan(0); span < numSpendSpans; span++ {
		probe := figureProbe(formatUSDTotal(s.Spans[span].USD))
		for other := spendSpan(0); other < numSpendSpans; other++ {
			if other == span {
				continue
			}
			if whole := formatUSDTotal(s.Spans[other].USD); strings.Contains(whole, probe) {
				t.Errorf("%s's probe %q appears inside %s's figure %q", spendSpanDefs[span].label,
					probe, spendSpanDefs[other].label, whole)
			}
		}
	}
}

// THE BAND MEASURES AND PADS IN DISPLAY COLUMNS, not in runes — the rule fitStripFigures states
// for this package and footer.go records the cost of breaking.
//
// NO PRODUCTION PATH REACHES THESE VALUES TODAY, and that is why the cells are built by hand: a
// label comes from spendSpanDefs and a value from digits, markers and an em dash, so every cell the
// renderer can currently produce is ASCII-wide. The rule still binds for two reasons — a styled
// cell is one style call away, and this package has already shipped a data-destroying bug by
// measuring styled text with a rune-counting primitive — so it is pinned at the two functions that
// implement it rather than left to a fixture the renderer cannot generate.
//
// WIDTH IS THE WHOLE PAIR, and it is measured off render() so the separating space is counted
// once. It used to be the wider HALF, because the two halves shared a column on separate rows.
//
// THE PADDING HALF OF THIS TEST IS GONE WITH padLeft. A one-line band pads nothing — each cell is
// exactly its own contents — so there is no second function to agree with a measurement. What
// replaced that agreement is render() against renderMuting(): the styled form must occupy the same
// columns as the plain one, or the fit is computed on one band and drawn from another. Asserted
// below with a mute that emits real escape sequences, since an identity function would pass for a
// renderMuting that counted them.
func TestBandCell_MeasuresInDisplayColumns(t *testing.T) {
	restore := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(restore) })

	styled := lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Render("$4.04")
	if styled == "$4.04" {
		t.Fatal("the style emitted no escape sequence, so the styled case asserts nothing")
	}
	mute := func(s string) string {
		return lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render(s)
	}
	if mute("x") == "x" {
		t.Fatal("the mute emitted no escape sequence, so the styling case asserts nothing")
	}

	for _, tc := range []struct {
		name  string
		cell  bandCell
		want  int
		cause string
	}{
		// 7 columns of label, one space, 5 of figure.
		{"ascii", bandCell{label: "LAST 1H", value: "$4.04"}, 13, "label, a space, and figure"},
		{"styled value", bandCell{label: "TODAY", value: styled}, 11,
			"escape bytes are not columns: counting them charges the cell for invisible text and " +
				"the band drops a figure that would have fitted"},
		// Four CJK runes are eight columns and four runes, so a rune count under-measures by four.
		{"wide runes", bandCell{label: "過去一時", value: "$4.04"}, 14,
			"a CJK rune occupies two columns, so counting runes under-measures the cell and the " +
				"band renders wider than the terminal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cell.width(); got != tc.want {
				t.Errorf("width() = %d, want %d — %s", got, tc.want, tc.cause)
			}
			// THE STYLED CELL OCCUPIES THE SAME COLUMNS, which is what lets the drop enumeration
			// run over the plain form and the screen be drawn from the muted one.
			plain, dressed := tc.cell.render(), tc.cell.renderMuting(mute)
			if got, want := lipgloss.Width(dressed), lipgloss.Width(plain); got != want {
				t.Errorf("renderMuting renders %d columns against render's %d — muting a label must "+
					"not move a figure", got, want)
			}
			if dressed == plain {
				t.Error("renderMuting returned the plain cell, so the styling assertion is vacuous")
			}
		})
	}
}

// AND THE WHOLE BAND COSTS THE SAME DRESSED AS IT DOES PLAIN.
//
// bandCell above pins one cell; this pins the line, which is what app.go actually measures against
// the terminal. A separator or an extra pad applied on only one of the two paths would be invisible
// to the per-cell test and would overflow the reservation here.
func TestRenderSpendBand_StylingCostsNoColumns(t *testing.T) {
	restore := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(restore) })

	for w := 1; w <= 120; w++ {
		plain := renderSpendBand(bandSummary(), w)
		dressed, drew := renderSpendBandStyled(bandSummary(), w)
		if len(plain) != spendBandLines || len(dressed) != spendBandLines {
			t.Fatalf("width %d: %d plain rows and %d styled, want %d of each",
				w, len(plain), len(dressed), spendBandLines)
		}
		if got, want := lipgloss.Width(dressed[0]), lipgloss.Width(plain[0]); got != want {
			t.Errorf("width %d: styled band is %d columns against the plain band's %d:\n%q",
				w, got, want, dressed[0])
		}
		// AND `drew` AGREES WITH THE PLAIN LINE AT EVERY WIDTH. paneView gates the drawer on it, so
		// a `drew` that disagreed would open the breakdown under an empty band — the state the
		// styled string cannot be asked about, since an escape sequence is not whitespace.
		if want := strings.TrimSpace(plain[0]) != ""; drew != want {
			t.Errorf("width %d: drew = %v but the plain band is %q", w, drew, plain[0])
		}
	}
	// Not vacuous: at a width that draws something, the styled form really does carry escapes.
	dressed, drew := renderSpendBandStyled(bandSummary(), 200)
	if !drew {
		t.Error("drew is false at width 200, so the agreement above was asserted on empty bands")
	}
	if dressed[0] == renderSpendBand(bandSummary(), 200)[0] {
		t.Error("the styled band is byte-identical to the plain one, so nothing was styled")
	}
}

// AND A CLEAN SPAN WEARS NO MARKER AT ALL, which is the half that was missing.
//
// Every marker test here was positive — set a condition, find the glyph — so a renderer that
// prepended one UNCONDITIONALLY passed all of them. Verified: making moneyAmount always prepend
// inexactMarker left the whole package green, and the same for damagedMarker.
//
// That is pointed rather than theoretical. Removing an unconditional `~` from SAVED is one of this
// branch's headline fixes — a saving marked "estimated" on every row taught operators to read the
// glyph as decoration — and nothing would have caught it coming back. A marker that is always
// present carries no information, and it costs the two markers that ARE informative their meaning.
func TestRenderSpendBand_ACleanSpanCarriesNoMarker(t *testing.T) {
	lines := renderSpendBand(bandSummary(), 200)
	joined := strings.Join(lines, "\n")

	// The fixture is exact, fully priced, unclamped, undamaged and inside retention, so every
	// glyph below would be a claim about nothing.
	for _, tc := range []struct{ marker, means string }{
		{inexactMarker, "estimated — set only by IncompleteRequests"},
		{partialMarker, "a floor — set by an unpriced gap or by days outside retention"},
		{damagedMarker, "short by an unstatable amount — set by a damaged read or a clamp"},
	} {
		if strings.Contains(joined, tc.marker) {
			t.Errorf("a clean band carries %q (%s):\n%s", tc.marker, tc.means, joined)
		}
	}
	// And the figures really are there, or "no markers" would be satisfied by an empty band.
	for span := spendSpan(0); span < numSpendSpans; span++ {
		if want := formatUSDTotal(bandSummary().Spans[span].USD); !strings.Contains(lines[0], want) {
			t.Fatalf("the clean band is missing %s's figure %q, so this test asserted nothing:\n%s",
				spendSpanDefs[span].label, want, joined)
		}
	}
}

// NO CELL THE BAND GAVE UP COULD HAVE BEEN KEPT, at any width.
//
// THIS REPLACES TestRenderSpendBand_RestoresACellThatStillFits, whose premise the fold retired, so
// the reasoning is recorded rather than deleted. That test was written against a pathology of the
// SHARED width: every surviving cell rendered at max(width), so surrendering a wide cell freed room
// for a narrow one the drop loop had already walked past. Measured at the time, with a stale TODAY
// whose label carries an age: the band drew MONTH alone at every width from 16 to 19 while LAST 1H
// and MONTH together needed 16 — two readings' worth of room going unused. bandWidth now sums each
// cell's OWN width, so a cell's cost does not depend on its neighbours and that shape cannot be
// constructed any more.
//
// What still has to hold is the guarantee the enumeration exists for, and it is asserted DIRECTLY
// here instead of at the few widths where the old bug happened to surface: at every width, adding
// back any single dropped cell must overflow. That needs no second copy of the enumeration in the
// test — reimplementing it would only prove the test agrees with itself.
//
// SWEPT, not sampled, and with a stale TODAY: an age on one label is what makes the four cells
// unequal enough for a maximality bug to have anywhere to hide.
func TestRenderSpendBand_NoDroppedCellCouldHaveBeenKept(t *testing.T) {
	stale := withSpan(bandSummary(), spanToday, func(r *spanReading) {
		r.Age, r.Stale = 12*time.Minute, true
	})

	// The cells the renderer itself would build, so the widths under test are the real ones.
	var cells [numSpendSpans]bandCell
	for span := spendSpan(0); span < numSpendSpans; span++ {
		cells[span] = bandSpanCell(spendSpanDefs[span].label, stale.Spans[span])
	}

	for w := 1; w <= 120; w++ {
		line := renderSpendBand(stale, w)[0]
		used := lipgloss.Width(line)
		if used > w {
			t.Errorf("width %d: band rendered %d columns: %q", w, used, line)
			continue
		}
		for span := spendSpan(0); span < numSpendSpans; span++ {
			if strings.Contains(line, spendSpanDefs[span].label) {
				continue
			}
			// A dropped cell would cost its own width, plus a separator unless it would be the
			// only cell on the line.
			cost := cells[span].width()
			if used > 0 {
				cost += bandSeparatorWidth
			}
			if used+cost <= w {
				t.Errorf("width %d: gave up %q, which needs %d more columns with %d of %d used — "+
					"it fits, so the surviving set is not maximal:\n%s",
					w, spendSpanDefs[span].label, cost, used, w, line)
			}
		}
	}
}

// EVERY MONEY SURFACE ROUNDS THE SAME MICROS TO THE SAME CENT, which two of them did not.
//
// usage_render.go rounds integer micros half-up; formatUSDCell used %.2f over a float, which
// rounds the BINARY value — a shade under the exact half for figures like $1.005 — and then
// half-to-even. Measured: 1_005_000 micros printed "COST $1.01" in the headline and "$1.00" in a
// cell, and 9_995_000 printed "$10.00" against "$9.99". usage_render.go's own comment claimed
// these surfaces "cannot disagree"; they agreed about negatives and not about rounding.
//
// THE HALF-CENT CASES ARE THE FIXTURE, because they are the only ones that can differ: the ledger
// and the aggregator both count in micros, so any figure landing exactly on a half-cent is where
// a float copy and the integer part company.
func TestMoneyRounding_TheCellAndTheHeadlineAgree(t *testing.T) {
	for _, micros := range []int64{
		1_005_000,  // $1.005 — half-up is $1.01
		9_995_000,  // $9.995 — half-up is $10.00, and carries into the dollars
		30_935_000, // $30.935 — the band fixture's own figure
		2_345_000,  // a value with no half-cent, as a control
		4_170_000,
	} {
		snap := &usage.Snapshot{Priced: true, Totals: usage.Counts{
			Requests: 1, CostMicros: micros, PricedRequests: 1, PriceableRequests: 1,
		}}
		headline := renderCostSummary(snap)
		cell := formatUSDTotal(float64(micros) / 1e6)
		if !strings.Contains(headline, cell) {
			t.Errorf("%d micros: the pane headline says %q and a table cell says %q — one figure, "+
				"two answers, and an operator comparing two screens cannot tell which is the money",
				micros, headline, cell)
		}
	}
}

// THE SET ONLY EVER GAINS VALUE AS THE TERMINAL GROWS, and MONTH is never hidden while it fits.
//
// Those are the two guarantees, and "no cell ever disappears" is NOT one of them — the three pull
// against each other and this is where the line is drawn. Every surviving cell shares one width, so
// a wide high-priority cell can cost two narrow low-priority ones:
//
//	with a month just over $999.99, ranking by CELL COUNT put the week on screen and hid the
//	month at widths 25-27 — the inversion bandDropOrder exists to prevent, on the figure the
//	whole band is read for
//
//	ranking by VALUE fixes that, and costs one width on a fixture with a five-figure damaged
//	TODAY where three cells become two as the terminal widens by one column
//
// The second is the lesser harm: a cell traded for a more valued one still answers the question the
// band is for, where hiding month-to-date does not. Both directions were measured across four
// fixtures before choosing — the earlier version of this test asserted the count instead, which is
// the guarantee that had to go.
//
// VALUE IS RANKED BY bandDropOrder, most valued highest, so "the set gained value" means it kept
// everything more important than whatever it gave up.
func TestRenderSpendBand_WideningOnlyEverGainsValue(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func() spendSummary
	}{
		{"the healthy fixture", bandSummary},
		{"a month just over $999.99, wider than every other cell", func() spendSummary {
			return withSpan(bandSummary(), spanMonth, func(r *spanReading) { r.USD = 1234.56 })
		}},
		{"a five-figure TODAY wearing all three markers", func() spendSummary {
			return withSpan(bandSummary(), spanToday, func(r *spanReading) {
				r.USD = 17265.97
				r.Incomplete, r.Unpriced, r.Priceable = 3, 40, 100
				r.Degraded = &usage.Degraded{UnreadableDays: 1}
			})
		}},
		{"a stale TODAY, whose label carries an age", func() spendSummary {
			return withSpan(bandSummary(), spanToday, func(r *spanReading) {
				r.Age, r.Stale = 12*time.Minute, true
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.build()

			// The month's own cell width: below it the month cannot be shown at all, and showing
			// something narrower beats showing nothing.
			var cells [numSpendSpans]bandCell
			for span := spendSpan(0); span < numSpendSpans; span++ {
				cells[span] = bandSpanCell(spendSpanDefs[span].label, s.Spans[span])
			}
			monthAlone := cells[spanMonth].width()

			value := func(w int) int {
				line := renderSpendBand(s, w)[0]
				v := 0
				for i := 0; i < int(numSpendSpans); i++ {
					if strings.Contains(line, spendSpanDefs[bandDropOrder[i]].label) {
						v |= 1 << i
					}
				}
				return v
			}

			for w := 1; w < 200; w++ {
				line := renderSpendBand(s, w)[0]
				if w >= monthAlone && !strings.Contains(line, spendSpanDefs[spanMonth].label) {
					t.Errorf("width %d holds the month's own cell (%d columns) and the band hides "+
						"it anyway: %q", w, monthAlone, line)
				}
				if got, next := value(w), value(w+1); next < got {
					t.Errorf("width %d keeps a more valued set than width %d — widening gave up a "+
						"reading for a less important one:\n%s\n%s", w, w+1,
						strings.Join(renderSpendBand(s, w), "\n"),
						strings.Join(renderSpendBand(s, w+1), "\n"))
				}
			}
		})
	}
}
