package usage

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// Group selects which label breakdown a Snapshot carries.
type Group string

const (
	GroupNone Group = "none"
	// GroupModel breaks totals down by the model named on the request.
	//
	// This is the series that shipped as GroupMethod. The aggregator only ever
	// populated it from Inference.Model (see foldInto) — A2A and MCP method names
	// never entered it — so "method" was a misnomer from the start. abctl's
	// events-table METHOD cell DOES show A2A/MCP methods, which is what made the
	// name look right; that is a different code path.
	GroupModel Group = "model"
	// GroupMethod is the name GroupModel shipped under. Accepted forever, resolves
	// to the same series.
	//
	// Kept because it is on the wire and tui/usage_pane.go's cycleGroup passes it:
	// dropping it would break a live client to fix a spelling. NOT a second map —
	// series() maps both to byMethod.
	GroupMethod Group = "method"
	// GroupEndpoint breaks totals down by target host.
	//
	// Half of the rate key. The same model bills differently on a discounted
	// gateway than on the vendor endpoint, and only the host distinguishes them, so
	// a cost table without this axis cannot explain why two identical-looking
	// requests cost different amounts.
	GroupEndpoint Group = "endpoint"
	// GroupSession breaks totals down by session.
	//
	// Requested on the ALL-sessions ring (session=""), which is the point: one
	// response then answers for every session a client is listing. Asked for
	// alongside session=<id> it is legal but degenerate — a single-entry series
	// duplicating Totals.
	//
	// What one key means depends on how the listener assigns session ids. While
	// several concurrent agents share one id (#949), their spend lands in one entry
	// and this axis reports per-id rather than per-agent cost. That is a property of
	// the current session bucketing, not of this grouping.
	GroupSession Group = "session"
	// GroupAgent breaks totals down by the coding agent that made the request, as
	// "name/version" — the axis #952 asks for, since "what did today cost" is only
	// actionable once it says which agent spent it. It is also what GroupSession
	// cannot answer while several concurrent agents share one session id (#949).
	//
	// The key is parsed from the request's User-Agent, so it is CLIENT-ASSERTED AND
	// TRIVIALLY SPOOFABLE: a display and cost-attribution axis, never an authorization
	// input. A caller that lies here mis-attributes its own spend and nothing else.
	//
	// Its accumulator has NO INFERENCE GUARD, so this series counts MCP and tool
	// traffic too and its request denominator differs from GroupModel's — the same
	// property GroupEndpoint has. See the byAgent field for the full reasoning.
	//
	// An event that carried no User-Agent lands under the reserved key "unknown",
	// which is NOT an agent name; an agent that was reported but is not recognised
	// lands under its raw User-Agent, so a new coding agent is visible the day someone
	// runs it rather than after a parser update ships.
	GroupAgent  Group = "agent"
	GroupStatus Group = "status"
	GroupPlugin Group = "plugin"
)

// ParseGroup validates a group parameter. Empty means GroupNone.
func ParseGroup(s string) (Group, error) {
	switch Group(s) {
	case "", GroupNone:
		return GroupNone, nil
	case GroupModel:
		return GroupModel, nil
	// Returned as itself rather than normalised to GroupModel: Snapshot echoes the
	// requested group back on the wire, and rewriting a caller's parameter into a
	// name it did not ask for would make the response look like it answered a
	// different question.
	case GroupMethod:
		return GroupMethod, nil
	case GroupEndpoint:
		return GroupEndpoint, nil
	case GroupSession:
		return GroupSession, nil
	case GroupAgent:
		return GroupAgent, nil
	case GroupStatus:
		return GroupStatus, nil
	case GroupPlugin:
		return GroupPlugin, nil
	}
	// Deliberately does NOT echo the caller's value: this message is returned
	// over an unauthenticated endpoint, and reflecting arbitrary query input
	// into a response body is how a reflected-content issue starts. The valid
	// set is short enough that naming it is more useful than quoting the input.
	return "", errors.New("unknown group (want none, model, endpoint, session, agent, status, plugin; method is accepted as an alias for model)")
}

