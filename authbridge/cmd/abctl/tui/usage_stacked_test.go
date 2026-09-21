package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// mkSeriesBuckets builds buckets whose Series carry the given per-label request
// counts, one bucket per map in the slice.
func mkSeriesBuckets(perBucket []map[string]int64) []usage.Bucket {
	base := time.Date(2026, 9, 6, 23, 24, 0, 0, time.UTC)
	out := make([]usage.Bucket, 0, len(perBucket))
	for i, labels := range perBucket {
		b := usage.Bucket{At: base.Add(time.Duration(i) * time.Minute)}
		series := map[string]usage.Counts{}
		for label, n := range labels {
			series[label] = usage.Counts{Requests: n, Tokens: n * 10}
			b.Counts.Requests += n
			b.Counts.Tokens += n * 10
		}
		if len(series) > 0 {
			b.Series = series
		}
		out = append(out, b)
	}
	return out
}

// stripANSI removes escape sequences so glyph assertions are not defeated by
// colour codes.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			i++ // skip the 'm'
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// plotLines drops the unit caption when the renderer emitted one, so a test can index
// plot rows from zero the way it did before the caption existed.
//
// By CONTENT, not by width: a test that hardcoded "wide terminals have one extra line"
// would silently go wrong the day the caption's width gate moves, and it would go wrong
// in the direction of asserting against the caption itself rather than the chart.
//
// The vocabulary is DERIVED from usageMetric.unit rather than restated, so renaming a
// unit cannot leave this helper silently matching nothing — which would not fail here,
// it would fail in whichever test indexes a row by position and quietly gets the
// caption instead of the top plot row.
func plotLines(lines []string) []string {
	if len(lines) == 0 {
		return lines
	}
	first := strings.TrimSpace(stripANSI(lines[0]))
	for m := usageMetric(0); m < usageMetricCount; m++ {
		if first == m.unit() {
			return lines[1:]
		}
	}
	return lines
}

// Each series must get a distinct mark, so the chart is readable with no colour
// at all — a terminal without colour support, a colour-vision deficiency, or a
// screenshot in an issue. Shaded blocks failed this in practice: █ against ▓ is
// nearly indistinguishable in most terminal fonts.
func TestRenderStacked_DistinctMarksPerSeries(t *testing.T) {
	buckets := mkSeriesBuckets([]map[string]int64{
		{"200": 10, "429": 5, "500": 2},
	})
	plot := stripANSI(strings.Join(renderStackedBars(buckets, metricRequests, usage.GroupStatus, 80, 0), "\n"))

	// Status labels have no letters, so their marks are the leading digits.
	for _, want := range []string{"2", "4", "5"} {
		if !strings.Contains(plot, strings.Repeat(want, barWidth)) {
			t.Errorf("expected a %q band in the stack; got:\n%s", want, plot)
		}
	}
}

// The mark must be derived from the label so a band is self-describing, and
// sibling models must not collide on a shared vendor prefix — every claude-*
// yielding "c" would defeat the point.
func TestSeriesLetter(t *testing.T) {
	for _, tc := range []struct {
		label string
		want  rune
	}{
		{"claude-sonnet-5", 's'},
		{"claude-opus-5", 'o'},
		{"claude-haiku-4-5-20251001", 'h'},
		{"anthropic/claude-sonnet-5", 's'}, // provider prefix skipped too
		{"gpt-4o", 'o'},                    // gpt is a vendor token; 4 is not a letter
		{"inference-parser", 'i'},
		{"tool-prune", 't'},
		{"denied", 'd'},
		{"200", '2'}, // no letters at all: fall back to the first character
		{"429", '4'},
		{"(other)", 'o'},
		{"", '?'},
		// Hosts, since group=host feeds this the same way. The letters are weaker
		// mnemonics than a model name's: seriesLetter splits on "." too, so
		// "api.anthropic.com" yields 'a' from the leading "api" rather than from
		// the vendor, and an in-cluster FQDN yields the letter of whatever token
		// follows a vendor-looking first one. Pinned as it is, not as it might
		// ideally be — assignLetters still guarantees distinct letters within one
		// chart, and the legend always names the host in full.
		{"api.anthropic.com", 'a'},
		{"api.openai.com", 'a'}, // same letter as above; assignLetters breaks the tie
		{"github-tool-mcp", 'g'},
		{"litellm.svc.cluster.local", 's'}, // "litellm" is a vendor token, so skipped
		{"localhost", 'l'},
		{"10.0.0.5", '1'}, // no letters: first character, as with a status code
	} {
		if got := seriesLetter(tc.label); got != tc.want {
			t.Errorf("seriesLetter(%q) = %q, want %q", tc.label, string(got), string(tc.want))
		}
	}
}

