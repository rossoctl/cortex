package tui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/session/archive"
	"github.com/rossoctl/cortex/core/sessionapi"
)

// stackSession is one session eraseStack creates: one request event, from the coding agent
// client names, or from none when it is nil.
type stackSession struct {
	id     string
	client *pipeline.EventClient
}

var claudeCode = &pipeline.EventClient{Name: "claude-code", Version: "2.1"}

// eraseStack is clearStack with the sessions chosen by the test: a real session API over a store
// and an archive, as on a laptop.
func eraseStack(t *testing.T, sessions ...stackSession) (*model, *session.Store) {
	t.Helper()
	resetSettingsForTest(t)
	a, err := archive.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := session.New(5*time.Minute, 100, 0)
	store.AddRecorder(a)
	for _, s := range sessions {
		store.Append(s.id, pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest,
			Host: "api.example.com", Client: s.client})
	}
	ts := httptest.NewServer(sessionapi.New(":0", store,
		sessionapi.WithArchive(a), sessionapi.WithClearAllowed(true)).Server().Handler)
	t.Cleanup(func() {
		ts.Close()
		store.Close()
		a.Close()
	})
	m := New(context.Background(), apiclient.New(ts.URL)).(*model)
	m.width, m.height = 120, 40
	m.pane = paneSessions
	m.Update(m.loadSessionsCmd()())
	return m, store
}

// cursorToSession puts the sessions table's cursor on id's row and returns its index.
func cursorToSession(t *testing.T, m *model, id string) int {
	t.Helper()
	i := slices.Index(m.sessionRowIDs, id)
	if i < 0 {
		t.Fatalf("no row for %q in %v", id, m.sessionRowIDs)
	}
	setCursorVisible(&m.sessionsTbl, i)
	return i
}

// pressXThenY opens the confirmation, lets its count land, answers y and delivers the result.
func pressXThenY(t *testing.T, m *model) {
	t.Helper()
	cmd := m.handleKey(keyRune('x'))
	if cmd == nil || m.clearConfirm == nil {
		t.Fatal("x opened no confirmation")
	}
	m.Update(cmd())
	if cmd = m.handleKey(keyRune('y')); cmd == nil {
		t.Fatal("y sent nothing")
	}
	m.Update(cmd())
}

// x asks about the highlighted session by name, with its agent and size — and names an untitled,
// agent-less session by its id without an empty "()" or a dangling comma.
func TestXKey_OnSessionsAsksAboutTheHighlightedSession(t *testing.T) {
	m, _ := eraseStack(t, stackSession{"s1", nil}, stackSession{"s2", claudeCode})
	for _, tc := range []struct{ id, want string }{
		{"s2", `Erase session "s2" (claude-code, 1 event) from this Cortex, in memory and on disk?`},
		{"s1", `Erase session "s1" (1 event) from this Cortex, in memory and on disk?`},
	} {
		cursorToSession(t, m, tc.id)
		cmd := m.handleKey(keyRune('x'))
		if cmd == nil || m.clearConfirm == nil {
			t.Fatalf("x on %s opened no confirmation", tc.id)
		}
		m.Update(cmd())
		text, hint := confirmText(m.clearConfirm)
		if !strings.Contains(text, tc.want) || !strings.Contains(text, "still using this session brings it back") {
			t.Errorf("x on %s says:\n%s\nwant %q", tc.id, text, tc.want)
		}
		if hint != "[y] erase  [n] keep" {
			t.Errorf("hint = %q", hint)
		}
		m.handleKey(keyRune('n'))
	}
	m.pane = paneEvents
	if m.handleKey(keyRune('x')); m.clearConfirm != nil {
		t.Fatal("x opened a confirmation on the events pane")
	}
}

