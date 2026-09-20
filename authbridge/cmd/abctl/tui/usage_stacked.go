package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// unlabelledLabel names the share of a bucket that no series claims. It reaches
// the chart as its own band and the legend as its own entry, so a bar whose
// height exceeds what its labels account for says so.
const unlabelledLabel = "(unlabelled)"

// maxNamedSeries is how many series get their own mark and legend entry before
// the rest fold together. Bounded by the palette so no two NAMED series share a
// colour.
//
// The two synthetic bands are outside that bound: renderStackedBars appends
// "(unlabelled)" and foldTailSeries can add "(other)", so with the palette full
// their ranks wrap onto the first two named colours. Harmless by the design in
// usage_glyphs.go — the letter is the encoding, and `·` and `o` still tell them
// apart from any model — but the colour alone does not distinguish them, which is
// why this says "named" rather than "every".
var maxNamedSeries = len(seriesPalette)

// seriesKey is one label's total across the window, used to decide segment order
// and which labels get their own glyph.
type seriesKey struct {
	label string
	total int64
}

// tailLabel collects series past maxNamedSeries. Matches the aggregator's own
// overflow name (usage.overflowLabel) so an operator sees one vocabulary whether
// the folding happened server-side, from label-cardinality capping, or here for
// palette reasons.
const tailLabel = "(other)"

// foldTailSeries collapses everything past keep into a single tailLabel series,
// rewriting the buckets to match.
//
// Folding before drawing, rather than only in the legend, is what makes every
// band decodable: marks come from the palette and repeat once it wraps, so a
// seventh series could draw with the same mark as the first while the legend
// named neither. Returns the buckets unchanged when nothing needs folding, so the
// common case allocates nothing.
func foldTailSeries(buckets []usage.Bucket, series []seriesKey, keep int) ([]seriesKey, []usage.Bucket) {
	if len(series) <= keep {
		return series, buckets
	}
	tail := make(map[string]bool, len(series)-keep)
	var tailTotal int64
	for _, s := range series[keep:] {
		tail[s.label] = true
		tailTotal += s.total
	}
	// Existing "(other)" from the aggregator's own capping merges in rather than
	// colliding: two bands both meaning "the rest" would be indefensible.
	kept := append([]seriesKey(nil), series[:keep]...)
	for i := range kept {
		if kept[i].label == tailLabel {
			kept[i].total += tailTotal
			tailTotal = 0
		}
	}
	if tailTotal > 0 {
		kept = append(kept, seriesKey{label: tailLabel, total: tailTotal})
	}

	out := make([]usage.Bucket, len(buckets))
	for i, b := range buckets {
		out[i] = b
		if len(b.Series) == 0 {
			continue
		}
		merged := make(map[string]usage.Counts, keep+1)
		var acc usage.Counts
		for label, c := range b.Series {
			if tail[label] {
				// usage.Counts.Add rather than summing the fields here: this used to
				// be a field-by-field copy, which silently dropped PricedRequests
				// when that field was added — under a comment asserting every field
				// was carried. One summation, next to the struct, cannot drift.
				acc.Add(c)
				continue
			}
			merged[label] = c
		}
		if acc.Requests > 0 || acc.Tokens > 0 || acc.Errors > 0 {
			cur := merged[tailLabel]
			cur.Add(acc)
			merged[tailLabel] = cur
		}
		out[i].Series = merged
	}
	return kept, out
}

// unlabelledTotal is how much of the window no series claims, summed across
// buckets that actually show a remainder band. Returns 0 when the labels account
// for everything, or over-account for it (per-plugin attribution does).
func unlabelledTotal(buckets []usage.Bucket, m usageMetric, series []seriesKey, peak int64) int64 {
	var out int64
	for _, b := range buckets {
		var sum int64
		for _, s := range series {
			sum += m.valueOf(b.Series[s.label])
		}
		d := m.value(b) - sum
		if d <= 0 {
			continue
		}
		// Count it only when this bucket would actually DRAW the band. allotRows
		// requires the remainder to fill at least one row, so summing every
		// shortfall here named "(unlabelled)" in the legend for a chart that never
		// draws it — a key to a band that is not there.
		if drawsRemainderBand(d, m.value(b), barRowsFor(m.value(b), peak)) {
			out += d
		}
	}
	return out
}

