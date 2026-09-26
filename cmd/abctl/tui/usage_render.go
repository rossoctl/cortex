package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rossoctl/cortex/core/usage"
)

// Bar geometry. Four columns wide with a one-column gap, so ten bars occupy 49
// columns and fit an 80-column terminal alongside the axis gutter.
//
// The gap earns its column twice over: adjacent bars blur into one mass at the
// tall end, and in a stacked view a boundary BETWEEN bars would otherwise be
// indistinguishable from a boundary WITHIN one. Four is also the practical floor
// for stacked segments — narrower and the block glyphs stop reading as distinct
// textures, which is what carries the encoding when color is unavailable.
const (
	barWidth = 4
	// barGap is 2, not 1, so a value label always has a separating column. Labels
	// are up to 5 wide ("48.2k", "1.2ms"); at a stride of 5 two adjacent labels
	// touched and read as one nonsense number ("1.2ms0"). Widening the gap keeps
	// every label — alternating them instead would have dropped the newest
	// bucket's value on a narrow terminal and hidden the "0" that distinguishes an
	// idle minute from a small one. Ten bars at stride 6 plus the gutter is 66
	// columns, still inside 80.
	barGap    = 2
	barStride = barWidth + barGap
	plotRows  = 10 // vertical resolution before partial blocks
	axisLabel = 6  // "  50k " gutter
)

// eighths are the vertical partial blocks, 1/8 through 8/8. They give the
// ungrouped view 8x the vertical resolution of whole cells for free: a bar's top
// cell renders its fractional row rather than rounding away up to a full row of
// value.
var eighths = [...]rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// usageMetric selects which series the bar chart plots.
type usageMetric int

const (
	metricTokens usageMetric = iota
	metricRequests
	metricErrors
	metricLatency
	metricCost

	// usageMetricCount bounds the [m] cycle. Kept adjacent to the iota block so
	// adding a metric means editing one line here.
	//
	// It derives the count from its OWN position, so a metric has to be added above
	// this line. One added below compiles, cycles nowhere, and is reachable only by
	// editing the config file by hand.
	usageMetricCount = iota
)

func (m usageMetric) String() string {
	switch m {
	case metricRequests:
		return "requests"
	case metricErrors:
		return "errors"
	case metricLatency:
		return "latency"
	case metricCost:
		return "cost"
	default:
		return "tokens"
	}
}

// isLatency reports whether this metric needs the whiskers renderer rather than
// bars. Latency is a distribution, not a magnitude from zero, so it gets a
// different form — see the comment on the whisker glyphs.
func (m usageMetric) isLatency() bool { return m == metricLatency }

// isCost reports whether this metric's values are money rather than a count, so
// every label surface formats them as money. Cost still plots as BARS — it is a
// magnitude from zero, unlike latency — so this gates formatting only, never the
// choice of renderer.
func (m usageMetric) isCost() bool { return m == metricCost }

// label renders one value of this metric for an axis tick, a value row or a legend
// entry, in at most maxCountLabelLen characters.
//
// ONE function behind all four of those surfaces, because the failure mode when they
// disagree is silent: cost reaching humanizeCount renders 1_200_000 micros as "1.2M",
// which is a plausible-looking token count rather than $1.20.
//
// TRUNCATES rather than trusting its formatters, because an over-wide label does not
// look like a formatting bug when it reaches the screen. The axis writes labels with
// "%5s ", which WIDENS to six columns rather than clipping, so one long label shifts
// every bar on that row one column right of the rows above it and the axis rule below;
// the value row, laid out at barStride 6, runs its label into its neighbour instead.
// Both read as a chart-drawing bug a long way from the formatter that caused it — and
// three cost branches did exactly this before their bounds were fixed. A clipped label
// is wrong in one cell and obvious; a shifted chart is wrong everywhere and is not.
func (m usageMetric) label(v int64) string {
	var s string
	if m.isCost() {
		s = humanizeCostMicros(v)
	} else {
		s = humanizeCount(v)
	}
	if len([]rune(s)) > maxCountLabelLen {
		return string([]rune(s)[:maxCountLabelLen])
	}
	return s
}

