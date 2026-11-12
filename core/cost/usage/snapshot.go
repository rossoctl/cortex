package usage

import (
	"errors"
	"math"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
)

// Group selects which label breakdown a Snapshot carries.
type Group string

const (
	GroupNone Group = "none"
	// GroupModel breaks totals down by the model named on the request.
	//
	// This is the series that shipped as GroupMethod. The aggregator only ever
	// populated it from Inference.Model (see foldInto) — A2A and MCP method names
	// never entered it — so "method" was a misnomer from the start. agentop's
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
	// GroupAgent breaks totals down by the coding agent that made the request, by
	// name with its version dropped (pipeline.AgentName) — the axis #952 asks for, since "what did today cost" is only
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
	GroupAgent Group = "agent"
	// GroupProject breaks totals down by project: the git repo root of the agent process's
	// working directory, from SessionEvent.Project. An event with no project lands under
	// UnknownProjectLabel. Folded for every event, like GroupAgent, so the series sum to the total.
	GroupProject Group = "project"
	// GroupCurrency breaks totals down by the BILLING UNIT the figures are denominated in.
	//
	// It exists so a deployment whose gateways bill in different units has somewhere to get a
	// figure per unit, once a combined total has been refused as meaningless. Unlike every other
	// axis here it is not a property of the traffic but of the RATE that priced it: the ledger
	// serves it from Row.Currency, part of its row key, and the in-memory ring from a per-bucket
	// tally of the same rule (pricing.UnitOf), so both kinds of window break down alike.
	GroupCurrency Group = "currency"
	GroupStatus   Group = "status"
	GroupPlugin   Group = "plugin"
	GroupHost     Group = "host"
)

// UnknownProjectLabel is the GroupProject series of an event with no project. A whole project
// label starts with "/" or "~", and a cut one is at least 93 bytes, so neither is this one.
const UnknownProjectLabel = "unknown"

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
	case GroupProject:
		return GroupProject, nil
	case GroupCurrency:
		return GroupCurrency, nil
	case GroupStatus:
		return GroupStatus, nil
	case GroupPlugin:
		return GroupPlugin, nil
	case GroupHost:
		return GroupHost, nil
	}
	// Deliberately does NOT echo the caller's value: this message is returned
	// over an unauthenticated endpoint, and reflecting arbitrary query input
	// into a response body is how a reflected-content issue starts. The valid
	// set is short enough that naming it is more useful than quoting the input.
	return "", errors.New("unknown group (want none, model, endpoint, session, agent, project, currency, status, plugin or host; method is accepted as an alias for model)")
}

