package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/cmd/agentop/money"
	"github.com/rossoctl/cortex/core/cost/usage"
)

// usagePollInterval is how often the pane refetches while it is open.
//
// 20s is a deliberate compromise: the server buckets at one minute, so polling
// faster cannot reveal anything new except the current bucket filling, while
// polling slower makes the newest bar look stale to someone watching a live
// agent. The ticker is armed only while the pane is focused, so a background
// pane costs nothing.
const usagePollInterval = 20 * time.Second

// errUsageUnsupported marks a proxy with no usage aggregator wired — an older
// binary, or session tracking disabled. Distinguished from a transport error so
// the pane can explain the cause instead of showing a bare failure.
var errUsageUnsupported = errors.New("usage endpoint not available")

// usageLoadedMsg carries a fetched snapshot back to Update.
type usageLoadedMsg struct {
	snap *usage.Snapshot
	// windowUnits is snap.Currencies before any agent scope narrowed it; see costUngroupedRow.
	windowUnits []string
	// agentLatency reports that snap's latency is the scoped agent's own; see graftAgentLatency.
	agentLatency bool
	// req is the monotonic id of the request this answers. Update discards any
	// reply whose id is not the newest one issued.
	//
	// A single id rather than a tuple of (session, window, resolution): comparing
	// fields means every future view option has to be added to the comparison or
	// it silently stops being covered, and two rapid presses of `w` produce two
	// in-flight requests that agree on session but differ on window — so an
	// out-of-order reply could repaint a stale window under the current heading.
	// An id cannot be partially right.
	req uint64
	err error
}

// usageTickMsg fires the periodic refetch. gen ties it to the polling chain that
// scheduled it, so ticks from a previous visit to the pane are ignored.
type usageTickMsg struct {
	gen uint64
}

// usageState is the pane's view state: which metric, window and scope.
type usageState struct {
	metric    usageMetric
	windowIdx int    // index into usageWindows
	session   string // "" means all sessions
	snap      *usage.Snapshot
	// windowUnits is the window's units, which the whole-window residual is in; see usageLoadedMsg.
	windowUnits []string
	// agentLatency is usageLoadedMsg's, assigned with snap.
	agentLatency bool
	err          error
	loading      bool
	lastFetch    time.Time

	// group is the active breakdown; GroupNone renders the ungrouped bars.
	group usage.Group

	// reqSeq is the id of the most recently ISSUED request. Any reply carrying a
	// smaller id is stale and dropped.
	reqSeq uint64

	// returnPane is where esc goes back to, kept here rather than in
	// model.previousPane because that field is shared with the catalog overlay:
	// opening the catalog from Usage overwrites it with paneUsage and the
	// catalog's own esc then clears it, so esc from Usage landed on Sessions
	// instead of the Events pane it was opened from.
	returnPane paneID

	// tickGen identifies the current polling chain. openUsage bumps it, and a
	// tick from an earlier visit is dropped — otherwise a quick exit and
	// re-entry leaves two chains alive, each rescheduling the other's successor
	// and doubling the request rate for the life of the session.
	tickGen uint64
}

// beginFetch invalidates what is on screen and returns the command for a fresh
// request. Every view-option change goes through it.
//
// Clearing snap matters as much as bumping the sequence: leaving the old
// snapshot in place means renderUsage keeps drawing the previous scope's or
// window's bars under the NEW heading until the reply lands — the same
// wrong-heading failure the discard guard prevents, arriving from the other
// direction.
func (m *model) beginFetch() tea.Cmd {
	m.usage.reqSeq++
	m.usage.snap = nil
	m.usage.err = nil
	m.usage.loading = true
	return m.fetchUsage()
}

func (u *usageState) window() (window, resolution time.Duration) {
	w := usageWindows[u.windowIdx%len(usageWindows)]
	return w.window, w.resolution
}

// cycleMetric advances [m] across the count metrics, latency and cost. Latency uses a
// different renderer (mean-with-whiskers) because a bar encodes magnitude from a
// zero baseline and mean latency has no meaningful zero.
func (u *usageState) cycleMetric() {
	u.metric = (u.metric + 1) % usageMetricCount
}

