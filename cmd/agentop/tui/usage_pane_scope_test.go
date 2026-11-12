package tui

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/cost/usage"
)

// TestUsageScopeMax_CoversTheWidestHeader recomputes usageScopeMax's allowance from the sources
// renderUsage draws on, rather than from a list written out by hand.
//
// THE HAND-WRITTEN LIST IS WHY THIS TEST EXISTED AND FAILED TO WORK. Its first version
// enumerated grouping strings the renderer never emits ("no breakdown", "by model" without the
// prefix it actually adds) while omitting the longest one it does — "no breakdown for latency",
// 24 columns by itself — and hardcoded one window, so its computed widest was 51 against a real
// 65. It then asserted that 51 fitted the constant, which it did, and passed.
//
// So the inputs come from usageWindows, the metric enum and the grouping branches in
// renderUsage. A grouping added there is the one thing this still cannot see; the switch is
// three lines long and sits beside the format string it feeds.
func TestUsageScopeMax_CoversTheWidestHeader(t *testing.T) {
	// The strings renderUsage's grouping switch can produce. "no breakdown for latency" is the
	// literal it emits for the latency metric; the rest are "by " + the group name.
	groupings := []string{"ungrouped", "no breakdown for latency"}
	for _, g := range []usage.Group{usage.GroupStatus, usage.GroupModel, usage.GroupPlugin, usage.GroupHost} {
		groupings = append(groupings, "by "+string(g))
	}

	widest, worst := 0, ""
	for m := usageMetric(0); m < usageMetricCount; m++ {
		for _, w := range usageWindows {
			for _, g := range groupings {
				// The same format renderUsage writes, with an empty scope so what remains is
				// the fixed cost the allowance has to cover.
				line := fmt.Sprintf("  USAGE — %s — %s @ %s — %s — %s", "", w.window, w.resolution, m, g)
				if n := len([]rune(line)); n > widest {
					widest, worst = n, fmt.Sprintf("%s / %s / %s@%s", m, g, w.window, w.resolution)
				}
			}
		}
	}

	// The allowance must cover the widest remainder. Asserted by asking usageScopeMax what it
	// grants on a terminal wide enough that its floor cannot be what answers — the floor is
	// what made the earlier version's "case that matters" assertion unreachable at width 80.
	const wide = 1000
	if granted := usageScopeMax(wide); granted != wide-widest {
		t.Errorf("usageScopeMax reserves %d columns for the rest of the line; the widest "+
			"reachable remainder is %d (%s)", wide-granted, widest, worst)
	}
}

// TestUsageHeader_LatencyOverflowsEightyAndFitsAt88 states the header's real width behaviour,
// because nothing did: the pane-fit tests render the model's zero value — tokens metric, the
// shortest window — and never reach the combination that motivated raising the allowance.
//
// THROUGH renderUsage, not a hand-built format string. An earlier revision of this test rebuilt
// the header itself; the two agreed exactly, so there was no live defect, but a change to the
// real format string would have moved the header and left this test green against its own copy.
//
// ASSERTED AS AN OVERFLOW, not as a fit. The header still runs past an 80-column terminal on the
// latency metric at the longest window, because usageScopeMax's 24-column floor wins there and
// returns more room than the line has. That floor is deliberate — a scope nobody can read is
// worse than a wrapped line — so this records the cost rather than asserting a fit the code does
// not deliver. If a later change removes the overflow, this fails and says so, which is the
// right way round for a known shortfall.
func TestUsageHeader_LatencyOverflowsEightyAndFitsAt88(t *testing.T) {
	const id = "0e61b82d-8578-4d16-a18e-1234567890ab"
	const title = "/Users/someone/src/cortex/.worktrees/x"
	header := func(w int) string {
		m := newTitleModel(t, map[string]SessionMetadata{id: {Title: title}})
		// The widest reachable header: latency's grouping literal, and the longest window.
		m.usage = usageState{session: id, metric: metricLatency, windowIdx: len(usageWindows) - 1}
		m.width = w
		return strings.Split(m.renderUsage(w, 20), "\n")[0]
	}
	if got := header(80); len([]rune(got)) <= 80 {
		t.Errorf("the latency header now fits 80 columns (%d): %q — the overflow this records is "+
			"gone, and usageScopeMax's doc should stop disclosing it", len([]rune(got)), got)
	}
	if got := header(88); len([]rune(got)) > 88 {
		t.Errorf("the latency header is %d columns at 88: %q", len([]rune(got)), got)
	}
}

