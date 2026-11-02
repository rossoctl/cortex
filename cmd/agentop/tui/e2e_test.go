package tui

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/sessionapi"
)

// startStack spins up a real session store + sessionapi.Server on a random
// port, returns the URL + store handle + teardown.
func startStack(t *testing.T) (string, *session.Store, func()) {
	t.Helper()
	store := session.New(5*time.Minute, 100, 0)
	srv := sessionapi.New(":0", store, sessionapi.WithHeartbeatInterval(50*time.Millisecond))
	ts := httptest.NewServer(srv.Server().Handler)
	teardown := func() {
		ts.Close()
		store.Close()
	}
	return ts.URL, store, teardown
}

// TestE2E_ModelReceivesStreamEvents drives the Bubble Tea Update loop
// directly: construct a real model wired to a real server, pump messages
// from Init, append an event on the server, then hand the Cmd-produced
// messages back to Update. Asserts the event appears in the model's
// internal state.
//
// Tests the full wire path without a PTY.
func TestE2E_ModelReceivesStreamEvents(t *testing.T) {
	url, store, teardown := startStack(t)
	defer teardown()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := apiclient.New(url)
	mdl := New(ctx, c).(*model)

	// Drive Init once. Init returns a tea.Batch; to get the initial msgs we
	// would need to run them, but we don't have a Program. Instead,
	// short-circuit: kick off the stream channel + sessions fetch manually.
	mdl.streamCh = c.Stream(ctx, "")

	// Append an event on the server side. First one with no subscribers (ok —
	// Subscribe happens inside Stream's goroutine, which is already running).
	time.Sleep(100 * time.Millisecond) // let Subscribe happen
	store.Append("test-ctx", pipeline.SessionEvent{
		Direction: pipeline.Inbound,
		Phase:     pipeline.SessionRequest,
		A2A:       &pipeline.A2AExtension{Method: "message/send", SessionID: "test-ctx"},
	})

	// Watch the session the event will land in: since #1542 a streamed event
	// is retained only for the session being watched, and this model has
	// opened nothing. Standing in for the user's Enter.
	mdl.selectedSess = "test-ctx"

	// Read one message off the stream — must be either the "open" status
	// or the event. Pump until we see the event.
	deadline := time.After(2 * time.Second)
	var gotEvent bool
	for !gotEvent {
		select {
		case ev, ok := <-mdl.streamCh:
			if !ok {
				t.Fatal("channel closed before event")
			}
			// Hand the msg to the model's Update (what tea would do).
			_, _ = mdl.Update(streamMsg(ev))
			if ev.Event != nil {
				gotEvent = true
			}
		case <-deadline:
			t.Fatal("timeout waiting for event")
		}
	}

	// Verify the model cached the event under the right session.
	if evs := mdl.events["test-ctx"]; len(evs) != 1 {
		t.Fatalf("model cache: got %d events, want 1", len(evs))
	}
	if evs := mdl.events["test-ctx"]; evs[0].A2A == nil || evs[0].A2A.Method != "message/send" {
		t.Errorf("cached event method = %+v", evs[0].A2A)
	}
}

// TestModel_KeyHandling_BackOutFromDetail verifies that pressing Esc in
// detail pane returns to events, and pressing Esc again returns to sessions.
// Pure unit test — no network.
func TestModel_KeyHandling_BackOutFromDetail(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := apiclient.New("http://127.0.0.1:1") // won't be called
	mdl := New(ctx, c).(*model)
	mdl.width, mdl.height = 80, 24
	mdl.pane = paneDetail
	mdl.selectedSess = "ctx"

	// Esc from detail → events.
	mdl.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if mdl.pane != paneEvents {
		t.Errorf("after Esc from detail, pane = %v, want events", mdl.pane)
	}

	// Esc from events → sessions.
	mdl.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if mdl.pane != paneSessions {
		t.Errorf("after Esc from events, pane = %v, want sessions", mdl.pane)
	}
}

// TestModel_PauseTogglesViaKey confirms `p` flips the paused flag.
func TestModel_PauseTogglesViaKey(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := apiclient.New("http://127.0.0.1:1")
	mdl := New(ctx, c).(*model)
	if mdl.paused {
		t.Fatal("paused should start false")
	}
	mdl.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	if !mdl.paused {
		t.Error("after p, paused should be true")
	}
	mdl.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	if mdl.paused {
		t.Error("after second p, paused should be false")
	}
}

// TestModel_SearchKeepsEverySession exercises the search path on the sessions
// pane: `/ bar enter` keeps every row and leaves the cursor on the one that
// matches "bar".
func TestModel_SearchKeepsEverySession(t *testing.T) {
	resetSettingsForTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := apiclient.New("http://127.0.0.1:1")
	mdl := New(ctx, c).(*model)
	mdl.width, mdl.height = 80, 24
	mdl.layout()
	mdl.sessions = []session.SessionSummary{
		{ID: "ctx-foo", UpdatedAt: time.Now()},
		{ID: "ctx-bar", UpdatedAt: time.Now()},
		{ID: "ctx-fooo", UpdatedAt: time.Now()},
	}
	mdl.rebuildSessionsTable()
	if got := len(mdl.sessionsTbl.Rows()); got != 3 {
		t.Fatalf("before the search: %d rows, want 3", got)
	}

	// Open the search with `/`, type "bar", commit with Enter.
	mdl.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	if !mdl.searching {
		t.Fatal("expected searching=true after /")
	}
	for _, r := range "bar" {
		mdl.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	mdl.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if mdl.searching {
		t.Error("the prompt should close on Enter")
	}
	if got := len(mdl.sessionsTbl.Rows()); got != 3 {
		t.Errorf("after searching 'bar': %d rows, want all 3", got)
	}
	if got := mdl.selectedSessionID(); got != "ctx-bar" {
		t.Errorf("after searching 'bar' the cursor is on %q, want ctx-bar", got)
	}
}
