package tui

import (
	"io"
	"os"
	"os/exec"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// wantCursorKeys reports whether agentop is running in Terminal.app, the terminal
// application cursor-key mode was added for. The mode is not free: terminals that
// follow xterm (the xterm-256color terminfo says khome=\EOH, kend=\EOF) then send
// Home and End as ESC O H and ESC O F, and bubbletea v1.3.10 cannot read either —
// it splits each into alt+O and a bare letter, so the tables' home/end bindings
// stop working and Home in the search prompt types an H. Terminal.app keeps Home and
// End for scrolling its own window by default, so it pays nothing for the mode.
// GNOME Terminal, Konsole, Alacritty and kitty are believed to turn an alt-screen
// scroll into arrows without it (not verified here), so elsewhere the mode would
// likely cost those keys and buy nothing.
//
// TERM_PROGRAM rather than GOOS, because the cost belongs to the terminal: iTerm2
// runs on the same Macs and is not known to share Terminal.app's Home/End default.
// tmux sets its own TERM_PROGRAM, and the scroll is tmux's to handle there anyway.
func wantCursorKeys() bool {
	return os.Getenv("TERM_PROGRAM") == "Apple_Terminal"
}

// openEditorCmd returns a tea.Cmd that suspends bubbletea, runs $EDITOR
// (vi if unset) on path, and emits editorExitedMsg when the editor exits.
// gen is captured at call time so the handler can detect a stale exit
// from an aborted-then-restarted edit.
func openEditorCmd(gen int, path string) tea.Cmd {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	c := exec.Command("sh", "-c", editor+" "+path)
	exited := func(err error) tea.Msg {
		return editorExitedMsg{gen: gen, err: err}
	}
	if wantCursorKeys() {
		return tea.Exec(cursorKeysCmd{autowrapCmd{c}}, exited)
	}
	return tea.Exec(autowrapCmd{c}, exited)
}

// autowrapCmd lends the terminal to a command with autowrap on, and turns it off again
// once the command exits. Run turns it off for agentop's own rows (see there), but an
// editor drawing its own screen expects the terminal's default, and bubbletea's
// ReleaseTerminal and RestoreTerminal do not know about the mode.
//
// Written to the command's own stdout, before its output and after it, for the reason
// cursorKeysCmd writes there: bubbletea has released the terminal while Run is called,
// so the writes cannot land inside a frame.
type autowrapCmd struct{ *exec.Cmd }

// The setters leave an already-set stream alone, as tea.ExecProcess's own wrapper does.
func (c autowrapCmd) SetStdin(r io.Reader) {
	if c.Stdin == nil {
		c.Stdin = r
	}
}

func (c autowrapCmd) SetStdout(w io.Writer) {
	if c.Stdout == nil {
		c.Stdout = w
	}
}

func (c autowrapCmd) SetStderr(w io.Writer) {
	if c.Stderr == nil {
		c.Stderr = w
	}
}

func (c autowrapCmd) Run() error {
	if c.Stdout != nil {
		_, _ = io.WriteString(c.Stdout, ansi.SetModeAutoWrap)
	}
	err := c.Cmd.Run()
	if c.Stdout != nil {
		_, _ = io.WriteString(c.Stdout, ansi.ResetModeAutoWrap)
	}
	return err
}

// cursorKeysCmd runs a command that borrows the terminal and sets application
// cursor-key mode again once it exits, because an editor resets the mode on its
// way out (vi's rmkx does) and bubbletea's RestoreTerminal does not know to set it.
// Without this, one `e` would leave the trackpad scrolling the terminal's
// scrollback for the rest of the session. Run is called while bubbletea has
// released the terminal, so the write cannot interleave with a frame.
//
// It wraps an autowrapCmd because every command agentop lends the terminal to needs
// that one, and takes its stream setters from it.
type cursorKeysCmd struct{ autowrapCmd }

func (c cursorKeysCmd) Run() error {
	err := c.autowrapCmd.Run()
	if c.Stdout != nil {
		_, _ = io.WriteString(c.Stdout, ansi.SetModeCursorKeys)
	}
	return err
}
