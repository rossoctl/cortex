package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// helpSep is the plain text of the separator between the two help columns.
const helpSep = " │ "

// helpColumns splits a two-column body line into its left and right column. ok is false
// when the line has no separator.
func helpColumns(line string) (left, right string, ok bool) {
	plain := ansi.Strip(line)
	i := strings.Index(plain, helpSep)
	if i < 0 {
		return "", "", false
	}
	return plain[:i], plain[i+len(helpSep):], true
}

// TestHelpBody_TwoColumnsAt140: a 140-column terminal gives a 134-column body, which has
// the pane group on the left beside ANYWHERE on the right, and EVERY PANE below them in
// one column.
func TestHelpBody_TwoColumnsAt140(t *testing.T) {
	const width = 134
	colW := (width - 3) / 2
	body := helpBodyLines(paneSessions, width)
	lines := strings.Split(body, "\n")

	left, right, ok := helpColumns(lines[0])
	if !ok {
		t.Fatalf("first line has no %q separator: %q", helpSep, ansi.Strip(lines[0]))
	}
	if !strings.HasPrefix(left, paneKeys[paneSessions].title) {
		t.Errorf("left column does not start with the pane group: %q", left)
	}
	if !strings.HasPrefix(right, anywhereTitle) {
		t.Errorf("right column does not start with %q: %q", anywhereTitle, right)
	}

	var leftText, rightText strings.Builder
	every := -1
	for i, ln := range lines {
		if w := lipgloss.Width(ln); w > width {
			t.Errorf("line %d is %d columns, over %d: %q", i, w, width, ansi.Strip(ln))
		}
		if strings.HasPrefix(ansi.Strip(ln), everyPaneTitle) {
			every = i
			break
		}
		l, r, ok := helpColumns(ln)
		if !ok {
			continue
		}
		if lipgloss.Width(l) != colW {
			t.Errorf("line %d: left column is %d wide, want %d: %q", i, lipgloss.Width(l), colW, l)
		}
		leftText.WriteString(l + "\n")
		rightText.WriteString(r + "\n")
	}
	if every < 0 {
		t.Fatalf("no %q section in one column:\n%s", everyPaneTitle, ansi.Strip(body))
	}
	for _, ln := range lines[every:] {
		if strings.Contains(ansi.Strip(ln), helpSep) {
			t.Errorf("EVERY PANE has a column separator: %q", ansi.Strip(ln))
		}
	}

	l, r := leftText.String(), rightText.String()
	if !(strings.Index(l, paneKeys[paneSessions].title) < strings.Index(l, jumpSectionTitle)) {
		t.Errorf("left column is not the pane group then %q:\n%s", jumpSectionTitle, l)
	}
	order := []string{anywhereTitle, helpNavTitle, spendDrawerTitle, drillSectionTitle}
	at := -1
	for _, title := range order {
		i := strings.Index(r, title)
		if i <= at {
			t.Errorf("right column does not hold %v in order (at %q):\n%s", order, title, r)
			break
		}
		at = i
	}
}

// TestHelpBody_OneColumnAt80: an 80-column terminal gives a 74-column body, which stays
// one column, in the order of today.
func TestHelpBody_OneColumnAt80(t *testing.T) {
	body := ansi.Strip(helpBodyLines(paneSessions, 74))
	if strings.Contains(body, helpSep) {
		t.Fatalf("74-column body has a column separator:\n%s", body)
	}
	order := []string{paneKeys[paneSessions].title, jumpSectionTitle, drillSectionTitle,
		anywhereTitle, helpNavTitle, spendDrawerTitle, everyPaneTitle}
	at := -1
	for _, title := range order {
		i := strings.Index(body, title)
		if i <= at {
			t.Fatalf("body does not hold %v in order (at %q):\n%s", order, title, body)
		}
		at = i
	}
}

