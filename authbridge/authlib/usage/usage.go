// Package usage aggregates session events into fixed-width time buckets so a
// client can chart volume, errors, latency and cost over wall-clock time.
//
// It lives server-side on purpose. An aggregate built inside a client would
// start empty when that client connected, so two operators watching the same
// pod would see different histories of the same traffic — and neither would see
// anything from before they attached. The store is the only place with the whole
// picture, so the arithmetic belongs next to it.
//
// Memory is O(buckets x distinct labels), independent of event volume: mean and
// standard deviation come from running sums (count, sum, sum-of-squares) rather
// than from retained samples, and every bucket is preallocated in a fixed ring.
// Nothing here grows with traffic.
package usage

import (
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
)

const (
	// BucketWidth is the storage resolution. Every window the API offers is a
	// whole multiple of it, and clients fold buckets together for wider views
	// rather than asking the server to pre-aggregate — one storage shape, and a
	// client is free to pick its own on-screen resolution.
	BucketWidth = time.Minute

	// NumBuckets covers the longest window offered (6h). The ring is
	// preallocated at this size for both the all-sessions aggregate and each
	// tracked session.
	NumBuckets = 360

	// MaxWindow is the longest span Snapshot will return.
	MaxWindow = NumBuckets * BucketWidth
)

// defaultMaxSessions bounds how many per-session rings are tracked. Each ring
// is NumBuckets buckets, so this is the knob that bounds worst-case memory.
// Sessions beyond the cap still land in the all-sessions aggregate; only their
// individual breakdown is dropped.
const defaultMaxSessions = 64

