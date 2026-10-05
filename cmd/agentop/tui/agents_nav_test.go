package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/core/cost/usage"
)

func navRows() []agentRow {
	return []agentRow{
		{label: "claude-code/2.1.270", Counts: usage.Counts{Requests: 10, PricedRequests: 10}},
		{label: "bob-shell/2.0.5", Counts: usage.Counts{Requests: 8}},
	}
}

// navModel is a connected model on the given pane. picker gives it a parent context, as the pods
// picker does; without one it is --endpoint mode.
func navModel(t *testing.T, pane paneID, picker bool) *model {
	t.Helper()
	m := &model{
		pane:               pane,
		previousPane:       paneNone,
		pipelineReturnPane: paneNone,
		agentsTbl:          newAgentsTable(),
		client:             deadClient(),
		agents:             navRows(),
	}
	if picker {
		m.parentCtx = context.Background()
		m.ctx, m.cancel = context.WithCancel(m.parentCtx)
		t.Cleanup(func() { m.cancel() })
	}
	return m
}

func pressA(t *testing.T, m *model, rows []agentRow) *model {
	t.Helper()
	cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'A'}})
	if cmd == nil {
		t.Fatalf("`A` on %v returned no fetch command", m.pane)
	}
	press, ok := cmd().(agentRowsLoadedMsg)
	if !ok {
		t.Fatal("`A`'s command did not produce an agentRowsLoadedMsg")
	}
	updated, _ := m.Update(agentRowsLoadedMsg{rows: rows, open: press.open, from: press.from})
	return updated.(*model)
}

func pressEsc(m *model) tea.Cmd { return m.handleKey(tea.KeyMsg{Type: tea.KeyEsc}) }

// #1231: an `A` press opens the pane however many agents there are.
func TestAgentsPane_APressOpensOnOneAgentAndOnNone(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []agentRow
		body string
	}{
		{"one agent", navRows()[:1], "claude-code/2.1.270"},
		{"no agents", nil, "no agent traffic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := navModel(t, paneSessions, false)
			m.width, m.height = 120, 40
			m.layout()
			m = pressA(t, m, tc.rows)
			if m.pane != paneAgents || m.flash != "" {
				t.Fatalf("`A` landed on %v with flash %q, want the pane and silence", m.pane, m.flash)
			}
			if view := m.paneView(); !strings.Contains(view, tc.body) {
				t.Errorf("pane body omits %q:\n%s", tc.body, view)
			}
		})
	}
}

// #1231: esc on a sessions list reached by picking an agent returns to the picker, on the agent
// picked, and refreshes its rows. From there esc backs out of the connection.
func TestSessionsPane_EscReturnsToThePickerItWasReachedThrough(t *testing.T) {
	for _, tc := range []struct {
		name    string
		picker  bool
		want    paneID
		escHint string
	}{
		{"pods picker mode", true, panePods, "[esc] pods"},
		{"--endpoint mode", false, paneAgents, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := navModel(t, paneEvents, tc.picker)
			m = pressA(t, m, navRows())
			m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
			m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
			m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
			if m.pane != paneSessions || m.agentScope != "bob-shell/2.0.5" {
				t.Fatalf("Enter: pane %v scope %q, want bob-shell's sessions", m.pane, m.agentScope)
			}
			if footer := m.helpView(); !strings.Contains(footer, "[esc] agents") {
				t.Errorf("sessions footer %q does not say esc returns to the picker", footer)
			}

			cmd := pressEsc(m)
			if m.pane != paneAgents || !m.agentsAboveSessions {
				t.Fatalf("esc from sessions: pane %v above %v, want the picker above Sessions",
					m.pane, m.agentsAboveSessions)
			}
			if got, _ := m.selectedAgentScope(); got != "bob-shell/2.0.5" {
				t.Errorf("cursor on %q, want the agent picked", got)
			}
			if cmd == nil {
				t.Fatal("esc into the picker did not refresh its rows")
			}
			if msg, ok := cmd().(agentRowsLoadedMsg); !ok || msg.open != agentsOpenNever {
				t.Errorf("refresh = %+v, want a background agentRowsLoadedMsg", msg)
			}
			footer := m.helpView()
			if tc.escHint != "" && !strings.Contains(footer, tc.escHint) {
				t.Errorf("picker footer %q omits %q", footer, tc.escHint)
			}
			if tc.escHint == "" && strings.Contains(footer, "[esc]") {
				t.Errorf("picker footer %q offers esc, which goes nowhere here", footer)
			}

			pressEsc(m)
			if m.pane != tc.want {
				t.Errorf("esc from the picker landed on %v, want %v", m.pane, tc.want)
			}
			if tc.picker && (m.sessionsViaAgents || m.agentsAboveSessions) {
				t.Errorf("backing out to pods kept via=%v above=%v for the next connection",
					m.sessionsViaAgents, m.agentsAboveSessions)
			}
		})
	}
}

// A sessions list the picker was never used for backs out to the pods picker, as it always has.
func TestSessionsPane_EscWithoutThePickerStillGoesToPods(t *testing.T) {
	m := navModel(t, paneSessions, true)
	if footer := m.helpView(); !strings.Contains(footer, "[esc] pods") {
		t.Errorf("sessions footer %q, want [esc] pods", footer)
	}
	pressEsc(m)
	if m.pane != panePods {
		t.Errorf("esc landed on %v, want the pods picker", m.pane)
	}
}

// `A` over a list reached through the picker is an overlay: esc returns to that list, and esc
// again reaches the picker as the level above it.
func TestAgentsPane_AOverAListReachedThroughThePickerIsAnOverlay(t *testing.T) {
	m := navModel(t, paneSessions, true)
	m.sessionsViaAgents = true
	m = pressA(t, m, navRows())
	if m.pane != paneAgents || m.agentsAboveSessions {
		t.Fatalf("`A`: pane %v above %v, want an overlay", m.pane, m.agentsAboveSessions)
	}
	if footer := m.helpView(); !strings.Contains(footer, "[esc] back") {
		t.Errorf("overlay footer %q, want [esc] back", footer)
	}
	pressEsc(m)
	if m.pane != paneSessions {
		t.Fatalf("esc from the overlay landed on %v, want sessions", m.pane)
	}
	pressEsc(m)
	if m.pane != paneAgents || !m.agentsAboveSessions {
		t.Errorf("esc from sessions: pane %v above %v, want the picker above Sessions",
			m.pane, m.agentsAboveSessions)
	}
}

// `A` on the picker above Sessions refreshes it without turning it into an overlay, and so does
// the catalog's round trip: esc still backs out of the connection rather than landing on Sessions.
func TestAgentsPane_ThePickerAboveSessionsSurvivesARefreshAndTheCatalog(t *testing.T) {
	m := navModel(t, paneSessions, true)
	m.enterAgentsAtStartup()
	m = pressA(t, m, navRows())
	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'C'}})
	if m.pane != paneCatalog {
		t.Fatalf("`C` landed on %v", m.pane)
	}
	pressEsc(m)
	if m.pane != paneAgents {
		t.Fatalf("esc from the catalog landed on %v, want the picker", m.pane)
	}
	pressEsc(m)
	if m.pane != panePods {
		t.Errorf("esc from the picker landed on %v, want the pods picker", m.pane)
	}
}
