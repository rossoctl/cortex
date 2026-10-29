package tui

import (
	"context"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/cmd/agentop/money"
	"github.com/rossoctl/cortex/cmd/agentop/tui/table"
	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/session"
)

// agentRow is one coding agent's totals for the whole window, as the AGENTS pane shows them.
//
// The label is pipeline.AgentName of pipeline.EventClient.Label — "claude-code", every release
// together — or the raw User-Agent for an agent the parser did not recognise. CLIENT-ASSERTED AND SPOOFABLE, like every other use
// of that field: a display and scoping key, never an authorization subject.
type agentRow struct {
	label string
	usage.Counts
	// units is what this agent's figures are in, as Snapshot.SeriesCurrencies (or, from an older
	// server, the window's Currencies) reports it; nil is dollars. See agentCostCellIn.
	units []string
}

// agentRowsFromBuckets folds a snapshot's per-agent series into one row per agent.
//
// The snapshot carries per-agent figures PER BUCKET, while this pane shows one row per agent
// for the window, so this fold is the pane's whole data path. Callers pass the buckets from a
// snapshot fetched with usage.GroupAgent; any other grouping yields rows labelled by that
// grouping's keys instead, which is the caller's error to avoid and not something this can
// detect — Bucket.Series does not record which axis produced it.
//
// THE FOLD ITSELF IS usage.FoldSeriesAcrossWindow, not a loop here. It saturates through
// Counts.Add rather than wrapping, and `agentop cost --agent` needs the identical answer — two
// copies would be two definitions of what a window total means. The drawer's rankSeriesByCost
// records what the alternative cost when it did write its own: a wrapped total ranks BELOW a
// ten-micro series, which here would sort the busiest agent to the bottom of the table.
//
// ORDERED BY usage.SortSeriesLabels, not by a comparison written here. `agentop cost --by` ranks
// the same series for the same reason, so the rule has one definition in core and both surfaces
// call it — the tie-break on the label matters more than it sounds, because every unpriced agent
// has CostMicros 0 and until billing units land the label is the entire order for all of them.
// That function's godoc carries why it takes a slice rather than the map.
// agentRowsFromSnapshot is agentRowsFromBuckets with each row's units attached: the agent's own
// SeriesCurrencies entry when the server sent one, else the window's list, so an older server's
// mixed window withholds every row rather than labelling one agent with another's units.
//
// UNRECOGNISED AGENTS ARE ONE ROW, otherAgents, folded here by foldOtherAgents before anything is
// counted. See otherAgents for why a row per raw User-Agent was a row that scoped nothing.
func agentRowsFromSnapshot(snap *usage.Snapshot) []agentRow {
	snap = foldOtherAgents(snap)
	rows := agentRowsFromBuckets(snap.Buckets)
	for i := range rows {
		units, ok := snap.SeriesCurrencies[rows[i].label]
		if !ok {
			units = snap.Currencies
		}
		rows[i].units = units
	}
	return rows
}

func agentRowsFromBuckets(buckets []usage.Bucket) []agentRow {
	totals := usage.FoldSeriesAcrossWindow(buckets)
	labels := make([]string, 0, len(totals))
	for label := range totals {
		labels = append(labels, label)
	}
	usage.SortSeriesLabels(labels, totals)
	// OTHER GOES LAST whatever it spent: it is the remainder, not an agent competing for rank, and
	// a catch-all sorted above a named agent reads as the busiest program on the machine.
	if i := slices.Index(labels, otherAgents); i >= 0 {
		labels = append(append(labels[:i:i], labels[i+1:]...), otherAgents)
	}
	out := make([]agentRow, 0, len(labels))
	for _, label := range labels {
		out = append(out, agentRow{label: label, Counts: totals[label]})
	}
	return out
}

// agentsPaneApplies reports whether the startup gate enters the AGENTS pane: two or more agents.
// A one-row picker at startup is a keystroke that changes nothing, and one agent is the common
// case. An `A` press opens the pane whatever this says.
func agentsPaneApplies(rows []agentRow) bool {
	return len(rows) >= 2
}

