package tui

import (
	"context"
	"maps"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/session"
)

// otherProjects is the project sub-row for spend that no named project holds. It is the same
// string as otherAgents, so agentRowsFromBuckets puts it last. A project label is a path ("/…" or
// "~…"), so no real project has this name.
const otherProjects = otherAgents

// projectOverflowLabel is the server's series for the overflow past MaxSeriesInResponse. It
// mirrors the unexported overflowLabel in core/cost/usage/labels.go.
const projectOverflowLabel = "(other)"

// namedProject reports whether a group=project series is one project, not the unknown or the
// overflow series. "~" is a named project.
func namedProject(label string) bool {
	return label != usage.UnknownProjectLabel && label != projectOverflowLabel
}

// projectRowsFromSnapshot gives one row per project of a group=project snapshot, in
// usage.SortSeriesLabels order, with the unknown and overflow series merged into otherProjects,
// which is last.
func projectRowsFromSnapshot(snap *usage.Snapshot) []agentRow {
	return seriesRowsFromSnapshot(foldSeriesInto(snap, namedProject, otherProjects))
}

// projectDisplayNames gives the name to show for each project label, in the same order: the
// shortest tail of "/"-separated segments that no other label has as its tail of the same segment
// count. When no tail is unique (a duplicate label), it is the full label.
func projectDisplayNames(labels []string) []string {
	segs := make([][]string, len(labels))
	for i, l := range labels {
		segs[i] = strings.Split(l, "/")
	}
	tail := func(s []string, k int) string { return strings.Join(s[len(s)-k:], "/") }
	out := make([]string, len(labels))
	for i, s := range segs {
		out[i] = labels[i]
		for k := 1; k <= len(s); k++ {
			t := tail(s, k)
			unique := true
			for j, o := range segs {
				if j != i && len(o) >= k && tail(o, k) == t {
					unique = false
					break
				}
			}
			if unique {
				out[i] = t
				break
			}
		}
	}
	return out
}

// agentProjectsData is one open row's group=project fetch: all is the window total, rows the
// project sub-rows.
type agentProjectsData struct {
	all  agentRow
	rows []agentRow
}

// agentProjectsLoadedMsg carries one open row's projects back to Update. window is the window the
// reply answers for, so a reply that `w` has outrun is dropped.
type agentProjectsLoadedMsg struct {
	agent  string
	data   agentProjectsData
	err    error
	window string
}

// fetchAgentProjectsCmd requests the projects of one agent row ("" is All agents) for the pane's
// window.
func (m *model) fetchAgentProjectsCmd(agent string) tea.Cmd {
	if m.client == nil {
		return nil
	}
	client := m.client
	// Captured here, on the update goroutine, for the reason fetchSpendSpan records.
	def := m.agentsSpanDef()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), agentsFetchTimeout)
		defer cancel()
		snap, err := fetchUsageScoped(ctx, client, def.window, def.resolution, agent, usage.GroupProject)
		msg := agentProjectsLoadedMsg{agent: agent, err: err, window: def.window}
		if err == nil {
			msg.data = agentProjectsData{
				all:  agentRow{Counts: snap.Totals, units: snap.Currencies},
				rows: projectRowsFromSnapshot(snap),
			}
		}
		return msg
	}
}

// fetchOpenProjectsCmd refetches the projects of every open row.
func (m *model) fetchOpenProjectsCmd() tea.Cmd {
	var cmds []tea.Cmd
	for _, agent := range slices.Sorted(maps.Keys(m.agentsExpanded)) {
		cmds = append(cmds, m.fetchAgentProjectsCmd(agent))
	}
	return tea.Batch(cmds...)
}

// sessionsNameProjects reports whether any listed session names a project. Without one, no row
// opens and the pane is as it was before projects.
func (m *model) sessionsNameProjects() bool {
	return slices.ContainsFunc(m.sessions, func(s session.SessionSummary) bool { return len(s.Projects) > 0 })
}

// agentRowExpandable reports whether space opens the agent row label. Other never opens.
func (m *model) agentRowExpandable(label string) bool {
	return label != otherAgents && m.sessionsNameProjects()
}

// agentRowOpen reports whether the agent row label shows as open.
func (m *model) agentRowOpen(label string) bool {
	return m.agentsExpanded[label] && m.agentRowExpandable(label)
}

// subRowSessions counts the listed sessions a sub-row d holds, and gives the newest UpdatedAt.
// A session in two projects counts in both.
func (m *model) subRowSessions(d agentRowDesc) (n int, last time.Time) {
	for _, s := range m.sessions {
		if foldedInto(s) != "" || (d.agent != "" && !agentRowLists(s, d.agent)) {
			continue
		}
		switch d.kind {
		case agentRowProject:
			// shortcut: an exact match on the series label, so a project path longer than
			// event.MaxLabelLen (which the usage side truncates) counts in no row; match on the
			// truncated form when a real path hits it.
			if !slices.Contains(s.Projects, d.project) {
				continue
			}
		case agentRowProjectOther:
			if len(s.Projects) > 0 {
				continue
			}
		}
		n++
		if s.UpdatedAt.After(last) {
			last = s.UpdatedAt
		}
	}
	return n, last
}