// valueOf extracts this metric from a per-label Counts. Bucket embeds Counts, so
// value below delegates here — one place decides what each metric means, and a
// stacked segment can never disagree with the bar it sits inside.
func (m usageMetric) valueOf(c usage.Counts) int64 {
	switch m {
	case metricRequests:
		return c.Requests
	case metricErrors:
		return c.Errors
	case metricCost:
		return c.CostMicros
	default:
		return c.Tokens
	}
}

// value extracts this metric from a bucket.
func (m usageMetric) value(b usage.Bucket) int64 {
	return m.valueOf(b.Counts)
}

// unit is the abbreviated unit for this metric's values, for the axis caption.
//
// Abbreviated because it has to fit the axis gutter, which is axisLabel wide and laid
// out against a 5-character label promise — "requests" would not fit and widening the
// gutter shifts every bar, time label and value in all three renderers.
func (m usageMetric) unit() string {
	switch m {
	case metricRequests:
		return "req"
	case metricErrors:
		return "err"
	case metricLatency:
		// UNREACHABLE AS A CAPTION and kept anyway: renderUsageChart routes latency to
		// renderWhiskers, which draws no caption because humanizeDurationMs puts the unit
		// in every label already. Kept so unit() is total over the enum — plotLines in the
		// tests iterates every metric to recognise a caption line, and a metric with no
		// unit would make that iteration lie rather than fail.
		return "ms"
	case metricCost:
		return "USD"
	default:
		return "tok"
	}
}

// axisCaptionWidth is the terminal width at which the unit caption appears.
//
// Chosen so the caption costs no COLUMNS: below it, every column is already spoken for
// by the chart itself. 66 is the width the bar geometry is documented against — ten bars
// at stride 6 plus the gutter — so at or above it the caption fits horizontally.
const axisCaptionWidth = 66

// The irreducible height of each renderer that can carry a caption, in rows, BEFORE the
// caption's own row. Each renderer passes its own — a single shared floor is what the
// first version of the height gate used, and the stacked renderer is two rows taller
// than the bar one, so the gate opened at a budget of 14 for a renderer needing 15 and
// the caption tipped the pane past the terminal at 66x25, 66x27 and 80x27.
const (
	// barChartFloor: ten plot rows plus the axis rule, the time labels and the value row.
	barChartFloor = plotRows + 3
	// stackedChartFloor: the same, plus the blank separator and at least one legend line.
	// renderLegend emits one line per wrap and never zero, so one is its minimum.
	stackedChartFloor = barChartFloor + 2
)

// axisCaption is the unit caption line, or "" when the terminal cannot spare it.
//
// Right-aligned INTO the gutter rather than centred over it: the labels below are
// rendered with %5s in a 6-column field, so aligning to their right edge puts the unit
// directly over the digits it qualifies.
//
// GATED ON HEIGHT AS WELL AS WIDTH, because the caption costs a ROW and the pane has a
// fixed row budget. Width alone was the first version and it broke the repo's own fit
// invariant at 80x24 — one of fitSizes — by exactly the one row it adds. The width gate
// made that look safe: 65 columns fit and 66 did not, which is the caption appearing
// rather than anything about columns.
//
// The height passed in is the rows available TO THE CHART, not the pane's whole budget:
// renderUsage spends rows on its header, its blank lines and the summary beneath, and at
// 80x24 that remainder leaves the chart exactly its own height with nothing spare. The
// caller subtracts what it spends; see usageChartHeight.
//
// A height of 0 means "unknown", which is what every pure renderer test passes; those
// get the caption, since a test measuring columns is not measuring a terminal.
//
// floor is the CALLER's irreducible height, not a shared constant: see barChartFloor /
// stackedChartFloor for why one shared number was wrong.
//
// Not called by renderWhiskers, deliberately — humanizeDurationMs already carries the
// unit in every label, so a fixed "ms" above them would contradict labels reading
// "4.1s". See the comment at the top of that renderer.
func axisCaption(m usageMetric, width, height, floor int) string {
	if width < axisCaptionWidth {
		return ""
	}
	if height > 0 && height < floor+1 {
		return ""
	}
	return fmt.Sprintf("%*s", maxCountLabelLen, m.unit())
}

