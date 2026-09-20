package tui

import (
	"math"
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
	lines := renderBars(mkBuckets([]int64{4100, 0, 300}), metricTokens, 80, 0)
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
	lines := renderBars(mkBuckets([]int64{50000, 50}), metricTokens, 80, 0)
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
		for _, line := range renderBars(mkBuckets(vals), metricTokens, width, 0) {
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
	lines := renderBars(mkBuckets(vals), metricTokens, 30, 0)
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
	lines := renderBars(mkBuckets([]int64{0, 0, 0}), metricTokens, 80, 0)
	if len(lines) == 0 {
		t.Fatal("no output for an all-idle window")
	}
	values := lines[len(lines)-1]
	if strings.Count(values, "0") < 3 {
		t.Errorf("all three idle buckets should be marked:\n%q", values)
	}
}

func TestRenderBars_EmptyInput(t *testing.T) {
	if got := renderBars(nil, metricTokens, 80, 0); len(got) != 1 {
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
	lines := renderBars(mkBuckets([]int64{50000, 40000, 30000}), metricTokens, 80, 0)

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
	lines := renderBars(mkBuckets(vals), metricTokens, 80, 0)

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
			want: "COST $1.84",
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
			want: "COST $1.84 (42/57 priced)",
		},
		{
			name: "one priced request out of many",
			snap: usage.Snapshot{
				Totals: usage.Counts{Requests: 100, PriceableRequests: 100, PricedRequests: 1, CostMicros: 500},
				Priced: true,
			},
			want: "COST <$0.01 (1/100 priced)",
		},
		{
			// float64(1_005_000)/1e6 is 1.0049999…; %.2f on it prints $1.00.
			// Integer cent rounding rounds the half-cent up to $1.01.
			name: "half-cent boundary rounds up under integer rounding",
			snap: usage.Snapshot{
				Totals: usage.Counts{Requests: 1, PriceableRequests: 1, PricedRequests: 1, CostMicros: 1_005_000},
				Priced: true,
			},
			want: "COST $1.01",
		},
		{
			// Go's / and % truncate toward zero, so integer-cent rendering
			// on a negative would print "$0.-1".
			name: "negative micros renders unavailable rather than malformed",
			snap: usage.Snapshot{
				Totals: usage.Counts{Requests: 1, PriceableRequests: 1, PricedRequests: 1, CostMicros: -10_000},
				Priced: true,
			},
			want: "COST unavailable",
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

// TestHumanizeCostMicros_NeverExceedsWidth is the money twin of
// TestHumanizeCount_NeverExceedsWidth, and exists for the same reason: the axis gutter
// is laid out against maxCountLabelLen and a wider label wraps the whole chart.
//
// Sweeps every magnitude rather than the plausible ones. A cost axis is the surface
// most likely to meet a number nobody predicted — a mispriced model, a gateway header
// in the wrong unit — and the failure is a broken layout, not a wrong number.
//
// THE SAMPLING SHAPE IS THE TEST. The first version multiplied bases {1,7,999} by
// powers of ten, which cannot express a value that rounds UP ACROSS a branch bound —
// the shape of every real failure here, since each branch divides and rounds. It
// therefore passed against three branches that returned six characters
// ("$10.00", "$1000k", "$1000M"), which is the bug it was written to catch.
//
// So the bounds are swept directly: each branch's upper limit, minus and plus a few
// steps of the unit that branch rounds to. A magnitude sweep still runs beside it for
// the ordinary cases.
func TestHumanizeCostMicros_NeverExceedsWidth(t *testing.T) {
	vals := []int64{0, -1, -1_000_000, 1, 4_999, 5_000, math.MaxInt64}
	for _, base := range []int64{1, 7, 999} {
		for mag := int64(1); mag <= 1_000_000_000_000_000_000; mag *= 10 {
			if base <= (1<<62)/mag {
				vals = append(vals, base*mag)
			}
		}
	}
	// Every branch bound in humanizeCostMicros AS THE CODE WRITES THEM, with the
	// granularity that branch rounds at. Copied from the switch rather than rounded to
	// the nearest power of ten: the bounds are deliberately half a unit below the round
	// number (that is the fix this test drove), so a table listing the round numbers
	// would only reach the real bound by way of its own ±3 steps — passing for a reason
	// unrelated to what it claims to sweep.
	for _, b := range []struct{ bound, step int64 }{
		{5_000, 1},
		{9_995_000, 10_000},                      // cents
		{999_500_000, 1_000_000},                 // whole dollars
		{9_950_000_000, 100_000_000},             // $0.1k
		{999_500_000_000, 1_000_000_000},         // $1k
		{9_950_000_000_000, 100_000_000_000},     // $0.1M
		{999_500_000_000_000, 1_000_000_000_000}, // $1M
	} {
		for k := int64(-3); k <= 3; k++ {
			if v := b.bound + k*b.step; v > 0 {
				vals = append(vals, v)
			}
			// And the half-step below the bound, which is what rounds up across it.
			if v := b.bound + k*b.step - b.step/2; v > 0 {
				vals = append(vals, v)
			}
		}
	}
	for _, v := range vals {
		got := humanizeCostMicros(v)
		if len([]rune(got)) > maxCountLabelLen {
			t.Errorf("humanizeCostMicros(%d) = %q (%d chars), cap is %d",
				v, got, len([]rune(got)), maxCountLabelLen)
		}
	}
}

// TestHumanizeCostMicros_DistinguishesFreeFromUnpriced pins the three states a money
// label has to keep apart, because collapsing any two of them reads as a fact.
//
// Zero micros means "nothing here could be priced" and NOT "this was free" — see
// usage.Counts.CostMicros — so it must not render as "$0.00". A negative total is not
// spend at all. And a positive sub-cent figure must not round down into either one.
func TestHumanizeCostMicros_DistinguishesFreeFromUnpriced(t *testing.T) {
	tests := []struct {
		name   string
		micros int64
		want   string
	}{
		{"unpriced reads as neither free nor spent", 0, "0"},
		{"a negative total is not spend", -5_000_000, "--"},
		{"a sub-cent charge is not free", 1, "<$.01"},
		{"just under a cent", 4_999, "<$.01"},
		{"a cent", 5_000, "$0.01"},
		{"dollars and cents", 1_200_000, "$1.20"},
		{"the float-rounding case renderCostSummary documents", 1_005_000, "$1.01"},
		{"tens of dollars round to whole", 12_400_000, "$12"},
		{"thousands abbreviate", 1_500_000_000, "$1.5k"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := humanizeCostMicros(tt.micros); got != tt.want {
				t.Errorf("humanizeCostMicros(%d) = %q, want %q", tt.micros, got, tt.want)
			}
		})
	}
}

// TestRenderBars_CostFormatsEverySurface is the four-call-site guard.
//
// humanizeCount has four callers — the two y-axes, the value row and the stacked legend
// — and routing only the axis through money formatting is the easy mistake: the axis is
// the visible half, so cost looks right while the value row under each bar still reads
// "1.2M" for $1.20. Asserts the axis and the value row in one render, since both come
// out of renderBars.
func TestRenderBars_CostFormatsEverySurface(t *testing.T) {
	// NO forced color profile here, unlike TestFooterShowsActiveSort. renderBars emits
	// plain glyphs and fmt.Sprintf output with no lipgloss styling of its own, so forcing
	// a profile changes nothing about what this asserts — verified by checking the render
	// for escape bytes with ANSI256 forced and finding none. The footer genuinely styles
	// its indicator, which is why the pin belongs there and not here.
	//
	// A cost fixture rather than mkBuckets, which populates Tokens only — a cost chart
	// over it plots nothing and the assertions below would pass against an empty chart.
	base := time.Date(2026, 9, 4, 23, 24, 0, 0, time.UTC)
	buckets := []usage.Bucket{ // $1.20 then $0.60, in micros
		{At: base, Counts: usage.Counts{Requests: 1, CostMicros: 1_200_000}},
		{At: base.Add(time.Minute), Counts: usage.Counts{Requests: 1, CostMicros: 600_000}},
	}
	lines := renderBars(buckets, metricCost, 80, 0)
	out := stripANSI(strings.Join(lines, "\n"))

	if !strings.Contains(out, "$") {
		t.Fatalf("a cost chart rendered no money anywhere:\n%s", out)
	}
	// The value row carries each bucket's own figure.
	if !strings.Contains(out, "$1.20") {
		t.Errorf("value row did not render $1.20 — cost reached humanizeCount:\n%s", out)
	}
	// And the axis names the unit rather than leaving the reader to guess.
	if !strings.Contains(out, "USD") {
		t.Errorf("chart did not caption its unit:\n%s", out)
	}
	// A raw micros count must appear nowhere: 1_200_000 through humanizeCount is "1.2M".
	if strings.Contains(out, "1.2M") {
		t.Errorf("micros leaked through the count formatter:\n%s", out)
	}
}

// TestRenderBars_RepeatedAxisLabelIsSuppressed pins the de-duplication the bar axis
// gained alongside cost, which applies to every metric rather than only to money.
//
// With a peak of 1 every gridline rounds to the same string, and the axis used to print
// that string on each labelled row — a column of identical numbers that reads as a
// rendering bug rather than as a scale too coarse to divide. renderWhiskers has always
// suppressed the repeat; the bar renderers now match it.
func TestRenderBars_RepeatedAxisLabelIsSuppressed(t *testing.T) {
	lines := renderBars(mkBuckets([]int64{1, 1}), metricTokens, 80, 0)

	// Collect the gutter labels of the plot rows, dropping the axis rule, time labels
	// and value row at the bottom. With a peak of 1 the ten gridlines round to "1" then
	// "0" nine times, so the assertion is that no label repeats — not that only one
	// appears, which would also forbid the legitimate 1-then-0 pair.
	var labels []string
	for _, l := range plotLines(lines)[:len(plotLines(lines))-3] {
		// Lines are TrimRight-ed, so an unlabelled row can be shorter than the gutter —
		// slicing to axisLabel unconditionally panics on exactly the rows being counted.
		plain := stripANSI(l)
		if len(plain) > axisLabel {
			plain = plain[:axisLabel]
		}
		if g := strings.TrimSpace(plain); g != "" {
			labels = append(labels, g)
		}
	}
	seen := map[string]bool{}
	for _, g := range labels {
		if seen[g] {
			t.Errorf("axis label %q appears more than once (labels: %v):\n%s",
				g, labels, stripANSI(strings.Join(lines, "\n")))
		}
		seen[g] = true
	}
	if len(labels) == 0 {
		t.Fatal("no axis labels at all — the fixture is not exercising the gutter")
	}
}
