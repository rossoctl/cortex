package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// costPollInterval is how often the pane refetches while it is open.
//
// The same 20s as the Usage pane and the spend strip's window chain, and for the
// same reason: the aggregator buckets at one minute, so a faster poll cannot reveal
// anything the server has not already folded. The chain is armed only while the
// pane is focused, so a backgrounded Cost pane costs nothing.
const costPollInterval = 20 * time.Second

// costPaneWindows are the spans [w] cycles, as the SERVER names them.
//
// Strings rather than the Usage pane's time.Duration pairs, because two of the
// three are not lengths: "today" is a boundary and "7d" is longer than the ring
// retains, so both are answered from the durable cost ledger and neither can be
// expressed as a duration (see usage.ParseWindowSpec). "1h" is the rolling ring
// window, kept because it is the only one that answers "what is it costing me right
// now" on a proxy with no ledger.
//
// "today" is first, so it is the default: the question this pane exists for is
// "what did today cost", and a rolling hour is the follow-up.
var costPaneWindows = []string{usage.WindowToday, usage.Window7d, "1h"}

// costPaneGroups are the breakdown axes [g] cycles.
//
// No GroupNone: an ungrouped view of this pane is the spend strip, which is already
// on screen one row above it, so a cycle position that showed nothing new would
// just be a way to lose the breakdown.
//
// Agent is LAST rather than first despite being the axis this pane was asked for,
// because it is the only one whose rows can be spoofed: a User-Agent is self-reported
// (see pipeline.EventClient), so a per-agent table is an attribution aid and not
// evidence. Model and endpoint come from the request the proxy actually forwarded.
// Ordering the trustworthy axes first means the spoofable one is something a reader
// chooses, not the first thing they are shown.
//
// Session stays in the cycle alongside it rather than being replaced by it: they
// answer different questions, and neither subsumes the other. Several concurrent
// agents can share one session id — usage.GroupSession's own doc records that their
// spend lands in one entry — while one agent moving between sessions spans several.
var costPaneGroups = []usage.Group{usage.GroupModel, usage.GroupEndpoint, usage.GroupSession, usage.GroupAgent}

// costPaneState is the Cost pane's view state and its poll chain.
//
// This is the THIRD copy of the reqSeq/tickGen guard pair in this package —
// usageState and spendState are the other two — and the triplication is deliberate
// rather than overlooked. Extracting a shared poller would mean refactoring the
// Usage pane's polling in the same commit that adds a pane, so any Usage-pane
// regression would land attributed to the new pane, in the last commit of a long
// PR. The extraction is worth doing; it is not worth doing here.
//
// Deliberately NOT m.usage or m.spend either. m.usage carries a user-chosen metric
// and an optional single-session scope, and m.spend is all-sessions chrome on a
// fixed rolling window; this pane needs its own window and its own breakdown axis,
// and sharing either state would make selecting one view silently move the other.
type costPaneState struct {
	snap      *usage.Snapshot
	err       error
	loading   bool
	lastFetch time.Time

	// windowIdx indexes costPaneWindows; group is the active breakdown axis.
	windowIdx int
	group     usage.Group

	// returnPane is where esc goes back to, kept here rather than in
	// model.previousPane for the reason usageState.returnPane records: that field is
	// shared with the catalog overlay, so opening the catalog from this pane would
	// clobber it and esc would land somewhere the user never came from.
	returnPane paneID

	// reqSeq is the id of the most recently ISSUED request; a reply carrying a
	// different id is stale and dropped. An id rather than a comparison of the
	// request's fields, for the reason usageLoadedMsg.req records: comparing fields
	// means every future view option has to be added to the comparison or it
	// silently stops being covered, while an id cannot be partially right.
	reqSeq uint64
	// tickGen identifies the current polling chain. See usageState.tickGen: a quick
	// exit and re-entry left two chains alive, each rescheduling the other's
	// successor and doubling the request rate for the life of the session.
	tickGen uint64
}

// window returns the span to request, as the server names it. Named to match
// usageState.window() so the third copy of this shape is recognisably the same
// thing rather than a new invention.
func (s *costPaneState) window() string {
	return costPaneWindows[s.windowIdx%len(costPaneWindows)]
}

func (s *costPaneState) cycleWindow() {
	s.windowIdx = (s.windowIdx + 1) % len(costPaneWindows)
}

// cycleGroup advances [g] through costPaneGroups, never landing on GroupNone.
//
// The zero value "" is treated as "before the first entry" so the first press
// advances to the second axis rather than re-selecting the default — the bug
// usageState.cycleGroup's comment records, where matching only GroupNone sent a
// freshly opened pane to the default arm and made the first press a no-op.
func (s *costPaneState) cycleGroup() {
	for i, g := range costPaneGroups {
		if g == s.group {
			s.group = costPaneGroups[(i+1)%len(costPaneGroups)]
			return
		}
	}
	// Unknown or unset (including "" and GroupNone): start at the head of the cycle.
	s.group = costPaneGroups[0]
}

// invalidate drops the data this state describes and disowns anything in flight.
//
// Both counters move, and clearing snap matters as much as either. spend.go learned
// the whole rule the hard way: bumping tickGen alone stops the old chain from
// scheduling but not the reply already in the air, so an in-flight answer still
// passed the reqSeq guard and landed with a fresh timestamp — presenting the
// previous window's figure as the current one's. And leaving snap in place draws the
// previous window's breakdown under the NEW heading until the reply lands, which is
// the same wrong-heading failure arriving from the other direction.
func (s *costPaneState) invalidate() {
	s.snap = nil
	s.err = nil
	s.lastFetch = time.Time{}
	s.loading = true
	s.reqSeq++
	s.tickGen++
}

// costLoadedMsg carries a fetched snapshot back to Update.
type costLoadedMsg struct {
	snap *usage.Snapshot
	req  uint64
	err  error
}

// costTickMsg fires the periodic refetch. gen ties it to the chain that scheduled
// it, so a tick from a superseded chain is ignored.
type costTickMsg struct{ gen uint64 }

// applySettings puts the pane on the remembered view, or on its defaults.
//
// DEVIATIONS ONLY, matching how EventSettings records columns: an empty field means
// "whatever this build defaults to", never "no window". So a release that reorders
// costPaneWindows moves every user who never pressed [w] along with it, instead of
// pinning them to a position they never chose.
//
// Both fields are reset first rather than only assigned when present. Without that, a
// second visit would inherit the previous visit's axis whenever the file had nothing
// to say about it — which is the same class of bug as a stale snapshot under a fresh
// heading, arriving through the settings instead of the wire.
//
// A value costView already discarded arrives here as "", and lands on the default.
func (s *costPaneState) applySettings(cs CostSettings) {
	s.windowIdx = 0
	s.group = costPaneGroups[0]
	if cs.Window != "" {
		for i, w := range costPaneWindows {
			if w == cs.Window {
				s.windowIdx = i
				break
			}
		}
	}
	if cs.Group != "" {
		for _, g := range costPaneGroups {
			if string(g) == cs.Group {
				s.group = g
				break
			}
		}
	}
}

// openCostPane enters the pane, remembering where to return to.
//
// The remembered view is applied on the way IN rather than at startup: the pane may
// never be opened in a session, and reading the settings here means a hand-edited file
// takes effect on the next open rather than only on the next launch.
func (m *model) openCostPane() tea.Cmd {
	m.costPane.returnPane = m.pane
	m.pane = paneCost
	// The NORMALISED view is written back, not merely applied. costView drops a field it
	// cannot honour and the pane falls back to its default, but Settings still held the
	// rejected value — so esc persisted it straight back to disk and the file never
	// healed: every future open re-read a value the pane had already refused. Storing the
	// normalised form makes the rejection stick, and an empty field is exactly what the
	// deviations-only convention already means.
	cs := Settings.costView()
	Settings.Cost = cs
	m.costPane.applySettings(cs)
	// resumeCostPolling is shared with the CATALOG-RETURN path (keys.go's paneCatalog esc
	// arm), which resumes whichever pane it is returning to. This comment used to claim
	// the sharing as the reason the two could not drift, while that arm resumed only
	// paneUsage — so `$` `P` `esc` came back to a Cost pane with a dead chain, its
	// freshness line counting up forever against a figure nothing would refresh.
	return m.resumeCostPolling()
}

// resumeCostPolling starts exactly one chain, invalidating whatever preceded it.
//
// invalidate() rather than a bare tickGen++, for the reason startSpendPolling gives:
// any path that starts a chain then gets the in-flight-reply guarantee without
// having to remember it. Fetching immediately as well means the pane is current on
// arrival rather than blank for up to costPollInterval.
func (m *model) resumeCostPolling() tea.Cmd {
	m.costPane.invalidate()
	return tea.Batch(m.fetchCost(), costTick(m.costPane.tickGen))
}

// beginCostFetch invalidates what is on screen and returns the command for a fresh
// request. Every view-option change goes through it.
//
// It does NOT bump tickGen — that would orphan the live chain and leave the pane
// with no scheduled successor. Only the request sequence moves, which is what
// discards the reply to the window the user just cycled away from.
func (m *model) beginCostFetch() tea.Cmd {
	m.costPane.snap = nil
	m.costPane.err = nil
	m.costPane.loading = true
	m.costPane.reqSeq++
	return m.fetchCost()
}

// costTickIsCurrent reports whether a tick belongs to the live chain.
func (m *model) costTickIsCurrent(gen uint64) bool { return gen == m.costPane.tickGen }

// applyCostLoaded stores a reply unless it is stale.
//
// lastFetch moves only on an accepted reply: advancing it for a discarded one would
// have the pane report the age of data it just threw away.
func (m *model) applyCostLoaded(msg costLoadedMsg) {
	if msg.req != m.costPane.reqSeq {
		return
	}
	m.costPane.loading = false
	if msg.err != nil {
		// snap is cleared rather than left in place: once the fetch failed we do not
		// know the current spend, and continuing to draw the last figure would present
		// a stale number as a current one. renderCostPane says so instead.
		m.costPane.snap = nil
		m.costPane.err = msg.err
		return
	}
	m.costPane.err = nil
	m.costPane.snap = msg.snap
	m.costPane.lastFetch = time.Now()
}

// costTick schedules the next poll for the given generation.
func costTick(gen uint64) tea.Cmd {
	return tea.Tick(costPollInterval, func(time.Time) tea.Msg { return costTickMsg{gen: gen} })
}