// agentChoices is m.agents plus a row for every recognised agent that owns a listed session but
// has no row there, because the window saw no traffic from it. It is what agentsPaneApplies is
// asked about, and what pickerRows builds on.
//
// THE PICKER SCOPES THE SESSIONS LIST, AND THE LIST IS NOT WINDOWED. Sessions outlive the day
// (session.ttl defaults to never), so rows taken from today's spending alone go stale at midnight:
// the morning after a night of three agents, the list still held OpenCode's and Bob's sessions
// while the window had seen only Claude Code, so the picker offered nothing that could narrow the
// list to the other two. An agent with sessions is an agent the reader can pick, whatever it spent
// today.
//
// RECOGNISED AGENTS ONLY, so the gate stays what it was for the single-agent user. The default
// bucket names no agent, and a session from an unrecognised client belongs to Other — counting
// either would put the picker in front of every one-agent proxy at startup. See pickerRows for
// where Other is added instead.
//
// MATCHED ON THE EXACT LABEL, the way sessionListed scopes and agentSessionsCell counts, so each
// added row lists exactly its own sessions. The added rows go after the window's, ordered by
// label — they have no figures to rank by — and before Other, which stays last.
func (m *model) agentChoices() []agentRow {
	var extra []string
	for _, s := range m.sessions {
		agent := sessionAgent(s)
		if !knownAgentLabel(agent) || slices.Contains(extra, agent) ||
			slices.ContainsFunc(m.agents, func(a agentRow) bool { return a.label == agent }) {
			continue
		}
		extra = append(extra, agent)
	}
	if len(extra) == 0 {
		return m.agents
	}
	slices.Sort(extra)
	out := make([]agentRow, 0, len(m.agents)+len(extra))
	var other []agentRow
	for _, a := range m.agents {
		if a.label == otherAgents {
			other = append(other, a)
			continue
		}
		out = append(out, a)
	}
	for _, label := range extra {
		out = append(out, agentRow{label: label})
	}
	return append(out, other...)
}

// agentsFetchTimeout bounds the one request this pane makes. The same 5s the usage pane
// allows itself, matched rather than chosen again: both call GetUsage against the same
// endpoint, and two different bounds on one call would be two different answers to "how long
// before we give up".
const agentsFetchTimeout = 5 * time.Second

// agentsPollInterval is how often the open pane refetches its rows.
const agentsPollInterval = 20 * time.Second

// agentsSpan is the span the breakdown covers: one of the band's four, today until `w` moves it.
//
// TODAY BY DEFAULT, a symbolic window, so the figures come from the durable cost ledger rather
// than the six-hour ring. "Which agents have spent what" is first a question about a day — a
// reader comparing this against `agentop cost` (which defaults to the same window) must not find
// two different denominators. Where the ledger is off the proxy serves the longest window it
// holds and says so in the response, which is the same degradation `agentop cost` documents.
//
// `w` CYCLES IT, and the drawer on this pane follows (spendDrawerSpan): the title used to say
// "today" whatever `w` did, because `w` moved only the drawer. An OFFSET from today rather than
// an index, so the zero value of a model that never touched it is today.
func (m *model) agentsSpan() spendSpan {
	start := slices.Index(spendDrawerSpans, spanToday)
	return spendDrawerSpans[wrapIndex(start+m.agentsWindowStep, len(spendDrawerSpans))]
}

func (m *model) agentsSpanDef() spendSpanDef { return spendSpanDefs[m.agentsSpan()] }

// agentsWindowLabel names the window of the rows ON SCREEN, which a `w` press outruns by one
// round trip — the drawer's caption follows the same rule (drawerLabels). The requested window
// stands in until the first reply lands.
func (m *model) agentsWindowLabel() string {
	window := m.agentsRowsWindow
	if window == "" {
		window = m.agentsSpanDef().window
	}
	return strings.ToLower(spanLabelFor(window))
}

// cycleAgentsWindow handles `w` on the agents pane, drawer open or closed: the rows refetch for
// the next span, and an open drawer with them, since on this pane it breaks down the same span.
func (m *model) cycleAgentsWindow() tea.Cmd {
	m.agentsWindowStep = (m.agentsWindowStep + 1) % len(spendDrawerSpans)
	cmds := []tea.Cmd{m.fetchAgentRowsCmd(agentsOpenNever, paneNone)}
	if m.spendDrawerVisible() {
		// cycleSpendWindow's reason: the old error describes a request for another span.
		m.spend.drawer.err = nil
		cmds = append(cmds, m.fetchSpendDrawer())
	}
	return tea.Batch(cmds...)
}

