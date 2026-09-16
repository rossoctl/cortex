package tui

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// spendPollInterval is how often the strip refreshes. Matches the Usage pane's
// cadence: the strip is glanceable chrome, not a live meter, and a faster poll
// would spend requests to move a figure the user is not watching.
const spendPollInterval = 20 * time.Second

// spendTodayPollInterval is how often the "today" headline refreshes.
//
// Fifteen times slower than the window poll on purpose: "today" is a figure that
// moves slowly by construction — it only ever grows, and by the size of one turn —
// so a 20s cadence would spend a request every 20 seconds to move a number a user
// reads once a session. It is also the more expensive of the two answers, since the
// server reads day files off disk for it rather than summing an in-memory ring.
const spendTodayPollInterval = 5 * time.Minute

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
// staleness, the last good figure sitting there indefinitely. Note that only the WINDOW
// chain has a timestamp; the today chain's own age is not tracked, so a wedged today poll
// is still indistinguishable from a fresh one.
const spendStaleAfter = 2 * spendPollInterval

// spendWindow is the span the strip REQUESTS, and spendResolution asks for it as
// a SINGLE bucket. One bucket means the server folds and the client does no
// arithmetic over buckets — a client-side sum would be a second implementation of
// the fold, and the one place cost is settled is the aggregator.
//
// Requested, not covered: what the answer actually spans is snap.Window, and
// nothing may assume the two agree. Deriving the burn rate from this constant
// instead of from the snapshot is a real defect (a rate over a span the label
// contradicts), so the divisor is parsed from the answer — see spendSummary.
const (
	spendWindow     = time.Hour
	spendResolution = time.Hour
)

