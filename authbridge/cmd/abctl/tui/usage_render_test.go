package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

func mkBuckets(vals []int64) []usage.Bucket {
	base := time.Date(2026, 9, 4, 23, 24, 0, 0, time.UTC)
	out := make([]usage.Bucket, 0, len(vals))
	for i, v := range vals {
		var reqs int64
		if v > 0 {
			reqs = 1
		}
		out = append(out, usage.Bucket{
			At:     base.Add(time.Duration(i) * time.Minute),
			Counts: usage.Counts{Requests: reqs, Tokens: v},
		})
	}
	return out
}

// The whole reason the value row exists: an idle bucket must print "0", and a
// very small one must print its value. A blank cell for both would make a gap in
// traffic indistinguishable from traffic too small to plot — the exact confusion
// an operator hits when usage looks wrong.
func TestRenderBars_ZeroBucketIsStatedNotBlank(t *testing.T) {
	lines := renderBars(mkBuckets([]int64{4100, 0, 300}), metricTokens, 80)
	values := lines[len(lines)-1]

	if !strings.Contains(values, "0") {
		t.Errorf("value row has no zero marker for the idle bucket:\n%q", values)
	}
	if !strings.Contains(values, "300") {
		t.Errorf("value row lost the small bucket's value:\n%q", values)
	}
	// The small bucket must also draw at least one glyph, or the chart implies
	// no traffic there.
	plot := strings.Join(lines[:len(lines)-3], "\n")
	if !strings.ContainsAny(plot, "▁▂▃▄▅▆▇█") {
		t.Errorf("no bar glyphs rendered at all:\n%s", plot)
	}
}

// A bucket far below the peak still has to be visible. Rounding it to zero rows
// would hide real traffic; the partial blocks exist for exactly this.
func TestRenderBars_SmallValueStillDrawsAGlyph(t *testing.T) {
	// 50 against a 50000 peak is 1/1000 — well under one full row.
	lines := renderBars(mkBuckets([]int64{50000, 50}), metricTokens, 80)
	bottom := lines[len(lines)-4] // last plot row before the axis

	// Two bars: the tall one full, the short one a partial block.
	if !strings.Contains(bottom, "█") {
		t.Errorf("tall bar missing from bottom row:\n%q", bottom)
	}
	if !strings.ContainsAny(bottom, "▁▂▃▄▅▆▇") {
		t.Errorf("small bar rendered no partial block, so it is invisible:\n%q", bottom)
	}
}

// Output must fit the terminal it was given. A line wider than the width wraps
// and destroys the chart.
func TestRenderBars_FitsWidth(t *testing.T) {
	for _, width := range []int{80, 100, 60, 40} {
		vals := make([]int64, 10)
		for i := range vals {
			vals[i] = int64(10000 * (i + 1))
		}
		for _, line := range renderBars(mkBuckets(vals), metricTokens, width) {
			if got := len([]rune(line)); got > width {
				t.Errorf("width %d: line is %d columns wide:\n%q", width, got, line)
			}
		}
	}
}

// A narrow terminal keeps the NEWEST buckets: an operator watching live traffic
// cares about now, not about the start of the window.
func TestRenderBars_NarrowKeepsNewest(t *testing.T) {
	vals := []int64{111, 222, 333, 444, 555, 666, 777, 888, 999, 1000}
	lines := renderBars(mkBuckets(vals), metricTokens, 30)
	values := lines[len(lines)-1]

	// 1000 humanizes to "1.0k".
	if !strings.Contains(values, "1.0k") {
		t.Errorf("newest bucket dropped on a narrow terminal:\n%q", values)
	}
	if strings.Contains(values, "111") {
		t.Errorf("oldest bucket kept instead of newest:\n%q", values)
	}
}

// All-zero data must render without panicking or dividing by a zero peak.
func TestRenderBars_AllZeroIsSafe(t *testing.T) {
	lines := renderBars(mkBuckets([]int64{0, 0, 0}), metricTokens, 80)
	if len(lines) == 0 {
		t.Fatal("no output for an all-idle window")
	}
	values := lines[len(lines)-1]
	if strings.Count(values, "0") < 3 {
		t.Errorf("all three idle buckets should be marked:\n%q", values)
	}
}

