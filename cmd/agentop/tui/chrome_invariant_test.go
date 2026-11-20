package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// hintLinesOf is the index of each line of view that holds one of marks: the hint lines
// of an overlay, found by their text.
func hintLinesOf(view string, marks ...string) []int {
	var out []int
	for i, ln := range strings.Split(view, "\n") {
		for _, mk := range marks {
			if strings.Contains(ln, mk) {
				out = append(out, i)
				break
			}
		}
	}
	return out
}

// TestChrome_PlainTextInvariant renders every pane and overlay under Ascii and under color,
// in the dark and the light theme, and checks that color changed no text and no width.
func TestChrome_PlainTextInvariant(t *testing.T) {
	type view struct {
		name  string
		build func(m *model)
		marks []string
	}
	panes := []paneID{paneSessions, paneEvents, paneDetail, panePipeline, paneUsage,
		paneAgents, paneTimeline, paneNamespaces, panePods}
	var views []view
	for _, p := range panes {
		views = append(views, view{name: fmt.Sprintf("pane %d", p), build: func(m *model) { m.pane = p }})
	}
	views = append(views,
		view{name: "help", build: func(m *model) {
			m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
		}, marks: []string{"[esc]", "[↵]"}},
		view{name: "clear", build: func(m *model) {
			m.clearConfirm = &clearConfirm{target: eraseSession, id: "sess-1"}
		}, marks: []string{"[esc]", "[↵]", "[y]"}},
		view{name: "server picker", build: func(m *model) {
			m.pane = paneAgents
			m.serverPicker = &serverPicker{agent: "claude-code", entries: []serverChoice{
				{}, {name: "local", host: "http://127.0.0.1:11434", mapping: "opus → granite"}}}
		}, marks: []string{"[esc]", "[↵]"}},
		view{name: "column picker", build: func(m *model) {
			m.pane = paneEvents
			m.colPicker = true
		}, marks: []string{"[esc]", "[↵]", "[space]"}},
	)
	origDark := lipgloss.HasDarkBackground()
	t.Cleanup(func() { lipgloss.SetHasDarkBackground(origDark) })
	for _, dark := range []bool{true, false} {
		lipgloss.SetHasDarkBackground(dark)
		for _, w := range []int{80, 100, 140} {
			for _, v := range views {
				t.Run(fmt.Sprintf("dark=%v/%d/%s", dark, w, v.name), func(t *testing.T) {
					m := fitModel(t, paneSessions, w, 30, cursorRowsFixture(60))
					v.build(m)
					m.layout()
					hints := []int{-1}
					if v.marks != nil {
						found := hintLinesOf(m.View(), v.marks...)
						if len(found) == 0 {
							t.Fatalf("the %s view draws none of %q", v.name, v.marks)
						}
						hints = append(hints, found...)
					}
					// An empty state's key line chips its key (empty_state.go).
					hints = append(hints, hintLinesOf(m.View(), "Press [")...)
					// The Timeline's cursor strip is a hint line once T7 chips it.
					if v.name == fmt.Sprintf("pane %d", paneTimeline) {
						hints = append(hints, hintLinesOf(m.View(), "▸ [←→]")...)
					}
					assertPlainInvariant(t, m.View, hints...)
				})
			}
		}
	}
}

// TestFooter_FeedbackIsDim: the feedback link is the quietest thing on the status line.
func TestFooter_FeedbackIsDim(t *testing.T) {
	restore := lipgloss.ColorProfile()
	t.Cleanup(func() { lipgloss.SetColorProfile(restore) })
	lipgloss.SetColorProfile(termenv.TrueColor)
	m := fitModel(t, paneSessions, 200, 30, nil)
	m.layout()
	want := styleDim.Render("   feedback: " + feedbackURL)
	if got := m.footerView(); !strings.Contains(got, want) {
		t.Fatalf("footer does not draw the feedback link dim:\n%q\nwant it to hold\n%q", got, want)
	}
}