// cycleGroup advances [g] through the groupings. Ungrouped keeps sub-row
// precision via partial blocks; a grouped view trades that for the breakdown,
// since a fractional top cell cannot also encode a segment boundary.
func (u *usageState) cycleGroup() {
	switch u.group {
	// The zero value is "" and GroupNone is "none": both mean ungrouped, so both
	// must advance to status. Matching only GroupNone sent a freshly opened pane
	// to the default arm, which set GroupNone and made the first [g] press a
	// no-op.
	case "", usage.GroupNone:
		u.group = usage.GroupStatus
	case usage.GroupStatus:
		u.group = usage.GroupMethod
	case usage.GroupMethod:
		u.group = usage.GroupPlugin
	case usage.GroupPlugin:
		u.group = usage.GroupHost
	default:
		u.group = usage.GroupNone
	}
}

func (u *usageState) cycleWindow() {
	u.windowIdx = (u.windowIdx + 1) % len(usageWindows)
}

// fetchUsage requests a snapshot. Returns a tea.Cmd so the HTTP call happens off
// the render loop.
func (m *model) fetchUsage() tea.Cmd {
	if m.client == nil {
		return nil
	}
	client := m.client
	window, resolution := m.usage.window()
	session := m.usage.session
	group := m.usage.group
	req := m.usage.reqSeq
	// THE SCOPE IS CAPTURED HERE, with the rest of the request, rather than read off the model
	// inside the closure: this runs on another goroutine, and a scope changed while the request
	// was in flight would narrow the reply to an agent the request was not grouped for.
	scope := m.agentScope
	if scope != "" {
		// The agent axis is not the pane's to choose while a scope is active: /v1/usage takes one
		// group parameter, and the narrowing below needs the per-agent series. m.usage.group is
		// left alone rather than overwritten, so clearing the scope restores the axis the
		// operator had picked.
		group = usage.GroupAgent
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		snap, err := client.GetUsage(ctx, window, resolution, session, group)
		var windowUnits []string
		if err == nil {
			windowUnits = snap.Currencies
		}
		var agentLatency bool
		if err == nil && scope != "" {
			// NarrowBuckets, not KeepBuckets: this pane renders a chart from Buckets, and
			// narrowing only the totals would title a whole-window chart with one agent's name.
			// The cost is that the narrowed buckets carry no latency, which graftAgentLatency
			// restores where the server can supply it — and the renderUsage branch says so where
			// it cannot, rather than plotting the zeros.
			//
			// A FAILURE IS REPORTED, not swallowed. A scope stops matching on its own as the
			// window moves past an agent's last request.
			//
			// Other is a fold of the series rather than one of them, so it is made one first.
			if scope == otherAgents {
				snap = foldOtherAgents(snap)
			}
			snap, err = usage.ScopeToAgent(snap, scope, usage.NarrowBuckets)
			// Not for Other, which no server keeps a ring for, nor within a session, which the
			// server answers by the same narrowing as this — neither reply could carry latency.
			if err == nil && scope != otherAgents && session == "" {
				agentLatency, err = graftAgentLatency(ctx, client, snap, window, resolution, scope)
			}
		}
		return usageLoadedMsg{snap: snap, windowUnits: windowUnits, agentLatency: agentLatency, req: req, err: err}
	}
}

