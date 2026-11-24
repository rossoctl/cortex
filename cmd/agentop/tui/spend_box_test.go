package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// The spend band is the label of the body box's top edge (#2020, change 4), not a row of its own.
func TestBandInTheTopEdge(t *testing.T) {
	forceColor(t)
	m := fitModel(t, paneSessions, 140, 30, cursorRowsFixture(60))
	lines := strings.Split(m.View(), "\n")
	edge := lines[1]
	plain := ansi.Strip(edge)
	if !strings.HasPrefix(plain, "╭─ LAST 1H") || !strings.HasSuffix(plain, "╮") {
		t.Errorf("the line after the title is not the box edge with the band in it: %q", plain)
	}
	if w := lipgloss.Width(edge); w != 140 {
		t.Errorf("the top edge is %d columns, want 140", w)
	}
}

// The band in the edge is fitted to width-6 by spendBandCells, which drops whole periods, so the
// edge is always the terminal width and never wraps.
func TestBandInTheTopEdge_NarrowDropsWholePeriods(t *testing.T) {
	forceColor(t)
	for _, w := range []int{30, 40, 60, 80} {
		m := fitModel(t, paneSessions, w, 30, cursorRowsFixture(60))
		// Trimmed: at 30 columns the title (not fitted) is wider than the terminal, and
		// JoinVertical pads every line to it. The edge itself is what this measures.
		edge := strings.TrimRight(ansi.Strip(strings.Split(m.View(), "\n")[1]), " ")
		if lipgloss.Width(edge) != w {
			t.Errorf("width %d: the top edge is %d columns", w, lipgloss.Width(edge))
		}
		band, _ := renderSpendBandStyled(m.spendSummary(), w-6, m.spendBandNeedsScopeMarker())
		if want := ansi.Strip(boxTop(band[0], w)); edge != want {
			t.Errorf("width %d: the top edge is\n  %q, want\n  %q", w, edge, want)
		}
		if band[0] == "" {
			t.Errorf("width %d: no band cell fit, so this case asserts nothing", w)
		}
	}
}

// Below the strip's floor the band does not draw, and the edge is plain.
func TestBandInTheTopEdge_NoBandBelowTheStripFloor(t *testing.T) {
	m := fitModel(t, paneSessions, 100, spendStripMinHeight-1, cursorRowsFixture(60))
	edge := ansi.Strip(strings.Split(m.View(), "\n")[1])
	if want := "╭" + strings.Repeat("─", 98) + "╮"; edge != want {
		t.Errorf("at height %d the top edge is %q, want a plain edge", m.height, edge)
	}
}

// With `$` open, the drawer is its own box under the band, and the table has a plain box under it.
func TestDrawerIsItsOwnPanel(t *testing.T) {
	forceColor(t)
	for _, w := range []int{80, 140} {
		for name, p := range map[string]paneID{"sessions": paneSessions, "events": paneEvents, "agents": paneAgents} {
			m := fitModel(t, p, w, 30, cursorRowsFixture(60))
			m.spend.drawer.snap = reasoningSnap()
			for span := spendSpan(0); span < numSpendSpans; span++ {
				m.spend.chains[span].snap = reasoningSnap()
			}
			m.handleKey(runeKey('$'))
			label := fmt.Sprintf("%s %dx30", name, w)
			if !m.spendDrawerVisible() {
				t.Fatalf("%s: the drawer did not open", label)
			}
			view := m.View()
			if got := lipgloss.Height(view); got != 30 {
				t.Errorf("%s: view is %d lines, want 30", label, got)
			}
			lines := strings.Split(ansi.Strip(view), "\n")
			n := spendDrawerLinesFor(m.innerWidth())
			framed := func(i int) bool {
				return strings.HasPrefix(lines[i], "│") && strings.HasSuffix(lines[i], "│")
			}
			if !strings.HasPrefix(lines[1], "╭─ ") || !strings.HasSuffix(lines[1], "╮") {
				t.Errorf("%s: the band is not in the first top edge: %q", label, lines[1])
			}
			drew := false
			for i := 2; i < 2+n; i++ {
				if !framed(i) {
					t.Errorf("%s: drawer row %d is not framed: %q", label, i, lines[i])
				}
				if strings.Trim(lines[i], "│ ") != "" {
					drew = true
				}
			}
			if !drew {
				t.Errorf("%s: the drawer box has no content", label)
			}
			if !strings.HasPrefix(lines[2+n], "╰") || !strings.HasSuffix(lines[2+n], "╯") {
				t.Errorf("%s: no bottom edge under the drawer: %q", label, lines[2+n])
			}
			if want := "╭" + strings.Repeat("─", w-2) + "╮"; lines[3+n] != want {
				t.Errorf("%s: no plain top edge over the table: %q", label, lines[3+n])
			}
			for i := 4 + n; i < 4+n+m.bodyHeight; i++ {
				if !framed(i) {
					t.Errorf("%s: body row %d is not framed: %q", label, i, lines[i])
				}
			}
			if b := lines[4+n+m.bodyHeight]; !strings.HasPrefix(b, "╰") || !strings.HasSuffix(b, "╯") {
				t.Errorf("%s: no bottom edge under the table: %q", label, b)
			}
			if got, want := len(lines)-5-n-m.bodyHeight, lipgloss.Height(m.footerView()); got != want {
				t.Errorf("%s: %d lines under the table box, want the footer's %d", label, got, want)
			}
		}
	}
}

// Rows add up with the drawer open and closed, on every pane. At 80x24 the drawer cannot open
// (24 < spendDrawerMinHeight), so `$` must leave the layout as it was.
func TestLayout_RowsAddUpWithTheDrawer(t *testing.T) {
	for _, dim := range [][2]int{{80, 24}, {100, 30}, {140, 30}} {
		for name, p := range boxedPanes {
			m := fitModel(t, p, dim[0], dim[1], cursorRowsFixture(60))
			label := fmt.Sprintf("%s %dx%d", name, dim[0], dim[1])
			if got := lipgloss.Height(m.View()); got != m.height {
				t.Errorf("%s closed: view is %d lines, want %d", label, got, m.height)
			}
			closed := m.bodyHeight
			m.handleKey(runeKey('$'))
			if got := lipgloss.Height(m.View()); got != m.height {
				t.Errorf("%s open: view is %d lines, want %d", label, got, m.height)
			}
			if dim[1] < spendDrawerMinHeight && (m.spendDrawerVisible() || m.bodyHeight != closed) {
				t.Errorf("%s: `$` changed the layout below the drawer's floor (body %d -> %d)",
					label, closed, m.bodyHeight)
			}
		}
	}
}
