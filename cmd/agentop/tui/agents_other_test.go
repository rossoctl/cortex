package tui

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// otherUsageJSON is the AGENTS window #1210 was filed from, cut down: one recognised agent and the
// IBM Bob IDE's three raw User-Agents, as a proxy that did not yet recognise the IDE served them.
const otherUsageJSON = `{
  "window":"today","bucketSeconds":60,"group":"agent","currencies":["USD"],
  "seriesCurrencies":{"claude-code":["USD"],"IBM Bob/2.2.1":null,
    "ai-sdk/openai-compatible/3.0.36 ai-sdk/provider-utils/5.0.29 runtime/node.js/v24.15.0 IBM Bob/2.":null,
    "ai/7.0.16 ai-sdk/provider-utils/5.0.29 runtime/node.js/v24.15.0 IBM Bob/2.2.1":null},
  "buckets":[
    {"at":"2026-10-01T10:00:00Z","requests":40,"tokens":5000,
     "series":{
       "claude-code":{"requests":17,"tokens":1000,"pricedRequests":17,"costMicros":900},
       "IBM Bob/2.2.1":{"requests":2},
       "ai-sdk/openai-compatible/3.0.36 ai-sdk/provider-utils/5.0.29 runtime/node.js/v24.15.0 IBM Bob/2.":{"requests":10,"tokens":3000},
       "ai/7.0.16 ai-sdk/provider-utils/5.0.29 runtime/node.js/v24.15.0 IBM Bob/2.2.1":{"requests":11,"tokens":1000}}}],
  "totals":{"requests":40,"tokens":5000}}`

func otherUsageServer(t *testing.T, query *atomic.Value) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if query != nil {
			query.Store(r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(otherUsageJSON))
	}))
	t.Cleanup(ts.Close)
	return ts
}

func otherUsageSnapshot(t *testing.T) *usage.Snapshot {
	t.Helper()
	snap, err := apiclient.New(otherUsageServer(t, nil).URL).GetUsageWindow(context.Background(), "today", 0, "", usage.GroupAgent)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// Every unrecognised User-Agent is one Other series, the recognised agent keeps its own, and
// nothing about the window changes: the series are regrouped, not recounted.
func TestFoldOtherAgents_PoolsEveryUnrecognisedSeries(t *testing.T) {
	snap := otherUsageSnapshot(t)
	folded := foldOtherAgents(snap)
	series := folded.Buckets[0].Series
	if got := slices.Sorted(maps.Keys(series)); !slices.Equal(got, []string{otherAgents, "claude-code"}) {
		t.Fatalf("series = %v, want claude-code and Other", got)
	}
	if o := series[otherAgents]; o.Requests != 23 || o.Tokens != 4000 {
		t.Errorf("Other = %+v, want the three Bob rows' 23 requests and 4000 tokens", o)
	}
	if folded.Totals != snap.Totals || folded.Buckets[0].Counts != snap.Buckets[0].Counts {
		t.Errorf("the fold changed a window figure: %+v vs %+v", folded.Totals, snap.Totals)
	}
	// A copy: the caller's snapshot still has its four series.
	if len(snap.Buckets[0].Series) != 4 {
		t.Errorf("the fold mutated its input: %v", snap.Buckets[0].Series)
	}
}

// The cap's "(other)" band, untagged traffic and a versioned label all land where the predicate
// says: the first two are no agent's, the third is Claude Code from a server older than the fold.
func TestFoldOtherAgents_TheCapBandAndUntaggedTrafficAreOthers(t *testing.T) {
	snap := &usage.Snapshot{Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
		"(other)":                   {Requests: 1},
		pipeline.UnknownClientLabel: {Requests: 2},
		"claude-code/2.1.285":       {Requests: 4},
	}}}}
	series := foldOtherAgents(snap).Buckets[0].Series
	if series[otherAgents].Requests != 3 || series["claude-code/2.1.285"].Requests != 4 || len(series) != 2 {
		t.Errorf("series = %v, want (other)+unknown as Other's 3 and claude-code/2.1.285 kept", series)
	}
}