func TestRenderBars_EmptyInput(t *testing.T) {
	if got := renderBars(nil, metricTokens, 80); len(got) != 1 {
		t.Errorf("renderBars(nil) = %v, want a single placeholder line", got)
	}
}

// Cost is reserved but unpopulated. The summary must say so rather than render
// $0.00, which reads as "this traffic was free".
func TestRenderUsageSummary_UnpricedSaysUnavailable(t *testing.T) {
	snap := &usage.Snapshot{
		Totals:  usage.Counts{Requests: 100, Errors: 3, Tokens: 5000},
		Priced:  false,
		Buckets: mkBuckets([]int64{5000}),
	}
	got := renderUsageSummary(snap)
	if !strings.Contains(got, "COST unavailable") {
		t.Errorf("unpriced summary should say cost is unavailable, got:\n%q", got)
	}
	if strings.Contains(got, "$0.00") {
		t.Errorf("summary renders $0.00, which reads as free traffic:\n%q", got)
	}
	if !strings.Contains(got, "3.0%") {
		t.Errorf("error rate missing from summary:\n%q", got)
	}
}

// Window latency must be request-weighted, not a mean of per-bucket means — the
// same trap the server avoids when folding.
func TestRenderUsageSummary_LatencyIsRequestWeighted(t *testing.T) {
	snap := &usage.Snapshot{
		Totals: usage.Counts{Requests: 100},
		Buckets: []usage.Bucket{
			{Counts: usage.Counts{Requests: 99}, LatMeanMs: 1000, LatSamples: 99},
			{Counts: usage.Counts{Requests: 1}, LatMeanMs: 5000, LatSamples: 1},
		},
	}
	got := renderUsageSummary(snap)
	// (99*1000 + 1*5000)/100 = 1040ms = 1.04s. Mean-of-means would be 3.00s.
	if !strings.Contains(got, "1.04s") {
		t.Errorf("latency should be 1.04s (request-weighted), got:\n%q", got)
	}
	if strings.Contains(got, "3.00s") {
		t.Errorf("latency is the mean-of-means:\n%q", got)
	}
}

