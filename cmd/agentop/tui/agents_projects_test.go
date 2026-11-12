package tui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/session"
)

func TestProjectDisplayNames(t *testing.T) {
	cases := []struct{ in, want []string }{
		{[]string{"~/works/cortex", "~/moca"}, []string{"cortex", "moca"}},
		{[]string{"~"}, []string{"~"}},
		{[]string{"~/works/go/src/github.com/kagenti/kagenti-extensions", "~/git/kagenti/kagenti-extensions"},
			[]string{"github.com/kagenti/kagenti-extensions", "git/kagenti/kagenti-extensions"}},
		{[]string{"~/a/b", "~/x/a/b"}, []string{"~/a/b", "x/a/b"}},
		{[]string{"/srv/app", "~/app", "~/tools"}, []string{"srv/app", "~/app", "tools"}},
	}
	for _, c := range cases {
		if got := projectDisplayNames(c.in); !slices.Equal(got, c.want) {
			t.Errorf("projectDisplayNames(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func projectCounts(micros int64) usage.Counts {
	return usage.Counts{CostMicros: micros, PricedRequests: 1}
}

func TestProjectRowsFromSnapshot_FoldsUnknownAndOverflowIntoOther(t *testing.T) {
	snap := &usage.Snapshot{Buckets: []usage.Bucket{
		{Series: map[string]usage.Counts{
			"~/works/cortex":          projectCounts(90_000_000),
			"~/moca":                  projectCounts(38_110_000),
			usage.UnknownProjectLabel: projectCounts(10_000_000),
		}},
		{Series: map[string]usage.Counts{
			"~/works/cortex":     projectCounts(7_400_000),
			projectOverflowLabel: projectCounts(5_520_000),
			"~":                  projectCounts(1_000),
		}},
	}}
	rows := projectRowsFromSnapshot(snap)
	var labels []string
	for _, r := range rows {
		labels = append(labels, r.label)
	}
	if want := []string{"~/works/cortex", "~/moca", "~", otherProjects}; !slices.Equal(labels, want) {
		t.Fatalf("labels = %q, want %q", labels, want)
	}
	if got := rows[0].CostMicros; got != 97_400_000 {
		t.Errorf("~/works/cortex CostMicros = %d, want 97400000", got)
	}
	if got := rows[3].CostMicros; got != 15_520_000 {
		t.Errorf("Other CostMicros = %d, want 15520000", got)
	}
}

func TestProjectRowsFromSnapshot_UnitsFollowTheSeries(t *testing.T) {
	snap := &usage.Snapshot{
		Currencies:       []string{"USD", "credits"},
		SeriesCurrencies: map[string][]string{"~/moca": {"credits"}},
		Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
			"~/moca":         projectCounts(2_000_000),
			"~/works/cortex": projectCounts(1_000_000),
		}}},
	}
	for _, r := range projectRowsFromSnapshot(snap) {
		want := snap.Currencies
		if r.label == "~/moca" {
			want = []string{"credits"}
		}
		if !slices.Equal(r.units, want) {
			t.Errorf("row %q units = %q, want %q", r.label, r.units, want)
		}
	}
}

const projAgent = "claude-code"

// projectSessions are claude-code sessions in ~/works/cortex, ~/moca, both, and none, and one
// codex session.
func projectSessions(now time.Time) []session.SessionSummary {
	mk := func(id, agent string, ago time.Duration, projects ...string) session.SessionSummary {
		return session.SessionSummary{ID: id, Agent: agent, UpdatedAt: now.Add(-ago), Projects: projects}
	}
	return []session.SessionSummary{
		mk("s1", projAgent, time.Minute, "~/works/cortex"),
		mk("s2", projAgent, 2*time.Minute, "~/moca"),
		mk("s3", projAgent, 3*time.Minute, "~/works/cortex", "~/moca"),
		mk("s4", projAgent, 4*time.Minute),
		mk("s5", "codex", 5*time.Minute, "~/tools"),
	}
}

// defaultProjectSeries is the group=project series the server gives for claude-code.
var defaultProjectSeries = map[string]map[string]int64{
	projAgent: {
		"~/works/cortex":          97_400_000,
		"~/moca":                  38_110_000,
		usage.UnknownProjectLabel: 10_000_000,
		projectOverflowLabel:      5_520_000,
	},
}

