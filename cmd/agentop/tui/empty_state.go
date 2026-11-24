package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// emptyState is msg centered in a width x height body, muted, with a dim key line under it.
// key is the bare key name ("esc"); before and after are the text around it.
//
// msg may hold "\n"; each line is centered on its own. The key is a chip under color and
// "[key]" under Ascii, the same width either way. Lines are cut after styling (truncToWidth),
// and the box pads the rows below, so the block is only the blank rows above it and its lines.
func emptyState(msg, before, key, after string, width, height int) string {
	chip := "[" + key + "]"
	if colorOn() {
		chip = styleChip.Render(" " + key + " ")
	}
	var block []string
	for _, l := range strings.Split(msg, "\n") {
		block = append(block, styleMuted.Render(l))
	}
	block = append(block, styleDim.Render(before)+chip+styleDim.Render(after))

	out := make([]string, max((height-len(block))/2, 0), max(height, len(block)))
	for _, l := range block {
		if width > 0 { // 0: no WindowSizeMsg yet, so nothing to fit or center in
			l = truncToWidth(l, width)
		}
		out = append(out, strings.Repeat(" ", max((width-lipgloss.Width(l))/2, 0))+l)
	}
	return strings.Join(out, "\n")
}

// paneEmptyState is emptyState sized to the pane body: the inner width and the rows layout()
// reserved for the body (the search prompt's row is reserved apart from them).
func (m *model) paneEmptyState(msg, before, key, after string) string {
	return emptyState(msg, before, key, after, m.innerWidth(), m.bodyHeight)
}
