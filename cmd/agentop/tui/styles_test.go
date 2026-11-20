package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/rossoctl/cortex/cmd/agentop/tui/table"
)

// withProfile forces a lipgloss color profile for one test and restores it after.
func withProfile(t *testing.T, p termenv.Profile) {
	t.Helper()
	restore := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(p)
	t.Cleanup(func() { lipgloss.SetColorProfile(restore) })
}

func TestRenderHints_AsciiIsToday(t *testing.T) {
	withProfile(t, termenv.Ascii)
	in := "[↵] open  [/] search"
	if got, want := renderHints(in), styleHint.Render(in); got != want {
		t.Fatalf("renderHints under Ascii = %q, want %q", got, want)
	}
}

func TestRenderHints_ChipsUnderColor(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	in := "[↵] open  [esc] back"
	got := renderHints(in)
	if s, want := ansi.Strip(got), " ↵  open   esc  back"; s != want {
		t.Fatalf("stripped = %q, want %q", s, want)
	}
	if lipgloss.Width(got) != lipgloss.Width(in) {
		t.Fatalf("width %d, want %d", lipgloss.Width(got), lipgloss.Width(in))
	}
	if !strings.Contains(got, styleChip.Render(" ↵ ")) {
		t.Fatalf("no chip for [↵] in %q", got)
	}
}

// A hint line fitHintLine cut at the front ("… ") and at the end (a bracket cut in half)
// keeps its text: only a closed [x] with no space inside becomes a chip.
func TestRenderHints_NoChipForSpaceOrUnclosed(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	for _, in := range []string{
		"[sort: X]",
		"… [es",
		"… [esc] back  [es…",
		"[]",
		"a [[b]",
	} {
		got := renderHints(in)
		want := in
		if in == "… [esc] back  [es…" {
			want = "…  esc  back  [es…"
		}
		if in == "a [[b]" {
			want = "a [ b "
		}
		if s := ansi.Strip(got); s != want {
			t.Errorf("renderHints(%q) stripped = %q, want %q", in, s, want)
		}
		if lipgloss.Width(got) != lipgloss.Width(in) {
			t.Errorf("renderHints(%q) width %d, want %d", in, lipgloss.Width(got), lipgloss.Width(in))
		}
	}
	// The real cut: fitHintLine truncating the last hint mid-bracket.
	cut := fitHintLine("[esc] back", 4)
	if s := ansi.Strip(renderHints(cut)); s != cut {
		t.Errorf("cut hint %q became %q", cut, s)
	}
}

func TestRenderTitle_TextUnchanged(t *testing.T) {
	in := "agentop v1.2 · http://127.0.0.1:47600 · claude (1a2b3c4d)"
	withProfile(t, termenv.Ascii)
	if got, want := renderTitle(in), styleTitle.Render(in); got != want {
		t.Fatalf("renderTitle under Ascii = %q, want %q", got, want)
	}
	lipgloss.SetColorProfile(termenv.TrueColor)
	got := renderTitle(in)
	if s := ansi.Strip(got); s != in {
		t.Fatalf("stripped = %q, want %q", s, in)
	}
	if !strings.Contains(got, styleAccentBold.Render("agentop")) {
		t.Errorf("leading agentop not accent bold in %q", got)
	}
	if !strings.Contains(got, styleDim.Render(" · ")) {
		t.Errorf("separator not dim in %q", got)
	}
	if !strings.Contains(got, styleMuted.Render("http://127.0.0.1:47600")) {
		t.Errorf("URL not muted in %q", got)
	}
	if !strings.Contains(got, styleMuted.Render("(1a2b3c4d)")) {
		t.Errorf("(…) group not muted in %q", got)
	}
}

func TestStatusGroup_EveryValue(t *testing.T) {
	for _, c := range []struct {
		in   string
		want colorGroup
	}{
		{"200", groupOK}, {"204", groupOK}, {"ok", groupOK},
		{"404", groupWarn}, {"429", groupWarn}, {"running", groupWarn},
		{"500", groupError}, {"503", groupError}, {"lost", groupError}, {"error", groupError},
		{"101", groupMuted}, {"302", groupMuted}, {"n/a", groupMuted}, {"", groupMuted},
		{"—", groupDim}, {"  200", groupOK},
	} {
		if got := statusGroup(c.in); got != c.want {
			t.Errorf("statusGroup(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestActionGroup_EveryValue(t *testing.T) {
	for _, c := range []struct {
		in   string
		want colorGroup
	}{
		{"modify", groupAccent}, {"modify*", groupAccent},
		{"deny", groupError}, {"deny*", groupError},
		{"tunnel", groupDim}, {"—", groupDim},
		{"allow", groupMuted}, {"allow*", groupMuted},
		{"observe", groupMuted}, {"observe*", groupMuted},
		{"", groupMuted}, {"bogus", groupMuted},
	} {
		if got := actionGroup(c.in); got != c.want {
			t.Errorf("actionGroup(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestDashDim_DashCellIsDim(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	f := dashDim(func(table.Row, int) lipgloss.Style { return styleOK })
	if got := f(table.Row{" — "}, 0).Render("x"); got != styleDim.Render("x") {
		t.Errorf("dash cell = %q, want dim", got)
	}
	if got := f(table.Row{"200"}, 0).Render("x"); got != styleOK.Render("x") {
		t.Errorf("other cell = %q, want the wrapped style", got)
	}
}

func TestAssertPlainInvariant_AcceptsAChippedHintLine(t *testing.T) {
	assertPlainInvariant(t, func() string {
		return renderTitle("agentop · x") + "\n" + renderHints("[q] quit  [/] search")
	}, -1)
}

func TestAssertPlainInvariant_AcceptsABarThatAsciiAlsoDraws(t *testing.T) {
	assertPlainInvariant(t, func() string {
		return styleDetailCursor.Render("▌") + " row\n" + styleDetailCursor.Render("▌") + " [q] quit"
	}, -1)
}