// renderBars draws the ungrouped bar chart: a y-axis with humanized labels, one
// bar per bucket using partial blocks for sub-row precision, a time axis, and a
// value row.
//
// Pure by design — buckets in, lines out, no terminal and no model state — so
// the exact glyph output is table-testable. Terminal rendering bugs are
// otherwise only visible by eye, and the two cases that matter most (an idle
// bucket versus a very small one; a fractional top cell) are precisely the ones
// eyes skip over.
func renderBars(buckets []usage.Bucket, m usageMetric, width, height int) []string {
	if len(buckets) == 0 {
		return []string{"  (no data)"}
	}
	// Fit as many bars as the terminal allows, keeping the newest: an operator
	// watching live traffic cares about now, not about the start of the window.
	maxBars := (width - axisLabel) / barStride
	if maxBars < 1 {
		maxBars = 1
	}
	if len(buckets) > maxBars {
		buckets = buckets[len(buckets)-maxBars:]
	}

	var peak int64
	for _, b := range buckets {
		if v := m.value(b); v > peak {
			peak = v
		}
	}

	out := make([]string, 0, plotRows+4)
	if caption := axisCaption(m, width, height, barChartFloor); caption != "" {
		out = append(out, caption)
	}

	// Plot rows, top down. Each row is a threshold; a bar fills the row when its
	// value reaches the row's ceiling, and renders a partial block when it lands
	// inside the row.
	lastAxisLabel := ""
	for row := plotRows; row >= 1; row-- {
		var sb strings.Builder
		// Label every other row, matching the axis tick density below. The label is
		// formatted only on the rows that can carry one — the guard used to sit after the
		// call, formatting ten values to use five.
		labelled := false
		if row%2 == 0 && peak > 0 {
			if label := m.label(peak * int64(row) / int64(plotRows)); label != lastAxisLabel {
				lastAxisLabel = label
				sb.WriteString(fmt.Sprintf("%5s ", label))
				labelled = true
			}
		}
		if !labelled {
			sb.WriteString(strings.Repeat(" ", axisLabel))
		}
		sb.WriteString(barCellsForRow(buckets, m, peak, row))
		out = append(out, strings.TrimRight(sb.String(), " "))
	}

	out = append(out, renderAxis(len(buckets)))
	out = append(out, renderTimeLabels(buckets))
	out = append(out, renderValues(buckets, m))
	return out
}

// barCellsForRow renders one horizontal slice of every bar.
func barCellsForRow(buckets []usage.Bucket, m usageMetric, peak int64, row int) string {
	var sb strings.Builder
	for _, b := range buckets {
		v := m.value(b)
		sb.WriteString(barCell(v, peak, row))
		sb.WriteString(strings.Repeat(" ", barGap))
	}
	return sb.String()
}

// barCell renders one bar's glyphs for one row: full blocks below the value, a
// partial block at the fractional boundary, spaces above.
func barCell(v, peak int64, row int) string {
	if v <= 0 || peak <= 0 {
		return strings.Repeat(" ", barWidth)
	}
	// Height in eighths of a row, so a bar shorter than one row still shows.
	//
	// NOT OVERFLOW-GUARDED, deliberately and after checking. The product overflows int64
	// past MaxInt64/80 ≈ 1.15e17, which for the cost metric is a single bucket holding
	// $115 billion of spend and for tokens is 1.15e17 tokens in one window — neither is
	// reachable from any aggregator this reads. The expression is byte-identical to what
	// the merge-base computes; adding cost as a metric did not widen the domain, because
	// CostMicros shares the same int64 range every other count already had. Left alone
	// rather than wrapped in a saturating multiply: a guard here would be untestable
	// except by constructing the unreachable input, and it would read to a later reader
	// as evidence that something once overflowed in practice.
	totalEighths := v * int64(plotRows) * 8 / peak
	// Floor at one eighth: integer division truncates a small-but-nonzero value
	// to nothing (50 against a 50k peak is 0.08 eighths), which would render
	// real traffic as an empty column — indistinguishable from the idle bucket
	// the value row deliberately marks "0". A visible sliver is the honest
	// answer; invisibility is not.
	if totalEighths == 0 {
		totalEighths = 1
	}
	rowFloor := int64(row-1) * 8
	inThisRow := totalEighths - rowFloor
	switch {
	case inThisRow >= 8:
		return strings.Repeat("█", barWidth)
	case inThisRow <= 0:
		return strings.Repeat(" ", barWidth)
	default:
		return strings.Repeat(string(eighths[inThisRow-1]), barWidth)
	}
}

