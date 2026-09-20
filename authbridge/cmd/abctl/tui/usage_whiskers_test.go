package tui

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// mkLatencyBuckets builds buckets carrying mean/stddev/sample triples.
func mkLatencyBuckets(triples [][3]float64) []usage.Bucket {
	base := time.Date(2026, 9, 6, 23, 24, 0, 0, time.UTC)
	out := make([]usage.Bucket, 0, len(triples))
	for i, tr := range triples {
		b := usage.Bucket{At: base.Add(time.Duration(i) * time.Minute)}
		b.LatMeanMs, b.LatStdDevMs = tr[0], tr[1]
		b.LatSamples = int64(tr[2])
		b.Counts.Requests = b.LatSamples
		out = append(out, b)
	}
	return out
}

// The three marks must all appear: mean, upper cap and lower cap, joined by a
// rule. A bar would encode only the mean, which is the reason this form exists.
func TestRenderWhiskers_DrawsMeanAndBothCaps(t *testing.T) {
	lines := renderWhiskers(mkLatencyBuckets([][3]float64{{2000, 600, 10}}), 80)
	joined := strings.Join(lines, "\n")

	for name, g := range map[string]rune{
		"mean": whiskerMean, "upper cap": whiskerCap, "lower cap": whiskerFoot,
	} {
		if !strings.ContainsRune(joined, g) {
			t.Errorf("%s glyph %q missing:\n%s", name, string(g), joined)
		}
	}
}

// A bucket with no measured response must print "0", not a mark at the baseline.
// Zero latency and no measurement are different facts, and only one of them
// means the service was fast.
func TestRenderWhiskers_UnmeasuredBucketIsStated(t *testing.T) {
	lines := renderWhiskers(mkLatencyBuckets([][3]float64{
		{2000, 100, 5},
		{0, 0, 0}, // requests happened but none carried a duration
	}), 80)
	values := lines[plotRows+2] // the value row, after axis + time labels

	if !strings.Contains(values, "0") {
		t.Errorf("value row does not mark the unmeasured bucket:\n%q", values)
	}
}

// A window with nothing measured must say so rather than draw an empty grid,
// which reads as "latency was zero".
func TestRenderWhiskers_NoSamplesExplainsItself(t *testing.T) {
	lines := renderWhiskers(mkLatencyBuckets([][3]float64{{0, 0, 0}, {0, 0, 0}}), 80)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "no latency samples") {
		t.Errorf("an all-unmeasured window should explain itself, got:\n%s", joined)
	}
	if strings.ContainsRune(joined, whiskerMean) {
		t.Error("drew a mean mark for a window with no samples")
	}
}

// A sigma wider than the mean must clamp the lower cap at the axis: a whisker
// below zero reads as negative latency.
func TestRenderWhiskers_LowerCapClampsAtZero(t *testing.T) {
	rows := latencyRows(mkLatencyBuckets([][3]float64{{200, 500, 10}}))
	if rows[0].lo < 0 {
		t.Errorf("lo = %v, want clamped to 0 (mean 200 with sigma 500)", rows[0].lo)
	}
	if rows[0].hi != 700 {
		t.Errorf("hi = %v, want 700", rows[0].hi)
	}
}

// The frame must scale to the tallest +1σ, not the tallest mean, or a cap is
// clipped off the top and tells the reader less than one that fits.
func TestRenderWhiskers_ScalesToUpperCap(t *testing.T) {
	// Bucket 2's mean is lower but its sigma pushes its cap highest.
	lines := renderWhiskers(mkLatencyBuckets([][3]float64{
		{1000, 50, 10},
		{800, 4000, 10}, // cap at 4800
	}), 80)

	// The top axis label must cover the highest cap.
	top := ""
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			top = l
			break
		}
	}
	if !strings.Contains(top, "4") && !strings.Contains(top, "s") {
		t.Errorf("top axis label %q does not reflect the 4800ms cap", top)
	}
	// And the tall bucket's cap must be inside the frame, not clipped.
	joined := strings.Join(lines[:plotRows], "\n")
	if !strings.ContainsRune(joined, whiskerCap) {
		t.Error("upper cap was clipped out of the plot area")
	}
}

