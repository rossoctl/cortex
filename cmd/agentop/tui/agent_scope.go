package tui

import (
	"context"
	"time"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/cost/usage"
)

// fetchUsageScoped is one /v1/usage read for the spend band or drawer, narrowed to agent when set.
// Unscoped it is the plain read. Scoped it asks the server to narrow, and where the answer carries
// no agent echo — a server that predates agent= — narrows the group=agent series itself, the way
// the usage pane always has.
//
// OTHER IS ALWAYS NARROWED HERE, never sent: it is no label the server knows, so agent=Other would
// come back as zero. The cost is the breakdown — narrowed buckets carry no series, so every axis
// is served as none, and the drawer says so (drawerScopeNote) as it does for any window a server
// could not break down for one agent.
func fetchUsageScoped(ctx context.Context, client *apiclient.Client, window string, resolution time.Duration, agent string, group usage.Group) (*usage.Snapshot, error) {
	if agent == "" {
		return client.GetUsageWindow(ctx, window, resolution, "", group)
	}
	if agent == otherAgents {
		all, err := client.GetUsageWindow(ctx, window, resolution, "", usage.GroupAgent)
		if err != nil {
			return nil, err
		}
		scoped := usage.NarrowToAgent(*foldOtherAgents(all), agent)
		return &scoped, nil
	}
	snap, err := client.GetUsageWindowForAgent(ctx, window, resolution, "", agent, group)
	if err != nil || snap.Agent == agent {
		return snap, err
	}
	all, err := client.GetUsageWindow(ctx, window, resolution, "", usage.GroupAgent)
	if err != nil {
		return nil, err
	}
	scoped := usage.NarrowToAgent(*all, agent)
	return &scoped, nil
}

// narrowToProject narrows a group=project snapshot to one project. For Other it first folds the
// unknown and overflow series into Other, as fetchUsageScoped does for the agent Other, so the
// figure is the one the project sub-row shows.
func narrowToProject(snap *usage.Snapshot, project string) *usage.Snapshot {
	if snap == nil {
		return nil
	}
	if project == otherProjects {
		snap = foldSeriesInto(snap, namedProject, otherProjects)
	}
	scoped := usage.NarrowToLabel(*snap, project)
	return &scoped
}

// fetchUsageScopedProject is fetchUsageScoped narrowed to one project when project is set: it
// asks for the project axis (of agent, when set) and narrows the reply here. No project= key goes
// to the server.
func fetchUsageScopedProject(ctx context.Context, client *apiclient.Client, window string, resolution time.Duration, agent, project string, group usage.Group) (*usage.Snapshot, error) {
	if project == "" {
		return fetchUsageScoped(ctx, client, window, resolution, agent, group)
	}
	snap, err := fetchUsageScoped(ctx, client, window, resolution, agent, usage.GroupProject)
	if err != nil {
		return nil, err
	}
	return narrowToProject(snap, project), nil
}

// sessionsScope is the agent the sessions list is narrowed to: the scope, when some session names
// its agent, else "" — a server that names none cannot say which sessions are whose.
func (m *model) sessionsScope() string {
	if m.agentScope == "" || !m.sessionsNameAgents() {
		return ""
	}
	return m.agentScope
}

// sessionsProjectScope is the project the sessions list is narrowed to: the scope, when some
// session names a project, else "" — a server that records no project cannot say which sessions
// are in one, and the list must not go empty.
func (m *model) sessionsProjectScope() string {
	if m.projectScope == "" || !m.sessionsNameProjects() {
		return ""
	}
	return m.projectScope
}

// spendAxes are the drawer's axes: every one, less the agent axis under a scope, where it would
// be a single row. The DRAWER'S scope, which on the agents pane is the highlighted row.
func (m *model) spendAxes() []usage.Group {
	if m.spendScopeAgent() == "" {
		return spendDrawerAxes
	}
	out := make([]usage.Group, 0, len(spendDrawerAxes))
	for _, a := range spendDrawerAxes {
		if a != usage.GroupAgent {
			out = append(out, a)
		}
	}
	return out
}

// spendAxis is the drawer's axis. Under a scope an index left on the agent axis reads as the
// first axis instead.
func (m *model) spendAxis() usage.Group {
	a := m.spend.axis()
	if m.spendScopeAgent() != "" && a == usage.GroupAgent {
		return spendDrawerAxes[0]
	}
	return a
}