// drawsRemainderBand reports whether an unclaimed share of a bucket is large
// enough to occupy a row.
//
// One predicate with two callers on purpose: allotRows decides whether to DRAW the
// band and unlabelledTotal decides whether to NAME it in the legend, and when
// those two disagreed the legend keyed a band the chart never drew. They had
// already drifted once — truncating in one and rounding in the other — which is
// the whole reason this is a named function.
//
// Rounds rather than truncates: on a one-row bar 940/1000 truncates to zero rows,
// so the remainder was excluded and the single row went to a named series holding
// 3% of the bucket.
func drawsRemainderBand(remainder, total, barRows int64) bool {
	if remainder <= 0 || total <= 0 || barRows <= 0 {
		return false
	}
	return 2*remainder*barRows >= total
}

// barRowsFor is a bar's height in whole rows. Shared by the renderer and by
// unlabelledTotal so the legend's idea of which bands are drawn cannot drift from
// the chart's.
func barRowsFor(total, peak int64) int64 {
	if total <= 0 || peak <= 0 {
		return 0
	}
	rows := total * int64(plotRows) / peak
	if rows == 0 {
		rows = 1 // non-zero traffic never renders as an empty column
	}
	return rows
}

// collectSeries totals each label across every bucket and returns them largest
// first, with ties broken by label so the legend and the stack order are stable
// between renders. An unstable order would make the chart appear to reshuffle on
// each 20s poll even when nothing changed.
func collectSeries(buckets []usage.Bucket, m usageMetric) []seriesKey {
	totals := map[string]int64{}
	for _, b := range buckets {
		for label, c := range b.Series {
			totals[label] += m.valueOf(c)
		}
	}
	out := make([]seriesKey, 0, len(totals))
	for label, total := range totals {
		out = append(out, seriesKey{label: label, total: total})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].total != out[j].total {
			return out[i].total > out[j].total
		}
		return out[i].label < out[j].label
	})
	return out
}

// isErrorStatus reports whether a group=status label denotes a failure, so it can
// be drawn in red.
//
// Only meaningful for GroupStatus: a model name or plugin name has no status to
// read. Callers gate on the grouping rather than this returning false for
// everything else, because "429" is a plausible model name in a way that should
// not silently colour a method chart red.
func isErrorStatus(label string) bool {
	if label == "denied" {
		return true // a rejected request never got a status code
	}
	// Labels are produced by strconv.Itoa on the status code, so a leading 4 or 5
	// with three digits is the whole test.
	if len(label) != 3 {
		return false
	}
	return label[0] == '4' || label[0] == '5'
}