// agentsOpen says what the reply to a rows fetch is allowed to do with them.
//
// AN ENUM RATHER THAN A BOOL because there are three answers, not two. `A` is owed an answer
// either way; the startup gate is owed the opposite: nobody asked for it, so it enters or it
// stays quiet.
type agentsOpen int

const (
	// agentsOpenNever is a background refresh: update the rows, enter nothing, say nothing. Sent by
	// `A` on the pane itself and by esc from a list reached through the picker.
	agentsOpenNever agentsOpen = iota
	// agentsOpenOnPress is an `A` press. It enters, or it flashes the fetch error.
	agentsOpenOnPress
	// agentsOpenAtStartup is the gate run once per connection. It enters when
	// agentsPaneApplies, and otherwise does nothing AND says nothing — see
	// startupAgentsGateCmd.
	agentsOpenAtStartup
)

// agentRowsLoadedMsg carries a fetched per-agent breakdown back to Update.
//
// NOT agentsLoadedMsg, which is TAKEN — by the Kubernetes namespace picker, whose
// Lister.ListAgents lists agent WORKLOADS. Same word, unrelated meaning; see paneAgents.
type agentRowsLoadedMsg struct {
	rows []agentRow
	err  error
	// open records who asked, so the reply knows whether it may enter the pane and whether it
	// may complain. A field rather than something inferred from the current pane: by the time a
	// reply lands the reader may have moved.
	open agentsOpen
	// from is the pane the `A` press came from, captured AT PRESS TIME and carried here for
	// exactly the reason the field above gives: by the time this reply lands the reader may have
	// moved, so reading m.pane then records a caller the press never had. Meaningless under
	// agentsOpenNever, which enters nothing, and unread on the startup path — see that arm in
	// Update for why it does not consult this field.
	from paneID
	// window is the window the rows answer for, so a reply `w` has outrun is dropped.
	window string
}

// fetchAgentRowsCmd requests the per-agent breakdown off the render loop.
//
// group=agent and no agent filter: the pane lists every agent, so the per-agent split arrives
// as Bucket.Series and is folded here.
func (m *model) fetchAgentRowsCmd(open agentsOpen, from paneID) tea.Cmd {
	if m.client == nil {
		return nil
	}
	client := m.client
	m.agentsFetchedAt = time.Now()
	// An opening fetch starts the visit on today; `w` lasts for one visit.
	if open != agentsOpenNever {
		m.agentsWindowStep = 0
	}
	// Captured here, on the update goroutine, for the reason fetchSpendSpan records.
	def := m.agentsSpanDef()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), agentsFetchTimeout)
		defer cancel()
		snap, err := client.GetUsageWindow(ctx, def.window, def.resolution, "", usage.GroupAgent)
		if err != nil {
			return agentRowsLoadedMsg{err: err, open: open, from: from, window: def.window}
		}
		return agentRowsLoadedMsg{rows: agentRowsFromSnapshot(snap), open: open, from: from, window: def.window}
	}
}

// agentsColumns is the table's declared layout, at the width it wants on a wide terminal.
//
// A FUNCTION RATHER THAN A LITERAL INSIDE newAgentsTable, matching pipelineColumns and
// catalogColumns, because layout() has to re-fit these on every resize and must do it from
// THESE definitions rather than from the live table's columns — refitting the live ones
// compounds each narrowing, so widening the terminal back up never restores what it took away.
//
// COST is widest because it is the column the pane exists for, and it holds "—" for an agent
// nothing could price, which is every Bob row until the billing-unit work lands. ITS TITLE NAMES
// THE WINDOW, because it is the pane's only windowed column: SESSIONS and LAST SEEN describe the
// whole list, which is not limited to the window, and SERVER is the routing now. REQUESTS and
// TOKENS were dropped for that reason — two more window figures beside two that are not read as
// one denominator, and the usage pane and `agentop cost --by agent` carry them.
func agentsColumns(window string) []table.Column {
	title := "COST " + strings.ToUpper(window)
	return []table.Column{
		{Title: "AGENT", Width: 34},
		{Title: title, Width: max(agentsCostWidth, len(title))},
	}
}