// y erases that session on the server and in agentop, leaves the others, and the cursor lands on
// the row after it rather than jumping to the top.
func TestXConfirm_YErasesTheSessionAndLandsOnTheNextRow(t *testing.T) {
	m, store := eraseStack(t, stackSession{"a", nil}, stackSession{"b", nil}, stackSession{"c", nil})
	rows := slices.Clone(m.sessionRowIDs)
	mid, next := rows[1], rows[2]
	cursorToSession(t, m, mid)
	m.events[mid] = []pipeline.SessionEvent{{Seq: 1}}
	m.selectedSess = mid

	pressXThenY(t, m)

	if store.View(mid) != nil {
		t.Fatalf("the server still holds %s", mid)
	}
	if store.View(rows[0]) == nil || store.View(next) == nil {
		t.Fatal("erasing one session erased another")
	}
	if _, ok := m.events[mid]; ok || m.selectedSess != "" {
		t.Fatalf("agentop kept what it held about %s: events=%v selected=%q", mid, ok, m.selectedSess)
	}
	if got := m.selectedSessionID(); got != next {
		t.Fatalf("cursor on %q after the erase, want the next row %q", got, next)
	}
	if !strings.Contains(m.flash, `erased session "`+mid+`"`) || !strings.Contains(m.flash, "cost ledger is kept") {
		t.Fatalf("flash = %q", m.flash)
	}
	if m.clearConfirm != nil {
		t.Fatal("the confirmation is still up")
	}
}

// A snapshot asked for before an x erase is dropped for the erased session only: a different
// session's snapshot, in flight at the same time, is still stored. (X drops every one.)
func TestSnapshot_AnEraseDropsOnlyThatSessionsStaleSnapshot(t *testing.T) {
	m, _ := eraseStack(t, stackSession{"s1", nil}, stackSession{"s2", nil})
	stale1, stale2 := m.snapshotCmd("s1")(), m.snapshotCmd("s2")()
	cursorToSession(t, m, "s1")
	pressXThenY(t, m)
	m.Update(stale1)
	m.Update(stale2)
	if evs := m.events["s1"]; len(evs) != 0 {
		t.Fatalf("a snapshot from before the erase brought s1 back with %d events", len(evs))
	}
	if evs := m.events["s2"]; len(evs) != 1 {
		t.Fatalf("s2's snapshot was dropped too: %d events stored", len(evs))
	}
}

// A session the proxy's list no longer holds offers nothing to erase.
func TestXConfirm_ASessionTheProxyNoLongerHoldsOffersOnlyClose(t *testing.T) {
	m, _ := eraseStack(t, stackSession{"s1", nil})
	m.Update(m.openEraseConfirm(&clearConfirm{target: eraseSession, id: "never"})())
	text, hint := confirmText(m.clearConfirm)
	if text != "This Cortex holds no such session." || hint != "[n] close" {
		t.Fatalf("confirmation = %q / %q", text, hint)
	}
	if cmd := m.handleKey(keyRune('y')); cmd != nil {
		t.Fatal("y sent an erase with nothing counted")
	}
}

// eraseFake answers every DELETE /v1/sessions/{id} with the status statusFor names, and counts
// the requests. A 500 for an id starting "archived-" is an archive-only session's: memory did not
// hold it, so it says "sessions":0.
func eraseFake(t *testing.T, statusFor func(id string) int) (*apiclient.Client, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		id := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
		code := statusFor(id)
		w.WriteHeader(code)
		switch {
		case code == http.StatusOK:
			w.Write([]byte(`{"sessions":1,"archivedSessions":1,"bytes":1000}`))
		case code == http.StatusForbidden:
			w.Write([]byte(`{"error":"not bound to loopback only"}`))
		case code == http.StatusInternalServerError && strings.HasPrefix(id, "archived-"):
			w.Write([]byte(`{"sessions":0,"archivedSessions":0,"bytes":0,"archiveError":"disk says no"}`))
		case code == http.StatusInternalServerError:
			w.Write([]byte(`{"sessions":1,"archivedSessions":0,"bytes":0,"archiveError":"disk says no"}`))
		}
	}))
	t.Cleanup(ts.Close)
	return apiclient.New(ts.URL), &n
}

