// Package checklist draws agentop setup's screen: a header, the consent list, one
// line per step that resolves to ✓, ·, ! or ✗, and the ending. On a terminal the
// running step animates and finished steps show how long they took, right-aligned;
// anywhere else the same text prints once per finished step with no colour, no
// carriage returns and no timings, which is what logs and tests see. Every text
// argument is one line: the screen indents by prefixing, so a second line would lose
// the indent.
package checklist

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/term"
)

const labelWidth = 12

// rightEdge is the column right-aligned text ends at on a terminal: a timing, and the
// header's version. install.sh right-aligns its title to the same column.
const rightEdge = 78

// One accent, rosso red, for the brand and for motion only. The status colours
// match the TUI's palette (tui/styles.go), which this package cannot import.
var (
	colorAccent = lipgloss.AdaptiveColor{Light: "#B42318", Dark: "#F97066"}
	colorOK     = lipgloss.AdaptiveColor{Light: "#047857", Dark: "#6EE7B7"}
	colorWarn   = lipgloss.AdaptiveColor{Light: "#92400E", Dark: "#FCD34D"}
	colorBad    = lipgloss.AdaptiveColor{Light: "#B91C1C", Dark: "#FCA5A5"}
	colorFaint  = lipgloss.AdaptiveColor{Light: "#6B7280", Dark: "#9CA3AF"}
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// logo is the figlet "ANSI Shadow" CORTEX: 6 rows, 50 columns, trailing spaces trimmed.
var logo = [6]string{
	" ██████╗ ██████╗ ██████╗ ████████╗███████╗██╗  ██╗",
	"██╔════╝██╔═══██╗██╔══██╗╚══██╔══╝██╔════╝╚██╗██╔╝",
	"██║     ██║   ██║██████╔╝   ██║   █████╗   ╚███╔╝",
	"██║     ██║   ██║██╔══██╗   ██║   ██╔══╝   ██╔██╗",
	"╚██████╗╚██████╔╝██║  ██║   ██║   ███████╗██╔╝ ██╗",
	" ╚═════╝ ╚═════╝ ╚═╝  ╚═╝   ╚═╝   ╚══════╝╚═╝  ╚═╝",
}

// Item is one row of the consent screen.
type Item struct{ Verb, What, Where string }

// UI writes the screen to one writer.
type UI struct {
	w       io.Writer
	animate bool
	now     func() time.Time
	edge    int // the column right-aligned text ends at

	accent, ok, warn, bad, faint lipgloss.Style

	mu           sync.Mutex
	cursorHidden bool
	running      *Running
}

// terminalWidth is how many columns w is wide, or 0 when it is not a terminal or will
// not say. A var so tests can give a writer a width.
var terminalWidth = func(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok {
		return 0
	}
	cols, _, err := term.GetSize(f.Fd())
	if err != nil {
		return 0
	}
	return cols
}

// New binds the styles to w, so colour follows w: none when w is not a terminal
// or NO_COLOR is set. animate turns on the spinner, timings and cursor handling,
// and should be true only when w is a terminal.
func New(w io.Writer, animate bool) *UI {
	u := newUI(w, animate, lipgloss.NewRenderer(w))
	u.edge = edgeFor(terminalWidth(w))
	return u
}

// edgeFor is the column right-aligned text ends at on a terminal cols wide: rightEdge,
// or the last column but one of a narrower terminal, as install.sh's title has it, so
// that a padded line does not wrap. cols 0 is a width nobody knows.
func edgeFor(cols int) int {
	if cols > 1 && cols-1 < rightEdge {
		return cols - 1
	}
	return rightEdge
}

func newUI(w io.Writer, animate bool, r *lipgloss.Renderer) *UI {
	return &UI{
		w: w, animate: animate, now: time.Now, edge: rightEdge,
		accent: r.NewStyle().Foreground(colorAccent),
		ok:     r.NewStyle().Foreground(colorOK),
		warn:   r.NewStyle().Foreground(colorWarn),
		bad:    r.NewStyle().Foreground(colorBad),
		faint:  r.NewStyle().Foreground(colorFaint),
	}
}

// Animated reports whether this UI draws motion and timings.
func (u *UI) Animated() bool { return u.animate }

func (u *UI) println(s string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.write(s + "\n")
}

// write is every write to the screen; the caller holds u.mu. A write that fails
// has nowhere better to be reported than the screen it failed on, so it is dropped.
func (u *UI) write(s string) { _, _ = io.WriteString(u.w, s) }

func pad(s string, n int) string {
	if w := lipgloss.Width(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

func (u *UI) mark(style lipgloss.Style, glyph, label, detail string) string {
	return "  " + style.Render(glyph) + " " + pad(label, labelWidth) + " " + detail
}

// took is how long a step took, as a finished row shows it: on a terminal only.
func (u *UI) took(d time.Duration) string {
	if !u.animate || d <= 0 {
		return ""
	}
	return u.faint.Render(FormatDuration(d))
}

// alignRight puts right after left so that right ends at the edge, or gap spaces after
// left when left is too long for that. Widths are what the terminal shows, so colour
// and a multi-byte glyph each count as what they take on screen.
func (u *UI) alignRight(left, right string, gap int) string {
	return left + strings.Repeat(" ", max(gap, u.edge-lipgloss.Width(left)-lipgloss.Width(right))) + right
}

// FormatDuration is the one way a duration is shown: tenths of a second.
func FormatDuration(d time.Duration) string { return fmt.Sprintf("%.1fs", d.Seconds()) }

// Header is the screen's first line: the title, and right, faint, 3 spaces after it;
// on a terminal, right-aligned to the edge, with those 3 spaces at the least.
func (u *UI) Header(title, right string) {
	left, meta := "  "+title, u.faint.Render(right)
	if !u.animate {
		u.println(left + "   " + meta)
		return
	}
	u.println(u.alignRight(left, meta, 3))
}

func (u *UI) Blank()            { u.println("") }
func (u *UI) Plain(text string) { u.println("  " + text) }
func (u *UI) Faint(text string) { u.println("  " + u.faint.Render(text)) }

// Consent lists what setup will change, one aligned row per change.
func (u *UI) Consent(items []Item) {
	verbW, whatW := 0, 0
	for _, it := range items {
		verbW = max(verbW, lipgloss.Width(it.Verb))
		whatW = max(whatW, lipgloss.Width(it.What))
	}
	u.println("  This will:")
	for _, it := range items {
		u.println("    " + pad(it.Verb, verbW+2) + pad(it.What, whatW+2) + u.faint.Render(it.Where))
	}
}

// Done marks a step that finished. On a terminal the time it took ends at the edge,
// or 2 spaces after a detail too long for that.
func (u *UI) Done(label, detail string, took time.Duration) {
	line := u.mark(u.ok, "✓", label, detail)
	if t := u.took(took); t != "" {
		line = u.alignRight(line, t, 2)
	}
	u.println(line)
}

func (u *UI) Already(label, detail string) {
	u.println(u.faint.Render("  · " + pad(label, labelWidth) + " " + detail))
}

// Advise marks a step that finished but wants the user to act. Each fix is one
// line, printed under the mark; a second line in one would lose the indent.
func (u *UI) Advise(label, reason string, fix ...string) {
	u.println(u.mark(u.warn, "!", label, reason))
	for _, f := range fix {
		u.Remedy("fix: ", f)
	}
}

// Remedy is a command for the user to run, at a mark's detail indent: lead, such
// as "fix: ", is faint and the command keeps its normal weight, to be read and typed.
func (u *UI) Remedy(lead, cmd string) { u.println("      " + u.faint.Render(lead) + cmd) }

// Fail marks a step that failed. Each detail is one line, printed under the mark:
// lipgloss pads a multi-line string to its widest line, and only the first line
// gets the indent.
func (u *UI) Fail(label, reason string, detail ...string) {
	u.println(u.mark(u.bad, "✗", label, reason))
	for _, d := range detail {
		u.println("      " + u.faint.Render(d))
	}
}

// Note is an indented, unmarked line — what a rollback did, for instance.
func (u *UI) Note(label, text string) {
	u.println("    " + u.faint.Render(pad(label, labelWidth)) + " " + text)
}

// Logo ends a run that worked: the logo in the accent, a blank line, then below,
// indented under it. The blank line above it is the caller's.
func (u *UI) Logo(below string) {
	for _, row := range logo {
		u.println("  " + u.accent.Render(row))
	}
	u.println("")
	u.println("    " + below)
}

// Step is one command for Next to print, with its comment.
type Step struct{ Cmd, Comment string }

// Next ends the screen on the commands to type: intro, then each step as
// "    <Cmd padded to the longest Cmd>        # <Comment>". Commands are ASCII, so
// len is their width.
func (u *UI) Next(intro string, steps ...Step) {
	w := 0
	for _, s := range steps {
		w = max(w, len(s.Cmd))
	}
	u.println("")
	u.println("  " + intro)
	u.println("")
	for _, s := range steps {
		u.println("    " + s.Cmd + strings.Repeat(" ", w-len(s.Cmd)+8) + u.faint.Render("# "+s.Comment))
	}
}

// Running is a step in progress. Off a terminal it prints nothing until it ends.
type Running struct {
	u     *UI
	label string
	start time.Time

	mu      sync.Mutex
	waiting string
	limit   time.Duration

	stop, stopped chan struct{}
	ended         sync.Once
}

func (u *UI) Start(label string) *Running {
	r := &Running{u: u, label: label, start: u.now()}
	if !u.animate {
		return r
	}
	// The channels exist before u.running can hand r to Close.
	r.stop, r.stopped = make(chan struct{}), make(chan struct{})
	u.mu.Lock()
	if !u.cursorHidden {
		u.write("\x1b[?25l")
		u.cursorHidden = true
	}
	u.running = r
	u.mu.Unlock()
	go r.spin()
	return r
}

// Wait names what the step is blocked on; after 3s the line says so, with the limit.
func (r *Running) Wait(what string, limit time.Duration) {
	r.mu.Lock()
	r.waiting, r.limit = what, limit
	r.mu.Unlock()
}

func (r *Running) spin() {
	defer close(r.stopped)
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for i := 0; ; i++ {
		r.draw(spinnerFrames[i%len(spinnerFrames)])
		select {
		case <-r.stop:
			return
		case <-t.C:
		}
	}
}

func (r *Running) draw(frame string) {
	elapsed := r.u.now().Sub(r.start)
	r.mu.Lock()
	waiting, limit := r.waiting, r.limit
	r.mu.Unlock()
	detail := ""
	if waiting != "" && elapsed >= 3*time.Second {
		detail = r.u.faint.Render(fmt.Sprintf("waiting for %s · %ds / %ds",
			waiting, int(elapsed.Seconds()), int(limit.Seconds())))
	}
	r.u.mu.Lock()
	defer r.u.mu.Unlock()
	r.u.write("\r\x1b[K  " + r.u.accent.Render(frame) + " " + pad(r.label, labelWidth) + " " + detail)
}

// end stops the spinner and clears its line. It runs its body once, so a second
// end (Done after Close, say) can neither close the channel twice nor clear twice.
func (r *Running) end() time.Duration {
	r.ended.Do(func() {
		if r.stop == nil {
			return
		}
		close(r.stop)
		<-r.stopped
		r.u.mu.Lock()
		r.u.write("\r\x1b[K")
		r.u.running = nil
		r.u.mu.Unlock()
	})
	return r.u.now().Sub(r.start)
}

func (r *Running) Done(detail string) { took := r.end(); r.u.Done(r.label, detail, took) }

func (r *Running) Fail(reason string, detail ...string) {
	r.end()
	r.u.Fail(r.label, reason, detail...)
}

// Advise ends the step as one that finished but wants the user to act, drawn as
// UI.Advise draws it.
func (r *Running) Advise(reason string, fix ...string) {
	r.end()
	r.u.Advise(r.label, reason, fix...)
}

// Close stops a spinner still running and gives the cursor back. Call it on
// every exit path, from the goroutine that drives the steps.
func (u *UI) Close() {
	u.mu.Lock()
	r := u.running
	u.mu.Unlock()
	if r != nil {
		r.end()
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.cursorHidden {
		u.write("\x1b[?25h")
		u.cursorHidden = false
	}
}