// fetchCost requests the pane's snapshot off the render loop.
//
// GetUsageWindow rather than GetUsage: two of the three windows are symbolic names a
// time.Duration cannot express. Resolution 0 omits the parameter — this pane reads
// Totals and the window-wide Series, never a per-bucket series, so asking for a
// resolution would only constrain how the server slices data nothing here draws.
//
// Returns nil before a client exists (picker mode), which keeps the tick chain alive
// without issuing a request — the same shape fetchSpend uses.
func (m *model) fetchCost() tea.Cmd {
	if m.client == nil {
		return nil
	}
	client := m.client
	window := m.costPane.window()
	group := m.costPane.group
	// Every issued request gets its own id, including the ones the 20s tick issues,
	// matching fetchSpend. Reusing the previous id would let a slow reply land AFTER a
	// faster newer one and overwrite it with an older figure carrying a fresh timestamp.
	// The doubled increment on the paths that also go through beginCostFetch is
	// harmless: the sequence only has to be monotonic.
	m.costPane.reqSeq++
	req := m.costPane.reqSeq
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		snap, err := client.GetUsageWindow(ctx, window, 0, "", group)
		return costLoadedMsg{snap: snap, req: req, err: err}
	}
}

// Token-kind bits, matching pipeline.InferenceExtension.PresentKinds and
// parsercommon.Kind (Input=1, CacheRead=2, CacheWrite=4, Output=8, Reasoning=16).
//
// Declared here rather than imported for the reason cmd_cost.go gives for its own
// copy: abctl decodes a wire shape, and the JSON contract is what pins the layout —
// parsercommon lives under authlib/plugins/internal and is not importable from here
// at all.
const (
	kindInput uint8 = 1 << iota
	kindCacheRead
	kindCacheWrite
	kindOutput
	kindReasoning
)

// Layout constants. costIndent is what makes a line a BODY line rather than a
// heading, which is the contract sectionOf reads in the tests: every heading is
// unindented, every line under it is indented.
const (
	costIndent = "  "
	costColGap = "  "
	// costMaxBarWidth caps the share bars so a 200-column terminal does not render a
	// 180-cell block. Bars encode a proportion; past a point more cells add no
	// precision a reader can use.
	costMaxBarWidth = 28
	// costMinBarWidth is the width below which a bar stops being readable and is
	// dropped entirely. Four cells cannot distinguish 10% from 20%.
	costMinBarWidth = 6
	// costMaxSeriesRows and costMaxGapRows cap the two list sections. Both disclose
	// what they elided, because a silently truncated list reads as a complete one.
	costMaxSeriesRows = 8
	costMaxGapRows    = 5
)

// truncCells shortens s to at most n DISPLAY CELLS, marking the cut with an
// ellipsis.
//
// Cells, not runes and not bytes. footer.go:88-92 records what the difference costs:
// a budget computed in display columns and then sliced by rune index rendered 55
// columns for a 40-column budget on wide characters. The package's trunc (rune
// indexed) and truncStr (byte indexed) are both display-cell-unaware, so neither is
// usable anywhere in this file — and model names are workload-chosen, arriving
// verbatim from the request body, so full-width input is not hypothetical.
//
// Only ever applied to LABELS. A figure is never truncated: "$12.5" for a $12.5000
// session is not a shortened number, it is a different and smaller one.
//
// Measured in the same units the CALLER measures, which is the bug this function
// used to have and the reason it now ends in a correction loop. It built the prefix
// one RUNE at a time and charged each rune lipgloss.Width(string(r)), while every
// caller measures the RESULT with lipgloss.Width — and those two disagree wherever a
// grapheme cluster is wider than the sum of its runes. Variation-selector-16 is the
// everyday case: Width("⚠️") is 2 while Width("⚠") + Width("️") is 1 + 0. The
// per-rune sum therefore UNDER-charged, the result came back up to twice its budget,
// and renderCostRows' pad went negative — `strings: negative Repeat count`, thrown
// from inside View(), which is fatal in bubbletea and leaves the terminal in
// alt-screen. Model names are workload-chosen and recorded verbatim, so "⚠️-model"
// is a name a workload can simply pick.
func truncCells(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	budget := n - 1 // the ellipsis costs one cell
	cells := splitCells(s)
	kept := 0
	w := 0
	for _, c := range cells {
		cw := lipgloss.Width(c)
		if w+cw > budget {
			break
		}
		kept++
		w += cw
	}
	// Self-correcting, and this is the load-bearing half. Whatever the loop above
	// believed each cluster cost, the string it produced is what the caller will
	// measure, so shrink until THAT measurement fits. A future disagreement between
	// per-cluster and whole-string width then costs a shorter label rather than a
	// panic.
	out := strings.Join(cells[:kept], "")
	for kept > 0 && lipgloss.Width(out+"…") > n {
		kept--
		out = strings.Join(cells[:kept], "")
	}
	return out + "…"
}

// splitCells splits s into display clusters: each leading rune together with every
// zero-width rune that follows it.
//
// Not a full Unicode grapheme segmenter — rivo/uniseg is only an INDIRECT dependency
// here, so importing it would mean editing go.mod, and the self-correcting loop in
// truncCells is what actually guarantees the budget. What this buys is that the loop
// rarely has to correct: a combining accent, a variation selector and a ZWJ all carry
// zero width of their own, so grouping them with the rune they modify makes
// lipgloss.Width of the GROUP the real cost of the group. A ZWJ emoji sequence still
// splits into several groups and is over-charged rather than under-charged, which
// truncates a little early and cannot overflow.
func splitCells(s string) []string {
	out := make([]string, 0, len(s))
	for _, r := range s {
		if len(out) > 0 && lipgloss.Width(string(r)) == 0 {
			out[len(out)-1] += string(r)
			continue
		}
		out = append(out, string(r))
	}
	return out
}

// wrapCells word-wraps prose to n display cells per line.
//
// Prose only — the caveat sentences, never a figure. A word wider than the budget is
// truncated rather than hard-split, because the alternative on a 40-column terminal
// is a model name broken across two lines and unrecognisable in both.
func wrapCells(s string, n int) []string {
	if n <= 0 {
		return nil
	}
	var out []string
	line := ""
	for _, word := range strings.Fields(s) {
		switch {
		case line == "":
			line = truncCells(word, n)
		case lipgloss.Width(line)+1+lipgloss.Width(word) <= n:
			line += " " + word
		default:
			out = append(out, line)
			line = truncCells(word, n)
		}
	}
	if line != "" {
		out = append(out, line)
	}
	return out
}

// costRow is one line of a list section: a wire-derived label, an optional share to
// draw as a bar, and the figures that must never be clipped.
//
// hasBar is separate from frac == 0 on purpose. An unpriced series has an UNKNOWN
// share, not a zero one, and an empty bar beside a blank right column would read as
// "this cost nothing" — which is the single claim this pane is forbidden to make.
type costRow struct {
	label  string
	right  string
	frac   float64
	hasBar bool
}

// renderCostRows lays rows out into aligned lines that fit width.
//
// Three columns — label, bar, figures — with the bar taking whatever is left over
// and being dropped entirely below costMinBarWidth. Dropping the bar rather than the
// figures is the priority order the whole pane holds to: the bar is a reading aid for
// a number that is already on the line, so losing it costs nothing but comfort.
//
// The label column is truncated to fit; the figure column never is. Returns nil when
// even a one-cell label cannot sit beside the figures, which the caller renders as a
// dropped section rather than as a row with no name on it.
//
// Every measurement is lipgloss.Width. See truncCells for what the alternative costs.
func renderCostRows(rows []costRow, width int) []string {
	if len(rows) == 0 {
		return nil
	}
	rightW, labelW := 0, 0
	for _, r := range rows {
		if w := lipgloss.Width(r.right); w > rightW {
			rightW = w
		}
		if w := lipgloss.Width(r.label); w > labelW {
			labelW = w
		}
	}
	fixed := lipgloss.Width(costIndent) + lipgloss.Width(costColGap) + rightW
	if avail := width - fixed; labelW > avail {
		labelW = avail
	}
	if labelW < 1 {
		return nil
	}
	// Whatever is left after the label and the figures, capped. Computed once for the
	// whole section so the bars share a baseline — a per-row width would make two rows
	// with the same share draw different lengths.
	barW := width - fixed - labelW - lipgloss.Width(costColGap)
	if barW > costMaxBarWidth {
		barW = costMaxBarWidth
	}
	if barW < costMinBarWidth {
		barW = 0
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		lbl := truncCells(r.label, labelW)
		var b strings.Builder
		b.WriteString(costIndent)
		b.WriteString(lbl)
		// max(..., 0), not the bare subtraction. truncCells guarantees the label fits, so
		// this can only clamp when that guarantee is broken — and the bare form was how a
		// broken guarantee reached the screen: strings.Repeat panics on a negative count,
		// from inside View(), which kills abctl and leaves the terminal in alt-screen.
		// Defence in depth on purpose: a future measurement bug now degrades to a ragged
		// column instead of a crash.
		b.WriteString(strings.Repeat(" ", max(labelW-lipgloss.Width(lbl), 0)))
		if barW > 0 {
			b.WriteString(costColGap)
			b.WriteString(costBar(r.frac, r.hasBar, barW))
		}
		b.WriteString(costColGap)
		// Clamped for the same reason, though rightW is a maximum over these very rows so
		// it cannot go negative today. The cost of the clamp is nothing; the cost of
		// relying on that invariant is the pane.
		b.WriteString(strings.Repeat(" ", max(rightW-lipgloss.Width(r.right), 0)))
		b.WriteString(r.right)
		out = append(out, b.String())
	}
	return out
}

// costFrac clamps a share into [0,1], treating NaN as no share.
//
// NaN FIRST, and by self-comparison, because NaN fails both range comparisons and would
// otherwise pass through untouched. int(NaN) is then implementation-defined: 0 on arm64,
// -2^63 on amd64 — where costBar's strings.Repeat panics, inside View(), which kills
// abctl. That is an architecture-dependent crash, which is the worst kind to leave in:
// it does not reproduce on the machine it was written on, so no local test run can find
// it.
//
// Split out of costBar rather than inlined so the guard is DIRECTLY assertable. Inline,
// deleting the NaN arm is invisible on arm64 — int(NaN) happens to land on 0 there and
// costBar's output is unchanged — so no test on this machine could pin it. A function
// returning the clamped number can be checked on any architecture.
//
// Zero rather than "unknown": costBar's `has` flag already carries that distinction, and
// a share that is not a number is not a share.
func costFrac(frac float64) float64 {
	if frac != frac || frac < 0 {
		return 0
	}
	if frac > 1 {
		return 1
	}
	return frac
}

