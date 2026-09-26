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
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rossoctl/cortex/core/costevent"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/pricing"
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
	//
	// THE SUBSET RELATION IS NOT ENFORCED HERE, which is a decision rather than an
	// omission. plausibleTokenReport screens for negatives and an implausible ceiling
	// but not for ReasoningTokens > OutputTokens, so a provider reporting a
	// contradictory pair is stored as it reported it — and `abctl cost`'s token line
	// and the detail pane both print it, which is the only way a reader notices the
	// provider bug. Clamping at ingest would make every surface agree on a number
	// nobody measured.
	//
	// ApportionReasoning DOES clamp what it derives, because a bar drawn longer than
	// its parent's is a containment claim the layout makes rather than one it relays.
	// Numbers stay faithful; geometry is not allowed to lie.
	ReasoningTokens int64 `json:"reasoningTokens,omitempty"`
	// RefusedTokenRequests counts the requests whose token report was REJECTED as
	// implausible and contributed nothing to any figure above. See plausibleTokenReport for
	// the rule and for why the whole report goes rather than the offending field.
	//
	// It exists because capping cost while leaving tokens unbounded is not a position that
	// survives being stated. Cost is bounded per request twice over
	// (pricing.MaxPlausibleRequestCostMicros for a gateway's own figure, pricing.MaxCostMicros
	// for the unit) while the token fields are read straight from a provider-controlled `int`
	// on the wire — so unbounded, the same forged response that cannot move the dollar total
	// by more than $10,000 moves the token total by 9.2e18, or moves it DOWN with a negative,
	// and both feed the same aggregate a client renders side by side.
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
	// saturating additions would depend on how many folds happened, which depends on the
	// bucket resolution the client asked for — the one property this package insists a total
	// must not have (see Totals and UngroupedCostMicros, both summed from the raw buckets for
	// exactly that reason). "This number is a ceiling" survives any regrouping; "it was
	// clamped four times" does not.
	//
	// OR-ED THROUGH Add, like PresentKinds: folding a saturated bucket into a clean one
	// yields a saturated total, because the total inherits the floor.
	//
	// NOT LOGGED, deliberately: this package has no logger, and the disclosure travels on the
	// same response as the number it qualifies.
	Saturated bool `json:"saturated,omitempty"`
	// CostMicros is millionths of a US dollar. An integer unit keeps bucket
	// addition exact and JSON round-tripping lossless, which float dollars do
	// not; a client divides by 1e6 to display. Zero when nothing here could be
	// priced, which is not the same as "this traffic was free" — the API omits
	// the field entirely in that case rather than asserting $0.
	CostMicros int64 `json:"costMicros,omitempty"`
	// The modelled cost of each token tier, summed over the requests in this bucket.
	//
	// ADJACENT TO CostMicros, not filed with the token counters they parallel, because the
	// distinction that matters here is not "tokens versus money" but AUTHORITATIVE VERSUS
	// MODELLED. CostMicros may be a gateway's own post-discount figure; these four are
	// always the rate table's, so they need NOT sum to it and a reader who assumes they do
	// is wrong. Put beside the field they qualify, that is visible in one glance instead of
	// inferred from a field name.
	//
	// USED AS A RATIO, never as a total — see ApportionTiers, which is the only thing that
	// should read them. All four zero means no modelled split reached this bucket, which is
	// NOT a claim that the traffic was free.
	//
	// Reasoning is deliberately absent: it is a subset of output, not a fifth tier, so a
	// field here would double-count. It stays in the token counters below.
	InputCostMicros      int64 `json:"inputCostMicros,omitempty"`
	CacheWriteCostMicros int64 `json:"cacheWriteCostMicros,omitempty"`
	CacheReadCostMicros  int64 `json:"cacheReadCostMicros,omitempty"`
	OutputCostMicros     int64 `json:"outputCostMicros,omitempty"`
	// AvoidedMicros is cost that was NOT INCURRED — tool-prune's removed prompt tokens
	// priced at the tier they would have landed in — in the same unit as CostMicros.
	//
	// A COUNTERFACTUAL SITTING BESIDE A MEASUREMENT, and the adjacency is deliberate: the
	// invariant on costevent.Event.Avoided forbids adding any of this to spend, to a budget
	// or to a usage total, and a reader who finds CostMicros finds that rule in the same
	// glance. Placing it further down the struct would hide the one thing a new consumer
	// must know. TestAggregator_TotalsAreInvariantToAvoidedCost holds the line.
	//
	// APPLIED savings only. costevent.Event.TotalAvoidedUSD skips the projected ones and
	// this counter inherits that: observe-mode figures are money that WAS spent, and a
	// total mixing them would claim a saving for every byte still on the wire.
	//
	// INDEPENDENT OF PRICEDNESS, which is why it does not travel with the cost figure. A
	// request whose response could not be priced still had tokens removed from its prompt,
	// and costevent.Record's own doc names that as the interesting case; deriving this from
	// the priced record would silently drop it.
	//
	// ESTIMATED, usually. Saving.Estimated marks a figure derived from a bytes-to-tokens
	// ratio rather than a tokenizer, and the per-request flag does not survive summation —
	// so a client must present this as approximate unconditionally rather than inferring
	// exactness from its absence here.
	//
	// GROSS, NOT NET, which is the caveat most likely to be dropped on the way to a screen.
	// tool-prune's own doc is explicit: changing the remove list re-writes the cached prompt
	// prefix at the cache-WRITE rate while the recurring saving accrues at the cache-READ
	// rate, tens of requests apart, and nothing subtracts the re-warm from these dollars. A
	// short window just after a config change therefore reads optimistically; a long steady
	// one converges. See docs/tool-prune-plugin.md, "The figure is gross, not net".
	//
	// SUMMABLE BECAUSE IT IS DOLLARS. The same doc refuses to publish one "tokens saved"
	// figure, because prompt tiers differ by up to 12.5x and a single token count invites
	// multiplying by one rate. That objection does not apply here and its absence is the
	// reason this field is money rather than tokens: each saving was priced at the tier it
	// actually came out of BEFORE reaching this counter, so the sum is tier-correct by
	// construction. A tokens-avoided aggregate would not be, and is deliberately not offered.
	AvoidedMicros int64 `json:"avoidedMicros,omitempty"`
	// PricedRequests counts the requests that actually produced a cost. Coverage
	// is a counter rather than a flag because buckets are summed when a client
	// asks for a coarser resolution, and because a deployment can price some of
	// its traffic and not the rest: several endpoints, rates known for some.
	//
	// PriceableRequests-minus-PricedRequests is the gap, correct at every
	// resolution, and it is what stops a partial total being presented as a
	// complete one.
	//
	// NOT Requests-minus-PricedRequests. Requests counts every proxied response — MCP
	// tool calls, health checks, tunnels — none of which can ever carry a price, so
	// that difference never reaches zero and a client obeying it marks every total
	// partial forever. Requests is not the denominator for coverage;
	// PriceableRequests is.
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
	// A SUBSET of PricedRequests, never a sibling of it. Their dollars are in CostMicros and
	// the requests are in PricedRequests, because both of those are true; the disclosure
	// rides alongside instead of subtracting from either — honest by saying more, not by
	// counting less. Subtracting would be an adjustment, and an adjustment invites a client
	// to render the remainder as an exact total, which is the error this counter exists to
	// prevent.
	//
	// A counter rather than a flag, for the reason PricedRequests is one: buckets are summed
	// when a client asks for a coarser resolution, and a count survives that where a flag
	// degrades to "somewhere in here".
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
	// WHAT COUNTS AS PRICEABLE WIDENED, and a client comparing figures across versions should
	// know. Requiring extractable token counts made a 2xx inference response whose usage the
	// parser could not read invisible: not priced, not priceable, and absent from UnpricedBy —
	// nine good requests plus one of those read "9/9 priced", parity, while real spend was
	// missing. It is now counted as priceable-but-unpriced and named as a gap, which is a
	// HIGHER priceable figure for identical traffic: a dashboard tracking the ratio across an
	// upgrade sees coverage appear to drop, and nothing got worse.
	//
	// There is deliberately NO version marker on the wire for it. A field whose meaning is
	// versioned needs every consumer to branch on the version, and the honest reading is the
	// same in both: this is the count of requests that could have carried a price.
	PriceableRequests int64 `json:"priceableRequests,omitempty"`
}

