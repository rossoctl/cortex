package tui

import (
	"strings"
	"testing"
)

// The footer must always carry a way to send feedback. A user who is looking at
// their session data is the user most likely to have an opinion about it; #975
// asks that the tool point them somewhere quiet and non-intrusive rather than
// leaving them to hunt for the repository.
func TestFooterShowsFeedbackLink(t *testing.T) {
	if feedbackURL == "" {
		t.Fatal("feedbackURL const is empty; the footer has nothing to point users at")
	}

	// A wide terminal so nothing is trimmed for width.
	m := &model{width: 200, pane: paneSessions}
	got := m.footerView()

	if !strings.Contains(got, feedbackURL) {
		t.Errorf("footerView() does not contain the feedback URL %q\n---\n%s", feedbackURL, got)
	}
}

// The feedback line must survive a narrow terminal. It is the whole point of the
// change, so it may not be the first thing dropped when width is tight — a user
// on an 80-column terminal still needs it.
func TestFooterFeedbackLinkSurvivesNarrowWidth(t *testing.T) {
	m := &model{width: 80, pane: paneEvents}
	got := m.footerView()

	if !strings.Contains(got, feedbackURL) {
		t.Errorf("footerView() dropped the feedback URL at width 80\n---\n%s", got)
	}
}
