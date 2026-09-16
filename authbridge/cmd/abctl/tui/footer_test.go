package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
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
		{"with drops and paused", &model{width: width, pane: paneEvents, connState: connStateInfo{phase: connOpen}, drops: 12, paused: true}},
		// The timed (non-sticky) flash path also appends to the status row before
		// the feedback link, so it is the tightest case. flashUntil in the future
		// keeps the flash live.
		{"with timed flash", &model{width: width, pane: paneEvents, connState: connStateInfo{phase: connOpen}, flash: "yanked → ~/.cortex/abctl-events/event.json", flashUntil: time.Now().Add(time.Hour)}},
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
// the same reasoning as [filter: …]. It matters most on a narrow terminal, where the
// sorted column may be one fitColumns dropped and there is then no header on screen
// carrying the glyph.
func TestFooterShowsActiveSort(t *testing.T) {
	for _, tc := range []struct {
		name    string
		col     eventColumnID
		desc    bool
		want    string
		notWant string
	}{
		{name: "descending", col: colDuration, desc: true, want: "[sort: DURATION" + sortGlyphDesc + "]"},
		{name: "ascending", col: colDuration, desc: false, want: "[sort: DURATION" + sortGlyphAsc + "]"},
		{name: "cost", col: colCost, desc: true, want: "[sort: COST" + sortGlyphDesc + "]"},
		{name: "chronological", col: "", desc: false, notWant: "[sort:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &model{
				pane: paneEvents, width: 200, eventColumns: defaultColumnSelection(),
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