// Snapshot is the wire shape of GET /v1/usage.
type Snapshot struct {
	// Window is the span this snapshot ACTUALLY covers, e.g. "10m", "6h0m0s" or
	// "today". Not necessarily the span requested.
	//
	// The difference matters: the symbolic windows ("today", "7d", "month") are served from the
	// durable cost ledger, and a proxy with no ledger — Kubernetes by design — answers
	// them from the ring's maximum window instead and reports THAT here. A client must
	// read this field rather than echo its own request, or it will label six hours of
	// spend as a day's.
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

	// Currencies is every distinct unit behind a figure in Totals — a priced request or a
	// saving — sorted.
	//
	// IT TRAVELS BESIDE THE FIGURES BECAUSE IT CANNOT LIVE INSIDE THEM. Counts is a flat
	// summable aggregate, and it has to stay one — a unit belongs to the grouping key, not to
	// the numbers, or every consumer would be invited to add across it. So the fact that a
	// total spans two units is stated here, and a reader decides what it may legally add.
	//
	// MORE THAN ONE ENTRY MEANS Totals.CostMicros IS NOT A FIGURE. Summing credits into dollars
	// produces a number that is neither, and it looks exactly like a correct one — larger, not
	// obviously wrong. A consumer seeing two entries must refuse to present a single total;
	// `agentop cost` does, and says which units it found.
	//
	// ABSENT MEANS NOT COMPUTED OR NOTHING TO LABEL, and a client must not turn that into a
	// refusal: an idle window names no unit, and a producer older than the ring's unit tally omits
	// it for ring windows. Both the ledger and the ring fill it now. One entry is fine. Only two or
	// more is a claim.
	Currencies []string `json:"currencies,omitempty"`

	// Agent is the agent= filter this snapshot was narrowed to, set only when the producer applied it,
	// so a client can tell a narrowed answer from a server that ignored the parameter.
	Agent string `json:"agent,omitempty"`
	// SeriesCurrencies names, for group=model, endpoint and agent, the units EACH series is in —
	// the cross-tabulation Currencies cannot carry, so one agent's (or model's, or endpoint's)
	// figure can be labelled and summed without borrowing another's units. Keyed like the series;
	// absent for every other grouping and from older producers, which a client reads as "unknown"
	// and falls back to Currencies for.
	SeriesCurrencies map[string][]string `json:"seriesCurrencies,omitempty"`
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
	// DaysOutsideRetention is how many days of the REQUESTED window fall before the durable
	// ledger's retention horizon. A COVERAGE statement, and a CEILING: the configuration does
	// not promise those days, so spend on them may be missing from the total.
	//
	// NOT A LOSS, AND DELIBERATELY NOT IN Degraded, whose downstream meaning is "rows are
	// missing from the sum". Nothing in the ledger records its own inception or what prune
	// deleted, so an absent old day is indistinguishable from a day that was never written — a
	// three-day-old install with retention_days=10 has twenty-two absent days before its cutoff
	// and lost nothing. Reporting that as damage would make the disclosure fire when nothing
	// was pruned, and noise on a disclosure is how a real one gets ignored.
	//
	// What it states instead is the part a client can act on and the server can prove: this
	// window asked for N days the configuration does not reach. Whether spend happened on them
	// is unknowable here, and so is whether the total includes it.
	//
	// AN EARLIER VERSION OF THIS SAID "that the total cannot include it is certain". It is not,
	// and the state that breaks it is ordinary rather than exotic: cost/ledger's prune floors its
	// reference day at the NEWEST day file, so a ledger nothing has written to keeps its last
	// retainDays files however long ago they were written — while this figure is measured from
	// the clock. A query then sums day files this field has already called outside. Reproduced
	// in cost/ledger's TestSnapshot_ADayReportedOutsideRetentionCanStillBeInTheTotal:
	// retention_days 10, files for 21 consecutive days, read ten days after the last write —
	// 21 days reported outside, and ten days of spend in the total.
	//
	// A CLIENT MUST THEREFORE RENDER THIS AS "may be missing", never as a deduction from the
	// figure beside it. Both shipped consumers do: the band marks the total a floor, and
	// `agentop cost` prints a coverage line.
	//
	// SAME SHAPE AS THE UNPRICED COVERAGE GAP beside it — Unpriced over Priceable — which is
	// why it sits here rather than with the damage counters: both say "this figure covers less
	// than the question implied", and neither says anything was destroyed.
	//
	// ZERO DOES NOT MEAN THE TOTAL IS COMPLETE, and a client must not read it that way. It is
	// measured against retention_days AS CONFIGURED NOW, so a day pruned while the setting was
	// SHORTER is inside today's horizon and reported by nothing: set retention_days to 9, run
	// for a week, set it back to 31, and the month's total is short with this field at 0. The
	// same is true of a day prune deleted for any other reason. Closing that needs a persisted
	// inception date or a prune record and the ledger keeps neither — and inferring one from the
	// files present is the false-positive design this field replaced.
	DaysOutsideRetention int64 `json:"daysOutsideRetention,omitempty"`
	// UnpricedBy counts the requests that could NOT be priced, keyed
	// "<endpoint> <model>". Present only when something was unpriced — and never
	// present at all on a ledger-backed window, where a per-minute row carries only
	// its own labels and cannot distinguish the unpriced pairs from the priced ones.
	// Absence is therefore not a claim that there were no gaps; compare
	// Totals.PricedRequests with Totals.PriceableRequests for that.
	//
	// A gap has to be nameable, not just countable. "Cost is incomplete" gives an
	// operator nothing to act on; "api.openai.com gpt-5: 412" names the pair to
	// investigate. Traffic carrying no model is excluded — naming every non-LLM
	// call the proxy handled would bury the real gaps.
	//
	// "ADD A PRICING ENTRY" IS NOT ALWAYS THE REMEDY, and this field cannot say which it
	// is. A pair lands here when no rate covers it, when the rate covers only some of the
	// tiers the request used, AND when the figure the rates produced was refused as
	// implausible — an absurd rate, or a token count that cannot be one. In that last case
	// the entry already exists and the fix is upstream of it. See usage.Aggregator.costOf,
	// which is where the causes converge, and event.RejectedImplausible, which keeps
	// them apart on the path that CAN report per request.
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
	// The dominant instance is a GATEWAY-PRICED RESPONSE THE INFERENCE PARSER COULD NOT
	// READ: /v1/embeddings and /v1/rerank are not among the paths it parses, so the event
	// carries no model while the gateway's own header settles a cost, and that spend counts
	// toward Totals and drops out of group=model. Per-axis that is the honest answer — there
	// is no model to attribute it to, and moving it into a made-up bucket or out of the
	// totals would each be worse. Undisclosed, though, it makes the response contradict
	// itself with nothing to explain the difference.
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
	// BOTH WINDOW KINDS POPULATE IT, verified rather than assumed — which is why this is not
	// ledger-only the way Degraded is, and it has to hold on both or a client would come to
	// trust the reconciliation and then have it break at whichever window boundary switches
	// storage:
	//   - a LEDGER window, because such a response is stored as a row with Model "" and
	//     ledger.labelFor then returns ok=false for group=model. Pinned by
	//     TestFold_GatewayPricedRowWithNoModelIsDisclosedAsUngrouped, which lives with the ledger
	//     reader.
	//   - a RING window, because Aggregator.costOf prices any SETTLED cost record whether or
	//     not the event carries an Inference extension, while foldInto guards byMethod on a
	//     non-empty model — so the same spend lands in the bucket total and in no series
	//     entry. Pinned by
	//     TestSnapshot_GatewayPricedTrafficWithNoModelIsDisclosedAsUngrouped.
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
	// IT MUST NOT COLLAPSE INTO "no residual". A `micros <= 0` guard treats "the breakdown
	// accounts for everything" and "the breakdown accounts for MORE than everything" as the
	// same clean answer, and the second is a defect report. Reconcilability is now a property
	// of the SOURCE as well as the group (see Group.Reconcilable), so a disagreement between
	// the two predicates is a live possibility, and this residual going negative is how it
	// would show.
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
	// UngroupedAvoidedMicros is UngroupedCostMicros for the OTHER money field: the part of
	// Totals.AvoidedMicros that no entry in this response's series carries.
	//
	// It exists because Counts.AvoidedMicros ships per label automatically — Series is a map
	// of Counts and Counts.Add carries the field — so the moment a client renders a per-model
	// "saved" breakdown, that breakdown can sum to LESS than the total beside it. Without
	// this field there is nothing to explain the gap, which is the exact failure
	// UngroupedCostMicros exists to prevent, arriving through a field added later.
	//
	// REACHABLE, and by a route the cost residual mostly is not: a row can carry a saving
	// with no model at all. ledger.Writer.Record admits a cost record whose only figure
	// is an applied saving even when the response had no inference extension to read a model
	// from — deliberately, because a saving on a request nothing could price is the case
	// event.Record was split out to serve. Under group=model that row has no label, so
	// its saving lands here and nowhere else.
	//
	// Same absent-not-zero rule as its twin: a zero would have to mean both "checked, the
	// breakdown is complete" and "nothing computed a residual on this path".
	UngroupedAvoidedMicros *int64 `json:"ungroupedAvoidedMicros,omitempty"`
	// SeriesAvoidedOvershootMicros is SeriesOvershootMicros for avoided cost: the amount by
	// which this response's series summed to MORE than Totals.AvoidedMicros.
	//
	// A SEPARATE FIELD FROM SeriesOvershootMicros, and not redundant with it, which is the
	// question to ask of any second defect-report field. The shared cause — a reconcilable
	// group whose series double-counts — moves both residuals and would fire both. But a row
	// carrying a saving and NO COST double-counted moves only this one: the cost residual
	// stays at zero, which reads as "the breakdown accounts for everything". So the cost
	// field alone cannot report an avoided overshoot, and collapsing them would make a real
	// defect invisible in exactly the case this PR made reachable.
	//
	// Nothing should render it. See SeriesOvershootMicros: this is "do not trust the
	// breakdown in this response, and file a bug".
	SeriesAvoidedOvershootMicros *int64 `json:"seriesAvoidedOvershootMicros,omitempty"`
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
	// A FLOOR ON ROWS LOST, NOT AN EXACT COUNT OF THEM. It counts lines because that is
	// what the reader can see. One undecodable line is usually one row — but the ledger's
	// newline fence only runs while the writing process is alive, so a crash mid-append
	// leaves an unterminated fragment and the next append is concatenated onto it: ONE
	// line holding TWO lost rows, measured that way.
	//
	// So a non-zero value means "AT LEAST this many rows are missing", which is the reading
	// a client has to present. Same number and same qualification as
	// ledger.Caveats.SkippedLines, which is where this field is copied from; see
	// cost/ledger's appendBytes for why the bytes on disk cannot support an exact figure.
	SkippedLines int64 `json:"skippedLines,omitempty"`
	// TruncatedDays is how many day files were abandoned part-way. Worse than a skipped
	// line by an unbounded amount: the rest of that file is missing, and a file holds a
	// whole day.
	TruncatedDays int64 `json:"truncatedDays,omitempty"`
	// DroppedRowsTotal is rows the WRITER lost: a failed append, or an enqueue dropped because the
	// queue was full while the filesystem stalled. Cumulative for the life of the process, which the
	// name says because it cannot be anything else — a drop is a fact about the writer, it happened
	// once, and no later read can rediscover it, so attributing it to whichever read happened to
	// notice would be a fiction.
	//
	// THE WRITE SIDE OF THIS OBJECT, which had only the read side. A day that lost a minute to ENOSPC
	// served a short total under priced:true with no caveat at all — exactly the silence this struct
	// was added to end, on the half nobody wired up. Compare across polls for a rate; a non-zero
	// value at all means some total below is short.
	DroppedRowsTotal int64 `json:"droppedRowsTotal,omitempty"`
	// UnreadableDays is how many day files could not be opened or scanned at all — a
	// permission change, a vanished mount, an IO error on the first read.
	//
	// THE WORST OF THE THREE, and the one this struct was missing. A producer reports a
	// caveat when any of them is non-zero, so a read whose only fault was an unopenable
	// day serialised as `"degraded":{}` — every field omitempty, nothing quantified — which
	// says "something was wrong" and withholds what. A whole day of spend is missing on
	// this path, which is more than a truncated file loses.
	UnreadableDays int64 `json:"unreadableDays,omitempty"`
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
// NOT SUFFICIENT ON ITS OWN — treated as such, it publishes a residual equal to an entire
// total. This says whether a group's series WOULD reconcile against Totals; it cannot say
// whether the source being read can produce that series at all. The second question belongs
// to the source: the ring keeps a status series, a plugin series and per-session rings, while
// a cost-ledger row is (endpoint, model, agent, provenance) per minute and has no column for
// status or session. ledger.Groupable answers it there, and ledger.Fold requires
// BOTH — "the breakdown falls short by this much" and "there is no breakdown to fall short"
// are different answers, and only the first is a residual.
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
// A NEGATIVE RESIDUAL IS A DEFECT REPORT, not "nothing to disclose": only a shortfall can
// happen to correct code, since a reconcilable group's series sums to the total or to less. A
// negative means a Group is marked reconcilable while its series double-counts, or an
// accumulator counted an event twice, so it travels as Snapshot.SeriesOvershootMicros rather
// than being discarded at the one place that could see it.
//
// Both callers get that for free, which is why the disclosure lives here rather than in an
// error return: an error would need every caller to hold a logger (this package has none) and
// would let a caller drop the signal again.
//
// IT TAKES AN event.CostSum RATHER THAN AN int64 so a clamped residual cannot arrive looking
// like an exact one. Accumulated with a bare `+=`, a wrap produces a NEGATIVE figure, which the
// second case below then publishes as SeriesOvershootMicros — so correct-but-saturated data
// fabricates a defect report while the real residual is lost. A parameter that carries its
// own saturation flag is what stops the two conditions being confused; an int64 plus a bool a
// caller could forget is the shape that produced it.
func (s *Snapshot) SetUngroupedCost(sum event.CostSum) {
	// SATURATION FIRST, and unconditionally: it is a statement about the money in this
	// snapshot, not about which of the two residual fields gets set, and it holds even when
	// the residual itself lands on zero. Totals.Saturated already means "read every money
	// figure here as a bound" and is OR-ed through Counts.Add, so the flag reaches a client
	// through the field it already has to consult.
	if sum.Saturated {
		s.Totals.Saturated = true
	}
	// The sign split lives in residualOf, shared with SetUngroupedAvoided; the FIELDS are
	// named here, in the setter that owns them. See residualOf for why that direction.
	s.UngroupedCostMicros, s.SeriesOvershootMicros = residualOf(sum)
}

// SetUngroupedAvoided is SetUngroupedCost for avoided cost.
//
// Split from it rather than folded into one two-argument call because the two are published
// on different fields and a caller may legitimately have only one to report — the ledger path
// computes both, the ring path computes both, and a future source that breaks down only spend
// should not be forced to pass a zero that this method cannot distinguish from a real one.
// The SHARED half — saturation, the sign split, the MinInt64 guard — lives in setResidual so
// the two cannot drift.
//
// NOT SPEND, and this method changes nothing about that: it publishes a residual for a
// counterfactual so a breakdown of it reconciles, on the same rule that forbids adding it to
// any dollar total. See Counts.AvoidedMicros.
func (s *Snapshot) SetUngroupedAvoided(sum event.CostSum) {
	if sum.Saturated {
		s.Totals.Saturated = true
	}
	s.UngroupedAvoidedMicros, s.SeriesAvoidedOvershootMicros = residualOf(sum)
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

// addCoverageInto folds one raw bucket's request count into a coverage breakdown, saturating.
//
// THE UNIFORM CALL SITE Counts.Add ARGUES FOR, APPLIED WHERE IT WAS NOT. Add checks Requests, Errors
// and the three coverage counters even though traffic cannot reach 2^63 of any of them, on the
// stated grounds that it is exported and "a uniform call site is the only kind that cannot be
// forgotten when a field is added". These three breakdowns are the same counters on the way out and
// were three bare `+=` on int64: two saturated buckets summed to -2, a negative population, which is
// read downstream as a series overshooting its own total.
//
// Needs a saturated bucket to reach, which Record cannot produce — so this is the cheap half of the
// same argument, not a live defect.
func addCoverageInto(m map[string]int64, k string, v int64, saturated *bool) {
	sum, over := event.AddSat(m[k], v)
	m[k] = sum
	if over {
		*saturated = true
	}
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
	return a.snapshot(window, resolution, sessionID, "", group, MaxSeriesInResponse)
}

// snapshot reads one ring: sessionID's when set, else agent's when set, else every session's. A
// ring that does not exist reads as zeroed buckets, for an agent as for a session.
func (a *Aggregator) snapshot(window, resolution time.Duration, sessionID, agent string, group Group, maxSeries int) Snapshot {
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
	switch {
	case sessionID != "":
		r, ok := a.sessions[sessionID]
		if !ok {
			ring = nil // fall through: emits zeroed buckets at the right times
		} else {
			ring = r.buckets
		}
	case agent != "":
		ring = a.agents[agent] // nil when the agent has sent nothing yet, as above
	}

	newest := a.now().Truncate(BucketWidth)
	// THE SPAN COVERED, NOT THE SPAN ASKED FOR, which is what the field promises and what a client
	// labels its chart with. n is clamped to NumBuckets above, so echoing the request reported a 12h
	// window over 360 one-minute buckets — six hours of data called half a day, the exact failure
	// Snapshot.Window's own godoc names ("or it will label six hours of spend as a day's"), and
	// "6h0m0s" is the example it gives. Whole buckets rather than the raw duration for the same
	// reason BucketSeconds is rounded: a 90s request is answered with one bucket, so it covers 1m.
	covered := time.Duration(n) * BucketWidth
	// AND THE BUCKET WIDTH CANNOT EXCEED THE WINDOW IT SITS IN. BucketSeconds was the requested
	// resolution verbatim while Window is derived, so the pair could contradict each other on a
	// SHORTENED span: window=today at 00:30 with resolution=1h came back window "30m0s" with
	// bucketSeconds 3600 — fold packs the thirty one-minute buckets into one partial group covering
	// half an hour and labels it six times its width, which is the mislabel Window's own derivation
	// three lines above exists to prevent. The divisibility check cannot catch it, because 1h divides
	// the 6h span the resolution is validated against; only the served span is shorter.
	bucketWidth := resolution
	if bucketWidth > covered {
		bucketWidth = covered
	}
	out := Snapshot{
		Window:        covered.String(),
		BucketSeconds: int(bucketWidth / time.Second),
		Session:       sessionID,
		Group:         group,
		Buckets:       make([]Bucket, 0, n),
	}

	// The dollars no series entry will carry. Accumulated per raw bucket and set once
	// below, for the reason Totals is: the figure must not change with the resolution the
	// caller asked for. See Snapshot.UngroupedCostMicros.
	//
	// An event.CostSum, not an int64: this is money, and a wrapped residual is read downstream as
	// the series overshooting its own total. See event.CostSum.
	var ungrouped event.CostSum
	// Its own accumulator, never folded into the line above: the two are different
	// quantities and only one of them is money. See Snapshot.UngroupedAvoidedMicros.
	var ungroupedAvoided event.CostSum
	// Raised by addCoverageInto, and read after the totals are assigned below — Totals is derived
	// post-loop, so setting the flag inside the loop would be overwritten.
	var coverageSaturated bool
	reconcilable := group.Reconcilable()
	units := map[string]struct{}{}
	var labelUnits map[string]map[string]struct{}
	unitAxis := group
	if unitAxis == GroupMethod {
		unitAxis = GroupModel
	}

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
			// chosen: one event is in the byMethod map and out of the byEndpoint one depending on
			// which labels it carried, so "what this breakdown leaves out" is not a property of
			// the event and cannot be accumulated at record time. The subtraction is exact for a
			// reconcilable group — every event lands in at most one entry of the map being read,
			// so the entries sum to the cost of the events that had a label for this axis and the
			// rest is the shortfall. Group.Reconcilable keeps group=plugin, whose entries
			// deliberately double-count, out of this arithmetic.
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
			// The same three saturating steps for the other money field, for the same
			// reason: a per-label "saved" breakdown a client renders must reconcile against
			// the total beside it. See Snapshot.UngroupedAvoidedMicros.
			sa := seriesAvoided(b.Series)
			ungroupedAvoided.Add(b.AvoidedMicros)
			ungroupedAvoided.Sub(sa.Micros)
			if sa.Saturated {
				ungroupedAvoided.Saturated = true
			}
		}
		if ring != nil {
			if src := &ring[slot(t)]; src.start.Equal(t) {
				for k, v := range src.byUnpriced {
					if out.UnpricedBy == nil {
						out.UnpricedBy = make(map[string]int64, len(src.byUnpriced))
					}
					addCoverageInto(out.UnpricedBy, k, v.Requests, &coverageSaturated)
				}
				for k, v := range src.byProvenance {
					if out.PricedBy == nil {
						out.PricedBy = make(map[string]int64, len(src.byProvenance))
					}
					addCoverageInto(out.PricedBy, k, v.Requests, &coverageSaturated)
				}
				// Same shape, same reason: summed from the raw buckets so the caveat's
				// breakdown is unaffected by the requested resolution, and left nil rather
				// than emitted empty so absence is not read as "checked, all exact".
				for k, v := range src.byIncomplete {
					if out.IncompleteBy == nil {
						out.IncompleteBy = make(map[string]int64, len(src.byIncomplete))
					}
					addCoverageInto(out.IncompleteBy, k, v.Requests, &coverageSaturated)
				}
				for _, us := range src.labelUnits[GroupAgent] {
					for u := range us {
						units[u] = struct{}{}
					}
				}
				labelUnits = unionUnits(labelUnits, src.labelUnits[unitAxis])
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
	// must render "cost unavailable", a declared-free window must render $0.00. See
	// event.Event.Settled, and TestPricing_SettledZeroIsNotRePriced, which pins it.
	out.Priced = out.Totals.PricedRequests > 0
	// A CLAMPED BREAKDOWN IS DISCLOSED ON THE SAME FLAG Counts.Add RAISES for a saturated Requests,
	// so this is not a new meaning: Saturated already says "a count or a figure here is a floor".
	// Set after Totals is assigned, because that assignment replaces the struct.
	if coverageSaturated {
		out.Totals.Saturated = true
	}
	// BOUNDED BEFORE IT IS SERVED, and after the totals and the residual are computed from
	// the raw buckets — so folding a label into (other) changes what the breakdown NAMES and
	// nothing about what it sums to. Doing it earlier would move UngroupedCostMicros, which is
	// a statement about labels this axis could not carry rather than about labels that did not
	// make the cut. See MaxSeriesInResponse for the 4.7 MB this bounds.
	capSeriesAcrossWindow(out.Buckets, maxSeries)
	out.Currencies = sortedUnits(units)
	out.SeriesCurrencies = seriesUnits(labelUnits, out.Buckets)

	// Absent unless there is something to disclose, which is the whole convention: see
	// SetUngroupedCost.
	out.SetUngroupedCost(ungrouped)
	out.SetUngroupedAvoided(ungroupedAvoided)

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
