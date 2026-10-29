package tui

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/session"
)

// morningAfterFixture is the laptop this was reported from, the afternoon after a night of three
// agents: the sessions list still holds OpenCode's and Bob's sessions from yesterday, while today's
// window has seen only Claude Code. Before the fix `A` refused here with "only claude-code has been
// seen", and the list mixed three agents with no way to narrow it.
func morningAfterFixture() *model {
	now := time.Now()
	return &model{width: 200, agentsTbl: newAgentsTable(), client: deadClient(),
		pane: paneSessions, previousPane: paneNone, pipelineReturnPane: paneNone,
		sessions: []session.SessionSummary{
			{ID: "claude-1", UpdatedAt: now, Agent: "claude-code"},
			{ID: "claude-2", UpdatedAt: now, Agent: "claude-code"},
			{ID: "oc-1", UpdatedAt: now.Add(-18 * time.Hour), Agent: "opencode"},
			{ID: "bob-1", UpdatedAt: now.Add(-19 * time.Hour), Agent: "bob-shell"},
			{ID: session.DefaultSessionID, UpdatedAt: now},
		},
		agents: []agentRow{{label: "claude-code", Counts: usage.Counts{
			Requests: 412, Tokens: 175_000_000, PricedRequests: 412, CostMicros: 113_630_000}}},
	}
}

func pickerLabels(rows []agentRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.label)
	}
	return out
}

// An agent that owns a listed session has a row whether or not it spent anything in the window.
// The picker scopes the sessions list, which spans days, so a row set taken from today's spending
// alone left yesterday's agents in the list with no row to reach them.
//
// After the agents that spent, ordered by label, and before Other: they are agents the window has
// no figures for, not rivals for the cost ranking.
func TestPickerRows_AnAgentWithSessionsButNoSpendHasARow(t *testing.T) {
	m := morningAfterFixture()
	want := []string{"claude-code", "bob-shell", "opencode", otherAgents}
	if got := pickerLabels(m.pickerRows()); !slices.Equal(got, want) {
		t.Fatalf("picker rows = %v, want %v", got, want)
	}
	if len(m.agents) != 1 {
		t.Errorf("pickerRows changed m.agents to %+v; the window's rows are the fetch's alone", m.agents)
	}
}

// Its row says it has sessions, when it was last seen, and that the window has nothing for it:
// SESSIONS and LAST SEEN describe what the scope lists, and the window's one figure is a dash.
// LAST SEEN is what tells "not used today" from "broken".
func TestRebuildAgentsTable_ASessionOnlyAgentShowsDashesForTheWindow(t *testing.T) {
	m := morningAfterFixture()
	// Other's one session, the default bucket, gets an age no named agent shares, so a Other that
	// matched every session or none would read "just now" or a dash.
	m.sessions[4].UpdatedAt = time.Now().Add(-5 * time.Hour)
	m.rebuildAgentsTable()
	rows := m.agentsTbl.Rows()
	var oc, other []string
	for _, r := range rows {
		switch r[0] {
		case "opencode":
			oc = r
		case otherAgents:
			other = r
		}
	}
	if other == nil || other[2] != "5h ago" {
		t.Errorf("Other row = %v, want LAST SEEN %q from the session that names no agent", other, "5h ago")
	}
	if oc == nil {
		t.Fatalf("no opencode row in %v", rows)
	}
	if want := []string{"opencode", "1", "18h ago", emptyCell}; !slices.Equal(oc, want) {
		t.Errorf("opencode row = %v, want %v", oc, want)
	}
	if cc := rows[1]; cc[0] != "claude-code" || cc[1] != "2" || cc[2] != "just now" || cc[3] != "$113.63" {
		t.Errorf("claude-code row = %v, want its figures untouched", cc)
	}
}

// A session over a day old shows as a date, and a two-digit day is relTime's widest form: at 11
// columns "Oct 23 14:02" lost its last minute digit.
func TestRebuildAgentsTable_LastSeenFitsATwoDigitDay(t *testing.T) {
	m := morningAfterFixture()
	m.sessions[3].UpdatedAt = time.Date(2025, time.October, 23, 14, 2, 0, 0, time.Local)
	m.rebuildAgentsTable()
	for _, r := range m.agentsTbl.Rows() {
		if r[0] != "bob-shell" {
			continue
		}
		// AGENT, SESSIONS, LAST SEEN: the fixture runs no router, so SERVER is not inserted.
		if r[2] != "Oct 23 14:02" {
			t.Fatalf("bob-shell LAST SEEN = %q, want the date form", r[2])
		}
		if w, n := sessionsColumnWidth(m.agentsTbl.Columns(), "LAST SEEN"), utf8.RuneCountInString(r[2]); w < n {
			t.Errorf("LAST SEEN is %d columns for the %d of %q", w, n, r[2])
		}
		return
	}
	t.Fatalf("no bob-shell row in %v", m.agentsTbl.Rows())
}