// startupAgentsGateCmd asks, once per connection, whether this proxy has enough agents on it to
// be worth a picker.
//
// ONE FETCH FROM initSessionView, which is the single place every entry point converges on:
// `--endpoint` mode's Init, the pod picker's portForwardReadyMsg, and `[l]`'s local endpoint all
// call it, and each replaces m.client first. Hooking it there rather than in Init is what makes
// the gate run again when the operator backs out to the pod picker and enters a DIFFERENT pod —
// a different proxy has different agents on it, and the answer from the previous one is not an
// answer about this one. The spend strip's chain is started from the same place for the same
// reason.
//
// NO MEMORY OF PREVIOUS ANSWERS, deliberately: the decision is a pure function of what the
// window currently shows. The Namespaces → Pods picker remembers nothing either, and a
// remembered dismissal would go stale exactly when it mattered — the moment a second agent
// appears is the moment the picker becomes worth showing.
//
// THE FIRST FRAME IS NOT BLOCKED. This returns a tea.Cmd like every other fetch, so the sessions
// pane paints and streams while the answer is in flight; entering AGENTS is something that
// happens a beat later, if it happens. A gate that waited would add its own latency to every
// startup, including the majority that it declines.
func (m *model) startupAgentsGateCmd() tea.Cmd {
	// paneNone: the gate has no caller pane, and its reply does not read this field.
	return m.fetchAgentRowsCmd(agentsOpenAtStartup, paneNone)
}

// newAgentsTable builds an empty per-agent breakdown table.
func newAgentsTable() table.Model {
	t := table.New(
		table.WithColumns(agentsColumns(spendSpanDefs[spanToday].label)),
		table.WithFocused(true),
	)
	t.SetStyles(tableStyles())
	return t
}

// rebuildAgentsTable rebuilds rows from All agents then m.pickerRows, which selectedAgentScope
// indexes too.
func (m *model) rebuildAgentsTable() {
	// Columns and rows change together, as in rebuildSessionsTable: SESSIONS appears only once a
	// session names its agent, so a server that names none shows the table unchanged.
	withSessions := m.sessionsNameAgents()
	cursor := m.agentsTbl.Cursor()
	prev, hadPrev := "", cursor >= 0 && cursor < len(m.agentRowLabels)
	if hadPrev {
		prev = m.agentRowLabels[cursor]
	}
	cols := agentsColumns(m.agentsWindowLabel())
	if withSessions {
		cols = slices.Insert(cols, 1,
			table.Column{Title: "SESSIONS", Width: 8},
			table.Column{Title: "LAST SEEN", Width: agentsLastSeenWidth})
	}
	// SERVER, last, only while the proxy runs an inference-router that routes — not one under
	// on_error: observe, which keeps its config but moves nothing (see routerStatus). Without
	// one no agent is routed, and a column reading "own choice" on every row would be noise.
	//
	// SQUEEZING AGENT ON A NARROW TERMINAL, NOT DROPPED AS THE SESSIONS TABLE'S IS, and on
	// purpose: this column is the one place the pane shows where each agent's new sessions go.
	// AGENT is the widest, so the fitter takes the room from it — 34 to 26 at 80 columns with
	// SESSIONS and LAST SEEN up — and what survives is at least the agent's name and minor version,
	// "claude-code/2.1…". That name is what SERVER and S act on, so two patch versions' rows that
	// read alike there are one agent with one server
	// (TestAgentsPane_TheAgentsNameSurvivesTheServerColumnAt80).
	router, routed := m.activeRouter()
	if routed {
		cols = append(cols, table.Column{Title: "SERVER", Width: agentsServerWidth})
	}
	if want := fitTableColumns(cols, m.width); !sameColumns(m.agentsTbl.Columns(), want) {
		m.agentsTbl.SetRows(nil)
		m.agentsTbl.SetColumns(want)
	}
	picker := m.pickerRows()
	rows := make([]table.Row, 0, len(picker)+1)
	// Its figures are left blank: the band above already carries the window's total, and summed
	// across agents billing in different units it would only read as mixed.
	all := table.Row{allAgentsLabel, ""}
	if withSessions {
		all = slices.Insert(all, 1, "", "")
	}
	// Nothing under SERVER either: routing is chosen per agent, and S on this row says so.
	if routed {
		all = append(all, "")
	}
	rows = append(rows, all)
	labels := make([]string, 0, len(picker)+1)
	labels = append(labels, "")
	now := m.clock()
	for _, a := range picker {
		labels = append(labels, a.label)
		row := table.Row{
			// SANITISED AT RENDER TIME. The label is a User-Agent, so it is
			// request-controlled; tui.sanitizeLabel is the package's render-time copy of the
			// rule, named as such in ledger's own comment.
			sanitizeLabel(a.label),
			agentCostCellIn(a.Counts, a.units, agentsCostWidth),
		}
		if withSessions {
			row = slices.Insert(row, 1, m.agentSessionsCell(a.label), relTime(now, m.agentLastSeen(a.label)))
		}
		if routed {
			row = append(row, agentServerCell(router, a.label))
		}
		rows = append(rows, row)
	}
	m.agentsTbl.SetRows(rows)
	m.agentRowLabels = labels
	// Follow the agent, not the index: a refresh can reorder the rows under the cursor.
	if i := slices.Index(labels, prev); hadPrev && i >= 0 {
		cursor = i
	}
	setCursorVisible(&m.agentsTbl, cursor)
}

