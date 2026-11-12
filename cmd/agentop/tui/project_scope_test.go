package tui

import (
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/pipeline"
)

var enterKey = tea.KeyMsg{Type: tea.KeyEnter}

func wantScopes(t *testing.T, m *model, agent, project string) {
	t.Helper()
	if m.agentScope != agent || m.projectScope != project {
		t.Errorf("scopes (%q, %q), want (%q, %q)", m.agentScope, m.projectScope, agent, project)
	}
}

func wantRows(t *testing.T, m *model, want ...string) {
	t.Helper()
	if !slices.Equal(m.sessionRowIDs, want) {
		t.Errorf("sessions rows %q, want %q", m.sessionRowIDs, want)
	}
}

func TestProjectScope_EnterOnAProjectSetsBothScopes(t *testing.T) {
	m, _ := projectsModel(t, defaultProjectSeries)
	openClaudeCode(t, m)
	onDesc(t, m, cortexRow)
	pressCmd(m, enterKey)
	wantScopes(t, m, projAgent, "~/works/cortex")
	if m.pane != paneSessions {
		t.Errorf("pane %v, want sessions", m.pane)
	}
	wantRows(t, m, "s1", "s3")
}

func TestProjectScope_EnterOnOtherListsSessionsWithNoProject(t *testing.T) {
	m, _ := projectsModel(t, defaultProjectSeries)
	openClaudeCode(t, m)
	onDesc(t, m, otherRow)
	pressCmd(m, enterKey)
	wantScopes(t, m, projAgent, otherProjects)
	wantRows(t, m, "s4")
}

func TestProjectScope_EnterOnAllProjectsClearsOnlyTheProject(t *testing.T) {
	m, _ := projectsModel(t, defaultProjectSeries)
	openClaudeCode(t, m)
	m.agentScope, m.projectScope = projAgent, "~/moca"
	onDesc(t, m, allProjs)
	pressCmd(m, enterKey)
	wantScopes(t, m, projAgent, "")
}

func TestProjectScope_EnterOnAllAgentsClearsBoth(t *testing.T) {
	m, _ := projectsModel(t, defaultProjectSeries)
	openClaudeCode(t, m)
	m.agentScope, m.projectScope = projAgent, "~/moca"
	m.agentsTbl.SetCursor(0)
	deliver(m, pressCmd(m, enterKey)) // opens All agents
	onDesc(t, m, agentRowDesc{kind: agentRowAllProjects})
	pressCmd(m, enterKey)
	wantScopes(t, m, "", "")
}

func TestProjectScope_EnterOnAnAgentRowClearsTheProject(t *testing.T) {
	m, _ := projectsModel(t, defaultProjectSeries)
	openClaudeCode(t, m)
	m.agentScope, m.projectScope = projAgent, "~/moca"
	onRow(t, m, "codex")
	deliver(m, pressCmd(m, enterKey)) // opens codex
	onDesc(t, m, agentRowDesc{agent: "codex", kind: agentRowAllProjects})
	pressCmd(m, enterKey)
	wantScopes(t, m, "codex", "")
}

// Enter on a row that opens is a toggle: it opens, then closes, and scopes nothing.
func TestProjectScope_EnterTogglesAnAgentRow(t *testing.T) {
	m, _ := projectsModel(t, defaultProjectSeries)
	onRow(t, m, projAgent)
	deliver(m, pressCmd(m, enterKey))
	if !m.agentRowOpen(projAgent) || m.pane != paneAgents {
		t.Fatalf("after Enter: open=%v pane=%v; want open on the agents pane", m.agentRowOpen(projAgent), m.pane)
	}
	onDesc(t, m, agentRowDesc{agent: projAgent, kind: agentRowHeading})
	pressCmd(m, enterKey)
	if m.agentRowOpen(projAgent) || m.pane != paneAgents {
		t.Fatalf("after 2nd Enter: open=%v pane=%v; want closed on the agents pane", m.agentRowOpen(projAgent), m.pane)
	}
	wantScopes(t, m, "", "")
}

