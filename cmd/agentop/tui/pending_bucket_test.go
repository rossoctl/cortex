package tui

import (
	"slices"
	"testing"

	"github.com/rossoctl/cortex/cmd/agentop/tui/table"
	"github.com/rossoctl/cortex/core/session"
)

// A real per-process pending bucket, as the proxy names one.
const pendingOpencode = "pending:opencode@45462.1790984641150001000"

// rowOf is the index of id's row in the sessions table, failing the test when it has none.
func rowOf(t *testing.T, m *model, id string) int {
	t.Helper()
	i := slices.Index(m.sessionRowIDs, id)
	if i < 0 {
		t.Fatalf("no row for %s in %v", id, m.sessionRowIDs)
	}
	return i
}

// The SESSION cell names the agent rather than printing a truncated "pending:openc…", which
// read as something unfinished. The row still selects by its full id.
func TestPendingBucket_SessionCellNamesTheAgent(t *testing.T) {
	m := uuidPicker(t, 160, uuidOpened, pendingOpencode)
	if got := m.sessionsTbl.Rows()[rowOf(t, m, pendingOpencode)][0]; got != "opencode" {
		t.Errorf("SESSION cell = %q, want %q", got, "opencode")
	}

	// A bucket the server has forgotten is still a row while agentop holds its events.
	m.sessions = slices.DeleteFunc(m.sessions, func(s session.SessionSummary) bool { return s.ID == pendingOpencode })
	m.rebuildSessionsTable()
	if got := m.sessionsTbl.Rows()[rowOf(t, m, pendingOpencode)][0]; got != "opencode" {
		t.Errorf("cached-only SESSION cell = %q, want %q", got, "opencode")
	}
}

// Nothing names a pending bucket, so the TITLE cell says what the row is, and which process
// when the bucket is one process's. The header keeps the id, as every header does.
func TestPendingBucket_TitleSaysItIsOutsideAnySession(t *testing.T) {
	m := uuidPicker(t, 160, pendingOpencode, "pending:bob")
	rows := m.sessionsTbl.Rows()
	if got, want := rows[rowOf(t, m, pendingOpencode)][1], "calls outside any session · pid 45462"; got != want {
		t.Errorf("TITLE cell = %q, want %q", got, want)
	}
	if got, want := rows[rowOf(t, m, "pending:bob")][1], "calls outside any session"; got != want {
		t.Errorf("agent-wide bucket's TITLE cell = %q, want %q", got, want)
	}
	if got, want := m.sessionLabel(pendingOpencode), "calls outside any session · pid 45462 ("+pendingOpencode+")"; got != want {
		t.Errorf("header label = %q, want %q", got, want)
	}
}

// The fallback is the last resort: a title the server derived from the bucket's own events
// is a fact about it, and wins.
func TestPendingBucket_AServedTitleStillWins(t *testing.T) {
	m := uuidPicker(t, 160, pendingOpencode)
	m.sessions[0].Title = "what does this repo do"
	m.rebuildSessionsTable()
	if got := m.sessionsTbl.Rows()[0][1]; got != "what does this repo do" {
		t.Errorf("TITLE cell = %q, want the served title", got)
	}
}

// A chatty bucket is the most recently updated row most of the time, and it is no session
// anyone opened. Pending buckets list after the real sessions, each group newest first.
func TestPendingBucket_RowsListAfterRealSessions(t *testing.T) {
	m := uuidPicker(t, 160, pendingOpencode, uuidOpened, "pending:bob", uuidOther)
	want := []string{uuidOpened, uuidOther, pendingOpencode, "pending:bob"}
	if !slices.Equal(m.sessionRowIDs, want) {
		t.Errorf("rows = %v, want %v", m.sessionRowIDs, want)
	}
}

