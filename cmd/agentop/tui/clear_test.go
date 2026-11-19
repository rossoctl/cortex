package tui

import (
	"context"
	"net/http/httptest"
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

// clearStack is a real session API over a store with two sessions and an archive, as on a
// laptop; allowed is what the binary would pass from listener.bind_loopback_only.
func clearStack(t *testing.T, allowed bool) (*model, *session.Store) {
	t.Helper()
	resetSettingsForTest(t)
	a, err := archive.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := session.New(5*time.Minute, 100, 0)
	store.AddRecorder(a)
	for _, id := range []string{"s1", "s2"} {
		store.Append(id, pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest, Host: "api.example.com"})
	}
	ts := httptest.NewServer(sessionapi.New(":0", store,
		sessionapi.WithArchive(a), sessionapi.WithClearAllowed(allowed)).Server().Handler)
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

// Clear-all asks before it erases, and says what it is about to erase: the count and the size come
// from the archive's own list, not from what agentop happens to hold. It is reached by `x` on All
// agents (erase_test.go); `X` is no longer a key.
func TestClearAll_OpensTheConfirmationWithCounts(t *testing.T) {
	m, _ := clearStack(t, true)
	cmd := m.openClearConfirm()
	if m.clearConfirm == nil || cmd == nil {
		t.Fatal("openClearConfirm opened no confirmation")
	}
	m.Update(cmd())
	view := m.View()
	for _, want := range []string{"Erase all 2 sessions", "on disk", "cost ledger is kept", "[y]"} {
		if !strings.Contains(view, want) {
			t.Errorf("the confirmation does not say %q:\n%s", want, view)
		}
	}
	m.clearConfirm = nil
	if m.handleKey(keyRune('X')); m.clearConfirm != nil {
		t.Fatal("X opened the confirmation; the key was removed")
	}
}

// y erases on the server and resets what agentop held about the sessions it erased, staying on
// the sessions pane rather than tearing the connection down.
func TestClearConfirm_YClearsAndResetsState(t *testing.T) {
	m, store := clearStack(t, true)
	m.events["s1"] = []pipeline.SessionEvent{{Seq: 1}}
	m.events["gone-from-server"] = []pipeline.SessionEvent{{Seq: 1}} // a cached-only row
	m.selectedSess = "s1"
	m.Update(m.openClearConfirm()())
	cmd := m.handleKey(keyRune('y'))
	if cmd == nil {
		t.Fatal("y sent no clear")
	}
	m.Update(cmd())
	if n := len(store.ListSessions()); n != 0 {
		t.Fatalf("the server still holds %d sessions", n)
	}
	if m.clearConfirm != nil || m.pane != paneSessions {
		t.Fatalf("after the clear: confirm=%v pane=%v, want closed on the sessions pane", m.clearConfirm, m.pane)
	}
	if len(m.events) != 0 || m.selectedSess != "" || len(m.sessions) != 0 {
		t.Fatalf("agentop kept what it held: events=%d selected=%q sessions=%d", len(m.events), m.selectedSess, len(m.sessions))
	}
	if !strings.Contains(m.flash, "cleared 2 sessions") {
		t.Fatalf("flash = %q, want the count", m.flash)
	}
}

func TestClearConfirm_NAndEscCancel(t *testing.T) {
	for _, k := range []tea.KeyMsg{keyRune('n'), keyRune('N'), {Type: tea.KeyEsc}} {
		m, store := clearStack(t, true)
		m.Update(m.openClearConfirm()())
		if cmd := m.handleKey(k); cmd != nil || m.clearConfirm != nil {
			t.Fatalf("%q: confirm=%v cmd=%v, want closed and nothing sent", k.String(), m.clearConfirm, cmd != nil)
		}
		if len(store.ListSessions()) != 2 {
			t.Fatalf("%q cleared the server", k.String())
		}
	}
}

// While the confirmation is up it owns the keyboard: no key reaches the pane beneath, and no key
// other than y erases anything.
func TestClearConfirm_SwallowsOtherKeys(t *testing.T) {
	m, store := clearStack(t, true)
	m.Update(m.openClearConfirm()())
	for _, k := range []tea.KeyMsg{keyRune('u'), keyRune('?'), keyRune('/'), keyRune('H'), keyRune('X'), keyRune('$'), {Type: tea.KeyEnter}} {
		if cmd := m.handleKey(k); cmd != nil {
			t.Errorf("%q returned a command", k.String())
		}
		if m.clearConfirm == nil || m.pane != paneSessions || m.helpVisible || m.searching {
			t.Fatalf("%q got past the confirmation: confirm=%v pane=%v help=%v searching=%v",
				k.String(), m.clearConfirm != nil, m.pane, m.helpVisible, m.searching)
		}
	}
	if len(store.ListSessions()) != 2 {
		t.Fatal("a key other than y cleared the server")
	}
}

// A proxy that refuses says why, and agentop shows that rather than pretending it worked.
func TestClearConfirm_ShowsTheServersRefusal(t *testing.T) {
	m, store := clearStack(t, false)
	m.events["s1"] = []pipeline.SessionEvent{{Seq: 1}}
	m.Update(m.openClearConfirm()())
	m.Update(m.handleKey(keyRune('y'))())
	if !strings.Contains(m.flash, "loopback only") {
		t.Fatalf("flash = %q, want the server's reason", m.flash)
	}
	if len(store.ListSessions()) != 2 || len(m.events) != 1 || m.clearConfirm != nil {
		t.Fatalf("a refused clear changed state: server=%d events=%d confirm=%v",
			len(store.ListSessions()), len(m.events), m.clearConfirm != nil)
	}
}

// A snapshot asked for before the clear and answered after it describes a session that is gone;
// stored, it would bring the session back as a cached row.
func TestSnapshot_FromBeforeAClearIsDropped(t *testing.T) {
	m, _ := clearStack(t, true)
	stale := m.snapshotCmd("s1")() // answered now, delivered after the clear
	if _, ok := stale.(snapshotLoadedMsg); !ok {
		t.Fatalf("snapshot = %T, want snapshotLoadedMsg", stale)
	}
	m.Update(m.openClearConfirm()())
	m.Update(m.handleKey(keyRune('y'))())
	m.Update(stale)
	if evs := m.events["s1"]; len(evs) != 0 {
		t.Fatalf("a snapshot from before the clear brought back %d events", len(evs))
	}
}