// Two series sharing a mark is the failure the shaded blocks had. Uniqueness
// beats the mnemonic, and the largest series keeps the intuitive letter.
func TestAssignLetters_AreUnique(t *testing.T) {
	series := []seriesKey{
		{"claude-opus-5", 100},  // wants 'o'
		{"claude-sonnet-5", 90}, // wants 's'
		{"(other)", 80},         // also wants 'o' — must yield
		{"openai/gpt-4o", 70},   // 'o' taken as well
		{"ollama-llama3", 60},
	}
	letters := assignLetters(series)
	if len(letters) != len(series) {
		t.Fatalf("assigned %d marks for %d series", len(letters), len(series))
	}
	seen := map[rune]string{}
	for label, r := range letters {
		if prev, dup := seen[r]; dup {
			t.Errorf("mark %q assigned to both %q and %q", string(r), prev, label)
		}
		seen[r] = label
	}
	// The largest series keeps its derived letter.
	if letters["claude-opus-5"] != 'o' {
		t.Errorf("largest series lost its mnemonic: got %q", string(letters["claude-opus-5"]))
	}
}

// Which series render as errors. Asserted on the DECISION, not on ANSI bytes:
// lipgloss strips colour when it detects no TTY, which is always the case under
// `go test`, so a byte-level assertion would pass vacuously and keep passing if
// the rule broke.
func TestRenderStacked_ErrorSeriesDecision(t *testing.T) {
	for _, tc := range []struct {
		label string
		group usage.Group
		want  bool
	}{
		{"500", usage.GroupStatus, true},
		{"429", usage.GroupStatus, true},
		{"400", usage.GroupStatus, true},
		{"denied", usage.GroupStatus, true},
		{"200", usage.GroupStatus, false},
		{"304", usage.GroupStatus, false},
		// Gated on the grouping: "429" is a plausible model name, and a method
		// chart must not turn red because a label happens to look like a status.
		{"429", usage.GroupMethod, false},
		{"500", usage.GroupPlugin, false},
		{"429", usage.GroupHost, false}, // a host is never a status, whatever it is named
		{"claude-sonnet-5", usage.GroupMethod, false},
	} {
		if got := isErrorSeries(tc.label, tc.group); got != tc.want {
			t.Errorf("isErrorSeries(%q, %s) = %v, want %v", tc.label, tc.group, got, tc.want)
		}
	}
}

// paintSegment must leave a non-error series byte-identical, so any styling it
// does apply is attributable to the error rule alone.
func TestPaintSegment_LeavesNonErrorsUntouched(t *testing.T) {
	const text = "████"
	if got := paintSegment(text, "200", usage.GroupStatus); got != text {
		t.Errorf("paintSegment styled a 2xx series: %q", got)
	}
	if got := paintSegment(text, "429", usage.GroupMethod); got != text {
		t.Errorf("paintSegment styled a method label: %q", got)
	}
}

func TestIsErrorStatus(t *testing.T) {
	for _, tc := range []struct {
		label string
		want  bool
	}{
		{"200", false}, {"201", false}, {"304", false},
		{"400", true}, {"429", true}, {"500", true}, {"503", true},
		{"denied", true},
		{"claude-sonnet-5", false},
		{"4", false}, {"40", false}, {"4000", false}, // wrong length
		{"", false},
		{"(other)", false},
	} {
		if got := isErrorStatus(tc.label); got != tc.want {
			t.Errorf("isErrorStatus(%q) = %v, want %v", tc.label, got, tc.want)
		}
	}
}

// Segment order must be stable across renders, or the chart appears to reshuffle
// on every 20s poll even when nothing changed.
func TestRenderStacked_SeriesOrderIsStable(t *testing.T) {
	buckets := mkSeriesBuckets([]map[string]int64{
		{"a": 5, "b": 5, "c": 5}, // equal totals: ties must break deterministically
	})
	first := stripANSI(strings.Join(renderStackedBars(buckets, metricRequests, usage.GroupStatus, 80, 0), "\n"))
	for i := 0; i < 5; i++ {
		again := stripANSI(strings.Join(renderStackedBars(buckets, metricRequests, usage.GroupStatus, 80, 0), "\n"))
		if again != first {
			t.Fatal("render is not deterministic for equal-total series")
		}
	}
}

