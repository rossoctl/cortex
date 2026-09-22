package tui

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// spendPollInterval is the band's FASTEST cadence: spendSpanDefs[spanHour].interval IS this
// constant, and each span's staleness threshold is twice its own interval. Matches the Usage
// pane's cadence, because the band is glanceable chrome,
// not a live meter, and a faster poll would spend requests to move a figure nobody is
// watching move.
//
// EACH SPAN'S OWN CADENCE LIVES IN spendSpanDefs, because they differ by fifteen times and for
// a reason that belongs beside the span rather than in a constant here: the hour is a ring
// read, the month is a walk over up to thirty-one day files. There is no longer a single
// staleness constant for them to contradict — the one that existed was twice THIS interval and
// described only the hour.
const spendPollInterval = 20 * time.Second

// spendFetchTimeout bounds one poll. Generous enough for a ledger walk over a month of day
// files on an operator-configured path — the slowest thing any of these ask for — and short
// enough that a wedged endpoint surfaces as staleness rather than as a stuck chain.
const spendFetchTimeout = 5 * time.Second

// spendResolution asks for a span as a SINGLE bucket. One bucket means the server folds and the
// client does no arithmetic over buckets — a client-side sum would be a second implementation of
// the fold, and the one place cost is settled is the aggregator.
//
// THE REQUESTED-SPAN CONSTANT THAT SAT BESIDE THIS IS GONE. Every span now names its own window in
// spendSpanDefs, so a single "the span we ask for" value described nothing: the four chains ask for
// four different windows. It survived as dead code because staticcheck's unused treats a GROUPED
// const declaration as used when any member of the group is used — verified by splitting it into
// its own declaration, which the linter then reported immediately — so the group was hiding it.
// Hence one declaration per constant here.
//
// Requested is still not covered: what an answer actually spans is snap.Window and nothing may
// assume the two agree. spanReadings compares them per span through servedAsRequested, and a cell
// whose served window is not the one asked for is drawn as unavailable rather than as a figure
// under the wrong label.
const spendResolution = time.Hour

// spendState is the always-on spend summary behind the strip.
//
// Deliberately NOT m.usage. That state is pane-scoped: openUsage sets it, and it
// carries a user-chosen window and an optional single-session scope. The strip
// needs all-sessions data whenever abctl is running, whatever pane is showing —
// driving both from one chain would blank the strip the moment a user scoped the
// Usage pane to one session, which is exactly when they are looking at cost.
//
// THE COST, recorded here so it reads as a decision rather than an oversight. Four chains,
// each on its own cadence (see spendSpanDefs): one ring read every 20s, one day file every
// minute, and two ledger walks every 5 minutes. Averaged out that is roughly five requests a
// minute, against the two the previous two-chain band made — the three ledger-backed spans
// are the ones that touch disk server-side, which is exactly why they are the slow ones.
//
// Plus the drawer's chain while the drawer is open, and the Usage pane's while that pane is.
// All six can be alive at once.
type spendState struct {
	// chains holds one poll chain per budget span, indexed by spendSpan.
	//
	// AN ARRAY RATHER THAN A FIELD GROUP PER SPAN, and the two it replaces are why. The
	// window and today chains were written out longhand — snap, err, lastFetch, reqSeq,
	// tickGen, twice, with each field's doc explaining that it could not be shared. Every
	// one of those reasons is a reason to have N chains rather than two, and none of them
	// is a reason to spell the Nth by hand: four copies of that group would be twenty
	// fields, and adding a fifth span would mean finding all five places that poll.
	//
	// The array also makes the invariants structural. A reply can only be stored through
	// its own span's index, so the "a today reply must never be applied as the window's"
	// rule is no longer a convention the dispatch has to honour — see spendLoadedMsg.
	chains [numSpendSpans]spendChain

	// drawer is the breakdown's OWN chain, and it exists because the band's chains cannot
	// serve it any more.
	//
	// They used to: one poll asked for the drawer's axis and the strip read Totals off the
	// same answer while the drawer read Series. That worked while the band showed exactly
	// one rolling window — the one the drawer was cycling — and it is precisely what made
	// `w` silently change what the band's window cell reported. With the band pinned to
	// four fixed spans the two questions have come apart: the band asks four windows with
	// no grouping, and the drawer asks ONE window folded by an axis.
	//
	// Polled only while the drawer is open, so a reader who never presses `$` pays nothing
	// for it.
	drawer spendChain

	// expanded is the spend drawer's only visibility state: whether the band is showing its
	// breakdown. See spend_drawer.go for why a drawer rather than a pane.
	expanded bool
	// groupIdx indexes spendDrawerAxes; windowStep is an OFFSET from
	// spendDrawerWindowDefault into spendDrawerWindows. Together they are the axis the
	// server folds for and the span it covers.
	//
	// STILL ON THIS STRUCT rather than on a drawer struct of its own, because they are what
	// the drawer's poll asks for and this is where that poll's state lives.
	groupIdx   int
	windowStep int
}

