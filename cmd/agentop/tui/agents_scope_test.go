package tui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/cost/usage"
)

// scopedModel is an AGENTS pane standing on the given TABLE row, with the given scope already set.
// Row 0 is All agents, so the fixture's agents are rows 1 and 2.
func scopedModel(t *testing.T, cursor int, scope string) *model {
	t.Helper()
	m := &model{
		pane:               paneAgents,
		previousPane:       paneSessions,
		agentScope:         scope,
		agentsTbl:          newAgentsTable(),
		client:             deadClient(),
		pipelineReturnPane: paneNone,
		agents: []agentRow{
			{label: "claude-code/2.1.270", Counts: usage.Counts{Requests: 10, PricedRequests: 10}},
			{label: "bob-shell/2.0.5", Counts: usage.Counts{Requests: 8}},
		},
	}
	m.rebuildAgentsTable()
	for i := 0; i < cursor; i++ {
		m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	}
	return m
}

// Enter scopes the usage pane to the row under the cursor, and leaves the pane.
//
// LEAVING IS PART OF THE ACTION, not a separate keystroke: the pane is a picker.
func TestAgentsPane_EnterScopesTheRowUnderTheCursorAndLeaves(t *testing.T) {
	m := scopedModel(t, 2, "")
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.agentScope != "bob-shell/2.0.5" {
		t.Errorf("agentScope = %q, want the row under the cursor", m.agentScope)
	}
	if m.pane != paneSessions {
		t.Errorf("Enter left the reader on %v, want the caller pane", m.pane)
	}
}

// Enter on the agent ALREADY scoped keeps it scoped. Enter means "show me the highlighted row",
// whatever is set now: it used to toggle here, clearing the scope, so a reader re-picking the
// agent they were on was silently handed every agent's sessions and spend instead.
func TestAgentsPane_EnterOnTheScopedAgentKeepsTheScope(t *testing.T) {
	m := scopedModel(t, 1, "claude-code/2.1.270")
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.agentScope != "claude-code/2.1.270" {
		t.Errorf("agentScope = %q, want the highlighted agent kept", m.agentScope)
	}
	if m.pane != paneSessions {
		t.Errorf("Enter left the reader on %v, want the caller pane", m.pane)
	}
}

// All agents is the first row, and Enter on it is the one way to clear the scope.
func TestAgentsPane_EnterOnAllAgentsClearsTheScope(t *testing.T) {
	m := scopedModel(t, 0, "claude-code/2.1.270")
	if got := m.agentsTbl.Rows()[0][0]; got != allAgentsLabel {
		t.Fatalf("first row = %q, want %q", got, allAgentsLabel)
	}
	if n := len(m.agentsTbl.Rows()); n != len(m.agents)+1 {
		t.Errorf("%d rows for %d agents, want one per agent plus All agents", n, len(m.agents))
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.agentScope != "" {
		t.Errorf("agentScope = %q, want cleared", m.agentScope)
	}
	if m.pane != paneSessions {
		t.Errorf("Enter left the reader on %v, want the caller pane", m.pane)
	}
}

// Enter lists the picked agent's sessions, whichever pane `A` was pressed on.
//
// IT USED TO RETURN TO THAT PANE, sharing esc's exit, and from a session's events that hid the
// pick: the events pane shows one session whatever the scope, so `A` there, then Enter on another
// agent, put the reader back in the session they were reading — under a title naming an agent it
// did not belong to. Picking an agent is asking for its sessions.
//
// Opened by a real `A` press, so the caller under test is the one keys.go records. Every row but
// Sessions is a caller the old exit returned to, so each can fail; the Sessions row is the case
// that already worked.
func TestAgentsPane_EnterListsThePickedAgentsSessionsFromAnyCaller(t *testing.T) {
	rows := []agentRow{
		{label: "claude-code/2.1.270", Counts: usage.Counts{Requests: 10, PricedRequests: 10}},
		{label: "bob-shell/2.0.5", Counts: usage.Counts{Requests: 8}},
	}
	for _, from := range []paneID{paneSessions, paneEvents, paneDetail, paneUsage, panePipeline, paneCatalog} {
		m := &model{
			pane:               from,
			previousPane:       paneNone,
			agentScope:         "claude-code/2.1.270",
			agentsTbl:          newAgentsTable(),
			client:             deadClient(),
			pipelineReturnPane: paneNone,
		}
		press, ok := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'A'}})().(agentRowsLoadedMsg)
		if !ok {
			t.Fatalf("`A` on %v did not fetch the agents", from)
		}
		updated, _ := m.Update(agentRowsLoadedMsg{rows: rows, open: agentsOpenOnPress, from: press.from})
		m = updated.(*model)
		if m.pane != paneAgents {
			t.Fatalf("`A` on %v did not open the picker: pane = %v", from, m.pane)
		}
		m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
		m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
		want, _ := m.selectedAgentScope()
		if want != "bob-shell/2.0.5" {
			t.Fatalf("cursor on %q, want the agent not already scoped", want)
		}
		m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
		if m.pane != paneSessions || m.agentScope != want {
			t.Errorf("`A` on %v, Enter on %s: pane %v scope %q, want the sessions list scoped to it",
				from, want, m.pane, m.agentScope)
		}
		// Spent, as esc's exit spends it: previousPane is shared with the catalog.
		if m.previousPane != paneNone {
			t.Errorf("`A` on %v: Enter left previousPane = %v, want it spent", from, m.previousPane)
		}
	}
}