// A grouped view with no labelled traffic must still show the volume it knows
// about rather than an empty frame.
func TestRenderStacked_NoSeriesFallsBackToBars(t *testing.T) {
	base := time.Date(2026, 9, 6, 23, 24, 0, 0, time.UTC)
	buckets := []usage.Bucket{
		{At: base, Counts: usage.Counts{Requests: 3, Tokens: 300}}, // no Series
	}
	lines := renderStackedBars(buckets, metricTokens, usage.GroupStatus, 80, 0)
	if !strings.ContainsAny(strings.Join(lines, "\n"), "▁▂▃▄▅▆▇█") {
		t.Error("no bars drawn when Series is absent; the frame is empty")
	}
}

// Every line must fit the terminal, colour codes excluded — they occupy no
// columns but do inflate len().
//
// RUN TWICE, once with the colour profile forced. CI has no TTY, so lipgloss falls back
// to Ascii and every Render() is the identity — which means the unforced arm asserts
// against plain text, stripANSI has nothing to strip, and renderLegend's "the mark is
// one column however many bytes of escape it carries" accounting is never tested at all.
// The styled arm is the one users see and the only one where these two can disagree.
func TestRenderStacked_FitsWidth(t *testing.T) {
	buckets := mkSeriesBuckets([]map[string]int64{
		{"200": 100, "429": 50, "500": 25, "503": 10, "418": 5},
		{"200": 80, "429": 40},
	})
	for _, styled := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "styled"}[styled], func(t *testing.T) {
			if styled {
				orig := lipgloss.ColorProfile()
				lipgloss.SetColorProfile(termenv.ANSI256)
				t.Cleanup(func() { lipgloss.SetColorProfile(orig) })
			}
			for _, width := range []int{80, 100, 66, 65, 60} {
				lines := renderStackedBars(buckets, metricRequests, usage.GroupStatus, width, 0)
				sawEscape := false
				for _, line := range lines {
					if strings.Contains(line, "\x1b[") {
						sawEscape = true
					}
					if got := len([]rune(stripANSI(line))); got > width {
						t.Errorf("width %d: line is %d columns:\n%q", width, got, stripANSI(line))
					}
				}
				// The forced arm has to actually produce escapes, or it is a duplicate of
				// the plain one and the coverage it claims is imaginary.
				if styled && !sawEscape {
					t.Errorf("width %d: forced ANSI256 and the render carries no escape "+
						"sequences — stripANSI is doing no work here", width)
				}
			}
		})
	}
}

// Series past the glyph set fold into the overflow marker, and the legend says
// how many were elided rather than running off the terminal.
func TestRenderStacked_OverflowSeriesAreMarked(t *testing.T) {
	labels := map[string]int64{}
	for i := 0; i < 12; i++ {
		labels[string(rune('a'+i))] = int64(12 - i)
	}
	lines := renderStackedBars(mkSeriesBuckets([]map[string]int64{labels}), metricRequests, usage.GroupMethod, 80, 0)
	joined := stripANSI(strings.Join(lines, "\n"))
	// Series past the palette fold into one named band. A "(+N more)" count would
	// be worse: the band is drawn, so it needs a key, not a tally.
	if !strings.Contains(joined, tailLabel) {
		t.Errorf("legend does not name the folded band %q: %q", tailLabel, joined)
	}
	if strings.Contains(joined, "more)") {
		t.Errorf("legend reports a count instead of naming the drawn band: %q", joined)
	}
}

// Non-zero traffic must never render as an empty column, matching the bar chart.
func TestRenderStacked_SmallBucketStillDraws(t *testing.T) {
	buckets := mkSeriesBuckets([]map[string]int64{
		{"200": 10000},
		{"200": 1}, // 1/10000 of the peak
	})
	lines := renderStackedBars(buckets, metricRequests, usage.GroupStatus, 80, 0)
	bottom := stripANSI(plotLines(lines)[plotRows-1]) // last plot row
	if strings.Count(bottom, "2") < barWidth*2 {
		t.Errorf("the tiny bucket drew no segment:\n%q", bottom)
	}
}