// A failure is counted and the run carries on; a 404 is a session already gone; a 500 erased
// memory but not disk, so it is erased from memory only, reported by its archiveError, and is no
// failure.
func TestEraseSessions_CarriesOnPastAFailureAndCountsA404AsGone(t *testing.T) {
	c, n := eraseFake(t, func(id string) int {
		switch id {
		case "x":
			return http.StatusBadGateway
		case "b":
			return http.StatusInternalServerError
		case "c":
			return http.StatusNotFound
		}
		return http.StatusOK
	})
	msg := eraseSessions(context.Background(), c, []string{"a", "x", "b", "c"})
	if n.Load() != 4 || !slices.Equal(msg.gone, []string{"a", "c"}) || !slices.Equal(msg.memOnly, []string{"b"}) ||
		msg.asked != 4 || msg.res.Sessions != 2 || msg.res.Bytes != 1000 || msg.err == nil {
		t.Fatalf("%d requests, eraseSessions = %+v", n.Load(), msg)
	}
	msg.name, msg.agent = "of opencode's sessions", true
	want := "erased 3 of 4 of opencode's sessions; 1 from memory only: disk says no; " +
		"1 failed: /v1/sessions/x: unexpected status 502"
	if got := erasedFlash(msg); got != want {
		t.Fatalf("flash = %q\nwant    %q", got, want)
	}
}

// A 500 erased the session from memory but not from disk. That is erased, not "nothing erased",
// and the flash says which half failed, as X's does.
func TestErasedFlash_A500IsErasedFromMemoryButNotFromDisk(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agent bool
		ids   []string
		fail  string // the id answering 500; "" for every id
		want  string
	}{
		{`session "fix router pins"`, false, []string{"s"}, "",
			`erased session "fix router pins" from memory, but not from disk: disk says no`},
		{"of opencode's sessions", true, []string{"a", "b"}, "b",
			"erased all 2 of opencode's sessions; 1 from memory only: disk says no"},
		{"of opencode's sessions", true, []string{"a", "b"}, "",
			"erased all 2 of opencode's sessions from memory, but not from disk: disk says no"},
	} {
		c, _ := eraseFake(t, func(id string) int {
			if tc.fail == "" || id == tc.fail {
				return http.StatusInternalServerError
			}
			return http.StatusOK
		})
		msg := eraseSessions(context.Background(), c, tc.ids)
		if want := len(tc.ids); len(msg.gone)+len(msg.memOnly) != want || msg.err != nil {
			t.Errorf("%v: eraseSessions = %+v, want all %d erased and no failure", tc.ids, msg, want)
		}
		msg.name, msg.agent = tc.name, tc.agent
		if got := erasedFlash(msg); got != tc.want {
			t.Errorf("flash = %q\nwant    %q", got, tc.want)
		}
	}
}

// A 500 saying "sessions":0 erased nothing: memory did not hold the session, and the archive's
// delete failed. That is an outright failure, not an erase from memory.
func TestErasedFlash_A500WithNothingInMemoryErasedNothing(t *testing.T) {
	c, _ := eraseFake(t, func(string) int { return http.StatusInternalServerError })
	msg := eraseSessions(context.Background(), c, []string{"archived-s"})
	if len(msg.gone) != 0 || len(msg.memOnly) != 0 || msg.err == nil {
		t.Fatalf("eraseSessions = %+v, want nothing erased and a failure", msg)
	}
	msg.name = `session "archived-s"`
	if got := erasedFlash(msg); !strings.HasPrefix(got, "nothing erased: ") || !strings.Contains(got, "disk says no") {
		t.Fatalf("flash = %q", got)
	}
}

// A session erased from memory only is forgotten like one erased whole. Memory dropped it, so the
// proxy soon stops listing it; events still cached here would keep it as a cached-only row that x
// can no longer erase. If its history is still on disk the list brings the row back, and Enter
// reads its events from disk.
func TestApplyErased_ForgetsASessionErasedFromMemoryOnly(t *testing.T) {
	m, _ := eraseStack(t, stackSession{"b", nil})
	// ghost is the 500's session: the proxy's memory no longer holds it, agentop's cache does.
	m.events["ghost"] = []pipeline.SessionEvent{{Seq: 1}}
	m.selectedSess = "ghost"
	m.rebuildSessionsTable()
	if !slices.Contains(m.sessionRowIDs, "ghost") {
		t.Fatalf("precondition: no cached-only row for ghost in %v", m.sessionRowIDs)
	}
	c, _ := eraseFake(t, func(string) int { return http.StatusInternalServerError })
	msg := eraseSessions(context.Background(), c, []string{"ghost"})
	msg.name = `session "ghost"`
	m.Update(msg)
	m.Update(m.loadSessionsCmd()())
	if _, ok := m.events["ghost"]; ok || m.selectedSess != "" {
		t.Fatalf("agentop kept what it held about ghost: events=%v selected=%q", ok, m.selectedSess)
	}
	if slices.Contains(m.sessionRowIDs, "ghost") {
		t.Fatalf("ghost is still a row: %v", m.sessionRowIDs)
	}
}

