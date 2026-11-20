package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

func TestBoxTop_Plain(t *testing.T) {
	if got := ansi.Strip(boxTop("", 10)); got != "╭────────╮" {
		t.Errorf("boxTop(\"\", 10) = %q", got)
	}
	for _, w := range []int{1, 0, -1} {
		if got := boxTop("", w); got != "" {
			t.Errorf("boxTop(\"\", %d) = %q, want empty", w, got)
		}
	}
}

func TestBoxTop_Label(t *testing.T) {
	got := boxTop("AB", 10)
	if s := ansi.Strip(got); s != "╭─ AB ───╮" {
		t.Errorf("boxTop(\"AB\", 10) = %q", s)
	}
	if w := lipgloss.Width(got); w != 10 {
		t.Errorf("boxTop(\"AB\", 10) is %d columns", w)
	}
}

func TestBoxBottom(t *testing.T) {
	if got := ansi.Strip(boxBottom(6)); got != "╰────╯" {
		t.Errorf("boxBottom(6) = %q", got)
	}
	if got := boxBottom(1); got != "" {
		t.Errorf("boxBottom(1) = %q, want empty", got)
	}
}

func TestBoxFrame_PadsAndCuts(t *testing.T) {
	got := boxFrame("a\nb", 6, 4)
	want := []string{"│a   │", "│b   │", "│    │", "│    │"}
	if len(got) != len(want) {
		t.Fatalf("boxFrame gave %d lines, want %d", len(got), len(want))
	}
	for i := range want {
		if s := ansi.Strip(got[i]); s != want[i] {
			t.Errorf("line %d = %q, want %q", i, s, want[i])
		}
	}
	if got := boxFrame("1\n2\n3", 6, 2); len(got) != 2 || ansi.Strip(got[1]) != "│2   │" {
		t.Errorf("a 3-line body at height 2 = %q", got)
	}
	if got := boxFrame("a", 1, 3); len(got) != 3 || got[0] != "" {
		t.Errorf("boxFrame at width 1 = %q, want 3 empty lines", got)
	}

	restore := lipgloss.ColorProfile()
	t.Cleanup(func() { lipgloss.SetColorProfile(restore) })
	lipgloss.SetColorProfile(termenv.TrueColor)
	styled := styleError.Render("abcdefghij")
	for _, ln := range boxFrame(styled, 6, 1) {
		if w := lipgloss.Width(ln); w != 6 {
			t.Errorf("a styled line too wide is %d columns after the frame, want 6: %q", w, ln)
		}
		if s := ansi.Strip(ln); s != "│abcd│" {
			t.Errorf("styled line framed = %q", s)
		}
	}
}

// boxedPanes are the panes that draw the body box: TestLayout_EveryPaneFitsTheTerminal's,
// and the two pickers.
var boxedPanes = map[string]paneID{
	"sessions": paneSessions, "events": paneEvents, "pipeline": panePipeline,
	"detail": paneDetail, "usage": paneUsage, "agents": paneAgents, "timeline": paneTimeline,
	"namespaces": paneNamespaces, "pods": panePods,
}

// boxSpan finds the box in a view: the line of its top edge and of its bottom edge, or -1.
func boxSpan(view string) (top, bottom int) {
	top, bottom = -1, -1
	for i, ln := range strings.Split(ansi.Strip(view), "\n") {
		ln = strings.TrimRight(ln, " ")
		switch {
		case top < 0 && strings.HasPrefix(ln, "╭") && strings.HasSuffix(ln, "╮"):
			top = i
		case top >= 0 && strings.HasPrefix(ln, "╰") && strings.HasSuffix(ln, "╯"):
			return top, i
		}
	}
	return top, -1
}

// ON EVERY PANE AND AT EVERY SIZE, which is what lets layout() reserve the box rows without
// asking anything about the pane: the top edge, framed rows, the bottom edge, then the bar.
func TestBox_DrawnOnEveryPaneAtEverySize(t *testing.T) {
	for _, dim := range fitSizes {
		for name, p := range boxedPanes {
			m := fitModel(t, p, dim[0], dim[1], cursorRowsFixture(60))
			label := fmt.Sprintf("%s %dx%d", name, dim[0], dim[1])
			view := m.View()
			lines := strings.Split(ansi.Strip(view), "\n")
			top, bottom := boxSpan(view)
			if top < 0 || bottom < 0 {
				t.Errorf("%s: no box in the view:\n%s", label, view)
				continue
			}
			for i := top; i <= bottom; i++ {
				if w := lipgloss.Width(lines[i]); w != dim[0] {
					t.Errorf("%s: box line %d is %d columns, want %d", label, i, w, dim[0])
				}
			}
			for i := top + 1; i < bottom; i++ {
				if !strings.HasPrefix(lines[i], "│") || !strings.HasSuffix(lines[i], "│") {
					t.Errorf("%s: line %d inside the box is not framed: %q", label, i, lines[i])
				}
			}
			// Only the footer is under the box, and its last line is the key bar.
			if got := len(lines) - 1 - bottom; got != lipgloss.Height(m.footerView()) && p != paneNamespaces && p != panePods {
				t.Errorf("%s: %d lines under the box, want the footer's %d", label, got, lipgloss.Height(m.footerView()))
			}
			if last := lines[len(lines)-1]; !strings.Contains(last, "[q] quit") {
				t.Errorf("%s: the last line is not the key bar: %q", label, last)
			}
		}
	}
}