// A series that is a rounding error of the bucket must still occupy a row.
// "claude-haiku at 912 tokens against 2.2M" is exactly the case an operator wants
// to spot, and a segment floored to zero rows is indistinguishable from absent.
//
// The per-series floor alone was not enough: with three series in a ten-row bar
// the largest took 9 rows and the two floored ones landed on rows 10 and 11, so
// the eleventh fell outside the bar and vanished anyway.
func TestRenderStacked_TinySeriesIsStillVisible(t *testing.T) {
	buckets := mkSeriesBuckets([]map[string]int64{{
		"claude-sonnet-5":           22000, // 2.2M tokens at 100x
		"claude-opus-5":             1050,
		"claude-haiku-4-5-20251001": 9, // 0.04% of the bucket
	}})
	plot := stripANSI(strings.Join(renderStackedBars(buckets, metricTokens, usage.GroupMethod, 80, 0), "\n"))

	for _, want := range []string{"s", "o", "h"} {
		if !strings.Contains(plot, strings.Repeat(want, barWidth)) {
			t.Errorf("series %q drew no band despite being present:\n%s", want, plot)
		}
	}
}

// A bar's height comes from the bucket total, so traffic no label claims must get
// its own band rather than being absorbed by the named series. Absorbing it drew
// a bucket that is 10% claude-sonnet-5 as a solid `s` bar: the height said "lots
// of traffic" and every row claimed to be sonnet.
func TestAllotRows_UnlabelledTrafficGetsItsOwnBand(t *testing.T) {
	b := mkSeriesBuckets([]map[string]int64{{"claude-sonnet-5": 100}})[0]
	b.Counts.Requests = 1000 // only 10% of the bucket is labelled
	series := collectSeries([]usage.Bucket{b}, metricRequests)

	got := allotRows(b, metricRequests, series, 10, b.Requests)

	rows := map[string]int64{}
	var sum int64
	for _, a := range got {
		rows[a.label] = a.rows
		sum += a.rows
	}
	if sum != 10 {
		t.Fatalf("allotted %d rows, bar is 10 tall", sum)
	}
	if rows["claude-sonnet-5"] != 1 && rows["claude-sonnet-5"] != 2 {
		t.Errorf("sonnet has %d of 10 rows for 10%% of the bucket", rows["claude-sonnet-5"])
	}
	if rows[unlabelledLabel] == 0 {
		t.Error("the 90% no label claims drew no band")
	}
}

// Per-plugin attribution counts one request once per plugin, so byPlugin
// sub-totals intentionally sum to MORE than the bucket (see the aggregator's
// foldInto). Dividing by the bucket total over-allotted rows, `acc` ran past the
// bar height, and whole series fell off the top — on every by-plugin bucket.
func TestAllotRows_HandlesOverAttributedSeries(t *testing.T) {
	b := mkSeriesBuckets([]map[string]int64{{"p1": 1000, "p2": 1000, "p3": 1000}})[0]
	b.Counts.Requests = 1000 // each plugin credited the whole bucket
	series := collectSeries([]usage.Bucket{b}, metricRequests)

	got := allotRows(b, metricRequests, series, 10, b.Requests)

	var sum int64
	for _, a := range got {
		if a.rows < 1 {
			t.Errorf("series %q allotted %d rows", a.label, a.rows)
		}
		sum += a.rows
	}
	if sum != 10 {
		t.Errorf("allotted %d rows for a 10-row bar — series would fall off the chart", sum)
	}
	if len(got) != 3 {
		t.Errorf("allotted %d bands, want all 3 plugins present", len(got))
	}
}

