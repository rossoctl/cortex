package tui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// A server that predates agent= ignores it and answers for every agent; the fetch then narrows
// client-side, from the group=agent series. A server that applied it is used as it answered, in
// one request.
func TestFetchUsageScoped_NarrowsWhateverTheServerDid(t *testing.T) {
	var hits atomic.Int32
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(usageSnapshotJSON))
	}))
	defer old.Close()
	snap, err := fetchUsageScoped(context.Background(), apiclient.New(old.URL), "today", 0, "bob-shell/2.0.5", usage.GroupNone)
	if err != nil || snap.Totals.Requests != 8 {
		t.Fatalf("old server: requests = %v (err %v), want Bob's 8 narrowed client-side", snap, err)
	}

	hits.Store(0)
	current := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"window":"today","agent":"` + r.URL.Query().Get("agent") + `","totals":{"requests":3}}`))
	}))
	defer current.Close()
	snap, err = fetchUsageScoped(context.Background(), apiclient.New(current.URL), "today", 0, "bob-shell/2.0.5", usage.GroupNone)
	if err != nil || snap.Totals.Requests != 3 || hits.Load() != 1 {
		t.Errorf("current server: requests = %v in %d requests (err %v), want its own 3 in one", snap, hits.Load(), err)
	}
}

// The band asks for the scoped agent, and asks exactly as before without one.
func TestFetchSpendSpan_SendsTheScopeAndNothingWithoutOne(t *testing.T) {
	var query atomic.Value
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query.Store(r.URL.RawQuery)
		_, _ = w.Write([]byte(`{"window":"today","agent":"` + r.URL.Query().Get("agent") + `","totals":{}}`))
	}))
	defer ts.Close()
	for _, scope := range []string{"", "bob-shell/2.0.5"} {
		m := &model{client: apiclient.New(ts.URL), agentScope: scope}
		m.fetchSpendSpan(spanToday)()
		got, _ := query.Load().(string)
		if has := strings.Contains(got, "agent="); has != (scope != "") {
			t.Errorf("scope %q: query %q", scope, got)
		}
	}
}

func sessionsFixture() []session.SessionSummary {
	now := time.Now()
	return []session.SessionSummary{
		{ID: "claude-1", UpdatedAt: now, Agent: "claude-code/2.1.284"},
		{ID: "task-1", UpdatedAt: now, Agent: "bob-shell/2.0.5"},
		{ID: session.DefaultSessionID, UpdatedAt: now},
	}
}

// Scoped, the sessions pane lists that agent's sessions only; the buckets that belong to no one
// agent go too.
func TestSessionsPane_ScopeListsOnlyThatAgentsSessions(t *testing.T) {
	m := &model{width: 200, sessions: sessionsFixture(), agentScope: "bob-shell/2.0.5",
		events: map[string][]pipeline.SessionEvent{"evicted-1": {{}}}}
	m.rebuildSessionsTable()
	if got := strings.Join(m.sessionRowIDs, ","); got != "task-1" {
		t.Errorf("scoped to bob: rows %q, want task-1 alone", got)
	}
	m.agentScope = ""
	m.rebuildSessionsTable()
	if got := strings.Join(m.sessionRowIDs, ","); got != "claude-1,task-1,default,evicted-1" {
		t.Errorf("unscoped: rows %q, want every session", got)
	}
}

// AGENT appears only when two agents are listed, so a single-agent layout is today's.
func TestSessionsPane_AgentColumnOnlyWhenTwoAgentsAreListed(t *testing.T) {
	m := &model{width: 200, sessions: sessionsFixture()}
	m.rebuildSessionsTable()
	if sessionsColumnWidth(m.sessionsTbl.Columns(), "AGENT") == 0 {
		t.Errorf("two agents listed and no AGENT column: %v", m.sessionsTbl.Columns())
	}
	one := &model{width: 200, sessions: sessionsFixture()[:1]}
	one.rebuildSessionsTable()
	if want := alignSessionsHeaders(fitTableColumns(sessionsColumnsFor(200), 200)); !sameColumns(one.sessionsTbl.Columns(), want) {
		t.Errorf("one agent: columns %v, want today's %v", one.sessionsTbl.Columns(), want)
	}
}

// Under a scope the drawer's agent axis would be one row, so `a` never lands on it.
func TestCycleSpendAxis_SkipsTheAgentAxisUnderAScope(t *testing.T) {
	m := &model{agentScope: "bob-shell/2.0.5"}
	if slices.Contains(m.spendAxes(), usage.GroupAgent) {
		t.Errorf("the hint's axis cycle %v lists the agent axis under a scope", m.spendAxes())
	}
	m.spend.groupIdx = slices.Index(spendDrawerAxes, usage.GroupAgent)
	if m.spendAxis() == usage.GroupAgent {
		t.Error("a scope set while the drawer was on the agent axis still asks for it")
	}
	shown := m.spendAxis()
	m.cycleSpendAxis()
	if m.spendAxis() == shown {
		t.Errorf("`a` from the agent index left the drawer on %q, the axis it already showed", shown)
	}
	for range 2 * len(spendDrawerAxes) {
		m.cycleSpendAxis()
		if m.spend.axis() == usage.GroupAgent {
			t.Fatalf("the axis cycle reached %q under a scope", usage.GroupAgent)
		}
	}
}

// Every pane under the scoped band says it is scoped, and a scope no session belongs to says so
// rather than drawing an empty table.
func TestSessionsPane_ScopedTitleAndTheEmptyScope(t *testing.T) {
	m := fitModel(t, paneSessions, 120, 40, nil)
	m.sessions = sessionsFixture()
	m.agentScope = "Claude-User (claude-code/2.1.284; +https://support.anthropic.com/)"
	m.rebuildSessionsTable()
	view := m.paneView()
	if title := strings.SplitN(view, "\n", 2)[0]; !strings.Contains(title, "agent=Claude-User") {
		t.Errorf("title %q does not name the scope", title)
	}
	if !strings.Contains(view, "no session belongs to") {
		t.Errorf("a raw User-Agent scope matched sessions or said nothing:\n%s", view)
	}
}

// SESSIONS joins the AGENTS pane only once a session names its agent, and counts per agent.
func TestAgentsPane_SessionsColumnOnlyOnceSessionsNameAgents(t *testing.T) {
	m := &model{agentsTbl: newAgentsTable(), agents: []agentRow{{label: "bob-shell/2.0.5"}, {label: "node"}}}
	m.rebuildAgentsTable()
	if !sameColumns(m.agentsTbl.Columns(), fitTableColumns(agentsColumns(m.agentsWindowLabel()), 0)) {
		t.Errorf("no session names an agent and the columns changed: %v", m.agentsTbl.Columns())
	}
	m.sessions = sessionsFixture()
	m.rebuildAgentsTable()
	// The fixture's default bucket names no agent, so it is Other's, and Other gets a row. Its
	// Claude Code session is a recognised agent's with nothing in the window, so that agent gets a
	// row too, after the window's; see agentChoices. Row 0 is All agents.
	if rows := m.agentsTbl.Rows(); len(rows) != 5 || rows[1][1] != "1" || rows[2][1] != emptyCell ||
		rows[3][0] != "claude-code/2.1.284" || rows[3][1] != "1" ||
		rows[4][0] != otherAgents || rows[4][1] != "1" {
		t.Errorf("SESSIONS cells = %v, want bob's 1, node's dash, claude-code's 1 and Other's 1", rows)
	}
}

// Enter on an agent restarts the band under the new scope: the next band request names it.
func TestAgentsPane_EnterRestartsTheBandUnderTheScope(t *testing.T) {
	var sawAgent atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("agent") == "bob-shell/2.0.5" {
			sawAgent.Store(true)
		}
		_, _ = w.Write([]byte(`{"window":"today","totals":{}}`))
	}))
	defer ts.Close()
	m := fitModel(t, paneAgents, 120, 40, nil)
	m.client = apiclient.New(ts.URL)
	m.agents = []agentRow{{label: "bob-shell/2.0.5"}, {label: "claude-code/2.1.284"}}
	m.rebuildAgentsTable()
	m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.agentScope != "bob-shell/2.0.5" || cmd == nil {
		t.Fatalf("scope %q, cmd %v", m.agentScope, cmd)
	}
	runBatchNoWait(cmd)
	if !sawAgent.Load() {
		t.Error("no band request named the new scope")
	}
}

// runBatchNoWait is runBatch for a batch holding ticks: each member runs on its own goroutine and
// the test waits a moment rather than for a tick that blocks for seconds.
func runBatchNoWait(cmd tea.Cmd) {
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, c := range batch {
				if c == nil {
					continue
				}
				go c()
			}
			time.Sleep(200 * time.Millisecond)
		}
	case <-time.After(time.Second):
	}
}

// Two versions of one agent are two AGENTS rows, and each scopes to its own sessions: the
// sessions list and the SESSIONS column match on the label the band sends as agent=.
func TestAgentScope_TwoVersionsOfOneAgentAreTwoScopes(t *testing.T) {
	now := time.Now()
	m := &model{width: 200, agentsTbl: newAgentsTable(), sessions: []session.SessionSummary{
		{ID: "claude-a", UpdatedAt: now, Agent: "claude-code/2.1.284"},
		{ID: "claude-b", UpdatedAt: now, Agent: "claude-code/2.1.285"},
		{ID: "task-1", UpdatedAt: now, Agent: "bob-shell/2.0.5"},
	}, agents: []agentRow{{label: "claude-code/2.1.284"}, {label: "claude-code/2.1.285"},
		{label: "bob-shell/2.0.5"}, {label: "claude-code/2.1.283"}}}
	m.rebuildAgentsTable()
	var cells []string
	for _, r := range m.agentsTbl.Rows()[1:] {
		cells = append(cells, r[1])
	}
	if want := []string{"1", "1", "1", emptyCell}; !slices.Equal(cells, want) {
		t.Errorf("SESSIONS cells = %v, want %v: one session per version, none for 2.1.283", cells, want)
	}
	m.agentScope = "claude-code/2.1.284"
	m.rebuildSessionsTable()
	if got := strings.Join(m.sessionRowIDs, ","); got != "claude-a" {
		t.Errorf("scoped to claude-code/2.1.284: rows %q, want claude-a alone", got)
	}
}

// Enter scopes to the row under the cursor after the column set changes, whichever order the
// sessions and the agent rows arrive in, and after a resize.
func TestAgentsPane_EnterScopesAfterTheColumnsChange(t *testing.T) {
	rows := []agentRow{{label: "bob-shell/2.0.5"}, {label: "claude-code/2.1.284"}}
	for name, arrive := range map[string]func(m *model){
		"sessions after rows": func(m *model) {
			m.agents = rows
			m.rebuildAgentsTable()
			m.Update(sessionsLoadedMsg(sessionsFixture()))
		},
		"rows after sessions": func(m *model) {
			m.sessions = sessionsFixture()
			m.Update(agentRowsLoadedMsg{rows: rows, open: agentsOpenNever, window: usage.WindowToday})
		},
		"resize": func(m *model) {
			m.sessions = sessionsFixture()
			m.agents = rows
			m.rebuildAgentsTable()
			m.Update(tea.WindowSizeMsg{Width: 90, Height: 40})
		},
	} {
		m := fitModel(t, paneAgents, 120, 40, nil)
		arrive(m)
		if m.pane != paneAgents {
			t.Fatalf("%s: left the agents pane", name)
		}
		m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
		m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
		if m.agentScope != "bob-shell/2.0.5" {
			t.Errorf("%s: Enter scoped to %q, want the row under the cursor", name, m.agentScope)
		}
	}
}

// A server that names no session's agent — one that predates the field — still lists every
// session under a scope, and says the list is not narrowed rather than that it is empty.
func TestSessionsPane_AnOlderServerListsEverySessionUnderAScope(t *testing.T) {
	now := time.Now()
	old := []session.SessionSummary{{ID: "claude-1", UpdatedAt: now}, {ID: "task-1", UpdatedAt: now}}
	m := &model{width: 200, sessions: old, agentScope: "bob-shell/2.0.5"}
	m.rebuildSessionsTable()
	if got := strings.Join(m.sessionRowIDs, ","); got != "claude-1,task-1" {
		t.Errorf("rows %q, want every session", got)
	}
	v := fitModel(t, paneSessions, 160, 40, nil)
	v.sessions, v.agentScope = old, "bob-shell/2.0.5"
	v.rebuildSessionsTable()
	if view := v.paneView(); strings.Contains(view, "no session belongs to") {
		t.Errorf("an older server's sessions read as belonging to no agent:\n%s", view)
	}
	if footer := v.footerView(); !strings.Contains(footer, "list not scoped") {
		t.Errorf("footer %q does not say the list is not narrowed", footer)
	}
}

// A scoped drawer the server answered without the breakdown asked for says which one, in the
// span it covers, instead of heading an empty column.
func TestSpendDrawer_SaysWhenOneAgentHasNoBreakdown(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"window":"1h0m0s","group":"none","agent":"` + r.URL.Query().Get("agent") + `","totals":{}}`))
	}))
	defer ts.Close()
	m := fitModel(t, paneSessions, 120, 50, nil)
	m.client = apiclient.New(ts.URL)
	m.agentScope = "bob-shell/2.0.5"
	m.spend.expanded = true
	m.Update(m.fetchSpendDrawer()())
	if view := m.paneView(); !strings.Contains(view, "no model breakdown for one agent") {
		t.Errorf("the drawer does not say the model breakdown is unavailable:\n%s", view)
	}
}