// Without projects no row opens, so Enter scopes as before rather than doing nothing.
func TestProjectScope_EnterScopesARowThatCannotOpen(t *testing.T) {
	m, _ := projectsModel(t, defaultProjectSeries)
	for i := range m.sessions {
		m.sessions[i].Projects = nil
	}
	m.rebuildAgentsTable()
	onRow(t, m, projAgent)
	pressCmd(m, enterKey)
	wantScopes(t, m, projAgent, "")
	if m.pane != paneSessions {
		t.Errorf("pane %v; want sessions", m.pane)
	}
}

func TestProjectScope_AProjectUnderAllAgentsHasNoAgent(t *testing.T) {
	series := map[string]map[string]int64{"": {"~/works/cortex": 1_000_000, "~/moca": 2_000_000}}
	m, _ := projectsModel(t, series)
	m.agentsTbl.SetCursor(0)
	deliver(m, pressCmd(m, spaceKey))
	onDesc(t, m, agentRowDesc{kind: agentRowProject, project: "~/works/cortex"})
	pressCmd(m, enterKey)
	wantScopes(t, m, "", "~/works/cortex")
	wantRows(t, m, "s1", "s3")
}

func TestProjectScope_NoSessionNamesAProjectListsEverything(t *testing.T) {
	m, _ := projectsModel(t, defaultProjectSeries)
	for i := range m.sessions {
		m.sessions[i].Projects = nil
	}
	m.agentScope, m.projectScope = projAgent, "~/moca"
	m.pane = paneSessions
	m.rebuildSessionsTable()
	wantRows(t, m, "s1", "s2", "s3", "s4")
	if f := m.footerView(); !strings.Contains(f, "no session names its project") {
		t.Errorf("footer %q names no unscoped project", f)
	}
}

func TestProjectScope_TitlesNameTheProject(t *testing.T) {
	m, _ := projectsModel(t, defaultProjectSeries)
	openClaudeCode(t, m)
	title := func() string { return strings.SplitN(m.paneView(), "\n", 2)[0] }

	onDesc(t, m, cortexRow)
	if got := title(); !strings.Contains(got, "project=cortex") {
		t.Errorf("agents title %q names no project", got)
	}
	m.selectedSess = "s1"
	for _, project := range []string{"~/works/cortex", "~/x\x1bq"} {
		m.agentScope, m.projectScope = projAgent, project
		for _, p := range []paneID{paneSessions, paneUsage, paneEvents} {
			m.pane = p
			got := title()
			if strings.Contains(got, "\x1bq") {
				t.Errorf("pane %v title %q carries the raw label", p, got)
			}
			if project != "~/works/cortex" {
				continue
			}
			if !strings.Contains(got, "agent=claude-code") || !strings.Contains(got, "project=cortex") {
				t.Errorf("pane %v title %q, want agent=claude-code and project=cortex", p, got)
			}
		}
	}
	for _, s := range []string{m.scopeSuffix(projAgent, "~/x\x1bq"), m.sessionHeader("s1", "")} {
		if strings.Contains(s, "\x1b") {
			t.Errorf("title part %q carries an escape", s)
		}
	}
}

func TestProjectScope_EscKeepsTheScope(t *testing.T) {
	m, _ := projectsModel(t, defaultProjectSeries)
	openClaudeCode(t, m)
	onDesc(t, m, cortexRow)
	pressCmd(m, enterKey)
	m.pane = paneAgents
	pressCmd(m, tea.KeyMsg{Type: tea.KeyEsc})
	wantScopes(t, m, projAgent, "~/works/cortex")
}

func TestProjectScope_EntryOpensTheParentAndPutsTheCursorOnTheRow(t *testing.T) {
	m, _ := projectsModel(t, defaultProjectSeries)
	m.agentScope, m.projectScope = projAgent, "~/moca"
	delete(m.agentsExpanded, projAgent)
	m.pane = paneSessions
	deliver(m, m.fetchAgentRowsCmd(agentsOpenOnPress, paneSessions))
	if !m.agentsExpanded[projAgent] {
		t.Fatalf("claude-code's row is not open")
	}
	if d, _ := m.selectedAgentRow(); d != mocaRow {
		t.Errorf("cursor on %+v, want %+v", d, mocaRow)
	}
}