// renderAxis draws the baseline. The corner glyph sits in the LAST gutter column
// so the dashes after it start at axisLabel — the column the bars start at.
// Writing axisLabel-2 spaces then "0 ┼" placed the corner AT axisLabel and
// shifted every tick one column right of the bar it underlines.
func renderAxis(n int) string {
	var sb strings.Builder
	// Right-align "0" in the gutter, leaving the final cell for the corner.
	sb.WriteString(strings.Repeat(" ", axisLabel-3))
	sb.WriteString("0 ┼")
	for i := 0; i < n; i++ {
		sb.WriteString(strings.Repeat("─", barWidth))
		if i < n-1 {
			// One tick plus dashes for the rest of the gap, so the tick stays on
			// the bar boundary and the baseline remains continuous whatever barGap
			// is set to.
			sb.WriteString("┴")
			sb.WriteString(strings.Repeat("─", barGap-1))
		}
	}
	return sb.String()
}

// renderTimeLabels labels every other bucket. A full HH:MM does not fit under a
// 4-column bar, so the first label carries the hour and the rest are minutes
// only — enough to place a spike once the reader has the hour.
func renderTimeLabels(buckets []usage.Bucket) string {
	// Painted into a column-indexed buffer rather than appended: a label is
	// wider than the 4-column bar it belongs to, so consecutive labels would
	// otherwise push each other rightward and drift out of alignment with the
	// bars they name. Placing by absolute column keeps every label under its own
	// bar no matter how wide the previous one was.
	row := make([]byte, axisLabel+len(buckets)*barStride+8)
	for i := range row {
		row[i] = ' '
	}
	for i, b := range buckets {
		if i%2 != 0 {
			continue
		}
		label := b.At.Format(":04")
		if i == 0 {
			label = b.At.Format("15:04")
		}
		at := axisLabel + i*barStride
		if at+len(label) <= len(row) {
			copy(row[at:], label)
		}
	}
	return strings.TrimRight(string(row), " ")
}

// renderValues prints each bucket's value under its bar. An idle bucket prints
// "0" rather than blank: a chart with a missing bar cannot otherwise distinguish
// no traffic from traffic too small to plot, and that is exactly the gap an
// operator is hunting when usage looks wrong.
func renderValues(buckets []usage.Bucket, m usageMetric) string {
	// Column-painted for the same reason as renderTimeLabels: a 5-character
	// value under a 4-column bar would otherwise shove its neighbours right.
	row := make([]byte, axisLabel+len(buckets)*barStride+8)
	for i := range row {
		row[i] = ' '
	}
	for i, b := range buckets {
		v := m.value(b)
		label := "0" // an idle bucket is stated, never blank
		if v != 0 {
			label = m.label(v)
		}
		at := axisLabel - 1 + i*barStride
		if at+len(label) <= len(row) {
			copy(row[at:], label)
		}
	}
	return strings.TrimRight(string(row), " ")
}

// maxCountLabelLen is the width humanizeCount promises. renderBars lays out the
// axis gutter and value row against it, so exceeding it pushes lines past the
// terminal width and wraps the chart.
const maxCountLabelLen = 5