// Snapshot is the wire shape of GET /v1/usage.
type Snapshot struct {
	// Window is the span this snapshot ACTUALLY covers, e.g. "10m", "6h0m0s" or
	// "today". Not necessarily the span requested.
	//
	// It used to be only the requested one, and the difference now matters: the
	// symbolic windows ("today", "7d") are served from the durable cost ledger, and a
	// proxy with no ledger — Kubernetes by design — answers them from the ring's
	// maximum window instead and reports THAT here. A client must read this field
	// rather than echo its own request, or it will label six hours of spend as a
	// day's.
	Window string `json:"window"`
	// BucketSeconds is the resolution of the buckets actually returned, which for a
	// ring-backed window is the requested resolution rounded to a whole multiple of
	// BucketWidth. A client reads this rather than assuming: asking for a resolution
	// the storage cannot divide evenly gets the nearest one that works, not an error.
	//
	// For a LEDGER-backed window it is the whole window's span, because that response
	// is one bucket rather than a series — the ledger answers "what did today cost",
	// and synthesising per-minute buckets from disk for a 7-day span would read
	// millions of rows to draw a chart nothing asks for. Reading this field is how a
	// client tells the two apart.
	BucketSeconds int `json:"bucketSeconds"`
	// Session is the session this covers, or "" for all sessions combined.
	Session string `json:"session,omitempty"`
	Group   Group  `json:"group"`
	// Buckets runs oldest to newest. For a ring-backed window it always has
	// Window/BucketWidth entries, including zeroed ones for idle minutes, so a client
	// can distinguish an idle minute from one that fell off the end of the ring.
	//
	// A LEDGER-backed window (see BucketSeconds) returns exactly ONE bucket spanning
	// the request. A client dividing len(Buckets) into the window to recover a
	// resolution must read BucketSeconds instead.
	Buckets []Bucket `json:"buckets"`
	// Totals sums every bucket, so a client need not re-add them to render a
	// summary line.
	Totals Counts `json:"totals"`
	// Priced reports whether ANY request in this window produced a cost. False
	// means CostMicros is absent everywhere, and a client must say "cost
	// unavailable" rather than display $0.00 — which would read as "this traffic
	// was free".
	//
	// True does NOT mean every request was priced. Compare Totals.PricedRequests
	// against Totals.PriceableRequests: cost comes from a plugin that may not be in
	// the pipeline for all traffic, and once rates are per-endpoint a deployment can
	// price some endpoints and not others. Where those differ the dollar total
	// covers only the priced subset, and a client showing it must say so.
	//
	// PRICEABLE, not Requests. Requests counts every proxied response — MCP tool
	// calls, health checks, tunnels — none of which can ever carry a price, so that
	// denominator makes a correctly configured deployment report itself incomplete
	// forever. See Counts.PriceableRequests, which exists for this.
	//
	// Nor does it mean the total is EXACT. Totals.IncompleteRequests counts priced
	// requests whose figure is a lower bound (a stream truncated before its output
	// count) or an approximation (a gateway reporting only a total). While that is
	// non-zero the dollar total is inexact, and a client rendering it as an exact figure
	// is making a claim the data does not support. It is a SUBSET of PricedRequests, not
	// a deduction from it — the coverage question and the exactness question are
	// separate, and this field answers neither on its own.
	Priced bool `json:"priced"`
	// UnpricedBy counts the requests that could NOT be priced, keyed
	// "<endpoint> <model>". Present only when something was unpriced — and never
	// present at all on a ledger-backed window, where a per-minute row carries only
	// its own labels and cannot distinguish the unpriced pairs from the priced ones.
	// Absence is therefore not a claim that there were no gaps; compare
	// Totals.PricedRequests with Totals.PriceableRequests for that.
	//
	// A gap has to be nameable, not just countable. "Cost is incomplete" gives an
	// operator nothing to act on; "api.openai.com gpt-5: 412" names the pricing
	// entry to add. Traffic carrying no model is excluded — naming every non-LLM
	// call the proxy handled would bury the real gaps.
	//
	// Summed across the window from the raw buckets, so it is unaffected by the
	// requested resolution, exactly like Totals.
	UnpricedBy map[string]int64 `json:"unpricedBy,omitempty"`
	// PricedBy counts priced requests by the provenance of their figure —
	// "authoritative" when the gateway reported it, otherwise the rate table's level
	// ("configured", "discovered", "bundled").
	//
	// Without it a total is unqualified, and the spec's own success criterion asks
	// for cost "labelled with provenance": $12.40 assembled from a gateway's own
	// numbers and $12.40 modelled from a shipped vendor-list table are not equally
	// trustworthy figures, and nothing else in the response distinguishes them.
	//
	// Summed from the raw buckets alongside Totals, so it is unaffected by the
	// requested resolution.
	PricedBy map[string]int64 `json:"pricedBy,omitempty"`
	// IncompleteBy counts the priced requests whose figure is INEXACT, keyed by WHICH WAY
	// it is inexact — "output-uncounted" for a lower bound (a stream that died before its
	// output count, so the true figure is HIGHER), "split-unreported" for an approximation
	// (a gateway reporting only a total, so it is off in no known direction), or
	// "unlabelled" for a caveat a producer disclosed without naming its kind. See
	// pricing.ReasonOutputUncounted and pricing.ReasonSplitUnreported.
	//
	// Totals.IncompleteRequests answers HOW MANY; this answers IN WHICH WAY, and the two
	// are different claims about money. "At least $12.40" and "roughly $12.40" cannot be
	// rendered the same way: one is a bound that will be exceeded and is usually a
	// transient failure worth chasing, the other a standing property of a gateway that
	// holds for every request it answers. A client that can see only the count has to
	// present them identically, which is how a permanent caveat comes to read as an
	// incident.
	//
	// Its counts SUM to Totals.IncompleteRequests. An inexact figure whose producer named
	// no reason is counted under "unlabelled" rather than omitted, so a client subtracting
	// the map from the counter gets zero and cannot mistake a dropped row for a request
	// whose figure is exact.
	//
	// Present only when something was inexact — and never present at all on a
	// LEDGER-backed window, where a per-minute row carries IncompleteRequests but not the
	// reason: the reason is no part of that row's (endpoint, model, agent, provenance) key,
	// so a persisted row cannot say which way its inexact figures were inexact. Absence is
	// therefore not a claim that every figure is exact; Totals.IncompleteRequests answers
	// that, and both window kinds populate it.
	//
	// Summed across the window from the raw buckets, so it is unaffected by the requested
	// resolution, exactly like Totals and the two maps above.
	IncompleteBy map[string]int64 `json:"incompleteBy,omitempty"`
	// UngroupedCostMicros is the part of Totals.CostMicros that NO entry in this
	// response's series carries — the amount by which summing the breakdown falls short
	// of the total printed beside it.
	//
	// It exists because that shortfall was undisclosed. The dominant instance is a
	// GATEWAY-PRICED RESPONSE THE INFERENCE PARSER COULD NOT READ: /v1/embeddings and
	// /v1/rerank are not among the paths it parses, so the event carries no model while the
	// gateway's own header settles a cost, and that spend counts toward Totals and drops
	// out of group=model. Per-axis that is the honest answer — there is no model to
	// attribute it to, and moving it into a made-up bucket or out of the totals would each
	// be worse. In aggregate it made the response contradict itself with nothing to
	// explain the difference, which is the same shape as the group=plugin dollar
	// duplication and the endpoint-versus-model denominator, both of which this API says
	// out loud.
	//
	// WHAT A CLIENT DOES WITH IT: render it as its own residual band — "unattributed", or
	// the (other) band a table already collapses into — and never present the sum of a
	// series as the window's total. The arithmetic it restores is exact:
	// sum(series CostMicros) + UngroupedCostMicros == Totals.CostMicros, for every group
	// where Group.Reconcilable is true AND the source answering could group by it — read
	// the Group this response reports, not the one that was requested, since a
	// ledger-backed window reports GroupNone for an axis its rows have no column for. See
	// Group.Reconcilable for why one predicate was not enough.
	//
	// BOTH WINDOW KINDS POPULATE IT, verified rather than assumed — which is why this is
	// not ledger-only the way Degraded is:
	//   - a LEDGER window, because such a response is stored as a row with Model "" and
	//     costledger.labelFor then returns ok=false for group=model. Pinned by
	//     TestFold_GatewayPricedRowWithNoModelIsDisclosedAsUngrouped.
	//   - a RING window, because Aggregator.costOf prices any SETTLED cost record whether
	//     or not the event carries an Inference extension, while foldInto guards byMethod
	//     on a non-empty model — so the same spend lands in the bucket total and in no
	//     series entry. Pinned by
	//     TestSnapshot_GatewayPricedTrafficWithNoModelIsDisclosedAsUngrouped.
	// A field that appeared on one kind and not the other would be worse than none: a
	// client would come to trust the reconciliation and then have it break at whichever
	// window boundary switches storage.
	//
	// A POINTER, absent rather than zero on a window whose series accounts for
	// everything — the Degraded convention, for the reason its doc gives. A zero would
	// have to carry two meanings: "the breakdown accounts for every dollar" and "this
	// group offers no reconciliation at all". group=none asks for no series, and
	// group=plugin's series counts one request once per plugin, so neither sets this; a
	// client must read Group before concluding anything from its absence. See
	// Group.Reconcilable.
	//
	// COST, not a request count, and not keyed like UnpricedBy. The question it answers is
	// arithmetic — a breakdown that does not add up to the total above it — and requests
	// cannot be subtracted from dollars. A key would have to come from a DIFFERENT axis
	// than the one the client grouped by (the endpoint, for a row with no model), and the
	// ring cannot produce that key without a label map it does not keep, so the two window
	// kinds would then disagree about the shape as well as the number.
	//
	// Summed from the raw buckets alongside Totals, so it is unaffected by the requested
	// resolution.
	UngroupedCostMicros *int64 `json:"ungroupedCostMicros,omitempty"`
	// SeriesOvershootMicros is the residual with the WRONG SIGN: the amount by which this
	// response's series summed to MORE than Totals.CostMicros.
	//
	// It is not a property of the traffic. Every event lands in at most one entry of a
	// reconcilable group's map, so the entries can only sum to the total or to less than it —
	// a negative residual means this process is wrong about its own arithmetic. There are
	// exactly two ways: a Group was marked Reconcilable when its series double-counts (which
	// is why GroupPlugin is excluded), or a series accumulator counts one event twice.
	//
	// IT USED TO BE DISCARDED. SetUngroupedCost took `micros <= 0` as "nothing to disclose"
	// and returned, which collapsed "the breakdown accounts for everything" together with
	// "the breakdown accounts for more than everything" — and the second is a defect report
	// this package threw away on the floor. That matters more now, not less: reconcilability
	// has become a property of the SOURCE as well as the group (see Group.Reconcilable), so a
	// disagreement between the two predicates is a live possibility, and this residual going
	// negative is exactly how it would show.
	//
	// A SEPARATE FIELD rather than a negative UngroupedCostMicros, because the two are
	// different claims and a client acts on them differently. UngroupedCostMicros is a
	// residual band to render — real spend with no key on this axis. This is "do not trust
	// the breakdown in this response, and file a bug", and no chart should draw it. Signing
	// one field would have every consumer branch on the sign to tell a normal response from a
	// broken one, and the ones that forgot would render a negative band.
	//
	// ABSENT unless it happened, on the same rule as UngroupedCostMicros and Degraded: it
	// should never appear in a healthy deployment, and a zero would have to mean both "checked
	// and fine" and "not checked".
	SeriesOvershootMicros *int64 `json:"seriesOvershootMicros,omitempty"`
	// Degraded reports that this answer is known to be MISSING ROWS, and is absent
	// whenever it is not.
	//
	// A DIFFERENT CLAIM from Totals.IncompleteRequests, and the two must not be merged.
	// That counter says a figure this response CARRIES is inexact — a floor, or an
	// approximation — and the request is still counted and still priced. This field says
	// rows the answer needed could not be read at all, so the total is short by an
	// amount nothing here can state. One qualifies a number; the other says a number is
	// missing from the sum.
	//
	// A POINTER, so a clean read serialises nothing rather than zeros a client has to
	// interpret. Absence means the read was clean; presence means look at it. Zeros in
	// an always-present object would read as "checked, fine" from a producer that never
	// checked, which is the same class of false reassurance as a $0.00 over unpriced
	// traffic.
	//
	// Only a ledger-backed window can populate it. The ring is memory: it has no lines
	// to fail to decode and no files to abandon, so a duration window leaves this nil
	// and that absence is the truth rather than a gap in the reporting.
	Degraded *Degraded `json:"degraded,omitempty"`
}

