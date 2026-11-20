package checklist

import (
	"bytes"
	"io"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// noForcedColour clears CLICOLOR_FORCE, which makes termenv colour a writer that
// is not a terminal, for a test that asserts what such a writer gets.
func noForcedColour(t *testing.T) { t.Setenv("CLICOLOR_FORCE", "") }

func TestPlainLines(t *testing.T) {
	noForcedColour(t)
	var b bytes.Buffer
	u := New(&b, false)
	u.Header("rosso cortex · setup", "dev · darwin/arm64")
	u.Consent([]Item{{"install", "agentop, cortex", "~/.local/bin"}, {"add", "~/.local/bin to PATH", "~/.zshrc"}})
	u.Done("installed", "agentop, cortex → ~/.local/bin", 3*time.Second)
	u.Already("config", "~/.cortex/config.yaml (kept)")
	u.Advise("PATH", "not on PATH", `export PATH="/h/.local/bin:$PATH"`)
	u.Fail("started", "never answered", "see ~/.cortex/proxy.log")
	u.Note("rolled back", "the changes above are undone")
	u.Next("Open a new terminal, then:", Step{"agentop configure claude-code enable", "connect your agent (or opencode, codex, bob, pi)"}, Step{"agentop observe", "watch your agent traffic live"})
	u.Close()
	want := strings.Join([]string{
		"  rosso cortex · setup   dev · darwin/arm64",
		"  This will:",
		"    install  agentop, cortex       ~/.local/bin",
		"    add      ~/.local/bin to PATH  ~/.zshrc",
		"  ✓ installed    agentop, cortex → ~/.local/bin",
		"  · config       ~/.cortex/config.yaml (kept)",
		"  ! PATH         not on PATH",
		`      fix: export PATH="/h/.local/bin:$PATH"`,
		"  ✗ started      never answered",
		"      see ~/.cortex/proxy.log",
		"    rolled back  the changes above are undone",
		"",
		"  Open a new terminal, then:",
		"",
		"    agentop configure claude-code enable        # connect your agent (or opencode, codex, bob, pi)",
		"    agentop observe" + strings.Repeat(" ", 21+8) + "# watch your agent traffic live",
		"",
	}, "\n")
	if b.String() != want {
		t.Errorf("plain output differs\n got: %q\nwant: %q", b.String(), want)
	}
}

func TestPlainModeHasNoControlBytes(t *testing.T) {
	noForcedColour(t)
	var b bytes.Buffer
	u := New(&b, false)
	r := u.Start("started")
	r.Wait("the proxy", 15*time.Second)
	r.Done("healthy")
	u.Close()
	if strings.ContainsAny(b.String(), "\r\x1b") {
		t.Errorf("plain output carries control bytes: %q", b.String())
	}
	if b.String() != "  ✓ started      healthy\n" {
		t.Errorf("got %q", b.String())
	}
}

func TestColourWhenTheProfileAllowsIt(t *testing.T) {
	var b bytes.Buffer
	r := lipgloss.NewRenderer(&b)
	r.SetColorProfile(termenv.ANSI256)
	u := newUI(&b, false, r)
	u.Done("installed", "x", 0)
	if !strings.Contains(b.String(), "\x1b[") {
		t.Fatalf("no colour under ANSI256: %q", b.String())
	}
	if got := ansi.Strip(b.String()); got != "  ✓ installed    x\n" {
		t.Errorf("text under the colour differs: %q", got)
	}
}

// A remedy's command keeps its normal weight, as Advise's fix does: only the lead
// is faint.
func TestARemedyLeavesItsCommandUnstyled(t *testing.T) {
	var b bytes.Buffer
	r := lipgloss.NewRenderer(&b)
	r.SetColorProfile(termenv.ANSI256)
	u := newUI(&b, false, r)
	u.Remedy("do it yourself: ", "agentop service restart")
	u.Advise("PATH", "not on PATH", "exec zsh")
	lines := strings.Split(b.String(), "\n")
	if len(lines) != 4 || !strings.Contains(lines[0], "\x1b[") {
		t.Fatalf("want 3 lines, the lead coloured under ANSI256: %q", b.String())
	}
	// The faint lead's reset sequence ends in m; a styled command would end in one too.
	for _, c := range []struct{ line, cmd string }{{lines[0], "agentop service restart"}, {lines[2], "exec zsh"}} {
		if !strings.HasSuffix(c.line, "m"+c.cmd) {
			t.Errorf("the command is styled, not left at normal weight after the faint lead: %q", c.line)
		}
	}
	if got := ansi.Strip(lines[0]); got != "      do it yourself: agentop service restart" {
		t.Errorf("remedy text = %q", got)
	}
}

func TestAnimatedRunningLineResolvesAndRestoresTheCursor(t *testing.T) {
	noForcedColour(t)
	var b bytes.Buffer
	u := New(&b, true)
	// Start reads the clock first; every later read is 250ms on.
	start := time.Unix(1000, 0)
	var reads atomic.Int32
	u.now = func() time.Time {
		if reads.Add(1) == 1 {
			return start
		}
		return start.Add(250 * time.Millisecond)
	}
	r := u.Start("started")
	r.Done("healthy")
	u.Close()
	out := b.String()
	// Every spinner frame starts with a clear too, so only a clear directly before
	// the resolved line proves end() cleared the last frame.
	for _, want := range []string{"\x1b[?25l", "\r\x1b[K  ✓ started      healthy", "\x1b[?25h"} {
		if !strings.Contains(out, want) {
			t.Errorf("animated output lacks %q: %q", want, out)
		}
	}
	if !regexp.MustCompile(`healthy +\d+\.\ds\n`).MatchString(ansi.Strip(out)) {
		t.Errorf("animated Done carries no timing: %q", out)
	}
}

// lockedBuffer lets a test read output a spinner goroutine may still be writing.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestCloseStopsARunningSpinnerAndClearsItsLine(t *testing.T) {
	var b lockedBuffer
	u := New(&b, true)
	r := u.Start("started")
	u.Close()
	out := b.String()
	if !strings.HasSuffix(out, "\r\x1b[K\x1b[?25h") {
		t.Errorf("Close did not clear the running line before showing the cursor: %q", out)
	}
	select {
	case <-r.stopped:
	default:
		t.Error("the spinner is still running after Close")
	}
}