// spendSpan identifies one of the four budget spans the band reports.
//
// FOUR, AND THESE FOUR, because they are the spans a budget is actually read against: the
// live hour (is something running away right now), the day, the week, and the month — which
// is when the budget resets. Anything shorter is a diagnostic rather than a budget, and
// belongs behind the drawer's own span cycle or `abctl cost --window`.
type spendSpan int

const (
	spanHour spendSpan = iota
	spanToday
	span7d
	spanMonth
	numSpendSpans
)

// valid reports whether a span came from this enum. Guarded rather than trusted because
// spendLoadedMsg carries one across a goroutine boundary and an out-of-range index is a panic
// inside the array, not a dropped reply.
func (s spendSpan) valid() bool { return s >= 0 && s < numSpendSpans }

// spendSpanDef is one span's wire window, its label on the band, and how often it is polled.
type spendSpanDef struct {
	// window is the /v1/usage `window` parameter. A STRING, not a duration, because three
	// of the four are symbolic boundaries that time.ParseDuration cannot express — see
	// usage.ParseWindowSpec.
	window string
	// resolution asks the ring for a single bucket. Zero omits the parameter, which is
	// right for every symbolic window: the ledger path serves one bucket regardless and
	// ignores the resolution entirely.
	resolution time.Duration
	// label is the band's column heading, and it NAMES THE SPAN. That is the invariant the
	// band rests on: every cell's label states the period its figure covers, so no figure
	// can sit on the band without saying what it is a figure of.
	label string
	// interval is this span's poll cadence.
	//
	// SLOWER FOR LONGER SPANS, and that is not a compromise. A month-to-date total moves by
	// well under a tenth of a percent in twenty seconds, while answering it costs the server
	// a walk over up to thirty-one day files on an operator-configured path. Polling it at
	// the hour's cadence would spend that walk every twenty seconds to move a figure nobody
	// can see move.
	interval time.Duration
}

// spendSpanDefs is the whole table. Ascending span order, which is also the band's
// left-to-right order: reading outwards from "right now" to "this month".
// pollInterval is this span's cadence with the ZERO CASE CLAMPED, and it exists so the two
// readers of `interval` cannot disagree about it. spendSpanDefs is a keyed array literal, so a
// fifth span added without an interval compiles with zero — and the scheduler and the staleness
// test drew opposite conclusions from that zero: spendTick clamped it and polled at the default
// cadence, while spanReadings compared the age against 2*0 and dated the span on every frame. A
// span that polls fine and reports itself permanently stale is worse than either failure alone,
// because the dated label is the signal an operator is supposed to trust.
//
// TestSpendSpanDefs_EverySpanIsComplete still catches the missing interval at development time.
// This bounds what it costs if one ever reaches a terminal.
func (d spendSpanDef) pollInterval() time.Duration {
	if d.interval <= 0 {
		return spendPollInterval
	}
	return d.interval
}

var spendSpanDefs = [numSpendSpans]spendSpanDef{
	// The only ring-served span, and therefore the only cheap one: no disk, no day files.
	spanHour: {window: "1h", resolution: time.Hour, label: "LAST 1H", interval: spendPollInterval},
	// One day file. Fast enough for a minute's cadence, and the day figure is the one an
	// operator watches most closely after the hour.
	spanToday: {window: usage.WindowToday, label: "TODAY", interval: time.Minute},
	// Up to nine day files (usage.Window7dLocalDays) and up to thirty-one
	// (usage.WindowMonthLocalDays). Both slow-moving, both on the slow cadence.
	span7d:    {window: usage.Window7d, label: "7 DAYS", interval: 5 * time.Minute},
	spanMonth: {window: usage.WindowMonth, label: "MONTH", interval: 5 * time.Minute},
}