// graftAgentLatency copies the scoped agent's own latency onto snap, whose narrowed buckets carry
// none, and reports whether it did.
//
// FROM A SECOND READ, agent=scope, because the server keeps a ring per recognised agent
// (usage.Aggregator.agents) and answers that from it, latency included. ONLY THE LATENCY IS
// TAKEN, not the whole answer: it has no whole-window residual for costUngroupedRow to disclose
// and no window's units to state it in, so counts and cost stay on the narrowing they have always
// come from. Matched by bucket time rather than position, since a minute can turn between the
// two reads.
//
// FALSE, AND SNAP UNTOUCHED, WHERE THE ANSWER CANNOT BE THE AGENT'S OWN. No agent echo is a
// server that ignored agent= and answered for every agent — grafting that would title everyone's
// response times with one agent's name. An echo with requests but no latency sample in any bucket
// is a server that narrowed the agent axis, as every one did before the per-agent rings; a ring
// cannot produce that unless every request went unmeasured. An agent with no requests at all is
// true: nothing is missing, and renderWhiskers says the window has no samples.
func graftAgentLatency(ctx context.Context, client *apiclient.Client, snap *usage.Snapshot, window, resolution time.Duration, scope string) (bool, error) {
	own, err := client.GetUsageWindowForAgent(ctx, window.String(), resolution, "", scope, usage.GroupNone)
	if err != nil {
		return false, err
	}
	if own.Agent != scope {
		return false, nil
	}
	byAt := make(map[int64]usage.Bucket, len(own.Buckets))
	for _, b := range own.Buckets {
		if b.LatSamples > 0 {
			byAt[b.At.Unix()] = b
		}
	}
	if len(byAt) == 0 {
		return own.Totals.Requests == 0, nil
	}
	// In place: snap.Buckets is the slice the narrowing allocated, owned by no one else.
	for i := range snap.Buckets {
		if b, ok := byAt[snap.Buckets[i].At.Unix()]; ok {
			snap.Buckets[i].LatMeanMs, snap.Buckets[i].LatStdDevMs, snap.Buckets[i].LatSamples =
				b.LatMeanMs, b.LatStdDevMs, b.LatSamples
		}
	}
	return true, nil
}

// usageTick schedules the next poll for the given generation.
func usageTick(gen uint64) tea.Cmd {
	return tea.Tick(usagePollInterval, func(time.Time) tea.Msg {
		return usageTickMsg{gen: gen}
	})
}

// openUsage enters the pane, remembering where to return to.
//
// session is the scope: the events pane passes its selected session so the chart
// matches the timeline the operator was just reading; the sessions pane passes ""
// for an all-sessions view.
func (m *model) openUsage(session string) tea.Cmd {
	m.usage.returnPane = m.pane
	m.pane = paneUsage
	m.usage.session = session
	// Shares resumeUsagePolling so the two entry points cannot drift on how a
	// chain is started or how the previous one is invalidated.
	return m.resumeUsagePolling()
}

// resumeUsagePolling restarts the poll chain when the pane regains focus without
// going through openUsage — returning from the catalog overlay, for instance.
//
// It bumps tickGen so any tick still in flight from the previous chain is stale,
// then starts exactly one new chain. Refetching immediately as well means the
// chart is current on arrival rather than showing data up to 20s old.
func (m *model) resumeUsagePolling() tea.Cmd {
	m.usage.tickGen++
	return tea.Batch(m.beginFetch(), usageTick(m.usage.tickGen))
}

// usagePaneChromeRows is how many of the pane's rows are spent outside the chart: the
// header line and its blank above, and the blank, summary and refresh note below.
//
// Measured, not estimated — TestUsageChartHeight_MatchesTheRenderedChrome keeps it equal
// to what renderUsage actually spends, so the caption's height gate cannot drift out of
// agreement with the layout it is protecting.
const usagePaneChromeRows = 6

// usageChartHeight converts the pane's row budget into the rows available to the chart.
//
// Zero (an unknown budget) passes straight through as zero, which the renderers read as
// "not measuring a terminal" and render at full fidelity.
func usageChartHeight(paneHeight int) int {
	if paneHeight <= 0 {
		return 0
	}
	// Exactly the chrome, with nothing held back. An earlier version subtracted one more
	// row: at the time renderUsage's output fit bodyHeight exactly at 80x24 while the
	// composed view still came out a row past the terminal, and dropping the caption was
	// what closed it. That is no longer what happens — the pane overflows 80x24 by one row
	// on the merge-base too, with no caption in existence there, because a row was added
	// above the body without the budget following. So the extra subtraction now fixes
	// nothing and costs the chart a row it can afford. Removed rather than kept as
	// insurance: a budget that is not the real affordance is a number no later reader can
	// check against anything.
	if h := paneHeight - usagePaneChromeRows; h > 0 {
		return h
	}
	// A budget this small cannot fit the chart at all; 1 is enough to say "no room to
	// spare" without claiming a negative height.
	return 1
}