// THE TWO STATES THAT DRAW NO BOX, and the property that makes that safe.
//
// paneView returns early for the edit overlay and for a zero width. Both are full-screen
// takeovers that render their own geometry and never read bodyHeight, so the rows layout()
// reserved for the box cannot be stranded in a body that does not exist. Asserted rather than
// described, because the next early return will inherit the exemption without inheriting the
// reason: if it sizes a body from bodyHeight it has to draw the box, and layout() will not say so.
func TestBox_ExemptStatesDrawNoBoxAndStillFit(t *testing.T) {
	t.Run("edit overlay", func(t *testing.T) {
		m := fitModel(t, paneSessions, 100, 30, cursorRowsFixture(60))
		m.editState.phase = editPhaseFetching
		view := m.View()
		// The edit dialog has a rounded border of its own; the body box is the one as wide as
		// the terminal.
		if edge := "╭" + strings.Repeat("─", m.width-2) + "╮"; strings.Contains(ansi.Strip(view), edge) {
			t.Errorf("the edit overlay drew the body box; it renders its own screen")
		}
		if got := lipgloss.Height(view); got > m.height {
			t.Errorf("edit overlay is %d lines for a %d-line terminal", got, m.height)
		}
	})
	// A zero-value model is on paneNamespaces, whose branch returns BEFORE the `m.width == 0`
	// check — so the picker renders at width 0, where every box function draws nothing.
	t.Run("zero width on a picker renders no box", func(t *testing.T) {
		m := &model{} // paneNamespaces is the zero value
		if top, _ := boxSpan(m.paneView()); top >= 0 {
			t.Errorf("a zero-width picker drew a box at line %d", top)
		}
	})
	t.Run("zero width on a data pane is a takeover", func(t *testing.T) {
		m := &model{pane: paneSessions}
		if got := m.paneView(); got != "initializing…" {
			t.Errorf("paneView at zero width = %q, want the takeover", got)
		}
	})
}

// Every pane body is sized from m.innerWidth(), so boxFrame never has to cut it. A pane that
// still sized from m.width would be hidden by the cut; this finds it. Usage is out: its chart
// does not follow the body size at small terminals, and the cut is its fix (plan Decision 3).
// The pickers are out: their tables are not fitted, and the cut is what bounds them.
func TestBox_BodyFitsInsideWithoutCutting(t *testing.T) {
	forceColor(t)
	panes := map[string]paneID{
		"sessions": paneSessions, "events": paneEvents, "pipeline": panePipeline,
		"detail": paneDetail, "agents": paneAgents, "timeline": paneTimeline,
	}
	for _, dim := range fitSizes {
		for name, p := range panes {
			for _, searching := range []bool{false, true} {
				if searching && (&model{}).surface(p) == nil {
					continue
				}
				m := fitModel(t, p, dim[0], dim[1], cursorRowsFixture(60))
				if searching {
					m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
				}
				_, body := m.paneContent()
				for i, ln := range strings.Split(body, "\n") {
					if w := lipgloss.Width(ln); w > m.innerWidth() {
						t.Errorf("%s %dx%d searching=%v: body line %d is %d columns, inner width %d: %q",
							name, dim[0], dim[1], searching, i, w, m.innerWidth(), trunc(ansi.Strip(ln), 50))
					}
				}
			}
		}
	}
}

