package tui

import (
	"bytes"
	"os/exec"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// clearsScreen reports whether running cmd yields bubbletea's clear-screen message,
// directly or inside a batch. The message type is unexported, so it is recognised by
// comparison with what tea.ClearScreen itself returns.
//
// Only the top level of a batch is run, and nothing else in it is: a batch's other
// commands are the model's own (fetches, ticks), and running them here would make a
// test of the repaint depend on what they do.
func clearsScreen(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	want := tea.ClearScreen()
	msg := cmd()
	if msg == want {
		return true
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			if c != nil && c() == want {
				return true
			}
		}
	}
	return false
}

// A message that changes panes repaints the whole screen, so a row that drifted out of
// step with bubbletea's record of it cannot survive the change. The divider under the
// spend band is identical on every pane, so bubbletea never redraws it on its own;
// without this, text from the detail pane stayed on it after leaving (#1461).
//
// Driven through Update, not handleKey, because the repaint is decided around the
// dispatch: a test of the handler would pass with the wrapper removed.
func TestUpdate_PaneChangeRepaintsScreen(t *testing.T) {
	m := &model{
		pane:               paneSessions,
		client:             deadClient(),
		previousPane:       paneNone,
		pipelineReturnPane: paneNone,
	}
	_, cmd := m.Update(keyRune('P'))
	if m.pane != panePipeline {
		t.Fatalf("`P` from the sessions pane opened pane %v, want panePipeline; the fixture no longer changes panes", m.pane)
	}
	if !clearsScreen(cmd) {
		t.Error("changing panes did not clear the screen; a row bubbletea skips as unchanged keeps whatever drifted onto it")
	}
}

// A message that leaves the pane alone does not repaint. A clear costs a frame, and the
// stream and the tickers deliver several messages a second, so repainting on every one
// would flicker the whole screen continuously.
func TestUpdate_SamePaneDoesNotRepaint(t *testing.T) {
	m := &model{pane: paneSessions, client: deadClient()}
	_, cmd := m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	if m.pane != paneSessions {
		t.Fatalf("a resize changed the pane to %v; the fixture no longer stays put", m.pane)
	}
	if clearsScreen(cmd) {
		t.Error("a message that did not change panes cleared the screen")
	}
}

// An editor borrowing the terminal gets autowrap back, since it draws its own screen
// and expects the terminal's default, and agentop gets it switched off again after.
// The order is what matters: the mode set after the command's output would leave the
// editor running unwrapped, and a test that only looked for both sequences would pass.
func TestAutowrapCmd_RestoresWrapForTheCommandOnly(t *testing.T) {
	var out bytes.Buffer
	var c tea.ExecCommand = autowrapCmd{exec.Command("sh", "-c", "printf editor")}
	c.SetStdout(&out)
	if err := c.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := out.String(), ansi.SetModeAutoWrap+"editor"+ansi.ResetModeAutoWrap; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

// A failed editor still had the terminal, so agentop's mode is restored on that path
// too, and the command's own error is what comes back.
func TestAutowrapCmd_RestoresModeWhenCommandFails(t *testing.T) {
	var out bytes.Buffer
	c := autowrapCmd{exec.Command("sh", "-c", "exit 3")}
	c.SetStdout(&out)
	if err := c.Run(); err == nil {
		t.Fatal("Run: want the command's exit error, got nil")
	}
	if got, want := out.String(), ansi.SetModeAutoWrap+ansi.ResetModeAutoWrap; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}