// Other's units are its members' units together; a member the producer named none for leaves them
// unknown, which is the window's list; and a producer that sends no cross-tabulation still sends
// none.
func TestFoldOtherAgents_UnitsFollowTheSeries(t *testing.T) {
	bucket := []usage.Bucket{{Series: map[string]usage.Counts{"claude-code": {}, "a": {}, "b": {}}}}
	for _, tc := range []struct {
		name string
		in   map[string][]string
		want []string
		none bool
	}{
		{"union", map[string][]string{"claude-code": {"USD"}, "a": {"credits"}, "b": {"USD"}}, []string{"USD", "credits"}, false},
		{"a member unlabelled", map[string][]string{"claude-code": {"USD"}, "a": {"credits"}}, []string{"EUR", "USD"}, false},
		{"older producer", nil, nil, true},
	} {
		got := foldOtherAgents(&usage.Snapshot{Buckets: bucket, Currencies: []string{"EUR", "USD"}, SeriesCurrencies: tc.in})
		if tc.none {
			if got.SeriesCurrencies != nil {
				t.Errorf("%s: SeriesCurrencies = %v, want none", tc.name, got.SeriesCurrencies)
			}
			continue
		}
		if !slices.Equal(got.SeriesCurrencies[otherAgents], tc.want) || !slices.Equal(got.SeriesCurrencies["claude-code"], []string{"USD"}) {
			t.Errorf("%s: SeriesCurrencies = %v, want Other %v and claude-code's own", tc.name, got.SeriesCurrencies, tc.want)
		}
	}
}

// The picker #1210 described is two rows: Claude Code, then Other. Other is last even when it
// out-spends a named agent, because it is the remainder rather than a rival.
func TestAgentRowsFromSnapshot_OtherIsOneRowAndLast(t *testing.T) {
	rows := agentRowsFromSnapshot(otherUsageSnapshot(t))
	if len(rows) != 2 || rows[0].label != "claude-code" || rows[1].label != otherAgents {
		t.Fatalf("rows = %+v, want claude-code then Other", rows)
	}
	if rows[1].Tokens != 4000 {
		t.Errorf("Other carries %d tokens, want the 4000 the IDE's rows held between them", rows[1].Tokens)
	}

	rich := agentRowsFromBuckets([]usage.Bucket{{Series: map[string]usage.Counts{
		otherAgents: {CostMicros: 9_000_000}, "bob-shell": {CostMicros: 1}, "claude-code": {CostMicros: 2},
	}}})
	if got := []string{rich[0].label, rich[1].label, rich[2].label}; !slices.Equal(got, []string{"claude-code", "bob-shell", otherAgents}) {
		t.Errorf("order = %v, want the named agents by cost and Other last", got)
	}
}

// otherSessionsFixture is a proxy with two recognised agents, a session from an agent it does not
// recognise, the default bucket, and a session only agentop's cache still holds.
func otherSessionsFixture() *model {
	now := time.Now()
	return &model{width: 200, agentsTbl: newAgentsTable(),
		sessions: []session.SessionSummary{
			{ID: "claude-1", UpdatedAt: now, Agent: "claude-code"},
			{ID: "task-1", UpdatedAt: now, Agent: "bob-shell"},
			{ID: "3981fd731b31c9d1aff4e17e6556f5c6", UpdatedAt: now},
			{ID: session.DefaultSessionID, UpdatedAt: now},
		},
		events: map[string][]pipeline.SessionEvent{"evicted-1": {{}}},
		agents: []agentRow{{label: "claude-code"}, {label: "bob-shell"}, {label: otherAgents}},
	}
}

// THE PROPERTY #1210 WAS MISSING: every row the picker offers scopes the sessions list to a
// disjoint part of it, and the parts together are the whole list. Before Other, the IDE's session
// and the default bucket belonged to no row, so any pick hid them.
func TestAgentScope_EveryListedSessionBelongsToExactlyOneRow(t *testing.T) {
	m := otherSessionsFixture()
	m.rebuildSessionsTable()
	all := slices.Clone(m.sessionRowIDs)
	var seen []string
	for _, a := range m.pickerRows() {
		m.agentScope = a.label
		m.rebuildSessionsTable()
		for _, id := range m.sessionRowIDs {
			if slices.Contains(seen, id) {
				t.Errorf("%s is listed under %q and under an earlier row", id, a.label)
			}
			seen = append(seen, id)
		}
	}
	slices.Sort(all)
	slices.Sort(seen)
	if !slices.Equal(seen, all) {
		t.Errorf("the rows together list %v, want every session %v", seen, all)
	}

	m.agentScope = otherAgents
	m.rebuildSessionsTable()
	if got := strings.Join(m.sessionRowIDs, ","); got != "3981fd731b31c9d1aff4e17e6556f5c6,default,evicted-1" {
		t.Errorf("Other lists %q, want the unrecognised session, the default bucket and the cached one", got)
	}
	m.rebuildAgentsTable()
	if cell := m.agentsTbl.Rows()[3]; cell[0] != otherAgents || cell[1] != "3" {
		t.Errorf("Other's row = %v, want SESSIONS 3, what its scope lists", cell)
	}
}