// AN OVERLAY OVER THE BOXED BODY keeps the box's right edge beside it, and no line is wider
// than the terminal.
func TestOverlayCenter_KeepsTheRightEdge(t *testing.T) {
	forceColor(t)
	edges := 0
	for _, tc := range []struct {
		name    string
		build   func(m *model)
		overlay func(m *model) string // what View draws over the pane, to know its width
	}{
		{"help", func(m *model) { m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}}) },
			func(m *model) string {
				return renderHelpOverlay(m.helpVp, m.width, m.height, m.helpPrompt(), m.searchStatus(targetHelp))
			}},
		{"column picker", func(m *model) { m.pane = paneEvents; m.colPicker = true },
			func(m *model) string {
				return renderColumnPicker(m.eventColumns, m.eventColWidths, m.colCursor, m.width, m.height, m.sortCol, m.sortDesc)
			}},
		{"server picker", func(m *model) {
			m.pane = paneAgents
			m.serverPicker = &serverPicker{agent: "claude-code", entries: []serverChoice{
				{}, {name: "local", host: "http://127.0.0.1:11434", mapping: "opus → granite"}}}
		}, func(m *model) string { return renderServerPicker(m.serverPicker, m.width) }},
		{"erase dialog", func(m *model) { m.clearConfirm = &clearConfirm{target: eraseSession, id: "sess-1"} },
			func(m *model) string { return renderClearConfirm(m.clearConfirm, m.width) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := fitModel(t, paneSessions, 100, 30, cursorRowsFixture(60))
			tc.build(m)
			m.layout()
			overW := 0
			for _, l := range strings.Split(tc.overlay(m), "\n") {
				overW = max(overW, lipgloss.Width(l))
			}
			// The overlay reaches the last column: no base text is right of it.
			reachesEdge := (m.width-overW)/2+overW >= m.width
			base := strings.Split(ansi.Strip(m.paneView()), "\n")
			view := strings.Split(ansi.Strip(m.View()), "\n")
			changed := 0
			for i, ln := range view {
				if w := lipgloss.Width(ln); w > m.width {
					t.Errorf("line %d is %d columns for a %d-column terminal", i, w, m.width)
				}
				if i >= len(base) || ln == base[i] {
					continue
				}
				changed++
				// A row the overlay covers, where the base had the box's right edge: it stays.
				if !reachesEdge && strings.HasSuffix(base[i], "│") {
					edges++
					if !strings.HasSuffix(ln, "│") {
						t.Errorf("line %d lost the box's right edge beside the overlay: %q", i, ln)
					}
				}
			}
			if changed == 0 {
				t.Fatalf("the %s overlay changed no line, so this case asserts nothing", tc.name)
			}
		})
	}
	if edges == 0 {
		t.Fatal("no overlay left base text to its right, so the right edge is unasserted")
	}
}

// A SELECTED-ROW TINT AND A SEARCH MATCH, under color, inside the box: boxFrame cuts and pads
// after styling with ANSI-aware functions only, so the right │ is on column m.width on every
// framed row.
func TestBox_StyledRowsKeepTheRightEdge(t *testing.T) {
	forceColor(t)
	for _, w := range []int{60, 80, 140} {
		m := fitModel(t, paneEvents, w, 30, cursorRowsFixture(60))
		m.search = map[paneID]string{paneEvents: "h"} // matches every row, to the right edge
		m.rebuildEventsTable()
		view := m.View()
		top, bottom := boxSpan(view)
		if top < 0 || bottom < 0 {
			t.Fatalf("%d: no box", w)
		}
		if !strings.Contains(view, "\x1b[") {
			t.Fatalf("%d: the view has no style, so this asserts nothing", w)
		}
		lines := strings.Split(view, "\n")
		for i := top + 1; i < bottom; i++ {
			if got := lipgloss.Width(lines[i]); got != w {
				t.Errorf("%d: framed line %d is %d columns", w, i, got)
			}
			if !strings.HasSuffix(ansi.Strip(lines[i]), "│") {
				t.Errorf("%d: framed line %d does not end with │: %q", w, i, ansi.Strip(lines[i]))
			}
		}
	}
}

// THE HELP OVERLAY'S SEARCH PROMPT is drawn on the overlay, not above the body, so the frame
// reserves no row for it: the view stays the terminal's height, before and after a re-layout.
func TestBox_HelpSearchPromptKeepsTheHeight(t *testing.T) {
	for _, p := range []paneID{paneSessions, paneEvents, paneNamespaces} {
		m := fitModel(t, p, 100, 30, cursorRowsFixture(60))
		m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
		m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
		if !m.searching || m.searchPane != targetHelp {
			t.Fatalf("pane %v: searching=%v target=%v, want the prompt open on help", p, m.searching, m.searchPane)
		}
		for _, when := range []string{"open", "re-layout"} {
			if when == "re-layout" {
				m.layout()
			}
			if got := len(strings.Split(m.View(), "\n")); got != m.height {
				t.Errorf("pane %v, %s: view is %d lines for a %d-line terminal", p, when, got, m.height)
			}
		}
	}
}

// A DOUBLE-WIDTH RUNE ACROSS THE OVERLAY'S RIGHT EDGE: its half right of the panel is a space,
// so the row is not wider than the terminal and the box's right edge stays on the last column.
func TestOverlayCenter_WideRuneAtTheRightEdge(t *testing.T) {
	base := "│" + strings.Repeat("中", 49) + "│"
	for _, overW := range []int{20, 21} {
		overlay := strings.Repeat("x", overW)
		got := strings.Split(overlayCenter(base, overlay, 100, 1), "\n")[0]
		if w := lipgloss.Width(got); w != 100 {
			t.Errorf("overlay %d wide: row is %d columns for a 100-column terminal: %q", overW, w, got)
		}
		if !strings.HasSuffix(got, "│") || !strings.Contains(got, overlay) {
			t.Errorf("overlay %d wide: row lost the edge or the panel: %q", overW, got)
		}
	}
	// A base row that ends under the panel gets no tail.
	if got := overlayCenter("中中", "xxxx", 10, 1); got != "中 xxxx" {
		t.Errorf("short base row: got %q, want %q", got, "中 xxxx")
	}
}