// agentCostCell renders one agent's cost, or "—" when nothing priced it.
//
// "—" AND NEVER "$0.00", which is this codebase's standing rule and the reason
// SessionSummary.CostMicros is omitempty: an agent whose traffic nothing could price is not an
// agent that spent nothing. Bob is exactly that case today — it bills in credits, which the
// cost model cannot yet represent — so every Bob row reads "—" rather than claiming it was
// free.
//
// PricedRequests is the test, not CostMicros, because a genuine zero is possible: a request
// can be priced at a rate of zero. Reading the money field instead would collapse "priced, and
// it cost nothing" into "not priced".
func agentCostCell(c usage.Counts) string {
	if c.PricedRequests == 0 {
		return emptyCell
	}
	// formatUSDTotalMicros, not %.2f over micros/1e6: it does the rounding on the integer, so
	// 1_005_000 micros renders $1.01 rather than the $1.00 a float64 %.2f produces. It also
	// carries the floor that keeps a known sub-cent charge from printing as free.
	return formatUSDTotalMicros(c.CostMicros)
}

// agentsCostWidth is the COST column's declared width, the budget a relabelled figure fits.
const agentsCostWidth = 12

// agentCostCellIn is agentCostCell in the agent's own units: dollars (nil, or USD) exactly as
// before, one foreign unit relabelled to fit the column, and more than one withheld as
// money.Mixed — an agent's figure summed across units is not an amount.
func agentCostCellIn(c usage.Counts, units []string, budget int) string {
	cell := agentCostCell(c)
	if cell == emptyCell {
		return cell
	}
	unit, ok := money.WindowUnit(units)
	if !ok {
		return money.Mixed
	}
	if out := money.Relabel(cell, unit, budget); out != "" {
		return out
	}
	return emptyCell
}

// enterAgents opens the pane as an overlay over `from`, the pane `A` was pressed on.
//
// `from` IS PASSED IN, NOT READ OFF m.pane: this runs when the reply lands, and by then the reader
// may have moved. agentRowsLoadedMsg.from carries the press-time pane across the round trip.
func (m *model) enterAgents(from paneID) {
	m.closeServerPanels()
	m.previousPane = from
	m.agentsAboveSessions = false
	m.pane = paneAgents
	m.rebuildAgentsTable()
}

// enterAgentsAtStartup is the startup gate's entry: it enters when agentsPaneApplies and reports
// whether it did, and it never speaks. Shared by the gate's reply and its one look at the first
// session list, so the two cannot decide differently. The caller checks it is on Sessions.
//
// It enters as the level above Sessions, so esc backs out of the connection rather than returning
// to a caller; the reply's `from` is deliberately not read.
func (m *model) enterAgentsAtStartup() bool {
	if !agentsPaneApplies(m.agentChoices()) {
		return false
	}
	m.closeServerPanels()
	m.previousPane = paneNone
	m.agentsAboveSessions = true
	m.pane = paneAgents
	m.rebuildAgentsTable()
	return true
}