// A refusal, or a proxy without the route, stops the run at the first id: every other id would
// get the same answer.
func TestEraseSessions_StopsAtARefusalOrAnOldProxy(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusMethodNotAllowed} {
		c, n := eraseFake(t, func(string) int { return code })
		msg := eraseSessions(context.Background(), c, []string{"a", "b", "c"})
		if n.Load() != 1 || len(msg.gone) != 0 || msg.err == nil {
			t.Fatalf("%d: %d requests, %+v; want one request, nothing gone, an error", code, n.Load(), msg)
		}
	}
	c, _ := eraseFake(t, func(string) int { return http.StatusForbidden })
	msg := eraseSessions(context.Background(), c, []string{"a"})
	msg.name = `session "a"`
	if got := erasedFlash(msg); !strings.HasPrefix(got, "nothing erased: ") || !strings.Contains(got, "loopback only") {
		t.Fatalf("flash = %q", got)
	}
	if !errors.Is(msg.err, apiclient.ErrClearRefused) {
		t.Fatalf("err = %v, want ErrClearRefused", msg.err)
	}
}

func TestErasedFlash_NamesWhatWentAndFromWhere(t *testing.T) {
	one := erasedMsg{name: `session "fix router pins"`, asked: 1, gone: []string{"s"},
		res: apiclient.ClearResult{Sessions: 1, ArchivedSessions: 1, Bytes: 18 << 20}}
	if got, want := erasedFlash(one), `erased session "fix router pins" (`+formatBytes(18<<20)+` from disk); the cost ledger is kept`; got != want {
		t.Errorf("flash = %q, want %q", got, want)
	}
	agent := erasedMsg{name: "of opencode's sessions", agent: true, asked: 2, gone: []string{"a", "b"}}
	if got, want := erasedFlash(agent), "erased all 2 of opencode's sessions; the cost ledger is kept"; got != want {
		t.Errorf("flash = %q, want %q", got, want)
	}
	partial := erasedMsg{name: "of opencode's sessions", agent: true, asked: 14, gone: make([]string, 12),
		err: errors.New("connection refused")}
	if got, want := erasedFlash(partial), "erased 12 of 14 of opencode's sessions; 2 failed: connection refused"; got != want {
		t.Errorf("flash = %q, want %q", got, want)
	}
}

// A count fetched for a dialog that has since closed does not fill the next one: x on an agent, n,
// and x again before the first list lands would otherwise count every session twice.
func TestXConfirm_ACountForAClosedDialogDoesNotFillTheNext(t *testing.T) {
	m, _ := eraseStack(t, stackSession{"cc-1", claudeCode}, stackSession{"cc-2", claudeCode})
	first := m.openEraseConfirm(&clearConfirm{target: eraseAgent, agent: "claude-code"})
	m.handleKey(keyRune('n'))
	second := m.openEraseConfirm(&clearConfirm{target: eraseAgent, agent: "claude-code"})
	m.Update(first())
	m.Update(second())
	if got := len(m.clearConfirm.ids); got != 2 {
		t.Fatalf("the confirmation holds %d ids, want 2: %v", got, m.clearConfirm.ids)
	}
	if text, _ := confirmText(m.clearConfirm); !strings.Contains(text, "Erase all 2 of claude-code's sessions") {
		t.Fatalf("confirmation says:\n%s", text)
	}
}

// cursorToAgent opens the agents pane with its cursor on label's row; "" is All agents.
func cursorToAgent(t *testing.T, m *model, label string) {
	t.Helper()
	m.pane = paneAgents
	m.rebuildAgentsTable()
	i := agentRowIndex(m, label)
	if i < 0 {
		t.Fatalf("no agents row %q in %v", label, m.agentRowDescs)
	}
	setCursorVisible(&m.agentsTbl, i)
}