// Degraded is what a ledger read could not deliver.
//
// It exists because the alternative was silence. The ledger already skipped a corrupt
// line rather than discarding the rest of the day, and already abandoned a file it
// could not scan — both strictly better than the behaviour they replaced. But the
// response looked IDENTICAL to a clean one: a short dollar total labelled priced:true
// with no caveat anywhere in it, which is the exact failure this branch keeps refusing
// in every other place it appears.
//
// The counters are GAUGES for the most recent read, not cumulative totals. One corrupt
// line re-read on every /v1/usage poll would make a running count climb forever over a
// single piece of damage, and a client watching it would infer an outage that is not
// happening.
type Degraded struct {
	// SkippedLines is how many LINES could not be decoded and were passed over. Each is
	// spend that happened and is not in the total.
	//
	// A FLOOR ON ROWS LOST, NOT AN EXACT COUNT OF THEM, and this doc used to read as
	// though it were exact. It counts lines because that is what the reader can see. One
	// undecodable line is usually one row — but the ledger's newline fence only runs while
	// the writing process is alive, so a crash mid-append leaves an unterminated fragment
	// and the next append is concatenated onto it: ONE line holding TWO lost rows.
	// MEASURED that way — two rows missing from the answer while the count said one.
	//
	// So a non-zero value means "AT LEAST this many rows are missing", which is the reading
	// a client has to present. Same number and same qualification as
	// costledger.Caveats.SkippedLines, which is where this field is copied from; see
	// costledger's appendBytes for why the bytes on disk cannot support an exact figure.
	SkippedLines int64 `json:"skippedLines,omitempty"`
	// TruncatedDays is how many day files were abandoned part-way. Worse than a skipped
	// line by an unbounded amount: the rest of that file is missing, and a file holds a
	// whole day.
	TruncatedDays int64 `json:"truncatedDays,omitempty"`
}

