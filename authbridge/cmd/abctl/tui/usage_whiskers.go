package tui

import (
	"fmt"
	"strings"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// Whisker glyphs. The mean is a crossbar, the +/-1σ caps are tees, and the span
// between them is a vertical rule.
//
// A bar chart is the wrong form for latency and this is why: a bar encodes
// magnitude from a zero baseline, but mean latency has no meaningful zero — a
// response taking 0ms is not the absence of a response — and the spread is
// usually the more interesting half. Drawing the mean with its dispersion says
// "typical, plus how much it varies", which is the question an operator actually
// has.
const (
	whiskerMean = '┼'
	whiskerCap  = '┬' // upper cap, +1σ
	whiskerFoot = '┴' // lower cap, -1σ
	whiskerRule = '│'
)

// latencyRow is one bucket reduced to the three numbers the chart draws.
type latencyRow struct {
	bucket usage.Bucket
	// mean and the sigma band, in milliseconds. lo is clamped at zero: a mean of
	// 200ms with a 500ms sigma would otherwise draw a cap below the axis, which
	// reads as negative latency.
	mean, lo, hi float64
	// measured is how many requests carried a duration. Zero means this bucket
	// has no latency to draw at all, which is not the same as zero latency.
	measured int64
}

// latencyRows projects buckets into plottable rows.
func latencyRows(buckets []usage.Bucket) []latencyRow {
	out := make([]latencyRow, 0, len(buckets))
	for _, b := range buckets {
		r := latencyRow{bucket: b, measured: b.LatSamples}
		if b.LatSamples > 0 && b.LatMeanMs > 0 {
			r.mean = b.LatMeanMs
			r.hi = b.LatMeanMs + b.LatStdDevMs
			r.lo = b.LatMeanMs - b.LatStdDevMs
			if r.lo < 0 {
				r.lo = 0
			}
		}
		out = append(out, r)
	}
	return out
}

// renderWhiskers draws mean-with-whiskers latency per bucket.
//
// Scaled to the highest +1σ rather than the highest mean, so a cap is never
// clipped off the top of the frame — a whisker that runs past the axis tells the
// reader less than one that fits.
func renderWhiskers(buckets []usage.Bucket, width int) []string {
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

	rows := latencyRows(buckets)

	var peak float64
	for _, r := range rows {
		if r.hi > peak {
			peak = r.hi
		}
	}
	if peak <= 0 {
		// No bucket carried a duration. Say so rather than draw an empty grid that
		// looks like latency was zero.
		return []string{
			"  (no latency samples in this window)",
			"",
			"  Latency is recorded per response; a window with no measured",
			"  responses has nothing to plot.",
		}
	}

	out := make([]string, 0, plotRows+4)
	// NO UNIT CAPTION HERE, unlike the bar renderers. humanizeDurationMs already puts
	// the unit in every label and picks it per magnitude — "4.1s", "820ms" — so a
	// fixed "ms" caption above them is not merely redundant, it contradicts the labels
	// it sits over as soon as the axis reaches a second. The caption exists for the
	// count metrics, whose labels are bare numbers.
	lastAxisLabel := ""
	for row := plotRows; row >= 1; row-- {
		var sb strings.Builder
		if row%2 == 0 {
			label := humanizeDurationMs(peak * float64(row) / float64(plotRows))
			// Suppress a repeat: when the peak is small every gridline rounds to
			// the same string, and a column of identical labels reads as a bug
			// rather than as a collapsed scale.
			if label == lastAxisLabel {
				sb.WriteString(strings.Repeat(" ", axisLabel))
			} else {
				lastAxisLabel = label
				sb.WriteString(fmt.Sprintf("%5s ", label))
			}
		} else {
			sb.WriteString(strings.Repeat(" ", axisLabel))
		}
		for _, r := range rows {
			sb.WriteString(whiskerCell(r, peak, row))
			sb.WriteString(strings.Repeat(" ", barGap))
		}
		out = append(out, strings.TrimRight(sb.String(), " "))
	}

	out = append(out, renderAxis(len(rows)))
	out = append(out, renderTimeLabels(buckets))
	out = append(out, renderLatencyValues(rows))
	out = append(out, "")
	legend := fmt.Sprintf("  %c mean   %c +1σ   %c −1σ   (0 = no measured responses)",
		whiskerMean, whiskerCap, whiskerFoot)
	// Drop the parenthetical before the glyph key: on a narrow terminal the key is
	// what makes the chart legible, and a wrapped line breaks the layout outright.
	if len([]rune(legend)) > width {
		legend = fmt.Sprintf("  %c mean   %c +1σ   %c −1σ", whiskerMean, whiskerCap, whiskerFoot)
	}
	if r := []rune(legend); len(r) > width {
		legend = string(r[:width])
	}
	out = append(out, legend)
	return out
}

// whiskerCell renders one bucket's glyph for one row. Centred in the bar width so
// the marks line up with the bars in the other two views.
func whiskerCell(r latencyRow, peak float64, row int) string {
	blank := strings.Repeat(" ", barWidth)
	if r.measured == 0 || r.mean <= 0 {
		return blank
	}

	// Which plot row each of the three marks falls on. Ceil so a small non-zero
	// value lands on row 1 rather than row 0 and disappears.
	rowOf := func(v float64) int {
		if v <= 0 {
			return 1
		}
		n := int((v/peak)*float64(plotRows) + 0.999)
		if n < 1 {
			n = 1
		}
		if n > plotRows {
			n = plotRows
		}
		return n
	}
	meanRow, hiRow, loRow := rowOf(r.mean), rowOf(r.hi), rowOf(r.lo)

	var g rune
	switch {
	case row == meanRow:
		g = whiskerMean // mean wins where marks collide: it is the headline number
	case row == hiRow:
		g = whiskerCap
	case row == loRow:
		g = whiskerFoot
	case row > loRow && row < hiRow:
		g = whiskerRule
	default:
		return blank
	}

	// Centre the mark: (barWidth-1)/2 leading spaces puts it under the middle of
	// the bar column the other renderers fill.
	lead := (barWidth - 1) / 2
	return strings.Repeat(" ", lead) + string(g) + strings.Repeat(" ", barWidth-lead-1)
}

// renderLatencyValues prints each bucket's mean under its mark, or "0" where no
// response was measured — the same idle-versus-small distinction the bar chart's
// value row makes.
func renderLatencyValues(rows []latencyRow) string {
	row := make([]byte, axisLabel+len(rows)*barStride+8)
	for i := range row {
		row[i] = ' '
	}
	for i, r := range rows {
		label := "0"
		if r.measured > 0 && r.mean > 0 {
			label = humanizeDurationMs(r.mean)
		}
		at := axisLabel - 1 + i*barStride
		if at+len(label) <= len(row) {
			copy(row[at:], label)
		}
	}
	return strings.TrimRight(string(row), " ")
}

// maxDurationLabelLen is the width humanizeDurationMs promises, matching
// humanizeCount so both renderers share one gutter geometry.
const maxDurationLabelLen = 5

// humanizeDurationMs renders a millisecond duration in at most
// maxDurationLabelLen characters.
//
// Every magnitude is covered rather than only the plausible ones — the same
// lesson as humanizeCount, which promised 5 characters and returned 7 above a
// billion because nobody revisited the fallthrough.
func humanizeDurationMs(ms float64) string {
	switch {
	case ms <= 0:
		return "0"
	case ms < 1:
		return "<1ms"
	case ms < 9.95:
		// Bounded below 9.95, not 10: %.1f rounds 9.99 up to "10.0ms", which is six
		// characters and breaks the width promise the gutter is laid out against.
		return fmt.Sprintf("%.1fms", ms) // 1.0ms..9.9ms
	case ms < 999.5:
		// Rounded, not truncated. int64(9.99) is 9, so the previous version
		// reported 9.99ms as "9ms" — a value rounding DOWN past a whole
		// millisecond, which is a worse error than the wide label the 9.95 bound
		// above exists to avoid. Same reasoning in each integer branch below.
		return fmt.Sprintf("%dms", int64(ms+0.5)) // 10ms..999ms
	case ms < 9_950:
		return fmt.Sprintf("%.1fs", ms/1000) // 1.0s..9.9s
	case ms < 599_500:
		return fmt.Sprintf("%ds", int64(ms/1000+0.5)) // 10s..599s
	case ms < 35_970_000:
		return fmt.Sprintf("%dm", int64(ms/60_000+0.5)) // 10m..599m
	default:
		return ">10h"
	}
}