// The usage pane's hints that send the reader to the agents picker name [u]. Enter there lists the
// picked agent's sessions, so a hint that stops at the picker leaves the reader on Sessions; [u]
// from Sessions is what charts the new scope. The latency hint also names the row that clears the
// scope, All agents. Every line naming a key fits 80 columns: bubbletea cuts a line at the
// terminal's width, so a key past it is never seen.
//
// Each route is then driven as written, so the keys the hints name are the keys that work.
func TestUsagePane_PickerHintsNameU(t *testing.T) {
	latency := &model{agentScope: "bob-shell/2.0.5"}
	latency.usage.metric = metricLatency
	latency.usage.snap = costChartSnapshot(nil)
	latencyHint := latency.renderUsage(80, 24)
	mixedHint := strings.Join(renderUsageChart(costChartSnapshot([]string{"Bobcoins", "USD"}), metricCost, "", 60, 12), "\n")
	for name, hint := range map[string]string{"latency": latencyHint, "mixed units": mixedHint} {
		if !strings.Contains(hint, "[A]") || !strings.Contains(hint, "[u]") {
			t.Errorf("%s hint does not name [A] and [u]:\n%s", name, hint)
		}
		for _, line := range strings.Split(hint, "\n") {
			if strings.Contains(line, "[") && lipgloss.Width(line) > 80 {
				t.Errorf("%s hint line is %d columns, past 80:\n%s", name, lipgloss.Width(line), line)
			}
		}
	}
	if !strings.Contains(latencyHint, "All agents") {
		t.Errorf("latency hint does not name All agents, the row that clears the scope:\n%s", latencyHint)
	}

	for _, tc := range []struct {
		name   string
		cursor int
		want   string
	}{
		{"latency: clear the scope", 0, ""},
		{"mixed units: scope to one agent", 2, "bob-shell/2.0.5"},
	} {
		m := &model{
			pane:               paneUsage,
			previousPane:       paneNone,
			agentScope:         "claude-code/2.1.270",
			agentsTbl:          newAgentsTable(),
			client:             deadClient(),
			pipelineReturnPane: paneNone,
		}
		press, ok := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'A'}})().(agentRowsLoadedMsg)
		if !ok {
			t.Fatalf("%s: `A` on Usage did not fetch the agents", tc.name)
		}
		m.Update(agentRowsLoadedMsg{rows: []agentRow{
			{label: "claude-code/2.1.270", Counts: usage.Counts{Requests: 10, PricedRequests: 10}},
			{label: "bob-shell/2.0.5", Counts: usage.Counts{Requests: 8}},
		}, open: agentsOpenOnPress, from: press.from})
		if m.pane != paneAgents {
			t.Fatalf("%s: `A` on Usage did not open the picker: pane = %v", tc.name, m.pane)
		}
		m.agentsTbl.SetCursor(tc.cursor)
		m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
		m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'u'}})
		if m.pane != paneUsage || m.agentScope != tc.want {
			t.Errorf("%s: [A], [enter], [u] left pane %v scope %q, want Usage scoped to %q",
				tc.name, m.pane, m.agentScope, tc.want)
		}
	}
}

// projectUsageJSON is a group=project reply that echoes the agent the request named.
func projectUsageJSON(agent string) string {
	return `{"window":"today","bucketSeconds":60,"group":"project","agent":"` + agent + `",
  "buckets":[{"at":"2026-09-29T10:00:00Z","requests":4,"costMicros":400,"latMeanMs":50,"latSamples":4,
    "series":{"~/a":{"requests":3,"costMicros":300},"~/b":{"requests":1,"costMicros":100}}}],
  "totals":{"requests":4,"costMicros":400}}`
}

// projectUsageFetch runs one usage fetch from m against a server that records every query. The
// server fails the test when a query has a project key (D3).
func projectUsageFetch(t *testing.T, m *model) (usageLoadedMsg, []url.Values) {
	t.Helper()
	var queries []url.Values
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Has("project") {
			t.Errorf("query %q carries project=", r.URL.RawQuery)
		}
		queries = append(queries, q)
		_, _ = w.Write([]byte(projectUsageJSON(q.Get("agent"))))
	}))
	t.Cleanup(ts.Close)
	m.client = apiclient.New(ts.URL)
	msg, ok := m.fetchUsage()().(usageLoadedMsg)
	if !ok || msg.err != nil {
		t.Fatalf("fetch gave %#v", msg)
	}
	return msg, queries
}