// usageSnapshotJSON is a two-agent group=agent response, the shape /v1/usage serves.
const usageSnapshotJSON = `{
  "window":"today","bucketSeconds":60,"group":"agent",
  "buckets":[
    {"at":"2026-09-29T10:00:00Z","requests":22,"tokens":2200,"latMeanMs":50,"latSamples":22,
     "series":{
       "claude-code/2.1.270":{"requests":14,"tokens":1400},
       "bob-shell/2.0.5":{"requests":8,"tokens":800}}}],
  "totals":{"requests":22,"tokens":2200}}`

// A scoped usage fetch asks for the AGENT axis, whatever axis the pane itself is showing.
//
// The scope can only be computed from a per-agent series, and /v1/usage takes one group
// parameter — so while a scope is active the wire axis is not the pane's to choose. The pane's
// own group is left on the model rather than overwritten, so clearing the scope restores the
// axis the operator had picked.
func TestFetchUsage_ScopedAsksForTheAgentAxis(t *testing.T) {
	var gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The narrowing read's query, not the latency read's that follows it and asks for agent=
		// instead. See graftAgentLatency.
		if !r.URL.Query().Has("agent") {
			gotQuery = r.URL.RawQuery
		}
		_, _ = w.Write([]byte(usageSnapshotJSON))
	}))
	defer ts.Close()

	m := &model{client: apiclient.New(ts.URL), agentScope: "claude-code/2.1.270"}
	m.usage.group = usage.GroupModel // the pane's own axis, which must not reach the wire
	cmd := m.fetchUsage()
	if cmd == nil {
		t.Fatal("fetchUsage returned no command with a client set")
	}
	msg, ok := cmd().(usageLoadedMsg)
	if !ok {
		t.Fatalf("fetchUsage produced %T, want usageLoadedMsg", cmd())
	}
	if msg.err != nil {
		t.Fatalf("fetch errored: %v", msg.err)
	}
	q, err := url.ParseQuery(gotQuery)
	if err != nil {
		t.Fatalf("unparseable query %q: %v", gotQuery, err)
	}
	if got := q.Get("group"); got != string(usage.GroupAgent) {
		t.Errorf("group = %q, want %q: the scope cannot be computed without the agent series",
			got, usage.GroupAgent)
	}
	if m.usage.group != usage.GroupModel {
		t.Errorf("the pane's own axis was overwritten to %q; clearing the scope would not "+
			"restore what the operator picked", m.usage.group)
	}
}

// The snapshot the pane receives is already narrowed — totals AND buckets.
//
// BOTH, because the pane renders a summary line from Totals and a chart from Buckets. Narrowing
// only the totals is what usage.KeepBuckets does, and it would title a whole-window chart with
// one agent's name.
func TestFetchUsage_ScopedNarrowsTotalsAndBuckets(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(usageSnapshotJSON))
	}))
	defer ts.Close()

	m := &model{client: apiclient.New(ts.URL), agentScope: "claude-code/2.1.270"}
	msg := m.fetchUsage()().(usageLoadedMsg)
	if msg.err != nil {
		t.Fatalf("fetch errored: %v", msg.err)
	}
	if msg.snap.Totals.Requests != 14 {
		t.Errorf("Totals.Requests = %d, want this agent's 14 (the window's is 22)",
			msg.snap.Totals.Requests)
	}
	if len(msg.snap.Buckets) != 1 {
		t.Fatalf("len(Buckets) = %d, want 1", len(msg.snap.Buckets))
	}
	if msg.snap.Buckets[0].Requests != 14 {
		t.Errorf("bucket Requests = %d, want this agent's 14; the chart would draw the whole "+
			"window under a title naming one agent", msg.snap.Buckets[0].Requests)
	}
}