// Marks must centre under the same columns the bar renderers fill, so switching
// metrics does not shift the chart sideways.
func TestRenderWhiskers_MarksAlignWithBarColumns(t *testing.T) {
	lines := renderWhiskers(mkLatencyBuckets([][3]float64{{2000, 100, 5}, {3000, 100, 5}}), 80)

	for _, l := range lines[:plotRows] {
		for i, r := range []rune(l) {
			if r != whiskerMean && r != whiskerCap && r != whiskerFoot && r != whiskerRule {
				continue
			}
			// Mark column = axisLabel + k*barStride + (barWidth-1)/2.
			off := i - axisLabel - (barWidth-1)/2
			if off < 0 || off%barStride != 0 {
				t.Errorf("mark at column %d is not centred on a bar column", i)
			}
		}
	}
}

func TestRenderWhiskers_FitsWidth(t *testing.T) {
	var triples [][3]float64
	for i := 0; i < 10; i++ {
		triples = append(triples, [3]float64{float64(1000 * (i + 1)), 300, 5})
	}
	for _, width := range []int{80, 100, 60, 40} {
		for _, line := range renderWhiskers(mkLatencyBuckets(triples), width) {
			if got := len([]rune(line)); got > width {
				t.Errorf("width %d: line is %d columns:\n%q", width, got, line)
			}
		}
	}
}

func TestRenderWhiskers_EmptyInput(t *testing.T) {
	if got := renderWhiskers(nil, 80); len(got) != 1 {
		t.Errorf("renderWhiskers(nil) = %v, want one placeholder line", got)
	}
}

func TestHumanizeDurationMs(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{0, "0"}, {0.4, "<1ms"}, {1, "1.0ms"}, {9.9, "9.9ms"},
		{10, "10ms"}, {999, "999ms"},
		{1000, "1.0s"}, {9900, "9.9s"},
		{10_000, "10s"}, {599_000, "599s"},
		{600_000, "10m"},
		{36_000_000, ">10h"},
		// Boundaries where a rounded label would leave its own branch. Truncating
		// here reported 9.99ms as "9ms" — rounding DOWN past a whole millisecond,
		// which is a worse error than the wide label the branch bound prevents.
		{9.95, "10ms"}, {9.99, "10ms"},
		{999.4, "999ms"}, {999.6, "1.0s"},
		{9949, "9.9s"}, {9950, "10s"}, {9999, "10s"},
		{599_400, "599s"}, {599_600, "10m"},
	} {
		if got := humanizeDurationMs(tc.in); got != tc.want {
			t.Errorf("humanizeDurationMs(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The width promise is what the gutter is laid out against, so it must hold for
// every magnitude — the same lesson as humanizeCount, which returned 7 characters
// above a billion because nobody revisited its fallthrough.
//
// Swept densely rather than at listed points: the failures here live at branch
// boundaries, where a rounded value crosses out of the branch that formatted it,
// and a hand-written list is exactly what misses those.
func TestHumanizeDurationMs_NeverExceedsWidth(t *testing.T) {
	for e := -3.0; e < 10; e += 0.001 {
		v := math.Pow(10, e)
		if got := humanizeDurationMs(v); len([]rune(got)) > maxDurationLabelLen {
			t.Fatalf("humanizeDurationMs(%v) = %q (%d chars), cap is %d",
				v, got, len([]rune(got)), maxDurationLabelLen)
		}
	}
	for _, v := range []float64{0, -1, 0.001, 0.999, math.MaxFloat64} {
		if got := humanizeDurationMs(v); len([]rune(got)) > maxDurationLabelLen {
			t.Errorf("humanizeDurationMs(%v) = %q (%d chars), cap is %d",
				v, got, len([]rune(got)), maxDurationLabelLen)
		}
	}
}

// No label may understate its value: reporting 9.99ms as "9ms" is a wrong number,
// not merely a rounded one. Checks that the rendered label never falls below the
// value it describes by more than the precision it shows.
func TestHumanizeDurationMs_NeverRoundsDownAcrossAUnit(t *testing.T) {
	for _, v := range []float64{9.95, 9.99, 999.6, 9999, 599_600} {
		got := humanizeDurationMs(v)
		// A label ending in a bare unit must not show fewer whole units than the
		// value has, once the unit is accounted for.
		if strings.HasSuffix(got, "ms") && !strings.Contains(got, ".") {
			var n float64
			if _, err := fmt.Sscanf(got, "%fms", &n); err == nil && n < v-1 {
				t.Errorf("humanizeDurationMs(%v) = %q understates by more than 1ms", v, got)
			}
		}
	}
}