// spendState is the always-on spend summary behind the strip.
//
// Deliberately NOT m.usage. That state is pane-scoped: openUsage sets it, and it
// carries a user-chosen window and an optional single-session scope. The strip
// needs all-sessions data whenever abctl is running, whatever pane is showing —
// driving both from one chain would blank the strip the moment a user scoped the
// Usage pane to one session, which is exactly when they are looking at cost.
//
// The cost is one extra GET /v1/usage per 20s asking for a single bucket, plus one
// per 5 MINUTES for the ledger-backed "today" figure on its own chain. Together with
// the Usage pane's chain that is three, and all three can be alive at once —
// recorded here so it reads as a decision rather than an oversight. The today poll is
// the only one that touches disk server-side, which is why it is the slow one.
type spendState struct {
	snap      *usage.Snapshot
	err       error
	lastFetch time.Time

	// todaySnap is the ledger-backed "today" answer, on its OWN chain with its own
	// counters. Two chains against one endpoint, and a reply from one must never be
	// applied as the other's: they ask different questions, so a today reply stored
	// as the window snapshot would label a day's spend with the window's label and
	// drive the burn rate off it.
	//
	// A separate error too, because the two can fail independently — an older proxy
	// answers the window fine and 400s on window=today — and one broken figure must
	// not blank the other.
	todaySnap *usage.Snapshot
	todayErr  error

	// reqSeq is the id of the most recently ISSUED request; a reply carrying a
	// different id is stale and dropped. An id rather than a comparison of the
	// request's fields, for the reason usageLoadedMsg.req records: comparing
	// fields means every future request option has to be added to the comparison
	// or it silently stops being covered, while an id cannot be partially right.
	reqSeq uint64
	// tickGen identifies the current polling chain. See usageState.tickGen: a
	// quick exit and re-entry left two chains alive, each rescheduling the other's
	// successor and doubling the request rate for the life of the session.
	tickGen uint64

	// todayReqSeq and todayTickGen are the today chain's own counters, for the reason
	// the snapshot is its own field: sharing reqSeq with the window chain would make
	// every window reply invalidate the today request in flight, so the slower poll
	// would never land at all.
	todayReqSeq  uint64
	todayTickGen uint64
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
func (s *spendState) invalidate() {
	s.snap = nil
	s.err = nil
	s.lastFetch = time.Time{}
	s.reqSeq++
	s.tickGen++
	// The today figure is a different pod's day just as much as the window figure is
	// its hour, and its reply outlives the switch by the same 5s timeout. Both
	// counters move for the reason the window's do: bumping tickGen alone stops the
	// old chain from scheduling, not the reply already in the air.
	s.todaySnap = nil
	s.todayErr = nil
	s.todayReqSeq++
	s.todayTickGen++
}

// spendLoadedMsg carries a fetched snapshot back to Update.
type spendLoadedMsg struct {
	snap *usage.Snapshot
	req  uint64
	err  error
}

// spendTickMsg fires the periodic refetch. gen ties it to the chain that
// scheduled it, so a tick from a superseded chain is ignored.
type spendTickMsg struct{ gen uint64 }

// spendTodayLoadedMsg carries the ledger-backed "today" snapshot back to Update.
// A distinct type from spendLoadedMsg so the two replies cannot be confused by the
// dispatch switch — the compiler enforces what a shared type would leave to a field.
type spendTodayLoadedMsg struct {
	snap *usage.Snapshot
	req  uint64
	err  error
}

// spendTodayTickMsg fires the today refetch, on its own generation.
type spendTodayTickMsg struct{ gen uint64 }

// spendSummary is what the strip renders.
//
// Optional fields rather than a narrower struct, because a figure can be genuinely
// unavailable rather than zero. "Today" is now measurable — the durable cost ledger
// supplies it, and applyTodayFigure sets HasToday only when the server actually
// served the "today" window AND priced it. "Saved" still is not: it needs tool-prune
// attribution aggregated across requests, so HasSaved stays false and the strip shows
// nothing for it rather than a zero, because "saved $0.00" asserts that pruning saved
// nothing when the truth is that nothing measures it yet.
type spendSummary struct {
	WindowUSD   float64
	WindowLabel string
	// BurnPerMin is 0 when it cannot be derived. Zero means "not enough data",
	// never "free".
	BurnPerMin float64
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
	// renderSpendStrip is a selector of that name — and the guard would go on passing after
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

	SavedUSD float64 // set once tool-prune savings are aggregated
	HasSaved bool

	// Age is how long ago the window figure was fetched, and Stale reports that it is
	// old enough to be worth saying — see spendStaleAfter. Age is only meaningful when
	// Stale is set; a fresh figure reports neither, because the strip must not carry a
	// permanent timestamp.
	Age   time.Duration
	Stale bool
}

// spendSummary derives the strip's figures from the last snapshot.
func (m *model) spendSummary() spendSummary {
	// An errored poll is an UNKNOWN cost, and the strip must SAY so. Reporting it
	// as the zero value made the renderer fall through to its "nothing to say"
	// path, so a broken or absent /v1/usage produced no figure, no explanation and
	// no diagnostic on every poll forever — while layout() went on reserving the
	// row, leaving a permanent blank line above the footer. Rendering nothing and
	// having nothing notice is the exact failure this whole strip exists to end.
	if m.spend.err != nil {
		// Failed describes the WINDOW poll. The today figure is deliberately NOT carried
		// here even when its own chain answered: renderSpendStrip returns on Failed
		// before it reads any figure, so setting one would be an assignment nothing
		// reads. Showing a good day total beside a failed window poll would be the
		// better strip, but it is a renderer change — the Failed branch would have to
		// yield to the figures — and this commit touches the data side only.
		return spendSummary{Failed: true}
	}
	snap := m.spend.snap
	if snap == nil {
		out := spendSummary{}
		m.applyTodayFigure(&out)
		return out
	}
	out := spendSummary{
		// sanitizeLabel, because snap.Window is server-supplied JSON that reaches the
		// terminal verbatim whenever parseWindowSpan below cannot read it as a duration.
		// Sanitised at the boundary rather than guarded at each use: the strip's whole
		// width guarantee is expressed in lipgloss.Width, and lipgloss.Width("abc\nabcdef")
		// is 6 — it measures the WIDEST LINE. So a label carrying a newline passes
		// fitStripFigures' budget check and renderSpendStrip returns a TWO-LINE string,
		// which costs a row of the table below it: the precise failure the strip's own
		// godoc opens with. Control characters and DEL become U+FFFD (one column, so the
		// arithmetic still holds) rather than being dropped, so tampering shows.
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
	// negativeCost: the guarantee is upstream in authlib/sessionapi and this is defence
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
	// The divisor comes from the span the SNAPSHOT reports, never from the
	// spendWindow constant. spendWindow is what we ASKED for; snap.Window is what
	// the server answered with, and the two are not the same promise. Dividing the
	// answer's cost by the request's span renders a "/min" rate over a period the
	// strip's own label contradicts — a wrong number wearing a right-looking label,
	// which is worse than no number.
	//
	// It also fixes a mismatch that is live today rather than hypothetical: the
	// aggregator sets Window from time.Duration.String(), so a one-hour request
	// comes back as "1h0m0s". Deriving the span lets the label be rendered from the
	// parsed duration instead of echoing that.
	span, spanOK := parseWindowSpan(snap.Window)
	if spanOK {
		out.WindowLabel = formatWindowLabel(span)
	}
	// How old this answer is. Read from the clock here rather than recorded on the
	// snapshot because staleness is a property of NOW, not of the reply: a figure fetched
	// once and rendered for ten minutes gets older every frame.
	if !m.spend.lastFetch.IsZero() {
		if age := time.Since(m.spend.lastFetch); age > spendStaleAfter {
			out.Age, out.Stale = age, true
		}
	}
	m.applyTodayFigure(&out)
	// out.Priced, not snap.Priced: the negative-total refusal above lives in out, and
	// reading the wire flag here would hand the renderer a figure the summary has
	// already declined to publish.
	if out.Priced {
		out.WindowUSD = float64(snap.Totals.CostMicros) / 1e6
		// Suppressed, not approximated, when the span is unknown. A rate is a
		// quotient: without a trustworthy denominator there is no honest figure to
		// show, and BurnPerMin == 0 already means "cannot be derived" everywhere
		// else. Substituting the requested window here is exactly the bug above.
		if spanOK {
			if mins := span.Minutes(); mins > 0 {
				out.BurnPerMin = out.WindowUSD / mins
			}
		}
	}
	return out
}

// sessionCost returns what one session cost over the strip's window.
//
// Reads the strip's own snapshot rather than issuing anything: fetchSpend asks for
// group=session on the all-sessions ring, so the poll that feeds the strip already
// carries a Series entry per session. A per-row fetch would be one request per
// visible row on a 20s cadence.
//
// priced=false means NO FIGURE, and the caller must render it as blank rather than
// as $0.00 — a table cell has even less room to explain itself than the strip does.
// Three distinct states collapse into it, all of them "unknown" and none of them
// "free": no snapshot yet, a window that priced nothing at all, and a session this
// window has no priced request for. The last is the one worth naming, because
// Snapshot.Priced is WINDOW-wide: a snapshot that priced another session's traffic
// says Priced == true, so trusting that flag alone would render $0.0000 against a
// session whose cost is simply unknown.
//
// inexact is the third answer, and it is about the figure rather than about whether
// there is one: true means at least one of this session's priced requests carries a
// figure that is not exact (usage.Counts.IncompleteRequests), so usd is a LOWER BOUND.
// Returned rather than folded into priced because the dollars are real and belong on
// screen — only the claim of exactness is withdrawn. The cell marks it; see
// fitCostCell.
func (m *model) sessionCost(id string) (usd float64, priced, inexact bool) {
	if m.spend.snap == nil || !m.spend.snap.Priced || id == "" {
		return 0, false, false
	}
	var micros int64
	var pricedReqs, incompleteReqs int64
	// Every bucket, not Buckets[0]. The strip asks for a single-bucket resolution
	// but the server negotiates it (see Snapshot.BucketSeconds), so reading the
	// first bucket would report the first slice of the window as the whole of it.
	for _, b := range m.spend.snap.Buckets {
		c, ok := b.Series[id]
		if !ok {
			continue
		}
		micros += c.CostMicros
		pricedReqs += c.PricedRequests
		incompleteReqs += c.IncompleteRequests
	}
	// A session whose summed figure is negative is UNPRICED, not cheap. The COST cell
	// renders priced=false as blank, which already means "nobody knows what this cost" in
	// this table — the honest answer for an impossible number, and the same treatment
	// costTotalSection chose. Without it the cell printed "$-5.0000", a credit nobody
	// issued, in a column a reader scans for the expensive row.
	//
	// Summed rather than per-bucket, because that is the figure the cell publishes: a
	// positive bucket and a negative one can cancel to something plausible, and it is the
	// PUBLISHED total that has to be refusable. See negativeCost — the guarantee is
	// upstream and this is defence in depth.
	if pricedReqs == 0 || negativeCost(micros) {
		return 0, false, false
	}
	return float64(micros) / 1e6, true, incompleteReqs > 0
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

// spendTickIsCurrent reports whether a tick belongs to the live chain.
func (m *model) spendTickIsCurrent(gen uint64) bool { return gen == m.spend.tickGen }

// applySpendLoaded stores a reply unless it is stale.
//
// lastFetch moves only on an accepted reply: advancing it for a discarded one
// would have the strip report the age of data it just threw away.
//
// A failed poll clears snap rather than leaving the previous one in place: once
// the fetch failed we do not know the current spend, and continuing to draw the
// last figure would present a stale number as a current one.
//
// It does NOT render as silence. err is what spendSummary turns into
// spendSummary.Failed, which the strip draws as "cost unavailable". The row is
// reserved on height alone, so a broken endpoint rendering "" would buy a
// permanent blank line above the footer and no diagnostic anywhere on screen.
func (m *model) applySpendLoaded(msg spendLoadedMsg) {
	if msg.req != m.spend.reqSeq {
		return
	}
	m.spend.snap, m.spend.err, m.spend.lastFetch = msg.snap, msg.err, time.Now()
}

// startSpendPolling begins (or restarts) the chain on a clean slate. Fetching
// immediately as well means the strip is current on arrival rather than blank for
// up to spendPollInterval.
//
// invalidate() rather than a bare tickGen++ so that entering a session view can
// never inherit a figure from a previous one. backToPodsPane already invalidates
// on the way OUT, which is what closes the window while the picker is up; this is
// the same guarantee on the way IN, so any future path that starts a chain gets it
// without having to remember. The doubled reqSeq++ (here and in fetchSpend) is
// harmless — the sequence only has to be monotonic.
func (m *model) startSpendPolling() tea.Cmd {
	m.spend.invalidate()
	// Both chains, each on its own generation. The today figure is fetched
	// immediately too rather than waiting out its 5-minute interval: the whole point
	// of the headline is that it is there when the user arrives.
	return tea.Batch(
		m.fetchSpend(), spendTick(m.spend.tickGen),
		m.fetchSpendToday(), spendTodayTick(m.spend.todayTickGen),
	)
}

// spendTick schedules the next poll for the given generation.
func spendTick(gen uint64) tea.Cmd {
	return tea.Tick(spendPollInterval, func(time.Time) tea.Msg { return spendTickMsg{gen: gen} })
}

// fetchSpend requests the all-sessions single-bucket snapshot off the render
// loop. Returns nil before a client exists (picker mode), which keeps the tick
// chain alive without issuing a request — the same shape tickMsg uses.
func (m *model) fetchSpend() tea.Cmd {
	if m.client == nil {
		return nil
	}
	client := m.client
	m.spend.reqSeq++
	req := m.spend.reqSeq
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Session "" is every session, and group=session asks for the per-session
		// breakdown ON that all-sessions ring — which is what makes ONE poll serve
		// both consumers: the strip reads Totals, the sessions table reads one
		// Series entry per row. The alternative is a request per row.
		//
		// Totals is unaffected by grouping (Snapshot sums it from the raw buckets
		// before folding), so the strip's figures are byte-identical to what
		// group=none returned. This asked for group=none until the sessions table
		// existed, on the sound-at-the-time grounds that a breakdown nothing renders
		// is a label map paid for and thrown away.
		snap, err := client.GetUsage(ctx, spendWindow, spendResolution, "", usage.GroupSession)
		return spendLoadedMsg{snap: snap, req: req, err: err}
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
// And the answer must be PRICED. renderSpendStrip guards its window figure on
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
	snap := m.spend.todaySnap
	if snap == nil || m.spend.todayErr != nil {
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
	// upstream in authlib/sessionapi, and this is defence in depth.
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
}

// spendTodayTickIsCurrent reports whether a today tick belongs to the live chain.
func (m *model) spendTodayTickIsCurrent(gen uint64) bool { return gen == m.spend.todayTickGen }

// applySpendTodayLoaded stores a today reply unless it is stale.
//
// Does not move lastFetch: that field reports the age of the WINDOW figure, and
// advancing it on a today reply would have the strip claim a freshness the rolling
// figure does not have.
func (m *model) applySpendTodayLoaded(msg spendTodayLoadedMsg) {
	if msg.req != m.spend.todayReqSeq {
		return
	}
	m.spend.todaySnap, m.spend.todayErr = msg.snap, msg.err
}

// spendTodayTick schedules the next today poll for the given generation.
func spendTodayTick(gen uint64) tea.Cmd {
	return tea.Tick(spendTodayPollInterval, func(time.Time) tea.Msg {
		return spendTodayTickMsg{gen: gen}
	})
}

// fetchSpendToday requests the ledger-backed day total off the render loop.
//
// GetUsageWindow rather than GetUsage: GetUsage takes a time.Duration and
// stringifies it, and "today" is a boundary rather than a length, so it cannot be
// expressed that way at all.
//
// group=none, unlike fetchSpend: the strip needs one number from this poll and the
// sessions table reads the window snapshot's series, so asking for a breakdown here
// would be a label map paid for and thrown away. Resolution 0 omits the parameter —
// the ledger serves this as a single bucket and does not read it.
func (m *model) fetchSpendToday() tea.Cmd {
	if m.client == nil {
		return nil
	}
	client := m.client
	m.spend.todayReqSeq++
	req := m.spend.todayReqSeq
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		snap, err := client.GetUsageWindow(ctx, usage.WindowToday, 0, "", usage.GroupNone)
		return spendTodayLoadedMsg{snap: snap, req: req, err: err}
	}
}