// costBar draws frac of n cells, padded to n so the column after it stays aligned.
//
// An absent share draws BLANK, not an empty bar at the left edge: the two look
// identical at 0% and mean opposite things, so the row's figure column carries the
// distinction and the bar declines to guess. n is a cell count and the block glyph is
// one cell wide, so the arithmetic and lipgloss.Width agree here.
func costBar(frac float64, has bool, n int) string {
	if !has || n <= 0 {
		return strings.Repeat(" ", max(n, 0))
	}
	frac = costFrac(frac)
	filled := int(frac*float64(n) + 0.5)
	// A non-zero share always gets at least one cell. Rounding 0.4% of 28 cells to
	// nothing renders a present-but-tiny series identically to an absent one.
	if filled == 0 && frac > 0 {
		filled = 1
	}
	return strings.Repeat("█", filled) + strings.Repeat(" ", n-filled)
}

// costSection is one stacked block: an unindented heading and its indented body.
//
// A struct rather than a []string so the height budget can drop a WHOLE section
// rather than trailing lines of one. Half a section is worse than none: a truncated
// COVERAGE reads as a complete list of gaps, and a truncated figure reads as a
// smaller number.
type costSection struct {
	heading string
	lines   []string
}

// costMicrosFloor is the smallest figure four decimal places can state, in micros:
// $0.0001. Anything positive below half of it renders as "$0.0000".
//
// The sibling of prune_saving.go's usdFloor, in this pane's integer units. Same
// number, same argument, and deliberately the same SPELLING of the result — see
// costMoney.
const costMicrosFloor = 100

// costMoney renders micros as dollars to four places, or says it is below them.
//
// Four, not two: a single turn can cost a fraction of a cent, and rounding it to
// $0.00 would render a real cost as free — the one thing this pane must never do.
// Division by 1e6 is the only arithmetic anywhere in this file that touches money:
// the server publishes an integer count of millionths, and turning that into a
// display string is presentation. Multiplying tokens by a rate would be pricing, and
// pricing does not happen here.
//
// But four decimals only MOVE the threshold; they do not remove it. Anything under 50
// micros still printed "$0.0000", and 30 micros is an ordinary cache-read-only turn —
// 100 cache-read tokens at $0.30/MTok. So a real, priced, non-zero cost read as free,
// beside a full bar and 100.0%, which is precisely the claim this pane exists to
// refuse. A settled zero is LEGITIMATE here (the producer means "this call was free"),
// so "free" and "too small to state" have to render differently or the distinction the
// whole surface is built on is lost at the last step.
//
// "<$0.0001" rather than a new spelling: formatUSDCell already established that form
// for the same floor in the sessions table, and two conventions for one fact is worse
// than either.
func costMoney(micros int64) string {
	if micros > 0 && micros < costMicrosFloor/2 {
		return "<" + fmt.Sprintf("$%.4f", float64(costMicrosFloor)/1e6)
	}
	return fmt.Sprintf("$%.4f", float64(micros)/1e6)
}

// costShare renders one published figure as a percentage of another, or nothing.
//
// Legitimate for the same reason costMoney is: both operands come off the wire, and a
// ratio of two published numbers is a way of displaying them, not a new measurement.
// The moment a percentage is taken of something the server did not publish it stops
// being presentation.
//
// Three refusals, and each is a pair of numbers that cannot both be right:
//
// whole <= 0 would divide by zero and render "NaN%" or "+Inf%" — in the breakdown, the
// token tiers and the caveat line at once. Nothing tested this, and weakening it to
// "whole < 0" survived the whole suite.
//
// part < 0 is a negative share. The server refuses negative costs, but this pane never
// restated that guarantee, and inheriting one silently is how it stops holding.
//
// part > whole renders something like "200.0%" beside a bar costBar has clamped to
// 100% — the number and the glyph then disagree, and a reader has no way to tell which
// one is the lie. Omitted rather than capped: capping to 100.0% would assert a share
// that is not the ratio of the two published figures. The row keeps its dollar figure
// and loses only the comparison, which is the priority order the whole pane holds to.
func costShare(part, whole int64) string {
	if whole <= 0 || part < 0 || part > whole {
		return ""
	}
	return fmt.Sprintf("%.1f%%", 100*float64(part)/float64(whole))
}

// costTotalFigure renders the pane's headline amount WEARING THE MARKERS THAT QUALIFY IT.
//
// Marked on the figure, not only spelled out in the prose beside it, and that is what makes
// the height budget safe rather than merely tidy. partialMarker's own doc states the rule the
// strip has always held to: a marker rides on the figure, so a budget that drops the
// explanation can never turn a qualified total into one that looks complete. The Cost pane
// was the ONE money surface stating its caveats in words alone — and words are exactly what a
// four-row terminal cuts off, which is how renderCostPane's fallback came to publish a bare
// "$12.5000" for a total that was both partial and inexact.
//
// Same three glyphs as the strip, the sessions COST cell and the Usage pane cell, in the same
// order and with the same meanings: damaged outermost (rows missing from the sum), then
// inexact (a figure in the sum is a floor), then partial as a suffix (the total covers only
// the priced subset). One spelling everywhere; see damagedMarker for why the three are not
// two.
//
// Three cells at most, on a line that already survives costWidthFloors' narrowest width with
// the figure alone, so nothing here can push the answer off the pane.
func costTotalFigure(snap *usage.Snapshot) string {
	amount := costMoney(snap.Totals.CostMicros)
	if snap.Totals.IncompleteRequests > 0 {
		amount = inexactMarker + amount
	}
	// Both causes of "short by an unstatable amount" take the same glyph, and a total that is
	// damaged AND clamped wears it once: the marker is the claim, not a count of reasons. See
	// figureIsShort and damagedMarker.
	if figureIsShort(snap.Degraded, snap.Totals.Saturated) {
		amount = damagedMarker + amount
	}
	// A coverage gap needs its denominator to be a gap at all: PriceableRequests of zero is
	// a window with nothing to price, not a total covering part of something.
	if p := snap.Totals.PriceableRequests; p > 0 && snap.Totals.PricedRequests < p {
		amount += partialMarker
	}
	return amount
}

// costTotalParts is costTotalSection's body, split into the units a height budget may drop
// WHOLE.
//
// It exists because renderCostPane's last-resort path cut the FLAT line list at
// out[:height], and the lines it cut were the coverage ratio and the lower-bounds caveat —
// so at layout()'s own four-row floor the pane kept the dollar amount and discarded
// everything that said the amount was partial or inexact. The comment there claimed "whole
// lines only", which was true of the FIGURE and of nothing else: every caveat is wrapped
// prose, and a wrapped sentence cut at a line boundary is half a sentence.
//
// head is the answer and travels together: the figure line, plus the provenance note when it
// had to move to its own line, or the whole of the "cost unavailable" prose on the paths that
// publish no figure.
//
// caveats are self-contained blocks, most severe first. A budget takes them whole or not at
// all. Dropping one is safe in a way cutting one is not, because costTotalFigure puts each
// claim on the figure as a marker: what is lost is an explanation, never the fact.
type costTotalParts struct {
	head    []string
	caveats [][]string
}

// lines flattens the parts back into a section body, which is what a height budget that fits
// the whole section wants.
func (p costTotalParts) lines() []string {
	out := append([]string(nil), p.head...)
	for _, c := range p.caveats {
		out = append(out, c...)
	}
	return out
}

// costTotalBody builds the TOTAL section's units. See costTotalParts for why they are units.
func costTotalBody(snap *usage.Snapshot, width int) costTotalParts {
	var parts costTotalParts
	body := width - lipgloss.Width(costIndent)
	wrap := func(prose string) []string {
		var out []string
		for _, l := range wrapCells(prose, body) {
			out = append(out, costIndent+l)
		}
		return out
	}
	// A damage disclosure is a unit on every path, and it LEADS the caveats, because it is
	// the only one of the three that says the SUM is incomplete rather than qualifying a
	// figure inside it. Coverage knows the size of what it excludes and can name the pricing
	// entry that would close it; exactness knows how many figures are floors. This one cannot
	// state how much is missing, which makes it both the most serious and the least
	// actionable, and it must not be merged with either — see snapshotDamaged and
	// usage.Snapshot.Degraded.
	//
	// This pane's window defaults to "today" (costPaneWindows[0]), the only window that can
	// populate the field, so this is the default path rather than an edge of one.
	//
	// A CLAMPED AGGREGATE LEADS EVEN THAT, and it is the one caveat that qualifies every
	// number in the answer rather than the dollars alone. usage.Counts.Saturated says an
	// addition into these totals hit the int64 ceiling and was capped rather than allowed to
	// wrap, so the requests, the tokens and the cost are all FLOORS. A damaged read loses rows
	// from one sum; this loses value from every sum in the struct, which is why it goes first.
	// Both wear damagedMarker on the figure — see figureIsShort — and each gets its own words,
	// because "a day file was lost" and "the arithmetic overflowed" send an operator to
	// completely different places.
	addDamage := func() {
		if snap.Totals.Saturated {
			parts.caveats = append(parts.caveats, wrap(costSaturatedNote))
		}
		if snapshotDamaged(snap.Degraded) {
			parts.caveats = append(parts.caveats, wrap(costDamagedNote(snap.Degraded)))
		}
	}
	if !snap.Priced {
		// Never "$0.0000". A zero cost and an unknown cost are different answers and
		// only one of them means the traffic was free.
		parts.head = wrap("cost unavailable — nothing in this window carried a cost")
		if gap := snap.Totals.PriceableRequests; gap > 0 {
			parts.caveats = append(parts.caveats, wrap(fmt.Sprintf(
				"%s priceable request%s went through, none of them priced",
				formatCount(int(gap)), plural(int(gap)))))
		}
		// Stated on this path TOO, and it is not redundant: "nothing carried a cost" is a
		// claim about the rows that were READ, and a damaged read may have lost the priced
		// ones. Without it a corrupt day file renders as a quiet day.
		addDamage()
		return parts
	}
	// A negative total is not a total. The server refuses negative costs, so this cannot
	// happen against a correct producer — which is exactly why the pane said "$-5.0000"
	// when the reviewer made it happen: the guarantee lived entirely on the other side of
	// the wire and this side never restated it. Treated as unpriced, because that is what
	// an impossible figure is: not a number to display, and certainly not a credit.
	//
	// Through negativeCost, which is now that test's one spelling on every money surface.
	// This pane held it alone and three others inherited it without restating it.
	if negativeCost(snap.Totals.CostMicros) {
		parts.head = wrap("cost unavailable — the server reported a negative total, " +
			"which cannot be spend")
		// A refused figure does not make a damaged read stop mattering: the two are
		// independent facts and an operator with a corrupt day file wants to hear about it
		// whatever else went wrong in the same answer.
		addDamage()
		return parts
	}
	// The figure on its own line so the height budget can never cut it, and the
	// provenance beside it because $12.40 from a gateway's own numbers and $12.40
	// modelled from a shipped vendor list are not equally trustworthy. provenanceNote
	// is silent for a wholly authoritative total, which is the baseline a reader
	// already assumes.
	//
	// Beside it only WHEN IT FITS. A mixed-provenance total on a narrow terminal renders
	// "[authoritative 200, bundled 60, configured 40]" — 46 columns of annotation next to
	// a 7-column figure — so on anything narrower the note moves to its own wrapped line
	// below. It is never truncated onto the figure's line, because clipping there would
	// eat the digits: the figure is the answer, the annotation qualifies it.
	//
	// The moved note joins HEAD rather than becoming a caveat unit: it belongs to the figure
	// it annotates, and a provenance note that outlived its own figure would say "bundled
	// 60" over nothing.
	figure := costIndent + costTotalFigure(snap)
	prov := provenanceNote(costPricedBy(snap.PricedBy))
	if lipgloss.Width(figure+prov) <= width {
		parts.head = []string{figure + prov}
	} else {
		parts.head = append([]string{figure}, wrap(strings.TrimSpace(prov))...)
	}

	priced, priceable := snap.Totals.PricedRequests, snap.Totals.PriceableRequests
	addDamage()
	// Against PRICEABLE requests, never all of them. Requests counts every proxied
	// response — MCP tool calls, health checks — while only inference can ever be
	// priced, so the wrong denominator left a correctly configured deployment reading
	// a permanent warning with nothing to act on.
	if priceable > 0 && priced < priceable {
		parts.caveats = append(parts.caveats, wrap(fmt.Sprintf(
			"covers %s of %s priceable requests — the rest carry no figure",
			formatCount(int(priced)), formatCount(int(priceable)))))
	}
	// IncompleteRequests is a SUBSET of PricedRequests: their dollars are in the total
	// and the disclosure rides alongside. Rendered only when it is non-zero, which is
	// the other half of the rule — a permanent caveat with nothing to act on is what
	// teaches an operator to ignore the one that matters.
	if inc := snap.Totals.IncompleteRequests; inc > 0 {
		parts.caveats = append(parts.caveats, wrap(costInexactNote(inc, priced, snap.IncompleteBy)))
	}
	return parts
}

