package tui

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/core/usage"
)

// spendPollInterval is the band's FASTEST cadence — the hour's — and the figure
// spendStaleAfter is derived from. Matches the Usage pane's: the band is glanceable chrome,
// not a live meter, and a faster poll would spend requests to move a figure nobody is
// watching move.
//
// EACH SPAN'S OWN CADENCE LIVES IN spendSpanDefs, because they differ by fifteen times and
// for a reason that belongs beside the span rather than in a constant here: the hour is a
// ring read, the month is a walk over up to thirty-one day files.
const spendPollInterval = 20 * time.Second

// spendFetchTimeout bounds one poll. Generous enough for a ledger walk over a month of day
// files on an operator-configured path — the slowest thing any of these ask for — and short
// enough that a wedged endpoint surfaces as staleness rather than as a stuck chain.
const spendFetchTimeout = 5 * time.Second

// spendStaleAfter is how old the window figure has to be before the strip says so.
//
// Twice the poll interval, so one dropped or slow reply is not an alarm and a wedged
// chain is. Shown only past that threshold, never always: a timestamp beside a healthy
// figure is noise on a line whose entire budget is width, and noise on an always-on
// indicator is how a real signal gets ignored.
//
// The alternative was what the strip did with spendState.lastFetch, which is maintained
// on every accepted reply and asserted by six tests: nothing rendered it. A poll chain
// that stops answering therefore looked exactly like a current reading — no error, no
// staleness, the last good figure sitting there indefinitely.
//
// EVERY CHAIN IS TIMED, and the age reported is the OLDEST. Only the window chain used to be,
// which put the gap on the worst possible figure: the day outranks the hour, polls far more
// slowly, and is among the last things the fitter drops — so the most prominent reading was the
// one that could silently go hours stale while the hour beside it carried a "polled Nm ago".
// One age for the band rather than one per figure, because this qualifies the freshness of the
// whole answer, and taking the oldest is what stops a fresh hour poll vouching for a wedged
// month poll.
//
// DERIVED FROM THE FASTEST CADENCE, which is the hour's. A threshold set against the SLOWEST
// would make a wedged hour chain look healthy for ten minutes; set against the fastest, a
// healthy month chain is briefly "stale" between its own polls — so the renderer reports the
// age rather than an alarm, and the reader sees a figure with a timestamp rather than a
// warning about nothing.
const spendStaleAfter = 2 * spendPollInterval

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
	spanHour: {window: "1h", resolution: time.Hour, label: "LAST 1H", interval: 20 * time.Second},
	// One day file. Fast enough for a minute's cadence, and the day figure is the one an
	// operator watches most closely after the hour.
	spanToday: {window: usage.WindowToday, label: "TODAY", interval: time.Minute},
	// Up to nine day files (usage.Window7dLocalDays) and up to thirty-one
	// (usage.WindowMonthLocalDays). Both slow-moving, both on the slow cadence.
	span7d: {window: usage.Window7d, label: "7 DAYS", interval: 5 * time.Minute},
	// "THIS MONTH", not "MONTH" and emphatically not "MTD". The window runs from the FIRST of the
	// local month, and a bare "MONTH" reads just as easily as a rolling thirty days — the one
	// thing it is not. "THIS" costs five columns and removes that ambiguity.
	//
	// A WORD, NOT AN INITIALISM, which is the choice usage.WindowMonth's own doc already made for
	// the wire token: "mtd" is "less ambiguous read cold, at the cost of a second naming
	// convention on the same small enum". "THIS MONTH" keeps the convention TODAY established —
	// a natural-language boundary phrase — and buys the precision in columns instead.
	//
	// AFFORDABLE ONLY SINCE THE BAND FOLDED. bandCell.width() used to give every surviving cell
	// max(width), so five extra columns here would have widened all four; on one line a cell
	// carries its own width. See TestRenderSpendBand_AWideCellDoesNotWidenTheOthers.
	spanMonth: {window: usage.WindowMonth, label: "THIS MONTH", interval: 5 * time.Minute},
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