// Sessions alone put an Other row in a picker that opens anyway, and never open one: the default
// bucket names no agent and exists on almost every proxy, so counting it would show a single-agent
// user the picker at every startup.
func TestPickerRows_SessionsAloneShowOtherButNeverOpenThePane(t *testing.T) {
	m := otherSessionsFixture()
	m.agents = []agentRow{{label: "claude-code"}}
	// Without Bob's session, which is a second recognised agent and opens the pane by itself (see
	// TestAgentsPane_APressOpensWhenOnlySessionsNameTheSecondAgent). What is left is one agent
	// and the sessions that belong to Other.
	m.sessions = slices.DeleteFunc(slices.Clone(m.sessions), func(s session.SessionSummary) bool {
		return s.Agent == "bob-shell"
	})
	if agentsPaneApplies(m.agentChoices()) {
		t.Fatal("one recognised agent opens the pane")
	}

	m.agents = []agentRow{{label: "claude-code"}, {label: "bob-shell"}}
	rows := m.pickerRows()
	if len(rows) != 3 || rows[2].label != otherAgents {
		t.Fatalf("picker rows = %+v, want Other appended for the sessions that name no agent", rows)
	}
	if len(m.agents) != 2 {
		t.Errorf("pickerRows changed m.agents to %+v; the gate would count the extra row", m.agents)
	}
	m.rebuildAgentsTable()
	m.agentsTbl.SetCursor(3)
	if got, _ := m.selectedAgentScope(); got != otherAgents {
		t.Errorf("cursor on the appended row selects %q, want Other", got)
	}

	// No session names an agent: the list ignores every scope, so Other would scope nothing.
	m.sessions = []session.SessionSummary{{ID: "old-1"}}
	if rows := m.pickerRows(); len(rows) != 2 {
		t.Errorf("picker rows = %+v against a server that names no agent, want no Other", rows)
	}
}

// Enter on Other scopes to it and lands on a list holding the session #1210 could not reach.
func TestAgentsPane_EnterOnOtherReachesTheUnrecognisedSession(t *testing.T) {
	m := otherSessionsFixture()
	m.pane, m.previousPane, m.client = paneAgents, paneSessions, deadClient()
	m.rebuildAgentsTable()
	m.agentsTbl.SetCursor(3)
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.agentScope != otherAgents || m.pane != paneSessions {
		t.Fatalf("scope %q on %v, want Other on the sessions pane", m.agentScope, m.pane)
	}
	if !slices.Contains(m.sessionRowIDs, "3981fd731b31c9d1aff4e17e6556f5c6") {
		t.Errorf("rows %v do not include the unrecognised agent's session", m.sessionRowIDs)
	}
}

// Other never goes on the wire as agent=, where a proxy would narrow to the literal string and
// answer zero; it is narrowed from the group=agent series instead, for the band and the drawer.
func TestFetchUsageScoped_OtherIsNarrowedHereAndNeverSent(t *testing.T) {
	var query atomic.Value
	ts := otherUsageServer(t, &query)
	snap, err := fetchUsageScoped(context.Background(), apiclient.New(ts.URL), "today", 0, otherAgents, usage.GroupModel)
	if err != nil {
		t.Fatal(err)
	}
	q, _ := query.Load().(string)
	if strings.Contains(q, "agent=") || !strings.Contains(q, "group=agent") {
		t.Errorf("query %q, want group=agent and no agent=", q)
	}
	if snap.Totals.Requests != 23 || snap.Agent != otherAgents || snap.Group != usage.GroupNone {
		t.Errorf("snapshot = %d requests, agent %q, group %q; want Other's 23, echoed, served as none",
			snap.Totals.Requests, snap.Agent, snap.Group)
	}
	// The drawer says what it could not break down, without calling several agents one.
	if note := drawerScopeNote(snap, usage.GroupModel, "today"); note != "no model breakdown for Other in today" {
		t.Errorf("drawer note %q", note)
	}
}

// The usage pane narrows its chart to Other the same way: totals and buckets both.
func TestFetchUsage_OtherScopeNarrowsToTheFold(t *testing.T) {
	ts := otherUsageServer(t, nil)
	m := &model{client: apiclient.New(ts.URL), agentScope: otherAgents}
	msg := m.fetchUsage()().(usageLoadedMsg)
	if msg.err != nil {
		t.Fatalf("fetch errored: %v", msg.err)
	}
	if msg.snap.Totals.Requests != 23 || msg.snap.Buckets[0].Requests != 23 {
		t.Errorf("totals %d, bucket %d, want Other's 23 in both", msg.snap.Totals.Requests, msg.snap.Buckets[0].Requests)
	}
}