// renderStackedBars draws one bar per bucket, split into segments by series.
//
// Segments are whole cells, not eighths: a fractional top cell cannot also encode
// a segment boundary, so the ungrouped renderer keeps sub-row precision and this
// one trades it for the breakdown. That is the reason "ungrouped" is its own
// cycle state rather than a special case of grouping.
func renderStackedBars(buckets []usage.Bucket, m usageMetric, group usage.Group, width, height int) []string {
	if len(buckets) == 0 {
		return []string{"  (no data)"}
	}
	maxBars := (width - axisLabel) / barStride
	if maxBars < 1 {
		maxBars = 1
	}
	if len(buckets) > maxBars {
		buckets = buckets[len(buckets)-maxBars:]
	}

	series := collectSeries(buckets, m)
	if len(series) == 0 {
		// Grouped view with no labelled traffic yet: fall back to the ungrouped
		// bars rather than an empty frame, so the pane still shows the volume it
		// does know about.
		return renderBars(buckets, m, width, height)
	}

	// Fold everything past maxNamedSeries into one band BEFORE drawing, so every
	// band on the chart has a legend entry. Drawing each of them with its own mark
	// while the legend named only the first few left bands nothing could decode —
	// the marks repeat once the palette wraps, so two unrelated series could even
	// share one.
	series, buckets = foldTailSeries(buckets, series, maxNamedSeries)

	var peak int64
	for _, b := range buckets {
		if v := m.value(b); v > peak {
			peak = v
		}
	}

	// The unlabelled remainder is drawn as a band but is not in `series`, so
	// register it for marks, colour and the legend. Appended last so it ranks
	// behind every named series and cannot take a palette slot from one.
	legendSeries := series
	if unlabelled := unlabelledTotal(buckets, m, series, peak); unlabelled > 0 {
		legendSeries = append(append([]seriesKey(nil), series...),
			seriesKey{label: unlabelledLabel, total: unlabelled})
	}

	// Rank each label once so segment order is identical in every bucket. A stack
	// whose layers reorder between adjacent bars is unreadable.
	rank := make(map[string]int, len(legendSeries))
	for i, s := range legendSeries {
		rank[s.label] = i
	}

	// One mark per series, assigned once for the whole chart so a letter means the
	// same thing in every bucket and in the legend.
	letters := assignLetters(legendSeries)

	out := make([]string, 0, plotRows+5)
	if caption := axisCaption(m, width, height); caption != "" {
		out = append(out, caption)
	}
	lastAxisLabel := ""
	for row := plotRows; row >= 1; row-- {
		var sb strings.Builder
		// Suppressing a repeat, as renderBars and renderWhiskers both do: when the peak
		// is small every gridline rounds to the same string, and a column of identical
		// labels reads as a bug rather than as a collapsed scale.
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
		for _, b := range buckets {
			sb.WriteString(stackedCell(b, m, group, series, rank, letters, peak, row))
			sb.WriteString(strings.Repeat(" ", barGap))
		}
		out = append(out, strings.TrimRight(sb.String(), " "))
	}

	out = append(out, renderAxis(len(buckets)))
	out = append(out, renderTimeLabels(buckets))
	out = append(out, renderValues(buckets, m))
	out = append(out, "")
	out = append(out, renderLegend(legendSeries, group, m, letters, rank, width)...)
	return out
}

// stackedCell renders one bar's glyphs for one row, choosing the segment whose
// cumulative height covers this row.
func stackedCell(b usage.Bucket, m usageMetric, group usage.Group,
	series []seriesKey, rank map[string]int, letters map[string]rune,
	peak int64, row int) string {

	total := m.value(b)
	if total <= 0 || peak <= 0 {
		return strings.Repeat(" ", barWidth)
	}
	// Whole rows only — see the renderStackedBars comment on why segments cannot
	// use partial blocks.
	barRows := barRowsFor(total, peak)
	if int64(row) > barRows {
		return strings.Repeat(" ", barWidth)
	}

	// Walk the allotment bottom-up until we pass this row.
	var acc int64
	for _, alloc := range allotRows(b, m, series, barRows, total) {
		acc += alloc.rows
		if int64(row) <= acc {
			return paintMark(alloc.label, letters, rank, group)
		}
	}
	// Rounding left this row uncovered: attribute it to the largest series rather
	// than punching a hole in the middle of a bar.
	return paintMark(series[0].label, letters, rank, group)
}

// rowAlloc is one series' share of a bar, in whole rows.
type rowAlloc struct {
	label string
	value int64 // this series' metric value in the bucket
	rows  int64
}