// spendSummary is what the strip renders.
//
// Optional fields rather than a narrower struct, because a figure can be genuinely
// unavailable rather than zero. "Today" is now measurable — the durable cost ledger
// supplies it, and applyTodayFigure sets HasToday only when the server actually
// served the "today" window AND priced it. "Saved" is measurable now too, from
// usage.Counts.AvoidedMicros; it was not when this struct was written, and HasSaved is
// still a separate flag rather than a SavedUSD > 0 test, because a deployment without
// tool-prune must show NOTHING there rather than "saved $0.00" — which would assert that
// pruning saved nothing when the truth is that nothing pruned.
type spendSummary struct {
	WindowUSD   float64
	WindowLabel string
	// Tokens and Errors are the window's own counters, shown because they are what a
	// reader checks the money against: a dollar figure with no idea of the volume behind
	// it cannot be judged, and an error count is the cheapest signal that some of that
	// spend bought nothing.
	//
	// NO PER-MINUTE RATE beside them, and its absence is a decision rather than an
	// omission. The window figure is already "$2.91 /1h", so a burn rate derived from the
	// same quotient says the same thing in smaller units — and a rate is a quotient of a
	// total that may itself be partial, so it inherited every caveat on the line while
	// adding no reading of its own.
	Tokens int64
	Errors int64
	// CacheHitPct is cache-read tokens as a percentage of the window's PROMPT tokens, and
	// HasCacheHit reports that the figure means anything at all.
	//
	// It earns a place on a line this narrow because it is the leading indicator of the
	// bill for an agent: a cache read bills at roughly 0.1x uncached input, so for
	// long-running agent traffic the hit rate IS the shape of the spend, and it moves
	// before the dollar figure does.
	//
	// PROMPT tokens as the denominator — input + cache-read + cache-write — never the
	// window's total. Output tokens are generated rather than read, so they cannot be
	// served from a cache and including them would report a ceiling no traffic could reach.
	//
	// HasCacheHit is false when the provider REPORTED no prompt breakdown, which is a
	// different answer from a breakdown that was genuinely zero. usage.Counts.PresentKinds
	// carries that distinction and this flag preserves it: a gateway reporting only
	// total_tokens would otherwise render "cache 0%" for traffic that may be entirely
	// cache reads.
	CacheHitPct float64
	HasCacheHit bool
	// Priced reports whether ANY request in the window produced a cost. False
	// means render "cost unavailable" — never $0.00, which reads as free traffic.
	Priced bool
	// Failed reports that the last poll did not answer at all — a broken, absent or
	// unauthorised /v1/usage.
	//
	// Distinct from Priced == false, which means the endpoint DID answer and
	// nothing in the window carried a cost. Both render "cost unavailable", but
	// only this one can be true with no counters at all, so the renderer must not
	// infer it from Priceable == 0: doing that made a failing endpoint render an
	// empty strip forever, with the row still reserved and nothing saying why.
	Failed bool
	// Unpriced is how many PRICEABLE requests carry no figure, and Priceable is
	// the denominator that makes it readable.
	Unpriced  int64
	Priceable int64
	// Incomplete is how many of the window's PRICED requests carry a figure that is not
	// EXACT: a stream that died before its output count, so the amount is a LOWER BOUND,
	// or a gateway that reported only a total.
	//
	// A SUBSET of the priced requests, never a deduction from them — the dollars are in
	// WindowUSD and belong there. It answers a different question from Unpriced:
	// coverage asks how much of the traffic the figure covers, exactness asks whether
	// the figure it does cover is the real number. Both can be true at once.
	//
	// This branch carries a commit titled "Stop publishing a truncated stream's floor as
	// an exact total", and cmd_cost.go was its only consumer in cmd/abctl: the strip, the
	// sessions table's COST cell and the Usage pane's cost cell all republished exactly
	// that floor as an exact figure. Carried here so the strip can mark it.
	Incomplete int64
	// Clamped reports that an addition into the window's totals reached the int64 ceiling
	// and was CLAMPED rather than allowed to wrap, so WindowUSD — and the counters beside it —
	// are FLOORS by an amount nothing in the response can state.
	//
	// NOT NAMED "Saturated", after the wire field it carries, and the reason is the guard.
	// snapshot_consumers_test.go's Counts half matches a selector by field NAME anywhere in
	// cmd/abctl, because a Counts is read through short-lived locals with no naming convention
	// and there is no base hint to key on. A view-model field spelled Saturated would therefore
	// satisfy the guard for usage.Counts.Saturated ALL BY ITSELF — `s.Saturated` in
	// renderSpendBand is a selector of that name — and the guard would go on passing after
	// every genuine read was deleted. MEASURED, not theorised: with all five real reads removed
	// the guard passed, and renaming this is what makes it fail again. It is the same collision
	// snapshotBaseHint exists to prevent on the Snapshot half, where `cs.Window` on CostSettings
	// would otherwise stand in for Snapshot.Window.
	//
	// A rename costs nothing here because this struct is a VIEW MODEL, not the schema: Incomplete
	// above already drops "Requests", WindowUSD is CostMicros divided out, and Unpriced is a
	// subtraction the wire does not carry. "Clamped" is also the verb the rendering uses
	// (saturatedNote reads "clamped, figures are floors"), so the field is named after what it
	// will say.
	//
	// A FOURTH claim about the window figure, and the one that is neither coverage, exactness
	// nor damage. Coverage says how much of the traffic the figure covers, exactness says
	// whether the figures it covers are the real ones, damage says rows are missing from the
	// sum — and this says the sum itself stopped being able to hold the answer. See
	// usage.Counts.Saturated, whose doc argues that the clamp is only acceptable BECAUSE this
	// flag travels with it.
	//
	// It is on the ROLLING window as well as the day, unlike TodayDegraded, because the flag
	// lives on usage.Counts rather than on the ledger's read: Counts.Add is where the clamp
	// happens, and the in-memory ring sums with the same method. A field carried for the day
	// alone would leave the strip's other reading able to publish a clamped figure bare.
	Clamped bool
	// HasSnapshot reports that a poll actually answered.
	//
	// It is what separates "we looked, and there was no inference traffic" from
	// "we have not looked yet" — identical in every counter, opposite in meaning.
	// The first is a finding worth a row; the second is honest silence that
	// self-corrects on the next poll. Without this the renderer had to guess from
	// Priceable == 0 and got both wrong the same way.
	HasSnapshot bool

	// TodayUSD is spend since local midnight, from the durable cost ledger.
	//
	// HasToday false means NO figure, never zero — see applyTodayFigure for the two
	// ways that happens (the server degraded to a ring window because it has no
	// ledger, or the window priced nothing).
	TodayUSD float64
	HasToday bool

	// TodayUnpriced and TodayPriceable are the TODAY figure's OWN coverage counters,
	// and they are the reason the fields above are not enough.
	//
	// Unpriced/Priceable come from the 1h ring snapshot and describe the HOUR. While
	// the today figure had no counters of its own, the strip's only coverage note was
	// built from those two and rendered at the end of the line — so a ledger day with
	// one priced request out of four hundred printed "$0.0031 today" with no marker at
	// all, and a fully-priced day beside a gappy hour printed "$4.1700 today  $1.1200
	// /1h  40 of 40 unpriced", where the warning reads as qualifying the DAY and
	// describes the HOUR. Both are the same defect: a figure wearing another figure's
	// caveat, or none.
	//
	// Populated on the ledger path — the ledger's Row embeds usage.Counts and Fold sums
	// them — and /v1/usage's own godoc says the ledger leaves MORE requests
	// priceable-but-unpriced than the ring does, which makes "today" the figure MORE
	// likely to be partial and, until now, the only one with no indicator.
	TodayUnpriced  int64
	TodayPriceable int64
	// TodayIncomplete is the day's own exactness counter, separate from Incomplete for
	// the same reason its coverage counters are separate from the window's: it is a
	// different question about a different span, and one figure must never wear another's
	// qualification.
	TodayIncomplete int64
	// TodayDegraded is the ledger's own disclosure that the read behind TodayUSD was
	// INCOMPLETE: rows the day needed could not be read at all, so the figure is SHORT by
	// an amount nothing in the response can state. nil means the read was clean.
	//
	// A THIRD claim about the day's figure, not a variant of the other two, and the one
	// nothing in cmd/abctl consumed. usage.Snapshot.Degraded was added, the server populates
	// it, and it reached no client — so a damaged read still printed a figure
	// byte-identical to a clean one, which is the failure its own doc says it exists to
	// prevent. It matters most HERE: this is the strip's only ledger-backed reading, and the
	// only window that can populate the field at all.
	//
	// Carried as the wire's own POINTER rather than unpacked into counters, so absence keeps
	// meaning "the read was clean" all the way to the renderer. See snapshotDamaged for why
	// presence rather than the counters is the claim.
	//
	// Set only alongside HasToday, so an unpriced or ring-served day leaves it nil: those
	// paths publish no figure, so there is no total for it to qualify. A damaged read of a
	// day that priced nothing is therefore disclosed by the Cost pane and `abctl cost` and
	// not by the strip — the strip has no reading to attach it to, and an unattached caveat
	// on this line is the misattribution moneyFigure exists to end.
	TodayDegraded *usage.Degraded
	// TodayClamped is the day figure's own clamp disclosure, separate from Clamped for the
	// reason every other Today* counter is separate from its window twin: it is a different
	// question about a different span, and one figure must never wear another's qualification.
	TodayClamped bool

	// SavedUSD is the WINDOW's avoided spend, and it is the FALLBACK reading — see
	// TodaySavedUSD, which outranks it whenever the day has a figure of its own.
	SavedUSD float64
	HasSaved bool

	// TodaySavedUSD is the DAY's avoided spend, and HasTodaySaved reports that it exists.
	//
	// A TWIN of SavedUSD rather than a span discriminator on one field, which is the shape
	// every other pair on this struct already has: TodayUnpriced/Unpriced,
	// TodayIncomplete/Incomplete, TodayClamped/Clamped. Their common reason applies here
	// unchanged — it is a different question about a different span, and one figure must never
	// wear another's qualification.
	//
	// IT EXISTS BECAUSE THE STRIP RENDERS THE SAVING AS THE HEADLINE'S PARTNER: renderSpendBand
	// puts it directly after the today figure on the stated grounds that "the pair is the
	// reading: what it cost and what it would have cost". The saving was read from the 1h ring,
	// so the pair spanned two windows and only one of them was labelled — measured on a local
	// proxy, "$64.1765 today  saved ~$1.0291" beside a day that had really avoided $2.1891,
	// understating the figure next to it by 2.1x.
	//
	// FREE TO CARRY: usage.Counts.AvoidedMicros is on the same Totals applyTodayFigure already
	// reads CostMicros from, so this is a field off a reply in hand rather than a second request.
	//
	// Set only alongside HasToday, so an unpriced or ring-served day leaves it unset. That is the
	// rule TodayDegraded states for itself: those paths publish no cost figure, so there is
	// nothing for a saving to be the partner of, and an unpartnered saving on this line is read
	// as the window's.
	TodaySavedUSD float64
	HasTodaySaved bool

	// Age is how long ago the window figure was fetched, and Stale reports that it is
	// old enough to be worth saying — see spendStaleAfter. Age is only meaningful when
	// Stale is set; a fresh figure reports neither, because the band must not carry a
	// permanent timestamp.
	//
	// THE BAND READS Spans[i].Age INSTEAD, per span and against that span's own cadence.
	//
	// AND NOTHING READS THESE TWO. That is not a caveat, it is a defect this struct is full of:
	// spendSummary has ONE non-test caller (paneView -> renderSpendBand) and that renderer reads
	// only Spans, so every other field here is write-only in production. An earlier revision of
	// this comment claimed "the summary's other consumers still read them" — there are no other
	// consumers, and the claim was simply false.
	//
	// It is the same defect the per-span readings were introduced to fix. Age and Stale were
	// computed and rendered nowhere, so a wedged chain looked current; now they are computed,
	// rendered nowhere, AND duplicated by Spans. The fields below them are worse, because the
	// features they describe (the savings aggregate, the cache-hit rate, the token and error
	// counts) left the band with the four-span change and have no surface at all.
	//
	// DELETING THEM IS A SEPARATE CHANGE, deliberately, and tracked rather than done here: it
	// removes spendHourAndDaySummary, applyTodayFigure, applyAges and cacheHitPct along with
	// roughly thirty of this package's tests, which is a thousand-line no-behaviour-change diff
	// on top of a feature PR. What is NOT deferred is saying so accurately.
	//
	// AND spendStaleAfter GOES WITH THEM, which an earlier version of this list left out.
	// applyAges is its only reader, so the constant is dead by the same route as the fields it
	// fills: the band derives its own threshold per span from that span's cadence — see
	// spendSpanDef.pollInterval — because one shared threshold cannot describe four cadences
	// that differ by fifteen times.
	Age   time.Duration
	Stale bool

	// Spans is what the band renders: one reading per budget span, indexed by spendSpan.
	//
	// THE ONLY FIELD WITH A READER. renderSpendBand takes this and nothing else, so a figure is
	// on the band if and only if it is here. The Window*/Today* fields above are dead — see
	// Age/Stale for why they are still present and what removing them costs.
	Spans [numSpendSpans]spanReading
}

