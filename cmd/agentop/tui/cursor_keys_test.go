package tui

import (
	"bytes"
	"os/exec"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// An editor resets application cursor-key mode as it exits, so the mode has to be
// set again after the command's own output, not before it. This is the order that
// keeps trackpad scrolling working after `e`; reversing it would pass a test that
// only looked for the sequence.
func TestCursorKeysCmdRestoresModeAfterCommand(t *testing.T) {
	var out bytes.Buffer
	var c tea.ExecCommand = cursorKeysCmd{exec.Command("sh", "-c", `printf 'editor\033[?1l'`)}
	c.SetStdout(&out)
	if err := c.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := out.String(), "editor"+ansi.ResetModeCursorKeys+ansi.SetModeCursorKeys; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

// A failed editor still borrowed the terminal and may have reset the mode, and
// bubbletea restores the terminal on that path too.
func TestCursorKeysCmdRestoresModeWhenCommandFails(t *testing.T) {
	var out bytes.Buffer
	c := cursorKeysCmd{exec.Command("sh", "-c", "exit 3")}
	c.SetStdout(&out)
	if err := c.Run(); err == nil {
		t.Fatal("Run: want the command's exit error, got nil")
	}
	if got := out.String(); got != ansi.SetModeCursorKeys {
		t.Fatalf("output = %q, want %q", got, ansi.SetModeCursorKeys)
	}
}
