package tui

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// TestSearchDoc_FindsAMatchTheWrapSplits: matching is on the line before it is wrapped.
func TestSearchDoc_FindsAMatchTheWrapSplits(t *testing.T) {
	var d searchDoc
	d.setLines([]string{"the quick brown fox jumps"}, 10)
	out := d.render("quick brown")
	if !slices.Equal(d.matches, []int{0}) {
		t.Fatalf("matches = %v, want [0], the row the match starts on", d.matches)
	}
	if got, want := ansi.Strip(out), ansi.Wrap("the quick brown fox jumps", 10, " -"); got != want {
		t.Errorf("highlighting changed the wrap:\n%q\nwant\n%q", got, want)
	}
}

// TestSearchDoc_PutsAMatchOnTheRowItStartsOn: one long JSON string is one line and many
// rows; the match is on its own row, not the line's first.
func TestSearchDoc_PutsAMatchOnTheRowItStartsOn(t *testing.T) {
	long := `  "completion": "` + strings.Repeat("filler words here ", 30) + `needle at the end",`
	var d searchDoc
	d.setLines([]string{"{", long, "}"}, 24)
	d.render("needle")
	rows := strings.Split(ansi.Wrap(long, 24, " -"), "\n")
	want := -1
	for k, r := range rows {
		if strings.Contains(r, "needle") {
			want = 1 + k // "{" is row 0
		}
	}
	if !slices.Equal(d.matches, []int{want}) {
		t.Errorf("matches = %v, want [%d]", d.matches, want)
	}
}

// TestSearchDoc_PlacesAWideMatchOnItsRow: wide runes are two columns, and the row a match
// starts on is counted in columns.
func TestSearchDoc_PlacesAWideMatchOnItsRow(t *testing.T) {
	line := strings.Repeat("日本語 ", 10) + "テキスト"
	var d searchDoc
	d.setLines([]string{line}, 12)
	d.render("テキ")
	rows := strings.Split(ansi.Wrap(line, 12, " -"), "\n")
	want := -1
	for k, r := range rows {
		if strings.Contains(r, "テキ") {
			want = k
		}
	}
	if !slices.Equal(d.matches, []int{want}) {
		t.Errorf("matches = %v, want [%d]", d.matches, want)
	}
}

// TestSearchDoc_KeepsTheJSONColorsAroundAHighlight, on the colorizer's own output.
func TestSearchDoc_KeepsTheJSONColorsAroundAHighlight(t *testing.T) {
	forceColour(t)
	lines := strings.Split(ColorizeJSONBytes([]byte(`{"model":"claude-opus-5-5","stream":true}`)), "\n")
	var d searchDoc
	d.setLines(lines, 0)
	out := d.render("opus")
	if !strings.Contains(out, styleMatch.Render("opus")) {
		t.Errorf("opus is not highlighted: %q", out)
	}
	if !strings.Contains(out, styleJSONBool.Render("true")) {
		t.Errorf("the value after the match lost its color: %q", out)
	}
}

// TestSearchDoc_TheCurrentRowSurvivesARedrawOfTheSameText: layout() redraws the detail on
// every prompt open and close; a new message or a new wrap width is a new document.
func TestSearchDoc_TheCurrentRowSurvivesARedrawOfTheSameText(t *testing.T) {
	forceColour(t)
	lines := []string{"alpha needle", "beta", "gamma needle"}
	var d searchDoc
	d.setLines(lines, 0)
	d.render("needle")
	d.setCurrent(2)
	if !strings.Contains(d.render("needle"), styleCurrentMatch.Render("needle")) {
		t.Fatal("the current row is not drawn in styleCurrentMatch")
	}
	d.setLines(slices.Clone(lines), 0)
	if r, ok := d.currentRow(); !ok || r != 2 {
		t.Errorf("same text: current = %d,%v, want 2", r, ok)
	}
	d.setLines(lines, 40)
	if _, ok := d.currentRow(); ok {
		t.Error("a new wrap width kept the current row")
	}
	d.setLines([]string{"other"}, 40)
	if _, ok := d.currentRow(); ok {
		t.Error("new text kept the current row")
	}
}

// TestSearchDoc_RenderIsLinearInALongLine: rowStarts measured each wrapped row's prefix from
// column 0 again, so one 225KB string cost 2.2s per keystroke at 200 columns — more on a
// narrower terminal, and Update is synchronous, so the whole TUI froze.
//
// GROWTH, NOT A TIME: CI runs under -race, which makes this ~20x slower than a laptop, so no one
// bound fits both. Four times the line is about four times the work when the render is linear and
// about sixteen when it is quadratic, on any machine.
func TestSearchDoc_RenderIsLinearInALongLine(t *testing.T) {
	render := func(n int) time.Duration {
		line := `  "completion": "` + strings.Repeat("filler words here ", n) + `",`
		best := time.Duration(1<<63 - 1)
		for range 3 {
			var d searchDoc
			d.setLines([]string{line}, 200)
			start := time.Now()
			d.render("words")
			best = min(best, time.Since(start))
			if len(d.matches) == 0 {
				t.Fatal("no matches")
			}
		}
		return best
	}
	small, large := render(3000), render(12000)
	if growth := float64(large) / float64(small); growth > 8 {
		t.Errorf("4x the line took %.1fx the time (%v → %v): render is not linear", growth, small, large)
	}
}

// The gutter takes two columns from the wrap and marks every row of the cursor's line.
func TestSearchDoc_GutterMarksTheCursorLine(t *testing.T) {
	d := searchDoc{gutter: true, cursor: 1}
	d.setLines([]string{"a", "the quick brown fox", "c"}, 12)
	got := strings.Split(ansi.Strip(d.render("")), "\n")
	want := []string{"  a", "▌ the quick", "▌ brown fox", "  c"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	if f, l := d.rowsOf(1); f != 1 || l != 2 {
		t.Errorf("rowsOf(1) = %d,%d, want 1,2", f, l)
	}
	if f, l := d.rowsOf(2); f != 3 || l != 3 {
		t.Errorf("rowsOf(2) = %d,%d, want 3,3", f, l)
	}
	for row, want := range []int{0, 1, 1, 2} {
		if got := d.lineAtRow(row); got != want {
			t.Errorf("lineAtRow(%d) = %d, want %d", row, got, want)
		}
	}
}

// A terminal narrower than the gutter must still wrap to a positive width.
func TestSearchDoc_GutterOnANarrowTerminal(t *testing.T) {
	d := searchDoc{gutter: true}
	d.setLines([]string{"abc"}, 2)
	if out := ansi.Strip(d.render("")); !strings.Contains(out, "a") || !strings.Contains(out, "c") {
		t.Errorf("the line was lost at width 2: %q", out)
	}
}