// spendChain is one span's poll state: the last answer, whether it failed, when it landed,
// and the two counters that decide which replies and which ticks still belong.
//
// Every field here is one the old two-chain code had to duplicate, each with a doc saying
// why it could not be shared. Those docs are preserved on the fields, because the reasons
// are what make this a per-chain struct rather than a shared one.
type spendChain struct {
	snap      *usage.Snapshot
	err       error
	lastFetch time.Time
	// reqSeq is the id of the most recently ISSUED request on this chain; a reply carrying a
	// different id is stale and dropped. An id rather than a comparison of the request's
	// fields, for the reason usageLoadedMsg.req records: comparing fields means every future
	// request option has to be added to the comparison or it silently stops being covered,
	// while an id cannot be partially right.
	//
	// PER CHAIN, because a shared counter would make every fast reply invalidate the slow
	// request in flight and the slower poll would never land at all.
	reqSeq uint64
	// tickGen identifies the current polling chain. See usageState.tickGen: a quick exit and
	// re-entry left two chains alive, each rescheduling the other's successor and doubling
	// the request rate for the life of the session.
	tickGen uint64
}

// invalidate drops this chain's data and disowns anything in flight. See
// spendState.invalidate for why both counters move rather than only tickGen.
func (c *spendChain) invalidate() {
	c.snap = nil
	c.err = nil
	c.lastFetch = time.Time{}
	c.reqSeq++
	c.tickGen++
}

// invalidate drops the data this state describes and disowns anything in flight.
//
// Called when the pod behind the strip changes. A different pod is a different
// aggregator, so both the figure and any in-flight request belong to the old one.
// Keeping the snapshot draws the previous pod's spend as the new pod's; and NOT
// bumping reqSeq lets an old reply — a 5s timeout, so it easily outlives the
// switch — pass applySpendLoaded's guard and be stored with a FRESH lastFetch,
// presenting a stale number as a current one. Bumping tickGen alone achieves
// neither: it stops the old chain from scheduling, not the reply already in the
// air.
//
// Mirrors the m.usage reset in backToPodsPane, which is the same state shape
// facing the same hazard.
// EVERY CHAIN, AND THE DRAWER'S TOO, by walking the array rather than naming them. Each
// span's figure is the previous pod's just as much as the hour's is, and each reply outlives
// the switch by the same 5s timeout — so a loop is not merely shorter than four hand-written
// blocks, it is what makes a fifth span impossible to forget.
func (s *spendState) invalidate() {
	for i := range s.chains {
		s.chains[i].invalidate()
	}
	s.drawer.invalidate()
}

// spendLoadedMsg carries one span's fetched snapshot back to Update.
//
// ONE TYPE CARRYING ITS SPAN, replacing the pair of near-identical types the two chains
// used. That pair's own doc argued for distinct types — "the compiler enforces what a shared
// type would leave to a field" — and it was right about the hazard while the dispatch was
// hand-written: two switch cases, each naming a chain, and nothing but care stopping the
// today case storing into the window's fields.
//
// FOUR SPANS RETIRE THAT ARGUMENT RATHER THAN MULTIPLYING IT. Eight types would not make the
// dispatch safer, only longer, and the routing is now DATA: applySpendLoaded indexes
// chains[msg.span], so there is exactly one place a reply can be stored and it cannot pick
// the wrong chain — a mis-tagged message would have to be constructed wrong at the fetch,
// where the span is the same variable that chose the window. The compiler's guarantee is
// replaced by a structural one rather than dropped.
//
// span is validated on arrival all the same: it crosses a goroutine boundary, and an
// out-of-range index is a panic inside the array rather than a dropped reply.
type spendLoadedMsg struct {
	span spendSpan
	snap *usage.Snapshot
	req  uint64
	err  error
}

// spendTickMsg fires one span's periodic refetch. gen ties it to the chain that scheduled it,
// so a tick from a superseded chain is ignored; span says which chain to reschedule.
type spendTickMsg struct {
	span spendSpan
	gen  uint64
}

// spendDrawerLoadedMsg and spendDrawerTickMsg are the drawer chain's own pair.
//
// DISTINCT TYPES from the band's, and here the original argument still holds: the drawer asks
// a different QUESTION — one window, folded by an axis — so its reply is not a band span's
// reply with a different tag, and there is no index that could route it. A shared type would
// need a sentinel span meaning "not a band span", which is the shape that invites a reply
// into chains[0].
type spendDrawerLoadedMsg struct {
	snap *usage.Snapshot
	req  uint64
	err  error
}