// Reconcilable reports whether a client can reconcile this group's series against
// Totals — whether sum(series CostMicros) + Snapshot.UngroupedCostMicros equals
// Totals.CostMicros.
//
// FALSE FOR GroupNone, which asks for no breakdown: there is nothing to reconcile, and
// a residual equal to the entire total would then appear on every group-less request —
// the default one — and read as a fault.
//
// FALSE FOR GroupPlugin, whose series counts one request once per plugin that touched
// it, so its dollars intentionally sum to MORE than the total (see the note on
// GroupPlugin's accumulator). Totals-minus-series is not a residual for that axis at
// all, and it is not merely useless: in a window where some requests touched no plugin
// and others touched several, the shortfall and the duplication cancel and produce a
// plausible zero. So it is not computed there rather than computed and clamped.
//
// Exported because both producers — Aggregator.Snapshot and the ledger's fold — have to
// make the same call, and a predicate written twice is a predicate that drifts.
//
// NOT SUFFICIENT ON ITS OWN, and a caller that treated it as such published a residual
// equal to an entire total. This says whether a group's series WOULD reconcile against
// Totals; it cannot say whether the source being read can produce that series at all.
// The two are different questions, and the second one belongs to the source: the ring
// keeps a status series, a plugin series and per-session rings, while a cost-ledger row
// is (endpoint, model, agent, provenance) per minute and has no column for status or
// session. costledger.Groupable answers it there, and costledger.Fold requires BOTH —
// because "the breakdown falls short by this much" and "there is no breakdown to fall
// short" are different answers, and only the first is a residual.
//
// So: reconcilability is a property of the SOURCE AND the group. This half is the
// group's, and a true here is a necessary condition rather than a licence.
func (g Group) Reconcilable() bool {
	switch g {
	case GroupNone, GroupPlugin:
		return false
	}
	return true
}

// SetUngroupedCost records the residual, whichever way it went, and leaves both fields
// ABSENT when there is none.
//
// The single implementation of the absent-not-zero rule, called by the ring in Snapshot
// and by the ledger in sessionapi, so neither can serialise a zero that would read as
// "checked, complete" from a path that computed nothing. See
// Snapshot.UngroupedCostMicros.
//
// A NEGATIVE RESIDUAL IS A DEFECT REPORT AND USED TO BE SWALLOWED HERE. The guard was
// `micros <= 0`, which treated "the series accounts for every dollar" and "the series
// accounts for MORE dollars than exist" as the same clean answer. Only the first can happen
// to correct code: a reconcilable group's series sums to the total or to less. The second
// means a Group is marked reconcilable while its series double-counts, or an accumulator
// counted an event twice — and it now travels as Snapshot.SeriesOvershootMicros instead of
// being discarded at the one place that could see it.
//
// Both callers get it for free, which is why the disclosure lives here rather than in an
// error return: an error would need every caller to hold a logger (this package has none)
// and would let a caller drop the signal again, which is the defect being fixed.
//
// IT TAKES A CostSum RATHER THAN AN int64 so that a clamped residual cannot arrive here
// looking like an exact one. Both callers accumulated the residual with a bare `+=` — a
// wrap there produced a NEGATIVE figure, which the second case below then published as
// SeriesOvershootMicros: a field whose own doc tells the reader this process is wrong about
// its own arithmetic. Correct-but-saturated data therefore fabricated a defect report,
// while the real residual it should have carried was lost. Making the parameter a type that
// carries its own saturation flag is what stops the two conditions ever being confused
// again; the alternative — an int64 plus a bool a caller could forget — is the shape that
// produced this.
func (s *Snapshot) SetUngroupedCost(sum CostSum) {
	// SATURATION FIRST, and unconditionally: it is a statement about the money in this
	// snapshot, not about which of the two residual fields gets set, and it holds even when
	// the residual itself lands on zero. Totals.Saturated already means "read every money
	// figure here as a bound" and is OR-ed through Counts.Add, so the flag reaches a client
	// through the field it already has to consult.
	if sum.Saturated {
		s.Totals.Saturated = true
	}
	micros := sum.Micros
	switch {
	case micros > 0:
		s.UngroupedCostMicros = &micros
	case micros < 0:
		// Negated into a magnitude, so the field reads as "the series overshot by this much"
		// rather than making a client interpret a sign it did not ask for.
		//
		// math.MinInt64 has no positive counterpart, so negating it blindly returns the same
		// negative number and publishes exactly the sign confusion this field exists to avoid.
		// Reachable only from a saturated total, which is itself already disclosed.
		over := int64(math.MaxInt64)
		if micros != math.MinInt64 {
			over = -micros
		}
		s.SeriesOvershootMicros = &over
	}
}

// seriesCost is the dollars a label breakdown accounts for.
//
// Reads Counts.CostMicros per entry rather than any running total, because the series
// is what a client sums and this has to be exactly that arithmetic.
//
// Returns a CostSum rather than an int64 because two saturated entries wrapped it, and a
// wrap here does not stay here: the caller subtracts this from a bucket total, so a
// negative sum inflates the residual past the total it is a residual of. See CostSum.
func seriesCost(series map[string]Counts) CostSum {
	var total CostSum
	for _, v := range series {
		total.Add(v.CostMicros)
	}
	return total
}

// ParseWindow validates a window parameter against the storage resolution.
//
// Durations ONLY. It rejects the symbolic windows "today" and "7d", because a
// time.Duration genuinely cannot express a boundary and silently substituting a
// length would report a number for a span nobody asked for. Callers that accept
// those use ParseWindowSpec, which delegates here for every fixed length so the
// duration rules are defined once.
func ParseWindow(s string) (time.Duration, error) {
	if s == "" {
		return 10 * BucketWidth, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		// Not wrapped with the caller's string, for the reason in ParseGroup.
		// time.ParseDuration's own error quotes the input, so it is not
		// forwarded either.
		return 0, errors.New("bad window (want a duration such as 10m, 1h or 6h)")
	}
	if d < BucketWidth {
		return 0, fmt.Errorf("window %s is shorter than the %s bucket width", d, BucketWidth)
	}
	if d > MaxWindow {
		return 0, fmt.Errorf("window %s exceeds the %s retained", d, MaxWindow)
	}
	if d%BucketWidth != 0 {
		return 0, fmt.Errorf("window %s is not a multiple of %s", d, BucketWidth)
	}
	return d, nil
}