// toggleAgentRow is space on the agents pane. On a closed row that can open, it opens the row and
// fetches its projects. On an open row or a sub-row, it closes the row and the cursor goes to it.
// ok is false on every other row, so space pages the table there.
func (m *model) toggleAgentRow() (tea.Cmd, bool) {
	d, ok := m.selectedAgentRow()
	if !ok {
		return nil, false
	}
	if d.kind != agentRowAgent {
		// rebuildAgentsTable puts the cursor on the first row of the same agent: the closed row.
		delete(m.agentsExpanded, d.agent)
		if len(m.agentsExpanded) == 0 {
			// No open row fetches again, so nothing else would clear it.
			m.agentProjectsErr = nil
		}
		m.rebuildAgentsTable()
		return nil, true
	}
	if !m.agentRowExpandable(d.agent) {
		return nil, false
	}
	if m.agentsExpanded == nil {
		m.agentsExpanded = map[string]bool{}
	}
	m.agentsExpanded[d.agent] = true
	m.rebuildAgentsTable()
	return m.fetchAgentProjectsCmd(d.agent), true
}

// selectedAgentActs is the agent that x and S act on: selectedAgentScope, but ok is false on a
// project or Other sub-row, which is not the whole agent.
func (m *model) selectedAgentActs() (string, bool) {
	d, ok := m.selectedAgentRow()
	if !ok || d.kind == agentRowProject || d.kind == agentRowProjectOther {
		return "", false
	}
	return d.agent, true
}

// projectScopeName is the name a title shows for the project label under agent: "Other" for
// otherProjects; the sub-row's name while that row's projects are loaded; else the last "/"
// segment of the label.
func (m *model) projectScopeName(agent, project string) string {
	if project == otherProjects {
		return otherProjects
	}
	if data, ok := m.agentProjects[agent]; ok {
		var labels []string
		for _, r := range data.rows {
			if r.label != otherProjects {
				labels = append(labels, r.label)
			}
		}
		if i := slices.Index(labels, project); i >= 0 {
			return projectDisplayNames(labels)[i]
		}
	}
	return project[strings.LastIndex(project, "/")+1:]
}

// scopeSuffix is the title text that names a scope: " · agent=<agent>" when agent is set, then
// " · project=<name>" when project is set. Both are request-controlled, so both are sanitised.
func (m *model) scopeSuffix(agent, project string) string {
	out := ""
	if agent != "" {
		out += " · agent=" + sanitizeLabel(agent)
	}
	if project != "" {
		out += " · project=" + sanitizeLabel(m.projectScopeName(agent, project))
	}
	return out
}

// revealProjectScope opens the scoped agent's row and asks rebuildAgentsTable to put the cursor on
// the committed project sub-row, now or when the row's projects arrive. Call it before
// rebuildAgentsTable.
func (m *model) revealProjectScope() {
	if m.projectScope == "" || !m.agentRowExpandable(m.agentScope) {
		return
	}
	if m.agentsExpanded == nil {
		m.agentsExpanded = map[string]bool{}
	}
	m.agentsExpanded[m.agentScope] = true
	want := agentRowDesc{agent: m.agentScope, kind: agentRowProject, project: m.projectScope}
	if m.projectScope == otherProjects {
		want = agentRowDesc{agent: m.agentScope, kind: agentRowProjectOther}
	}
	m.agentsCursorWant, m.agentsCursorWantAt = &want, m.agentsTbl.Cursor()
}

// agentRowAdder is how rebuildAgentsTable adds one row: its descriptor and its cells.
type agentRowAdder func(d agentRowDesc, agent, sessions, lastSeen, cost, server string)

// addProjectRows adds the sub-rows of the open row agent: All projects, one row per project, then
// Other. a is the agent's own row, nil for All agents. Until the first reply lands, only All
// projects shows.
func (m *model) addProjectRows(add agentRowAdder, agent string, a *agentRow, now time.Time) {
	const indent = "    "
	data, loaded := m.agentProjects[agent]
	sessions := func(d agentRowDesc) (string, string) {
		n, last := m.subRowSessions(d)
		if n == 0 {
			return emptyCell, relTime(now, last)
		}
		return formatCount(n), relTime(now, last)
	}

	all := agentRowDesc{agent: agent, kind: agentRowAllProjects}
	n, last := sessions(all)
	// Under an agent, its own row's figures, so opening a row changes no number on screen.
	cost, server := "", ""
	if a != nil {
		cost = agentCostCellIn(a.Counts, a.units, agentsCostWidth)
		router, _ := m.activeRouter()
		server = agentServerCell(router, agent)
	} else if loaded {
		cost = agentCostCellIn(data.all.Counts, data.all.units, agentsCostWidth)
	}
	add(all, indent+"All projects", n, last, cost, server)
	if !loaded {
		return
	}

	var named []agentRow
	other := agentRow{label: otherProjects}
	for _, r := range data.rows {
		if r.label == otherProjects {
			other = r
			continue
		}
		named = append(named, r)
	}
	labels := make([]string, len(named))
	for i, r := range named {
		labels[i] = r.label
	}
	for i, name := range projectDisplayNames(labels) {
		d := agentRowDesc{agent: agent, kind: agentRowProject, project: named[i].label}
		n, last := sessions(d)
		add(d, indent+sanitizeLabel(name), n, last,
			agentCostCellIn(named[i].Counts, named[i].units, agentsCostWidth), "")
	}
	od := agentRowDesc{agent: agent, kind: agentRowProjectOther}
	if count, _ := m.subRowSessions(od); other.CostMicros == 0 && count == 0 {
		return
	}
	n, last = sessions(od)
	add(od, indent+otherProjects, n, last, agentCostCellIn(other.Counts, other.units, agentsCostWidth), "")
}
