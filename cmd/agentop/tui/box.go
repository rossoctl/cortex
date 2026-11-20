package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// The rows and columns the body box costs, and the numbers layout() must reserve for it.
//
// Named constants for literal 1s and 2s, for the reason spendBandLines and spendDrawerLines are:
// paneView draws them and layout() reserves them, in two files, and the two going out of step is
// not a cosmetic bug. Under-reserve and the view is taller than the terminal, which scrolls it
// and takes the footer off the bottom; over-reserve and the body leaves a row nobody fills.
const (
	boxSideCols    = 2 // the │ on each side
	boxTopLines    = 1 // the top edge
	boxBottomLines = 1 // the bottom edge
)

// innerWidth is the width a pane body renders at: the terminal less the box's two sides.
func (m *model) innerWidth() int { return max(m.width-boxSideCols, 0) }

// boxTop is the box's top edge, ╭─…─╮ across width, with label after "╭─ " when label is
// not empty. The label is already styled and already fitted to width-6 columns; the edge keeps
// at least one ─ after it.
//
// DRAWN ON EVERY PANE THAT HAS A BODY, whether or not the spend band or its drawer had anything
// to say. That is what makes it reservable unconditionally in layout().
//
// TWO STATES DO NOT DRAW IT: paneView returns early for the edit overlay and for `m.width == 0`
// before the first WindowSizeMsg. Both are FULL-SCREEN TAKEOVERS that render their own geometry
// and never read bodyHeight, so the rows layout() reserved cannot be stranded. Whoever adds a
// third early return that renders a body sized from bodyHeight has to draw the box too, and the
// reservation will not say so. TestBox_ExemptStatesDrawNoBoxAndStillFit pins both.
//
// FULL TERMINAL WIDTH, not the width of the content: an edge that moved with the figures in it
// would read as a rendering bug rather than as structure.
func boxTop(label string, width int) string {
	if width < 2 {
		return ""
	}
	k := width - 5 - lipgloss.Width(label) // "╭─ " + label + " " + k×"─" + "╮"
	if label == "" || k < 1 {
		return styleDim.Render("╭" + strings.Repeat("─", width-2) + "╮")
	}
	return styleDim.Render("╭─ ") + label + styleDim.Render(" "+strings.Repeat("─", k)+"╮")
}

// boxBottom is the box's bottom edge, ╰─…─╯ across width.
func boxBottom(width int) string {
	if width < 2 {
		return ""
	}
	return styleDim.Render("╰" + strings.Repeat("─", width-2) + "╯")
}

// boxFrame frames body as exactly height lines: "│", the line cut or padded to width-2 columns,
// "│". A short body gets blank framed rows, so the footer under the box stays on the last row;
// a tall one is cut at height, so the footer is not pushed off the screen.
//
// The cut and the pad count display columns of the styled line (truncToWidth, lipgloss.Width),
// so a selected-row tint or a search match at the right edge keeps its width.
func boxFrame(body string, width, height int) []string {
	if height <= 0 {
		return nil
	}
	out := make([]string, height)
	if width < 2 {
		return out
	}
	inner := width - boxSideCols
	side := styleDim.Render("│")
	lines := strings.Split(body, "\n")
	for i := range out {
		ln := ""
		if i < len(lines) {
			ln = truncToWidth(lines[i], inner)
		}
		if w := lipgloss.Width(ln); w < inner {
			ln += strings.Repeat(" ", inner-w)
		}
		out[i] = side + ln + side
	}
	return out
}