type spendDrawerTickMsg struct{ gen uint64 }

// spendSummary is what the band renders.
//
// ONE FIELD, and it used to be twenty-seven. The other twenty-six described the hour-and-day
// band that the four budget spans replaced: WindowUSD and TodayUSD and their coverage, inexact
// and clamp twins, plus the savings aggregate, the cache-hit rate and the token and error
// counts. renderSpendBand reads Spans and nothing else, and spendSummary has exactly one
// non-test caller, so every one of them was write-only — computed on every frame and discarded.
//
// THEY WERE DELETED AS A SEPARATE CHANGE from the one that orphaned them, deliberately: the
// four-span band is a behaviour change and this is not, and a thousand lines of deletion inside
// a feature diff hides both. What made the deletion safe rather than hopeful is that the rules
// those fields enforced were re-pinned against spanReadings FIRST — see
// TestSpanReadings_ANegativeTotalIsUnpricedNotARefund and its siblings.
//
// Nothing was lost in the move. The day figure used to be withheld on exactly four conditions —
// no snapshot or a failed poll, a served window that was not "today", an unpriced answer, a
// negative total — and spanReadings carries all four per span, the second of them with a better
// disclosure than before: servedAsRequested names it Unanswerable, so the band draws an em dash
// AND the reason is knowable, where the old path simply published nothing.
type spendSummary struct {
	// Spans is one reading per budget span, indexed by spendSpan.
	Spans [numSpendSpans]spanReading
}

// spendSummary is the whole answer the band draws from.
//
// A ONE-LINE FUNCTION NOW, and it was a wrapper around a hundred-line derivation whose early
// returns described the HOUR chain only — so a failed hour poll returned before the other three
// spans were read, and the wrapper existed to fill them outside it. spanReadings walks every
// chain unconditionally, which is the independence the chains were split for, expressed once
// instead of worked around. Pinned by
// TestSpanReadings_AFailedChainDoesNotBlankTheOthers, which reintroduces that early return and
// fails on it.
func (m *model) spendSummary() spendSummary {
	return spendSummary{Spans: m.spanReadings()}
}

// parseWindowSpan interprets a snapshot's Window string as a duration.
//
// Not every value has to be one. The symbolic windows now exist — usage.WindowToday
// and usage.Window7d are labels no duration parser reads — and while the strip's
// WINDOW chain asks only for fixed lengths, so this succeeds on every label it
// currently sees, the field is free-form on the wire and a server is entitled to
// answer with a span it names rather than measures. Returning false then is what
// lets the caller suppress the burn rate instead of inventing a denominator.
func parseWindowSpan(label string) (time.Duration, bool) {
	d, err := time.ParseDuration(label)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// formatWindowLabel renders a span the way a strip has room for: "1h", not
// "1h0m0s".
//
// time.Duration.String() always emits every non-zero-suffixed unit, so the
// aggregator's own Window field reads "1h0m0s" for a one-hour window — six columns
// where two would do, on a line whose whole design problem is width. No test ever
// saw it because the fixtures in the brief used the tidy form.
func formatWindowLabel(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int64(d/time.Hour))
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", int64(d/time.Minute))
	default:
		return d.String()
	}
}

// spendTickIsCurrent reports whether a tick belongs to the live chain for its span.
func (m *model) spendTickIsCurrent(span spendSpan, gen uint64) bool {
	return span.valid() && gen == m.spend.chains[span].tickGen
}