// TestHelpBody_WrapsUnderDescription: in a column, a long description wraps to the
// description column, not to the key column, and no line is wider than its column.
func TestHelpBody_WrapsUnderDescription(t *testing.T) {
	const width = 134
	colW := (width - 3) / 2
	lines := strings.Split(helpBodyLines(paneSessions, width), "\n")
	for i, ln := range lines {
		l, r, ok := helpColumns(ln)
		if !ok {
			continue
		}
		// The row is padded to the full width (so the overlay spans the terminal); the
		// right column's text still ends inside colW.
		if lipgloss.Width(ln) != width {
			t.Errorf("line %d is %d columns, want the full %d", i, lipgloss.Width(ln), width)
		}
		if lipgloss.Width(l) > colW || lipgloss.Width(strings.TrimRight(r, " ")) > colW {
			t.Errorf("line %d is wider than its %d-column column: %q | %q", i, colW, l, r)
		}
	}

	// A pane binding whose description needs more than one row at this column width.
	checked := 0
	for _, kb := range paneKeys[paneSessions].bindings {
		row := -1
		for i, ln := range lines {
			if l, _, ok := helpColumns(ln); ok && strings.HasPrefix(strings.TrimSpace(l), kb.keys+" ") {
				row = i
				break
			}
		}
		if row < 0 || row+1 >= len(lines) {
			continue
		}
		l, _, _ := helpColumns(lines[row])
		first := strings.Fields(kb.desc)[0]
		descCol := strings.Index(l, "  "+first) + 2
		wrapped := wrapWords(kb.desc, colW-descCol)
		if len(wrapped) < 2 {
			continue
		}
		next, _, _ := helpColumns(lines[row+1])
		want := strings.Repeat(" ", descCol) + wrapped[1]
		if strings.TrimRight(next, " ") != want {
			t.Errorf("%q wraps to %q, want %q (under the description)", kb.keys, next, want)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("setup: no sessions binding wraps at this width")
	}
}

// TestHelpBody_CurrentPaneEmphasis: in two columns, the pane group keeps its title style
// and its key style.
func TestHelpBody_CurrentPaneEmphasis(t *testing.T) {
	restore := lipgloss.ColorProfile()
	t.Cleanup(func() { lipgloss.SetColorProfile(restore) })
	lipgloss.SetColorProfile(termenv.TrueColor)

	g := paneKeys[paneSessions]
	body := helpBodyLines(paneSessions, 134)
	if want := styleTitle.Render(g.title); !strings.Contains(body, want) {
		t.Errorf("pane group title is not in styleTitle: want %q in\n%q", want, body)
	}
	if want := styleOK.Render(padRight(g.bindings[0].keys, helpKeyColWidth)); !strings.Contains(body, want) {
		t.Errorf("pane group keys are not in styleOK: want %q", want)
	}
	if want := styleDim.Render(helpSep); !strings.Contains(body, want) {
		t.Errorf("separator is not dim: want %q", want)
	}
	lipgloss.SetColorProfile(termenv.Ascii)
	plain := helpBodyLines(paneSessions, 134)
	if ansi.Strip(body) != plain {
		t.Error("the plain text differs under TrueColor and Ascii")
	}
}

// TestHelpOverlay_TwoColumnsScroll: the two-column body still scrolls, and the foot says
// so.
func TestHelpOverlay_TwoColumnsScroll(t *testing.T) {
	m := helpModelAt(t, paneSessions, 140, 20)
	if m.helpVp.TotalLineCount() <= m.helpVp.VisibleLineCount() {
		t.Fatal("setup: the body fits, so nothing scrolls")
	}
	if v := stripANSI(m.View()); !strings.Contains(v, "[↑↓] scroll  0%") {
		t.Errorf("no scroll hint on the two-column overlay:\n%s", v)
	}
	m.Update(keyRune('G'))
	if !m.helpVp.AtBottom() {
		t.Error("G did not reach the bottom of the two-column body")
	}
}

// TestHelpSearch_FindsTheRightColumn: a search on a two-column body highlights a match in
// the right column, and n moves to it.
func TestHelpSearch_FindsTheRightColumn(t *testing.T) {
	restore := lipgloss.ColorProfile()
	t.Cleanup(func() { lipgloss.SetColorProfile(restore) })
	lipgloss.SetColorProfile(termenv.TrueColor)

	const q = "cycle the axis"
	m := helpModelAt(t, paneSessions, 140, 40)
	typeKeys(m, "/"+q)
	press(m, tea.KeyEnter)
	if len(m.helpDoc.matches) != 1 {
		t.Fatalf("matches = %v, want one", m.helpDoc.matches)
	}
	row := m.helpDoc.matches[0]
	line := ansi.Strip(m.helpDoc.lines[row])
	if i := strings.Index(line, helpSep); i < 0 || strings.Index(line, q) < i {
		t.Fatalf("setup: the match is not in the right column: %q", line)
	}
	m.Update(keyRune('n'))
	if r, ok := m.helpDoc.currentRow(); !ok || r != row {
		t.Errorf("n: current = %d,%v, want %d", r, ok, row)
	}
	if want := styleCurrentMatch.Render(q); !strings.Contains(m.helpVp.View(), want) {
		t.Errorf("the right-column match is not highlighted: want %q in\n%q", want, m.helpVp.View())
	}
}