// A scoped drawer the server DID break down — a recognised agent's last hour, served from its own
// ring — heads the column with the axis and lists the agent's models, with no note.
func TestSpendDrawer_ShowsOneAgentsBreakdownWhenServed(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		one := usage.Counts{Requests: 1, CostMicros: 760_000, PricedRequests: 1, PriceableRequests: 1}
		_ = json.NewEncoder(w).Encode(usage.Snapshot{
			Window: "1h0m0s", Group: usage.GroupModel, Agent: r.URL.Query().Get("agent"),
			Totals: one, Priced: true,
			Buckets: []usage.Bucket{{At: time.Now(), Counts: one, Series: map[string]usage.Counts{"claude-opus-5": one}}},
		})
	}))
	defer ts.Close()
	m := fitModel(t, paneSessions, 120, 50, nil)
	m.client = apiclient.New(ts.URL)
	m.agentScope = "claude-code"
	m.spend.expanded = true
	m.Update(m.fetchSpendDrawer()())
	view := m.paneView()
	if !strings.Contains(view, "BY MODEL") || !strings.Contains(view, "claude-opus-5") || strings.Contains(view, "breakdown for one agent") {
		t.Errorf("the drawer does not show the served model breakdown:\n%s", view)
	}
}

// A cached pending bucket the server no longer lists is still retained history — after an
// eviction, an expiry or a proxy restart — so it is listed.
func TestSessionsPane_ListsACachedPendingBucket(t *testing.T) {
	m := &model{width: 200, sessions: sessionsFixture()[:1], events: map[string][]pipeline.SessionEvent{
		session.PendingSessionID("bob-shell"): {{}},
	}}
	m.rebuildSessionsTable()
	if got := strings.Join(m.sessionRowIDs, ","); got != "claude-1,pending:bob-shell" {
		t.Errorf("rows %q, want the cached pending history listed", got)
	}
}