// applySpendLoaded stores one span's reply unless it is stale.
//
// ROUTED BY msg.span, which is what makes "a reply from one chain must never be applied as
// another's" structural rather than a rule the dispatch has to honour. There is one store
// site and it indexes the span the fetch chose, so a today reply cannot land in the hour's
// fields the way two hand-written switch cases could let it.
//
// lastFetch moves only on an accepted reply: advancing it for a discarded one would have the
// band report the age of data it just threw away.
//
// A failed poll clears snap rather than leaving the previous one in place: continuing to draw
// the last figure would present a stale number as a current one.
//
// THE COST OF THAT RULE ROSE WITH THE FOUR CADENCES, and it is worth stating rather than
// leaving to be rediscovered. The month polls every five minutes, so one dropped reply blanks
// the cell an operator opened abctl to read for five minutes, discarding a figure that was
// good thirty seconds ago. Under the single twenty-second cadence this branch replaced, the
// same rule cost twenty seconds.
//
// KEPT ANYWAY, and not because the reason is unchanged — it is weaker: the band can now say
// "stale, and this old", per span, so a retained figure need not present as current. What
// stops the change being a comment-sized one is the VOCABULARY. A kept-but-failed figure is a
// fifth claim about a number, distinct from inexact, partial and damaged — "this is what it
// was, and we could not check" — and the band's markers, the README that documents them and
// the drop order that pays for label width all take a position on the set. An em dash plus a
// failure is honest today; a figure wearing a claim nothing else on screen uses would not be.
//
// It does NOT render as silence. err is what spendSummary turns into a per-span failure,
// which the band draws as an unavailable cell. The rows are reserved on height alone, so a
// broken endpoint rendering "" would buy a permanent blank line above the footer and no
// diagnostic anywhere on screen.
func (m *model) applySpendLoaded(msg spendLoadedMsg) {
	if !msg.span.valid() {
		return
	}
	c := &m.spend.chains[msg.span]
	if msg.req != c.reqSeq {
		return
	}
	c.snap, c.err, c.lastFetch = msg.snap, msg.err, time.Now()
}

// startSpendPolling begins (or restarts) every span's chain on a clean slate. Fetching
// immediately as well means the band is current on arrival rather than blank for up to the
// slowest interval — five minutes, which for the month figure would be five minutes of empty
// cell on the reading an operator opened abctl to see.
//
// invalidate() rather than a bare tickGen++ so that entering a session view can never inherit
// a figure from a previous one. backToPodsPane already invalidates on the way OUT, which is
// what closes the window while the picker is up; this is the same guarantee on the way IN, so
// any future path that starts a chain gets it without having to remember. The doubled reqSeq++
// (here and in fetchSpendSpan) is harmless — the sequence only has to be monotonic.
//
// A LOOP OVER THE TABLE, so adding a span cannot leave it unpolled.
//
// AND THE DRAWER'S CHAIN TOO, WHEN IT IS OPEN, which it did not used to be. invalidate() walks
// every chain including the drawer's — a different pod is a different breakdown just as much as
// it is a different total — so leaving the drawer out of the restart meant: open the drawer,
// esc to the pods picker, pick another pod, and the drawer came back expanded with snap == nil
// and nothing scheduled to fill it. Headers and blank rows, indefinitely, revivable only by
// closing and reopening.
//
// CONDITIONAL on expanded, so a closed drawer still costs nothing: the chain starts with the
// drawer and this only re-arms one that was already running.
func (m *model) startSpendPolling() tea.Cmd {
	m.spend.invalidate()
	cmds := make([]tea.Cmd, 0, 2*numSpendSpans+2)
	for span := spendSpan(0); span < numSpendSpans; span++ {
		cmds = append(cmds, m.fetchSpendSpan(span), spendTick(span, m.spend.chains[span].tickGen))
	}
	if m.spend.expanded {
		cmds = append(cmds, m.fetchSpendDrawer(), spendDrawerTick(m.spend.drawer.tickGen, m.spend.pollInterval()))
	}
	return tea.Batch(cmds...)
}

// spendTick schedules the next poll for one span, at that span's own cadence.
func spendTick(span spendSpan, gen uint64) tea.Cmd {
	if !span.valid() {
		return nil
	}
	// THE CLAMP IS pollInterval's, shared with the staleness test so the two cannot disagree; see
	// there for why a zero is clamped rather than honoured. What matters HERE is that tea.Tick(0)
	// fires immediately, reschedules at zero, and turns the poll chain into an unbounded stream of
	// /v1/usage requests at the proxy.
	//
	// CLAMPED RATHER THAN DROPPED, because returning nil here would leave the new span silently
	// unpolled — the precise failure startSpendPolling's doc promises cannot happen. A span polled
	// too slowly is a dated figure that says so; a span never polled is a permanent em dash with no
	// explanation.
	d := spendSpanDefs[span].pollInterval()
	return tea.Tick(d, func(time.Time) tea.Msg { return spendTickMsg{span: span, gen: gen} })
}

