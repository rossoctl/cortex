package tui

import (
	"slices"

	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// otherAgents is the AGENTS row, and the scope, for everything no recognised agent accounts for:
// every unrecognised User-Agent's traffic, and every session that names no recognised agent.
//
// WHY ONE ROW AND NOT ONE PER User-Agent. The server reports an unrecognised agent under its raw
// User-Agent, and one program sends several — the IBM Bob IDE was three rows before it was
// recognised (#1210), and the one that read as Bob carried none of its tokens. Worse, none of
// those rows could reach a session: a session names an agent only when the proxy recognised it,
// so picking a raw User-Agent scoped the list to nothing. Pooling them makes the picker a
// partition — every listed session belongs to exactly one row — so no pick can hide a session
// that no other pick shows.
//
// NOT A LABEL THE SERVER KNOWS, so it never goes on the wire as agent=: a proxy would narrow to
// the literal string and answer zero. fetchUsageScoped and fetchUsage fold it client-side
// instead. It cannot collide with a real agent either way — "Other" is no canonical name, and a
// client that sent "User-Agent: Other" is unrecognised and belongs here anyway.
//
// The raw User-Agents stay visible where they were: the unscoped spend drawer's agent axis and
// `agentop cost --by agent` both show them, which is how a new agent gets noticed and added to
// pipeline's knownClients.
const otherAgents = "Other"

// knownAgentLabel reports whether label is a recognised agent's row rather than Other's.
//
// FOLDED FIRST, so a versioned label from a server that predates the fold ("claude-code/2.1.285")
// still counts as Claude Code. The same predicate decides series and sessions alike, which is
// what keeps the partition consistent across version skew: an agent the proxy recognises and this
// binary does not lands in Other on BOTH sides, with its sessions beside its spend.
func knownAgentLabel(label string) bool {
	return pipeline.IsKnownAgent(pipeline.AgentName(label))
}

// inOtherAgents reports whether a session belongs to the Other row: it names no recognised agent.
// That includes the default bucket, which is no one agent's. A pending bucket belongs to the agent
// its id names; see sessionAgent.
func inOtherAgents(s session.SessionSummary) bool {
	return !knownAgentLabel(sessionAgent(s))
}

// foldOtherAgents returns a copy of a group=agent snapshot with every unrecognised series merged
// into one otherAgents series, so ScopeToAgent, NarrowToAgent and agentRowsFromSnapshot all treat
// Other as one more agent with no code of their own.
//
// THE CAP'S "(other)" BAND FOLDS IN TOO. It is the server's overflow past MaxSeriesInResponse —
// traffic no series accounts for — so it is unattributable by construction, which is this row's
// definition. Totals and every window-level field are untouched: the series are regrouped, not
// recounted.
//
// Counts.Add, so a fold of several saturated series saturates rather than wrapping.
//
// SeriesCurrencies FOLLOW THE SERIES: Other's units are the union of the units its members
// reported. A member the producer named no units for makes Other's units unknown, and unknown
// falls back to the window's list — over-refusing a mixed figure rather than labelling one agent's
// spend with another's units, the same trade agentRowsFromSnapshot makes.
func foldOtherAgents(snap *usage.Snapshot) *usage.Snapshot {
	if snap == nil {
		return nil
	}
	out := *snap
	out.Buckets = make([]usage.Bucket, len(snap.Buckets))
	var members []string
	for i, b := range snap.Buckets {
		out.Buckets[i] = b
		if b.Series == nil {
			continue
		}
		series := make(map[string]usage.Counts, len(b.Series))
		for label, c := range b.Series {
			key := label
			if !knownAgentLabel(label) {
				key = otherAgents
				if !slices.Contains(members, label) {
					members = append(members, label)
				}
			}
			cur := series[key]
			cur.Add(c)
			series[key] = cur
		}
		out.Buckets[i].Series = series
	}
	if snap.SeriesCurrencies != nil {
		out.SeriesCurrencies = make(map[string][]string, len(snap.SeriesCurrencies))
		var units []string
		unknown := false
		for _, label := range members {
			u, ok := snap.SeriesCurrencies[label]
			if !ok {
				unknown = true
			}
			for _, unit := range u {
				if !slices.Contains(units, unit) {
					units = append(units, unit)
				}
			}
		}
		for label, u := range snap.SeriesCurrencies {
			if knownAgentLabel(label) {
				out.SeriesCurrencies[label] = u
			}
		}
		if len(members) > 0 {
			if unknown {
				units = append([]string(nil), snap.Currencies...)
			}
			slices.Sort(units)
			out.SeriesCurrencies[otherAgents] = units
		}
	}
	return &out
}

// otherSessionsCount is how many sessions the Other scope lists: every server-listed session that
// names no recognised agent, plus the cached-only rows — sessions the server no longer lists,
// which therefore name no agent at all.
func (m *model) otherSessionsCount() int {
	n := 0
	for _, s := range m.sessions {
		if agentRowLists(s, otherAgents) {
			n++
		}
	}
	n += len(m.cachedOnlySessionIDs())
	return n
}

// pickerRows is the AGENTS table's rows: agentChoices, plus an Other row when sessions belong to
// Other but no unrecognised traffic in the window put one there.
//
// THAT EXTRA ROW NEVER OPENS THE PANE BY ITSELF, which is why it is added here and not in
// agentChoices, which agentsPaneApplies counts. The default bucket names no agent and exists on
// nearly every proxy, so counting it would put the picker in front of every single-agent user at
// startup — the cost agentsPaneApplies exists to avoid. Where the pane opens anyway it is shown,
// so a scope never leaves the default bucket unreachable.
//
// Only when some session names its agent: with none named the list ignores every scope (see
// sessionsScope), so an Other row would scope nothing.
func (m *model) pickerRows() []agentRow {
	choices := m.agentChoices()
	for _, a := range choices {
		if a.label == otherAgents {
			return choices
		}
	}
	if !m.sessionsNameAgents() || m.otherSessionsCount() == 0 {
		return choices
	}
	return append(slices.Clip(choices), agentRow{label: otherAgents})
}