// costTotalSection is the answer to the pane's own question, with every caveat that
// applies to it and none that does not.
//
// It carries the coverage RATIO as well as the figure, while the COVERAGE section
// below names the pairs. That split is deliberate: the ratio is the part that must
// survive the height budget, because a partial total presented without it reads as a
// complete one, and COVERAGE is the first section a short terminal drops.
//
// A thin wrapper over costTotalBody, whose units renderCostPane's fallback needs: this
// section is the flat form, for a budget that can take the whole of it.
func costTotalSection(snap *usage.Snapshot, width int) costSection {
	return costSection{heading: "TOTAL", lines: costTotalBody(snap, width).lines()}
}

// costSaturatedNote is the Cost pane's spelling of a CLAMPED AGGREGATE.
//
// usage.Counts.Saturated is a BOOL, not a counter, and its doc says why: "this number is a
// ceiling" is a property of the number and survives any regrouping, where "it was clamped four
// times" is a property of the arithmetic path and does not. So this is a constant sentence with
// no figure in it — there is no honest number to interpolate, and inventing one ("clamped by
// $N") would be the same fabrication the pane refuses everywhere else.
//
// EVERY FIGURE, not just the dollars, and the sentence says all three out loud. Add clamps on
// the whole Counts, so Requests and Tokens are floors alongside CostMicros. A caveat naming
// only money would leave a reader trusting a request count that is also short — and the
// request count is the denominator of every ratio on this pane.
//
// It says CLAMPED RATHER THAN WRAPPED, because that is the only reason the number on screen is
// worth anything at all: the alternative was a wrapped total, which is a large negative or a
// small positive presented as a fact. The clamp is what makes the figure a floor instead of
// fiction, and this disclosure is what makes the clamp honest.
const costSaturatedNote = "every figure in this window is a FLOOR — a total reached the " +
	"largest whole number the aggregate can hold and was CLAMPED rather than allowed to wrap, " +
	"so the real requests, tokens and cost are all larger, by an amount nothing here can state"

// costDamagedNote is the Cost pane's spelling of a damaged ledger read: the sentence form,
// where the strip has room only for damagedNote's three words and a one-cell marker.
//
// It says the total is SHORT, names what was lost, and says the shortfall is UNSTATABLE.
// All three do work. "Incomplete" alone gives an operator nothing to act on, where "1 day
// file abandoned part-way" names something to go and look at. And the unstatable part is
// the whole difference from the inexactness caveat below it: that one knows how many
// figures are floors and which direction the total is wrong in; this one cannot know how
// much spend is absent, because the rows that would say are the rows that could not be
// read.
//
// A truncated day is called out as worse than a skipped line, because it is worse by an
// unbounded amount: a line is one request, a file is a day.
//
// No counters at all is still a disclosure — see snapshotDamaged for why presence is the
// claim — and gets the sentence without a number rather than being dropped.
func costDamagedNote(d *usage.Degraded) string {
	switch {
	case d.SkippedLines > 0 && d.TruncatedDays > 0:
		return fmt.Sprintf("this total is SHORT — the cost ledger skipped %s unreadable line%s "+
			"and abandoned %s day file%s part-way; that spend happened and is missing from the "+
			"sum, by an amount nothing here can state",
			formatCount(int(d.SkippedLines)), plural(int(d.SkippedLines)),
			formatCount(int(d.TruncatedDays)), plural(int(d.TruncatedDays)))
	case d.SkippedLines > 0:
		return fmt.Sprintf("this total is SHORT — the cost ledger skipped %s unreadable line%s; "+
			"that spend happened and is missing from the sum, by an amount nothing here can state",
			formatCount(int(d.SkippedLines)), plural(int(d.SkippedLines)))
	case d.TruncatedDays > 0:
		return fmt.Sprintf("this total is SHORT — the cost ledger abandoned %s day file%s "+
			"part-way; a file holds a whole day, so the amount missing from the sum is unbounded",
			formatCount(int(d.TruncatedDays)), plural(int(d.TruncatedDays)))
	default:
		return "this total is SHORT — the cost ledger reported an incomplete read without " +
			"saying how much it lost; rows are missing from the sum"
	}
}

// costInexactNote is the pane's sentence for an inexact total, and it names a DIRECTION only
// where the response named one.
//
// It used to assert one unconditionally: "N of M priced figures are lower bounds — a stream
// ended before its output count arrived, so the real total is higher". That sentence is true
// for pricing.ReasonOutputUncounted and FALSE for pricing.ReasonSplitUnreported, where a
// gateway reported only a total and the figure is off in NO KNOWN DIRECTION — over-priced for
// a cache-heavy request, under-priced for a generation-heavy one. It was printed over both,
// because Totals.IncompleteRequests is a single number that cannot tell them apart.
// usage.Snapshot.IncompleteBy is the field that can, and reading it is the difference between
// "the total is at least this" and "the total is about this" — different claims about money.
//
// The two also want different reactions, which is why one wording cannot serve: a floor is
// usually a transient failure worth chasing (a stream that died), while an approximation is a
// standing property of a gateway that holds for every request it answers. Presented as an
// incident, the second teaches an operator to ignore the first.
//
// NO REASONS AT ALL gets the direction-free wording rather than the old sentence, and that is
// the common case on this pane: today and 7d are ledger-backed, and a persisted per-minute row
// has no reason column, so IncompleteBy is never populated for them. The count line still
// prints — absence of the reasons is not a claim of exactness, per usage.Snapshot.IncompleteBy's
// own doc — it simply stops claiming to know which way.
//
// An unrecognised key is PRINTED under its own name, never dropped: the map's counts sum to
// IncompleteRequests by contract, so a dropped key would leave a reader's own subtraction
// implying those figures were exact. Sanitized on the way out for the reason costPricedBy
// sanitizes its keys — a reason string is a wire value, and an ESC in one reaches the TTY.
func costInexactNote(inc, priced int64, by map[string]int64) string {
	head := fmt.Sprintf("%s of %s priced figures are inexact",
		formatCount(int(inc)), formatCount(int(priced)))
	clauses := costInexactClauses(by)
	if len(clauses) == 0 {
		return head + " — the total is not exact, and this window does not record which way"
	}
	return head + " — " + strings.Join(clauses, "; ")
}

