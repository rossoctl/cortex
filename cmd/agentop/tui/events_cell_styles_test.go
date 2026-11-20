package tui

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session/timeline"
)

// fgSGR is the SGR parameter text that sets c as the foreground under the current
// profile and theme.
func fgSGR(c lipgloss.AdaptiveColor) string {
	hex := c.Light
	if lipgloss.HasDarkBackground() {
		hex = c.Dark
	}
	return lipgloss.ColorProfile().Color(hex).Sequence(false)
}

// inkBefore is the SGR sequence directly in front of the first text in view, after the
// cell's alignment spaces. It fails the test when text is missing or no SGR is there.
func inkBefore(t *testing.T, view, text string) string {
	t.Helper()
	i := strings.Index(view, text)
	if i < 0 {
		t.Fatalf("%q is not in the view:\n%s", text, ansi.Strip(view))
	}
	before := strings.TrimRight(view[:i], " ")
	j := strings.LastIndex(before, "\x1b[")
	if j < 0 || !strings.HasSuffix(before, "m") || strings.Contains(before[j:], " ") {
		t.Fatalf("no SGR directly before %q in %q", text, before[max(0, len(before)-40):])
	}
	return before[j:]
}

func assertInk(t *testing.T, view, text string, c lipgloss.AdaptiveColor, name string) {
	t.Helper()
	if sgr := inkBefore(t, view, text); !strings.Contains(sgr, fgSGR(c)) {
		t.Errorf("%q: SGR %q has no %s ink (%s)", text, sgr, name, fgSGR(c))
	}
}

// cellStylesEvents is a modify exchange answered 200, an exchange answered 503 and an
// unbridged CONNECT, so the ACTION and STATUS cells hold one value of each group.
func cellStylesEvents() []pipeline.SessionEvent {
	ok := trExchange(1, 10, "")
	ok[0].Invocations = &pipeline.Invocations{Outbound: []pipeline.Invocation{
		{Plugin: "token-exchange", Action: pipeline.ActionModify},
	}}
	failed := trExchange(2, 20, "")
	failed[1].StatusCode = 503
	tunnel := pipeline.SessionEvent{At: tlAt(30), Seq: 5, Direction: pipeline.Outbound,
		Phase: pipeline.SessionRequest, Host: "tun.test:443", Tunnel: true, RequestID: "rt"}
	return slices.Concat(ok, failed, []pipeline.SessionEvent{tunnel})
}

// Column styles follow the column id: with DIR and PLUGIN hidden and STATUS sorted (its
// header reads STATUS▼), every cell still takes its own column's ink.
func TestEventsCellStyles_ByColumnID(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	m := fitModel(t, paneEvents, 200, 40, cellStylesEvents())
	m.eventColumns[colDir], m.eventColumns[colPlugin] = false, false
	m.sortCol, m.sortDesc = colStatus, true
	m.rebuildEventsTable()
	view := m.eventsTbl.View()
	if !strings.Contains(view, "STATUS▼") {
		t.Fatalf("no sorted STATUS header in:\n%s", ansi.Strip(view))
	}
	assertInk(t, view, "503", colorError, "error")
	assertInk(t, view, "200", colorOK, "ok")
	assertInk(t, view, "modify", colorAccent, "accent")
	assertInk(t, view, "tunnel", colorDim, "dim")
	for _, line := range strings.Split(ansi.Strip(view), "\n") {
		if slices.Contains(strings.Fields(line), "out") {
			t.Errorf("DIR is hidden, but a row shows %q: %q", "out", line)
		}
	}
}

func TestEventsCellStyles_ToolRows(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	events := slices.Concat(trExchange(1, 10, ""), trExchange(2, 30, ""))
	tl := &timeline.Timeline{Tools: []timeline.Tool{trTool(2, "t1", "Grep", 98.8, -1, "")}}
	m := toolRowsModel(t, events, tl)
	m.now = func() time.Time { return tlAt(100) }
	m.rebuildEventsTable()
	_, row := toolRowAt(t, m, "t1")
	dur := toolCell(m, row, colDuration)
	if !strings.HasPrefix(dur, "⋯") {
		t.Fatalf("DURATION = %q, want a running tool's ⋯ cell", dur)
	}
	view := m.eventsTbl.View()
	assertInk(t, view, "┗⚒", colorTool, "tool")
	assertInk(t, view, "Grep", colorTool, "tool")
	assertInk(t, view, dur, colorTool, "tool")
	assertInk(t, view, "running", colorWarn, "warn")
}

func TestEventsCellStyles_BlockRowAccent(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	m := newBlockModel(t, blockEvents())
	view := m.eventsTbl.View()
	assertInk(t, view, "▸─", colorAccent, "accent")
}

func TestEventsCellStyles_PlainInvariant(t *testing.T) {
	for _, w := range []int{80, 100, 140} {
		events := slices.Concat(cellStylesEvents(), trExchange(3, 40, ""))
		tl := &timeline.Timeline{Tools: []timeline.Tool{trTool(6, "t1", "Bash", 42, 43, "ls")}}
		m := toolRowsModel(t, events, tl)
		m.width = w
		assertPlainInvariant(t, func() string {
			m.rebuildEventsTable()
			return m.eventsTbl.View()
		})
	}
}