// Counts is the per-label tuple accumulated in each bucket.
type Counts struct {
	Requests int64 `json:"requests"`
	Errors   int64 `json:"errors,omitempty"`
	Tokens   int64 `json:"tokens,omitempty"`
	// The four billed token kinds, plus reasoning. Names match
	// pipeline.InferenceExtension exactly: one vocabulary from parser to aggregate
	// to ledger to collector, because aggregate-side synonyms are how two halves
	// of a system come to disagree about what a field means.
	//
	// Tokens above stays as the legacy aggregate for clients written against it.
	// These are additive to the wire, not a replacement.
	//
	// Tokens is NOT always the sum of the fields below. parsercommon.Fill prefers
	// the provider's own total_tokens when one was reported and falls back to
	// summing the split otherwise, so a gateway reporting only a total — prompt and
	// completion absent — yields a non-zero Tokens with every split field at 0. A
	// client normalising a stacked bar against Tokens would be wrong in that case;
	// PresentKinds below is what keeps it legible, since no bits set means nothing
	// reported a breakdown at all, which is a different answer from a breakdown that
	// was genuinely zero.
	//
	// They matter because the kinds price very differently — a cache read is
	// roughly 0.1x uncached input and a cache write roughly 1.25x — so for a
	// long-running agent, whose traffic is overwhelmingly cache reads, the split
	// IS the shape of the bill. One scalar cannot express that.
	InputTokens      int64 `json:"inputTokens,omitempty"`
	CacheReadTokens  int64 `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens int64 `json:"cacheWriteTokens,omitempty"`
	OutputTokens     int64 `json:"outputTokens,omitempty"`
	// ReasoningTokens is a SUBSET of OutputTokens, not a sibling of it: the
	// provider reports how much of what it generated was reasoning. Adding the two
	// double-counts every reasoning token at the output rate, which is the most
	// expensive tier there is.
	ReasoningTokens int64 `json:"reasoningTokens,omitempty"`
	// RefusedTokenRequests counts the requests whose token report was REJECTED as
	// implausible and contributed nothing to any figure above. See plausibleTokenReport for
	// the rule and for why the whole report goes rather than the offending field.
	//
	// It exists because capping cost while leaving tokens unbounded is not a position that
	// survives being stated. Cost is bounded per request twice over
	// (pricing.MaxPlausibleRequestCostMicros for a gateway's own figure,
	// pricing.MaxCostMicros for the unit), and the token fields were read straight from a
	// provider-controlled `int` on the wire with no bound at all — so the same forged
	// response that cannot move the dollar total by more than $10,000 could move the token
	// total by 9.2e18, and both feed the same aggregate that a client renders side by side.
	// A negative figure was accepted too, and subtracted.
	//
	// REFUSED, NOT CLAMPED, matching how an out-of-range cost is handled: a clamp invents a
	// number nobody reported, and 10,000,000 tokens presented as fact is a worse answer than
	// no figure plus a count of what was dropped. The request itself still counts in
	// Requests, and its cost — settled by a different producer, bounded separately — is
	// untouched, because this says nothing about whether the response was billed.
	//
	// A COUNTER, on the same reasoning as PricedRequests: buckets are summed when a client
	// asks for a coarser resolution, and a count survives that where a flag degrades to
	// "somewhere in here". Non-zero here means Tokens and the split are SHORT by whatever
	// those requests really used, which is unknowable by construction.
	RefusedTokenRequests int64 `json:"refusedTokenRequests,omitempty"`
	// PresentKinds is the OR of every folded event's InferenceExtension.PresentKinds:
	// a set bit means at least one response in this bucket actually REPORTED that
	// kind. Bit layout matches parsercommon.Kind (Input=1, CacheRead=2,
	// CacheWrite=4, Output=8, Reasoning=16).
	//
	// Summing would be meaningless, hence the union. It exists because a zero in
	// one of the fields above has two readings — "this traffic wrote no cache" and
	// "nothing here reports cache writes" — and a by-model table showing a blank
	// column cannot be interpreted without knowing which. The per-event flag
	// already carries that distinction; dropping it at the aggregate would throw
	// away the only thing that makes an empty cell readable.
	PresentKinds uint8 `json:"presentKinds,omitempty"`
	// Saturated says that at least one addition into this Counts hit the int64 ceiling and
	// was CLAMPED rather than allowed to wrap. Every number here is then a FLOOR: the real
	// figure is larger, and by an amount nothing in this struct can state.
	//
	// It exists because the alternative was a wrapped total, and a wrapped total is a lie
	// that reads as a fact. No per-request bound can prevent the wrap — for any bound C the
	// sum overflows after ceil(math.MaxInt64/C) requests and nothing bounds the request
	// count (see pricing.MaxCostMicros, which spells out the arithmetic) — so the only place
	// to close it is where the sum is kept, which is Add below. A clamp on its own would
	// merely trade a large negative lie for a large positive one; this field is what makes
	// the clamp honest, and it is the whole reason the clamp is acceptable.
	//
	// A BOOL, NOT A COUNTER, unlike every other disclosure in this struct. A count of
	// saturating additions would depend on how many folds happened, and that depends on the
	// bucket resolution the client asked for — which is the one property this package
	// insists a total must not have (see Totals and UngroupedCostMicros, both summed from
	// the raw buckets for exactly that reason). "This number is a ceiling" is a property of
	// the number and survives any regrouping; "it was clamped four times" is a property of
	// the arithmetic path and does not.
	//
	// OR-ED THROUGH Add, like PresentKinds: folding a saturated bucket into a clean one
	// yields a saturated total, because the total inherits the floor.
	//
	// NOT LOGGED, deliberately. This package has no logger and a log line on the fold path
	// would either flood or be sampled into uselessness; the disclosure travels on the same
	// response as the number it qualifies, which is where an operator reading that number
	// will see it.
	Saturated bool `json:"saturated,omitempty"`
	// CostMicros is millionths of a US dollar. An integer unit keeps bucket
	// addition exact and JSON round-tripping lossless, which float dollars do
	// not; a client divides by 1e6 to display. Zero when nothing here could be
	// priced, which is not the same as "this traffic was free" — the API omits
	// the field entirely in that case rather than asserting $0.
	CostMicros int64 `json:"costMicros,omitempty"`
	// PricedRequests counts the requests that actually produced a cost. Coverage
	// is a counter rather than a flag because buckets are summed when a client
	// asks for a coarser resolution, and because a deployment can price some of
	// its traffic and not the rest: several endpoints, rates known for some.
	//
	// PriceableRequests-minus-PricedRequests is the gap, correct at every
	// resolution, and it is what stops a partial total being presented as a
	// complete one.
	//
	// NOT Requests-minus-PricedRequests, which this comment used to say and which
	// PriceableRequests' own doc thirty lines below explicitly refutes. Requests
	// counts every proxied response — MCP tool calls, health checks, tunnels — none
	// of which can ever carry a price, so that difference never reaches zero and a
	// client obeying it marks every total partial forever. Requests is not the
	// denominator for coverage; PriceableRequests is.
	//
	// It counts an INEXACT figure too — see IncompleteRequests. This counter answers
	// "did anything price this", which a request priced from partial counters
	// truthfully did; withholding it here would answer a question this counter is not
	// asking and would state the same caveat twice in two vocabularies.
	PricedRequests int64 `json:"pricedRequests,omitempty"`
	// IncompleteRequests counts the priced requests whose figure is not EXACT: known-low
	// because a stream died before its output count arrived, or approximate because the
	// gateway reported only a total. costevent.Event.IncompleteReason carries which, per
	// request; this counter is the aggregate's answer to "is this dollar total exact".
	//
	// HOW MANY, not in which way. The two readings are different claims about money — a
	// floor means the real figure is higher, an approximation means it is off in no known
	// direction — and this counter cannot tell them apart because it is one number.
	// Snapshot.IncompleteBy carries the split, keyed on the reason, and its counts sum to
	// this one.
	//
	// A SUBSET of PricedRequests, never a sibling of it. Their dollars are in CostMicros
	// and the requests are in PricedRequests, because both of those are true; the
	// disclosure rides alongside instead of subtracting from either. That is the same
	// posture as priced-versus-priceable: honest by saying more, not by counting less.
	// Subtracting would be an adjustment, and an adjustment invites a client to render
	// the remainder as an exact total, which is the very error this counter exists to
	// prevent.
	//
	// A counter rather than a flag, for the reason PricedRequests is one: buckets are
	// summed when a client asks for a coarser resolution, and a count survives that
	// where a flag would degrade to "somewhere in here".
	//
	// NOT a pricing gap, and deliberately absent from byUnpriced: nothing an operator
	// adds to a rate table would make one of these disappear, so naming it there would
	// point at an entry that already exists.
	IncompleteRequests int64 `json:"incompleteRequests,omitempty"`
	// PriceableRequests counts the requests that COULD be priced — those carrying a
	// model and a non-zero token count.
	//
	// It exists because Requests is the wrong denominator for coverage. Requests
	// counts every proxied response, including MCP tool calls, health checks and any
	// other non-LLM traffic the sidecar handled, while PricedRequests can only ever
	// cover inference. Dividing one by the other made a CORRECTLY configured
	// deployment read "1/10 priced" forever with an empty gap list — a permanent
	// warning with nothing to act on, which trains an operator to ignore the one
	// signal that matters.
	//
	// Priced-versus-priceable is the ratio that answers "is my cost total complete",
	// and it reaches parity when it should.
	//
	// WHAT COUNTS AS PRICEABLE WIDENED, and a client comparing figures across versions
	// should know. It used to require extractable token counts, so a 2xx inference response
	// whose usage the parser could not read was invisible: not priced, not priceable, and
	// absent from UnpricedBy — nine good requests plus one of those read "9/9 priced",
	// parity, while real spend was missing. That is now counted as priceable-but-unpriced
	// and named as a gap, which is the honest answer and also a HIGHER priceable figure
	// for identical traffic. A dashboard tracking the ratio across an upgrade will show
	// coverage appear to drop; nothing got worse, the denominator stopped lying.
	//
	// There is deliberately NO version marker on the wire for this. A field whose meaning
	// is versioned needs every consumer to branch on the version, and the honest reading is
	// the same in both: this is the count of requests that could have carried a price. The
	// change is recorded here, and in the PR that made it, rather than encoded in a
	// compatibility flag nothing would read — which is the shape of defect this package hit
	// four times over.
	PriceableRequests int64 `json:"priceableRequests,omitempty"`
}

// Add accumulates o into c, field by field — except PresentKinds, which is
// OR-ed. It is a set of which token kinds a response reported, so adding two
// buckets' flags would produce a number that is not a bit set at all. Do not
// pattern-match on the `+=` below when adding a field of that shape.
//
// Exported because consumers fold these too — abctl collapses low-volume series
// into an "(other)" band — and an unexported version left them hand-summing the
// fields in another module. That copy silently missed PricedRequests when it was
// added, under a comment explaining that every field had to be carried. One
// summation, in the same file as the struct, is the only way that stays true.
//
// Pointer receiver and mutating, matching how the aggregator accumulates on the
// hot path. For a map value, read-modify-write: `v := m[k]; v.Add(o); m[k] = v`.
//
// EVERY FIELD IS A CHECKED ACCUMULATE. `+=` wrapped, and a wrapped total is the worst
// failure this package has: 1,024 requests at pricing.MaxCostMicros summed to
// -9214364837600034816, which then sat in the durable ledger for its full retention with
// no repair path. pricing.MaxCostMicros' own doc proves no per-request bound can close
// that and names this function as the place that must. It is four lines here because this
// PR made Add the single summation point; every site that used to sum fields by hand now
// delegates, so the guard lands once and cannot be forgotten at a call site.
//
// WHICH FIELDS ARE ACTUALLY AT RISK, in order:
//   - The TOKEN fields. They are read from a provider-reported `int` on the wire, and
//     foldInto now refuses an implausible one — but the refusal is a ceiling per request,
//     not on the sum, and Add is exported so a consumer can hand it anything.
//   - CostMicros. Bounded per request at pricing.MaxPlausibleRequestCostMicros for the
//     modelled path and pricing.MaxCostMicros for a gateway's own cost header, which puts
//     the wrap at ~9.2e8 and 1,024 requests respectively. The second is reachable.
//   - Requests, Errors and the three coverage counters are one per event at the source, so
//     traffic cannot reach 2^63 of them. They are checked anyway: Add is exported, abctl
//     folds arbitrary Counts through it to build its "(other)" band, and a uniform call
//     site is the only kind that cannot be forgotten when a field is added.
func (c *Counts) Add(o Counts) {
	c.addInto(&c.Requests, o.Requests)
	c.addInto(&c.Errors, o.Errors)
	c.addInto(&c.Tokens, o.Tokens)
	c.addInto(&c.CostMicros, o.CostMicros)
	c.addInto(&c.PricedRequests, o.PricedRequests)
	// Summed alongside PricedRequests, never out of it: it is a subset disclosure, not a
	// deduction. See the field's own comment for why the aggregate discloses rather than
	// adjusts.
	c.addInto(&c.IncompleteRequests, o.IncompleteRequests)
	c.addInto(&c.PriceableRequests, o.PriceableRequests)
	c.addInto(&c.InputTokens, o.InputTokens)
	c.addInto(&c.CacheReadTokens, o.CacheReadTokens)
	c.addInto(&c.CacheWriteTokens, o.CacheWriteTokens)
	c.addInto(&c.OutputTokens, o.OutputTokens)
	c.addInto(&c.RefusedTokenRequests, o.RefusedTokenRequests)
	// Summed alongside OutputTokens, never into it: it is a subset of the output
	// the provider already reported, so folding it in would bill it twice.
	c.addInto(&c.ReasoningTokens, o.ReasoningTokens)
	// Union, not sum: PresentKinds is a set of which kinds were reported, so
	// adding two buckets' flags would produce a number that is not a bit set.
	c.PresentKinds |= o.PresentKinds
	// Inherited, not merely OR-ed for symmetry: if o's own total was a floor then any total
	// containing it is a floor too. See the field.
	if o.Saturated {
		c.Saturated = true
	}
}

// addInto accumulates v into *dst, saturating at the int64 bounds, and records on c that
// the figure it produced is no longer a sum.
//
// A METHOD RATHER THAN A CLOSURE over a local flag: this runs once per field per folded
// event on the aggregator's hot path, and a closure capturing a bool escapes to the heap.
//
// The receiver is the same Counts that owns dst in every call above. It is passed
// separately because the point is to write two places — the field and the disclosure — from
// one call, so a saturating add cannot record the clamp and lose the fact that it clamped.
func (c *Counts) addInto(dst *int64, v int64) {
	sum, saturated := addSat(*dst, v)
	*dst = sum
	if saturated {
		c.Saturated = true
	}
}

// addSat is a + b, clamped to the int64 range instead of wrapping, and whether it clamped.
//
// The test is written as `a > math.MaxInt64-b` rather than by inspecting the sign of the
// result, because computing the wrapped sum first and then reasoning about it is signed
// overflow — undefined in most languages and merely unhelpful in Go, where it silently
// produces the very number this function exists to avoid returning.
//
// BOTH DIRECTIONS. No producer in this package can settle a negative cost — costevent
// refuses one and MicrosFromUSD rejects it — so the lower clamp is unreachable through the
// aggregator today. It is here because Add is exported, because "unreachable today" is how
// the wrap arrived in the first place, and because a half-guarded accumulator invites a
// reader to conclude the other half was considered and ruled out.
// maxPlausibleRequestTokens is the largest token count one request could report, per field.
//
// THE SAME NUMBER AND THE SAME REASONING AS pricing's unexported maxPlausibleTokens: the
// largest context window on any path we run is 1,000,000 tokens (the Claude [1m] beta), a
// request bills prompt plus completion, so 2,000,000 covers the worst real call and ten
// million is 5x that. A future window growth cannot turn a legitimate response into a
// refusal.
//
// DERIVED, NOT RESTATED. This was a second literal carrying the same number, with a comment
// recording the duplication as a debt and naming the fix: export one and derive the other,
// in a commit that can touch both packages. This is that commit. pricing.MaxPlausibleTokens
// is now exported for exactly this, and the debt is paid rather than documented — two
// literals that must agree is the shape that let config's retention floor drift from the
// window span it protects, shipping a floor of 7 against a window that opens 8 files.
//
// PER FIELD, NOT PER REPORT. Six fields at the bound is 6e7, which is nowhere near an int64
// and needs no separate sum check; a per-report bound would have to pick between refusing a
// legitimate large prompt and admitting a forged split, and a per-field one refuses neither.
const maxPlausibleRequestTokens = pricing.MaxPlausibleTokens

// plausibleTokenReport reports whether an event's token counters could have come from a real
// inference response.
//
// ALL OR NOTHING, and that is the point. If one figure in the report is impossible then the
// report is not trustworthy, and mixing a believed number with a refused one in the same row
// produces a breakdown that cannot be reconciled against its own total — the client is then
// worse off than with no figures at all. So one bad field refuses the whole set, and
// Counts.RefusedTokenRequests says how many times that happened.
//
// NEGATIVE IS REFUSED TOO, not merely the ceiling. These arrive as `int` decoded from a
// provider's JSON, so a negative is one minus sign away, and a negative token count
// SUBTRACTS from the aggregate — a forged response that makes a real bill look smaller,
// which is the direction an attacker actually wants.
//
// The nil check is the caller's; every call site here has already tested it.
func plausibleTokenReport(inf *pipeline.InferenceExtension) bool {
	for _, n := range [...]int{
		inf.TotalTokens, inf.InputTokens, inf.CacheReadTokens,
		inf.CacheWriteTokens, inf.OutputTokens, inf.ReasoningTokens,
	} {
		if n < 0 || n > maxPlausibleRequestTokens {
			return false
		}
	}
	return true
}

func addSat(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64, true
	}
	if b < 0 && a < math.MinInt64-b {
		return math.MinInt64, true
	}
	return a + b, false
}

// CostSum is a running total of money in micros that clamps instead of wrapping, and
// remembers that it clamped.
//
// IT EXISTS BECAUSE Counts.Add WAS NOT THE ONLY PLACE MONEY IS SUMMED, which is the half
// the accumulation fix got wrong. Counts.addInto routes every field of an aggregate through
// addSat, and the reasoning for that is written out at length on Counts.Saturated and
// pricing.MaxCostMicros — but three money accumulates sat outside it, all of them totals
// DERIVED from Counts rather than fields of one:
//
//	seriesCost              sums CostMicros across a label breakdown
//	Snapshot's ungrouped    the residual, per raw bucket
//	costledger.Fold's       the same residual, per ledger row
//
// Each was a bare `+=` on an int64 that a saturating add had already been chosen for one
// call frame away. Two saturated buckets or two saturated rows — reachable at the ~1,024
// requests pricing.MaxCostMicros documents, and immediately from a hand-edited day file —
// wrapped them.
//
// AND THE RESIDUAL IS THE WORST PLACE FOR IT TO HAPPEN, which is why this is a type rather
// than an exported function. A wrapped residual goes NEGATIVE, and
// Snapshot.SetUngroupedCost reads a negative residual as the series having overshot its own
// total — a condition whose field doc tells the reader that this process is wrong about its
// own arithmetic and to file a bug. So the wrap did not merely produce a wrong number: it
// fabricated a defect report about correct data, and suppressed the real residual while
// doing it. Clamping alone would still be a lie; the flag is what makes it honest, exactly
// as Counts.Saturated is for the fields.
//
// A TYPE, NOT `func AddCostMicros(a, b int64) (int64, bool)`, for the reason given on
// Counts.addInto: the point is that one call writes both the figure and the disclosure, so
// there is no shape in which a caller takes the clamped sum and drops the fact that it
// clamped. An exported saturating add would have been one `_` away from restoring this bug.
type CostSum struct {
	// Micros is the total so far, clamped to the int64 range.
	Micros int64
	// Saturated says Micros is a bound rather than a sum. Callers surface it by setting
	// Counts.Saturated on the totals the residual belongs to: that field already means
	// "read every money figure here as a bound", and a second flag for the same fact
	// would let a client trust one while the other contradicted it.
	Saturated bool
}

// Add accumulates micros into s.
func (s *CostSum) Add(micros int64) {
	sum, saturated := addSat(s.Micros, micros)
	s.Micros = sum
	if saturated {
		s.Saturated = true
	}
}

// Sub subtracts micros from s, which is how a residual is computed: a bucket's total
// minus what its breakdown accounted for.
//
// Separate from Add(-micros) for one input only, and it is the input that made
// SetUngroupedCost need its own guard: math.MinInt64 has no positive counterpart, so
// negating it yields math.MinInt64 again and SUBTRACTING it would add. Reachable only
// from an already-clamped figure, and clamped to the ceiling with the flag set rather
// than being special-cased into exactness — the true answer is out of range in that
// direction whatever s holds, and pretending otherwise for the sub-case where it
// happens to fit would put a branch nothing can test in the middle of the one
// arithmetic in this file that must be obviously right.
func (s *CostSum) Sub(micros int64) {
	if micros == math.MinInt64 {
		s.Micros, s.Saturated = math.MaxInt64, true
		return
	}
	s.Add(-micros)
}

// Bucket is one BucketWidth slice of time, as served to clients.
//
// A bucket with no traffic is still emitted, with zeroed counts. That is
// deliberate: a client rendering a bar chart must be able to distinguish an idle
// minute from a minute that fell off the end of the ring, and inferring absent
// buckets from timestamps is exactly the kind of thing every client would get
// slightly differently.
type Bucket struct {
	At          time.Time `json:"at"`
	Counts                // totals across every label
	LatMeanMs   float64   `json:"latMeanMs,omitempty"`
	LatStdDevMs float64   `json:"latStdDevMs,omitempty"`
	// LatSamples is how many requests in this bucket carried a duration, which
	// is not always Requests: an unmeasured response counts as traffic but not
	// as a latency sample. Folding needs it to weight each bucket by its real
	// sample count, and a client showing a window-wide mean needs it for the
	// same reason.
	LatSamples int64 `json:"latSamples,omitempty"`
	// Series is the requested grouping, keyed by model / status / plugin name.
	// Nil when group=none.
	Series map[string]Counts `json:"series,omitempty"`
}

// bucket is the internal accumulator. It keeps all three groupings at once so
// the group= parameter is a read-time choice: an operator cycling groupings in a
// TUI sees the same history from each angle, instead of each grouping only
// having data from the moment it was first selected.
type bucket struct {
	start time.Time // truncated to BucketWidth; zero means never written
	Counts
	latSum   float64 // milliseconds
	latSumSq float64 // milliseconds squared, for stddev
	// latN counts only the requests that actually carried a duration. Dividing
	// latSum by Requests instead reports a mean diluted by every unmeasured
	// response: one 2s response plus one unmeasured one reported 1s, not 2s.
	latN int64
	// byMethod tallies by the model named on the request — despite the name, which
	// is the one GroupModel shipped under. See GroupModel's godoc: nothing has ever
	// put an A2A or MCP method name in here.
	byMethod map[string]Counts
	// byEndpoint tallies by target host. Bounded by maxLabelsPerBucket like every
	// other label map: Host comes off the request, so its cardinality is set
	// off-host rather than by anything this process controls.
	byEndpoint map[string]Counts
	// bySession tallies by the session the event was recorded under, so ONE
	// snapshot of the all-sessions ring answers for every row of a sessions list
	// instead of costing a request per row.
	//
	// Bounded by maxLabelsPerBucket like every other label map, and here the bound
	// is the load-bearing one: the id is request-derived rather than process-chosen —
	// the reverse proxy takes it from the A2A contextId and falls back to "default"
	// (reverseproxy.inboundSessionID) — so its cardinality is set off-host. A client
	// varying the contextId every turn would otherwise add a retained map entry and a
	// retained string per request, in a ring that frees a slot only a full lap later.
	//
	// Recorded on the per-session rings too, where it is a single-key map and
	// redundant. That is deliberate: a uniform call site in foldInto cannot fall out
	// of step with itself, and one key costs nothing.
	bySession map[string]Counts
	// byAgent tallies by the coding agent that made the request, as
	// pipeline.EventClient.Label reports it ("claude-code/2.1.14").
	//
	// NO INFERENCE GUARD, unlike byMethod. It is folded for every event, so MCP, A2A
	// and tool traffic are attributed here too — which means this axis has a
	// DIFFERENT DENOMINATOR from group=model's, exactly as byEndpoint does. A client
	// putting a per-agent table beside a per-model one will not see the request counts
	// reconcile, and that is correct rather than a bug. Deliberate: "what did this
	// agent cost me" has to include the tool calls it made, not only its LLM turns.
	//
	// Bounded by maxLabelsPerBucket and maxLabelLen like every other label map, and
	// here the bound is load-bearing for the same reason bySession's is: the key is
	// derived from a REQUEST HEADER, so both its length and its cardinality are set
	// off-host. Capped twice on the way in — pipeline.maxClientLen when the header is
	// first retained, then truncateLabel at the addLabel call in foldInto.
	//
	// The value is CLIENT-ASSERTED AND TRIVIALLY SPOOFABLE, so this is a display axis
	// and never a basis for a policy decision; see pipeline.EventClient.
	//
	// An event with no client folds under the reserved "unknown" bucket rather than ""
	// — Label() is nil-safe and answers that — because a blank key renders as a blank
	// row, which reads as a rendering bug rather than as unattributed traffic.
	// "unknown" is NOT an agent name and a consumer must not present it as one. An
	// agent that WAS reported but matched no known name keeps its raw User-Agent
	// instead, so a new coding agent never pools in with untagged traffic.
	byAgent  map[string]Counts
	byStatus map[string]Counts
	byPlugin map[string]Counts
	// byProvenance tallies priced requests by where their figure came from, so a
	// total can disclose how much of it is a gateway's own number versus modelled
	// from a rate table. Like byUnpriced, kept outside the Group machinery: it
	// qualifies the cost total, and a client needs it whichever grouping it asked
	// for.
	byProvenance map[string]Counts
	// byUnpriced tallies endpoint/model pairs that could not be priced. Kept
	// outside the Group machinery on purpose: it is not an alternative view of the
	// same counts but a coverage gap, and a client needs it whichever grouping it
	// asked for.
	byUnpriced map[string]Counts
	// byIncomplete tallies inexact figures by WHICH WAY they are inexact, keyed on
	// costevent.Event.IncompleteReason. Outside the Group machinery for the same
	// reason as the two above: it qualifies the dollar total, so a client needs it
	// whichever grouping it asked for.
	//
	// Counts.IncompleteRequests answers how many; this answers in which way, and the
	// two are different claims about money. A floor (a stream that died before its
	// output count) means the real figure is HIGHER — "at least $X" — while an
	// approximation (a gateway reporting only a total) means it is off in NO KNOWN
	// DIRECTION — "roughly $X". A client that can only see the count has to render
	// both the same way, which is how a permanent property of a gateway comes to read
	// as an incident.
	byIncomplete map[string]Counts
}

// eventCost is one event's settled cost, decoded once per Record and passed to
// each ring's foldInto.
//
// Cost is read from the figure inference-parser settles per response and publishes
// on the session event (see authlib/costevent): it prefers the gateway's own
// post-discount cost header and falls back to pricing the parsed token counters.
// litellm-budget-track consumes that figure to enforce a budget; it settles nothing
// of its own.
//
// This package is NOT rate-free, which this comment previously claimed. WithPricing
// stores a pricing.Resolver, and costOf below resolves a rate for any request that
// carries a model and tokens but arrives with no cost record. So there are two
// sources: a published figure is preferred, a modelled one is the fallback, and the
// two can answer differently about the same request. Collapsing them onto the
// parser's figure alone is later work — until it lands, do not describe cost here as
// coming from a single place.
//
// Traffic neither path could price contributes no cost and is visible as the gap
// between Counts.PricedRequests and Counts.PriceableRequests — NOT against
// Counts.Requests, which counts traffic that could never have carried a price and so
// never reaches parity; Snapshot.UnpricedBy names the endpoint/model pairs involved. See
// docs/superpowers/specs/2026-09-09-pricing-consolidation-design.md.
type eventCost struct {
	micros int64
	// priced is 1 when a cost was found and 0 otherwise, so it sums into
	// Counts.PricedRequests as a coverage count rather than needing a separate
	// branch at every accumulation site.
	priced int64
	// incomplete is 1 when micros is not an exact total. It rides ALONGSIDE priced,
	// which stays 1: the figure exists and belongs in every total it was already in, and
	// only the claim of exactness is withdrawn. See Counts.IncompleteRequests and
	// costevent.Event.Incomplete.
	incomplete int64
	// incompleteReason names WHICH WAY micros is inexact — pricing.ReasonOutputUncounted
	// for a floor, pricing.ReasonSplitUnreported for an approximation — so the aggregate
	// can report the two separately instead of collapsing them into the counter's one
	// bit. Empty for an exact figure, and never empty when incomplete is 1: see
	// unlabelledReason for the case where the producer disclosed the caveat and not the
	// reason.
	incompleteReason string
	// priceable is 1 when the request carried a model and tokens, so it belongs in
	// the coverage denominator whether or not a rate was found.
	priceable int64
	// provenance names where the figure came from, so a reported total can say how
	// much of it is a gateway's own number and how much is modelled from a rate
	// table. Empty for an unpriced request.
	provenance string
	// unpricedKey names the endpoint and model of a request that COULD have been
	// priced but was not, so the gap is nameable instead of merely counted.
	// Empty for a priced request, and empty for traffic that carries no model at
	// all — naming every non-LLM call the proxy handled would bury the real gaps.
	unpricedKey string
}

// costOf settles one event's cost, preferring a published figure and falling back
// to the rate table.
//
// The order is deliberate. A cost event is a figure inference-parser already
// settled — often the gateway's own post-discount number — so it is never
// second-guessed by a model. The resolver covers what the parser did not: a
// pipeline without it, or a request whose response carried no usable figure.
//
// Before this fallback, cost required some plugin to have published a figure, and
// litellm-budget-track was the only thing that did. A deployment running only
// inference-parser reported every request unpriced however many tokens it burned,
// which reads as "this traffic was free" rather than "nothing here priced it". That
// is history on both counts now: the parser settles cost itself, and this resolver
// backs it up when nothing published one.
//
// Runs outside the aggregator's lock: resolution is a read of an immutable table
// and must not hold up the hot path.
func (a *Aggregator) costOf(e *pipeline.SessionEvent) eventCost {
	if ce, ok := costevent.Decode(e); ok {
		prov := ce.Provenance
		if prov == "" {
			// An event from a producer predating the field. It is a settled figure,
			// so the honest label is authoritative-or-modelled-unknown rather than
			// silently claiming either.
			prov = unlabelledLabel
		}
		ec := eventCost{micros: ce.Micros(), priced: 1, priceable: 1, provenance: prov}
		if ce.Incomplete {
			// Disclosed, not deducted. micros, priced and provenance all stand: the
			// figure exists, something priced it, and it came from where provenance
			// says. Only exactness is withdrawn. See costevent.Event.Incomplete for why
			// adjusting any of the three would be the worse answer, and
			// Counts.IncompleteRequests for what a client does with this.
			ec.incomplete = 1
			// The REASON travels with the count, because a floor and an approximation are
			// different claims about money — see bucket.byIncomplete. The producer sends
			// both fields together (costing.Settle sets neither alone), so an empty reason
			// here means an event from a producer predating IncompleteReason; labelled
			// rather than dropped, for the reason unlabelledLabel gives.
			ec.incompleteReason = ce.IncompleteReason
			if ec.incompleteReason == "" {
				ec.incompleteReason = unlabelledLabel
			}
		}
		return ec
	}
	// Only inference traffic can be priced or named. A plain proxied request has no
	// model and no tokens, and is neither.
	if e.Inference == nil || e.Inference.Model == "" {
		return eventCost{}
	}
	key := e.Host + " " + e.Inference.Model
	u := pricing.UsageFromInference(e.Inference)
	if u == (pricing.Usage{}) {
		// A FAILED response carrying no tokens is not a pricing gap: a 5xx, or a
		// denial after the parser ran, reports a model with zero usage. Naming it
		// would advertise a missing rate for a model that may well have one, and
		// adding that entry would never make the row disappear.
		if e.StatusCode >= 400 || e.Phase == pipeline.SessionDenied {
			return eventCost{}
		}
		// A SUCCEEDED one is a coverage gap and must be counted as one. Returning a
		// bare eventCost{} here left the request in Requests and out of
		// PricedRequests, PriceableRequests and UnpricedBy alike — so nine priced
		// requests plus one of these read "9/9 priced", parity, while real spend was
		// missing. That falsifies Counts.PriceableRequests' own promise that the
		// ratio "reaches parity when it should".
		//
		// The dominant instance is a TRUNCATED OPENAI-DIALECT STREAM. OpenAI puts
		// every counter on the final chunk, so a stream that dies mid-body yields no
		// usage at all — not even a floor, because pricing.outputUncounted needs a
		// prompt-side count to be a lower bound OF. Anthropic escapes this only
		// because message_start carries the prompt counts, which is why the
		// incomplete-reason path catches it there and never sees it here. Also
		// reachable on a request/response stream-shape mismatch, since the parser
		// picks its arm from the REQUEST's stream flag.
		//
		// Unpriced, never priced-zero: no figure was settled, so claiming one would
		// report unmeasured traffic as free. Named, because the parser recognised
		// this traffic well enough to report a Model — the information to name the
		// gap was in hand, and this is not the off-allowlist blindness the design
		// declares out of scope, where nothing is recognised at all.
		return eventCost{priceable: 1, unpricedKey: key}
	}
	if a.rates == nil {
		return eventCost{priceable: 1, unpricedKey: key}
	}
	rates, prov := a.rates.Resolve(e.Host, e.Inference.Model, u.PromptTotal())
	if prov == pricing.ProvNone {
		// No rate for this pair at all — the one case an operator fixes by adding a
		// pricing entry, so the one case worth naming.
		return eventCost{priceable: 1, unpricedKey: key}
	}
	micros, ok := pricing.Cost(rates, u)
	if !ok {
		// A rate was found but it does not cover every tier this request used. Still
		// a gap an operator can close, and naming the pair points at the entry to
		// extend rather than to create.
		return eventCost{priceable: 1, unpricedKey: key}
	}
	ec := eventCost{micros: micros, priced: 1, priceable: 1, provenance: prov.String()}
	// The same exactness test the producer applies, on this package's OWN figure. There
	// are two sources of cost here (see the type doc above) and a truncated stream
	// reaching this fallback is priced prompt-only exactly as it would have been by the
	// producer — so leaving the test on one side of the fork would disclose the caveat
	// for one of the two ways a figure can arrive and not the other.
	//
	// The reason is kept, not just the bit: this arm has the actual answer in hand —
	// pricing.IncompleteReason returns which of the two it is — and throwing it away here
	// would make a fallback-priced window unable to say what a producer-priced one can.
	if reason := pricing.IncompleteReason(e.Inference); reason != "" {
		ec.incomplete, ec.incompleteReason = 1, reason
	}
	return ec
}

// unlabelledLabel is the reserved key for a caveat a producer disclosed WITHOUT saying
// which kind it was: an event from a version predating the field that names it.
//
// One spelling for both maps that need one — PricedBy's provenance and IncompleteBy's
// reason — because it is the same situation in both: the producer stated the fact and not
// the label. It cannot collide with a real value; pricing's reasons are hyphenated
// lowercase ("output-uncounted", "split-unreported") and its provenance levels are
// ("authoritative", "configured", "discovered", "bundled").
//
// LABELLED, never dropped, so IncompleteBy's counts sum to Counts.IncompleteRequests. A
// client subtracting the map from the counter to find "the rest" must get zero; silently
// omitting a row would make that difference read as requests whose figures are exact.
const unlabelledLabel = "unlabelled"

// Aggregator is a fixed ring of per-minute buckets. Safe for concurrent use.
//
// Expiry is implicit: a slot is indexed by minutes-since-epoch modulo
// NumBuckets, so a write whose timestamp does not match the slot's recorded
// start has landed on a stale bucket from a previous lap and resets it. No
// sweeper goroutine, no cleanup path. The tradeoff is that a large backwards
// clock jump can reset buckets that were still current; for observability
// counters that is acceptable, and it is preferable to a timer that has to be
// stopped on shutdown.
type Aggregator struct {
	mu       sync.RWMutex
	all      []bucket
	sessions map[string]*sessionRing
	maxSess  int
	now      func() time.Time

	// pending holds request-phase plugin names awaiting their response event,
	// keyed by RequestID. See Record.
	pending map[string]*pendingRequest

	// rates prices requests that carry no cost event. Optional: nil means only
	// published figures count, which is the behaviour before WithPricing existed.
	rates pricing.Resolver
}

// pendingRequest is the request half of one turn: the plugins that ran before the
// response existed, held until the response arrives so they can be attributed to
// the same bucket with the same tokens and latency.
type pendingRequest struct {
	plugins []string
	at      time.Time // for expiry when no response ever arrives
}

// maxPendingRequests bounds the pending map. A request whose response never
// arrives — client disconnect, upstream hang, a proxy restart mid-turn — would
// otherwise leak an entry per turn forever.
//
// Generous relative to real in-flight concurrency for one sidecar, so eviction is
// a backstop rather than something the steady state relies on.
const maxPendingRequests = 4096

// pendingTTL bounds how long a request half waits for its response. Matched to the
// streaming read timeout in the forward proxy: a turn quiet for longer than that
// has been abandoned by the listener too, so its plugins will never be paired.
const pendingTTL = 5 * time.Minute

// sessionRing is one session's buckets plus the last time it was written.
//
// lastSeen exists because the store evicts and expires sessions without telling
// the aggregator. Without reclamation, a pod that churns through session ids
// fills the map to the cap and then refuses every new session forever:
// /v1/sessions would list a live session while /v1/usage?session=<id> returned
// zeroed buckets, which looks exactly like "this session did nothing". Matching
// the store's own cap only delays that. Reusing the coldest ring instead bounds
// memory AND keeps the newest sessions answerable, which is what an operator is
// looking at.
type sessionRing struct {
	buckets  []bucket
	lastSeen time.Time
}

// Option configures an Aggregator.
type Option func(*Aggregator)

// WithClock overrides time.Now, for deterministic tests.
func WithClock(now func() time.Time) Option { return func(a *Aggregator) { a.now = now } }

// WithMaxSessions bounds the number of per-session rings retained. Each ring is
// NumBuckets buckets, so this is the knob that bounds worst-case memory.
//
// n == 0 means track NO per-session rings: /v1/usage?session=... returns zeroed
// buckets, while the all-sessions aggregate keeps counting everything. That is
// the reading the name implies, and the safe one if this is ever wired to
// operator config — a 0 in a ConfigMap should not silently mean "unlimited".
// Pass a negative n to leave the default in place.
func WithMaxSessions(n int) Option {
	return func(a *Aggregator) {
		if n >= 0 {
			a.maxSess = n
		}
	}
}

// WithPricing supplies the rate table used for requests that carry no cost event.
//
// The resolver is long-lived and its table swaps in place, which matters here: the
// aggregator is created once at startup while the pipeline is rebuilt on every
// config reload, so holding the Registry keeps it on current rates without being
// rebuilt itself. See pricing.Registry.
func WithPricing(r pricing.Resolver) Option { return func(a *Aggregator) { a.rates = r } }

// New returns an empty Aggregator.
func New(opts ...Option) *Aggregator {
	a := &Aggregator{
		all:      make([]bucket, NumBuckets),
		sessions: make(map[string]*sessionRing),
		pending:  make(map[string]*pendingRequest),
		maxSess:  defaultMaxSessions,
		now:      time.Now,
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Record folds one event into the aggregate.
//
// Counts and timings come from the response event only: a request event carries no
// status, no duration and no usage, so counting it as traffic would double every
// request and pull the latency mean toward zero. Denials (phase "denied") are
// counted as errors — they are requests that happened and failed, and omitting
// them would make an authentication outage look like a traffic drop.
//
// A request event is not ignored, though. The listener splits plugin invocations
// by phase — the request event carries InvocationPhaseRequest, the response event
// InvocationPhaseResponse — so a plugin that only acts on the request appears in
// neither the response event nor, previously, the by-plugin breakdown. context-guru
// and tool-prune are exactly that shape (WritesRequestBody with a stub OnResponse),
// so `by plugin` silently meant "plugins that ran on the response".
//
// Request-phase plugin names are therefore held by RequestID — the field that
// exists for this pairing — and merged when the paired response arrives, so each
// gets the response's own tokens and latency and one turn counts once. Pairing on
// the id rather than positionally matters because a client can have several
// requests in flight at a time.
func (a *Aggregator) Record(sessionID string, e *pipeline.SessionEvent) {
	if e == nil {
		return
	}

	// A request event with nothing to hold is the common case — Invocations is nil
	// on any plain proxied request — and this runs synchronously inside
	// Store.Append on the request hot path. Checked before the lock so that path
	// stays lock-free: neither guard touches aggregator state, so there is nothing
	// to protect.
	if e.Phase == pipeline.SessionRequest && (e.RequestID == "" || e.Invocations == nil) {
		return
	}

	at := e.At
	if at.IsZero() {
		at = a.now()
	}

	// Decoded before the lock, for the same reason the guards above are: it
	// touches no aggregator state, and foldInto runs up to twice per event (the
	// all-sessions ring and this session's ring), which would otherwise unmarshal
	// the same JSON twice while holding mu.
	//
	// Skipped for request events, which return below without ever reaching
	// foldInto. The cost is only ever published on the response pass, so this
	// would find nothing anyway — but not calling it at all beats calling it and
	// relying on that.
	var ec eventCost
	if e.Phase != pipeline.SessionRequest {
		ec = a.costOf(e)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if e.Phase == pipeline.SessionRequest {
		a.holdRequestPluginsLocked(e, at)
		return
	}
	if e.Phase != pipeline.SessionResponse && e.Phase != pipeline.SessionDenied {
		return
	}

	// Claim the request half, if it is still waiting.
	var requestPlugins []string
	if e.RequestID != "" {
		if p, ok := a.pending[e.RequestID]; ok {
			requestPlugins = p.plugins
			delete(a.pending, e.RequestID)
		}
	}

	t := at.Truncate(BucketWidth)
	a.foldInto(a.all, t, sessionID, e, requestPlugins, ec)

	if ring, ok := a.sessions[sessionID]; ok {
		ring.lastSeen = at
		a.foldInto(ring.buckets, t, sessionID, e, requestPlugins, ec)
		return
	}
	// maxSess == 0 means no per-session rings at all — see WithMaxSessions. The
	// event still counts toward the all-sessions total above.
	if a.maxSess == 0 {
		return
	}
	// At the cap, reclaim the least-recently-written ring rather than refuse the
	// new session. The store expires and evicts sessions without notifying us, so
	// the coldest ring is very likely one the store has already dropped; refusing
	// instead would make every session after the first maxSess unanswerable for
	// the life of the process.
	if len(a.sessions) >= a.maxSess {
		a.evictColdestLocked()
	}
	ring := &sessionRing{buckets: make([]bucket, NumBuckets), lastSeen: at}
	a.sessions[sessionID] = ring
	a.foldInto(ring.buckets, t, sessionID, e, requestPlugins, ec)
}

// holdRequestPluginsLocked stashes a request event's plugin names until its
// response arrives. Caller holds mu.
//
// Nothing is counted here — no requests, no tokens, no latency. The request event
// contributes only the LABELS its plugins need in order to be attributed later.
func (a *Aggregator) holdRequestPluginsLocked(e *pipeline.SessionEvent, at time.Time) {
	if e.RequestID == "" || e.Invocations == nil {
		// Without an id there is nothing to pair against, and pairing positionally
		// misattributes as soon as two requests are in flight — the reason
		// SessionEvent carries RequestID at all.
		//
		// Record checks the same two conditions before taking the lock, so this is
		// normally unreachable; kept so the helper is correct on its own terms
		// rather than relying on its only caller.
		return
	}
	names := invocationPlugins(e.Invocations)
	if len(names) == 0 {
		return
	}
	// Sweep before inserting so a burst of abandoned turns cannot push the map
	// past its bound between sweeps.
	if len(a.pending) >= maxPendingRequests {
		a.expirePendingLocked(at)
	}
	if len(a.pending) >= maxPendingRequests {
		// Still full of live requests: drop this one's labels rather than grow
		// without bound. The response will still be counted, just without its
		// request-phase plugins — losing a label is preferable to unbounded memory
		// on the synchronous append path.
		return
	}
	a.pending[e.RequestID] = &pendingRequest{plugins: names, at: at}
}

// expirePendingLocked drops request halves whose response never arrived. Caller
// holds mu.
func (a *Aggregator) expirePendingLocked(now time.Time) {
	for id, p := range a.pending {
		if now.Sub(p.at) > pendingTTL {
			delete(a.pending, id)
		}
	}
}

// invocationPlugins returns the distinct plugin names in an Invocations set.
//
// Deduped because one plugin can append several invocations to a single pass, and
// counting it twice would inflate its share of a stacked bar.
func invocationPlugins(inv *pipeline.Invocations) []string {
	if inv == nil {
		return nil
	}
	seen := make(map[string]bool, len(inv.Outbound)+len(inv.Inbound))
	var out []string
	for _, list := range [][]pipeline.Invocation{inv.Outbound, inv.Inbound} {
		for _, iv := range list {
			if iv.Plugin == "" || seen[iv.Plugin] {
				continue
			}
			seen[iv.Plugin] = true
			out = append(out, iv.Plugin)
		}
	}
	return out
}

// evictColdestLocked drops the ring with the oldest lastSeen. Caller holds mu.
//
// Linear scan rather than a heap: maxSess is a few dozen, this runs only when the
// map is full and a genuinely new session arrives, and a heap would need
// maintaining on every write instead.
func (a *Aggregator) evictColdestLocked() {
	var coldestID string
	var coldest time.Time
	for id, r := range a.sessions {
		if coldestID == "" || r.lastSeen.Before(coldest) {
			coldestID, coldest = id, r.lastSeen
		}
	}
	if coldestID != "" {
		delete(a.sessions, coldestID)
	}
}

// foldInto accumulates one event into one ring.
//
// sessionID is passed rather than derived because the ring itself does not know
// which session it belongs to: a.all is shared and a sessionRing holds only
// buckets. Both call sites pass the same id, which is what lets the bySession
// label be recorded uniformly instead of only on the all-sessions ring.
func (a *Aggregator) foldInto(ring []bucket, t time.Time, sessionID string, e *pipeline.SessionEvent, requestPlugins []string, ec eventCost) {
	b := &ring[slot(t)]
	if !b.start.Equal(t) {
		*b = bucket{start: t} // stale lap: reset rather than accumulate onto old data
	}

	var tokens int64
	var model string
	// split is read from the wire event's own split counters rather than derived
	// from tokens: the parser is the only component that knows the breakdown, and
	// there is no way to recover it from the total afterwards.
	var split Counts
	// refusedTokens is 1 when the report was rejected as implausible, so the drop is
	// counted rather than silent. See Counts.RefusedTokenRequests.
	var refusedTokens int64
	if e.Inference != nil {
		model = e.Inference.Model
		if plausibleTokenReport(e.Inference) {
			tokens = int64(e.Inference.TotalTokens)
			split = Counts{
				InputTokens:      int64(e.Inference.InputTokens),
				CacheReadTokens:  int64(e.Inference.CacheReadTokens),
				CacheWriteTokens: int64(e.Inference.CacheWriteTokens),
				OutputTokens:     int64(e.Inference.OutputTokens),
				ReasoningTokens:  int64(e.Inference.ReasoningTokens),
				PresentKinds:     e.Inference.PresentKinds,
			}
		} else {
			// REFUSED WHOLE, including PresentKinds. Those bits assert "the provider reported
			// these kinds", and this branch is the one where that report is not believed; a
			// breakdown flagged as present with every figure dropped would be the worst of both
			// answers. The model is still carried: the request happened, and which model it
			// named is a label rather than a number.
			refusedTokens = 1
		}
	}

	one := Counts{
		Requests:             1,
		Tokens:               tokens,
		CostMicros:           ec.micros,
		PricedRequests:       ec.priced,
		IncompleteRequests:   ec.incomplete,
		PriceableRequests:    ec.priceable,
		RefusedTokenRequests: refusedTokens,
	}
	// The split is CARRIED rather than re-enumerated. Add is the one summation over
	// Counts' fields, for the reason its own doc gives — the copy that hand-summed them
	// silently missed PricedRequests when it was added — and re-listing six of them here
	// was the same exposure at the same distance: a field added to Counts and wired into
	// Add would still have been dropped on the floor by this literal, and nothing would
	// have failed.
	//
	// Behaviour-identical, not merely equivalent-looking: split is only ever built from
	// the token counters above, so every other field is zero on one side of Add's `+=`,
	// and PresentKinds is `0 | x`. Errors is set below and so is unaffected by the order.
	one.Add(split)
	if ec.unpricedKey != "" {
		addLabel(&b.byUnpriced, ringLabel(ec.unpricedKey), Counts{Requests: 1})
	}
	if ec.provenance != "" {
		addLabel(&b.byProvenance, ringLabel(ec.provenance), Counts{Requests: 1})
	}
	// Keyed on the reason, counted per request: Counts.IncompleteRequests above says how
	// many figures are inexact, and this says in which way — "at least $X" versus "roughly
	// $X", which are different claims about money. Non-empty exactly when incomplete is 1
	// (costOf labels an unlabelled caveat rather than dropping it), so these counts sum to
	// IncompleteRequests.
	if ec.incompleteReason != "" {
		addLabel(&b.byIncomplete, ringLabel(ec.incompleteReason), Counts{Requests: 1})
	}
	if e.StatusCode >= 400 || e.Phase == pipeline.SessionDenied {
		one.Errors = 1
	}
	b.Counts.Add(one)

	// Latency: only from events that actually carry one. A zero duration is
	// "not measured", not "instant", and folding it in would drag the mean down.
	if ms := float64(e.Duration.Milliseconds()); ms > 0 {
		b.latSum += ms
		b.latSumSq += ms * ms
		b.latN++
	}

	if model != "" {
		addLabel(&b.byMethod, ringLabel(model), one)
	}
	// Recorded on whichever ring is being folded, including the per-session one
	// where it is redundant — a uniform call site beats a conditional, and the
	// per-session map has exactly one key so it costs nothing.
	//
	// Empty is skipped for the same reason Host is below: an unattributed event
	// would render as a blank row, which reads as a bug rather than as missing
	// attribution.
	//
	// What a key MEANS is the listener's business, not this package's. While
	// several concurrent agents are recorded under one session id (#949), their
	// spend lands in one entry and a per-session figure is a per-id figure — a
	// caveat about the current session-bucketing behaviour, not about this axis.
	if sessionID != "" {
		addLabel(&b.bySession, ringLabel(sessionID), one)
	}
	// Guarded on non-empty: Host is unset when the listener did not populate it,
	// and an "" key renders as a blank row in a breakdown table, which reads as a
	// bug rather than as missing data.
	if e.Host != "" {
		addLabel(&b.byEndpoint, ringLabel(e.Host), one)
	}
	// UNCONDITIONAL, where byMethod and byEndpoint are guarded on a non-empty value.
	// Label() is nil-safe and answers "unknown" for an event that carried no client,
	// so there is no empty key to guard against — and folding unconditionally is what
	// makes this axis's series sum to the bucket total. That is also what gives it a
	// different denominator from group=model's; see byAgent.
	addLabel(&b.byAgent, ringLabel(e.Client.Label()), one)
	if e.StatusCode > 0 {
		addLabel(&b.byStatus, strconv.Itoa(e.StatusCode), one)
	} else if e.Phase == pipeline.SessionDenied {
		addLabel(&b.byStatus, "denied", one)
	}
	// Per-plugin attribution counts the request once per plugin that ran, so
	// these sub-totals intentionally sum to more than Requests when several
	// plugins touched one message. Tokens are attributed whole to each plugin
	// for the same reason: there is no defensible way to split one response's
	// usage between the plugins that observed it.
	//
	// CostMicros and PricedRequests are duplicated the same way, and both are
	// exposed under group=plugin. Summing cost across the per-plugin series
	// therefore over-reports dollars by the number of plugins that touched each
	// turn — a worse error than over-reporting tokens, because it reads as spend.
	// Use Totals for any dollar figure; the per-plugin values answer "what did
	// traffic this plugin saw cost", not "what did this plugin cost".
	// Invocations is a POINTER and is nil whenever no plugin appended a record —
	// which is the common case for a plain proxied response. Dereferencing it
	// unguarded panics inside Store.Append, i.e. on the request hot path.
	// Response-phase plugins from this event, plus the request-phase plugins held
	// from its paired request event. Deduped across the two halves: a plugin that
	// ran in both phases is still one plugin that touched one turn.
	seen := make(map[string]bool, 4)
	for _, name := range invocationPlugins(e.Invocations) {
		seen[name] = true
		addLabel(&b.byPlugin, ringLabel(name), one)
	}
	for _, name := range requestPlugins {
		if seen[name] {
			continue
		}
		addLabel(&b.byPlugin, ringLabel(name), one)
	}
}

// maxLabelsPerBucket caps distinct keys in one bucket's grouping map.
//
// The model name comes off the request body, so its cardinality is set by
// whatever the client sends, not by anything this process controls. Unbounded, a
// caller varying the model string every request would add a retained map entry
// and a retained string per request, in a ring that only frees a slot when it is
// reused a full lap later — 6h at one minute. That is memory growth driven from
// off-host, on the synchronous session-append path.
//
// 64 is well above any real deployment: a pod talks to a handful of models, a
// few status codes and its own plugin list. Past the cap, further keys fold into
// overflowLabel so the totals stay correct and the excess is visible rather than
// silently dropped.
const maxLabelsPerBucket = 64

// overflowLabel collects everything past maxLabelsPerBucket. Named rather than
// dropped so a chart's segments still sum to the bucket total, and so an
// operator can see that cardinality was capped instead of wondering why a model
// is missing.
const overflowLabel = "(other)"

// maxLabelLen bounds one retained label. The model name is request-controlled, so
// without this a caller could park a megabyte of string in a bucket that lives
// for a full ring lap. Long enough for any real model id, including provider
// prefixes and dated suffixes.
const maxLabelLen = 96

// truncateLabel caps a label at maxLabelLen BYTES, cutting on a rune boundary.
//
// BYTES, because that is what bounds the memory a label occupies and the line it becomes in
// the ledger; RUNE BOUNDARY, because a byte cut breaks the very byte cap it enforces. The cut
// used to be s[:maxLabelLen], which can split a multi-byte sequence and leave an invalid
// trailing fragment — and encoding/json then expands each invalid byte into a 3-byte U+FFFD,
// so a 121-byte label cut at 96 SERIALISED AT 100 BYTES (measured in costledger, where the
// same defect was fixed first). The invalid fragment is the smaller problem; the cap silently
// not holding is the reason this is a fix rather than tidying.
//
// Walking back off continuation bytes shortens the label by at most three bytes, which no
// real model id notices.
func truncateLabel(s string) string {
	if len(s) <= maxLabelLen {
		return s
	}
	cut := maxLabelLen
	// At most three steps: no UTF-8 sequence is longer than four bytes.
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// ringLabel prepares one string for a bucket's label map: sanitised, then capped.
//
// THE ORDER MATTERS, for the reason costledger.rowLabel gives: sanitising can triple a
// string's length — every replaced byte becomes a 3-byte U+FFFD — so capping first would let
// 96 control bytes become 288 and break the bound. Capping last is exact now that
// truncateLabel cuts on a rune boundary.
func ringLabel(s string) string {
	return truncateLabel(sanitizeLabel(s))
}

// sanitizeLabel replaces C0 controls, DEL, C1 controls and invalid UTF-8 with U+FFFD.
//
// THE RING DID NOT SANITISE AT ALL, and that was the larger half of this gap. The model comes
// off the parsed request body and the endpoint is the host the workload asked for, so
// GET /v1/usage served a model name containing an ANSI escape straight out of memory — while
// the ledger's copy of the very same label was clean, because costledger sanitises on write.
// Two surfaces, one request, different bytes: group=model showed the label as two series, and
// only the surface nobody had fixed could reposition an operator's cursor. CWE-150.
//
// REPLACED, NOT DROPPED, so tampering stays visible rather than collapsing into a
// plausible-looking label: "m\x1b[31mx" reads as "m�[31mx" rather than as "m[31mx",
// which nobody would question.
//
// C1 IS INCLUDED — U+0080–U+009F. U+009B is the single-character CSI, and a terminal decoding
// UTF-8 acts on it exactly as on ESC [, so "2J" clears the pane with no ESC byte in the
// label at all. C1 encodes as 0xC2 0x80–0xC2 0x9F, so nothing in it is below 0x20 and a byte
// scan steps straight past it — which is why the scan below decodes runes.
//
// FOURTH COPY OF A FIVE-LINE RULE, AND THE LAYERING IS WHY:
//
//   - pipeline.sanitizeUA is PRIMARY for the Agent label, at the point a User-Agent header
//     becomes a value, so it already covers this package's byAgent.
//   - costledger.sanitizeLabel is PRIMARY for the durable row — the copy in front of a file
//     retained for retentionDays that cannot be edited afterwards.
//   - THIS one is primary for the RING's byMethod and byEndpoint, which reach /v1/usage
//     without passing through either of the other two.
//   - abctl's tui.sanitizeLabel is a render-time copy in a main module this library must not
//     import, and still filters C0 and DEL only.
//
// Neither of the first two is reachable from here: pipeline's is unexported and costledger
// IMPORTS this package, so referencing it would invert the layering into a cycle. A shared
// leaf package is the real fix and cannot be done from this side alone — migrating one caller
// to it while the other two stay put makes five copies rather than one. So: copied, with the
// rule stated identically, and a change to any of them belongs in all of them.
func sanitizeLabel(s string) string {
	if !hasControlRunes(s) {
		// The overwhelmingly common case, and no allocation for it: this runs on the fold path,
		// once per label per event, under the aggregator's write lock.
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if isControlRune(r) || (r == utf8.RuneError && size == 1) {
			b.WriteRune('�')
			i += size
			continue
		}
		b.WriteString(s[i : i+size])
		i += size
	}
	return b.String()
}

// hasControlRunes reports whether s carries anything sanitizeLabel would replace.
//
// A RUNE scan for the reason in sanitizeLabel: a byte scan is exact for C0 and DEL and blind
// to C1. An INVALID byte reports true (RuneError at size 1 is the decoder saying "this is not
// UTF-8"); a legitimately encoded U+FFFD does not, because it decodes at size 3, so an
// already-sanitised label does not read as still hostile.
func hasControlRunes(s string) bool {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if isControlRune(r) || (r == utf8.RuneError && size == 1) {
			return true
		}
		i += size
	}
	return false
}

// isControlRune is the shared rule: C0, DEL, C1, and the bidi and zero-width runes that
// rewrite or hide the text around them. Identical to costledger.isControlRune and pipeline's,
// deliberately — see sanitizeLabel.
//
// THE THREE COPIES MOVE TOGETHER. This one is the /v1/usage serving path, so a rune that gets
// past it reaches every client of that endpoint whatever the other two do — which is the
// reason the identical-rule rule exists rather than being tidiness. See
// pipeline.isControlRune for why bidi and zero-width belong in the same set as C1.
func isControlRune(r rune) bool {
	if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
		return true
	}
	switch r {
	case // Bidi overrides and isolates: reorder the glyphs around them.
		'\u202a', '\u202b', '\u202c', '\u202d', '\u202e',
		'\u2066', '\u2067', '\u2068', '\u2069',
		// Zero-width: make two distinct labels render identically.
		'\u200b', '\u200c', '\u200d', '\u2060', '\ufeff':
		return true
	}
	return false
}

func addLabel(m *map[string]Counts, key string, c Counts) {
	if *m == nil {
		*m = make(map[string]Counts, 4)
	}
	// Reserve the last slot for overflowLabel: switching to it only once the map
	// is already full would make the overflow key itself the (cap+1)th entry.
	if _, ok := (*m)[key]; !ok && len(*m) >= maxLabelsPerBucket-1 {
		key = overflowLabel
	}
	cur := (*m)[key]
	cur.Add(c)
	(*m)[key] = cur
}

func slot(t time.Time) int {
	s := int(t.Unix()/int64(BucketWidth/time.Second)) % NumBuckets
	if s < 0 {
		s += NumBuckets
	}
	return s
}
