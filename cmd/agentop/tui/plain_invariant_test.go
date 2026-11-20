package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// assertPlainInvariant renders once under Ascii and once under TrueColor, and checks that
// color changed no text and no width. On the lines in hintLines (negative counts from the
// end) the color line may also show each [x] key hint as a ` x ` chip. On every line, a `▌`
// at column 0 of the color line may be the selection bar, so it may also count as a space.
func assertPlainInvariant(t *testing.T, render func() string, hintLines ...int) {
	t.Helper()
	restore := lipgloss.ColorProfile()
	t.Cleanup(func() { lipgloss.SetColorProfile(restore) })

	lipgloss.SetColorProfile(termenv.Ascii)
	plain := strings.Split(render(), "\n")
	lipgloss.SetColorProfile(termenv.TrueColor)
	color := strings.Split(render(), "\n")
	if len(plain) != len(color) {
		t.Fatalf("color render has %d lines, Ascii has %d", len(color), len(plain))
	}
	hints := make([]int, len(hintLines))
	for i, h := range hintLines {
		if h < 0 {
			h += len(plain)
		}
		hints[i] = h
	}
	for i := range plain {
		if wp, wc := lipgloss.Width(plain[i]), lipgloss.Width(color[i]); wp != wc {
			t.Errorf("line %d: width %d under color, %d under Ascii", i, wc, wp)
		}
		got := ansi.Strip(color[i])
		// Compare as is first: a `▌` the Ascii line also draws (the detail cursor) is text.
		cands := []string{got}
		if rest, ok := strings.CutPrefix(got, "▌"); ok {
			cands = append(cands, " "+rest)
		}
		hinted := slices.Contains(hints, i)
		if slices.ContainsFunc(cands, func(c string) bool {
			return c == plain[i] || hinted && c == chipText(plain[i])
		}) {
			continue
		}
		t.Errorf("line %d: color text\n  %q\nAscii text\n  %q", i, got, plain[i])
	}
}

// chipText is the plain text renderHints draws under color: each [x] becomes ` x `.
func chipText(s string) string {
	var b strings.Builder
	for _, p := range splitHintChips(s) {
		if p.chip {
			b.WriteString(" " + p.text + " ")
		} else {
			b.WriteString(p.text)
		}
	}
	return b.String()
}