// Every band drawn must have a legend entry. Marks come from the palette and
// repeat once it wraps, so a seventh series could draw with the first one's mark
// while the legend named neither — an undecodable band.
func TestRenderStacked_EveryDrawnBandIsInTheLegend(t *testing.T) {
	labels := map[string]int64{}
	for i := 0; i < 12; i++ {
		labels[fmt.Sprintf("series-%c", 'a'+i)] = int64(100 - i*5)
	}
	lines := renderStackedBars(mkSeriesBuckets([]map[string]int64{labels}), metricRequests, usage.GroupMethod, 120, 0)

	// plotLines first, then split: the unit caption sits above the axis, so leaving it in
	// the chart half would collect the letters of "req" as though they were series marks
	// — and "q" is no series's mark, so the test would fail on its own caption.
	lines = plotLines(lines)

	// Split chart rows from legend rows at the axis.
	var axisAt int
	for i, l := range lines {
		if strings.ContainsRune(stripANSI(l), '┼') {
			axisAt = i
			break
		}
	}
	chart := stripANSI(strings.Join(lines[:axisAt], "\n"))
	legend := stripANSI(strings.Join(lines[axisAt:], "\n"))

	// Collect the distinct marks actually drawn.
	drawn := map[rune]bool{}
	for _, r := range chart {
		if unicode.IsLetter(r) || r == '·' {
			drawn[r] = true
		}
	}
	if len(drawn) == 0 {
		t.Fatal("no marks drawn")
	}
	for r := range drawn {
		// The legend lists each mark followed by its name.
		if !strings.ContainsRune(legend, r) {
			t.Errorf("mark %q is drawn on the chart but absent from the legend:\n%s", string(r), legend)
		}
	}
}

