package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/rossoctl/cortex/core/session/timeline"
)

// The bottom bar feeds into an unbounded lipgloss.JoinVertical, so if it overflows the
// terminal the terminal wraps it onto a second row, and the view is one row too tall.
// This test measures the rendered width, the axis the bug lives on.
func TestFooterStatusRowFitsNarrowWidth(t *testing.T) {
	const width = 80

	cases := []struct {
		name  string
		model *model
	}{
		{"plain connected", &model{width: width, pane: paneSessions, connState: connStateInfo{phase: connOpen}}},
		{"paused", &model{width: width, pane: paneEvents, connState: connStateInfo{phase: connOpen}, paused: true}},
		// The timed (non-sticky) flash path also appends to the status, so it is
		// the tightest case. flashUntil in the future keeps the flash live.
		{"with timed flash", &model{width: width, pane: paneEvents, connState: connStateInfo{phase: connOpen}, flash: "yanked → ~/.cortex/agentop-events/event.json", flashUntil: time.Now().Add(time.Hour)}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := tc.model.footerView()
			if w := lipgloss.Width(row); w > width {
				t.Errorf("status row is %d columns at width %d (must not wrap):\n%q", w, width, row)
			}
		})
	}
}

// A non-chronological sort is state the operator chose, so the status row names it —
// the same reasoning as the search indicator. It matters most on a narrow terminal, where the
// sorted column may be one fitColumns dropped and there is then no header on screen
// carrying the glyph.
func TestFooterShowsActiveSort(t *testing.T) {
	// The indicator is styleWarn-rendered and CI has no TTY, so without forcing a
	// profile every assertion below runs against unstyled text and the styled path —
	// the one users see — is never exercised. stripANSI then does real work.
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(orig) })

	for _, tc := range []struct {
		name    string
		pane    paneID
		col     eventColumnID
		desc    bool
		want    string
		notWant string
	}{
		{name: "descending", pane: paneEvents, col: colDuration, desc: true, want: "[sort: DURATION" + sortGlyphDesc + "]"},
		{name: "ascending", pane: paneEvents, col: colDuration, desc: false, want: "[sort: DURATION" + sortGlyphAsc + "]"},
		{name: "cost", pane: paneEvents, col: colCost, desc: true, want: "[sort: COST" + sortGlyphDesc + "]"},
		{name: "chronological", pane: paneEvents, col: "", desc: false, notWant: "[sort:"},
		// sortCol is model-global and restored from settings, so it is set here exactly
		// as it would be after sorting the events table and pressing [u]. The Usage pane
		// is a chart in time order: naming an ordering it does not have is #1060.
		{name: "not on the usage pane", pane: paneUsage, col: colCost, desc: true, notWant: "[sort:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &model{
				pane: tc.pane, width: 200, eventColumns: defaultColumnSelection(),
				sortCol: tc.col, sortDesc: tc.desc,
			}
			got := stripANSI(m.footerView())
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Errorf("status row = %q, want it to contain %q", got, tc.want)
			}
			if tc.notWant != "" && strings.Contains(got, tc.notWant) {
				t.Errorf("status row = %q, want no %q with no sort active", got, tc.notWant)
			}
		})
	}
}

// TestFooterShowsSearchOnEveryPane: every pane searches what it shows, so a committed search
// is on every pane's status row. (It used to be on sessions and events only, because only
// those two had a search.) TestSearch_SlashOpensOnEveryPane is what says every pane has one.
func TestFooterShowsSearchOnEveryPane(t *testing.T) {
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(orig) })

	for p := paneID(0); p <= lastPaneID; p++ {
		m := &model{
			pane: p, width: 200, eventColumns: defaultColumnSelection(),
			search: map[paneID]string{p: "github-tool"},
		}
		if m.surface(p) == nil {
			continue
		}
		if got := stripANSI(m.footerView()); !strings.Contains(got, "[/github-tool") {
			t.Errorf("pane %v: status row = %q, want the search", p, got)
		}
	}
}

// Each [w] hint names the window it is on now, so the key reads the same on every pane.
func TestFooter_WShowsTheCurrentWindow(t *testing.T) {
	u := &model{pane: paneUsage}
	if got := u.helpView(); !strings.Contains(got, "[w] 10m") {
		t.Errorf("usage footer at the first window: %q, want [w] 10m", got)
	}
	u.usage.cycleWindow()
	if got := u.helpView(); !strings.Contains(got, "[w] 1h") {
		t.Errorf("usage footer after w: %q, want [w] 1h", got)
	}

	resetSettingsForTest(t)
	tl := timelineModel(t, 120, 40) // a 30-minute session
	tl.timeline.windowIdx = 1       // 1h: longer than the session, so "all" is drawn
	if got := tl.helpView(); !strings.Contains(got, "[w] all") {
		t.Errorf("timeline footer: %q, want [w] all", got)
	}
	// An ended session with a tool that never ended: the pane draws from tl.End, not the clock.
	tl.timeline.tl.Tools = append(tl.timeline.tl.Tools, timeline.Tool{Agent: timeline.MainAgent, ID: "open", Name: "Bash", Start: tlAt(1700)})
	_, _, _, win := tl.timelineRange()
	if want := "[w] " + timelineWindows[win].label + " "; !strings.Contains(tl.helpView(), want) {
		t.Errorf("timeline footer with an open tool: %q, want %q", tl.helpView(), want)
	}

	a := &model{pane: paneAgents}
	label := a.agentsWindowLabel()
	if label == "" {
		t.Fatal("agentsWindowLabel is empty before the first rows reply")
	}
	if got := a.helpView(); !strings.Contains(got, "[w] "+label) {
		t.Errorf("agents footer: %q, want [w] %s", got, label)
	}

	for _, m := range []*model{u, tl, a} {
		if strings.Contains(m.helpView(), "[w] window") {
			t.Errorf("pane %v still says [w] window", m.pane)
		}
	}
}