// projectsModel is a connected agents pane over claude-code, codex and Other. Its server answers
// group=project with series[agent] (agent "" is All agents) and group=agent with the two agents.
// Requests are recorded as their query strings.
func projectsModel(t *testing.T, series map[string]map[string]int64) (*model, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var asked []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.RawQuery)
		mu.Unlock()
		q := r.URL.Query()
		// The agent is echoed, as the real server does, so fetchUsageScoped takes no fallback.
		snap := usage.Snapshot{Window: q.Get("window"), Group: usage.Group(q.Get("group")), Agent: q.Get("agent")}
		var s map[string]int64
		switch usage.Group(q.Get("group")) {
		case usage.GroupProject:
			s = series[q.Get("agent")]
		case usage.GroupAgent:
			s = map[string]int64{projAgent: 151_030_000, "codex": 30_900_000}
		}
		b := usage.Bucket{Series: map[string]usage.Counts{}}
		for label, micros := range s {
			b.Series[label] = projectCounts(micros)
			snap.Totals.Add(projectCounts(micros))
		}
		snap.Buckets = []usage.Bucket{b}
		_ = json.NewEncoder(w).Encode(snap)
	}))
	t.Cleanup(ts.Close)
	now := time.Now()
	m := &model{
		width:     120,
		height:    40,
		pane:      paneAgents,
		client:    apiclient.New(ts.URL),
		agentsTbl: newAgentsTable(),
		agents: []agentRow{
			{label: projAgent, Counts: usage.Counts{CostMicros: 151_030_000, PricedRequests: 5}},
			{label: "codex", Counts: usage.Counts{CostMicros: 30_900_000, PricedRequests: 2}},
			{label: otherAgents},
		},
		sessions: projectSessions(now),
		now:      func() time.Time { return now },
		ctx:      context.Background(),
	}
	m.rebuildAgentsTable()
	return m, func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := asked
		asked = nil
		return out
	}
}

// pressCmd sends one key through Update and returns its command.
func pressCmd(m *model, key tea.KeyMsg) tea.Cmd {
	_, cmd := m.Update(key)
	return cmd
}

var spaceKey = tea.KeyMsg{Type: tea.KeySpace}

// deliver runs cmd and passes each rows or projects reply it gives through Update, then delivers
// what Update returned in the same way. Other replies are dropped, and a command that blocks (a
// tick) is given up.
func deliver(m *model, cmd tea.Cmd) {
	var mu sync.Mutex
	var msgs []tea.Msg
	var wg sync.WaitGroup
	var run func(c tea.Cmd)
	run = func(c tea.Cmd) {
		if c == nil {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			done := make(chan tea.Msg, 1)
			go func() { done <- c() }()
			select {
			case msg := <-done:
				switch msg := msg.(type) {
				case tea.BatchMsg:
					for _, cc := range msg {
						run(cc)
					}
				case agentRowsLoadedMsg, agentProjectsLoadedMsg:
					mu.Lock()
					msgs = append(msgs, msg)
					mu.Unlock()
				}
			case <-time.After(300 * time.Millisecond):
			}
		}()
	}
	run(cmd)
	wg.Wait()
	for _, msg := range msgs {
		_, next := m.Update(msg)
		deliver(m, next)
	}
}

// agentCells is one column of the agents table, by title prefix.
func agentCells(t *testing.T, m *model, title string) []string {
	t.Helper()
	for i, c := range m.agentsTbl.Columns() {
		if strings.HasPrefix(headerTitle(c), title) {
			var out []string
			for _, r := range m.agentsTbl.Rows() {
				out = append(out, r[i])
			}
			return out
		}
	}
	t.Fatalf("no column %q", title)
	return nil
}

// onDesc puts the cursor on the row d.
func onDesc(t *testing.T, m *model, d agentRowDesc) {
	t.Helper()
	i := slices.Index(m.agentRowDescs, d)
	if i < 0 {
		t.Fatalf("no row %+v in %+v", d, m.agentRowDescs)
	}
	m.agentsTbl.SetCursor(i)
}

// openClaudeCode opens claude-code's row and delivers its projects.
func openClaudeCode(t *testing.T, m *model) {
	t.Helper()
	onRow(t, m, projAgent)
	deliver(m, pressCmd(m, spaceKey))
}