// humanizeCount renders a count in at most maxCountLabelLen characters.
//
// Every magnitude has to be covered, not just the plausible ones: the previous
// version fell through to "%.1fM" above a million, so a billion tokens rendered
// as "1000.0M" — seven characters — and a large enough total silently broke the
// layout. Token counts over a 6h window are exactly the figure that grows without
// anyone revisiting this function.
func humanizeCount(v int64) string {
	switch {
	case v < 0:
		return "0"
	case v < 1000:
		return fmt.Sprintf("%d", v) // 0..999
	case v < 10_000:
		return fmt.Sprintf("%.1fk", float64(v)/1000) // 1.0k..9.9k
	case v < 1_000_000:
		return fmt.Sprintf("%dk", v/1000) // 10k..999k
	case v < 10_000_000:
		return fmt.Sprintf("%.1fM", float64(v)/1e6) // 1.0M..9.9M
	case v < 1_000_000_000:
		return fmt.Sprintf("%dM", v/1e6) // 10M..999M
	case v < 10_000_000_000:
		return fmt.Sprintf("%.1fG", float64(v)/1e9) // 1.0G..9.9G
	case v < 1_000_000_000_000:
		return fmt.Sprintf("%dG", v/1e9) // 10G..999G
	case v < 10_000_000_000_000:
		return fmt.Sprintf("%.1fT", float64(v)/1e12) // 1.0T..9.9T
	case v < 1_000_000_000_000_000:
		return fmt.Sprintf("%dT", v/1e12) // 10T..999T
	default:
		// Past 999T an int64 has about four decades left; clamp rather than
		// widen, since a chart is unreadable long before this.
		return ">999T"
	}
}

// renderUsageSummary is the footer line: totals plus latency and cost.
func renderUsageSummary(snap *usage.Snapshot) string {
	var parts []string
	parts = append(parts, fmt.Sprintf("REQUESTS %d", snap.Totals.Requests))

	errPct := ""
	if snap.Totals.Requests > 0 {
		errPct = fmt.Sprintf(" (%.1f%%)", 100*float64(snap.Totals.Errors)/float64(snap.Totals.Requests))
	}
	parts = append(parts, fmt.Sprintf("ERRORS %d%s", snap.Totals.Errors, errPct))
	parts = append(parts, fmt.Sprintf("TOKENS %s", humanizeCount(snap.Totals.Tokens)))

	// Latency across the window, weighted by request count so a quiet minute
	// does not count as much as a busy one (the same mean-of-means trap the
	// server avoids when folding).
	var latSum float64
	var latN int64
	for _, b := range snap.Buckets {
		// Weight by LatSamples (requests that carried a duration), not Requests:
		// an unmeasured response is traffic but not a latency sample, and using
		// Requests understates the mean in proportion to how many there were.
		if b.LatMeanMs > 0 && b.LatSamples > 0 {
			latSum += b.LatMeanMs * float64(b.LatSamples)
			latN += b.LatSamples
		}
	}
	if latN > 0 {
		parts = append(parts, fmt.Sprintf("LATENCY %.2fs", latSum/float64(latN)/1000))
	}

	parts = append(parts, renderCostSummary(snap))
	return "  " + strings.Join(parts, "    ")
}