func TestColourFollowsTheWriterNotTheDefaultRenderer(t *testing.T) {
	noForcedColour(t)
	prev := lipgloss.DefaultRenderer()
	t.Cleanup(func() { lipgloss.SetDefaultRenderer(prev) })
	d := lipgloss.NewRenderer(io.Discard)
	d.SetColorProfile(termenv.ANSI256)
	d.SetHasDarkBackground(true)
	lipgloss.SetDefaultRenderer(d)

	var b bytes.Buffer
	u := New(&b, false)
	u.Done("installed", "x", 0)
	u.Logo("ready")
	if strings.Contains(b.String(), "\x1b") {
		t.Errorf("colour came from the default renderer, not from w: %q", b.String())
	}
}

func TestEndingASecondTimeIsSafe(t *testing.T) {
	cases := []struct {
		name   string
		finish func(*UI, *Running)
	}{
		{"done then close", func(u *UI, r *Running) { r.Done("healthy"); u.Close() }},
		{"close twice", func(u *UI, r *Running) { u.Close(); u.Close() }},
		{"close then done", func(u *UI, r *Running) { u.Close(); r.Done("healthy") }},
		// Outside Close's contract, but a second end must still not double-close.
		{"close and done at once", func(u *UI, r *Running) {
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); u.Close() }()
			go func() { defer wg.Done(); r.Done("healthy") }()
			wg.Wait()
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var b lockedBuffer
			u := New(&b, true)
			r := u.Start("started")
			c.finish(u, r)
			out := b.String()
			if n := strings.Count(out, "\x1b[?25h"); n != 1 {
				t.Errorf("cursor shown %d times, want 1: %q", n, out)
			}
		})
	}
}

func TestWaitingShowsAfterThreeSeconds(t *testing.T) {
	var b bytes.Buffer
	u := New(&b, true)
	start := time.Unix(1000, 0)
	u.now = func() time.Time { return start }
	r := &Running{u: u, label: "started", start: start}
	r.Wait("the proxy to answer /healthz", 15*time.Second)
	u.now = func() time.Time { return start.Add(2 * time.Second) }
	r.draw("⠋")
	if strings.Contains(b.String(), "waiting") {
		t.Errorf("waiting text before 3s: %q", b.String())
	}
	u.now = func() time.Time { return start.Add(4 * time.Second) }
	r.draw("⠙")
	if !strings.Contains(ansi.Strip(b.String()), "waiting for the proxy to answer /healthz · 4s / 15s") {
		t.Errorf("waiting text missing at 4s: %q", b.String())
	}
}

// A running step's Advise draws what UI.Advise draws, its fixes below: on a
// terminal once the spinner's line is cleared, and with no timing, as UI.Advise
// shows none.
func TestRunningAdviseEndsAsUIAdviseDraws(t *testing.T) {
	noForcedColour(t)
	const want = "  ! PATH         edited; left as it is\n      fix: delete the two lines\n"
	var plain bytes.Buffer
	u := New(&plain, false)
	u.Start("PATH").Advise("edited; left as it is", "delete the two lines")
	u.Close()
	if plain.String() != want {
		t.Errorf("plain: got %q, want %q", plain.String(), want)
	}

	var b lockedBuffer
	a := New(&b, true)
	r := a.Start("PATH")
	r.Advise("edited; left as it is", "delete the two lines")
	a.Close()
	out := b.String()
	for _, w := range []string{"\x1b[?25l", "\r\x1b[K" + want, "\x1b[?25h"} {
		if !strings.Contains(out, w) {
			t.Errorf("animated output lacks %q: %q", w, out)
		}
	}
}