// The PresentKinds bits, one per token kind a provider can report.
//
// EXPORTED, AND HERE, because PresentKinds is where the layout is documented and this is the
// package every consumer of it already imports. The authority is
// core/plugins/internal/parsercommon.Kind, which cannot be imported from outside
// core/plugins — so before these existed, every reader spelled the bits itself: abctl's
// `cost` command, abctl's spend strip, and this package's own tests as a bare `1 | 8`. Three
// uncoordinated copies of a wire format, with nothing comparing them.
//
// TestKindBits_MatchTheParserThatProducesThem, which lives with parsercommon because that is
// the one place both sets are visible, is what pins these to it.
//
// WIRE FORMAT: they are serialised in PresentKinds and stored in the cost ledger, so they
// cannot be renumbered whatever any Go identifier is called.
const (
	KindInput uint8 = 1 << iota
	KindCacheRead
	KindCacheWrite
	KindOutput
	KindReasoning
)

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
// EVERY FIELD IS A CHECKED ACCUMULATE. A bare `+=` wraps, and a wrapped total is the worst
// failure this package has: 1,024 requests at pricing.MaxCostMicros sum to
// -9214364837600034816, which then sits in the durable ledger for its full retention with no
// repair path. pricing.MaxCostMicros' own doc proves no per-request bound can close that and
// names this function as the place that must. Add is the single summation point, so the guard
// lands once and cannot be forgotten at a call site.
//
// WHICH FIELDS ARE ACTUALLY AT RISK, in order:
//   - The TOKEN fields. They are read from a provider-reported `int` on the wire, and
//     foldInto refuses an implausible one — but the refusal is a ceiling per request, not on
//     the sum, and Add is exported so a consumer can hand it anything.
//   - CostMicros. Bounded per request at pricing.MaxPlausibleRequestCostMicros for the
//     modelled path and pricing.MaxCostMicros for a gateway's own cost header, which puts the
//     wrap at ~9.2e8 and 1,024 requests respectively. The second is reachable.
//   - Requests, Errors and the three coverage counters are one per event at the source, so
//     traffic cannot reach 2^63 of them. They are checked anyway: Add is exported, abctl folds
//     arbitrary Counts through it to build its "(other)" band, and a uniform call site is the
//     only kind that cannot be forgotten when a field is added.
func (c *Counts) Add(o Counts) {
	c.addInto(&c.Requests, o.Requests)
	c.addInto(&c.Errors, o.Errors)
	c.addInto(&c.Tokens, o.Tokens)
	c.addInto(&c.CostMicros, o.CostMicros)
	// The tier split, checked like everything else. A saturated tier matters even though the
	// figures are only ever a ratio: a clamped numerator against an unclamped denominator
	// silently changes the SHAPE of the split, which is the one thing these fields carry.
	c.addInto(&c.InputCostMicros, o.InputCostMicros)
	c.addInto(&c.CacheWriteCostMicros, o.CacheWriteCostMicros)
	c.addInto(&c.CacheReadCostMicros, o.CacheReadCostMicros)
	c.addInto(&c.OutputCostMicros, o.OutputCostMicros)
	// Its own accumulate, never folded into the line above. Both are money-shaped and only
	// one is money; see the field. Checked like the rest because a saving is modelled from
	// the same table as a cost and inherits its range.
	c.addInto(&c.AvoidedMicros, o.AvoidedMicros)
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

// maxPlausibleRequestTokens is the largest token count one request could report, per field.
//
// THE SAME NUMBER AND THE SAME REASONING AS pricing's unexported maxPlausibleTokens: the
// largest context window on any path we run is 1,000,000 tokens (the Claude [1m] beta), a
// request bills prompt plus completion, so 2,000,000 covers the worst real call and ten
// million is 5x that. A future window growth cannot turn a legitimate response into a
// refusal.
//
// DERIVED, NOT RESTATED. pricing.MaxPlausibleTokens is exported for exactly this: two
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

// addSat is a + b, clamped to the int64 range instead of wrapping, and whether it clamped.
//
// The test is written as `a > math.MaxInt64-b` rather than by inspecting the sign of the
// result, because computing the wrapped sum first and then reasoning about it is signed
// overflow — undefined in most languages and merely unhelpful in Go, where it silently
// produces the very number this function exists to avoid returning.
//
// BOTH DIRECTIONS. No producer in this package can settle a negative cost — costevent refuses
// one and MicrosFromUSD rejects it — so the lower clamp is unreachable through the aggregator
// today. It is here because Add is exported, because "unreachable today" is how the wrap
// arrived in the first place, and because a half-guarded accumulator invites a reader to
// conclude the other half was considered and ruled out.
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
// IT EXISTS BECAUSE Counts.Add IS NOT THE ONLY PLACE MONEY IS SUMMED. Counts.addInto routes
// every field of an aggregate through addSat — the reasoning is on Counts.Saturated and
// pricing.MaxCostMicros — but three money accumulates sit outside it, all of them totals
// DERIVED from Counts rather than fields of one:
//
//	seriesCost              sums CostMicros across a label breakdown
//	Snapshot's ungrouped    the residual, per raw bucket
//	costledger.Fold's       the same residual, per ledger row
//
// Each was a bare `+=` on an int64 that a saturating add had already been chosen for one call
// frame away. Two saturated buckets or two saturated rows wrap them — reachable at the ~1,024
// requests pricing.MaxCostMicros documents, and immediately from a hand-edited day file.
//
// AND THE RESIDUAL IS THE WORST PLACE FOR IT TO HAPPEN, which is why this is a type rather
// than an exported function. A wrapped residual goes NEGATIVE, and Snapshot.SetUngroupedCost
// reads a negative residual as the series having overshot its own total — a condition whose
// field doc tells the reader this process is wrong about its own arithmetic and to file a bug.
// So the wrap does not merely produce a wrong number: it fabricates a defect report about
// correct data and suppresses the real residual while doing it. Clamping alone would still be
// a lie; the flag is what makes it honest, exactly as Counts.Saturated is for the fields.
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

// bucket is the internal accumulator. It keeps all four groupings at once so
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
	// Bounded by maxLabelsPerBucket and maxLabelLen like every other label map, and here
	// the bound is load-bearing for the same reason bySession's is: the key is derived from
	// a REQUEST HEADER, so both its length and its cardinality are set off-host. Capped
	// twice on the way in — pipeline.maxClientLen when the header is first retained, then
	// truncateLabel at the addLabel call in foldInto.
	//
	// The value is CLIENT-ASSERTED AND TRIVIALLY SPOOFABLE, so this is a display axis and
	// never a basis for a policy decision; see pipeline.EventClient.
	//
	// An event with no client folds under the reserved "unknown" bucket rather than "" —
	// Label() is nil-safe and answers that — because a blank key renders as a blank row,
	// which reads as a rendering bug rather than as unattributed traffic. "unknown" is NOT
	// an agent name. An agent that WAS reported but matched no known name keeps its raw
	// User-Agent instead, so a new coding agent never pools in with untagged traffic.
	byAgent  map[string]Counts
	byStatus map[string]Counts
	byPlugin map[string]Counts
	byHost   map[string]Counts
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
// on the session event (see core/costevent): it prefers the gateway's own
// post-discount cost header and falls back to pricing the parsed token counters.
// litellm-budget-track consumes that figure to enforce a budget; it settles nothing
// of its own.
//
// THIS PACKAGE IS NOT RATE-FREE. WithPricing stores a pricing.Resolver, and costOf below
// resolves a rate for any request that carries a model and tokens but arrives with no cost
// record. So there are two sources: a published figure is preferred, a modelled one is the
// fallback, and the two can answer differently about the same request. Collapsing them onto
// the parser's figure alone is later work — until it lands, do not describe cost here as
// coming from a single place.
//
// Traffic neither path could price contributes no cost and is visible as the gap between
// Counts.PricedRequests and Counts.PriceableRequests — NOT against Counts.Requests, which
// counts traffic that could never have carried a price and so never reaches parity;
// Snapshot.UnpricedBy names the endpoint/model pairs involved. See
// docs/superpowers/specs/2026-09-09-pricing-consolidation-design.md.
type eventCost struct {
	micros int64
	// tierMicros is the modelled cost of each rate tier, indexed by pricing.Tier.
	//
	// Filled from the published record's Tiers when there is one and from THIS package's own
	// pricing when there is not. Both sources, because a deployment produces either and a
	// mix populated on one path only is blank for half the traffic while every test on the
	// other path stays green.
	tierMicros [pricing.NumTiers]int64
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
// Without the fallback, cost requires some plugin to have published a figure, and a
// deployment running only inference-parser reports every request unpriced however many tokens
// it burned — which reads as "this traffic was free" rather than "nothing here priced it".
//
// Runs outside the aggregator's lock: resolution is a read of an immutable table
// and must not hold up the hot path.
// ce is the event's cost record and haveRec whether it had one, decoded by the caller.
// Passed in rather than read here because the caller needs the same record for its avoided
// figure, and costevent.Record is a JSON unmarshal — see Record's call site. The pricedness
// rule is applied here, which is the only thing costevent.Decode did that Record does not,
// so this arm still means exactly "a priced record was published".
func (a *Aggregator) costOf(e *pipeline.SessionEvent, ce costevent.Event, haveRec bool) eventCost {
	if haveRec && ce.Priced() {
		prov := ce.Provenance
		if prov == "" {
			// An event from a producer predating the field. It is a settled figure,
			// so the honest label is authoritative-or-modelled-unknown rather than
			// silently claiming either.
			prov = unlabelledLabel
		}
		ec := eventCost{micros: ce.Micros(), priced: 1, priceable: 1, provenance: prov}
		// Absent Tiers is the normal case for a gateway-priced request on a model with no
		// rates: no split exists, and leaving the array zero is how that is said. Nil and
		// zero are different states here — see costevent.Event.Tiers.
		if tc := ce.Tiers; tc != nil {
			ec.tierMicros = [pricing.NumTiers]int64{
				pricing.TierInput:      pricing.MicrosOrZero(tc.Input),
				pricing.TierCacheWrite: pricing.MicrosOrZero(tc.CacheWrite),
				pricing.TierCacheRead:  pricing.MicrosOrZero(tc.CacheRead),
				pricing.TierOutput:     pricing.MicrosOrZero(tc.Output),
			}
		}
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
	tiers, micros, ok, _ := pricing.CostByTier(rates, u)
	if !ok {
		// TWO CAUSES REACH HERE, AND ONLY ONE OF THEM IS A MISSING RATE. Either a rate was
		// found that does not cover every tier this request used — a gap an operator closes
		// by extending the entry — or the figure the rates produced was REFUSED: an
		// implausible token count, or a modelled total past
		// pricing.MaxPlausibleRequestCostMicros. See pricing.Cost for the full list.
		//
		// The distinction matters to whoever reads UnpricedBy, because the second cause is
		// not fixed by touching the rate table: an entry is already there, and either the
		// counts on the wire or the rate's magnitude is wrong. Naming the pair is still
		// right — that pair genuinely produced no figure — but "add or extend a pricing
		// entry" is only half the advice, and Snapshot.UnpricedBy says so.
		//
		// The header path keeps its causes apart (costevent.RejectedImplausible) because the
		// refusal is published per request; this path has no equivalent field, and adding one
		// is a wire change rather than a comment. Recorded here so the asymmetry is known
		// rather than inferred from a total that will not reconcile.
		return eventCost{priceable: 1, unpricedKey: key}
	}
	ec := eventCost{micros: micros, priced: 1, priceable: 1, provenance: prov.String()}
	ec.tierMicros = tiers
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
	// avoided rides BESIDE ec rather than inside it, so the counterfactual and the measured
	// figure travel on separate wires and no expression can add one to the other by
	// resembling it. See Counts.AvoidedMicros.
	var avoided int64
	if e.Phase != pipeline.SessionRequest {
		// ONE unmarshal, feeding both. costOf wants the record only when it priced
		// something and this wants it either way, so each calling costevent for itself
		// would decode the same JSON twice per event — and foldInto runs twice, which is
		// what hoisting this out of it was for.
		rec, haveRec := costevent.Record(e)
		ec = a.costOf(e, rec, haveRec)
		if haveRec {
			avoided = rec.TotalAvoidedMicros()
		}
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
	a.foldInto(a.all, t, sessionID, e, requestPlugins, ec, avoided)

	if ring, ok := a.sessions[sessionID]; ok {
		ring.lastSeen = at
		a.foldInto(ring.buckets, t, sessionID, e, requestPlugins, ec, avoided)
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
	a.foldInto(ring.buckets, t, sessionID, e, requestPlugins, ec, avoided)
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
//
// A SEPARATE found FLAG, NOT `coldestID == ""`, because "" IS A KEY HERE. Record accepts an empty
// session id and gives it a ring like any other — an unattributed event is a case this package
// handles rather than rejects, and foldInto only suppresses its LABEL. Keyed on the id, two things
// went wrong at once: the "" ring read as "nothing chosen yet", so whichever key came next
// overwrote it regardless of lastSeen and a WARMER ring was evicted in its place; and when "" was
// visited last, the delete was skipped entirely and the map stayed over its cap. Measured with a
// cap of 2: "" recorded coldest, then "hot", then "new" left rings ["", "new"].
func (a *Aggregator) evictColdestLocked() {
	var coldestID string
	var coldest time.Time
	found := false
	for id, r := range a.sessions {
		if !found || r.lastSeen.Before(coldest) {
			coldestID, coldest, found = id, r.lastSeen, true
		}
	}
	if found {
		delete(a.sessions, coldestID)
	}
}

// foldInto accumulates one event into one ring.
//
// sessionID is passed rather than derived because the ring itself does not know
// which session it belongs to: a.all is shared and a sessionRing holds only
// buckets. Both call sites pass the same id, which is what lets the bySession
// label be recorded uniformly instead of only on the all-sessions ring.
func (a *Aggregator) foldInto(ring []bucket, t time.Time, sessionID string, e *pipeline.SessionEvent, requestPlugins []string, ec eventCost, avoided int64) {
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
		AvoidedMicros:        avoided,
		InputCostMicros:      ec.tierMicros[pricing.TierInput],
		CacheWriteCostMicros: ec.tierMicros[pricing.TierCacheWrite],
		CacheReadCostMicros:  ec.tierMicros[pricing.TierCacheRead],
		OutputCostMicros:     ec.tierMicros[pricing.TierOutput],
		PricedRequests:       ec.priced,
		IncompleteRequests:   ec.incomplete,
		PriceableRequests:    ec.priceable,
		RefusedTokenRequests: refusedTokens,
	}
	// The split is CARRIED rather than re-enumerated. Add is the one summation over Counts'
	// fields, for the reason its own doc gives, and re-listing six of them here would be the
	// same exposure at the same distance: a field added to Counts and wired into Add would
	// still be dropped on the floor by this literal, and nothing would fail.
	//
	// Behaviour-identical, not merely equivalent-looking: split is only ever built from the
	// token counters above, so every other field is zero on one side of Add's `+=`, and
	// PresentKinds is `0 | x`. Errors is set below and so is unaffected by the order.
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
	// Host is the :authority as the listener saw it, so it is request-controlled
	// and goes through truncateLabel like the model name. Guarded on empty for the
	// same reason: SessionEvent.Host is "" when the listener did not populate
	// pctx.Host, and a "" key would draw a nameless band. Left out of the series
	// entirely, that traffic shows up as the renderer's "(unlabelled)" remainder,
	// which is what the other groupings already do for the events they skip.
	//
	// The port is stripped, or one host draws two bands. A CONNECT tunnel-open
	// records the authority from the request line, ports and all
	// ("api.anthropic.com:443"), while the parsed request inside that tunnel
	// records the bare host — so the same upstream splits in two, which is the kind
	// of split that makes a breakdown untrustworthy. Unlike byPlugin, one request
	// records exactly one host, so these sub-totals do sum to Requests.
	if h := hostLabel(e.Host); h != "" {
		addLabel(&b.byHost, truncateLabel(h), one)
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
	//
	// Response-phase plugins from this event, plus the request-phase plugins held from its
	// paired request event. Deduped across the two halves: a plugin that ran in both phases
	// is still one plugin that touched one turn. invocationPlugins is nil-safe, which it has
	// to be — Invocations is a POINTER and is nil for any plain proxied response, and
	// dereferencing it unguarded would panic inside Store.Append, on the request hot path.
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

// MaxSeriesInResponse bounds how many label series ONE RESPONSE carries, per bucket.
//
// A SEPARATE BOUND FROM maxLabelsPerBucket, and the reason both exist is that they bound
// different things. That one is a MEMORY bound at record time: 64 per axis, so a caller
// varying a label per request cannot grow this process. This one is a RESPONSE bound, and
// nothing was enforcing it — the two multiply. Measured on the endpoint as shipped:
// window=6h&resolution=1m&group=session is 360 buckets times up to 64 series, which produced
// a 4.7 MB response from a single unauthenticated GET.
//
// SIXTEEN, because the response is something a person reads. A by-model or by-agent breakdown
// past a dozen rows is not a breakdown anyone consumes; the rest belongs in the (other) band,
// which every client already renders because the record-time cap has always produced it. That
// is the property that makes this cheap: no new wire vocabulary, and a client written against
// the old shape shows a correct total with one extra row.
//
// RANKED ACROSS THE WHOLE WINDOW, not per bucket, which is the part that has to be got right.
// Capping each bucket independently would let the SAME label be a series in one minute and
// part of (other) in the next, so a chart would show a line appearing and vanishing while the
// traffic behind it was steady. The ranking is by cost, because this is a cost API and the
// question "where did the money go" is the one the top of the list has to answer.
const MaxSeriesInResponse = 16

// MaxLabelLen bounds one retained label. The model name is request-controlled, so
// without this a caller could park a megabyte of string in a bucket that lives
// for a full ring lap. Long enough for any real model id, including provider
// prefixes and dated suffixes.
//
// EXPORTED SO THE DURABLE COPY CAN BE PINNED TO IT. costledger caps its own labels at the same
// number and said so in a comment — "matched deliberately rather than chosen again" — while both
// copies were unexported, so no test could compare them and raising one to 4096 left both suites
// green. Same defect and same fix as MaxRetentionDays.
const MaxLabelLen = 96

// maxLabelLen is the internal spelling, so this package's call sites read unchanged.
const maxLabelLen = MaxLabelLen

// hostLabel reduces an :authority to the host alone, so "api.anthropic.com:443"
// and "api.anthropic.com" are one series rather than two.
//
// net.SplitHostPort, not a Cut on ":": an IPv6 literal is full of colons, and
// splitting on the first turns "[::1]:9094" into "[". SplitHostPort errors on an
// authority carrying no port at all, so that case falls through to the input.
//
// The brackets then have to come off separately, or IPv6 splits the very way this
// function exists to prevent: SplitHostPort unwraps them when it succeeds
// ("[::1]:9094" -> "::1"), leaving a port-less "[::1]" as its own band for the
// same upstream.
//
// A port with no host (":443") reduces to "", which the caller treats as absent
// and folds into the renderer's "(unlabelled)" remainder. That is the right
// answer for an authority that names no host: there is nothing to label it with,
// and inventing a band for it would be worse than counting it as unattributed.
//
// The same reduction exists in three listeners and in abctl's events pane, each
// private to its package; this is a fourth rather than a shared helper because
// core/usage must not import a listener, and hoisting one is a refactor this
// change does not need.
func hostLabel(authority string) string {
	if h, _, err := net.SplitHostPort(authority); err == nil {
		return h
	}
	// No port: still strip brackets, so a bare "[::1]" matches the "::1" that the
	// ported form reduces to.
	return strings.TrimSuffix(strings.TrimPrefix(authority, "["), "]")
}

// truncateLabel caps a label at maxLabelLen BYTES, cutting on a rune boundary.
//
// BYTES, because that is what bounds the memory a label occupies and the line it becomes in
// the ledger; RUNE BOUNDARY, because a byte cut breaks the very byte cap it enforces. A plain
// s[:maxLabelLen] can split a multi-byte sequence and leave an invalid trailing fragment, and
// encoding/json then expands each invalid byte into a 3-byte U+FFFD — so a 121-byte label cut
// at 96 SERIALISES AT 100 BYTES (measured in costledger, where the same defect was fixed
// first). The invalid fragment is the smaller problem; the cap silently not holding is the
// reason this is a fix rather than tidying.
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
// THE RING IS A SERVING SURFACE OF ITS OWN. The model comes off the parsed request body and
// the endpoint is the host the workload asked for, so without this GET /v1/usage serves a
// model name containing an ANSI escape straight out of memory — while the ledger's copy of the
// very same label is clean, because costledger sanitises on write. Two surfaces, one request,
// different bytes: group=model shows the label as two series, and the unsanitised one can
// reposition an operator's cursor. CWE-150.
//
// REPLACED, NOT DROPPED, so tampering stays visible rather than collapsing into a
// plausible-looking label: "m\x1b[31mx" reads as "m�[31mx" rather than as "m[31mx",
// which nobody would question.
//
// C1 IS INCLUDED — U+0080–U+009F — and so are the bidi and zero-width runes; isControlRune
// names the set and pipeline.sanitizeUA carries the argument for it. The short version: U+009B
// is a single-character CSI, so a byte scan for anything below 0x20 steps straight past a
// working escape sequence, which is why the scan below decodes runes.
//
// FOUR SANITISING SURFACES, ONE RULE, AND THE RULE IS NOW SHARED:
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
// The first three no longer each carry their own copy of WHICH RUNES COUNT: that predicate is
// pipeline.IsControlRune, and both this package and costledger import pipeline, so there is one
// definition repo-wide. This comment used to argue the opposite — that referencing pipeline's
// would "invert the layering into a cycle" and so the rule had to be copied — which was wrong
// in the direction that matters: nothing in pipeline imports this package. Three byte-identical
// copies existed on the strength of that paragraph, only one of them tested for the bidi marks.
//
// What is still per-surface is the SHAPE of the sanitiser around it (what it replaces with, and
// whether it caps), and abctl's is deliberately narrower and out of this module's reach.
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
		if pipeline.IsControlRune(r) || (r == utf8.RuneError && size == 1) {
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
		if pipeline.IsControlRune(r) || (r == utf8.RuneError && size == 1) {
			return true
		}
		i += size
	}
	return false
}

// CapSeries folds everything past the n costliest entries into the (other) band.
//
// EXPORTED FOR costledger, which serves the symbolic windows from disk and had no bound of its
// own at all: its rows come from day files, so the number of distinct (endpoint, model, agent)
// keys in a response is however many a month of traffic produced. One map rather than a whole
// snapshot, because that source answers with a single bucket.
//
// Ranked by cost, ties broken by requests and then by label, so the answer is deterministic —
// a breakdown whose membership changed between two identical requests would be worse than an
// unbounded one, because a client could not tell a traffic change from a tie-break.
//
// The (other) entry is Counts.Add-ed, so every field it carries is summed by the one
// summation point and a field added to Counts is carried here with no edit.
func CapSeries(series map[string]Counts, n int) map[string]Counts {
	if len(series) <= n {
		return series
	}
	keys := make([]string, 0, len(series))
	for k := range series {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := series[keys[i]], series[keys[j]]
		if a.CostMicros != b.CostMicros {
			return a.CostMicros > b.CostMicros
		}
		if a.Requests != b.Requests {
			return a.Requests > b.Requests
		}
		return keys[i] < keys[j]
	})
	out := make(map[string]Counts, n+1)
	var other Counts
	for i, k := range keys {
		if i < n {
			out[k] = series[k]
			continue
		}
		other.Add(series[k])
	}
	// Merged rather than assigned: a label spelled "(other)" by a caller is already in the
	// map, and overwriting it would drop that traffic. The band is not a reserved key — see
	// costledger's overflowLabel, which makes the same point about spoofability.
	cur := out[overflowLabel]
	cur.Add(other)
	out[overflowLabel] = cur
	return out
}

// capSeriesAcrossWindow applies MaxSeriesInResponse to every bucket using ONE ranking taken
// over the whole window, so a label is either a series everywhere or (other) everywhere.
//
// See MaxSeriesInResponse for why per-bucket capping is the wrong shape. The ranking is built
// from the same Counts.Add the totals use, so "costliest" means the same thing here as in the
// figure the client renders above the chart.
func capSeriesAcrossWindow(buckets []Bucket, n int) {
	window := map[string]Counts{}
	for _, b := range buckets {
		for k, v := range b.Series {
			cur := window[k]
			cur.Add(v)
			window[k] = cur
		}
	}
	if len(window) <= n {
		return
	}
	keep := make(map[string]bool, n)
	for k := range CapSeries(window, n) {
		keep[k] = true
	}
	for i := range buckets {
		if len(buckets[i].Series) == 0 {
			continue
		}
		out := make(map[string]Counts, n+1)
		var other Counts
		for k, v := range buckets[i].Series {
			if keep[k] {
				out[k] = v
				continue
			}
			other.Add(v)
		}
		if other != (Counts{}) {
			cur := out[overflowLabel]
			cur.Add(other)
			out[overflowLabel] = cur
		}
		buckets[i].Series = out
	}
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
