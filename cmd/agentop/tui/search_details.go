package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/rossoctl/cortex/cmd/agentop/tui/find"
)

// THE EVENTS PANE'S `details` BOX (#1680). With the box on, Enter, n and N find the next row
// whose detail view shows the query: every field the pane draws, the raw capture too. The
// events list carries summaries, so the scan fetches each event it checks, one at a time, and
// drops it after the check. It stops at the first match, so its cost is the distance to the
// next match, not the size of the session.

// detailHas reports whether the detail pane, opened on r, shows q: in a header line, or in a
// line of the tree with every fold open. It reads the lines before wrapping, with the matcher
// the pane's own search uses. So a row it reports opens on a highlighted match.
func detailHas(r eventRow, q string) bool {
	head, root := detailContent(r)
	for _, h := range head {
		for _, l := range strings.Split(h, "\n") {
			if find.Contains(ansi.Strip(l), q) {
				return true
			}
		}
	}
	if root == nil {
		return false
	}
	for _, l := range renderTree(root, foldState{all: true}) {
		if find.Contains(ansi.Strip(l.text), q) {
			return true
		}
	}
	return false
}