func TestHumanizeCount(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, "0"}, {999, "999"}, {1200, "1.2k"}, {9999, "10.0k"},
		{18900, "18k"}, {48200, "48k"}, {1_500_000, "1.5M"},
		{999_999_999, "999M"},
		{1_000_000_000, "1.0G"}, // was "1000.0M" — 7 chars, broke the layout
		{50_000_000_000, "50G"},
		{1_500_000_000_000, "1.5T"},
		{999_000_000_000_000, "999T"},
	} {
		if got := humanizeCount(tc.in); got != tc.want {
			t.Errorf("humanizeCount(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The width promise is what renderBars lays out against, so it must hold for
// every magnitude an int64 can reach — not just the ones a table happens to list.
func TestHumanizeCount_NeverExceedsWidth(t *testing.T) {
	vals := []int64{0, -1, 9223372036854775807}
	for _, base := range []int64{1, 7, 999} {
		for mag := int64(1); mag <= 1_000_000_000_000_000_000; mag *= 10 {
			if base <= (1<<62)/mag {
				vals = append(vals, base*mag)
			}
		}
	}
	for _, v := range vals {
		if got := humanizeCount(v); len(got) > maxCountLabelLen {
			t.Errorf("humanizeCount(%d) = %q (%d chars), cap is %d",
				v, got, len(got), maxCountLabelLen)
		}
	}
}

// runeIndexAny returns the RUNE index of the first rune from set, or -1.
// Byte offsets are useless here: the block glyphs and box-drawing characters are
// multi-byte, so strings.IndexAny would report a position no terminal column
// corresponds to.
func runeIndexAny(s, set string) int {
	for i, r := range []rune(s) {
		if strings.ContainsRune(set, r) {
			return i
		}
	}
	return -1
}

// The axis must underline the bars, not sit one column off. Previously
// renderAxis wrote axisLabel-2 spaces then "0 ┼" — a 7-column prefix against the
// 6-column gutter every other row uses — so every tick landed one column right
// of the bar above it.
//
// The existing tests could not catch this: they assert substrings and line
// lengths, neither of which changes when a whole row shifts sideways. This
// asserts the column positions that actually make the chart readable.
func TestRenderBars_AxisAlignsWithBars(t *testing.T) {
	lines := renderBars(mkBuckets([]int64{50000, 40000, 30000}), metricTokens, 80)

	// Find the first bar glyph column from a plot row, and the axis row.
	barCol := -1
	for _, l := range lines {
		if c := runeIndexAny(l, "▁▂▃▄▅▆▇█"); c >= 0 {
			barCol = c
			break
		}
	}
	if barCol < 0 {
		t.Fatal("no bar glyphs rendered")
	}

	axis := ""
	for _, l := range lines {
		if strings.ContainsRune(l, '┼') {
			axis = l
			break
		}
	}
	if axis == "" {
		t.Fatal("no axis row rendered")
	}

	// The corner sits in the last gutter column, so the dashes after it begin at
	// the same column the bars do.
	cornerCol := runeIndexAny(axis, "┼")
	if cornerCol != barCol-1 {
		t.Errorf("axis corner at column %d, bars start at %d — dashes begin at %d, "+
			"so ticks are offset from the bars they underline",
			cornerCol, barCol, cornerCol+1)
	}

	// And the first dash must be exactly under the first bar.
	dashCol := runeIndexAny(axis, "─")
	if dashCol != barCol {
		t.Errorf("first axis dash at column %d, first bar at column %d", dashCol, barCol)
	}
}

// Every tick must fall on a bar boundary: barStride columns apart, starting at
// the first bar's column.
func TestRenderBars_TicksFallOnBarBoundaries(t *testing.T) {
	const n = 5
	vals := make([]int64, n)
	for i := range vals {
		vals[i] = int64(1000 * (i + 1))
	}
	lines := renderBars(mkBuckets(vals), metricTokens, 80)

	var axis []rune
	for _, l := range lines {
		if strings.ContainsRune(l, '┼') {
			axis = []rune(l)
			break
		}
	}
	if axis == nil {
		t.Fatal("no axis row")
	}
	for i, r := range axis {
		if r != '┴' {
			continue
		}
		// A tick closes bar k, so it sits at axisLabel + k*barStride + barWidth.
		off := i - axisLabel - barWidth
		if off < 0 || off%barStride != 0 {
			t.Errorf("tick at column %d is not on a bar boundary", i)
		}
	}
}

// The COST cell has three states, and the partial one is the reason
// usage.Counts.PricedRequests exists: cost comes from litellm-budget-track, which
// need not be in the pipeline for all traffic, so a window that mixes priced and
// unpriced requests is normal. Showing its subtotal bare would read as total spend.
func TestRenderCostSummary(t *testing.T) {
	tests := []struct {
		name string
		snap usage.Snapshot
		want string
	}{
		{
			name: "nothing priced",
			snap: usage.Snapshot{Totals: usage.Counts{Requests: 40}, Priced: false},
			want: "COST unavailable",
		},
		{
			name: "fully priced omits the coverage note",
			snap: usage.Snapshot{
				Totals: usage.Counts{Requests: 40, PriceableRequests: 40, PricedRequests: 40, CostMicros: 1_842_100},
				Priced: true,
			},
			want: "COST $1.8421",
		},
		// PriceableRequests is the denominator now, not Requests: the latter counts
		// non-inference traffic that can never be priced, so a correct deployment read
		// as permanently partial. These fixtures set both to the same value, which is
		// the all-inference case they were written for.
		{
			name: "partially priced names the coverage",
			snap: usage.Snapshot{
				Totals: usage.Counts{Requests: 57, PriceableRequests: 57, PricedRequests: 42, CostMicros: 1_842_100},
				Priced: true,
			},
			want: "COST $1.8421 (42/57 priced)",
		},
		{
			name: "one priced request out of many",
			snap: usage.Snapshot{
				Totals: usage.Counts{Requests: 100, PriceableRequests: 100, PricedRequests: 1, CostMicros: 500},
				Priced: true,
			},
			want: "COST $0.0005 (1/100 priced)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderCostSummary(&tc.snap); got != tc.want {
				t.Errorf("renderCostSummary() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The cell bypassed formatUSDCell and formatted the amount itself, so the floor that
// exists to stop a real charge printing as free was not applied here. One cache-read-only
// request is $0.000038 — 100 cache-read tokens at a typical rate — and it rendered "COST
// $0.0000", which is the exact reading this feature forbids everywhere else.
func TestRenderCostSummary_SubFloorChargeIsNotRenderedAsFree(t *testing.T) {
	snap := usage.Snapshot{
		Priced: true,
		Totals: usage.Counts{Requests: 1, PriceableRequests: 1, PricedRequests: 1, CostMicros: 38},
	}
	got := renderCostSummary(&snap)

	if strings.Contains(got, "$0.0000") {
		t.Errorf("rendered %q — a real charge printed as free", got)
	}
	if want := "COST <$0.0001"; got != want {
		t.Errorf("rendered %q, want %q from the shared money formatter", got, want)
	}
}

// A SETTLED zero still renders as one: the gateway declared the traffic free, which is a
// known answer and the reason the floor is "below the floor" rather than "any small
// number". Pins the other side of the line so the fix cannot become "never print zero".
func TestRenderCostSummary_SettledZeroStillReadsAsZero(t *testing.T) {
	snap := usage.Snapshot{
		Priced: true,
		Totals: usage.Counts{Requests: 1, PriceableRequests: 1, PricedRequests: 1, CostMicros: 0},
	}
	if got := renderCostSummary(&snap); got != "COST $0.0000" {
		t.Errorf("rendered %q, want %q for a settled zero", got, "COST $0.0000")
	}
}

// An inexact total must say so here too. usage.Counts.IncompleteRequests means the figure
// is a lower bound or an approximation, and this cell showed it identically to an exact
// one — the same floor the CLI already refuses to publish as a total.
func TestRenderCostSummary_InexactTotalIsMarkedAndCounted(t *testing.T) {
	snap := usage.Snapshot{
		Priced: true,
		Totals: usage.Counts{
			Requests: 40, PriceableRequests: 40, PricedRequests: 40,
			CostMicros: 1_842_100, IncompleteRequests: 4,
		},
	}
	got := renderCostSummary(&snap)

	if !strings.Contains(got, inexactMarker+"$1.8421") {
		t.Errorf("rendered %q — the figure is a lower bound and carries no marker", got)
	}
	if !strings.Contains(got, "4 of 40 inexact") {
		t.Errorf("rendered %q — the pane has room to say how many, and does not", got)
	}
}

// And an exact one carries neither, so the marker keeps meaning something.
func TestRenderCostSummary_ExactTotalCarriesNoMarker(t *testing.T) {
	snap := usage.Snapshot{
		Priced: true,
		Totals: usage.Counts{Requests: 40, PriceableRequests: 40, PricedRequests: 40, CostMicros: 1_842_100},
	}
	got := renderCostSummary(&snap)

	if strings.Contains(got, inexactMarker) {
		t.Errorf("rendered %q — an exact total wears the inexactness marker", got)
	}
	if strings.Contains(got, "inexact") {
		t.Errorf("rendered %q — a caveat with nothing to act on", got)
	}
}

// TestRenderCostSummary_ANegativeTotalIsUnavailableNotACredit.
//
// The Usage pane's cost cell, a money surface the review did not name and which had the
// same hole as the two it did: it renders through formatUSDCell, which is faithful about
// the sign, so the footer read "COST $-5.0000".
//
// Unavailable, matching costTotalSection: an impossible figure is not a number to display
// and certainly not a credit. Leaving one surface unguarded is how the guarantee stops
// holding — the same reasoning that put negativeCost in one place.
func TestRenderCostSummary_ANegativeTotalIsUnavailableNotACredit(t *testing.T) {
	snap := &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{Requests: 10, CostMicros: -5_000_000,
			PricedRequests: 10, PriceableRequests: 10},
		Priced: true,
	}

	got := renderCostSummary(snap)
	if strings.Contains(got, "$-") {
		t.Errorf("cost cell %q renders a negative amount", got)
	}
	if !strings.Contains(got, "unavailable") {
		t.Errorf("cost cell %q neither showed a figure nor declined one", got)
	}
}