var (
	cortexRow = agentRowDesc{agent: projAgent, kind: agentRowProject, project: "~/works/cortex"}
	mocaRow   = agentRowDesc{agent: projAgent, kind: agentRowProject, project: "~/moca"}
	otherRow  = agentRowDesc{agent: projAgent, kind: agentRowProjectOther}
	allProjs  = agentRowDesc{agent: projAgent, kind: agentRowAllProjects}
)

func hasQuery(queries []string, parts ...string) bool {
	return slices.ContainsFunc(queries, func(q string) bool {
		for _, p := range parts {
			if !strings.Contains(q, p) {
				return false
			}
		}
		return true
	})
}

func TestAgentsProjects_NoProjectsNoMarkerAndSpacePagesDown(t *testing.T) {
	m, asked := projectsModel(t, defaultProjectSeries)
	for i := range m.sessions {
		m.sessions[i].Projects = nil
	}
	m.rebuildAgentsTable()
	for _, cell := range agentCells(t, m, "AGENT") {
		if strings.ContainsAny(cell, "▸▾") {
			t.Errorf("cell %q has a mark, and no session names a project", cell)
		}
	}
	tbl := m.agentsTbl
	tbl, _ = tbl.Update(spaceKey)
	runCmds(pressCmd(m, spaceKey))
	if m.agentsTbl.Cursor() != tbl.Cursor() {
		t.Errorf("space moved the cursor to %d; the table's page-down goes to %d", m.agentsTbl.Cursor(), tbl.Cursor())
	}
	if q := asked(); hasQuery(q, "group=project") {
		t.Errorf("space fetched projects: %v", q)
	}
}

func TestAgentsProjects_SpaceOnAnAgentFetchesItsProjects(t *testing.T) {
	m, asked := projectsModel(t, defaultProjectSeries)
	openClaudeCode(t, m)
	if q := asked(); !hasQuery(q, "agent=claude-code", "group=project") {
		t.Fatalf("queries %v; want one for claude-code's projects", q)
	}
	i := slices.Index(m.agentRowDescs, agentRowDesc{agent: projAgent, kind: agentRowHeading})
	if i < 0 {
		t.Fatalf("no heading row in %+v", m.agentRowDescs)
	}
	agents := agentCells(t, m, "AGENT")[i : i+5]
	if want := []string{"▾ claude-code", "    All projects", "    cortex", "    moca", "    Other"}; !slices.Equal(agents, want) {
		t.Errorf("AGENT cells %q, want %q", agents, want)
	}
	costs := agentCells(t, m, "COST")[i : i+5]
	if want := []string{"", "$151.03", "$97.40", "$38.11", "$15.52"}; !slices.Equal(costs, want) {
		t.Errorf("COST cells %q, want %q", costs, want)
	}
}

func TestAgentsProjects_SpaceOnAllAgentsFetchesWithoutAgent(t *testing.T) {
	m, asked := projectsModel(t, defaultProjectSeries)
	onRow(t, m, "")
	deliver(m, pressCmd(m, spaceKey))
	q := asked()
	if !hasQuery(q, "group=project") {
		t.Fatalf("queries %v; want a group=project one", q)
	}
	for _, s := range q {
		if strings.Contains(s, "group=project") && strings.Contains(s, "agent=") {
			t.Errorf("All agents asked %q; want no agent=", s)
		}
	}
	if got := agentCells(t, m, "AGENT")[0]; got != "▾ "+allAgentsLabel {
		t.Errorf("first cell %q", got)
	}
}

func TestAgentsProjects_SharedTailsShowTheShortestUniqueTail(t *testing.T) {
	m, _ := projectsModel(t, map[string]map[string]int64{projAgent: {
		"~/works/go/src/github.com/kagenti/kagenti-extensions": 2_000_000,
		"~/git/kagenti/kagenti-extensions":                     1_000_000,
	}})
	openClaudeCode(t, m)
	cells := agentCells(t, m, "AGENT")
	for _, want := range []string{"    github.com/kagenti/kagenti-extensions", "    git/kagenti/kagenti-extensions"} {
		if !slices.Contains(cells, want) {
			t.Errorf("no cell %q in %q", want, cells)
		}
	}
}