// dayAnchorHour is the hour used to establish WHICH local date is meant, before any
// boundary arithmetic is done on it.
//
// NOON, and deliberately the same convention and the same value as costledger's dayHour:
// these two layers have to agree about where a day starts, because this package computes
// the window bounds that select the day files that package names. A DST transition moves
// the clock by an hour (Australia/Lord_Howe by thirty minutes), so no transition can move
// noon onto a different DATE. Midnight is the opposite, and that is this constant's whole
// reason to exist.
//
// AN ANCHOR, NEVER A BOUND, which is the one place the two layers legitimately differ.
// costledger.dayOf may stop at noon because it only has to IDENTIFY a day — it names a
// file. A window's From has to be the day's FIRST INSTANT, so noon is not reusable
// directly and StartOfLocalDay sweeps back from it.
const dayAnchorHour = 12

// dayStartSweepSteps refine StartOfLocalDay's forward sweep, coarsest first.
//
// Hours then minutes then seconds is at most about 150 zone lookups, against roughly
// 90,000 for a second-by-second sweep of a whole day. A second is the finest step worth
// taking: every UTC offset and every transition instant in the IANA database is a whole
// number of seconds, so the boundary this lands on is exact rather than rounded.
var dayStartSweepSteps = []time.Duration{time.Hour, time.Minute, time.Second}

// StartOfLocalDay is the earliest instant that EXISTS on t's local calendar date, in t's
// own zone. It is the lower bound of "today".
//
// IT IS NOT time.Date(y, m, d, 0, 0, 0, 0, loc), and the difference is money. In a zone
// whose DST transition falls AT 00:00 the spring-forward day has no midnight at all, and
// time.Date resolves a wall time inside the gap onto the far side of it. MEASURED, with
// that expression as the bound:
//
//	now  = 2026-03-08 15:00 -0400 America/Havana
//	From = 2026-03-07 23:00 -0500   ← an hour before the previous day even ended
//	now  = 2026-09-06 15:00 -0300 America/Santiago
//	From = 2026-09-05 23:00 -0400   ← same shape
//
// So window=today reached an hour and fifty-nine minutes back into YESTERDAY and folded
// its last hour of spend into today's total. America/New_York, whose transition is at
// 02:00, was unaffected — which is why a suite whose only non-UTC zone was a
// time.FixedZone passed. A fixed offset has no transitions and structurally cannot express
// this. See TestStartOfLocalDay_IsTheFirstInstantOnTheDateInEveryZone.
//
// IT MATTERS MORE THAN IT DID, because the layer underneath was just corrected. The cost
// ledger names its day files from a date carried at noon (costledger.dayOf, dayFromName
// and dayHour), so a file is now named for the date a row is genuinely on, in every zone.
// This function is what SELECTS those files. Left as a midnight, the bound and the naming
// disagreed about where a Havana day starts, and the disagreement was silent: no error, no
// caveat, just a total including an hour that belongs to another date.
//
// THE RULE, stated plainly: sweep the instant axis forward from a point certainly before
// the date began and take the FIRST instant whose local date is the one wanted. That is
// the definition of "earliest existing instant" evaluated directly, rather than a wall
// time handed to time.Date and hoped to exist. It needs no assumption about which
// direction time.Date normalises, or that local time is monotone across a transition.
//
// AUTUMN-BACK IS THE OTHER HALF, and it is a real choice rather than a corollary. Where
// the clock goes back THROUGH midnight the wall time 00:00 occurs TWICE — in
// America/Havana on 2026-11-01 at 00:00 -0400 and again at 00:00 -0500, an hour apart.
// Both are on the date, so "midnight" alone does not name a bound. THE EARLIER ONE IS
// CHOSEN: a lower bound of the later instant would exclude the first hour of the day, and
// any spend in it would be missing from today's total with nothing saying so — the same
// silent shortfall in the other direction. The sweep picks it for free, because the
// earlier instant is the first one it reaches. Go's own time.Date happens to resolve an
// ambiguous wall time to the earlier occurrence here, but its documentation explicitly
// declines to guarantee that, so the choice is made here instead of inherited.
//
// EXPORTED because sessionapi's tests have to anchor a "today" fixture to the SAME
// boundary this serves. That helper was a second copy of the midnight expression and
// carried the identical flaw; a boundary derived twice is a boundary that drifts. The
// dependency direction is sessionapi to usage, never the reverse.
func StartOfLocalDay(t time.Time) time.Time {
	loc := t.Location()
	y, m, d := t.Date()
	onDate := func(x time.Time) bool {
		xy, xm, xd := x.Date()
		return xy == y && xm == m && xd == d
	}

	// before must be an instant OUTSIDE this date and earlier than it, so the sweep below
	// always starts before the boundary it is looking for. Noon anchors the date; whole days
	// back from that noon leave it.
	//
	// SUBTRACTED FROM THE ANCHOR AS A DURATION, not stepped with AddDate, and the difference
	// is a hang. A zone can skip a whole calendar date — Pacific/Apia dropped 2011-12-30
	// entirely when it crossed the date line — and then that date has no noon either, so
	// time.Date resolves it onto the NEXT one. AddDate on an already-normalised anchor
	// re-normalises to the same instant and the loop spins forever; MEASURED as a hang on
	// Pacific/Apia 2011-12-31 while writing this. A duration subtraction cannot normalise.
	//
	// It terminates because each iteration moves a fixed 24h further back on the instant
	// axis while a local date covers a bounded interval of it — under 50h even for
	// 1892-07-04 in Pacific/Apia, the longest in the database, where crossing the line the
	// other way made the date happen twice. So no iteration cap is needed, and one that
	// gave up while still inside the date would only hide the failure.
	noon := time.Date(y, m, d, dayAnchorHour, 0, 0, 0, loc)
	before := noon
	for back := 1; onDate(before); back++ {
		before = noon.Add(-time.Duration(back) * 24 * time.Hour)
	}

	first := before
	for _, step := range dayStartSweepSteps {
		first = before
		for !onDate(first) {
			first = first.Add(step)
		}
		// first is on the date and first-step is not — the sweep visited it and moved on —
		// so the boundary lies in (first-step, first]. Restart the next, finer pass there.
		before = first.Add(-step)
	}
	return first
}