// The scope marker survives the events and detail titles being fitted to the terminal.
func TestSessionHeader_KeepsTheScopeOnALongTitle(t *testing.T) {
	for _, p := range []paneID{paneEvents, paneDetail} {
		m := fitModel(t, p, 80, 40, nil)
		m.sessions = []session.SessionSummary{{ID: "sess-1", Title: strings.Repeat("a long title ", 20)}}
		m.agentScope = "bob-shell/2.0.5"
		title := strings.SplitN(m.paneView(), "\n", 2)[0]
		if !strings.Contains(title, "agent=bob-shell/2.0.5") || lipgloss.Width(title) > 80 {
			t.Errorf("pane %d title %q (%d wide): want the scope within 80 columns", p, title, lipgloss.Width(title))
		}
	}
}

// The AGENT cell is a server string and is sanitised like every other one on the pane.
func TestSessionsPane_SanitisesTheAgentCell(t *testing.T) {
	now := time.Now()
	m := &model{width: 200, sessions: []session.SessionSummary{
		{ID: "claude-1", UpdatedAt: now, Agent: "claude-code/2.1.284"},
		{ID: "x-1", UpdatedAt: now, Agent: "evil\x1b[2J/1"},
	}}
	m.rebuildSessionsTable()
	for _, r := range m.sessionsTbl.Rows() {
		for _, c := range r {
			if strings.ContainsRune(c, 0x1b) {
				t.Errorf("a cell carries an escape: %q", c)
			}
		}
	}
}