// Buckets list after the real sessions whichever source each row comes from: the server's
// list, or agentop's cache of a session the server has forgotten, as every session is right
// after a proxy restart.
func TestPendingBucket_RowsListAfterRealSessionsFromEitherSource(t *testing.T) {
	const opencodeSession = "ses_f00ff1689ffeGD4X9nXaHggqh1" // sorts after "pending:"
	for _, tc := range []struct {
		name   string
		ids    []string
		cached []string // dropped from the server's list; agentop still holds their events
		want   []string
	}{
		{"restart: the bucket is listed, the real sessions are cached",
			[]string{uuidOpened, uuidOther, pendingOpencode}, []string{uuidOpened, uuidOther},
			[]string{uuidOther, uuidOpened, pendingOpencode}},
		{"both cached, the real id sorting after the bucket's",
			[]string{opencodeSession, pendingOpencode}, []string{opencodeSession, pendingOpencode},
			[]string{opencodeSession, pendingOpencode}},
		{"the bucket cached, the real session listed",
			[]string{uuidOpened, pendingOpencode}, []string{pendingOpencode},
			[]string{uuidOpened, pendingOpencode}},
		{"a listed bucket and a cached one, after a listed and a cached session",
			[]string{"pending:bob", uuidOpened, pendingOpencode, opencodeSession}, []string{pendingOpencode, opencodeSession},
			[]string{uuidOpened, opencodeSession, "pending:bob", pendingOpencode}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := uuidPicker(t, 160, tc.ids...)
			m.sessions = slices.DeleteFunc(m.sessions, func(s session.SessionSummary) bool { return slices.Contains(tc.cached, s.ID) })
			m.sessionsTbl, m.sessionRowIDs = newSessionsTable(), nil // nothing selected yet, as when agentop attaches after the restart
			m.rebuildSessionsTable()
			if !slices.Equal(m.sessionRowIDs, tc.want) {
				t.Errorf("rows = %v, want %v", m.sessionRowIDs, tc.want)
			}
			if got := m.selectedSessionID(); got != tc.want[0] {
				t.Errorf("cursor on %q, want the first real session %q", got, tc.want[0])
			}
		})
	}
}

// With the AGENT column showing, a pending bucket's cell names the agent from its id: the
// server leaves Agent blank for one, which attributes the bucket to no session's agent.
func TestPendingBucket_AgentCellNamesTheAgent(t *testing.T) {
	m := uuidPicker(t, 200, uuidOpened, uuidOther, pendingOpencode)
	m.sessions[0].Agent, m.sessions[1].Agent = "claude-code", "bob"
	m.rebuildSessionsTable()
	cols := m.sessionsTbl.Columns()
	agent := slices.IndexFunc(cols, func(c table.Column) bool { return headerTitle(c) == "AGENT" })
	if agent < 0 {
		t.Fatalf("no AGENT column at width 200: %v", cols)
	}
	if got := m.sessionsTbl.Rows()[rowOf(t, m, pendingOpencode)][agent]; got != "opencode" {
		t.Errorf("AGENT cell = %q, want %q", got, "opencode")
	}
}

func TestPendingBucket(t *testing.T) {
	for _, tc := range []struct {
		id, agent, pid string
		ok             bool
	}{
		{pendingOpencode, "opencode", "45462", true},
		{"pending:claude-code", "claude-code", "", true},
		{"pending:", "", "", false},
		{session.DefaultSessionID, "", "", false},
		{uuidOpened, "", "", false},
	} {
		agent, pid, ok := pendingBucket(tc.id)
		if agent != tc.agent || pid != tc.pid || ok != tc.ok {
			t.Errorf("pendingBucket(%q) = %q, %q, %v; want %q, %q, %v", tc.id, agent, pid, ok, tc.agent, tc.pid, tc.ok)
		}
	}
}

// A pending bucket belongs to the agent its id names, not to Other: its spend is already that
// agent's, and the list must put the row beside it. The default bucket stays Other's.
func TestSessionAgent_APendingBucketBelongsToItsAgent(t *testing.T) {
	for _, tc := range []struct{ id, want string }{
		{pendingOpencode, "opencode"},
		{"pending:claude-code@100.5", "claude-code"},
		{"pending:bob-shell@1.2", "bob"}, // archived under a retired spelling
		{session.DefaultSessionID, ""},
	} {
		s := session.SessionSummary{ID: tc.id}
		if got := sessionAgent(s); got != tc.want {
			t.Errorf("sessionAgent(%s) = %q, want %q", tc.id, got, tc.want)
		}
		if inOtherAgents(s) != (tc.want == "") {
			t.Errorf("inOtherAgents(%s) = %v", tc.id, inOtherAgents(s))
		}
	}
	m := uuidPicker(t, 200, uuidOpened, "pending:claude-code@100.5", session.DefaultSessionID)
	m.sessions[0].Agent = "claude-code"
	m.agentScope = "claude-code"
	m.rebuildSessionsTable()
	if !slices.Equal(m.sessionRowIDs, []string{uuidOpened, "pending:claude-code@100.5"}) {
		t.Errorf("claude-code scope lists %v, want its session and its bucket", m.sessionRowIDs)
	}
}