// x on an agent counts that agent's sessions from the proxy's list, erases exactly those, and
// leaves every other agent's.
func TestXOnAgents_ErasesEveryOneOfThatAgentsSessions(t *testing.T) {
	m, store := eraseStack(t, stackSession{"cc-1", claudeCode}, stackSession{"cc-2", claudeCode}, stackSession{"plain", nil})
	cursorToAgent(t, m, "claude-code")
	cmd := m.handleKey(keyRune('x'))
	if cmd == nil || m.clearConfirm == nil || m.clearConfirm.target != eraseAgent {
		t.Fatal("x on the claude-code row opened no agent confirmation")
	}
	m.Update(cmd())
	text, _ := confirmText(m.clearConfirm)
	if !strings.Contains(text, "Erase all 2 of claude-code's sessions from this Cortex, in memory and on disk?") ||
		!strings.Contains(text, "still using one of these sessions") {
		t.Fatalf("the confirmation says:\n%s", text)
	}
	if cmd = m.handleKey(keyRune('y')); cmd == nil {
		t.Fatal("y sent nothing")
	}
	m.Update(cmd())
	if store.View("cc-1") != nil || store.View("cc-2") != nil || store.View("plain") == nil {
		t.Fatalf("after the erase: cc-1=%v cc-2=%v plain=%v", store.View("cc-1") != nil, store.View("cc-2") != nil, store.View("plain") != nil)
	}
	if !strings.Contains(m.flash, "erased all 2 of claude-code's sessions") {
		t.Fatalf("flash = %q", m.flash)
	}
}

// Other is everything no recognised agent accounts for: an unrecognised client's sessions and the
// default bucket go, a recognised agent's stay.
func TestXOnAgents_OtherErasesTheUnrecognisedAndTheDefaultBucket(t *testing.T) {
	m, store := eraseStack(t,
		stackSession{"cc-1", claudeCode},
		stackSession{"plain", nil},
		stackSession{session.DefaultSessionID, nil},
		stackSession{"odd", &pipeline.EventClient{Name: "curl", Version: "8.7"}})
	cursorToAgent(t, m, otherAgents)
	pressXThenY(t, m)
	for _, id := range []string{"plain", session.DefaultSessionID, "odd"} {
		if store.View(id) != nil {
			t.Errorf("%s survived erasing Other", id)
		}
	}
	if store.View("cc-1") == nil {
		t.Fatal("erasing Other erased claude-code's session")
	}
}

// All agents is every session, so x there is X's confirmation, erasing by one request — and the
// reader stays on the agents pane, as after any other erase there.
func TestXOnAgents_AllAgentsIsXsConfirmation(t *testing.T) {
	m, store := eraseStack(t, stackSession{"cc-1", claudeCode}, stackSession{"plain", nil})
	cursorToAgent(t, m, "")
	cmd := m.handleKey(keyRune('x'))
	if cmd == nil || m.clearConfirm == nil || m.clearConfirm.target != eraseAll {
		t.Fatal("x on All agents did not open X's confirmation")
	}
	m.Update(cmd())
	if text, _ := confirmText(m.clearConfirm); !strings.Contains(text, "Erase all 2 sessions") {
		t.Fatalf("the confirmation says:\n%s", text)
	}
	if cmd = m.handleKey(keyRune('y')); cmd == nil {
		t.Fatal("y sent nothing")
	}
	m.Update(cmd())
	if n := len(store.ListSessions()); n != 0 {
		t.Fatalf("the server still holds %d sessions", n)
	}
	if m.pane != paneAgents {
		t.Fatalf("after the clear the pane is %v, want the agents pane", m.pane)
	}
	if !strings.Contains(m.flash, "cleared 2 sessions") {
		t.Fatalf("flash = %q", m.flash)
	}
}

// An agent with spend in the window but no session left offers nothing to erase.
func TestConfirmText_AnAgentWithNoSessionsOffersOnlyClose(t *testing.T) {
	cc := &clearConfirm{target: eraseAgent, agent: "opencode", counted: true}
	if text, hint := confirmText(cc); text != "opencode has no sessions on this Cortex." || hint != "[n] close" {
		t.Fatalf("confirmation = %q / %q", text, hint)
	}
}

// x is named in the help of both panes it works on.
func TestPaneKeys_NameXWhereItErases(t *testing.T) {
	for _, p := range []paneID{paneSessions, paneAgents} {
		if !slices.ContainsFunc(paneKeys[p].bindings, func(b keyBinding) bool { return b.keys == "x" }) {
			t.Errorf("%s names no x", paneKeys[p].title)
		}
	}
}