// allAgentsLabel is the picker's first row, the one that clears the scope.
//
// A ROW AND NOT A TOGGLE. Enter used to clear the scope when pressed on the agent already scoped,
// so every row but one meant "show me this agent" and that one meant "show me everything" — and a
// reader re-picking the agent they were on got every agent's spend instead. With this row, Enter
// always shows what is highlighted.
//
// ADDED BY rebuildAgentsTable, not by pickerRows: pickerRows is the partition of the sessions
// list that agentsPaneApplies and the Other row reason about, and this row partitions nothing.
const allAgentsLabel = "All agents"

// selectedAgentScope is the scope the row under the cursor selects: "" on All agents, else that
// agent's label. ok is false when the cursor is on no row.
//
// READ OFF m.pickerRows BY CURSOR INDEX, not out of the rendered table cell: the cell is passed
// through sanitizeLabel, which is a display transform — a control character or a long label
// arrives on the wire and leaves that function altered, so scoping to what the cell says could
// scope to a string no agent ever sent. The two are kept in step by rebuildAgentsTable, which
// builds the rows from m.pickerRows in order, after All agents.
func (m *model) selectedAgentScope() (scope string, ok bool) {
	i := m.agentsTbl.Cursor()
	if i == 0 {
		return "", true
	}
	picker := m.pickerRows()
	if i < 1 || i > len(picker) {
		return "", false
	}
	return picker[i-1].label, true
}

// leaveAgentsPane is esc's exit. As the level above Sessions it backs out of the connection the
// way esc on Sessions does: to the pods picker, or nowhere in --endpoint mode. As an overlay it
// returns to whichever pane `A` was pressed on, or to Sessions when none was recorded.
//
// RETURNING INTO USAGE RESTARTS ITS POLLING CHAIN, which its `m.pane != paneUsage` guard dropped
// while this pane was up.
func (m *model) leaveAgentsPane() tea.Cmd {
	if m.agentsAboveSessions {
		if m.parentCtx != nil {
			m.backToPodsPane()
		}
		return nil
	}
	if m.previousPane != paneNone {
		m.pane = m.previousPane
		m.previousPane = paneNone
	} else {
		m.pane = paneSessions
	}
	if m.pane == paneUsage {
		return m.resumeUsagePolling()
	}
	if m.pane == paneTimeline {
		return m.resumeTimeline()
	}
	return nil
}

// returnToAgentsPane is esc from a sessions list reached by picking an agent: back to the picker,
// as the level above Sessions, with its rows refreshed.
func (m *model) returnToAgentsPane() tea.Cmd {
	m.closeServerPanels()
	m.previousPane = paneNone
	m.agentsAboveSessions = true
	m.pane = paneAgents
	m.rebuildAgentsTable()
	return m.fetchAgentRowsCmd(agentsOpenNever, paneNone)
}

// agentSessionsCell counts the listed sessions that belong to the agent a row names, or a dash
// where none does. Other's count is what its scope lists; see otherSessionsCount.
func (m *model) agentSessionsCell(label string) string {
	n := 0
	if label == otherAgents {
		n = m.otherSessionsCount()
	} else {
		for _, s := range m.sessions {
			if agentRowLists(s, label) {
				n++
			}
		}
	}
	if n == 0 {
		return emptyCell
	}
	return formatCount(n)
}

// agentsLastSeenWidth fits relTime's widest form, a two-digit day: "Oct 23 15:04".
const agentsLastSeenWidth = len("Oct 23 15:04")

// agentLastSeen is the newest UpdatedAt among the listed sessions agentSessionsCell counts for
// label, or zero when it lists none. Other's cached-only rows carry no summary and add no time.
func (m *model) agentLastSeen(label string) time.Time {
	var last time.Time
	for _, s := range m.sessions {
		if agentRowLists(s, label) {
			if s.UpdatedAt.After(last) {
				last = s.UpdatedAt
			}
		}
	}
	return last
}

// agentRowLists reports whether s is one of the rows the agent label lists: its own sessions and
// pending buckets — or, for Other, every row naming no recognised agent — less the buckets folded
// into a session.
func agentRowLists(s session.SessionSummary, label string) bool {
	if foldedInto(s) != "" {
		return false
	}
	if label == otherAgents {
		return inOtherAgents(s)
	}
	return sessionAgent(s) == label
}

// sessionsNameAgents reports whether any listed session names its agent.
func (m *model) sessionsNameAgents() bool {
	for _, s := range m.sessions {
		if sessionAgent(s) != "" {
			return true
		}
	}
	return false
}