// fetchSpendSpan requests one span's all-sessions total off the render loop. Returns nil
// before a client exists (picker mode), which keeps the tick chain alive without issuing a
// request — the same shape tickMsg uses.
//
// GetUsageWindow rather than GetUsage, for every span including the hour: GetUsage takes a
// time.Duration and stringifies it, and three of the four windows here are symbolic
// boundaries that a duration cannot express at all. One request-building path for all four
// rather than a duration path and a string path, so the hour cannot drift from the others.
//
// GROUP NONE, unconditionally, which is a change from the single chain this replaces. That
// one asked for the drawer's axis so the drawer could read Series off the same answer; the
// drawer now has its own chain, because the band asks four windows and the drawer asks one
// window folded — see spendState.drawer. Nothing reads a band chain's Series, so asking for a
// label map here would be work paid for and thrown away, four times over.
//
// Session "" is every session, and it cannot be anything else: the ledger's row key carries
// no session dimension by design, and the server rejects session= alongside a symbolic
// window. The band is a per-proxy reading, and making it look otherwise would be a wrong
// number wearing a right label.
func (m *model) fetchSpendSpan(span spendSpan) tea.Cmd {
	if m.client == nil || !span.valid() {
		return nil
	}
	client := m.client
	c := &m.spend.chains[span]
	c.reqSeq++
	req := c.reqSeq
	// Read on the update goroutine and captured, not read inside the closure: the closure
	// runs on bubbletea's command goroutine, where touching m is a data race.
	def := spendSpanDefs[span]
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), spendFetchTimeout)
		defer cancel()
		snap, err := client.GetUsageWindow(ctx, def.window, def.resolution, "", usage.GroupNone)
		return spendLoadedMsg{span: span, snap: snap, req: req, err: err}
	}
}

// spendDrawerTickIsCurrent reports whether a drawer tick belongs to the live drawer chain.
func (m *model) spendDrawerTickIsCurrent(gen uint64) bool { return gen == m.spend.drawer.tickGen }

// applySpendDrawerLoaded stores a drawer reply unless it is stale.
func (m *model) applySpendDrawerLoaded(msg spendDrawerLoadedMsg) {
	if msg.req != m.spend.drawer.reqSeq {
		return
	}
	m.spend.drawer.snap, m.spend.drawer.err, m.spend.drawer.lastFetch = msg.snap, msg.err, time.Now()
}

// spendDrawerTick schedules the next drawer refresh, AT THE CADENCE OF THE SPAN IT IS SHOWING.
//
// Per span rather than one constant, because the drawer's window is one of the band's four and
// the breakdown has to keep up with the figure above it: the hour's cell refreshes every twenty
// seconds, so an hour breakdown on a five-minute cadence disagreed with it for up to five
// minutes. A month keeps the slow cadence — re-folding thirty-one day files to redraw four rows
// is what the slow cadence was chosen for. See spendState.pollInterval.
//
// A keypress refetches immediately — see cycleSpendAxis and cycleSpendWindow — so this only
// governs how stale an UNTOUCHED open drawer gets.
func spendDrawerTick(gen uint64, every time.Duration) tea.Cmd {
	return tea.Tick(every, func(time.Time) tea.Msg {
		return spendDrawerTickMsg{gen: gen}
	})
}

// fetchSpendDrawer requests the breakdown: ONE window, folded by the current axis.
//
// The window comes from the drawer's own cycle and the axis from `a`, and both are captured
// on the update goroutine for the reason fetchSpendSpan records.
func (m *model) fetchSpendDrawer() tea.Cmd {
	if m.client == nil {
		return nil
	}
	client := m.client
	m.spend.drawer.reqSeq++
	req := m.spend.drawer.reqSeq
	// GetUsageWindow, not GetUsage: the drawer's span can now be a symbolic boundary, which a
	// time.Duration cannot express. One request-building path for every span it can be pointed
	// at, so the hour cannot drift from the other three.
	window, resolution, axis := m.spend.window(), m.spend.windowResolution(), m.spend.axis()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), spendFetchTimeout)
		defer cancel()
		snap, err := client.GetUsageWindow(ctx, window, resolution, "", axis)
		return spendDrawerLoadedMsg{snap: snap, req: req, err: err}
	}
}