// THE REPORTED DEFECT, driven the way it was met: `A` on the sessions pane, the reply carrying one
// agent for today, and the list holding three. The pane opens, says nothing, and Enter on OpenCode
// narrows the list to OpenCode's session.
func TestAgentsPane_APressOpensWhenOnlySessionsNameTheSecondAgent(t *testing.T) {
	m := morningAfterFixture()
	cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'A'}})
	if cmd == nil {
		t.Fatal("`A` returned no fetch command")
	}
	failed, ok := cmd().(agentRowsLoadedMsg)
	if !ok {
		t.Fatal("`A`'s command did not produce an agentRowsLoadedMsg")
	}
	updated, _ := m.Update(agentRowsLoadedMsg{rows: m.agents, open: agentsOpenOnPress, from: failed.from})
	m = updated.(*model)
	if m.pane != paneAgents {
		t.Fatalf("`A` did not open the pane: pane = %v, flash %q", m.pane, m.flash)
	}
	if m.flash != "" {
		t.Errorf("`A` opened the pane and also flashed %q", m.flash)
	}
	// All agents, claude-code, bob-shell, opencode.
	m.agentsTbl.SetCursor(3)
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.agentScope != "opencode" || m.pane != paneSessions {
		t.Fatalf("scope %q on %v, want opencode on the sessions pane", m.agentScope, m.pane)
	}
	if got := strings.Join(m.sessionRowIDs, ","); got != "oc-1" {
		t.Errorf("scoped to opencode: rows %q, want oc-1 alone", got)
	}
}

// The pane opens on two agents from sessions alone, with nothing spent in the window: the morning
// before any agent has made a request. The body is the table, not "(no agent traffic …)".
func TestAgentsPane_OpensOnSessionsAloneAndShowsTheTable(t *testing.T) {
	m := morningAfterFixture()
	m.agents = nil
	m.enterAgents(paneSessions)
	if view := m.View(); strings.Contains(view, "no agent traffic") || !strings.Contains(view, "opencode") {
		t.Errorf("the pane did not render the session-only rows:\n%s", view)
	}
}

// #1231: one recognised agent, the default bucket and an unrecognised client's session. The
// startup gate still skips the picker, but `A` opens it — and it has more than one row, because
// the sessions that name no recognised agent belong to Other.
func TestAgentsPane_OneAgentAndOtherSessions_StartupSkipsButAOpens(t *testing.T) {
	m := morningAfterFixture()
	m.sessions = []session.SessionSummary{
		{ID: "claude-1", UpdatedAt: time.Now(), Agent: "claude-code"},
		{ID: "3981fd731b31c9d1aff4e17e6556f5c6", UpdatedAt: time.Now(), Agent: "curl/8.7.1"},
		{ID: session.DefaultSessionID, UpdatedAt: time.Now()},
	}
	updated, _ := m.Update(agentRowsLoadedMsg{rows: m.agents, open: agentsOpenAtStartup})
	m = updated.(*model)
	if m.pane != paneSessions || m.flash != "" {
		t.Errorf("startup landed on %v with flash %q, want sessions and silence", m.pane, m.flash)
	}
	updated, _ = m.Update(agentRowsLoadedMsg{rows: m.agents, open: agentsOpenOnPress, from: paneSessions})
	m = updated.(*model)
	if m.pane != paneAgents || m.flash != "" {
		t.Fatalf("`A` landed on %v with flash %q, want the pane and silence", m.pane, m.flash)
	}
	if got, want := pickerLabels(m.pickerRows()), []string{"claude-code", otherAgents}; !slices.Equal(got, want) {
		t.Errorf("picker rows = %v, want %v", got, want)
	}
	if view := m.View(); !strings.Contains(view, otherAgents) {
		t.Errorf("the pane does not show the Other row:\n%s", view)
	}
}

// The startup gate's reply can land before the first session list, when the list cannot yet vote.
// A decline then is provisional: the first list gets one look, and opens the picker if it names
// the second agent.
func TestAgentsPane_StartupLooksAgainWhenTheFirstSessionListLands(t *testing.T) {
	m := morningAfterFixture()
	listed := m.sessions
	m.sessions = nil
	updated, _ := m.Update(agentRowsLoadedMsg{rows: m.agents, open: agentsOpenAtStartup})
	m = updated.(*model)
	if m.pane != paneSessions {
		t.Fatalf("startup on one agent and no sessions landed on %v", m.pane)
	}
	updated, _ = m.Update(sessionsLoadedMsg(listed))
	m = updated.(*model)
	if m.pane != paneAgents {
		t.Fatalf("the first session list named two more agents and the picker did not open: %v", m.pane)
	}
	if m.previousPane != paneNone || m.flash != "" {
		t.Errorf("previousPane %v, flash %q; want the gate's paneNone and silence", m.previousPane, m.flash)
	}
}

// ONE look, not a watch. A list that arrives later naming a new agent does not pull the operator
// into the picker mid-session; `A` is there for that.
func TestAgentsPane_StartupLooksAtOneSessionListOnly(t *testing.T) {
	m := morningAfterFixture()
	listed := m.sessions
	m.sessions = nil
	updated, _ := m.Update(agentRowsLoadedMsg{rows: m.agents, open: agentsOpenAtStartup})
	m = updated.(*model)
	updated, _ = m.Update(sessionsLoadedMsg(listed[:2]))
	m = updated.(*model)
	if m.pane != paneSessions {
		t.Fatalf("a first list of Claude Code alone opened the picker: %v", m.pane)
	}
	updated, _ = m.Update(sessionsLoadedMsg(listed))
	m = updated.(*model)
	if m.pane != paneSessions {
		t.Errorf("a later list opened the picker: %v", m.pane)
	}
}

