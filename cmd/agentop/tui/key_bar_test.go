package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// runeSGR returns, for each printed rune of s, the SGR parameters in force when it is drawn.
func runeSGR(s string) []string {
	var out []string
	cur := ""
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], "\x1b[") {
			end := strings.IndexByte(s[i:], 'm')
			params := s[i+2 : i+end]
			if params == "0" || params == "" {
				cur = ""
			} else {
				cur += ";" + params + ";"
			}
			i += end + 1
			continue
		}
		_, size := firstRune(s[i:])
		out = append(out, cur)
		i += size
	}
	return out
}

func firstRune(s string) (rune, int) {
	for _, r := range s {
		return r, len(string(r))
	}
	return 0, 0
}

// sgrOf is the SGR parameters TrueColor emits for c in the current theme.
func sgrOf(c lipgloss.AdaptiveColor, bg bool) string {
	hex := c.Light
	if lipgloss.HasDarkBackground() {
		hex = c.Dark
	}
	return ";" + termenv.TrueColor.Color(hex).Sequence(bg) + ";"
}

// checkBar asserts out is the key bar for the plain hint fitted at width w.
func checkBar(t *testing.T, out, fitted string, w int) {
	t.Helper()
	if got := lipgloss.Width(out); got != w {
		t.Fatalf("bar width %d, want %d", got, w)
	}
	want := chipText(fitted)
	if got := strings.TrimRight(ansi.Strip(out), " "); got != strings.TrimRight(want, " ") {
		t.Fatalf("bar text %q, want %q", got, want)
	}
	// Mark the runes of each chip in the plain text.
	var chip []bool
	for _, p := range splitHintChips(fitted) {
		n := len([]rune(p.text))
		if p.chip {
			n += 2
		}
		for range n {
			chip = append(chip, p.chip)
		}
	}
	bar, chipBg, chipFg := sgrOf(colorBarBg, true), sgrOf(colorBarChipBg, true), sgrOf(colorBarChipFg, false)
	for i, sgr := range runeSGR(out) {
		if i < len(chip) && chip[i] {
			if !strings.Contains(sgr, chipBg) || !strings.Contains(sgr, chipFg) {
				t.Fatalf("chip rune %d drawn with %q, want %q and %q", i, sgr, chipBg, chipFg)
			}
		} else if !strings.Contains(sgr, bar) {
			t.Fatalf("rune %d drawn with %q, want bar background %q", i, sgr, bar)
		}
	}
}

func eachTheme(t *testing.T, f func(t *testing.T)) {
	orig := lipgloss.HasDarkBackground()
	t.Cleanup(func() { lipgloss.SetHasDarkBackground(orig) })
	for _, dark := range []bool{true, false} {
		lipgloss.SetHasDarkBackground(dark)
		t.Run(map[bool]string{true: "dark", false: "light"}[dark], f)
	}
}

func TestRenderKeyBar_FullWidthUnderColor(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	h := "[↵] drill  [u] usage  [/] search  [?] keys  [q] quit"
	eachTheme(t, func(t *testing.T) {
		fitted := fitHintLine(h, 100)
		checkBar(t, renderKeyBar(fitted, 100), fitted, 100)
		// The status on the right keeps its brackets, and every rune of it, gap included,
		// is on the bar's ground.
		st := "error: [::1]"
		out := bottomBar(h, barInk(styleError).Render(st), 100)
		if got := lipgloss.Width(out); got != 100 {
			t.Fatalf("bar width %d, want 100", got)
		}
		if got := ansi.Strip(out); !strings.HasPrefix(got, chipText(h)) || !strings.HasSuffix(got, st) {
			t.Fatalf("bar text %q, want the keys then %q", got, st)
		}
		bar := sgrOf(colorBarBg, true)
		sgr := runeSGR(out)
		for i := len([]rune(chipText(h))); i < len(sgr); i++ {
			if !strings.Contains(sgr[i], bar) {
				t.Fatalf("status rune %d drawn with %q, want bar background", i, sgr[i])
			}
		}
	})
}

// Review Focus 4: a cut that starts with "… " and ends in half a bracket still fills the
// width with the bar, and the half bracket is not a chip.
func TestRenderKeyBar_CutHintFillsWidth(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	h := "[↵] drill  [u] usage  [/] search  [?] keys  [q] quit"
	eachTheme(t, func(t *testing.T) {
		for _, w := range []int{30, 20, 6} {
			fitted := fitHintLine(h, w)
			if w == 30 && !strings.HasPrefix(fitted, "… ") {
				t.Fatalf("fit at %d = %q, want a … cut", w, fitted)
			}
			checkBar(t, renderKeyBar(fitted, w), fitted, w)
		}
		half := "… [esc] back  [es"
		checkBar(t, renderKeyBar(half, 40), half, 40)
		if s := ansi.Strip(renderKeyBar(half, 40)); !strings.Contains(s, "[es") {
			t.Fatalf("half bracket became a chip: %q", s)
		}
	})
}

func TestRenderKeyBar_AsciiUnchanged(t *testing.T) {
	withProfile(t, termenv.Ascii)
	eachTheme(t, func(t *testing.T) {
		for _, in := range []string{"[↵] open  [/] search", "… [es"} {
			if got, want := renderKeyBar(in, 100), renderHints(in); got != want {
				t.Fatalf("renderKeyBar(%q) = %q, want %q", in, got, want)
			}
		}
	})
}

func TestRenderKeyBar_NoPadWithoutWidth(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	in := "[q] quit"
	if got := lipgloss.Width(renderKeyBar(in, 0)); got != lipgloss.Width(in) {
		t.Fatalf("width %d with no terminal width, want %d", got, lipgloss.Width(in))
	}
}

// The last line of every screen that draws the key bar fills the terminal width; the status
// line above it is unchanged.
func TestKeyBar_LastLineOfEachScreen(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	lastLine := func(s string) string { l := strings.Split(s, "\n"); return l[len(l)-1] }
	bar := sgrOf(colorBarBg, true)
	isBar := func(line string, w int) bool {
		sgr := runeSGR(line)
		return lipgloss.Width(line) == w && strings.Contains(sgr[0], ";48;") && strings.Contains(sgr[len(sgr)-1], bar)
	}

	m := fitModel(t, paneSessions, 120, 30, nil)
	m.layout()
	if got := lastLine(m.footerView()); !isBar(got, 120) {
		t.Fatalf("session footer bar %q: width %d, want 120 with the bar color", got, lipgloss.Width(got))
	}
	m.flash, m.flashSticky = "yanked → /tmp/x", true
	if got := lastLine(m.footerView()); !isBar(got, 120) {
		t.Fatalf("sticky-flash footer bar: width %d, want 120", lipgloss.Width(got))
	}

	lister := &fakeLister{namespaces: fixtureNamespaces}
	pm := newPickerModel(context.Background(), lister, nil)
	updated, _ := pm.Update(pm.Init()())
	mm := updated.(*model)
	mm.width, mm.height = 120, 30
	if got := lastLine(mm.View()); !isBar(got, 120) {
		t.Fatalf("namespaces footer bar: width %d, want 120", lipgloss.Width(got))
	}
	mm.pane = panePods
	if got := lastLine(mm.View()); !isBar(got, 120) {
		t.Fatalf("pods footer bar: width %d, want 120", lipgloss.Width(got))
	}
}