// WindowToday and Window7d are the symbolic windows the API accepts.
//
// Symbolic because neither is a LENGTH: "today" is a boundary, and while "7d" has a
// fixed span it is longer than the ring retains, so both can only be answered from
// the durable cost ledger. time.ParseDuration reads neither string, which is why
// ParseWindow already rejects them and why they need their own parse.
const (
	WindowToday = "today"
	Window7d    = "7d"
)

// Window7dSpan is what "7d" MEANS: a rolling seven times twenty-four hours back from
// now. Stated once, and read by ParseWindowSpec, so nothing that has to reason about
// the span can spell it differently.
const Window7dSpan = 7 * 24 * time.Hour

// Window7dLocalDays is the MOST distinct LOCAL DATES a 7d window can touch: NINE.
//
// A CEILING, NOT A COUNT, and that is the correction. It used to be 8 and to be described as
// how many dates the window touches, which is right for an ordinary week and wrong once a
// year in every zone that observes DST.
//
// Two increments, for two different reasons:
//
//   - +1 because the span is ROLLING rather than calendar-aligned. Unless it begins exactly
//     at midnight it starts part-way through one date and ends part-way through another —
//     seven days' worth of hours spread across eight dates. Asked at 15:30 it runs from
//     15:30 seven days ago to 15:30 today and needs a piece of every date between, both ends
//     included. Asked exactly at midnight it needs the eighth date for one instant, which is
//     still a file to open.
//   - +1 again because A SPRING-FORWARD WEEK IS 167 HOURS. Eight assumed every day in the
//     week is 24 hours long, and one week a year is not: a 168-hour span reaches an hour
//     further back than a calendar week does. MEASURED at 00:00 local on 2026-03-15, From
//     lands at 23:00 on 2026-03-07 and the window touches NINE dates — in America/Havana, in
//     America/Santiago and in America/New_York alike, because this has nothing to do with
//     WHERE in the day the transition falls. Autumn is the harmless direction: a 169-hour
//     week means the span reaches less far and the ceiling is slack by a day.
//
// It is therefore the number of DAY FILES a durable ledger must still hold to answer this
// window, which is why config's retention floor agrees with this rather than being written
// as its own independent number. They were two constants that had to agree and did not: the
// floor was 7, so retention_days: 7 passed validation and then answered window:"7d" over a
// partial week — the exact case the floor exists to refuse. Nine has now been through the
// same correction twice, which is the argument for the ceiling rather than the count.
//
// COSTS ONE DAY FILE OF RETENTION ALL YEAR to cover two mornings of it, and that is the
// trade taken deliberately. The alternative considered was making 7d CALENDAR-ALIGNED, which
// is the more correct fix — it would make the count exactly 8 and true every week — but it
// changes what the window MEANS on the wire: "the last 7 days" and "since midnight 7 days
// ago" differ by up to a day of spend, every client comparing figures across the change
// would see a step, and ParseWindowSpec's own doc promises the rolling reading. That is a
// product decision, so it is disclosed rather than made here. A day file is roughly 300 KB
// at the volumes this ledger is sized for; the partial week it prevents is a wrong dollar
// total that nothing downstream can detect.
//
// Distinct from the local-midnight defect StartOfLocalDay fixes: 7d's From is a plain
// duration subtraction from now and contains no calendar arithmetic at all.
// TestParseWindowSpec_SevenDaysTouchesAtMostWindow7dLocalDays pins the ordinary week,
// TestParseWindowSpec_ASpringForwardWeekReachesTheNinthLocalDate pins the week that forced
// the second +1, and TestCostLedgerConfig_TheFloorCoversEveryDayTheWindowTouches pins the
// agreement with config.
const Window7dLocalDays = int(Window7dSpan/(24*time.Hour)) + 2

// Spec is a parsed window request. Either Dur is set (a fixed length the ring can
// serve) or From/To are (a boundary only the ledger can serve).
//
// Two representations in one type rather than two functions, because the handler's
// job is exactly to choose between them and a caller that forgets to ask which
// kind it has would silently serve the wrong span.
type Spec struct {
	// Label is what the response echoes back, so a client always learns which
	// window it actually got — which matters most when it is not the one asked for.
	Label string
	// Dur is non-zero for a fixed-length window.
	Dur time.Duration
	// From and To bound a symbolic window. Zero when Dur is set.
	From, To time.Time
}

// Symbolic reports whether this window needs the ledger.
func (s Spec) Symbolic() bool { return s.Dur == 0 }

// ParseWindowSpec parses any window the API accepts, including the symbolic ones.
//
// now is passed rather than read from the clock so a test can pin a day boundary,
// and so "today" is computed once per request instead of drifting between the
// bound calculation and the response label.
//
// "today" is the START OF THE LOCAL DAY to now. Local, not UTC: a laptop that
// crosses a timezone must not have its day reset mid-afternoon, which a UTC day would
// do. Just after the day starts it is a five-minute window, not a 24-hour one — that
// is the point of a boundary rather than a length.
//
// The start of the day, NOT "local midnight", and the distinction is not pedantry:
// midnight does not exist on the spring-forward day of any zone whose transition is at
// 00:00, and it occurs twice on the autumn one. StartOfLocalDay states which instant is
// meant and why, and it is the same day boundary the cost ledger names its files by.
//
// "7d" is exactly 7x24h back from now, ROLLING rather than seven calendar days.
// The label is echoed as "7d" so a client can read it that way; a "last 7 days"
// that silently meant "since last Monday" would be a different number, and the two
// differ by up to a day of spend.
//
// Delegates every fixed-length case to ParseWindow, so the duration set is defined
// in one place and the two cannot drift.
func ParseWindowSpec(s string, now time.Time) (Spec, error) {
	switch s {
	case WindowToday:
		return Spec{
			Label: WindowToday,
			From:  StartOfLocalDay(now),
			To:    now,
		}, nil
	case Window7d:
		// Window7dSpan rather than a second spelling of 7x24h: the ledger's retention
		// floor is derived from the same constant, and a span defined twice is how the two
		// came to disagree. See Window7dLocalDays.
		return Spec{Label: Window7d, From: now.Add(-Window7dSpan), To: now}, nil
	}
	d, err := ParseWindow(s)
	if err != nil {
		// Not wrapped with the caller's string, for the reason in ParseGroup: this
		// travels back over an unauthenticated endpoint. ParseWindow's own messages are
		// fixed strings for the same reason, so forwarding one is safe.
		return Spec{}, err
	}
	// Label is the caller's own spelling, not d.String(). It is what a response
	// SHOULD echo — "1h", not "1h0m0s" — though on the fixed-length path the label
	// actually served comes from Aggregator.Snapshot, which stringifies the duration;
	// only the ledger path reads this field. Carried anyway so the type answers "which
	// window is this" uniformly, and so a future caller cannot get a symbolic label
	// right and a duration label wrong.
	//
	// An empty window means the ten-bucket default, which has no caller spelling to
	// echo, so the duration's own form is the only label available.
	label := s
	if label == "" {
		label = d.String()
	}
	return Spec{Label: label, Dur: d}, nil
}