// renderUsageChart picks the form the data calls for.
//
// Three renderers rather than one parameterised one, because the forms differ in
// kind and not just in decoration: bars encode a magnitude from zero with
// sub-row precision; a stack trades that precision for a breakdown, since a
// fractional top cell cannot also encode a segment boundary; and latency is a
// distribution whose zero is meaningless, so it gets marks and a range instead.
func renderUsageChart(snap *usage.Snapshot, m usageMetric, group usage.Group, width, height int) []string {
	if m.isLatency() {
		// Grouping is ignored here: the aggregator carries no per-label latency,
		// so a "by status" latency chart would silently show the bucket-wide mean
		// under a heading implying otherwise.
		//
		// HEIGHT IS DROPPED HERE, deliberately and not silently: renderWhiskers is a
		// fixed plotRows+5 frame with no optional row to trade away, so there is nothing
		// for a budget to decide. It predates the budget mechanism and ignored the
		// terminal's height before this branch existed too, so a latency chart overflows
		// a short pane exactly as much as it always did — pre-existing, tracked with the
		// other half of the pane's height debt (see fitModel's paneUsage note), and not
		// something a caption gate can reach. Passing height in to be discarded inside
		// the renderer would only move the discard somewhere less visible.
		return renderWhiskers(snap.Buckets, width)
	}
	unit := ""
	if m.isCost() {
		// A COST CHART OVER SEVERAL UNITS IS NOT DRAWN: every bar would stack credits on dollars.
		// Scoping to one agent narrows the window to that agent's units, which is the way out.
		var ok bool
		if unit, ok = money.WindowUnit(snap.Currencies); !ok {
			return []string{"  cost chart withheld: this window mixes billing units", "  [A] scope to one agent, then [u], or view tokens"}
		}
	}
	if group != "" && group != usage.GroupNone {
		return renderStackedBarsIn(snap.Buckets, m, group, width, height, unit)
	}
	return renderBarsIn(snap.Buckets, m, width, height, unit)
}

// usageScopeMax is the room the header line can give the scope label at this width.
//
// The rest of that line — "  USAGE — ", the window, the resolution, the metric and the
// grouping — is not a fixed cost. Swept across every metric, every entry in usageWindows and
// every grouping string renderUsage actually emits, it runs to 65 columns, widest on the
// latency metric at the 6h window: latency renders "no breakdown for latency", which is 24
// columns on its own, and 6h renders as "6h0m0s @ 30m0s".
//
// An earlier revision said 59 and measured it against grouping strings the renderer never
// produces ("no breakdown", "by model" without its prefix) while omitting the longest one it
// does. The allowance must be the WIDEST reachable remainder, not an average: too small and it
// fails to truncate, and the line wraps.
//
// EVEN AT 65 THIS DOES NOT PREVENT OVERFLOW, and an earlier comment claimed it did. The floor
// below wins on a narrow terminal — at 80 columns a 65-column header leaves 15, under the
// floor's 24 — so the label is truncated to 24 and the line runs past the terminal. The floor
// is deliberate: the tail of the label carries the leaf directory and the session id, and a
// scope nobody can read is worse than a wrapped line. What this does is reduce a pre-existing
// overflow, not remove it.
func usageScopeMax(width int) int {
	// The widest reachable non-scope remainder. Pinned by
	// TestUsageScopeMax_CoversTheWidestHeader, which recomputes it from usageWindows, the
	// metric enum and renderUsage's own grouping branches rather than trusting this number.
	const otherFields = 65
	if room := width - otherFields; room > 24 {
		return room
	}
	return 24
}