func TestProjectScope_ReturnToAgentsPutsTheCursorOnTheRow(t *testing.T) {
	m, _ := projectsModel(t, defaultProjectSeries)
	m.agentScope, m.projectScope = projAgent, otherProjects
	m.pane = paneSessions
	deliver(m, m.returnToAgentsPane())
	if d, _ := m.selectedAgentRow(); d != otherRow {
		t.Errorf("cursor on %+v, want %+v", d, otherRow)
	}
}

func TestClear_EraseAgentIgnoresTheProjectScope(t *testing.T) {
	m, _ := projectsModel(t, defaultProjectSeries)
	m.projectScope = "~/moca"
	cc := &clearConfirm{target: eraseAgent, agent: projAgent}
	m.clearConfirm = cc
	m.applyClearCounts(clearCountsMsg{cc: cc, list: apiclient.SessionList{Sessions: m.sessions}})
	if want := []string{"s1", "s2", "s3", "s4"}; !slices.Equal(cc.ids, want) {
		t.Errorf("erase ids %q, want %q", cc.ids, want)
	}
}

func TestProjectScope_ProjectsReplyThatMovesTheCursorResyncsTheBand(t *testing.T) {
	m, _ := projectsModel(t, defaultProjectSeries)
	openClaudeCode(t, m)
	data := m.agentProjects[projAgent]
	delete(m.agentProjects, projAgent)
	m.agentScope, m.projectScope = projAgent, "~/works/cortex"
	m.revealProjectScope()
	m.rebuildAgentsTable()
	runCmds(m.syncSpendScope())
	if m.spend.bandProject != "" {
		t.Fatalf("before the reply bandProject %q, want the whole agent", m.spend.bandProject)
	}
	_, cmd := m.Update(agentProjectsLoadedMsg{agent: projAgent, data: data, window: m.agentsSpanDef().window})
	if d, _ := m.selectedAgentRow(); d != cortexRow {
		t.Fatalf("the reply put the cursor on %+v, want %+v", d, cortexRow)
	}
	runCmds(cmd)
	if m.spend.bandProject != "~/works/cortex" {
		t.Errorf("after the reply bandProject %q, want ~/works/cortex", m.spend.bandProject)
	}
}

func TestProjectScope_APathPastTheLabelCapMatchesItsSeriesLabel(t *testing.T) {
	m, _ := projectsModel(t, defaultProjectSeries)
	long := "/home/u/" + strings.Repeat("deep/", 30) + "repo"
	label := long[len(long)-96:] // the usage side keeps the last 96 bytes of a path
	m.sessions[0].Projects = []string{long}
	m.agentScope, m.projectScope = projAgent, label
	m.pane = paneSessions
	m.rebuildSessionsTable()
	wantRows(t, m, m.sessions[0].ID)
	if n, _ := m.subRowSessions(agentRowDesc{agent: projAgent, kind: agentRowProject, project: label}); n != 1 {
		t.Errorf("the sub-row counts %d sessions, want 1", n)
	}
}

func TestProjectScope_ACachedOnlySessionListsOnlyUnderNoNamedProject(t *testing.T) {
	m, _ := projectsModel(t, defaultProjectSeries)
	m.events = map[string][]pipeline.SessionEvent{"gone": {{Host: "example.test"}}}
	m.pane = paneSessions
	for project, want := range map[string]bool{"": true, otherProjects: true, "~/works/cortex": false} {
		m.projectScope = project
		m.rebuildSessionsTable()
		if got := slices.Contains(m.sessionRowIDs, "gone"); got != want {
			t.Errorf("project scope %q: cached-only row listed %v, want %v", project, got, want)
		}
	}
}

func TestProjectScope_TimelineTitleNamesTheScopeOnce(t *testing.T) {
	m, _ := projectsModel(t, defaultProjectSeries)
	m.agentScope, m.projectScope = projAgent, "~/works/cortex"
	m.pane = paneTimeline
	m.timeline.session = "s1"
	title := strings.SplitN(m.paneView(), "\n", 2)[0]
	if n := strings.Count(title, "project=cortex"); n != 1 {
		t.Errorf("timeline title %q names the project %d times, want 1", title, n)
	}
}