// allotRows divides a bar's rows among the series present in it.
//
// Every present series gets at least one row, so a model with a rounding-error
// share of the traffic is still visible — "claude-haiku at 912 tokens against
// 2.2M" is exactly the case an operator wants to spot, and a segment floored to
// zero rows makes it indistinguishable from absent.
//
// The floor cannot simply be applied per series, which is what the previous
// version did: with three series in a ten-row bar the largest took 9 rows and the
// two floored ones landed on rows 10 and 11, so the eleventh fell outside the bar
// and its series vanished anyway. Guaranteed rows are reserved FIRST and the
// remainder shared out proportionally, so the total always fits.
func allotRows(b usage.Bucket, m usageMetric, series []seriesKey, barRows, total int64) []rowAlloc {
	present := make([]rowAlloc, 0, len(series)+1)
	var seriesSum int64
	for _, s := range series {
		if v := m.valueOf(b.Series[s.label]); v > 0 {
			present = append(present, rowAlloc{label: s.label, value: v})
			seriesSum += v
		}
	}
	// Traffic no label claims gets its own band rather than being absorbed by the
	// named series. The bar's height comes from the bucket total, so silently
	// sharing the unlabelled remainder out drew a bucket that is 10%
	// claude-sonnet-5 as a solid `s` bar — the height said "lots of traffic" and
	// every row of it claimed to be sonnet.
	//
	// Considered BEFORE the empty check below, not after. Returning early on
	// seriesSum == 0 reached the same misattribution by another route: a bucket
	// with traffic but no labelled series got no allotment at all, stackedCell
	// exhausted its empty loop and fell through to painting every row as
	// series[0], while unlabelledTotal counted that bucket and keyed an
	// (unlabelled) band the chart never drew. That is the chart/legend divergence
	// drawsRemainderBand exists to prevent, inverted. Reachable in the by-plugin
	// view by any bucket whose turns invoked no plugin.
	//
	// Only when the shortfall is large enough to occupy a row: rounding noise does
	// not deserve a band, and a one-row remainder on every bar would be more
	// misleading than omitting it.
	if unlabelled := total - seriesSum; drawsRemainderBand(unlabelled, total, barRows) {
		present = append(present, rowAlloc{label: unlabelledLabel, value: unlabelled})
		seriesSum += unlabelled
	}
	// Nothing to draw: neither a labelled series nor a remainder worth a row.
	if len(present) == 0 || seriesSum == 0 {
		return nil
	}
	// More series than rows: the bar cannot show them all, so give a row each to
	// as many as fit, largest first (series is already sorted). The legend still
	// names the rest.
	if int64(len(present)) >= barRows {
		// Keep the LARGEST bands, not the first barRows of them. present ends with
		// the unlabelled remainder, so truncating positionally dropped exactly the
		// band that keeps the bar honest: a 3-row bar that was 94% unclaimed
		// reattributed all of it to the named series, which is the misattribution
		// the remainder exists to prevent.
		byValue := append([]rowAlloc(nil), present...)
		sort.SliceStable(byValue, func(i, j int) bool { return byValue[i].value > byValue[j].value })
		keep := make(map[string]bool, barRows)
		for _, a := range byValue[:barRows] {
			keep[a.label] = true
		}
		out := make([]rowAlloc, 0, barRows)
		for _, a := range present { // preserve stacking order
			if keep[a.label] {
				a.rows = 1
				out = append(out, a)
			}
		}
		return out
	}

	// Proportions are taken against seriesSum, NOT the bucket total. The two are
	// not the same number in either direction:
	//
	//   - Under: a bucket can carry traffic no label claims, so the labelled
	//     series may sum to a fraction of the total. Dividing by the total then
	//     under-allots every series and the leftover rows all went to the largest,
	//     drawing a bucket that is 10% claude-sonnet-5 as a solid `s` bar.
	//   - Over: per-plugin attribution counts one request once per plugin that
	//     ran, so byPlugin sub-totals intentionally sum to MORE than the bucket
	//     (see the aggregator's foldInto). Dividing by the total then over-allotted
	//     rows, `acc` ran past barRows, and whole series fell off the top of the
	//     chart — by-plugin did this on every bucket.
	//
	// Normalising against the sum of what is actually drawn makes the shares add
	// up by construction, whichever way the totals disagree.
	surplus := barRows - int64(len(present))
	var assigned int64
	for i := range present {
		present[i].rows = 1 + present[i].value*surplus/seriesSum
		assigned += present[i].rows
	}
	// Integer division leaves rows unassigned; give them to the largest series so
	// the bar reaches its full height.
	if rem := barRows - assigned; rem > 0 {
		present[0].rows += rem
	}
	return present
}

// segmentStyle reports whether a series should be drawn as an error, separated
// from the rendering so tests can assert the DECISION rather than the bytes.
//
// lipgloss strips colour when it detects no TTY, which is always true under `go
// test`, so asserting on ANSI escapes cannot verify this. Returning the intent
// keeps the rule testable and leaves styling to one call site.
func isErrorSeries(label string, group usage.Group) bool {
	return group == usage.GroupStatus && isErrorStatus(label)
}