// renderUsage draws the pane.
func (m *model) renderUsage(width, height int) string {
	var b strings.Builder

	scope := "all sessions"
	if m.usage.session != "" {
		// No "session: " prefix: the pane is reached by pressing u on a session, and the
		// label already reads as one. Titled, it said "session: <uuid>" where the operator
		// had just selected a name — the id restated, and the name nowhere.
		//
		// Clipped, unlike the id it replaces. This line is one unwrapped Sprintf, and it
		// already overflowed an 80-column terminal by 13 with a bare id — a title added
		// whole would have taken that to 78 over.
		//
		// CLIPPING REDUCES THAT OVERFLOW; IT DOES NOT REMOVE IT. On the latency metric at the
		// 6h window the line is still 8 columns past an 80-column terminal, first fitting at
		// 88, because usageScopeMax's floor wins there and hands back more room than the line
		// has. See its doc, which states the same thing — an earlier revision of this comment
		// framed the clipping as what avoids the wrap, which would leave a reader believing 80
		// columns is safe.
		//
		// Truncated from the left for the reason sessionTitleCell is: the tail of the label
		// carries the leaf directory and the whole id.
		scope = truncLeft(m.sessionLabel(m.usage.session), usageScopeMax(width))
	}
	window, resolution := m.usage.window()
	// The header must not claim a breakdown the chart is not showing. Latency has
	// no per-label data in the aggregator, so renderUsageChart ignores the group
	// entirely — displaying "by status" over a bucket-wide mean would assert a
	// breakdown that does not exist. The selection is kept, not cleared, so it is
	// still there when the operator cycles back to a count metric.
	//
	// Likewise under an agent scope: the scope has taken the wire axis, so there is no
	// second one to break down by.
	grouping := "ungrouped"
	switch {
	case m.usage.metric.isLatency():
		grouping = "no breakdown for latency"
	case m.agentScope == "" && m.usage.group != "" && m.usage.group != usage.GroupNone:
		grouping = "by " + string(m.usage.group)
	}
	b.WriteString(fmt.Sprintf("  USAGE — %s — %s @ %s — %s — %s\n\n",
		scope, window, resolution, m.usage.metric, grouping))

	switch {
	case m.usage.err != nil && errors.Is(m.usage.err, errUsageUnsupported):
		// The proxy has no aggregator: an older binary, or session tracking
		// disabled. Say which, rather than showing an empty chart that would
		// read as "no traffic".
		b.WriteString("  Usage aggregation is not available on this proxy.\n")
		b.WriteString("  It requires session tracking enabled and a build that serves /v1/usage.\n")
	case m.usage.err != nil:
		b.WriteString(fmt.Sprintf("  Error: %v\n", m.usage.err))
	case m.usage.snap == nil && m.usage.loading:
		b.WriteString("  Loading…\n")
	case m.usage.snap == nil:
		b.WriteString("  (no data)\n")
	case m.agentScope != "" && m.usage.metric.isLatency() && !m.usage.agentLatency:
		// SAID HERE RATHER THAN LEFT TO renderWhiskers, which would answer "no latency samples
		// in this window" — true of the narrowed snapshot and false about the window, and it
		// would send the reader looking for traffic that is there. usage.ScopeToAgent zeroes the
		// latency fields because Bucket.Series is map[string]Counts and Counts carries no
		// latency, so a bucket's mean describes every agent that shared it. graftAgentLatency
		// puts them back from the agent's own ring, and this branch is where there was none.
		//
		// THE REASON IS NAMED, because the three cases have different fixes: Other and a session
		// are views to leave, while a proxy without per-agent rings is one to upgrade.
		b.WriteString("  (latency is not available per agent)\n")
		b.WriteString("\n")
		switch {
		case m.agentScope == otherAgents:
			b.WriteString("  Other pools agents the proxy keeps no response times of their own\n")
			b.WriteString("  for, so its latency cannot be told from every other agent's.\n")
		case m.usage.session != "":
			b.WriteString("  Within a session, response times are recorded across every agent\n")
			b.WriteString("  that shared it, so they cannot be attributed to one.\n")
		default:
			b.WriteString("  This proxy keeps no response times for this agent alone — it\n")
			b.WriteString("  predates per-agent latency, or does not recognise the agent.\n")
		}
		b.WriteString("  To plot latency for every agent instead, clear the scope:\n")
		b.WriteString("  [A], [enter] on All agents, then [u].\n")
		b.WriteString("\n")
		b.WriteString(renderUsageSummary(m.usage.snap))
		b.WriteString("\n")
		// BOTH BRANCHES that render the summary render this, because both print its COST cell
		// from the narrowed Totals. Empty string when no disclosure is due.
		b.WriteString(costUngroupedRow(m.usage.snap, m.agentScope, m.usage.windowUnits))
	default:
		for _, line := range renderUsageChart(m.usage.snap, m.usage.metric, m.usage.group, width, usageChartHeight(height)) {
			b.WriteString(line)
			b.WriteString("\n")
		}
		// The note takes the blank row above the summary rather than adding one: it is
		// conditional, so usagePaneChromeRows cannot count it.
		if note := costUngroupedRow(m.usage.snap, m.agentScope, m.usage.windowUnits); note != "" {
			b.WriteString(note)
		} else {
			b.WriteString("\n")
		}
		b.WriteString(renderUsageSummary(m.usage.snap))
		b.WriteString("\n")
		if !m.usage.lastFetch.IsZero() {
			b.WriteString(fmt.Sprintf("\n  updated %s ago (every %s)\n",
				time.Since(m.usage.lastFetch).Truncate(time.Second), usagePollInterval))
		}
	}

	// No key hints here: helpView renders the footer for every pane, and a
	// second copy inside the body would drift from it the first time a binding
	// changed. The pane's keys live in helpView and in paneKeys (the [?]
	// overlay), which the coverage test holds to the real pane list.
	return b.String()
}