// renderCostSummary is the COST cell, in one of three states.
//
// Nothing priced: say so rather than render $0.00, which reads as "this traffic
// was free" — a zero cost and an unknown cost are different answers.
//
// Partially priced: show the figure AND the coverage, because the total only
// covers the requests that carried a cost. Cost comes from litellm-budget-track,
// which may not be in the pipeline for all traffic and cannot price every
// endpoint, so a window mixing priced and unpriced requests is the normal case
// rather than an edge one. Presenting its subtotal as if it were the whole spend
// is the failure this coverage count exists to prevent.
func renderCostSummary(snap *usage.Snapshot) string {
	if !snap.Priced {
		// Not "$0.00". A zero cost and an unknown cost are different answers, and
		// only one of them means the traffic was free.
		return "COST unavailable"
	}
	// The cent arithmetic and the sub-cent floor live in formatUSDTotalMicros, with the rule that
	// says which surfaces get cents. They were stated here and nowhere else, which is how this
	// panel came to round to cents while the band above it showed the same money to four decimals.
	micros := snap.Totals.CostMicros
	var cell string
	if negativeCost(micros) {
		// Go's / and % truncate toward zero, so a negative would render "$0.-1" through the
		// integer-cent arithmetic. Through the shared predicate so this surface and the spend
		// strip cannot disagree about what an impossible figure is.
		cell = "COST unavailable"
	} else {
		// Micros, not the float: the integer is already in hand, and the float64 entry point
		// exists for callers that only have dollars.
		cell = "COST " + formatUSDTotalMicros(micros)
	}
	// Compared against PRICEABLE requests, not all of them. Requests counts every
	// proxied response — MCP tool calls, health checks, anything else the sidecar
	// handled — while only inference can ever be priced, so the old ratio left a
	// correctly configured deployment reading "1/10 priced" forever with an empty gap
	// list. A permanent warning with nothing to act on trains an operator to ignore
	// the one signal that matters.
	// Provenance qualifies the figure: $12.40 from a gateway's own numbers and $12.40
	// modelled from a shipped vendor-list table are not equally trustworthy, and the
	// column showed them identically. Only shown when it tells the reader something —
	// a single uniform provenance that is authoritative needs no annotation.
	cell += provenanceNote(snap.PricedBy)

	priceable := snap.Totals.PriceableRequests
	if priceable == 0 || snap.Totals.PricedRequests >= priceable {
		return cell
	}
	// Partial coverage: the total covers only the priced subset, so say so, and
	// name what is missing. "Cost is incomplete" is not actionable; the endpoint and
	// model are exactly what an operator needs to write a pricing entry for.
	cell += fmt.Sprintf(" (%d/%d priced", snap.Totals.PricedRequests, priceable)
	if gaps := topUnpriced(snap.UnpricedBy, 3); gaps != "" {
		cell += "; unpriced: " + gaps
	}
	return cell + ")"
}

// topUnpriced names the biggest coverage gaps, largest first, capped so one
// pathological deployment cannot push the rest of the line off screen.
//
// Ordered by count rather than alphabetically because an operator fixing coverage
// wants the entry that buys the most first. Ties break on the key so the line does
// not reshuffle between refreshes for no reason.
func topUnpriced(by map[string]int64, max int) string {
	if len(by) == 0 {
		return ""
	}
	keys := make([]string, 0, len(by))
	for k := range by {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if by[keys[i]] != by[keys[j]] {
			return by[keys[i]] > by[keys[j]]
		}
		return keys[i] < keys[j]
	})
	shown := keys
	if len(shown) > max {
		shown = shown[:max]
	}
	parts := make([]string, 0, len(shown))
	for _, k := range shown {
		parts = append(parts, fmt.Sprintf("%s x%d", sanitizeLabel(k), by[k]))
	}
	out := strings.Join(parts, ", ")
	if rest := len(keys) - len(shown); rest > 0 {
		out += fmt.Sprintf(", +%d more", rest)
	}
	return out
}

// usageWindows are the spans the [w] key cycles.
var usageWindows = []struct {
	window     time.Duration
	resolution time.Duration
}{
	{10 * time.Minute, time.Minute},
	{time.Hour, 5 * time.Minute},
	{6 * time.Hour, 30 * time.Minute},
}

// provenanceNote renders where a cost total came from, or "" when saying so would
// add nothing.
//
// Silent for a wholly authoritative total, because "the gateway told us" is the
// baseline a reader already assumes. Loud for anything modelled, and loudest when
// mixed — a partly-modelled total is the case where a reader most needs to know
// which part to trust.
func provenanceNote(by map[string]int64) string {
	if len(by) == 0 {
		return ""
	}
	if len(by) == 1 {
		for k := range by {
			if k == "authoritative" {
				return ""
			}
			return " [" + k + "]"
		}
	}
	// Mixed: list every level, largest first, so the dominant source reads first.
	keys := make([]string, 0, len(by))
	for k := range by {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if by[keys[i]] != by[keys[j]] {
			return by[keys[i]] > by[keys[j]]
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, by[k]))
	}
	return " [" + strings.Join(parts, ", ") + "]"
}