// paintMark renders one series' cell: its letter, repeated across the bar width,
// on the series background.
//
// Repeated rather than centred so the segment reads as a solid band — a single
// letter floating in a 4-column cell looks like a data point, not a share of a
// stack.
func paintMark(label string, letters map[string]rune, rank map[string]int, group usage.Group) string {
	r, ok := letters[label]
	if !ok {
		r = '?'
	}
	return seriesStyle(rank[label], isErrorSeries(label, group)).
		Render(strings.Repeat(string(r), barWidth))
}

// paintSegment applies the error style to legend text when the series calls for
// it. Foreground only: a coloured background belongs on the chart marks, where it
// encodes the series, not on a line of prose.
func paintSegment(text, label string, group usage.Group) string {
	if isErrorSeries(label, group) {
		return styleError.Render(text)
	}
	return text
}

// renderLegend keys the chart: each series' mark, name and total. Error statuses
// are coloured to match their segments, so the legend is the key to the chart
// rather than a separate vocabulary.
//
// Returns one or more lines. Wrapping rather than eliding matters because the mark
// is the only way to identify a band — a series dropped from the legend leaves an
// unreadable segment on the chart, and model names are long enough
// ("claude-haiku-4-5-20251001") that three of them do not fit 80 columns on one
// line. Only series past maxNamedSeries fold into a count, and those share a
// colour anyway.
func renderLegend(series []seriesKey, group usage.Group, m usageMetric,
	letters map[string]rune, rank map[string]int, width int) []string {
	const sep = "   "
	const indent = "  "

	// No cap here. foldTailSeries has already reduced the series to at most
	// maxNamedSeries named bands plus a "(other)" fold, and re-capping at the same
	// number cut that fold off — the largest unnamed band was drawn on the chart
	// and replaced in the legend by "(+1 more)", which named nothing. Whatever
	// reaches this function is what the chart draws, so all of it gets an entry.
	named := series

	var lines []string
	var parts []string
	plain := len(indent)

	flush := func() {
		if len(parts) > 0 {
			lines = append(lines, indent+strings.Join(parts, sep))
			parts = nil
			plain = len(indent)
		}
	}

	for _, s := range named {
		mark := seriesStyle(rank[s.label], isErrorSeries(s.label, group)).
			Render(string(letters[s.label]))
		text := fmt.Sprintf(" %s (%s)", s.label, m.label(s.total))
		// Measured on the plain text: the mark is one column however many bytes of
		// escape sequence it carries.
		cost := 1 + len([]rune(text))
		if len(parts) > 0 {
			cost += len(sep)
		}
		if plain+cost > width && len(parts) > 0 {
			flush()
			cost = 1 + len([]rune(text))
		}
		// A single entry can still exceed the width with nothing to wrap against —
		// one long model name on a narrow terminal. Truncate the NAME rather than
		// the whole entry, so the mark and the total survive: those are what
		// identify the band and say how much it is, and a legend line wider than
		// the terminal wraps and destroys the chart above it.
		if plain+cost > width {
			text = truncateLegendText(text, width-plain-1)
			cost = 1 + len([]rune(text))
		}
		parts = append(parts, mark+paintSegment(text, s.label, group))
		plain += cost
	}
	flush()

	if len(lines) == 0 {
		return nil
	}
	return lines
}

// truncateLegendText shortens a legend entry's text to fit, preserving the
// trailing "(total)" so the entry still says how much the band is worth.
//
// The name is what gets cut, with an ellipsis marking it, because a name is
// recognisable from a prefix while a truncated number is simply wrong.
func truncateLegendText(text string, max int) string {
	r := []rune(text)
	if max <= 0 {
		return ""
	}
	if len(r) <= max {
		return text
	}
	// Keep the parenthesised total if there is room for it plus a token name.
	if i := strings.LastIndex(text, " ("); i > 0 {
		total := text[i:]
		tr := []rune(total)
		if keep := max - len(tr) - 1; keep > 1 {
			return string(r[:keep]) + "…" + total
		}
	}
	if max == 1 {
		return "…"
	}
	return string(r[:max-1]) + "…"
}