// spendSummary derives the strip's figures from the last snapshot.
// spendSummary is the whole answer: the per-span readings the band draws, plus the hour and
// day views the summary's older consumers read.
//
// A WRAPPER, so Spans is filled OUTSIDE every branch of the hour-and-day derivation below.
// That derivation returns early for a failed hour poll and for a snapshot that has not
// arrived, and both describe the HOUR chain only — the month chain can be perfectly healthy
// while the hour is erroring. Filling the readings here is what stops the band losing three
// spans because a fourth failed, which is the independence the chains were split for.
func (m *model) spendSummary() spendSummary {
	out := m.spendHourAndDaySummary()
	out.Spans = m.spanReadings()
	return out
}

func (m *model) spendHourAndDaySummary() spendSummary {
	// An errored poll is an UNKNOWN cost, and the strip must SAY so. Reporting it
	// as the zero value made the renderer fall through to its "nothing to say"
	// path, so a broken or absent /v1/usage produced no figure, no explanation and
	// no diagnostic on every poll forever — while layout() went on reserving the
	// row, leaving a permanent blank line above the footer. Rendering nothing and
	// having nothing notice is the exact failure this whole strip exists to end.
	if m.spend.chains[spanHour].err != nil {
		// Failed describes the WINDOW poll ONLY, and the today figure is carried through it.
		//
		// That is the invariant spendState.todaySnap gives as the reason for splitting the two
		// chains: "the two can fail independently — an older proxy answers the window fine and
		// 400s on window=today — and one broken figure must not blank the other." It held in one
		// direction and not the other, because the renderer returned on Failed before reading
		// any figure, so a wedged window poll discarded a perfectly good day total. The Failed
		// branch yields to the figures now, which is what that fix was waiting on.
		out := spendSummary{Failed: true}
		m.applyTodayFigure(&out)
		m.applyAges(&out)
		return out
	}
	snap := m.spend.chains[spanHour].snap
	if snap == nil {
		out := spendSummary{}
		m.applyTodayFigure(&out)
		return out
	}
	out := spendSummary{
		// sanitizeLabel, because snap.Window is server-supplied JSON that reaches the
		// terminal verbatim whenever parseWindowSpan below cannot read it as a duration.
		// Sanitised at the boundary rather than guarded at each use: the band's whole width
		// guarantee is expressed in rune counts and lipgloss.Width, and
		// lipgloss.Width("abc\nabcdef") is 6 — it measures the WIDEST LINE. So a label
		// carrying a newline passes the budget check and then renders as an EXTRA line.
		// layout() reserves exactly spendBandLines rows and paneView pads to them, so a
		// third line is not merely untidy: the view comes out taller than the terminal and
		// the footer goes off the bottom. Control characters and DEL become U+FFFD (one
		// column, so the arithmetic still holds) rather than being dropped, so tampering
		// shows.
		WindowLabel: sanitizeLabel(snap.Window),
		Priced:      snap.Priced,
		Priceable:   snap.Totals.PriceableRequests,
		// Carried whether or not the window is priced: a snapshot cannot report an inexact
		// figure without reporting a priced one, but reading it unconditionally means the
		// renderer decides what to do with it in one place rather than two.
		Incomplete: snap.Totals.IncompleteRequests,
		// Read unconditionally for the same reason, and NOT gated on Priced: a clamp says every
		// counter in this Counts is a floor, and Requests overflowing is enough on its own —
		// there need be no dollars involved for the answer to have stopped fitting.
		Clamped:     snap.Totals.Saturated,
		HasSnapshot: true,
	}
	// A negative total is refused HERE, before anything derives a figure from it, which
	// is what makes one guard cover the amount and the burn rate at once. See
	// negativeCost: the guarantee is upstream in core/sessionapi and this is defence
	// in depth. Reported as UNPRICED rather than clamped, so the strip renders "cost
	// unavailable" — the same treatment the Cost pane already chose, because "$-5.0000
	// /1h" on the strip reads as a refund nobody issued.
	if negativeCost(snap.Totals.CostMicros) {
		out.Priced = false
	}
	// Priceable minus priced, NOT requests minus priced. Requests counts every
	// proxied response — MCP calls, health checks — while only inference can ever
	// be priced, so the wrong denominator left a correctly configured deployment
	// reading a permanent warning with nothing to act on.
	if gap := snap.Totals.PriceableRequests - snap.Totals.PricedRequests; gap > 0 {
		out.Unpriced = gap
	}
	// The label comes from the span the SNAPSHOT reports, never from the window that was
	// REQUESTED. The request and the answer are not the same promise — labelling the answer with
	// the request is a wrong number wearing a right-looking label, which is worse than no number.
	//
	// It also fixes a mismatch that is live today rather than hypothetical: the aggregator
	// sets Window from time.Duration.String(), so a one-hour request comes back as
	// "1h0m0s". Parsing it lets the label render as "1h".
	//
	// This used to feed a per-minute burn rate as well, which is gone: the window figure is
	// already "$2.91 /1h" and a rate off the same quotient said the same thing in smaller
	// units. The span is still needed for the label.
	span, spanOK := parseWindowSpan(snap.Window)
	if spanOK {
		out.WindowLabel = formatWindowLabel(span)
	}
	m.applyAges(&out)
	m.applyTodayFigure(&out)
	// out.Priced, not snap.Priced: the negative-total refusal above lives in out, and
	// reading the wire flag here would hand the renderer a figure the summary has
	// already declined to publish.
	if out.Priced {
		out.WindowUSD = float64(snap.Totals.CostMicros) / 1e6
	}
	// The volume figures are read OUTSIDE the Priced guard: tokens, errors and the cache
	// ratio are counted for traffic nothing could price, and suppressing them alongside
	// the money would blank the only readings a deployment with no rate table has.
	out.Tokens = snap.Totals.Tokens
	out.Errors = snap.Totals.Errors
	out.CacheHitPct, out.HasCacheHit = cacheHitPct(snap.Totals)
	// Savings, likewise outside it, and for the reason usage.Counts.AvoidedMicros gives:
	// a saving is counted whether or not the response could be priced. A window that
	// priced nothing and pruned something reports "cost unavailable" beside a real saved
	// figure, and both are true.
	//
	// > 0 rather than != 0: the aggregate is a sum of non-negative figures, so a negative
	// here means a broken producer, and the same defence-in-depth negativeCost applies to
	// spend applies to this. A zero leaves HasSaved false, which is what makes the strip
	// silent on a deployment that does not prune.
	if av := snap.Totals.AvoidedMicros; av > 0 {
		out.SavedUSD = float64(av) / 1e6
		out.HasSaved = true
	}
	return out
}