// The logo is the figlet "ANSI Shadow" CORTEX, row for row, each row indented 2
// spaces, then a blank line and what goes under it, indented 4.
func TestLogoIsTheANSIShadowCortex(t *testing.T) {
	noForcedColour(t)
	var b bytes.Buffer
	u := New(&b, false)
	u.Logo("v1.2.3 ready in 5.4s")
	want := strings.Join([]string{
		"   ██████╗ ██████╗ ██████╗ ████████╗███████╗██╗  ██╗",
		"  ██╔════╝██╔═══██╗██╔══██╗╚══██╔══╝██╔════╝╚██╗██╔╝",
		"  ██║     ██║   ██║██████╔╝   ██║   █████╗   ╚███╔╝",
		"  ██║     ██║   ██║██╔══██╗   ██║   ██╔══╝   ██╔██╗",
		"  ╚██████╗╚██████╔╝██║  ██║   ██║   ███████╗██╔╝ ██╗",
		"   ╚═════╝ ╚═════╝ ╚═╝  ╚═╝   ╚═╝   ╚══════╝╚═╝  ╚═╝",
		"",
		"    v1.2.3 ready in 5.4s",
		"",
	}, "\n")
	if b.String() != want {
		t.Errorf("logo differs\n got:\n%s\nwant:\n%s", b.String(), want)
	}
}

// Each row of the logo is in the accent where the profile allows colour; the line
// under it is not.
func TestLogoIsInTheAccent(t *testing.T) {
	var b bytes.Buffer
	r := lipgloss.NewRenderer(&b)
	r.SetColorProfile(termenv.ANSI256)
	u := newUI(&b, false, r)
	u.Logo("ready")
	lines := strings.Split(b.String(), "\n")
	if len(lines) != len(logo)+3 || !strings.Contains(lines[0], "\x1b[") {
		t.Fatalf("want %d lines, the first coloured under ANSI256: %q", len(logo)+3, b.String())
	}
	accent := r.NewStyle().Foreground(colorAccent)
	for i, row := range logo {
		if want := "  " + accent.Render(row); lines[i] != want {
			t.Errorf("row %d is not in the accent:\n got %q\nwant %q", i, lines[i], want)
		}
	}
	if lines[len(logo)+1] != "    ready" {
		t.Errorf("the line under the logo = %q, want it uncoloured", lines[len(logo)+1])
	}
}

// On a terminal a finished row's time ends at column 78, measured as the terminal
// shows it: ✓ and → take a column each, and colour takes none.
func TestATimingEndsAtColumn78(t *testing.T) {
	for _, profile := range []termenv.Profile{termenv.Ascii, termenv.ANSI256} {
		var b bytes.Buffer
		r := lipgloss.NewRenderer(&b)
		r.SetColorProfile(profile)
		u := newUI(&b, true, r)
		u.Done("installed", "agentop, cortex → ~/.local/bin", 3*time.Second)
		const left = "  ✓ installed    agentop, cortex → ~/.local/bin" // 47 columns
		want := left + strings.Repeat(" ", 78-47-4) + "3.0s\n"
		if got := ansi.Strip(b.String()); got != want {
			t.Errorf("profile %v: got %q, want %q", profile, got, want)
		}
	}
}

// A row too long for its time to end at column 78 keeps it 2 spaces after the detail.
func TestATimingPastColumn78KeepsTwoSpaces(t *testing.T) {
	noForcedColour(t)
	var b bytes.Buffer
	u := New(&b, true)
	detail := strings.Repeat("x", 70)
	u.Done("installed", detail, 3*time.Second)
	if got, want := b.String(), "  ✓ installed    "+detail+"  3.0s\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// On a terminal the header's version ends at column 78 too, or stays 3 spaces after
// a title too long for that. Off one it is 3 spaces after the title, as it always was.
func TestTheHeaderRightAlignsOnATerminal(t *testing.T) {
	noForcedColour(t)
	long := strings.Repeat("v", 60)
	for _, c := range []struct {
		animate     bool
		right, want string
	}{
		{true, "dev · darwin/arm64", "  rosso cortex · setup" + strings.Repeat(" ", 78-22-18) + "dev · darwin/arm64\n"},
		{true, long, "  rosso cortex · setup   " + long + "\n"},
		{false, "dev · darwin/arm64", "  rosso cortex · setup   dev · darwin/arm64\n"},
	} {
		var b bytes.Buffer
		New(&b, c.animate).Header("rosso cortex · setup", c.right)
		if b.String() != c.want {
			t.Errorf("animate=%v: got %q, want %q", c.animate, b.String(), c.want)
		}
	}
}

// On a terminal narrower than 79 columns the time ends at its last column but one,
// so the row does not wrap; a width nobody knows is 78.
func TestTheEdgeFollowsANarrowTerminal(t *testing.T) {
	noForcedColour(t)
	saved := terminalWidth
	t.Cleanup(func() { terminalWidth = saved })
	for _, c := range []struct{ cols, edge int }{{0, 78}, {79, 78}, {200, 78}, {78, 77}, {60, 59}} {
		terminalWidth = func(io.Writer) int { return c.cols }
		var b bytes.Buffer
		New(&b, true).Done("installed", "x", time.Second)
		if got := lipgloss.Width(strings.TrimSuffix(b.String(), "\n")); got != c.edge {
			t.Errorf("%d columns: the row is %d wide, want %d: %q", c.cols, got, c.edge, b.String())
		}
	}
}
