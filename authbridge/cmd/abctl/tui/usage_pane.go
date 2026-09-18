package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
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
	err       error
	loading   bool
	lastFetch time.Time

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

// cycleMetric advances [t] across the count metrics and latency. Latency uses a
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
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		snap, err := client.GetUsage(ctx, window, resolution, session, group)
		return usageLoadedMsg{snap: snap, req: req, err: err}
	}
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

// renderUsageChart picks the form the data calls for.
//
// Three renderers rather than one parameterised one, because the forms differ in
// kind and not just in decoration: bars encode a magnitude from zero with
// sub-row precision; a stack trades that precision for a breakdown, since a
// fractional top cell cannot also encode a segment boundary; and latency is a
// distribution whose zero is meaningless, so it gets marks and a range instead.
func renderUsageChart(snap *usage.Snapshot, m usageMetric, group usage.Group, width int) []string {
	if m.isLatency() {
		// Grouping is ignored here: the aggregator carries no per-label latency,
		// so a "by status" latency chart would silently show the bucket-wide mean
		// under a heading implying otherwise.
		return renderWhiskers(snap.Buckets, width)
	}
	if group != "" && group != usage.GroupNone {
		return renderStackedBars(snap.Buckets, m, group, width)
	}
	return renderBars(snap.Buckets, m, width)
}

// usageScopeMax is the room the header line can give the scope label at this width.
//
// The rest of that line — " USAGE — ", the window, the resolution, the metric and the
// grouping — runs to about 50 columns, so the label gets what is left. Floored at 24 so a
// narrow terminal still shows the end of the label rather than nothing, and left unbounded
// above: a wide terminal has the room, and the label is the only thing on the line an
// operator cannot reconstruct from the pane itself.
func usageScopeMax(width int) int {
	const otherFields = 50
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
		// already overflowed an 80-column terminal by 13 with a bare id — adding a title
		// would have taken that to 78 over, wrapping the header and pushing the chart down
		// a line. Truncated from the left for the reason sessionTitleCell is: the tail of
		// the label carries the leaf directory and the whole id.
		scope = truncLeft(m.sessionLabel(m.usage.session), usageScopeMax(width))
	}
	window, resolution := m.usage.window()
	// The header must not claim a breakdown the chart is not showing. Latency has
	// no per-label data in the aggregator, so renderUsageChart ignores the group
	// entirely — displaying "by status" over a bucket-wide mean would assert a
	// breakdown that does not exist. The selection is kept, not cleared, so it is
	// still there when the operator cycles back to a count metric.
	grouping := "ungrouped"
	switch {
	case m.usage.metric.isLatency():
		grouping = "no breakdown for latency"
	case m.usage.group != "" && m.usage.group != usage.GroupNone:
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
	default:
		for _, line := range renderUsageChart(m.usage.snap, m.usage.metric, m.usage.group, width) {
			b.WriteString(line)
			b.WriteString("\n")
		}
		b.WriteString("\n")
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