// Folding the tail must preserve the totals: bounding how many bands are drawn is
// the point, losing traffic is not.
func TestFoldTailSeries_PreservesTotals(t *testing.T) {
	labels := map[string]int64{}
	var want int64
	for i := 0; i < 10; i++ {
		v := int64(100 - i*5)
		labels[fmt.Sprintf("s%c", 'a'+i)] = v
		want += v
	}
	buckets := mkSeriesBuckets([]map[string]int64{labels})
	series := collectSeries(buckets, metricRequests)

	kept, folded := foldTailSeries(buckets, series, 4)
	if len(kept) != 5 { // 4 named + the fold
		t.Errorf("kept %d series, want 4 named plus the fold", len(kept))
	}
	var got int64
	for _, c := range folded[0].Series {
		got += c.Requests
	}
	if got != want {
		t.Errorf("folded buckets total %d, want %d — folding lost traffic", got, want)
	}
	// An existing "(other)" from the aggregator's own capping must merge, not
	// collide: two bands both meaning "the rest" would be indefensible.
	withOther := mkSeriesBuckets([]map[string]int64{{
		"a": 100, "b": 90, "c": 80, "d": 70, "e": 60, tailLabel: 50,
	}})
	s2 := collectSeries(withOther, metricRequests)
	kept2, _ := foldTailSeries(withOther, s2, 3)
	seen := 0
	for _, k := range kept2 {
		if k.label == tailLabel {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("%d %q bands after folding, want exactly 1", seen, tailLabel)
	}
}

// Allotment must fill the bar exactly: overshooting pushes top segments outside
// the frame, undershooting leaves a gap at the top.
func TestAllotRows_SumsToBarHeight(t *testing.T) {
	for _, tc := range []struct {
		name    string
		values  map[string]int64
		barRows int64
	}{
		{"tiny tail", map[string]int64{"a": 22000, "b": 1050, "c": 9}, 10},
		{"even split", map[string]int64{"a": 10, "b": 10, "c": 10}, 9},
		{"more series than rows", map[string]int64{"a": 5, "b": 4, "c": 3, "d": 2, "e": 1}, 3},
		{"single series", map[string]int64{"a": 7}, 10},
		{"one row", map[string]int64{"a": 7, "b": 3}, 1},
	} {
		b := mkSeriesBuckets([]map[string]int64{tc.values})[0]
		series := collectSeries([]usage.Bucket{b}, metricRequests)
		got := allotRows(b, metricRequests, series, tc.barRows, b.Requests)

		var sum int64
		for _, a := range got {
			if a.rows < 1 {
				t.Errorf("%s: series %q allotted %d rows", tc.name, a.label, a.rows)
			}
			sum += a.rows
		}
		if sum != tc.barRows {
			t.Errorf("%s: allotted %d rows, bar is %d tall", tc.name, sum, tc.barRows)
		}
	}
}

// The legend is the only key to a band, so a present series must never be elided
// for width — it wraps instead. Model names are long enough that three do not fit
// 80 columns on one line.
func TestRenderLegend_WrapsRatherThanElidingPresentSeries(t *testing.T) {
	series := []seriesKey{
		{"claude-sonnet-5", 4_600_000},
		{"claude-opus-5", 218_000},
		{"claude-haiku-4-5-20251001", 1900},
	}
	letters := assignLetters(series)
	rank := map[string]int{}
	for i, s := range series {
		rank[s.label] = i
	}

	lines := renderLegend(series, usage.GroupMethod, metricTokens, letters, rank, 80)
	joined := stripANSI(strings.Join(lines, "\n"))
	for _, s := range series {
		if !strings.Contains(joined, s.label) {
			t.Errorf("legend dropped %q:\n%s", s.label, joined)
		}
	}
	if strings.Contains(joined, "more)") {
		t.Errorf("legend elided a named series instead of wrapping:\n%s", joined)
	}
	for _, l := range lines {
		if got := len([]rune(stripANSI(l))); got > 80 {
			t.Errorf("legend line is %d columns:\n%q", got, stripANSI(l))
		}
	}
}

// A legend line wider than the terminal wraps and destroys the chart above it, so
// the width bound has to hold even for a single entry with nothing to wrap
// against — one long model name on a narrow terminal.
func TestRenderLegend_BoundsALoneOverWideEntry(t *testing.T) {
	series := []seriesKey{{"anthropic/claude-haiku-4-5-20251001-preview-experimental", 912}}
	letters := assignLetters(series)
	rank := map[string]int{series[0].label: 0}

	for _, width := range []int{10, 16, 20, 40, 80} {
		lines := renderLegend(series, usage.GroupMethod, metricTokens, letters, rank, width)
		for _, l := range lines {
			if got := len([]rune(stripANSI(l))); got > width {
				t.Errorf("width %d: legend line is %d columns:\n%q", width, got, stripANSI(l))
			}
		}
		// The total identifies how much the band is worth, so it survives
		// truncation wherever there is room for it at all.
		if width >= 20 {
			if !strings.Contains(stripANSI(strings.Join(lines, "")), "(912)") {
				t.Errorf("width %d: truncation dropped the total: %q", width, stripANSI(lines[0]))
			}
		}
	}
}

// renderLegend names everything it is handed — capping is foldTailSeries' job, and
// doing it in both places cut the fold off. Whatever the chart draws must be
// named, at every width, wrapping as needed.
func TestRenderLegend_NamesEverySeriesAtEveryWidth(t *testing.T) {
	var series []seriesKey
	for i := 0; i < 9; i++ {
		series = append(series, seriesKey{fmt.Sprintf("series-%c-name", 'a'+i), int64(100 - i)})
	}
	letters := assignLetters(series)
	rank := map[string]int{}
	for i, s := range series {
		rank[s.label] = i
	}

	for width := 30; width <= 100; width++ {
		lines := renderLegend(series, usage.GroupMethod, metricTokens, letters, rank, width)
		joined := stripANSI(strings.Join(lines, "\n"))
		for _, s := range series {
			if !strings.Contains(joined, s.label) {
				t.Errorf("width %d: legend omits %q, whose band is drawn", width, s.label)
			}
		}
		for _, l := range lines {
			if got := len([]rune(stripANSI(l))); got > width {
				t.Errorf("width %d: line is %d columns:\n%q", width, got, stripANSI(l))
			}
		}
	}
}

// The fold must survive the legend. foldTailSeries emits maxNamedSeries named
// bands plus "(other)"; re-capping at maxNamedSeries cut that fold off, so the
// largest unnamed band was drawn and the legend said "(+1 more)" — naming nothing.
func TestRenderLegend_KeepsTheFoldedBand(t *testing.T) {
	var series []seriesKey
	for i := 0; i < 12; i++ {
		series = append(series, seriesKey{fmt.Sprintf("series-%c", 'a'+i), int64(100 - i*5)})
	}
	kept, _ := foldTailSeries(nil, series, maxNamedSeries)
	if len(kept) != maxNamedSeries+1 {
		t.Fatalf("foldTailSeries kept %d, want %d named plus the fold", len(kept), maxNamedSeries)
	}
	letters := assignLetters(kept)
	rank := map[string]int{}
	for i, s := range kept {
		rank[s.label] = i
	}
	joined := stripANSI(strings.Join(renderLegend(kept, usage.GroupMethod, metricTokens, letters, rank, 120), "\n"))
	if !strings.Contains(joined, tailLabel) {
		t.Errorf("legend dropped the folded band:\n%s", joined)
	}
}

func TestTruncateLegendText(t *testing.T) {
	const text = " claude-sonnet-5 (2.2M)"
	for _, max := range []int{0, 1, 2, 5, 10, 22, 23, 40} {
		got := truncateLegendText(text, max)
		if len([]rune(got)) > max {
			t.Errorf("truncateLegendText(%d) = %q (%d runes), over budget", max, got, len([]rune(got)))
		}
	}
	if got := truncateLegendText(text, 40); got != text {
		t.Errorf("a text that already fits was altered: %q", got)
	}
}

// A short bar cannot show every band, so it keeps the LARGEST — not the first few
// by position. present ends with the unlabelled remainder, so positional
// truncation dropped exactly the band that keeps the bar honest: a 3-row bar that
// was 94% unclaimed reattributed all of it to the named series.
func TestAllotRows_ShortBarKeepsTheLargestBands(t *testing.T) {
	b := mkSeriesBuckets([]map[string]int64{{"a": 30, "b": 20, "c": 10}})[0]
	b.Counts.Requests = 1000 // 94% of the bucket claimed by no label
	series := collectSeries([]usage.Bucket{b}, metricRequests)

	for _, barRows := range []int64{1, 2, 3, 4, 10} {
		got := allotRows(b, metricRequests, series, barRows, b.Requests)
		var sum int64
		found := false
		for _, a := range got {
			sum += a.rows
			if a.label == unlabelledLabel {
				found = true
			}
		}
		if sum != barRows {
			t.Errorf("barRows=%d: allotted %d rows", barRows, sum)
		}
		if !found {
			t.Errorf("barRows=%d: dropped the unlabelled band, reattributing 94%% of the bucket", barRows)
		}
	}
}

// The legend must not key a band the chart never draws. allotRows only emits the
// remainder when it fills a row, so the legend has to use the same predicate.
func TestUnlabelledTotal_OnlyCountsDrawnBands(t *testing.T) {
	// A one-token shortfall against a large bucket cannot fill a row.
	b := mkSeriesBuckets([]map[string]int64{{"a": 999}})[0]
	b.Counts.Requests = 1000
	series := collectSeries([]usage.Bucket{b}, metricRequests)

	if got := unlabelledTotal([]usage.Bucket{b}, metricRequests, series, b.Requests); got != 0 {
		t.Errorf("unlabelledTotal = %d for a sub-row shortfall, want 0", got)
	}
	// And the rendered legend agrees.
	joined := stripANSI(strings.Join(renderStackedBars([]usage.Bucket{b}, metricRequests, usage.GroupMethod, 80, 0), "\n"))
	if strings.Contains(joined, unlabelledLabel) {
		t.Errorf("legend names %q for a band that is never drawn:\n%s", unlabelledLabel, joined)
	}

	// A large shortfall does draw, and is named.
	b2 := mkSeriesBuckets([]map[string]int64{{"a": 100}})[0]
	b2.Counts.Requests = 1000
	s2 := collectSeries([]usage.Bucket{b2}, metricRequests)
	if got := unlabelledTotal([]usage.Bucket{b2}, metricRequests, s2, b2.Requests); got != 900 {
		t.Errorf("unlabelledTotal = %d, want 900", got)
	}
}

// A bucket with traffic but NO labelled series at all must draw the remainder,
// not fall through to painting every row as the largest series.
//
// The existing unlabelled test covers the partially-labelled case (seriesSum > 0),
// which is why the suite passed straight through this one: an early return on
// seriesSum == 0 gave the bucket no allotment, stackedCell exhausted its empty
// loop and hit the series[0] fallback, and unlabelledTotal counted the bucket
// anyway — so the legend keyed an (unlabelled) band the chart never drew.
//
// Reachable in the by-plugin view by any bucket whose turns invoked no plugin.
func TestAllotRows_FullyUnlabelledBucketDrawsTheRemainder(t *testing.T) {
	b := usage.Bucket{Counts: usage.Counts{Requests: 900}} // no Series at all
	labelled := mkSeriesBuckets([]map[string]int64{{"plugin-x": 800, "plugin-y": 200}})[0]
	series := collectSeries([]usage.Bucket{labelled, b}, metricRequests)

	got := allotRows(b, metricRequests, series, 9, b.Requests)
	if len(got) == 0 {
		t.Fatal("no allotment for a bucket with traffic — every row would paint as series[0]")
	}
	var sum int64
	for _, a := range got {
		if a.label != unlabelledLabel {
			t.Errorf("allotted rows to %q, which claims none of this bucket", a.label)
		}
		sum += a.rows
	}
	if sum != 9 {
		t.Errorf("allotted %d rows, bar is 9 tall", sum)
	}
}

// The chart and the legend must agree on a fully unlabelled bucket: it draws the
// remainder mark, and no named series appears in a bucket that has none.
func TestRenderStacked_FullyUnlabelledBucketMatchesItsLegend(t *testing.T) {
	base := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	labelled := usage.Bucket{At: base, Counts: usage.Counts{Requests: 1000},
		Series: map[string]usage.Counts{
			"plugin-x": {Requests: 800}, "plugin-y": {Requests: 200},
		}}
	unlabelled := usage.Bucket{At: base.Add(time.Minute), Counts: usage.Counts{Requests: 900}}

	lines := renderStackedBars([]usage.Bucket{labelled, unlabelled}, metricRequests, usage.GroupPlugin, 80, 0)

	// The second bar's column: axisLabel + 1*barStride.
	col := axisLabel + barStride
	var secondBar string
	// plotLines first: at width 80 this chart carries a unit caption, and indexing from
	// the front without dropping it shifts the window up a row — the scan then stops one
	// short of the bottom plot row and still passes, which is the silent weakening the
	// helper exists to prevent rather than a failure anyone would notice.
	for _, l := range plotLines(lines)[:plotRows] {
		r := []rune(stripANSI(l))
		if len(r) > col && r[col] != ' ' {
			secondBar += string(r[col])
		}
	}
	if secondBar == "" {
		t.Fatal("the fully unlabelled bucket drew nothing")
	}
	for _, r := range secondBar {
		if r != unlabelledMark {
			t.Errorf("unlabelled bucket drew %q, want only %q — a named series was misattributed",
				secondBar, string(unlabelledMark))
			break
		}
	}
	// And the legend keys exactly what was drawn.
	joined := stripANSI(strings.Join(lines, "\n"))
	if !strings.Contains(joined, unlabelledLabel) {
		t.Error("legend does not name the remainder band that is drawn")
	}
}

// The folded "(other)" band must carry every Counts field, PricedRequests
// included: a band reporting zero coverage for traffic that WAS priced would
// claim its cost is partial when it is whole.
//
// This once summed the fields by hand in this package and silently missed
// PricedRequests when that field was added. It now delegates to usage.Counts.Add,
// so the arithmetic lives once, beside the struct. This test covers the behaviour;
// what makes it stay correct is the delegation, not the assertion — a field added
// to Counts and to Add is carried here with no change to this file.
func TestFoldTailSeries_CarriesEveryCountsField(t *testing.T) {
	// Two series beyond keep=1, so both fold into "(other)".
	buckets := []usage.Bucket{{
		Counts: usage.Counts{Requests: 30, Errors: 3, Tokens: 3000, CostMicros: 900, PricedRequests: 21},
		Series: map[string]usage.Counts{
			"keep-me": {Requests: 10, Errors: 1, Tokens: 1000, CostMicros: 300, PricedRequests: 7},
			"tail-a":  {Requests: 12, Errors: 1, Tokens: 1200, CostMicros: 400, PricedRequests: 9},
			"tail-b":  {Requests: 8, Errors: 1, Tokens: 800, CostMicros: 200, PricedRequests: 5},
		},
	}}

	// Ordered as the caller supplies them; keep=1 retains only the first, so
	// tail-a and tail-b fold into "(other)".
	series := []seriesKey{
		{label: "keep-me", total: 10},
		{label: "tail-a", total: 12},
		{label: "tail-b", total: 8},
	}

	_, out := foldTailSeries(buckets, series, 1)
	other, ok := out[0].Series[tailLabel]
	if !ok {
		t.Fatalf("no %q band; series = %v", tailLabel, out[0].Series)
	}

	want := usage.Counts{Requests: 20, Errors: 2, Tokens: 2000, CostMicros: 600, PricedRequests: 14}
	if other != want {
		t.Errorf("folded %q = %+v, want %+v", tailLabel, other, want)
	}
}