func TestAgentsProjects_OtherIsAbsentWhenZeroAndEmpty(t *testing.T) {
	m, _ := projectsModel(t, map[string]map[string]int64{projAgent: {"~/works/cortex": 1_000_000, "~/moca": 1_000}})
	m.sessions = slices.DeleteFunc(m.sessions, func(s session.SessionSummary) bool { return len(s.Projects) == 0 })
	m.rebuildAgentsTable()
	openClaudeCode(t, m)
	if slices.Contains(m.agentRowDescs, otherRow) {
		t.Errorf("an Other sub-row with no cost and no session: %+v", m.agentRowDescs)
	}
}

func TestAgentsProjects_OpenStateAndCursorSurviveRefetchAndW(t *testing.T) {
	m, _ := projectsModel(t, defaultProjectSeries)
	openClaudeCode(t, m)
	onDesc(t, m, mocaRow)
	_, cmd := m.Update(agentRowsLoadedMsg{
		rows:   []agentRow{{label: "codex", Counts: projectCounts(300_000_000)}, m.agents[0]},
		open:   agentsOpenNever,
		window: m.agentsSpanDef().window,
	})
	deliver(m, cmd)
	if d, _ := m.selectedAgentRow(); d != mocaRow {
		t.Fatalf("after a refetch the cursor is on %+v, want %+v", d, mocaRow)
	}
	deliver(m, pressCmd(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'w'}}))
	if !m.agentsExpanded[projAgent] {
		t.Error("w closed the row")
	}
	if d, _ := m.selectedAgentRow(); d != mocaRow {
		t.Errorf("after w the cursor is on %+v, want %+v", d, mocaRow)
	}
}

func TestAgentsProjects_SpaceOnASubRowClosesTheParent(t *testing.T) {
	m, _ := projectsModel(t, defaultProjectSeries)
	openClaudeCode(t, m)
	onDesc(t, m, cortexRow)
	runCmds(pressCmd(m, spaceKey))
	if len(m.agentsExpanded) != 0 {
		t.Errorf("rows still open: %v", m.agentsExpanded)
	}
	d, _ := m.selectedAgentRow()
	if d != (agentRowDesc{agent: projAgent, kind: agentRowAgent}) {
		t.Errorf("cursor on %+v, want the closed claude-code row", d)
	}
	if got := agentCells(t, m, "AGENT")[m.agentsTbl.Cursor()]; got != "▸ claude-code" {
		t.Errorf("cell under the cursor %q", got)
	}
}