// cacheHitPct is cache-read tokens over PROMPT tokens, as a percentage.
//
// ok is false when the provider reported no prompt breakdown at all, which is why this
// consults PresentKinds rather than testing the denominator: prompt == 0 has two readings
// — "no prompt tokens" and "nothing reported them" — and only the flags can tell them
// apart. See usage.Counts.PresentKinds, whose own doc is about exactly this ambiguity.
//
// TWO BITS ARE REQUIRED, NOT THREE. KindCacheRead alone would divide by a denominator nobody
// reported; KindInput alone would report 0% for a gateway that reports input and not cache
// reads, which is a claim about caching made from an absence of evidence. Those are the two
// terms that must be proven.
//
// THE CACHE-WRITE TERM IS ADDED WITHOUT PROOF, and that is safe for a specific reason rather
// than by indifference: the two parsers in this repo omit the bit exactly when the prompt has no
// cache-write tokens in it, so adding zero is not an approximation, it is the right answer.
//
//   - The OpenAI-compatible path NEVER sets it, because OpenAI bills cache writes as ordinary
//     input (inferenceparser.toNeutral says so). It reports Input = prompt_tokens − cached and
//     CacheRead = cached, so input + cacheRead already IS prompt_tokens exactly.
//   - The Anthropic path sets it whenever cache_creation_input_tokens is on the wire, so an
//     absent bit there means the response reported no cache creation.
//
// REQUIRING IT WAS A REGRESSION, and a total one: with three bits demanded, HasCacheHit was
// structurally false for every OpenAI-dialect response and the cache figure never rendered on
// that path at all — over arithmetic that was already correct. It was tightened to close the
// opposite hole, a producer whose unreported cache writes shrink the denominator and inflate the
// ratio. That hole is real but no producer here has it, and suppressing a whole parser's figure
// to guard against a producer that does not exist is the worse trade. A third parser that
// reports cache writes and forgets the bit would read high; this comment is the warning.
func cacheHitPct(t usage.Counts) (float64, bool) {
	const promptKinds = usage.KindInput | usage.KindCacheRead
	if t.PresentKinds&promptKinds != promptKinds {
		return 0, false
	}
	prompt := t.InputTokens + t.CacheReadTokens + t.CacheWriteTokens
	// 0/0 is NaN, and "cache NaN%" is the one output worse than no figure. Reachable: a
	// response can report the kinds and then carry zero counters.
	if prompt <= 0 {
		return 0, false
	}
	return float64(t.CacheReadTokens) / float64(prompt) * 100, true
}