// spanReading is one band cell's data: the figure, whether it can be shown at all, and every
// caveat that rides on it.
//
// ONE SHAPE FOR ALL FOUR SPANS, which is the point. The summary's Today*/Window* field pairs
// exist because the band once had exactly two figures with different provenance; four spans
// spelled that way would be a field per caveat per span, and the renderer would have to know
// which prefix belonged to which cell. Here the renderer walks an array and asks the same
// questions of every entry.
type spanReading struct {
	// NO LABEL FIELD. It was here, on the theory that a cell and its heading could not then
	// come from different places — and that had it exactly backwards: a reading built anywhere
	// but spanReadings carried an EMPTY label, so the band rendered four nameless columns. The
	// label is a property of the SPAN, not of one poll's answer, so spendSpanDefs is the only
	// place it lives and the renderer indexes it by the span it is drawing.
	USD float64
	// Priced says a figure exists to show. False means "nothing to say", which the band
	// renders as an em dash — never $0.00, which would assert the traffic was free.
	//
	// TRUE WITH A ZERO USD IS A DIFFERENT STATEMENT and is legal: the gateway settled every
	// request in the span at nothing, so "this was free" is the answer rather than a
	// substitute for one. Read as "an em dash unless a figure is known", not as "an em dash
	// unless the figure is non-zero" — the second reading turns a settled-free window into a
	// missing one, and usage.Snapshot.Priced exists in that shape deliberately.
	Priced bool
	// Failed says this span's own poll errored. Per span, because the chains fail
	// independently: an older proxy answers the hour fine and 400s on window=month.
	Failed bool
	// Unanswerable says THIS DEPLOYMENT CANNOT ANSWER THIS SPAN, which is a different thing
	// from a failure or an empty answer and the one a reader most needs told.
	//
	// With no cost ledger — Kubernetes by design — the server serves symbolic windows from the
	// ring instead, clamped to the window asked for, and reports the window it actually
	// served. The ring holds six hours. So "month" comes back as a six-hour figure, and
	// drawing it under a MONTH label would understate the month by a factor of about 120 while
	// looking entirely well-formed. Detected by comparing what was asked for against what was
	// served; rendered as an em dash, because a wrong number wearing a right label is the one
	// thing every money surface here refuses.
	Unanswerable bool
	// DaysOutsideRetention is how many days of this span's window fall before the ledger's
	// retention horizon, so the figure MAY NOT include them. COVERAGE, not damage — see
	// usage.Snapshot.DaysOutsideRetention — and it earns partialMarker rather than
	// damagedMarker: the real total is no smaller than this one, and nothing was destroyed.
	//
	// "MAY NOT", because a ledger nothing has written to keeps day files older than the horizon
	// the clock implies — prune floors its reference day at the newest file — so a day counted
	// here can be in the sum. partialMarker is the right glyph either way: it claims the figure
	// is a FLOOR, which holds whether or not those days are present.
	DaysOutsideRetention            int64
	Unpriced, Priceable, Incomplete int64
	Degraded                        *usage.Degraded
	Clamped                         bool
	// Age and Stale report that this chain's last answer is old enough to say so. PER SPAN
	// rather than one age for the band, because the four poll fifteen times apart: an age that
	// described the whole band would either alarm on a healthy month chain between its own
	// five-minute polls, or stay quiet while the hour chain wedged.
	Age   time.Duration
	Stale bool
}

// servedAsRequested reports whether the server answered the window that was asked for.
//
// NOT A STRING COMPARE, because a duration window comes back stringified: ask for "1h" and the
// answer says "1h0m0s", which is the same span spelled by Go rather than by the caller. Both
// sides are parsed when both parse, and compared as text otherwise — which is what makes the
// symbolic windows strict. "month" does not parse as a duration, so a served "6h0m0s" cannot
// accidentally equal it, and that mismatch is exactly the no-ledger degradation.
func servedAsRequested(want, served string) bool {
	if want == served {
		return true
	}
	wd, wok := parseWindowSpan(want)
	sd, sok := parseWindowSpan(served)
	return wok && sok && wd == sd
}

