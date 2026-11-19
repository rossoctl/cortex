package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// dividerLine finds the rule in a rendered view and reports which line it is on, or -1.
//
// Identified by content rather than by index, because the top block's height varies — the band
// is two lines when it draws and absent below the fold, the drawer adds five more — and an
// index would assert the layout it was written against rather than the invariant.
func dividerLine(view string) int {
	for i, ln := range strings.Split(stripANSI(view), "\n") {
		if t := strings.TrimRight(ln, " "); t != "" && strings.Trim(t, "─") == "" {
			return i
		}
	}
	return -1
}

// ON EVERY PANE AND AT EVERY SIZE, which is what lets layout() reserve its row without asking
// anything about the pane. A pane that skipped drawing it would leave a row nobody fills, and
// the reservation is where that goes wrong silently — see renderDivider.
func TestDivider_DrawnOnEveryPaneAtEverySize(t *testing.T) {
	panes := map[string]paneID{
		"sessions": paneSessions, "events": paneEvents, "pipeline": panePipeline,
		"detail": paneDetail, "usage": paneUsage,
		"namespaces": paneNamespaces, "pods": panePods,
	}
	for _, dim := range fitSizes {
		for name, p := range panes {
			m := fitModel(t, p, dim[0], dim[1], cursorRowsFixture(60))
			label := fmt.Sprintf("%s %dx%d", name, dim[0], dim[1])
			view := m.View()
			at := dividerLine(view)
			if at < 0 {
				t.Errorf("%s: no divider in the view:\n%s", label, view)
				continue
			}
			// FULL TERMINAL WIDTH. A rule as wide as the content above it would move with the
			// figures; one wider than the terminal wraps and costs the row it was meant to be.
			//
			// The RULE, measured after trailing spaces are trimmed, not the rendered line:
			// lipgloss.JoinVertical pads every line out to the widest one, and the pods picker
			// declares 62 rendered columns that layout() never fits (it fits only the pipeline
			// and catalog tables), so at 60 columns that padding would be measured here as the
			// divider being too wide. Pre-existing and worth its own fix; this assertion is
			// about the rule.
			line := strings.TrimRight(strings.Split(stripANSI(view), "\n")[at], " ")
			if got := lipgloss.Width(line); got != dim[0] {
				t.Errorf("%s: divider is %d columns in a %d-column terminal", label, got, dim[0])
			}
		}
	}
}

// BELOW THE WHOLE TOP BLOCK, not inside it. Appended after the band and its drawer, so opening
// the drawer must push the rule down rather than leaving it between the band and the breakdown.
func TestDivider_SitsBelowTheBandAndItsDrawer(t *testing.T) {
	m := fitModel(t, paneSessions, 120, 40, cursorRowsFixture(60))
	closed := dividerLine(m.View())
	if closed < 0 {
		t.Fatal("no divider with the drawer closed")
	}

	m.spend.expanded = true
	m.layout()
	open := dividerLine(m.View())
	if open < 0 {
		t.Fatal("no divider with the drawer open")
	}
	if open <= closed {
		t.Errorf("divider at line %d with the drawer open and %d with it closed — the drawer's "+
			"rows must push it down, or the rule is inside the block it closes", open, closed)
	}
}

// The rule is re-measured on resize, like every other width-dependent line. Held to the same
// standard as the tables: a narrower terminal must not leave a rule that wraps.
func TestDivider_TracksAResize(t *testing.T) {
	m := fitModel(t, paneSessions, 120, 40, cursorRowsFixture(60))
	for _, w := range []int{80, 200, 64} {
		m.width = w
		m.layout()
		lines := strings.Split(stripANSI(m.View()), "\n")
		at := dividerLine(m.View())
		if at < 0 {
			t.Fatalf("width %d: no divider", w)
		}
		if got := lipgloss.Width(lines[at]); got != w {
			t.Errorf("width %d: divider is %d columns", w, got)
		}
	}
}

// THE TWO STATES THAT DRAW NO RULE, and the property that makes that safe.
//
// paneView returns early for the edit overlay and for a zero width, so "on every pane and in
// every state" was an overclaim. Both are exempt because they are full-screen takeovers that
// render their own geometry and never read bodyHeight — the reserved row cannot be stranded in a
// body that does not exist. Asserted rather than described, because the next early return will
// inherit the exemption without inheriting the reason: if it sizes a body from bodyHeight it has
// to draw the rule, and layout() will not say so.
func TestDivider_ExemptStatesDrawNoRuleAndStillFit(t *testing.T) {
	t.Run("edit overlay", func(t *testing.T) {
		m := fitModel(t, paneSessions, 100, 30, cursorRowsFixture(60))
		m.editState.phase = editPhaseFetching
		view := m.View()
		if at := dividerLine(view); at >= 0 {
			t.Errorf("the edit overlay drew a rule at line %d; it renders its own screen", at)
		}
		// And the takeover still fits, which is what makes the unreserved row harmless.
		if got := lipgloss.Height(view); got > m.height {
			t.Errorf("edit overlay is %d lines for a %d-line terminal", got, m.height)
		}
	})
	// The zero-width state reaches paneView by two different routes, and the exemption has to
	// hold on both. A zero-value model is on paneNamespaces, whose branch returns BEFORE the
	// `m.width == 0` check — so the picker renders at width 0 and calls renderDivider(0), which
	// is empty. Only a data pane reaches "initializing…".
	t.Run("zero width on a picker renders no rule", func(t *testing.T) {
		m := &model{} // paneNamespaces is the zero value
		if at := dividerLine(m.paneView()); at >= 0 {
			t.Errorf("a zero-width picker drew a rule at line %d", at)
		}
	})
	t.Run("zero width on a data pane is a takeover", func(t *testing.T) {
		m := &model{pane: paneSessions}
		if got := m.paneView(); got != "initializing…" {
			t.Errorf("paneView at zero width = %q, want the takeover", got)
		}
	})
}

// A zero width is a real state — the model exists before the first WindowSizeMsg — and
// strings.Repeat would panic on a negative count.
func TestRenderDivider_EmptyBelowZeroWidth(t *testing.T) {
	for _, w := range []int{0, -1} {
		if got := renderDivider(w); got != "" {
			t.Errorf("renderDivider(%d) = %q, want empty", w, got)
		}
	}
}