// The second look interrupts Sessions and nothing else, for the reason the first one does.
func TestAgentsPane_StartupSecondLookLeavesAnOperatorWhoHasMoved(t *testing.T) {
	m := morningAfterFixture()
	listed := m.sessions
	m.sessions = nil
	updated, _ := m.Update(agentRowsLoadedMsg{rows: m.agents, open: agentsOpenAtStartup})
	m = updated.(*model)
	m.pane = paneUsage
	updated, _ = m.Update(sessionsLoadedMsg(listed))
	m = updated.(*model)
	if m.pane != paneUsage {
		t.Errorf("the second look moved an operator on Usage to %v", m.pane)
	}
}

// The look that was owed belongs to the connection that declined. Backing out to the pods and
// entering another pod must not spend it on the next pod's first list, judged against the rows the
// last pod reported; that pod's own gate runs when its own rows arrive.
func TestAgentsPane_StartupSecondLookDoesNotOutliveItsConnection(t *testing.T) {
	m := morningAfterFixture()
	listed := m.sessions
	m.sessions = nil
	m.parentCtx = context.Background()
	m.ctx, m.cancel = context.WithCancel(m.parentCtx)
	defer func() { m.cancel() }()
	updated, _ := m.Update(agentRowsLoadedMsg{rows: m.agents, open: agentsOpenAtStartup})
	m = updated.(*model)
	m.backToPodsPane()
	m.client, m.pane = deadClient(), paneSessions
	updated, _ = m.Update(sessionsLoadedMsg(listed))
	m = updated.(*model)
	if m.pane != paneSessions {
		t.Errorf("the previous pod's owed look opened the picker on the next pod's list: %v", m.pane)
	}
}

// An agent's rows are what sessionAgent names, pending buckets included, less those folded into a
// session: LAST SEEN is taken over the same rows SESSIONS counts. A Codex daemon started before
// `enable` lists only its pending bucket, which is the case LAST SEEN exists to tell from "broken".
func TestRebuildAgentsTable_LastSeenCoversTheRowsSessionsCounts(t *testing.T) {
	m := morningAfterFixture()
	now := time.Now()
	m.sessions = append(m.sessions,
		session.SessionSummary{ID: "pending:codex@123.456", UpdatedAt: now.Add(-2 * time.Hour)},
		session.SessionSummary{ID: "pending:claude-code@9.9", UpdatedAt: now.Add(-3 * time.Hour),
			LinkedTo: []session.PendingLink{{Session: "claude-1"}}, LinkedAll: true},
	)
	m.sessions[0].UpdatedAt, m.sessions[1].UpdatedAt = now.Add(-5*time.Hour), now.Add(-5*time.Hour)
	m.rebuildAgentsTable()
	got := map[string][]string{}
	for _, r := range m.agentsTbl.Rows() {
		got[r[0]] = r
	}
	if r := got["codex"]; len(r) < 3 || r[1] != "1" || r[2] != relTime(now, now.Add(-2*time.Hour)) {
		t.Errorf("codex row = %v, want SESSIONS 1 and LAST SEEN from its pending bucket", r)
	}
	if r := got["claude-code"]; len(r) < 3 || r[1] != "2" || r[2] != relTime(now, now.Add(-5*time.Hour)) {
		t.Errorf("claude-code row = %v, want LAST SEEN from its own sessions, not the folded bucket", r)
	}
}

// Other counts and dates the rows its list shows: a bucket folded into a session is hidden there,
// so it adds to neither SESSIONS nor LAST SEEN. Reached when the proxy knows an agent this agentop
// does not, so both the session and its bucket land under Other.
func TestRebuildAgentsTable_OtherLeavesOutFoldedBuckets(t *testing.T) {
	m := morningAfterFixture()
	now := time.Now()
	m.sessions[4].UpdatedAt = now.Add(-5 * time.Hour)
	m.sessions = append(m.sessions,
		session.SessionSummary{ID: "new-1", UpdatedAt: now.Add(-6 * time.Hour), Agent: "newagent"},
		session.SessionSummary{ID: "pending:newagent@7.7", UpdatedAt: now.Add(-1 * time.Hour),
			LinkedTo: []session.PendingLink{{Session: "new-1"}}, LinkedAll: true},
	)
	m.rebuildAgentsTable()
	for _, r := range m.agentsTbl.Rows() {
		if r[0] != otherAgents {
			continue
		}
		if r[1] != "2" || r[2] != "5h ago" {
			t.Errorf("Other row = %v, want SESSIONS 2 and LAST SEEN 5h ago: the folded bucket is not listed", r)
		}
		return
	}
	t.Fatalf("no Other row in %v", m.agentsTbl.Rows())
}