// costInexactClauses is costInexactNote's per-reason prose, in a fixed order: the floor first
// because it is the stronger claim and the only one with a direction, then the approximation,
// then a caveat whose kind nobody named, then any key this build does not know (sorted, so two
// runs of the same data read the same).
//
// The keys come from pricing rather than being spelled here, so what this matches on is what
// the producer writes. "unlabelled" has to be a literal — usage keeps that key unexported —
// and usage.Snapshot.IncompleteBy's doc is where its meaning is defined.
func costInexactClauses(by map[string]int64) []string {
	if len(by) == 0 {
		return nil
	}
	var out []string
	known := map[string]bool{}
	for _, r := range []struct{ key, singular, plural string }{
		{pricing.ReasonOutputUncounted,
			"1 is a lower bound (a stream ended before its output count arrived, so the real total is higher)",
			"%s are lower bounds (a stream ended before its output count arrived, so the real total is higher)"},
		{pricing.ReasonSplitUnreported,
			"1 is an approximation (a gateway reported only a total, so it is off in no known direction)",
			"%s are approximations (a gateway reported only a total, so they are off in no known direction)"},
		{"unlabelled",
			"1 carries a caveat whose kind its producer did not name",
			"%s carry a caveat whose kind their producer did not name"},
	} {
		known[r.key] = true
		switch n := by[r.key]; {
		case n == 1:
			out = append(out, r.singular)
		case n > 1:
			out = append(out, fmt.Sprintf(r.plural, formatCount(int(n))))
		}
	}
	var rest []string
	for k, n := range by {
		if !known[k] && n > 0 {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		out = append(out, fmt.Sprintf("%s under %q, a reason this build does not know",
			formatCount(int(by[k])), sanitizeLabel(k)))
	}
	return out
}

// costPricedBy sanitizes the provenance keys before they reach provenanceNote.
//
// snap.PricedBy was the ONE wire-derived string this pane wrote out raw. snap.Window,
// every series label, every UnpricedBy key and the error text all go through
// sanitizeLabel; these keys did not, and they are just as much a wire value — a
// producer that names a pricing level "bun\ndled\x1b[31m" sends ESC straight to the
// TTY (CWE-150, the hazard this file cites four times), while its newline both blows
// the height budget and emits an UNINDENTED body line, breaking the "every heading is
// unindented" contract the section logic and sectionOf both read.
//
// Here rather than inside provenanceNote because that function lives in
// usage_render.go, which this change does not own. Sanitizing at the call site is
// equivalent for this caller; if the Usage pane ever grows the same note, the fix
// belongs one level down.
//
// Keys are summed rather than overwritten on collision: two raw keys can sanitize to
// the same string, and the note reports counts.
func costPricedBy(by map[string]int64) map[string]int64 {
	if len(by) == 0 {
		return nil
	}
	out := make(map[string]int64, len(by))
	for k, v := range by {
		out[sanitizeLabel(k)] += v
	}
	return out
}

// costSeries sums a snapshot's per-label counts across the WHOLE window.
//
// Every bucket, not Buckets[0]. This pane asks for a single-bucket resolution but the
// server negotiates it (see Snapshot.BucketSeconds), and a ledger-backed window
// returns exactly one bucket while a ring-backed one returns many — so reading the
// first bucket would report the first slice of the window as the whole of it.
//
// An empty label is dropped. The aggregator already refuses to key a series on one,
// but a bucket that arrived from an older producer could carry it, and a row with no
// name against a real dollar figure is unactionable.
func costSeries(snap *usage.Snapshot) map[string]usage.Counts {
	out := map[string]usage.Counts{}
	for _, b := range snap.Buckets {
		for label, c := range b.Series {
			if label == "" {
				continue
			}
			cur := out[label]
			cur.Add(c)
			out[label] = cur
		}
	}
	return out
}

// costUngroupedLabel names the residual band: the spend NO row on this axis carries.
//
// A STATE, not a name, in costUnattributedLabel's convention — parenthesised and
// lowercase — rather than a second spelling of the same idea. That constant's brackets
// are load-bearing for the reason its own doc gives, and the reason carries over here: no
// User-Agent product token begins with one, and nor does a model or a host a gateway
// would route, so the band cannot be mistaken for a key at a glance.
//
// "(unattributed)" rather than "(other)". "Other" implies a residual of the same KIND as
// the rows above it — a quieter model, a cheaper endpoint — when the truth is that these
// dollars have no value on this axis AT ALL: usage.Snapshot.UngroupedCostMicros' dominant
// case is a gateway-priced /v1/embeddings response, which carries no model to be "other"
// than.
//
// NOT collision-proof, and cannot be, exactly as costUnattributedLabel records: a series
// key is wire-derived, so a caller may send this literal. On group=agent both labels can
// appear in one table, and they say different things — "(no user-agent)" is a row of real
// spend whose requests named no client, "(unattributed)" is spend with no row at all.
// Two spellings because they are two facts; one spelling would lose the distinction.
const costUngroupedLabel = "(unattributed)"

// costUngroupedRow is the residual band, or false when the breakdown accounts for every
// dollar.
//
// ABSENT MEANS RECONCILED, so a nil pointer renders NOTHING rather than a zero band. A
// "(unattributed) $0.0000" row would assert a residual that does not exist, on every
// correctly attributed window there is — the permanent-caveat-with-nothing-to-act-on
// failure this pane refuses in costCoverageSection and in every caveat above. The field is
// a pointer for precisely that reading; see usage.Snapshot.UngroupedCostMicros and
// usage.Group.Reconcilable, where absence on group=none and group=plugin means "no
// reconciliation is offered" rather than "the breakdown is complete".
//
// A non-positive value renders nothing too, and that is DEFENCE IN DEPTH rather than dead
// code: usage.SetUngroupedCost already refuses to publish one, and this pane restates the
// guarantees it leans on instead of inheriting them silently — the same rule costTotalBody
// applies to a negative total, which is a figure the server also promises never to send. A
// zero is the absent case wearing a value; a negative residual is not spend.
//
// The share and the bar come from costShare, the same ratio-of-two-published-figures the
// label rows get, against the same denominator. That is what makes the band comparable
// with the rows rather than merely adjacent to them.
func costUngroupedRow(snap *usage.Snapshot) (costRow, bool) {
	if snap.UngroupedCostMicros == nil {
		return costRow{}, false
	}
	micros := *snap.UngroupedCostMicros
	if micros <= 0 || negativeCost(micros) {
		return costRow{}, false
	}
	row := costRow{label: costUngroupedLabel, right: costMoney(micros)}
	if pct := costShare(micros, snap.Totals.CostMicros); pct != "" {
		// Padded like the label rows' share so the two line up on the percent sign; see the
		// same expression in costBreakdownSection for why that matters.
		row.right += costColGap + fmt.Sprintf("%6s", pct)
		row.frac = float64(micros) / float64(snap.Totals.CostMicros)
		row.hasBar = true
	}
	return row, true
}

// costOvershootNote is the breakdown's DEFECT REPORT, or "" when there is none.
//
// usage.Snapshot.SeriesOvershootMicros is the residual with the wrong sign: the amount by
// which this response's series summed to MORE than Totals.CostMicros. Its own doc is
// explicit that this is not a property of the traffic — every event lands in at most one
// entry of a reconcilable group's map, so the entries can sum to the total or to less, never
// to more. A positive value means the producer is wrong about its own arithmetic: a Group
// marked Reconcilable whose series double-counts, or an accumulator counting one event twice.
//
// SO IT IS NOT A BAND, AND MUST NOT LOOK LIKE ONE. costUngroupedRow renders the OTHER
// residual as a row with a figure, a share and a bar, because that residual is legitimate
// unattributed spend and belongs in the money column beside the rows it completes. This one
// is the opposite claim — the rows are WRONG — and no chart should draw it. Confusing the two
// would be worse than rendering neither: a reader would take a defect report for real money
// and go looking for the workload that spent it. That is why the field is a separate unsigned
// magnitude rather than a signed residual (see usage.SetUngroupedCost), and rendering it in
// the same column would put the sign back.
//
// It names TOTAL as unaffected, which is the whole difference from costDamagedNote. Totals is
// summed from the raw buckets before any grouping, so an overshoot indicts the SERIES and
// leaves the headline exactly as trustworthy as it was. A reader told only "this answer is
// broken" would stop believing the one figure that is still correct.
//
// The MAGNITUDE is stated even though no chart draws it, because it is what makes the bug
// report actionable: a quarter of a dollar over says one row is doubled, and a figure the
// size of the total says the whole series is. It sits inside a sentence that opens by
// refusing the rows, never in the figure column, for the reason above.
//
// ABSENT OR NON-POSITIVE RENDERS NOTHING, on the same rule costUngroupedRow holds to: the
// field is a pointer precisely so a healthy response serialises no zero, and a zero would
// have to mean both "checked, the series adds up" and "not checked". The non-positive guard
// is defence in depth — usage.SetUngroupedCost publishes only a positive magnitude here — and
// restating it is this pane's convention rather than inheriting a guarantee silently.
func costOvershootNote(snap *usage.Snapshot) string {
	if snap.SeriesOvershootMicros == nil {
		return ""
	}
	over := *snap.SeriesOvershootMicros
	if over <= 0 {
		return ""
	}
	return fmt.Sprintf("DO NOT TRUST THESE ROWS — they sum to %s MORE than the total above, "+
		"which cannot happen to a correct breakdown: a series here is counting spend twice. "+
		"The total itself is unaffected. This is a defect in Cortex, not a gap in your rate "+
		"table — please report it.", costMoney(over))
}

// costBreakdownSection is where the money went, by the selected axis.
//
// Ordered by cost descending, ties broken on the label: map iteration is
// nondeterministic, and a table that reshuffles between 20-second polls is unreadable
// even when every number in it is right.
//
// Series with no priced request sort LAST and render "cost unavailable" rather than a
// figure. Their cost is unknown, not zero, and Snapshot.Priced is window-wide — so a
// snapshot that priced another model's traffic says priced:true, and trusting that
// flag per row would print $0.0000 against a model whose cost nobody knows.
//
// The RESIDUAL BAND closes the arithmetic. sum(rows) is NOT the window total: a
// gateway-priced response the inference parser cannot read (/v1/embeddings, /v1/rerank)
// carries no model, so it counts toward Totals and can be no series key — and until
// usage.Snapshot.UngroupedCostMicros landed, that difference was undisclosed. A reader
// adding this column up got a smaller number than the total two sections above, with
// nothing on screen to explain the gap. The band is that difference, and with it
// sum(rows) + band == Totals.CostMicros exactly, for every axis this pane offers (all four
// are usage.Group.Reconcilable).
func costBreakdownSection(snap *usage.Snapshot, group usage.Group, width int) costSection {
	// The requested axis rather than snap.Group: beginCostFetch clears the snapshot on
	// every view change, so the two cannot disagree, and snap.Group can echo the
	// GroupMethod alias — which would put "BY METHOD" over what is really the model
	// series (see usage.GroupModel's doc for why that name was wrong from the start).
	sec := costSection{heading: "BY " + strings.ToUpper(string(group))}
	body := width - lipgloss.Width(costIndent)
	series := costSeries(snap)
	if len(series) == 0 {
		// Not silence. group=session on a ledger-backed window carries no series at all —
		// a per-minute row holds no session id — and an empty block under a "BY SESSION"
		// heading reads as "none of this cost anything".
		//
		// And NO RESIDUAL BAND here, though this is the path where the field is largest: a
		// session-grouped ledger window populates UngroupedCostMicros with the WHOLE total,
		// because costledger.Fold finds no label for any row and every dollar is therefore
		// ungrouped. A "(unattributed) 100.0%" band would restate the sentence above in a
		// form that reads as a mystery spender, and a residual is the part a BREAKDOWN leaves
		// out — where there is no breakdown there is nothing for it to be residual of.
		// Nothing is understated either way: TOTAL still carries every dollar.
		for _, l := range wrapCells(fmt.Sprintf(
			"no %s breakdown for this window", group), body) {
			sec.lines = append(sec.lines, costIndent+l)
		}
		return sec
	}
	labels := make([]string, 0, len(series))
	for label := range series {
		labels = append(labels, label)
	}
	sort.Slice(labels, func(i, j int) bool {
		a, b := series[labels[i]], series[labels[j]]
		// Priced before unpriced: a known figure outranks an unknown one whatever their
		// notional order, and comparing a real total against a placeholder zero would
		// interleave the two.
		if (a.PricedRequests > 0) != (b.PricedRequests > 0) {
			return a.PricedRequests > 0
		}
		if a.CostMicros != b.CostMicros {
			return a.CostMicros > b.CostMicros
		}
		return labels[i] < labels[j]
	})
	shown := labels
	if len(shown) > costMaxSeriesRows {
		shown = shown[:costMaxSeriesRows]
	}
	rows := make([]costRow, 0, len(shown))
	for _, label := range shown {
		c := series[label]
		// sanitizeLabel because these keys are wire-derived: the model half comes from the
		// request body's `model` field, chosen by the workload and recorded verbatim by the
		// parser, so writing it raw to a TTY lets an escape sequence recolour the pane or
		// erase the very row it is reporting. CWE-150.
		row := costRow{label: costSeriesLabel(label, group)}
		// A negative figure counts as unpriced, not as a small one: see costTotalSection
		// for why the pane restates a guarantee the server already makes. "$-5.0000" in a
		// column of costs reads as a refund nobody issued.
		if c.PricedRequests == 0 || negativeCost(c.CostMicros) {
			row.right = "cost unavailable"
			rows = append(rows, row)
			continue
		}
		row.right = costMoney(c.CostMicros)
		if pct := costShare(c.CostMicros, snap.Totals.CostMicros); pct != "" {
			// Right-padded to a fixed width so the shares line up on the percent sign. Without
			// it "91.6%" and "0.2%" sit at different columns and the eye cannot compare them,
			// which is the one thing a share column is for.
			row.right += costColGap + fmt.Sprintf("%6s", pct)
			row.frac = float64(c.CostMicros) / float64(snap.Totals.CostMicros)
			row.hasBar = true
		}
		rows = append(rows, row)
	}
	// WHERE THE BAND RANKS. It is a row, so it competes for height like one, and it sits
	// LAST INSIDE THIS SECTION — after the label rows, after the "+N more" note — and is
	// deliberately NOT a caveat unit on TOTAL.
	//
	// After the label rows because it is not one of them: it is what they leave out, and a
	// residual read before the rows it is residual of says nothing.
	//
	// After the "+N more" note because that note ends "each smaller than the last row". A
	// band between the rows and the note would make "the last row" name the BAND, so the
	// note would appear to be counting series entries cheaper than the residual — a claim
	// nothing here makes. The two are also DIFFERENT DISCLOSURES and must not read as one:
	// "+N more" is rows omitted FOR SPACE, and a wider or taller terminal reveals them; the
	// band is spend with no row on this axis AT ALL, and no terminal will ever produce one.
	// Merged, they would send a reader looking for a row that cannot exist.
	//
	// Not a TOTAL caveat, though it is arithmetic about money, because nothing the caveats'
	// ranking protects is at stake here. A caveat rides on TOTAL because a partial or
	// inexact figure shown without it reads as a complete one, which is why costTotalFigure
	// puts each claim on the figure as a marker. The residual qualifies the ROWS, not the
	// figure: Totals.CostMicros already includes every ungrouped dollar, so when the height
	// budget drops this section whole the pane still states the total exactly and leaves no
	// column for a reader to add up. It travels with the rows it is about, and it is never
	// half-shown, because fitCostSections drops sections and never clips one.
	//
	// ONE renderCostRows call for the rows and the band together, so they share a single
	// column grid. Rendered separately their figures would sit at different columns, and a
	// band a reader cannot align with the rows is a band they cannot compare — which is the
	// whole purpose of giving it the same share and bar.
	//
	// Outside costMaxSeriesRows on purpose: that cap limits LABEL rows, so counting the band
	// against it would let a long tail of cheap models push the reconciliation off screen.
	labelRows := len(rows)
	band, hasBand := costUngroupedRow(snap)
	if hasBand {
		rows = append(rows, band)
	}
	// The rows FIRST, and an empty section when there are none. renderCostRows returns
	// nil when even a one-cell label cannot sit beside the figures, and appending the
	// "+N more" note regardless produced a section that was all disclosure and no data:
	// at w=20 with ten series, "BY MODEL" followed only by "+2 more, each smaller than
	// the last row" — naming a last row that is not on screen and counting 2 elided when
	// all 10 were lost. The caller drops a section with no lines, which is the honest
	// answer: this axis does not fit this terminal.
	lines := renderCostRows(rows, width)
	if len(lines) == 0 {
		return costSection{}
	}
	// Clamped for the reason renderCostRows clamps its own pads: renderCostRows returns one
	// line per row or none at all, so this cannot bite today, and relying on that invariant
	// silently would cost a panic inside View() if it ever changed.
	if labelRows > len(lines) {
		labelRows = len(lines)
	}
	// THE DEFECT REPORT LEADS, above the rows rather than below them, and it is the one
	// disclosure in this section that does. The band and the "+N more" note both qualify how
	// COMPLETE the rows are, so they follow the rows they are about; this one says the rows are
	// WRONG, and a reader who scans the table and stops has read numbers they were told not to
	// believe. Every other money claim on this branch rides on its figure as a marker for that
	// exact reason (see costTotalFigure); an overshoot has no single figure to ride on — it
	// indicts the whole series — so leading the section is the only form available.
	//
	// Inside the section, so the height budget drops it WITH the rows: fitCostSections takes a
	// section whole or not at all, and with no rows on screen there is nothing left to
	// distrust. Same reason it is not emitted on the no-series path above — "no model breakdown
	// for this window" leaves no breakdown for this to be about, and the field cannot arrive
	// there anyway, since an empty series sums to zero and zero cannot exceed a total the
	// server refuses to publish negative.
	if note := costOvershootNote(snap); note != "" {
		for _, l := range wrapCells(note, body) {
			sec.lines = append(sec.lines, costIndent+l)
		}
	}
	// Copied rather than sliced onto sec.lines. lines[:labelRows] keeps the whole backing
	// array, so appending the note to it would overwrite the band's own line — the section
	// would then show the note twice and the residual not at all.
	sec.lines = append(sec.lines, lines[:labelRows]...)
	if rest := len(labels) - len(shown); rest > 0 {
		// Disclosed, not silently dropped: a truncated list reads as a complete one, and
		// the elided rows are the cheap ones only because the sort put them there.
		for _, l := range wrapCells(fmt.Sprintf("+%d more, each smaller than the last row", rest), body) {
			sec.lines = append(sec.lines, costIndent+l)
		}
	}
	sec.lines = append(sec.lines, lines[labelRows:]...)
	return sec
}

// costUnattributedLabel renders the reserved agent bucket.
//
// pipeline.UnknownClientLabel is the wire value "unknown", and its own doc forbids a
// consumer presenting it as an agent name. Rendered verbatim it would sit in the same
// column as "claude-code/2.1.14" and read as a program called "unknown" that spent
// real money — so this pane, which IS that consumer, spells it as a state instead. The
// brackets are load-bearing: no User-Agent product token contains them at the start,
// so the row cannot be mistaken for a name at a glance.
//
// Not collision-proof, and cannot be: Label() returns the RAW User-Agent for an
// unrecognised client, so a caller sending literally "(no user-agent)" renders
// identically. That is strictly narrower than the status quo it replaces — "unknown" is
// a plausible thing for a real client to send, "(no user-agent)" is not — and the
// spoofability of the whole axis is documented on pipeline.EventClient.
//
// The PARENTHESISED-LOWERCASE FORM is the pane's convention for "this is a state, not a
// key", and costUngroupedLabel is the other member of it. Add to the convention rather
// than inventing a second one: two spellings of "not a name" in one table teaches a
// reader neither.
const costUnattributedLabel = "(no user-agent)"

// costSeriesLabel renders one breakdown key.
//
// Only group=agent gets the substitution. On any other axis "unknown" is a value the
// wire really carried — a model or an endpoint could legitimately be named that — and
// rewriting it there would hide a real key behind a state.
func costSeriesLabel(key string, group usage.Group) string {
	if group == usage.GroupAgent && key == pipeline.UnknownClientLabel {
		return costUnattributedLabel
	}
	// sanitizeLabel for everything else: these keys are wire-derived (see the call
	// site). The constant above needs no sanitising, being authored here.
	return sanitizeLabel(key)
}

// costTierHeading names the token section, and says in the heading what it reports.
//
// "tokens, not dollars" is in the heading rather than in a footnote because the
// footnote is the part a reader skips. See costTokenSection for why there is no
// dollar figure to put here.
const costTierHeading = "WHERE IT WENT (tokens, not dollars)"

// costTier is one billed token kind: its declaration bit, its label, and its count.
type costTier struct {
	bit   uint8
	label string
	count int64
}

// costTokenSection reports the four-way token split as shares.
//
// TOKENS, NOT DOLLARS, and that is the whole design of this section rather than a
// shortcut. usage.Counts aggregates the token split — real, exact, already on the
// wire — and aggregates NO per-tier dollars; nothing does. costevent.Event carries
// PromptUSD and OutputUSD, but PromptUSD's own doc is explicit that when the total
// came from a gateway header "PromptUSD + anything is not a total of anything", and
// OutputUSD warns it is not CostUSD minus PromptUSD. So a four-tier dollar breakdown
// summing to the total would be an invented number, and a prompt-versus-output split
// would fail to reconcile precisely on the deployments whose figures are most
// trustworthy — the ones with a gateway header.
//
// Volume still answers the question a reader came with, because the tiers price
// roughly 1x / 0.1x / 1.25x / output: the largest share is the first place to look.
// If a dollar breakdown is wanted, the honest route is to aggregate the per-tier
// figures into Counts AND disclose that they do not reconcile with an authoritative
// total. That is a separate change with its own argument.
//
// AVOIDED is deliberately absent, which is where a reader would expect it. The
// tool-prune savings container exists upstream but nothing aggregates it into Counts,
// so there is no window-scoped figure: a section reading a per-request field would
// show one request's saving under a window heading.
func costTokenSection(snap *usage.Snapshot, width int) costSection {
	sec := costSection{heading: costTierHeading}
	body := width - lipgloss.Width(costIndent)
	add := func(prose string) {
		for _, l := range wrapCells(prose, body) {
			sec.lines = append(sec.lines, costIndent+l)
		}
	}
	t := snap.Totals
	// The refusal caveat, which belongs to EVERY path out of this function.
	//
	// usage.Counts.RefusedTokenRequests counts requests whose token report was rejected as
	// implausible and contributed nothing to any figure in this section. Non-zero means the
	// counts AND the split are SHORT by an amount that is unknowable by construction — the
	// report that would have said how much is the report that was thrown away.
	//
	// SHORT BY AN UNSTATABLE AMOUNT is damagedMarker's claim, not inexactMarker's, and the
	// glyph is not reused here only because there is no figure for it to ride on: a refused
	// report says nothing about WHICH tier lost tokens, so marking one row would assert a
	// locality the field does not have and marking all of them would spell one fact five
	// times. Prose is safe where it would not be on TOTAL, because fitCostSections drops a
	// section whole and never clips one — the rows and this sentence cannot be separated.
	//
	// IT SAYS THE DOLLARS ARE FINE, and that is the point of putting it here rather than on
	// TOTAL. Cost is settled by a different producer and bounded separately
	// (pricing.MaxPlausibleRequestCostMicros, pricing.MaxCostMicros), so a refused token
	// report does not remove a cent from Totals.CostMicros. A window can carry trustworthy
	// dollars and short tokens at once, and a caveat that let a reader think their money
	// figure was wrong would send them after the wrong number.
	addRefused := func() {
		if r := t.RefusedTokenRequests; r > 0 {
			// Both grammatical numbers come out of one sentence with no verb to agree: "1 token
			// report REFUSED" and "3 token reports REFUSED" both read, where "was/were" would
			// need a second string for a caveat about money that must not read as a typo.
			add(fmt.Sprintf("%s token report%s REFUSED as implausible, counted in none of the "+
				"figures here — so these counts and this split are SHORT by an amount nothing can "+
				"state. The dollar total is unaffected: cost is settled and bounded separately.",
				formatCount(int(r)), plural(int(r))))
		}
	}
	// A tier is shown when the provider DECLARED it or when it carries a count.
	//
	// Declared-and-zero is a real measurement worth a row: "this traffic wrote no
	// cache" is an answer. Undeclared-and-zero is not — it means nothing here reports
	// cache writes, and a 0% row would assert a measurement nobody made. That is
	// exactly the distinction PresentKinds exists for.
	//
	// Counted-but-undeclared still shows, matching cmd_cost.go's tokenSplit: it comes
	// from a producer predating PresentKinds, where the value is the only evidence
	// there is, and dropping it would hide a real number.
	var tiers []costTier
	for _, c := range []costTier{
		{kindInput, "input", t.InputTokens},
		{kindCacheRead, "cache-read", t.CacheReadTokens},
		{kindCacheWrite, "cache-write", t.CacheWriteTokens},
		{kindOutput, "output", t.OutputTokens},
	} {
		if t.PresentKinds&c.bit == 0 && c.count == 0 {
			continue
		}
		tiers = append(tiers, c)
	}
	if len(tiers) == 0 {
		if t.Tokens > 0 {
			// The gateway-reports-a-total case. parsercommon.Fill prefers the provider's own
			// total_tokens when one was reported, so Tokens is non-zero with every split field
			// at zero — a different answer from a split that was genuinely all zeros.
			add(fmt.Sprintf("%s tokens, but no breakdown: this provider reported only a total",
				formatCount(int(t.Tokens))))
		} else {
			add("no tokens recorded in this window")
		}
		// STATED HERE TOO, and this is the path where it matters most: "no tokens recorded in
		// this window" over a refused report is a false negative, not a shortfall. Reports
		// arrived, were rejected, and left the section looking like a quiet window. The sentence
		// above is about the rows that were KEPT; this says some were thrown away.
		addRefused()
		return sec
	}
	// The denominator is the sum of the SHOWN tiers, never Counts.Tokens. Tokens is not
	// always the sum of the split — a gateway reporting only a total yields a non-zero
	// Tokens with every field at zero — so normalising against it would draw shares
	// that do not add up, and would do so exactly on the responses whose split is least
	// trustworthy.
	var whole int64
	for _, c := range tiers {
		whole += c.count
	}
	countW := 0
	for _, c := range tiers {
		if w := lipgloss.Width(formatCount(int(c.count))); w > countW {
			countW = w
		}
	}
	sort.SliceStable(tiers, func(i, j int) bool { return tiers[i].count > tiers[j].count })
	rows := make([]costRow, 0, len(tiers)+1)
	for _, c := range tiers {
		count := formatCount(int(c.count))
		right := strings.Repeat(" ", countW-lipgloss.Width(count)) + count
		row := costRow{label: c.label, right: right}
		if pct := costShare(c.count, whole); pct != "" {
			row.right = right + costColGap + fmt.Sprintf("%6s", pct)
			row.frac = float64(c.count) / float64(whole)
			row.hasBar = true
		}
		rows = append(rows, row)
	}
	// Reasoning is a SUBSET of output, so it gets no share and no bar: adding it to the
	// tiers above would bill every reasoning token twice, at the dearest rate there is.
	// The label says so, for the reader who would otherwise add the column up.
	if t.PresentKinds&kindReasoning != 0 || t.ReasoningTokens != 0 {
		count := formatCount(int(t.ReasoningTokens))
		rows = append(rows, costRow{
			label: "reasoning (part of output)",
			right: strings.Repeat(" ", max(countW-lipgloss.Width(count), 0)) + count,
		})
	}
	// Rows first here too, same rule: the caveat below is about the tiers, so it has
	// nothing to qualify once the label column has collapsed.
	tierLines := renderCostRows(rows, width)
	if len(tierLines) == 0 {
		return costSection{}
	}
	sec.lines = tierLines
	// The shortfall BEFORE the how-to-read note, in severity order: one says the numbers above
	// are incomplete, the other says how to compare them. A reader who stops after the first
	// sentence must have read the one that qualifies the figures.
	addRefused()
	add("Tiers price very differently — a cache read is roughly 0.1x uncached input, a cache " +
		"write roughly 1.25x, and output the dearest — so the largest share is the first place " +
		"to look, not the largest cost. Volume only: no per-tier dollars are aggregated " +
		"anywhere, so a money breakdown here would be invented.")
	return sec
}

// costCoverageSection names the pricing gaps, or renders nothing at all.
//
// Nothing at all is the important half. A fully priced deployment carries NO coverage
// warning: the ratio it would show reaches parity when it should, and a permanent
// warning with nothing to act on is what trains an operator to ignore the one signal
// that matters. That is the same lesson the old Requests-based denominator taught, and
// it is why the gap is measured against PRICEABLE requests here.
//
// A gap has to be nameable, not merely countable. "Cost is incomplete" gives an
// operator nothing to do; "api.openai.com gpt-5 x412" names the rate-table entry to
// add. Ordered largest first, because the entry that buys the most goes first.
func costCoverageSection(snap *usage.Snapshot, width int) costSection {
	priced, priceable := snap.Totals.PricedRequests, snap.Totals.PriceableRequests
	if priceable <= 0 || priced >= priceable {
		return costSection{}
	}
	sec := costSection{heading: "COVERAGE"}
	body := width - lipgloss.Width(costIndent)
	add := func(prose string) {
		for _, l := range wrapCells(prose, body) {
			sec.lines = append(sec.lines, costIndent+l)
		}
	}
	if len(snap.UnpricedBy) == 0 {
		// A ledger-backed window (today, 7d) never carries UnpricedBy: a per-minute row
		// holds only its own labels, so it cannot tell a priced pair from an unpriced one.
		// The gap is still real and still visible in the counters, so it is reported and
		// the absence explained — rendering nothing here would read as "no gaps found",
		// which is a claim the rows do not support.
		// Two calls, not one sentence: wrapCells breaks on word boundaries, and a phrase a
		// reader is scanning for must not be able to straddle two lines.
		add(fmt.Sprintf("%s priceable requests carry no figure.",
			formatCount(int(priceable-priced))))
		add("The pairs are not reported for this window; ask for a duration window such as 1h to name them.")
		return sec
	}
	pairs := make([]string, 0, len(snap.UnpricedBy))
	for k := range snap.UnpricedBy {
		pairs = append(pairs, k)
	}
	sort.Slice(pairs, func(i, j int) bool {
		if snap.UnpricedBy[pairs[i]] != snap.UnpricedBy[pairs[j]] {
			return snap.UnpricedBy[pairs[i]] > snap.UnpricedBy[pairs[j]]
		}
		// Ties break on the key so the list does not reshuffle between refreshes.
		return pairs[i] < pairs[j]
	})
	shown := pairs
	if len(shown) > costMaxGapRows {
		shown = shown[:costMaxGapRows]
	}
	rows := make([]costRow, 0, len(shown))
	for _, k := range shown {
		n := int(snap.UnpricedBy[k])
		rows = append(rows, costRow{
			// sanitizeLabel for the reason costBreakdownSection gives: the model half of the
			// key is workload-chosen and reaches here verbatim. CWE-150.
			label: sanitizeLabel(k),
			right: fmt.Sprintf("x%s", formatCount(n)),
		})
	}
	// The rows before the prose, for the reason costBreakdownSection records: a heading
	// and an "add a rate for each" instruction over no pairs at all is worse than no
	// section, because it names an action against a list the reader cannot see.
	rowLines := renderCostRows(rows, width)
	if len(rowLines) == 0 {
		return costSection{}
	}
	add("unpriced pairs, largest first — add a rate for each:")
	sec.lines = append(sec.lines, rowLines...)
	if rest := len(pairs) - len(shown); rest > 0 {
		add(fmt.Sprintf("+%d more pair%s, each with fewer requests", rest, plural(rest)))
	}
	return sec
}

// renderCostPane draws the pane: a header, then the sections in importance order.
//
// The window in the header comes from snap.Window — what the server ACTUALLY served —
// never from what this pane requested. A proxy with no durable cost ledger answers
// window=today from the ring's maximum span and reports that span here, so echoing
// the request would label six hours of spend as a day's: a wrong number wearing a
// right-looking label, which is worse than no number. It is also why the pane's title
// bar names no window at all.
//
// height is a budget, not a target. Sections are dropped WHOLE from the bottom when
// they do not fit, and the blank separators go first, because a half-rendered section
// misinforms: a truncated COVERAGE reads as a complete list of gaps, and a truncated
// "$12.5" for a $12.5000 total reads as a real, smaller number. No figure is ever
// clipped, at any width or height.
//
// And no figure is ever shown STRIPPED OF ITS QUALIFICATION either, which is a stronger
// promise than "never clipped" and the one the last-resort path below used to break: it
// kept the amount and cut the coverage ratio and the lower-bounds caveat, so a four-row
// terminal published a partial, inexact total as a bare confident number. Whatever survives
// the budget, the figure wears its markers — see costTotalFigure — so the claims outlive
// the words that explain them.
//
// group is the axis to break down by; a zero or GroupNone value falls back to the head
// of costPaneGroups, because this pane's whole purpose is the breakdown and an
// ungrouped view of it is the spend strip one row above.
func renderCostPane(snap *usage.Snapshot, group usage.Group, width, height int) string {
	if group == "" || group == usage.GroupNone {
		group = costPaneGroups[0]
	}
	if snap == nil {
		// No answer yet, and the pane must not imply one. Not an error either — the poll
		// chain fetches on arrival, so this is a frame or two, and the error path renders
		// its own diagnostic.
		return truncCells("COST — waiting for the first answer", width)
	}
	header := truncCells(fmt.Sprintf("COST — %s — by %s", sanitizeLabel(snap.Window), group), width)
	// Importance order. TOTAL first because it is the pane's answer, COVERAGE last
	// because the part of it that must survive truncation — the priced-versus-priceable
	// ratio — already rides on TOTAL.
	sections := []costSection{
		costTotalSection(snap, width),
		costBreakdownSection(snap, group, width),
		costTokenSection(snap, width),
		costCoverageSection(snap, width),
	}
	// An empty section is not a section. costCoverageSection returns one for a fully
	// priced deployment, and a bare heading over nothing would be exactly the permanent
	// warning it declines to render.
	kept := sections[:0]
	for _, s := range sections {
		if s.heading != "" && len(s.lines) > 0 {
			kept = append(kept, s)
		}
	}
	sections = kept
	if lines, ok := fitCostSections(header, sections, width, height); ok {
		return strings.Join(lines, "\n")
	}
	// Not even the header and the first section fit. The "TOTAL" heading is what goes —
	// the header one row above already says COST, so the heading is the only thing here
	// carrying no information — and the figure is kept, because it is the answer to the
	// question the pane exists for.
	//
	// WHOLE UNITS, not whole lines, and the difference is the whole point of this path. It
	// used to cut the section's flat line list at out[:height], and the lines it cut were
	// the coverage ratio and the lower-bounds caveat: at layout()'s own four-row floor the
	// pane kept the dollar amount and threw away everything saying the amount was partial or
	// inexact. The comment here claimed "whole lines only… this path cannot cut it", which
	// was true of the FIGURE and of nothing else — the caveats are wrapped prose, so a cut at
	// a line boundary left half a sentence, and one row shallower left none of it.
	//
	// What makes dropping a caveat safe is that costTotalFigure puts each of its claims on
	// the figure as a one-cell marker, so "~$12.5000+" still says partial and inexact with no
	// words at all. A qualified figure is therefore never shown stripped of its
	// qualification, at any height this can be called with — that is the branch's rule, and
	// it is why the fix is a marker on the figure rather than a cleverer truncation.
	parts := costTotalBody(snap, width)
	out := append([]string{header}, parts.head...)
	if height == 1 {
		// One row to spend: the answer outranks the header, which on its own answers nothing.
		// head[0] and not a cut of it — the figure line is one line by construction, and on
		// the unpriced paths the first line of the prose is the part that says "unavailable".
		return parts.head[0]
	}
	// The HEADER yields before the answer does, for the reason it yields at height 1: the
	// pane's title bar already says COST, so the header's information is the window label,
	// where these lines are the figure and what qualifies it.
	if height > 0 && len(out) > height {
		out = parts.head
	}
	// And if the head alone still overruns — a provenance note that had to wrap onto its own
	// lines, on a terminal two rows tall — its FIRST line survives, because that is the
	// figure. This is the only cut left in this path, it is below layout()'s four-row floor,
	// and it never touches a caveat.
	if height > 0 && len(out) > height {
		out = out[:height]
	}
	for _, c := range parts.caveats {
		if height > 0 && len(out)+len(c) > height {
			// Stop at the first unit that does not fit rather than skipping to a shorter one
			// later in the list: the order is severity order, and a pane that showed the
			// exactness caveat because it was shorter than the damage one would be ranking by
			// length.
			break
		}
		out = append(out, c...)
	}
	return strings.Join(out, "\n")
}

// fitCostSections assembles the widest prefix of sections that fits the height
// budget, preferring more sections over prettier spacing.
//
// The preference order is the specification: all sections with separators, then all
// sections without, then one fewer section with separators, and so on. Separators are
// decoration and go first; a section is information and goes only when it must.
//
// height <= 0 means unbounded, which is what a model that has not laid out yet
// reports. Refusing to render then would leave the pane blank until the first
// WindowSizeMsg.
//
// width is a budget for the HEADINGS, and it is here because nothing else fits them.
// Every body line is emitted through renderCostRows or wrapCells and every figure line
// is width-checked by its own section, but the heading was appended raw — so
// costTierHeading, 35 display cells of it, overflowed at every terminal narrower than
// 35. The existing width tests bottomed out at w=40 and could not see it.
func fitCostSections(header string, sections []costSection, width, height int) ([]string, bool) {
	assemble := func(n int, spaced bool) []string {
		out := []string{header}
		for _, s := range sections[:n] {
			if spaced {
				out = append(out, "")
			}
			heading := s.heading
			// width <= 0 is "unmeasured", the same convention height uses: fit nothing rather
			// than truncate every heading to the empty string.
			if width > 0 {
				heading = truncCells(heading, width)
			}
			out = append(out, heading)
			out = append(out, s.lines...)
		}
		return out
	}
	for n := len(sections); n >= 1; n-- {
		for _, spaced := range []bool{true, false} {
			if out := assemble(n, spaced); height <= 0 || len(out) <= height {
				return out, true
			}
		}
	}
	return nil, false
}

// costTruncated is the last line of a body the height budget had to cut.
//
// It exists so a clipped diagnostic cannot read as a complete one — the same rule the
// section budget holds to, where a truncated COVERAGE reads as a full list of gaps.
const costTruncated = "…(truncated)"

// clampCostLines cuts lines to height, saying on the last line that it did.
//
// height <= 0 is unbounded, matching renderCostPane and fitCostSections: a model that
// has not laid out yet reports zero, and refusing to render then would blank the pane
// until the first WindowSizeMsg.
//
// At height 1 the marker is NOT written. One row buys either the diagnostic's first
// line or an admission that there were more; the first line is the one that says what
// went wrong.
func clampCostLines(lines []string, width, height int) []string {
	if height <= 0 || len(lines) <= height {
		return lines
	}
	out := make([]string, height)
	copy(out, lines[:height])
	if height > 1 {
		out[height-1] = truncCells(costIndent+costTruncated, width)
	}
	return out
}

// renderCostBody is the pane's body: the rendered answer, or why there is none.
//
// The error lives here rather than in renderCostPane because renderCostPane takes
// only the ANSWER — it is a pure function of a snapshot, which is what makes every
// honesty rule in it table-testable without a model. The failure is model state.
//
// A failed poll must SAY so. spend.go records what the alternative costs: a broken,
// absent or unauthorised /v1/usage that renders "" produces no figure, no explanation
// and no diagnostic, on every poll, forever — while the layout goes on reserving the
// space. Rendering nothing and having nothing notice is the failure the whole cost
// surface exists to end.
//
// The error text is sanitized: it can carry a server-authored body, and writing that
// straight to a TTY lets an escape sequence recolour the pane or erase the line it is
// reporting on. CWE-150, the same reason the series labels go through it.
func (m *model) renderCostBody() string {
	if m.costPane.err != nil {
		lines := []string{truncCells("COST — unavailable", m.width)}
		for _, l := range wrapCells(sanitizeLabel(m.costPane.err.Error()), m.width-lipgloss.Width(costIndent)) {
			lines = append(lines, costIndent+l)
		}
		// No figure of any kind on this path. A stale total drawn beside a failed poll
		// presents a number nobody can vouch for as the current one.
		//
		// Capped like every other body path in this pane, which this one was not. The
		// error text is a SERVER-authored body: a 502 HTML page or an
		// upstream-connect-error chain wrapped to 42 lines in a 20-row body, paneView
		// joins without clipping, and the frame then grew past the terminal — footer off
		// the bottom, terminal scrolling. An unbounded diagnostic is not more informative
		// than a bounded one; it just takes the rest of the UI with it.
		return strings.Join(clampCostLines(lines, m.width, m.bodyHeight), "\n")
	}
	if m.costPane.snap == nil {
		// "loading" and "no data" are different answers, exactly as renderUsage
		// distinguishes them: the first self-corrects on the reply already in flight, the
		// second means nothing is coming. renderCostPane has its own nil guard — reached
		// only if a future caller passes a snapshot-less state — but it cannot tell the two
		// apart, because it is a pure function of the answer and this is a fact about the
		// request.
		if m.costPane.loading {
			return truncCells("COST — loading…", m.width)
		}
		return truncCells("COST — no data yet", m.width)
	}
	out := renderCostPane(m.costPane.snap, m.costPane.group, m.width, m.bodyHeight)
	// A freshness line, in space the sections did not want.
	//
	// It earns its place because the pane refreshes on a timer with no other sign of
	// life: a figure carrying no age cannot be told from a frozen one, and a chain that
	// silently died looks exactly like a quiet hour. But it is worth strictly less than
	// any section, so it takes a row only when one is spare — the same priority order the
	// height budget applies to the separators.
	if !m.costPane.lastFetch.IsZero() {
		if n := len(strings.Split(out, "\n")); m.bodyHeight <= 0 || n < m.bodyHeight {
			age := time.Since(m.costPane.lastFetch).Truncate(time.Second)
			out += "\n" + costIndent + truncCells(
				fmt.Sprintf("updated %s ago (every %s)", age, costPollInterval),
				m.width-lipgloss.Width(costIndent))
		}
	}
	return out
}