// ParseResolution validates a resolution parameter — the width of the buckets
// the caller wants back, as opposed to the window's total span.
//
// Folding happens server-side so every client gets the same arithmetic. A 1h
// window at 1m resolution is 60 bars, which no terminal renders legibly; at 5m
// it is 12. Doing that here rather than in each client means the mean-of-means
// problem below is solved once, correctly, instead of per consumer.
//
// Zero or empty means "storage resolution" (BucketWidth). A resolution finer
// than BucketWidth is an error rather than a silent upgrade: returning coarser
// data than asked for would make a client's axis labels wrong.
func ParseResolution(s string, window time.Duration) (time.Duration, error) {
	if s == "" {
		return BucketWidth, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		// Does not echo the caller's value — see ParseGroup.
		return 0, errors.New("bad resolution (want a duration such as 1m, 5m or 30m)")
	}
	if d < BucketWidth {
		return 0, fmt.Errorf("resolution %s is finer than the %s storage bucket", d, BucketWidth)
	}
	if d%BucketWidth != 0 {
		return 0, fmt.Errorf("resolution %s is not a multiple of %s", d, BucketWidth)
	}
	if d > window {
		return 0, fmt.Errorf("resolution %s exceeds the %s window", d, window)
	}
	// The window must divide by the resolution, or the NEWEST bucket is a lie.
	//
	// fold() emits a partial trailing group rather than truncating it — deliberately,
	// because the still-filling block is the one an operator is watching — but every
	// bucket is labelled with the same BucketSeconds. So window=10m at resolution=3m
	// returned four buckets of which the last covered ONE minute while claiming 180
	// seconds, and a client deriving a burn rate from the newest bar tripled it.
	//
	// Rejected here rather than papered over downstream: the caller asked for a slicing
	// this window cannot express, and serving a differently-sized bucket under the
	// requested label would be a wrong number wearing a right label. A client that
	// wants the fine bar picks a window the resolution divides.
	//
	// FIXED LITERAL, no interpolation. This endpoint is unauthenticated, so an error
	// body must never reflect caller-supplied bytes back — see ParseGroup. The messages
	// above interpolate only a re-stringified time.Duration, which cannot carry
	// arbitrary bytes; this one needs neither operand to be actionable.
	if window%d != 0 {
		return 0, errors.New("resolution does not divide the window evenly (the newest bucket would be shorter than the width reported for it)")
	}
	return d, nil
}

// fold groups src into buckets of the given width, summing counts and combining
// latency statistics.
//
// Latency is the part that cannot be done naively. Averaging the per-minute
// means would weight a minute with 1 request equally with a minute with 500 —
// the mean-of-means error. Instead each source bucket's running sums are
// reconstituted (sum = mean x n, sumSq recovered from the variance identity) and
// re-accumulated, so the folded mean and standard deviation are exactly what a
// single wider bucket would have recorded.
func fold(src []Bucket, width time.Duration) []Bucket {
	if width <= BucketWidth || len(src) == 0 {
		return src
	}
	per := int(width / BucketWidth)
	out := make([]Bucket, 0, (len(src)+per-1)/per)

	for i := 0; i < len(src); i += per {
		end := i + per
		if end > len(src) {
			end = len(src)
		}
		acc := Bucket{At: src[i].At}
		var latSum, latSumSq float64
		var latN int64
		series := map[string]Counts{}

		for _, b := range src[i:end] {
			acc.Counts.Add(b.Counts)
			// Weighted by LatSamples, not Requests: the source mean was computed
			// over measured requests only, so reconstituting with Requests would
			// re-introduce the dilution latStats exists to avoid.
			if b.LatMeanMs > 0 && b.LatSamples > 0 {
				n := float64(b.LatSamples)
				sum := b.LatMeanMs * n
				// variance = sumSq/n - mean^2  =>  sumSq = n*(variance + mean^2)
				sumSq := n * (b.LatStdDevMs*b.LatStdDevMs + b.LatMeanMs*b.LatMeanMs)
				latSum += sum
				latSumSq += sumSq
				latN += b.LatSamples
			}
			for k, v := range b.Series {
				cur := series[k]
				cur.Add(v)
				series[k] = cur
			}
		}

		if latN > 0 {
			acc.LatSamples = latN
			n := float64(latN)
			acc.LatMeanMs = latSum / n
			if variance := latSumSq/n - acc.LatMeanMs*acc.LatMeanMs; variance > 0 {
				acc.LatStdDevMs = math.Sqrt(variance)
			}
		}
		if len(series) > 0 {
			acc.Series = series
		}
		out = append(out, acc)
	}
	return out
}