// applyAges sets the one staleness reading on the line, from the OLDER of the two poll chains.
//
// Read from the clock here rather than recorded on a snapshot because staleness is a property of
// NOW, not of the reply: a figure fetched once and rendered for ten minutes gets older every
// frame.
//
// THE OLDER OF THE TWO, which is the whole point — see spendStaleAfter. A chain that has never
// answered is skipped rather than treated as infinitely old: today is unavailable on a proxy
// with no ledger, and reporting that absence as staleness would put a permanent age on every
// Kubernetes deployment's strip.
func (m *model) applyAges(out *spendSummary) {
	var oldest time.Duration
	for _, at := range []time.Time{m.spend.chains[spanHour].lastFetch, m.spend.chains[spanToday].lastFetch} {
		if at.IsZero() {
			continue
		}
		// NEGATIVE AGES DISCARDED. time.Since goes negative if the wall clock steps backwards
		// between the fetch and this read — an NTP correction is the realistic cause — and
		// formatSpendAge would render that as "polled -5s ago". Cosmetic and unlikely, and a
		// nonsense figure on an always-on line is the kind a reader stops trusting the rest of.
		if age := time.Since(at); age > oldest && age > 0 {
			oldest = age
		}
	}
	if oldest > spendStaleAfter {
		out.Age, out.Stale = oldest, true
	}
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

// applyTodayFigure fills in the strip's "today" headline from the ledger-backed
// poll, or leaves it unset.
//
// Two conditions, and both are load-bearing:
//
// The response's window must actually BE "today". A proxy with no durable cost
// ledger — Kubernetes by design, or a local install with it turned off — answers
// window=today from the in-memory ring's maximum span and reports that span as the
// window it served. Setting HasToday from the request rather than from the answer
// would label a six-hour total as a day's, which is a wrong number wearing a right
// label. This is the one case where the honest answer is to show less: the strip
// falls back to its rolling-window figure, which is correctly labelled.
//
// And the answer must be PRICED. renderSpendBand guards its window figure on
// Priced but renders the today figure whenever HasToday is set, so an unpriced day
// admitted here would print "$0.0000 today" — a settled zero for a cost nobody
// knows, the one thing the strip is forbidden to do. Guarded here rather than there
// because the renderer already handles both fields and this commit is data-only.
//
// The day's own COVERAGE counters come out with the figure, and they are not optional
// decoration. This read CostMicros and threw PriceableRequests away, so a day the
// ledger could price one request of four hundred rendered as a complete total: the
// headline figure of this whole branch, published as exact, with the only gap marker on
// screen built from a different window's numbers. See spendSummary.TodayUnpriced.
func (m *model) applyTodayFigure(out *spendSummary) {
	snap := m.spend.chains[spanToday].snap
	if snap == nil || m.spend.chains[spanToday].err != nil {
		return
	}
	if snap.Window != usage.WindowToday {
		return
	}
	if !snap.Priced {
		return
	}
	// And it must not be NEGATIVE. Declined by leaving HasToday unset, which is this
	// field's own spelling of "no figure" and the same treatment the window figure and the
	// Cost pane give an impossible number — the strip then falls back to its rolling
	// figure rather than printing "$-5.0000 today". See negativeCost: the guarantee is
	// upstream in core/sessionapi, and this is defence in depth.
	if negativeCost(snap.Totals.CostMicros) {
		return
	}
	out.TodayUSD = float64(snap.Totals.CostMicros) / 1e6
	out.HasToday = true
	// Priceable minus priced, for the reason the window figure's gap is computed that
	// way: Requests counts traffic that could never carry a price, so it never reaches
	// parity and would leave a correct deployment reading a permanent warning.
	out.TodayPriceable = snap.Totals.PriceableRequests
	if gap := snap.Totals.PriceableRequests - snap.Totals.PricedRequests; gap > 0 {
		out.TodayUnpriced = gap
	}
	// And the day's exactness, which is a different claim from its coverage: a day can be
	// fully covered and still be a floor, because one truncated stream is enough.
	out.TodayIncomplete = snap.Totals.IncompleteRequests
	// And the ledger's own damage disclosure, which is a THIRD claim: coverage says how much
	// of the traffic the figure covers, exactness says whether the figure it covers is the
	// real number, and this says rows are missing from the sum entirely. A day can be fully
	// covered, wholly exact, and still short — a skipped line is spend that happened and is
	// not in the total.
	//
	// Copied as the pointer, so nil keeps meaning "the read was clean" rather than becoming
	// zeros the renderer has to interpret. This is the one field on this whole path that
	// only a ledger-backed window can populate, which is why it hangs off the today figure
	// and off nothing else. See spendSummary.TodayDegraded.
	out.TodayDegraded = snap.Degraded
	// And the day's own clamp, which is a FOURTH claim and the only one of the four that says
	// the arithmetic itself ran out of room. A day can be fully covered, wholly exact, read
	// cleanly, and still be a floor — see usage.Counts.Saturated.
	out.TodayClamped = snap.Totals.Saturated
	// And the day's SAVING, which is not a claim about the figure above it but a second figure —
	// the one the strip renders as its partner. Read here rather than in spendSummary's window
	// block because it has to come off THIS reply: the window block reads the ring, and the ring
	// is an hour.
	//
	// INSIDE every guard above, which is what keeps the pair honest in both directions. A day
	// that published no cost must publish no saving either, or the saving renders next to the
	// WINDOW's figure and reads as the window's.
	//
	// > 0 rather than != 0, and for the reason the window's own read gives: the aggregate is a sum
	// of non-negative figures, so a negative one means a broken producer, and a zero must leave
	// the flag unset so a deployment that prunes nothing says nothing rather than "saved $0.00".
	if av := snap.Totals.AvoidedMicros; av > 0 {
		out.TodaySavedUSD = float64(av) / 1e6
		out.HasTodaySaved = true
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
		// TWICE THE INTERVAL, which is what spendStaleAfter means for the hour: one dropped or
		// slow reply is not an alarm and a wedged chain is. Derived per span rather than shared,
		// so a retuned cadence carries its own threshold with it.
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
