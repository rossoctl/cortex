package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// statusRow returns the first line of the footer — the status row, where the
// feedback link lives. The second line is the keybinding hint.
func statusRow(footer string) string {
	return strings.Split(footer, "\n")[0]
}

// On a terminal wide enough to hold everything, the footer must carry a way to
// send feedback. A user looking at their session data is the user most likely
// to have an opinion about it; #975 asks that the tool point them somewhere
// rather than leaving them to hunt for the repository.
func TestFooterShowsFeedbackLinkWhenWide(t *testing.T) {
	if feedbackURL == "" {
		t.Fatal("feedbackURL const is empty; the footer has nothing to point users at")
	}

	m := &model{width: 200, pane: paneSessions}
	got := m.footerView()

	if !strings.Contains(got, feedbackURL) {
		t.Errorf("footerView() at width 200 does not contain the feedback URL %q\n---\n%s", feedbackURL, got)
	}
}

// The status row is the one footer line that nothing else bounds, and it feeds
// into an unbounded lipgloss.JoinVertical — so if it overflows the terminal the
// terminal wraps it onto a third row, breaking the "two-line footer" contract.
// The feedback link is the longest thing on it and has the weakest claim on the
// columns, so a narrow terminal must drop it rather than wrap. This is the test
// that the earlier presence-only check could not be: it measures the rendered
// width, the axis the bug lives on.
func TestFooterStatusRowFitsNarrowWidth(t *testing.T) {
	const width = 80

	cases := []struct {
		name  string
		model *model
	}{
		{"plain connected", &model{width: width, pane: paneSessions, connState: connStateInfo{phase: connOpen}}},
		{"paused", &model{width: width, pane: paneEvents, connState: connStateInfo{phase: connOpen}, paused: true}},
		// The timed (non-sticky) flash path also appends to the status row before
		// the feedback link, so it is the tightest case. flashUntil in the future
		// keeps the flash live.
		{"with timed flash", &model{width: width, pane: paneEvents, connState: connStateInfo{phase: connOpen}, flash: "yanked → ~/.cortex/agentop-events/event.json", flashUntil: time.Now().Add(time.Hour)}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := statusRow(tc.model.footerView())
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
			got := stripANSI(statusRow(m.footerView()))
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
		if got := stripANSI(statusRow(m.footerView())); !strings.Contains(got, "[/github-tool") {
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