// An unscoped fetch still asks for the pane's own axis. The regression guard for the case
// above: forcing GroupAgent unconditionally would break every breakdown the pane offers.
func TestFetchUsage_UnscopedAsksForThePanesOwnAxis(t *testing.T) {
	var gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"window":"today","group":"model","buckets":[],"totals":{}}`))
	}))
	defer ts.Close()

	m := &model{client: apiclient.New(ts.URL)}
	m.usage.group = usage.GroupModel
	if _, ok := m.fetchUsage()().(usageLoadedMsg); !ok {
		t.Fatal("fetchUsage produced the wrong message type")
	}
	q, _ := url.ParseQuery(gotQuery)
	if got := q.Get("group"); got != string(usage.GroupModel) {
		t.Errorf("group = %q, want the pane's own %q", got, usage.GroupModel)
	}
}

// A scope that no longer matches any agent in the window is REPORTED, not silently ignored.
//
// The window moves while agentop runs — "today" is a boundary, and an agent that stopped sending
// falls out of it — so this is reachable without anyone doing anything wrong. The error names
// the agents that ARE in the window, which is what the operator needs in order to pick a
// different one.
func TestFetchUsage_ScopeThatMatchesNothingIsReported(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(usageSnapshotJSON))
	}))
	defer ts.Close()

	m := &model{client: apiclient.New(ts.URL), agentScope: "an-agent-that-went-away/1.0"}
	msg := m.fetchUsage()().(usageLoadedMsg)
	if msg.err == nil {
		t.Fatal("a scope matching no agent produced no error; the pane would show every agent")
	}
	if !strings.Contains(msg.err.Error(), "claude-code/2.1.270") {
		t.Errorf("error %q does not name the agents in the window", msg.err)
	}
}

// The usage title carries the scope, so a narrowed chart is never unlabelled.
func TestUsageTitle_NamesTheAgentScope(t *testing.T) {
	m := fitModel(t, paneUsage, 120, 40, nil)
	m.agentScope = "claude-code/2.1.270"
	// The TITLE ROW, not the whole view: paneView returns title plus body, and asserting over
	// both would pass on a label that happened to appear inside the chart.
	title := strings.SplitN(m.paneView(), "\n", 2)[0]
	if !strings.Contains(title, "claude-code/2.1.270") {
		t.Errorf("title %q does not name the scoped agent", title)
	}
}

// [b] is omitted from the footer while a scope is active, because it cannot act.
//
// The wire axis is the agent's while scoped, so there is no second axis to break down by. Same
// treatment the footer already gives [b] under latency, and for the same stated reason: a footer
// that advertises an inert key is worse than a shorter footer.
func TestUsageFooter_OmitsTheBreakdownKeyWhileScoped(t *testing.T) {
	m := fitModel(t, paneUsage, 120, 40, nil)
	if !strings.Contains(m.helpView(), "[b] breakdown") {
		t.Fatal("the unscoped footer does not offer [b]; this test is measuring the wrong thing")
	}
	m.agentScope = "claude-code/2.1.270"
	if strings.Contains(m.helpView(), "[b] breakdown") {
		t.Error("the footer still advertises [b] under an agent scope, where it cannot act")
	}
}

// Nor does the header claim a breakdown while scoped. The pane's own axis stays on the model, so
// printing it would put "by model" over a chart the scope has left ungrouped.
func TestUsageHeader_ClaimsNoBreakdownWhileScoped(t *testing.T) {
	m := fitModel(t, paneUsage, 120, 40, nil)
	m.usage.group = usage.GroupModel
	header := func() string { return strings.SplitN(m.renderUsage(m.width, m.bodyHeight), "\n", 2)[0] }
	if !strings.Contains(header(), "by model") {
		t.Fatalf("the unscoped header %q does not show the breakdown; this test is measuring the wrong thing", header())
	}
	m.agentScope = "claude-code/2.1.270"
	if got := header(); !strings.Contains(got, "ungrouped") {
		t.Errorf("the scoped header %q claims a breakdown the chart is not showing", got)
	}
}

// [b] does nothing while scoped, rather than refetching against an axis the scope has taken.
func TestUsageKeys_BreakdownIsInertWhileScoped(t *testing.T) {
	m := fitModel(t, paneUsage, 120, 40, nil)
	m.agentScope = "claude-code/2.1.270"
	before := m.usage.group
	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	if m.usage.group != before {
		t.Errorf("[b] moved the axis to %q under a scope; the wire axis is the agent's",
			m.usage.group)
	}
}

// Latency under an agent scope says it is unavailable rather than plotting the zeros.
//
// THE NARROWING CANNOT CARRY LATENCY. Bucket.Series is map[string]Counts and Counts holds no
// latency, so a bucket's LatMeanMs describes every agent that shared it — usage.ScopeToAgent
// zeroes the fields rather than attributing one agent's chart to another's response times. A
// chart of those zeros would read as "this agent was instant", which is the worst of the three
// available answers.
func TestUsagePane_LatencyUnderAScopeSaysItIsUnavailable(t *testing.T) {
	m := fitModel(t, paneUsage, 120, 40, nil)
	for !m.usage.metric.isLatency() {
		m.usage.cycleMetric()
	}
	m.agentScope = "claude-code/2.1.270"
	body := m.renderUsage(m.width, m.bodyHeight)
	if !strings.Contains(body, "latency") {
		t.Errorf("the scoped latency view does not mention latency:\n%s", body)
	}
	if !strings.Contains(body, "agent") {
		t.Errorf("the scoped latency view does not say the scope is why:\n%s", body)
	}
}

// The scoped pane STATES THE RESIDUAL its COST cell leaves out.
//
// usage.Snapshot.UngroupedCostMicros is a whole-window figure and survives usage.ScopeToAgent, so
// under a scope the pane holds a COST cell describing one agent beside a residual describing no
// agent. `agentop cost` has disclosed this at writeCostSummary's --agent note for as long as --agent
// has existed; the usage pane is the surface that gained the same duty when it gained a scope, and
// it had nothing to say about it.
//
// BOTH RENDER BRANCHES, because both print the summary and so both print its COST cell: the default
// one, and the latency one that prints the summary after saying latency is unavailable per agent. A
// disclosure added to one and not the other is the failure this covers.
//
// THE AMOUNT IS AN INDEPENDENT LITERAL rather than formatUSDTotalMicros(residual): asserting
// against the producer's own output would pass on whatever figure it chose to render, including the
// window total. 750_000 micros is $0.75, and it is distinct from the fixture's 55_000 total, so a
// note built from the wrong field cannot satisfy this.
func TestUsagePane_ScopedSummaryDisclosesTheCostNoAgentCarries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		latency bool
	}{
		{name: "count metric, the default branch"},
		{name: "latency metric, the branch that says latency is unavailable", latency: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := fitModel(t, paneUsage, 120, 40, nil)
			if tc.latency {
				for !m.usage.metric.isLatency() {
					m.usage.cycleMetric()
				}
			}
			residual := int64(750_000)
			m.usage.snap.UngroupedCostMicros = &residual
			m.agentScope = "claude-code/2.1.270"
			body := m.renderUsage(m.width, m.bodyHeight)
			if !strings.Contains(body, "$0.75") {
				t.Errorf("the scoped summary does not state the residual amount:\n%s", body)
			}
			if !strings.Contains(body, "attributed to no agent") {
				t.Errorf("the scoped summary names an amount without saying what it is:\n%s", body)
			}
		})
	}
}

// The note row is conditional, so usagePaneChromeRows does not count it and
// TestUsageChartHeight_MatchesTheRenderedChrome, which renders unscoped, cannot see it. Both
// sizes fit the terminal unscoped; the scoped pane with a residual must fit them too.
func TestUsagePane_TheScopedNoteFitsWhereTheUnscopedPaneDoes(t *testing.T) {
	for _, tc := range []struct {
		name string
		w, h int
	}{{"80x26", 80, 26}, {"120x26", 120, 26}} {
		m := fitModel(t, paneUsage, tc.w, tc.h, cursorRowsFixture(60))
		residual := int64(750_000)
		m.usage.snap.UngroupedCostMicros = &residual
		m.agentScope = "claude-code/2.1.270"
		if !strings.Contains(m.View(), "attributed to no agent") {
			t.Fatalf("%s: the fixture does not reach the note row", tc.name)
		}
		assertFits(t, m, "scoped usage "+tc.name)
	}
}

// UNSCOPED, the pane says nothing about the residual: the COST cell and the residual then describe
// the same window, and there is no shortfall between them to explain.
//
// THE COMPANION THAT MAKES THE TEST ABOVE MEAN SOMETHING. An unconditional note would satisfy that
// one and be wrong on every unscoped window, which is the pane's normal state.
func TestUsagePane_UnscopedSummarySaysNothingAboutTheResidual(t *testing.T) {
	m := fitModel(t, paneUsage, 120, 40, nil)
	residual := int64(750_000)
	m.usage.snap.UngroupedCostMicros = &residual
	if body := m.renderUsage(m.width, m.bodyHeight); strings.Contains(body, "attributed to no agent") {
		t.Errorf("the unscoped pane disclosed a residual that describes the very window it is showing:\n%s", body)
	}
}

// The states with nothing to say say nothing, rather than a note reading "$0.00" or a negative.
//
// A ZERO RESIDUAL is the ordinary case on a reconcilable window — the series accounts for every
// dollar — and a note for it would train an operator to ignore the line. AN ABSENT field is a
// producer that computed no residual at all. A NEGATIVE cannot arrive, because usage's residualOf
// routes one to SeriesOvershootMicros, so refusing it is this surface's impossible-figure rule and
// not a formatting fix: formatUSDTotalMicros renders a negative as a four-decimal figure perfectly
// well, and the note would then assert a negative residual in prose.
func TestCostUngroupedRow_SaysNothingWhereThereIsNothingToSay(t *testing.T) {
	for _, tc := range []struct {
		name   string
		micros int64
		absent bool
		noSnap bool
		scope  string
	}{
		{name: "a residual, but no scope", micros: 750_000},
		{name: "scoped, the producer computed no residual", absent: true, scope: "claude-code/2.1.270"},
		{name: "scoped, the series accounts for every dollar", micros: 0, scope: "claude-code/2.1.270"},
		{name: "scoped, an impossible negative residual", micros: -150_000, scope: "claude-code/2.1.270"},
		{name: "scoped, no snapshot fetched yet", noSnap: true, scope: "claude-code/2.1.270"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var snap *usage.Snapshot
			if !tc.noSnap {
				snap = &usage.Snapshot{}
				if !tc.absent {
					micros := tc.micros
					snap.UngroupedCostMicros = &micros
				}
			}
			if got := costUngroupedRow(snap, tc.scope, nil); got != "" {
				t.Errorf("costUngroupedRow = %q, want the empty string", got)
			}
		})
	}
}

// The AGENTS pane's own title names the scope, so returning to the picker shows what is set.
func TestAgentsPane_TitleNamesTheActiveScope(t *testing.T) {
	m := scopedModel(t, 2, "bob-shell/2.0.5")
	m.width, m.height = 120, 40
	m.layout()
	title := strings.SplitN(m.paneView(), "\n", 2)[0]
	if !strings.Contains(title, "bob-shell/2.0.5") {
		t.Errorf("AGENTS title %q does not name the active scope", title)
	}
}

// The footer names what Enter shows: the highlighted agent on an agent row, including the one
// already scoped, and every agent on the All agents row.
func TestAgentsPane_FooterSaysWhatEnterWillShow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cursor int
		want   string
	}{
		{"the scoped agent", 1, "scope to this agent"},
		{"another agent", 2, "scope to this agent"},
		{"All agents", 0, "all agents"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := scopedModel(t, tc.cursor, "claude-code/2.1.270")
			if got := m.helpView(); !strings.Contains(got, tc.want) {
				t.Errorf("footer = %q, want %q", got, tc.want)
			}
		})
	}
}

// The residual belongs to no agent, so its unit is the WINDOW's: scoped to a dollars-only agent
// in a window that also bills credits, the note must not state it in dollars.
func TestUsagePane_TheResidualKeepsTheWindowsUnits(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"window":"1h","group":"agent","priced":true,
			"currencies":["USD","credits"],"seriesCurrencies":{"claude-code/2.1.270":["USD"]},
			"ungroupedCostMicros":5000000,
			"totals":{"requests":20,"costMicros":9000000,"pricedRequests":20,"priceableRequests":20},
			"buckets":[{"at":"2026-09-27T10:00:00Z","series":{
			   "claude-code/2.1.270":{"requests":14,"costMicros":4000000,"pricedRequests":14,"priceableRequests":14}}}]}`))
	}))
	defer ts.Close()
	m := fitModel(t, paneUsage, 120, 40, nil)
	m.client = apiclient.New(ts.URL)
	m.agentScope = "claude-code/2.1.270"
	msg := m.fetchUsage()().(usageLoadedMsg)
	if msg.err != nil {
		t.Fatalf("fetch errored: %v", msg.err)
	}
	next, _ := m.Update(msg)
	m = next.(*model)
	if body := m.renderUsage(m.width, m.bodyHeight); strings.Contains(body, "$5.00") {
		t.Errorf("a whole-window residual in a mixed window was stated in the agent's dollars:\n%s", body)
	}
}