// Snapshot returns the last window of buckets, oldest first.
//
// The newest bucket is the one containing now, still filling — a client
// rendering it should expect its final value to rise. Reporting it partially is
// better than withholding it: an operator watching a live chart wants the
// current minute visible.
//
// An unknown session yields a snapshot of all-zero buckets rather than an error:
// a session that has produced no priceable traffic yet is a normal state, and
// the caller already knows whether the session exists from /v1/sessions.
func (a *Aggregator) Snapshot(window, resolution time.Duration, sessionID string, group Group) Snapshot {
	if resolution < BucketWidth {
		resolution = BucketWidth
	}
	n := int(window / BucketWidth)
	if n < 1 {
		n = 1
	}
	if n > NumBuckets {
		n = NumBuckets
	}

	a.mu.RLock()
	defer a.mu.RUnlock()

	ring := a.all
	if sessionID != "" {
		r, ok := a.sessions[sessionID]
		if !ok {
			ring = nil // fall through: emits zeroed buckets at the right times
		} else {
			ring = r.buckets
		}
	}

	newest := a.now().Truncate(BucketWidth)
	out := Snapshot{
		Window:        window.String(),
		BucketSeconds: int(resolution / time.Second),
		Session:       sessionID,
		Group:         group,
		Buckets:       make([]Bucket, 0, n),
	}

	// The dollars no series entry will carry. Accumulated per raw bucket and set once
	// below, for the reason Totals is: the figure must not change with the resolution the
	// caller asked for. See Snapshot.UngroupedCostMicros.
	//
	// A CostSum, not an int64: this is money, and a wrapped residual is read downstream as
	// the series overshooting its own total. See CostSum.
	var ungrouped CostSum
	reconcilable := group.Reconcilable()

	for i := n - 1; i >= 0; i-- {
		t := newest.Add(-time.Duration(i) * BucketWidth)
		b := Bucket{At: t}
		if ring != nil {
			if src := &ring[slot(t)]; src.start.Equal(t) {
				b.Counts = src.Counts
				b.LatMeanMs, b.LatStdDevMs = src.latStats()
				b.LatSamples = src.latN
				b.Series = src.series(group)
			}
		}
		out.Totals.Add(b.Counts)
		if reconcilable {
			// Derived here rather than counted in foldInto, because THIS is where a group is
			// chosen: one event is in the byMethod map and out of the byEndpoint one depending
			// on which labels it carried, so "what this breakdown leaves out" is not a property
			// of the event and cannot be accumulated at record time. The subtraction is exact
			// for a reconcilable group — every event lands in at most one entry of the map
			// being read, so the entries sum to the cost of the events that had a label for
			// this axis, and the rest is the shortfall. Group.Reconcilable is what keeps
			// group=plugin, whose entries deliberately double-count, out of this arithmetic.
			//
			// Three saturating steps rather than one expression, because all three can clamp
			// and dropping any one of the flags reinstates a silent wrap: the series sum, the
			// bucket total going in, and the subtraction.
			sc := seriesCost(b.Series)
			ungrouped.Add(b.CostMicros)
			ungrouped.Sub(sc.Micros)
			if sc.Saturated {
				ungrouped.Saturated = true
			}
		}
		if ring != nil {
			if src := &ring[slot(t)]; src.start.Equal(t) {
				for k, v := range src.byUnpriced {
					if out.UnpricedBy == nil {
						out.UnpricedBy = make(map[string]int64, len(src.byUnpriced))
					}
					out.UnpricedBy[k] += v.Requests
				}
				for k, v := range src.byProvenance {
					if out.PricedBy == nil {
						out.PricedBy = make(map[string]int64, len(src.byProvenance))
					}
					out.PricedBy[k] += v.Requests
				}
				// Same shape, same reason: summed from the raw buckets so the caveat's
				// breakdown is unaffected by the requested resolution, and left nil rather
				// than emitted empty so absence is not read as "checked, all exact".
				for k, v := range src.byIncomplete {
					if out.IncompleteBy == nil {
						out.IncompleteBy = make(map[string]int64, len(src.byIncomplete))
					}
					out.IncompleteBy[k] += v.Requests
				}
			}
		}
		out.Buckets = append(out.Buckets, b)
	}
	// Derived after the loop: Totals is only complete once every bucket has been
	// added, so this cannot be set in the literal above.
	//
	// An inexact figure is still a figure, and PricedRequests counts it — which is what
	// keeps this flag's contract intact. It promises that false means CostMicros is
	// absent everywhere, and a floor's dollars ARE in CostMicros, so a window whose every
	// request was a truncated stream must not report "cost unavailable" over a real
	// non-zero total. Its inexactness is disclosed by Totals.IncompleteRequests, never by
	// withholding the figure.
	//
	// The COUNTER, never CostMicros > 0. A window whose every request was DECLARED FREE
	// by the gateway — a settled zero — has priced requests and no dollars, and reading
	// the total would report it "cost unavailable". Those are different truths: false
	// must render "cost unavailable", a declared-free window must render $0.0000. See
	// costevent.Event.Settled, and TestPricing_SettledZeroIsNotRePriced, which pins it.
	out.Priced = out.Totals.PricedRequests > 0
	// Absent unless there is something to disclose, which is the whole convention: see
	// SetUngroupedCost.
	out.SetUngroupedCost(ungrouped)

	// Fold last: totals are summed from the raw buckets above and are unaffected
	// by grouping width, so a client's summary line agrees with its chart no
	// matter which resolution it asked for.
	out.Buckets = fold(out.Buckets, resolution)
	return out
}

// latStats derives mean and population standard deviation from the running
// sums. Guarded against a small negative variance, which float cancellation can
// produce when every sample is identical.
func (b *bucket) latStats() (mean, stddev float64) {
	if b.latN == 0 || b.latSum == 0 {
		return 0, 0
	}
	// Divide by the number of MEASURED requests, not all of them. A response with
	// a zero duration is unmeasured, not instant, and counting it in the divisor
	// understates the mean in proportion to how many such responses there were.
	n := float64(b.latN)
	mean = b.latSum / n
	variance := b.latSumSq/n - mean*mean
	if variance <= 0 {
		return mean, 0
	}
	return mean, math.Sqrt(variance)
}

// series copies the requested label map. Copied, not aliased: the caller holds
// only a read lock, and handing out the live map would let a JSON encoder read
// it while a later Record mutates it.
func (b *bucket) series(g Group) map[string]Counts {
	var src map[string]Counts
	switch g {
	// Both spellings read the same map. GroupMethod is the name this series
	// shipped under; see its godoc.
	case GroupModel, GroupMethod:
		src = b.byMethod
	case GroupEndpoint:
		src = b.byEndpoint
	case GroupSession:
		src = b.bySession
	case GroupAgent:
		src = b.byAgent
	case GroupStatus:
		src = b.byStatus
	case GroupPlugin:
		src = b.byPlugin
	default:
		return nil
	}
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]Counts, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}