func TestAgentsProjects_EnterOnAProjectRowScopesTheParent(t *testing.T) {
	m, _ := projectsModel(t, defaultProjectSeries)
	openClaudeCode(t, m)
	onDesc(t, m, cortexRow)
	pressCmd(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.agentScope != projAgent || m.pane != paneSessions {
		t.Errorf("scope %q pane %v; want claude-code on sessions", m.agentScope, m.pane)
	}
}

func TestAgentsProjects_EraseAndServerDoNothingOnAProjectRow(t *testing.T) {
	m, _ := projectsModel(t, defaultProjectSeries)
	openClaudeCode(t, m)
	for _, d := range []agentRowDesc{cortexRow, otherRow} {
		onDesc(t, m, d)
		pressCmd(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
		pressCmd(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'S'}})
		if m.clearConfirm != nil || m.serverNotice != "" {
			t.Errorf("%+v: x or S acted (confirm %+v, notice %q)", d, m.clearConfirm, m.serverNotice)
		}
		if strings.Contains(m.helpView(), "[S]") {
			t.Errorf("%+v: footer offers [S]", d)
		}
		m.clearConfirm, m.serverNotice = nil, ""
	}
	onDesc(t, m, allProjs)
	pressCmd(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if m.clearConfirm == nil || m.clearConfirm.agent != projAgent {
		t.Errorf("x on All projects: confirm %+v, want one for claude-code", m.clearConfirm)
	}
}

func TestAgentsProjects_BandFollowsTheParentAgent(t *testing.T) {
	m, asked := projectsModel(t, defaultProjectSeries)
	openClaudeCode(t, m)
	onRow(t, m, "codex")
	// The band follows codex first, as a key press there would make it.
	runCmds(m.syncSpendScope())
	asked()
	runCmds(pressCmd(m, tea.KeyMsg{Type: tea.KeyUp}))
	if d, _ := m.selectedAgentRow(); d != otherRow {
		t.Fatalf("cursor on %+v, want claude-code's Other sub-row", d)
	}
	got := bandAgents(asked())
	if len(got) == 0 {
		t.Fatal("no band fetch when the cursor moved from codex to a claude-code sub-row")
	}
	for _, a := range got {
		if a != projAgent {
			t.Errorf("band asked for %q, want claude-code", a)
		}
	}
	onDesc(t, m, mocaRow)
	if got := m.spendScopeAgent(); got != projAgent {
		t.Errorf("spendScopeAgent on moca = %q", got)
	}
}

func TestAgentsProjects_OtherAgentNeverExpands(t *testing.T) {
	m, asked := projectsModel(t, defaultProjectSeries)
	onRow(t, m, otherAgents)
	if got := agentCells(t, m, "AGENT")[m.agentsTbl.Cursor()]; !strings.HasPrefix(got, "  ") || strings.Contains(got, "▸") {
		t.Errorf("Other cell %q; want no mark", got)
	}
	runCmds(pressCmd(m, spaceKey))
	if len(m.agentsExpanded) != 0 || hasQuery(asked(), "group=project") {
		t.Errorf("space opened Other: %v", m.agentsExpanded)
	}
}

func TestAgentsProjects_SessionInTwoProjectsCountsInBoth(t *testing.T) {
	m, _ := projectsModel(t, defaultProjectSeries)
	openClaudeCode(t, m)
	cells := agentCells(t, m, "SESSIONS")
	for d, want := range map[agentRowDesc]string{cortexRow: "2", mocaRow: "2", otherRow: "1", allProjs: "4"} {
		if got := cells[slices.Index(m.agentRowDescs, d)]; got != want {
			t.Errorf("%+v: SESSIONS %q, want %q", d, got, want)
		}
	}
}

func TestAgentsProjects_ParentGoneKeepsTheCursorInRange(t *testing.T) {
	m, _ := projectsModel(t, defaultProjectSeries)
	openClaudeCode(t, m)
	onDesc(t, m, mocaRow)
	m.sessions = nil
	m.Update(agentRowsLoadedMsg{rows: []agentRow{m.agents[1]}, open: agentsOpenNever, window: m.agentsSpanDef().window})
	if _, ok := m.selectedAgentRow(); !ok {
		t.Errorf("cursor %d is on no row of %+v", m.agentsTbl.Cursor(), m.agentRowDescs)
	}
}

func TestAgentsProjects_FooterNamesTheFullLabel(t *testing.T) {
	const label = "~/works/cor\x1btex"
	m, _ := projectsModel(t, map[string]map[string]int64{projAgent: {label: 1_000_000}})
	m.sessions[0].Projects = []string{label}
	m.rebuildAgentsTable()
	onRow(t, m, projAgent)
	if got := m.helpView(); !strings.Contains(got, "[space] projects") {
		t.Errorf("closed row footer %q; want [space] projects", got)
	}
	deliver(m, pressCmd(m, spaceKey))
	onDesc(t, m, agentRowDesc{agent: projAgent, kind: agentRowProject, project: label})
	got := m.helpView()
	if !strings.Contains(got, sanitizeLabel(label)) || !strings.Contains(got, "[space] close") {
		t.Errorf("footer %q; want %q and [space] close", got, sanitizeLabel(label))
	}
	if strings.Contains(got, "\x1b") {
		t.Errorf("footer carries a raw escape: %q", got)
	}
}

func TestAgentsProjects_FailedFetchKeepsTheRows(t *testing.T) {
	m, _ := projectsModel(t, defaultProjectSeries)
	openClaudeCode(t, m)
	before := slices.Clone(m.agentsTbl.Rows())
	m.Update(agentProjectsLoadedMsg{agent: projAgent, err: errors.New("boom"), window: m.agentsSpanDef().window})
	if got := m.agentsTbl.Rows(); len(got) != len(before) || !slices.Contains(agentCells(t, m, "AGENT"), "    cortex") {
		t.Errorf("rows changed after a failed fetch: %v", got)
	}
	if v := m.View(); !strings.Contains(v, "refresh failed") {
		t.Errorf("view has no error line:\n%s", v)
	}
}
