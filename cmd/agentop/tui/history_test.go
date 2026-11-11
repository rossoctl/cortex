package tui

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/session/archive"
	"github.com/rossoctl/cortex/core/sessionapi"
)

// Every session-list fetch asks for history. A proxy with a session archive answers with the
// sessions only its disk holds as well; one without ignores the parameter. There is no key for it:
// a restart must not take a session off the list.
func TestLoadSessions_AlwaysAsksForHistory(t *testing.T) {
	var path, query string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, query = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"sessions":[{"id":"old","resident":false,"eventCount":3}]}`)
	}))
	defer ts.Close()
	m := &model{ctx: context.Background(), client: apiclient.New(ts.URL)}
	msg, ok := m.loadSessionsCmd()().(sessionsLoadedMsg)
	if !ok {
		t.Fatalf("loadSessionsCmd returned %T, want sessionsLoadedMsg", msg)
	}
	if path != "/v1/sessions" || query != "archived=true" {
		t.Fatalf("requested %s?%s, want /v1/sessions?archived=true", path, query)
	}
	if len(msg) != 1 || msg[0].ID != "old" {
		t.Fatalf("rows = %+v", msg)
	}
}

// A row only the archive holds is an ordinary row: UPDATED says when it was last active, as for any
// other, rather than a marker standing in for the time.
func TestSessionsPane_ADiskOnlyRowShowsWhenItWasUpdated(t *testing.T) {
	m := newTitleModel(t, nil)
	no := false
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	m.Update(sessionsLoadedMsg{
		{ID: "live-1", UpdatedAt: now},
		{ID: "old-2", UpdatedAt: old, Resident: &no, EventCount: 7},
	})
	if got := strings.Join(m.sessionRowIDs, ","); got != "live-1,old-2" {
		t.Fatalf("rows %s, want live-1,old-2", got)
	}
	if want := relTime(m.clock(), old); !rowHas(m.sessionsTbl.Rows()[1], want) {
		t.Fatalf("disk-only row %v does not show %q", m.sessionsTbl.Rows()[1], want)
	}
}

func rowHas(row []string, cell string) bool {
	for _, c := range row {
		if strings.TrimSpace(c) == cell {
			return true
		}
	}
	return false
}

// The whole path against a real session API and archive: history from a previous process is
// listed on the first fetch, with no key pressed, and Enter on it opens its timeline through the
// same snapshot a live session uses, since the server pages into the archive for it.
func TestHistory_AnArchivedSessionIsListedAndOpens(t *testing.T) {
	resetSettingsForTest(t) // New seeds the filter from Settings; another test's must not hide the row
	root := t.TempDir()
	prev, err := archive.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	prevStore := session.New(0, 0, 0)
	prevStore.AddRecorder(prev)
	for range 4 {
		prevStore.Append("old", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest, Host: "api.example.com"})
	}
	prevStore.Close()
	prev.Close()

	a, err := archive.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	store.AddRecorder(a)
	ts := httptest.NewServer(sessionapi.New(":0", store, sessionapi.WithArchive(a)).Server().Handler)
	defer ts.Close()

	m := New(context.Background(), apiclient.New(ts.URL)).(*model)
	m.width, m.height = 120, 40
	m.pane = paneSessions
	m.Update(m.loadSessionsCmd()())
	idx := slices.Index(m.sessionRowIDs, "old")
	if idx < 0 {
		t.Fatalf("the archived session is not listed: %v", m.sessionRowIDs)
	}
	m.sessionsTbl.SetCursor(idx)
	cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter on an archived row fetched nothing")
	}
	msg := cmd()
	if b, ok := msg.(tea.BatchMsg); ok {
		msg = b[0]() // the snapshot; the timeline fetch for the tool rows comes after it
	}
	m.Update(msg)
	if n := len(m.events["old"]); n != 4 {
		t.Fatalf("the archived timeline holds %d events, want 4", n)
	}
}

// A proxy without an archive — any cluster sidecar — gets the same request, ignores the parameter
// and lists what it holds, and agentop says nothing about history it was never offered.
func TestHistory_AProxyWithoutAnArchiveListsItsSessionsQuietly(t *testing.T) {
	resetSettingsForTest(t)
	store := session.New(0, 0, 0)
	defer store.Close()
	store.Append("live", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest})
	ts := httptest.NewServer(sessionapi.New(":0", store).Server().Handler)
	defer ts.Close()

	m := New(context.Background(), apiclient.New(ts.URL)).(*model)
	m.width, m.height = 120, 40
	m.pane = paneSessions
	m.Update(m.loadSessionsCmd()())
	if !slices.Contains(m.sessionRowIDs, "live") || m.flash != "" {
		t.Fatalf("rows %v flash %q, want live listed and no flash", m.sessionRowIDs, m.flash)
	}
}

// The sessions footer keeps its cost keys at 80 columns and offers no history key: history is
// always on.
func TestSessionsFooter_KeepsTheCostKeysAndHasNoHistoryKey(t *testing.T) {
	m := &model{pane: paneSessions}
	line := fitHintLine(m.helpView(), 80)
	for _, want := range []string{"[u] usage", "[$] spend", "[?] keys", "[q] quit"} {
		if !strings.Contains(line, want) {
			t.Errorf("at 80 columns the sessions footer lost %q: %q", want, line)
		}
	}
	for _, viaAgents := range []bool{false, true} {
		m.sessionsViaAgents = viaAgents
		if wide := fitHintLine(m.helpView(), 140); strings.Contains(wide, "[H]") {
			t.Errorf("viaAgents=%v: the footer still offers [H]: %q", viaAgents, wide)
		}
	}
}

// A row only the archive holds is never what the harvest is waiting on. Thirty days of history
// carry rows nothing can name — default, pending:*, agents with no transcripts on this machine —
// and counting them held the gate open, so the transcript tree was re-walked at the backoff cap
// forever. The same row, once its session resumes, is resident and counts again.
func TestHarvestGate_IgnoresRowsOnlyTheArchiveHolds(t *testing.T) {
	no := false
	m := newTitleModel(t, nil)
	m.sessions = []session.SessionSummary{{ID: "default", UpdatedAt: time.Now().Add(-time.Hour), Resident: &no}}
	if m.untitledSettled(time.Now()) || m.untitledFresh() {
		t.Fatal("an archive-only row opened the harvest gate")
	}
	if counted, fresh := m.countUntitled(); len(counted) != 0 || fresh {
		t.Fatalf("countUntitled counted an archive-only row: %v %v", counted, fresh)
	}
	if m.harvestNamedSomething(map[string]SessionMetadata{"default": {Title: "named"}}) {
		t.Fatal("a harvest naming an archive-only row counted as progress")
	}
	m.sessions[0].Resident = nil
	if !m.untitledSettled(time.Now()) || !m.untitledFresh() {
		t.Fatal("the same row, resident, did not open the gate")
	}
}