// costUngroupedRow is the pane's form of the disclosure `agentop cost` prints at writeCostSummary:
// the part of the window's spend that NO agent carries.
//
// usage.Snapshot.UngroupedCostMicros is a WHOLE-WINDOW residual and survives usage.ScopeToAgent
// deliberately — that function's comment says why, and names this one as the pane's half of the
// duty. So under a scope it sits in a snapshot whose Totals describe one agent while it describes
// traffic belonging to none, beside a COST cell computed from those narrowed Totals. Without a
// word about it, a reader who scopes to each agent in turn and sums the figures finds a shortfall
// with nothing to explain it.
//
// THE SCOPE IS A PARAMETER rather than read from the model, and this lives outside
// renderCostSummary for the same reason: that function renders from a *usage.Snapshot alone, and
// a narrowed snapshot is indistinguishable from a whole-window one — ScopeToAgent rewrites
// Totals, not the question that produced them. Only the pane knows a scope is in force.
//
// NOT SUBTRACTED FROM OR ADDED TO the figure beside it: this agent's total is this agent's and the
// residual is nobody's. Stated beside it, not folded into it — writeCostSummary's rule, kept here
// so the two surfaces cannot disagree about the arithmetic.
//
// ZERO AND NEGATIVE BOTH RENDER NOTHING, and the negative goes through the shared negativeCost
// rather than a local comparison. A residual cannot arrive negative in the first place — usage's
// residualOf publishes a negative one as SeriesOvershootMicros instead — so this is the
// impossible-figure guard every money cell on this surface carries, not a case with a reading.
//
// THE GUARD IS NOT ABOUT MIS-FORMATTING: formatUSDTotalMicros already refuses the integer-cent
// arithmetic for a negative and falls back to four decimals, and its doc is explicit that NAMING an
// impossible figure stays the caller's job. Left to it, this line would read "$-0.1500 of this
// window is attributed to no agent" — prose asserting a negative residual, which is worse than the
// silence. The shape is sessionMoneyCell's.
func costUngroupedRow(snap *usage.Snapshot, scope string, windowUnits []string) string {
	if scope == "" || snap == nil || snap.UngroupedCostMicros == nil {
		return ""
	}
	micros := *snap.UngroupedCostMicros
	if micros == 0 || negativeCost(micros) {
		return ""
	}
	// DOLLARS ONLY carry an amount. The residual belongs to no agent, so a scoped snapshot's units
	// — the agent's — do not say what it is in, and a foreign or mixed list drops the amount.
	if unit, ok := money.WindowUnit(windowUnits); !ok || !money.IsDefault(unit) {
		return "  note  part of this window's cost is attributed to no agent\n"
	}
	return fmt.Sprintf("  note  %s of this window is attributed to no agent\n",
		formatUSDTotalMicros(micros))
}