// sanitizeLabel makes a wire-derived label safe to write to a terminal.
//
// These keys are "<endpoint> <model>", and the model half comes from the `model` field
// of the request body — chosen by the workload, and the inference parser records it
// verbatim. Writing it straight to a TTY lets an escape sequence reposition the cursor,
// recolour the pane, or erase the very coverage gap it is reporting; a newline alone
// breaks the table apart. CWE-150.
//
// Control characters and DEL become U+FFFD rather than being dropped, so tampering is
// visible instead of silently producing a plausible-looking label.
func sanitizeLabel(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == 0x7f, r < 0x20:
			b.WriteRune('\uFFFD')
		// C1 controls (U+0080-U+009F). Mostly inert in a UTF-8 terminal, since they arrive as
		// two bytes rather than as the single bytes an escape parser acts on — but they are
		// controls, they are not printable, and there is no reason for one to survive into a
		// label. Replaced for the same reason as C0.
		case r >= 0x80 && r <= 0x9f:
			b.WriteRune('\uFFFD')
		// BIDI OVERRIDES AND ISOLATES: U+202A-U+202E and U+2066-U+2069. These are the ones that
		// actually render: each is zero-width, so the width math stays self-consistent and the
		// frame never breaks, but the terminal REORDERS the text around it. A title can then
		// display in an order that is not the order of its bytes — "report\u202Egnp.exe" reads as
		// something else entirely — and these titles are LLM-generated transcript text from an
		// unauthenticated-by-nature file, so their content is not ours to trust.
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			b.WriteRune('\uFFFD')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// snapshotDamaged reports that the read behind a snapshot's totals was INCOMPLETE — rows
// the answer needed could not be read at all, so the figures are SHORT by an amount
// nothing in the response can state.
//
// A DIFFERENT CLAIM from usage.Counts.IncompleteRequests, and the two must never be merged
// or shown under one marker. That counter says a figure the response CARRIES is inexact —
// a floor from a stream that died before its output count, or a gateway's approximation —
// and the request is still counted and still priced. This says rows are MISSING FROM THE
// SUM. One qualifies a number; the other says a number is absent from it.
// usage.Snapshot.Degraded's own doc draws that line; damagedMarker is the separate glyph it
// requires.
//
// PRESENCE is the claim, not the counters. The field is a pointer precisely so a clean read
// serialises nothing, so absence means the read was clean and NOTHING may be rendered for
// it. A present object whose counters are both zero still reports damage: a producer that
// sent the object is saying it found some, and reading zeros as "checked, fine" is the same
// class of false reassurance as $0.00 over unpriced traffic.
//
// Only a LEDGER-backed window can populate it. The in-memory ring has no lines to fail to
// decode and no files to abandon, so a duration window leaves it nil and that absence is
// the truth rather than a gap in the reporting. Which is why two money surfaces do NOT
// consult it: the sessions COST cell is summed out of the strip's fixed-hour ring window,
// and renderCostSummary's pane cycles usageWindows, all of which are durations. The strip's
// TODAY figure and `abctl cost` are the surfaces that can be ledger-backed, and they are
// the ones that render it.
//
// A pointer parameter rather than a *usage.Snapshot, so the strip can ask about the day
// figure's own disclosure without carrying the whole snapshot into spendSummary.
func snapshotDamaged(d *usage.Degraded) bool { return d != nil }

// negativeCost reports that a published cost figure cannot be spend, and is the ONE
// spelling of that test on every money surface in this package.
//
// A negative total is not a total. The session API refuses to publish one — cost is a
// sum of per-request figures that are themselves non-negative — so this can only fire
// against a broken or hostile producer. THE GUARANTEE IS UPSTREAM, in
// core/sessionapi, and nothing here re-derives it: this is DEFENCE IN DEPTH, a
// refusal to render a figure that contradicts a promise made on the other side of the
// wire.
//
// It is a function rather than inline comparisons because it was three inline
// comparisons short of existing: renderCostSummary above made the test and "$-5.0000"
// still reached the spend strip, where a minus sign in a column of costs reads as a
// refund nobody issued.
//
// Every caller treats it as UNPRICED, never as a small or clamped figure. An impossible
// number is not a number to display, and $0.00 would assert that the traffic was free
// — the one claim this whole surface exists to refuse.
func negativeCost(micros int64) bool { return micros < 0 }

// humanizeCostMicros renders a cost in at most maxCountLabelLen characters, for the
// chart's axis ticks, value row and legend.
//
// Same money rules as renderCostSummary — negatives are not spend, and a positive
// sub-cent figure must not read as free — but a chart cannot spend five columns on
// the word "unavailable", so each of those states gets a glyph the gutter can hold.
//
// The magnitudes above a dollar abbreviate rather than widen, for the reason
// humanizeCount's doc gives: the gutter is laid out against a 5-character promise and
// a wider label wraps the chart. That costs precision on the axis, which is the right
// trade — the exact total is one line below in the summary.
//
// The cents branch defers to formatUSDTotalMicros, which is the whole point: a bar and
// the summary total beneath it must not round the same money two different ways. This
// branch originally carried its own copy of that arithmetic; #1077 moved the one
// authoritative copy into prune_saving.go under the precision rule, so the copy is gone
// and the sharing now runs through that. Note the sub-cent and negative cases are
// screened ABOVE, so the only inputs reaching it are the non-negative ones its own doc
// requires — and its cents output is at most "$9.99", five characters, inside the gutter's
// promise only because that branch stops half a cent BELOW $10 rather than at it; see
// the bound comment inside the switch.
func humanizeCostMicros(micros int64) string {
	switch {
	case negativeCost(micros):
		// Not "$0.00": an impossible figure is not a small one. Through the shared
		// predicate so this and the summary agree on what impossible means.
		//
		// UNPADDED, like every other branch. These two used to return "  --" and "   0",
		// right-aligned for the axis — which the axis does not need, since it writes
		// labels with "%5s", and which the other two surfaces got wrong: renderValues and
		// the legend place the string themselves, so copy(row[at:], label) copied the
		// padding too and put "--" two columns right of the bar it labels.
		return "--"
	case micros == 0:
		// Zero micros is "nothing here could be priced", NOT "this was free" — see
		// usage.Counts.CostMicros. A bare "$0.00" would assert the second.
		return "0"
	case micros < 5_000:
		// "<$.01", a character narrower than formatUSDTotalMicros' own "<$0.01" floor,
		// because five is all the gutter has. Same rule, spelled for the width.
		return "<$.01"
	// EVERY BOUND BELOW IS SET WHERE ROUNDING OVERFLOWS, not at the round number above
	// it. Each branch divides and rounds to nearest, so a value just under a power of ten
	// rounds UP ACROSS it: $9.995 through the cents branch is "$10.00", and $999,500k is
	// "$1000k" — six characters against a five-character gutter. Bounding at the round
	// number tests as correct for every value except the handful that actually break.
	//
	// The rule: subtract half the unit the branch rounds to. Same lesson as
	// humanizeDurationMs's 9.95ms, applied at every bound rather than one.
	case micros < 9_995_000: // under $10: cents matter
		return formatUSDTotalMicros(micros)
	case micros < 999_500_000: // $10..$999
		return fmt.Sprintf("$%d", (micros+500_000)/1_000_000)
	case micros < 9_950_000_000: // $1.0k..$9.9k
		return fmt.Sprintf("$%.1fk", float64(micros)/1e9)
	case micros < 999_500_000_000: // $10k..$999k
		return fmt.Sprintf("$%dk", (micros+500_000_000)/1_000_000_000)
	case micros < 9_950_000_000_000: // $1.0M..$9.9M
		return fmt.Sprintf("$%.1fM", float64(micros)/1e12)
	case micros < 999_500_000_000_000: // $10M..$999M
		return fmt.Sprintf("$%dM", (micros+500_000_000_000)/1_000_000_000_000)
	default:
		// Past $999M an int64 of micros has little room left; clamp rather than
		// widen, as humanizeCount does.
		return ">$1G"
	}
}