// spanReadings builds one reading per budget span from the chains.
//
// EVERY PATH FILLS EVERY ENTRY, including the ones with nothing to report, so the band always
// has four cells to lay out and a missing figure is a stated em dash rather than a gap the
// renderer has to guess at.
func (m *model) spanReadings() [numSpendSpans]spanReading {
	now := time.Now()
	var out [numSpendSpans]spanReading
	for span := spendSpan(0); span < numSpendSpans; span++ {
		def := spendSpanDefs[span]
		c := &m.spend.chains[span]
		var r spanReading
		// Staleness is read off the chain whatever the answer was: a wedged chain holding a
		// good old figure is precisely the case worth disclosing.
		//
		// AGAINST THIS SPAN'S OWN CADENCE, not one threshold for the band. The four poll
		// fifteen times apart, so a single figure cannot describe all of them: measured against
		// the hour's forty seconds, the week and the month — which answer every five minutes —
		// were dated for about 87% of every healthy cycle. "MONTH 3m" on a chain that answered
		// three minutes ago and is not due for another two reads as wedged when nothing is
		// wrong, which is how a real signal gets ignored.
		//
		// It cost width as well as credibility. The age rides on the label and the band is one
		// uniform cell width, so two permanently-dated cells pushed the band from 34 columns to
		// 42 and dropped 7 DAYS at a width where all four had fitted.
		//
		// TWICE THE INTERVAL, so one dropped or slow reply is not an alarm and a wedged chain is.
		// Derived per span rather than shared, so a retuned cadence carries its own threshold
		// with it — and there is no named constant left to go stale against it, which is what
		// one shared threshold had become once the four cadences diverged by fifteen times.
		if !c.lastFetch.IsZero() {
			if age := now.Sub(c.lastFetch); age > 2*def.pollInterval() {
				r.Age, r.Stale = age, true
			}
		}
		switch {
		case c.err != nil:
			r.Failed = true
		case c.snap == nil:
			// Nothing has answered yet. Not a failure, and not zero.
		// sanitizeLabel here CANNOT CHANGE THE ANSWER, and saying so is more useful than implying
		// it can: measured against every control-character fixture in spend_sanitize_test.go, this
		// comparison returns the same result sanitised or raw, because a label carrying a control
		// byte neither parses as a duration nor equals the requested string either way. It stays
		// because no wire string should be compared or carried raw — but the band's own cell never
		// prints this label, so the sanitising that MATTERS is at the drawer's caption and its
		// error text, which is where removing the call fails a test.
		case !servedAsRequested(def.window, sanitizeLabel(c.snap.Window)):
			r.Unanswerable = true
		default:
			snap := c.snap
			r.Unpriced, r.Priceable = unpricedGap(snap.Totals)
			r.Incomplete = snap.Totals.IncompleteRequests
			r.DaysOutsideRetention = snap.DaysOutsideRetention
			r.Degraded = snap.Degraded
			r.Clamped = snap.Totals.Saturated
			// A NEGATIVE TOTAL IS REFUSED, not clamped, and not rendered: the aggregator sums
			// non-negative per-request figures, so a negative can only come from a broken
			// producer, and "-$5.00" on a spend band reads as a refund nobody issued. Treated
			// as unpriced, which is the honest reading — we do not know what this span cost.
			//
			// A ZERO TOTAL IS KEPT, and that is not an oversight here. usage.Snapshot.Priced is
			// PricedRequests > 0 and deliberately not CostMicros > 0, because a window whose
			// every request the gateway SETTLED AT ZERO has priced requests and no dollars —
			// snapshot.go requires that to render as a zero figure rather than as "cost
			// unavailable", and the two are different answers. Adding a > 0 test here would
			// withhold a figure we have. The reading that would actually be a lie — a real
			// charge displayed as free — is refused downstream by formatUSDTotalMicros, which
			// is what a band cell formats through and whose floor renders anything positive
			// under half a cent as "<$0.01". (formatUSDCell is the four-decimal path, floored
			// at "<$0.0001"; the events table reads through it, this does not.) See
			// TestRenderSpendBand_UnpricedZeroAndSubCentAreThreeDifferentCells, which pins all
			// three cells.
			if snap.Priced && !negativeCost(snap.Totals.CostMicros) {
				r.USD = float64(snap.Totals.CostMicros) / 1e6
				r.Priced = true
			}
		}
		out[span] = r
	}
	return out
}

// unpricedGap is the coverage gap: how many PRICEABLE requests carry no figure, and the
// denominator that makes it readable.
//
// Priceable rather than Requests, which is the distinction usage.Snapshot.Priced documents:
// Requests counts every proxied response including MCP calls and health checks, none of which
// can ever carry a price, so that denominator makes a correctly configured deployment report
// itself permanently incomplete.
func unpricedGap(c usage.Counts) (unpriced, priceable int64) {
	priceable = c.PriceableRequests
	if gap := priceable - c.PricedRequests; gap > 0 {
		unpriced = gap
	}
	return unpriced, priceable
}