func TestUsagePane_ProjectScopeAsksAgentAndGroupProject(t *testing.T) {
	m := &model{agentScope: "claude-code/2.1.270", projectScope: "~/a"}
	m.usage.group = usage.GroupModel
	msg, queries := projectUsageFetch(t, m)
	if len(queries) != 1 {
		t.Fatalf("got %d reads, want 1: %v", len(queries), queries)
	}
	if q := queries[0]; q.Get("agent") != "claude-code/2.1.270" || q.Get("group") != string(usage.GroupProject) {
		t.Errorf("query = %v, want agent=claude-code/2.1.270 and group=project", q)
	}
	if msg.snap.Totals.CostMicros != 300 {
		t.Errorf("Totals.CostMicros = %d, want the project's 300", msg.snap.Totals.CostMicros)
	}
	var plotted int64
	for _, b := range msg.snap.Buckets {
		plotted += b.CostMicros
	}
	if plotted != 300 {
		t.Errorf("plotted bucket cost = %d, want 300", plotted)
	}
}

func TestUsagePane_ProjectScopeUnderAllAgentsHasNoAgent(t *testing.T) {
	m := &model{projectScope: "~/a"}
	_, queries := projectUsageFetch(t, m)
	if len(queries) != 1 || queries[0].Has("agent") || queries[0].Get("group") != string(usage.GroupProject) {
		t.Errorf("queries = %v, want one with group=project and no agent=", queries)
	}
}

// Decision 3: within a session the agent axis has no project series, so the read drops agent=.
func TestUsagePane_ProjectScopeWithinASessionDropsTheAgent(t *testing.T) {
	m := &model{agentScope: "claude-code/2.1.270", projectScope: "~/a"}
	m.usage.session = "s1"
	msg, queries := projectUsageFetch(t, m)
	if len(queries) != 1 {
		t.Fatalf("got %d reads, want 1: %v", len(queries), queries)
	}
	if q := queries[0]; q.Get("session") != "s1" || q.Get("group") != string(usage.GroupProject) || q.Has("agent") {
		t.Errorf("query = %v, want session=s1, group=project and no agent=", q)
	}
	if msg.snap.Totals.CostMicros != 300 {
		t.Errorf("Totals.CostMicros = %d, want the project's 300", msg.snap.Totals.CostMicros)
	}
}

func TestUsageKeys_BreakdownIsInertUnderAProjectScope(t *testing.T) {
	m := &model{pane: paneUsage, projectScope: "~/a", client: deadClient()}
	m.usage.group = usage.GroupModel
	m.usage.metric = metricCost
	if cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}}); cmd != nil {
		t.Error("`b` under a project scope returned a fetch")
	}
	if m.usage.group != usage.GroupModel {
		t.Errorf("group = %q, want it unchanged", m.usage.group)
	}
	if strings.Contains(m.helpView(), "[b]") {
		t.Errorf("footer offers [b] under a project scope:\n%s", m.helpView())
	}
	m.usage.snap = costChartSnapshot(nil)
	if head := strings.SplitN(m.renderUsage(120, 30), "\n", 2)[0]; !strings.Contains(head, "ungrouped") {
		t.Errorf("header = %q, want ungrouped", head)
	}
}

func TestUsagePane_LatencyUnderAProjectScopeSaysPerProject(t *testing.T) {
	m := &model{projectScope: "~/a"}
	m.usage.metric = metricLatency
	msg, queries := projectUsageFetch(t, m)
	if len(queries) != 1 {
		t.Errorf("got %d reads, want only the window read: %v", len(queries), queries)
	}
	m.Update(msg)
	m.usage.snap, m.usage.agentLatency = msg.snap, msg.agentLatency
	body := m.renderUsage(100, 30)
	if !strings.Contains(body, "latency is not available per project") ||
		!strings.Contains(body, "Response times are recorded per agent, not per project.") {
		t.Errorf("body does not say latency is per agent only:\n%s", body)
	}
	if strings.Contains(body, "per agent)") {
		t.Errorf("body gives the per-agent text under a project scope:\n%s", body)
	}
}
