// Package event is the canonical wire shape of the per-request cost that
// litellm-budget-track publishes onto a session event.
//
// It lives in its own package because the producer is a plugin while the
// consumers are the usage aggregator and abctl, and none of those should import
// each other. Before this package existed the struct was declared once in the
// plugin, again in abctl, and was about to be declared a third time in the
// aggregator — which is the duplication cortex #910 exists to remove.
//
// Dependency-light on purpose: the aggregator links this on every build,
// including the trimmed "lite" images that exclude the plugin entirely. core/cost/pricing
// is the one non-pipeline dependency, for the single micros bound both packages must
// agree on (see Micros).
//
// THAT EDGE ADDS NOTHING ANYWHERE, and the check is `go list -deps`, not a grep for a direct
// import: every module that links this package already linked pricing — core and
// authbridge-proxy price traffic, and abctl reaches it through core/cost/usage and core/config.
package event

import (
	"encoding/json"

	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/pipeline"
)

// Key is the session-event key the cost record is published under.
//
// It names the CONCERN, not the producer. That distinction is the whole point: the key used
// to be the producing plugin's name, so moving costing to whichever component actually
// knows the tokens — cortex #972 moves it to inference-parser — would have been a breaking
// wire change for every live consumer AND for every event already written to a session
// store. Keyed on the concern, the producer can move and no consumer notices.
//
// The framework set this precedent and stated the reason. pipeline/context.go publishes
// "body-mutation"+PluginEventSuffix from the core, which is not a plugin at all, because
// "a switch of plugin names in a future refactor shouldn't break operators' dashboards".
// pipeline/snapshot.go does not require the key to be a plugin name.
//
// Exactly ONE component may write this key per request. Extensions.Custom is a plain map,
// so a second writer would win silently.
//
// A PRODUCER does not write this key directly — it writes Key+pipeline.PluginEventSuffix
// into pctx.Extensions.Custom, and pipeline/snapshot.go strips the suffix when building
// SessionEvent.Plugins. Use settle.Publish rather than assembling the key by hand.
const Key = "cost"

// PluginName is the LEGACY key: the name of the plugin that used to be the sole producer.
//
// Still read by Decode and Record, because session stores hold events written under it and
// a consumer reading history must not see a gap where the rename happened. Still written by
// litellm-budget-track until #972 phase 2 moves the producer, and a test in that package
// asserts it equals BudgetTrack.Name() for as long as that remains true.
//
// Deprecated: write Key. Reads keep working.
const PluginName = "litellm-budget-track"

// Source names how the cost was arrived at.
const (
	// SourceGatewayHeader is the gateway's own post-discount figure, read from
	// x-litellm-response-cost. Authoritative.
	SourceGatewayHeader = "gateway-header"
	// SourceUsageFallback is priced from token counters because the header
	// reported 0 — which every streamed response does, so for a streaming agent
	// this is the common case, not the exception.
	SourceUsageFallback = "usage-fallback"
)

// Reasons a producer REFUSED a cost figure that was on the wire. Carried in
// RejectedReason, which is the only evidence such a figure existed at all.
//
// A wire string, spelled like Source and IncompleteReason: hyphenated lowercase.
const (
	// RejectedImplausible: a COST FIGURE larger than one inference call could plausibly be
	// (pricing.MaxPlausibleRequestCostMicros). Two producers of it, and the reason is the
	// same claim in both: a gateway's own header on an endpoint this pipeline could not
	// parse, so nothing corroborated it (see settle.headerCost), or a figure MODELLED from
	// plausible counts at an implausible rate — an operator's typo, or a rate discovered
	// from a gateway's /model/info. The second one matters because its cause is ours: it
	// unprices every request the rate touches while looking exactly like traffic nobody had
	// rates for.
	RejectedImplausible = "cost-implausible"

	// RejectedImplausibleUsage: a TOKEN COUNT no request could have reported — negative, or
	// past pricing.MaxPlausibleTokens — so every figure derived from it was refused,
	// including the halves. Distinct from RejectedImplausible because the two say different
	// things about where the impossibility is: there, a dollar figure nobody could have
	// charged; here, a counter nobody could have counted, which also makes the token totals
	// rendered beside the money untrustworthy. See settle.Settle.
	RejectedImplausibleUsage = "usage-implausible"
)

// Event is one request's settled cost. Unlike tool-prune's event, which carries
// rates and leaves the arithmetic to the consumer, this carries a finished
// figure.
type Event struct {
	CostUSD float64 `json:"cost_usd"`
	// Source is SourceGatewayHeader or SourceUsageFallback. A consumer that
	// distinguishes authoritative from modelled figures reads this.
	Source        string  `json:"source"`
	DailyTotalUSD float64 `json:"daily_total_usd"`
	DailyMaxUSD   float64 `json:"daily_max_usd"`

	// Provenance names where the figure came from: "authoritative" when the
	// gateway reported it, otherwise the rate table's own level ("configured",
	// "discovered", "bundled"). See core/cost/pricing.Provenance.
	//
	// ADDITIVE, and omitempty, so a consumer written against the four original
	// fields keeps decoding unchanged and an event from an older producer decodes
	// here with an empty Provenance. Source is retained rather than replaced for
	// the same reason: it already ships, and its two values still answer a
	// different question — WHICH PATH priced this (header vs token counts) rather
	// than how much to trust the rates.
	Provenance string `json:"provenance,omitempty"`

	// HeaderOmittedCache says the gateway reported a figure that priced only the uncached
	// tiers, so the rate table was charged instead of it. See settle.Settled.
	//
	// ON THE WIRE BECAUSE THE IN-PROCESS FLAG REACHES NOTHING. settle.Settled keeps both
	// figures so the drift reporter can compare them, and the warning that reporter emits
	// fires once per endpoint and model — right for a log line, useless for an audit. Without
	// this field a substituted row is distinguishable only as Source: usage-fallback, which is
	// also what a stream that never had a header looks like, and the four-day per-request
	// analysis that found this defect could not be repeated on a ledger recorded after the fix.
	// Same argument as the Incomplete and RejectedReason fields NewRecord carries.
	HeaderOmittedCache bool `json:"header_omitted_cache,omitempty"`

	// GatewayUSD is the gateway's own figure when it was NOT the one charged.
	//
	// Set only alongside HeaderOmittedCache, because that is the only case where it says
	// something CostUSD does not: where the header wins, CostUSD IS that figure and Source
	// names it, so a second copy would be duplication every event pays for.
	//
	// DIAGNOSTIC, NEVER SUMMED. It is not a discount and not an adjustment — it is a figure
	// this process declined, kept so an operator can see by how much the gateway
	// under-reported. The aggregate ledger cannot carry a per-request flag and distinguishes
	// these rows by provenance instead ("bundled" where it would have said "authoritative").
	GatewayUSD float64 `json:"gateway_usd,omitempty"`

	// Settled marks a figure the producer settled deliberately, INCLUDING zero.
	//
	// A COST OF ZERO MUST NOT READ AS "NO FIGURE". Gate a producer on cost > 0 — as
	// litellm-budget-track's own charging does — and a gateway that reported a present 0
	// (a genuine free call, a cache hit, an error) emits no event at all; have Decode
	// reject CostUSD <= 0 and the same figure disappears on the read side. The usage
	// aggregator then sees nothing, falls through to its rate table, and FABRICATES a cost
	// for a call the gateway explicitly declared free, counting it as priced.
	//
	// A settled zero is now a real answer that suppresses the fallback.
	Settled bool `json:"settled,omitempty"`

	// Incomplete marks CostUSD as not an EXACT total, and IncompleteReason says which way it is
	// inexact — see pricing's reasons: a known-LOW figure (a stream that died before its output
	// count arrived), or one approximate in no known direction (a gateway reporting only
	// total_tokens). A reason beside the flag rather than one bit, because a floor is bounded on
	// one side and usually transient while an approximation is a standing property of the
	// gateway, and collapsed they would be indistinguishable.
	//
	// DISCLOSURE, not adjustment. CostUSD keeps the figure and Priced still returns true, so a
	// consumer keeps these dollars in its total: the figure is the best available, and what the
	// flag withdraws is only the claim of EXACTNESS. It stays in every priced count — coverage
	// answers a different question — and is disclosed alongside.
	//
	// ADDITIVE and omitempty: an event from an older producer decodes with Incomplete false,
	// which is the pre-fix reading, so a consumer ignoring these fields is as correct as before.
	Incomplete       bool   `json:"incomplete,omitempty"`
	IncompleteReason string `json:"incomplete_reason,omitempty"`

	// PromptUSD is the modelled cost of the PROMPT alone, output excluded.
	//
	// A breakdown, not a component of a sum: it is the table's figure even when CostUSD
	// is the gateway's, so PromptUSD + anything is not a total of anything. It exists so
	// a request row can show what that request cost while the response row shows what
	// the response cost, which is a question the gateway's single number cannot answer.
	PromptUSD float64 `json:"prompt_usd,omitempty"`

	// OutputUSD is the modelled cost of the OUTPUT alone, prompt excluded — the other
	// half of the same breakdown, so that a per-row column can be row-local.
	//
	// Not CostUSD minus PromptUSD. That difference mixes a possibly-authoritative total
	// with a modelled half, so it concentrates the table's whole error into the
	// completion figure and can go negative; this is the table pricing the generated
	// tokens directly.
	//
	// ADDITIVE and omitempty, like Provenance: an event from an older producer decodes
	// here with OutputUSD zero, which a consumer must read as "no figure" rather than as
	// a free completion.
	OutputUSD float64 `json:"output_usd,omitempty"`

	// Tiers is the modelled per-tier split, or NIL when the rate table could not produce
	// one — a gateway-priced request on a model this deployment has no rates for.
	//
	// A POINTER, deliberately. A zero-valued struct cannot distinguish "no split exists"
	// from "every tier cost nothing", and consumers APPORTION a real total by these
	// numbers: an absent split read as four zeros divides by zero, and read as a real one
	// reports all traffic as free. Absence is the load-bearing state here, the same way
	// Agent "" is in ledger.Row.
	//
	// OVERLAPS OutputUSD ABOVE, and not by accident. That field is the output HALF as
	// Settle computes it — a second pricing call with the prompt tiers zeroed, guarded by
	// the pair ceiling — and it feeds a per-row figure in the events pane. This is one
	// element of a decomposition of the WHOLE request, computed in a single pass under one
	// resolved rate set. Merging them would make the events pane's output figure depend on
	// this feature's arithmetic, so they stay separate and each keeps its own provenance.
	Tiers *TierCost `json:"tiers,omitempty"`

	// UsageRefused says the response's TOKEN COUNTS were impossible — negative, or past
	// pricing.MaxPlausibleTokens — whatever happened to the money.
	//
	// SEPARATE FROM RejectedReason, which is about a figure and reads as UNPRICED everywhere. When
	// a gateway's header priced the response, that header is what the call charged whatever the
	// counters claimed, so refusing the money would discard a real charge; but the counts are still
	// impossible, and a client renders them beside it. Carried alone, this is the only way to say
	// "the dollars are good, the token totals are not" — and without it that record read as fully
	// exact, with impossible counts next to exact money.
	//
	// Never spendable on its own and never a reason to drop the charge: Trust() is unaffected,
	// because trust is about the FIGURE. A consumer rendering token totals is the one that must
	// read this.
	UsageRefused bool `json:"usage_refused,omitempty"`

	// RejectedReason names a cost figure the producer REFUSED — RejectedImplausible
	// today — and is the ONLY evidence that a figure was on the wire at all.
	//
	// It exists so a refusal is a DISCLOSED COVERAGE GAP rather than silence: publishing nothing
	// instead makes a response that reported $9,000,000,000 and one that reported nothing the same
	// event to every consumer, hiding both the misconfiguration and the attack.
	//
	// NOT A FIGURE, and never clamped. CostUSD stays 0 and Priced() returns false whenever this is
	// set, so no consumer can read a refused figure as money however the other fields arrive. A
	// clamped figure is a wrong number wearing a right label, and worse than a named gap: the ring
	// forgets a gap in six hours, the durable ledger keeps a wrong number for thirty days.
	//
	// LIMIT OF THE DISCLOSURE: it does not reach usage.Snapshot.UnpricedBy, which keys on
	// "<endpoint> <model>" and counts requests that COULD have been priced — and this refusal
	// lands on traffic carrying no model at all, which is why the figure could not be
	// corroborated. The evidence is this field plus the operator warning cost/settle emits.
	//
	// ADDITIVE and omitempty: an older consumer sees an unsettled zero, which is the pre-fix
	// reading of a refused figure.
	RejectedReason string `json:"rejected_reason,omitempty"`

	// Avoided is cost that was NOT incurred. Nothing in here is spend.
	//
	// A nested list rather than sibling floats, deliberately. More counterfactuals are
	// coming — compaction, redaction, "what a cheaper model would have cost" — and as
	// flat fields beside CostUSD the record becomes half-real and half-hypothetical,
	// which is how someone eventually sums two fields that must never be summed. One
	// category-named container keeps the boundary visible in the type, and a new
	// contributor appends an entry instead of adding a field.
	//
	// The invariant: no consumer may add any Avoided figure to spend, to a budget, or
	// to usage totals. A test in the usage package asserts the aggregator's totals are
	// unchanged by their presence.
	Avoided []Saving `json:"avoided,omitempty"`
}

// Saving is one component's contribution to cost that was not incurred.
type Saving struct {
	// Component is what avoided the cost, e.g. "tool-prune". Attributed from the
	// framework's own body-mutation record rather than self-reported, so a plugin
	// cannot claim someone else's saving.
	Component string `json:"component"`
	// TokensAvoided is the estimated prompt-token reduction.
	TokensAvoided int `json:"tokensAvoided,omitempty"`
	// USD is TokensAvoided priced at the tier the prompt actually landed in, through
	// the same table, multiplier and provenance as CostUSD above.
	USD float64 `json:"usd,omitempty"`
	// Provenance is how trustworthy the dollar figure is: the rate table's level
	// ("configured", "bundled", ...), or empty when no rate covered the tier.
	//
	// NOT the record's top-level Provenance, which may be "authoritative" because the
	// gateway reported what the request actually cost. A saving is counterfactual, so it
	// is always modelled from the table — claiming the gateway's authority for it would
	// overstate a figure nobody measured.
	Provenance string `json:"provenance,omitempty"`
	// Tier names which prompt tier the saving came out of: cache-write, cache-read or
	// input. It matters because they differ by 12.5x, and a saving quoted without it
	// cannot be checked.
	Tier string `json:"tier,omitempty"`
	// Projected marks a saving that was measured but NOT APPLIED — tool-prune's
	// observe mode leaves every byte on the wire. A consumer must present this as
	// "would have saved", never as money already not spent, and must not add it to
	// any total that includes applied savings.
	Projected bool `json:"projected,omitempty"`
	// Estimated marks TokensAvoided as derived from a bytes-to-tokens ratio rather
	// than counted by a tokenizer. It is calibrated per request and therefore wrong
	// when the removed span had a different token density from what remained — so a
	// UI must not present the dollar figure as measured.
	Estimated bool `json:"estimated,omitempty"`
}

// TotalAvoidedUSD sums the APPLIED savings, skipping projected ones.
//
// Provided so consumers do not each write the loop and disagree about whether
// observe-mode figures count. They do not: a projected saving is money that WAS spent.
func (e Event) TotalAvoidedUSD() float64 {
	var t float64
	for _, s := range e.Avoided {
		if !s.Projected {
			t += s.USD
		}
	}
	return t
}

// TotalAvoidedMicros is TotalAvoidedUSD in millionths of a dollar.
//
// Exists so the aggregate accumulates savings in the same integer unit it accumulates spend,
// for the reason Micros gives: bucket addition stays exact and the JSON round-trips. A
// consumer summing the float and converting at the end would drift against a total the
// server already converted per record.
//
// BOUNDED through the same pricing.MicrosFromUSD, and zero when the figure will not
// represent — not clamped. A saving is a modelled counterfactual, so an implausible one is
// likelier to be a broken estimator than a real bargain, and "no figure" is the honest
// answer for one this package cannot hold. Reachable: TokensAvoided is provider-adjacent and
// the tier multiplier spans 12.5x.
//
// RejectedReason IS DELIBERATELY NOT CONSULTED, unlike Micros. That reason is a judgment
// about the figure that was INCURRED — a cost header the producer refused as implausible —
// while a saving is modelled from the rate table on a separate path, and settle.NewRecord
// carries Avoided orthogonally to it. Zeroing here would discard a sound estimate because an
// unrelated figure on the same record was unsound, and Record's own doc names savings on
// requests that could not be priced as the interesting case.
//
// STILL NOT SPEND. This returns the aggregate's unit, not permission to add it to one: see
// Event.Avoided for the invariant, and usage.Counts.AvoidedMicros for where it lands.
func (e Event) TotalAvoidedMicros() int64 {
	m, ok := pricing.MicrosFromUSD(e.TotalAvoidedUSD())
	if !ok {
		return 0
	}
	return m
}

// Micros converts CostUSD to millionths of a dollar, rounded to nearest.
//
// The integer unit is what usage.Counts accumulates: it keeps bucket addition
// exact and JSON round-tripping lossless, which float dollars are not. A cost
// below half a micro rounds to 0 while still counting as priced — a real
// sub-micro charge, not an unknown one.
//
// BOUNDED, through the same pricing.MicrosFromUSD that prices a token tally: the header path
// accepts any finite non-negative float, so a gateway reporting 1e13 saturated to MaxInt64 and two
// such requests wrapped usage.Counts.Add to −2 micros — an aggregate that then sat in the durable
// ledger for thirty days.
//
// The bound removes the SATURATION, not the wrap: 1024 figures at the bound wrap the same sum, and
// no per-request bound can fix that — see MaxCostMicros.
//
// Zero for an out-of-range figure, and Priced returns false for the same one, so no
// consumer reaches this value believing it is a price. NOT clamped to the bound: a
// clamped figure is a wrong number wearing a right label, and unpriced is the honest
// answer for a figure this package cannot represent.
//
// Zero for a REFUSED figure too, on the same rule. Micros and Priced must agree — a
// consumer that read a figure here while Priced said no would add money to a total while
// counting the request as uncovered — and RejectedReason means there is no figure to
// convert, whatever CostUSD happens to hold.
func (e Event) Micros() int64 {
	if e.RejectedReason != "" {
		return 0
	}
	m, ok := pricing.MicrosFromUSD(e.CostUSD)
	if !ok {
		return 0
	}
	return m
}

// TierCost is the modelled cost of one request, split by rate tier.
//
// NAMED FIELDS RATHER THAN A POSITIONAL ARRAY, because this lands in an APPEND-ONLY FILE
// retained for thirty days. `"tiers":[0.007,0.022,0.3,0.46]` requires every future reader
// to know pricing.Tier's declaration order, and that order is an implementation detail of
// another package — one whose own doc says arrays exist there so a forgotten tier cannot
// hide. Four names cost a few bytes each under omitempty and are self-describing to
// anyone reading the ledger with jq.
//
// MODELLED, NEVER AUTHORITATIVE — the same standing as PromptUSD, and for the same reason
// given there: a gateway reports one total for the call and never breaks it down. These
// are the rate table's answer even when CostUSD is the gateway's, so they do NOT sum to
// it and nothing may add them up and present the result as spend.
//
// A zero member means that tier carried no tokens. "No split at all" is the nil pointer
// on Event.Tiers, not a zero-valued struct.
type TierCost struct {
	Input      float64 `json:"input,omitempty"`
	CacheWrite float64 `json:"cache_write,omitempty"`
	CacheRead  float64 `json:"cache_read,omitempty"`
	Output     float64 `json:"output,omitempty"`
}

// Trust is the ONE answer to "what may be believed about this figure", derived from the record
// rather than stored beside it.
//
// WHY IT EXISTS. Seven rounds of review added a field or a string every time a case turned up
// where the existing labels lied: RejectedReason (two values), IncompleteReason (three), Settled,
// and the Priced predicate that has to agree with all of them. Each addition was justified on its
// own; together they were a taxonomy nobody designed, whose COMBINATIONS nothing enumerated —
// while every consumer had to reconstruct the same verdict from the parts, and one of them
// (a renderer testing PromptUSD > 0) got it wrong in a way that showed a refused figure.
//
// DERIVED, NOT A NEW WIRE FIELD, deliberately. A stored verdict is a second source of truth that
// can disagree with the fields it summarises, on old records most of all: this repo's own ledger
// keeps thirty days of them, so a field added today would be absent from most of the file and
// every reader would need the derivation anyway. As a method it is exactly one rule, and
// TestTrust_CoversEveryCombination enumerates the cross-product it is a rule over.
type Trust string

const (
	// TrustExact: a figure, and the counters or the gateway support it as a total.
	TrustExact Trust = "exact"
	// TrustFloor: a real figure known to be LOW — an output tally that never arrived, or a
	// response whose own total exceeds the counters that were priced. Spendable: it is the best
	// available number and understating is disclosed, not corrected.
	TrustFloor Trust = "floor"
	// TrustApproximate: a figure inexact in no known direction, from a gateway that reported a
	// total with no per-kind split. Spendable, with the caveat carried.
	TrustApproximate Trust = "approximate"
	// TrustRefused: a figure was on the wire and this process declined it — implausible cost, or
	// an impossible token report. NOT spendable, and the record exists so the gap is nameable.
	TrustRefused Trust = "refused"
	// TrustUnpriced: no figure to believe. An unsettled zero, a negative, or one the micros unit
	// cannot hold. NOT spendable, and distinct from a settled zero, which is a gateway saying a
	// call was free.
	TrustUnpriced Trust = "unpriced"
)

// WHICH QUESTION IS WHICH, because "consumers ask different questions" was half the complaint and
// the answer is not "always ask Trust". Measured across this repo, three consumers read a money
// field directly, and two of them are RIGHT to:
//
//	may this be added to a total?          Priced(), i.e. Trust().Spendable(). Never a comparison
//	                                       against zero: a refused record can carry a number, and
//	                                       an unsettled zero is not a free call.
//	what caveat do I render beside it?     TrustReason(), which returns the reason belonging to the
//	                                       verdict rather than leaving a consumer to pick a field.
//	is there a figure for THIS cell?       PromptUSD > 0 / OutputUSD > 0 is the right test. A row
//	                                       with no prompt figure renders blank, and that is a
//	                                       question about presence, not about trust — abctl's
//	                                       promptCost and outputCost are this case, deliberately.
//	do I have a divisor?                   The number itself. litellm-budget-track's drift check
//	                                       needs a positive authoritative figure to divide by,
//	                                       which is arithmetic, not a verdict.
//
// The bug that made this worth writing down was none of the four: a renderer showed a prompt figure
// for a record whose token report had been refused. That was fixed where it belonged, at the
// producer — the halves are dropped with the whole — because a consumer cannot be expected to
// re-derive a producer's refusal from the parts.

// Spendable reports whether a figure carrying this trust may be added to a total.
//
// The whole point of one verdict: every admission guard in the system — the ledger writer's, the
// aggregator's Decode, a budget's accumulate — asks this one question, and the disclosure of HOW
// approximate a spendable figure is travels separately, in the reason.
func (tr Trust) Spendable() bool {
	switch tr {
	case TrustExact, TrustFloor, TrustApproximate:
		return true
	default:
		return false
	}
}

// Trust returns what may be believed about this record's figure.
//
// ORDERED MOST-DAMNING FIRST, and the order is the semantics. A refusal beats everything, because
// the figure was declined; unpriced beats the qualifiers, because there is nothing to qualify; and
// a floor beats an approximation when both could apply, which under-claims precision rather than
// over-claiming it — the same ordering pricing.IncompleteReason itself uses.
func (e Event) Trust() Trust {
	if e.RejectedReason != "" {
		return TrustRefused
	}
	if e.CostUSD < 0 {
		return TrustUnpriced
	}
	if _, ok := pricing.MicrosFromUSD(e.CostUSD); !ok {
		return TrustUnpriced
	}
	if !(e.CostUSD > 0 || e.Settled) {
		return TrustUnpriced
	}
	if !e.Incomplete {
		return TrustExact
	}
	switch e.IncompleteReason {
	case pricing.ReasonSplitUnreported:
		return TrustApproximate
	default:
		// Every other reason names a figure known to be LOW — and so does an EMPTY one, which is
		// a producer that set Incomplete without saying why. Treating that as approximate would
		// let a missing reason quietly upgrade the claim.
		return TrustFloor
	}
}

// TrustReason is the record's own explanation for a verdict that is not exact, or "" when it is.
//
// One accessor rather than a consumer choosing between two fields by inspecting a third: which of
// RejectedReason and IncompleteReason applies is decided by Trust, and asking the record removes
// the chance of rendering an incomplete-reason on a refused record or the reverse.
func (e Event) TrustReason() string {
	switch e.Trust() {
	case TrustRefused:
		return e.RejectedReason
	case TrustFloor, TrustApproximate:
		return e.IncompleteReason
	default:
		return ""
	}
}

// Priced reports whether this record carries a usable dollar figure.
//
// Named and exported because it is the predicate Decode applies, and a consumer that took
// the record through Record needs to ask the same question without reimplementing it. The
// two must agree; a test asserts they do.
//
// A settled zero is priced: the producer means "this call was free". An unsettled zero is
// not: it means nobody priced this, and rendering $0.00 for it would report unpriced
// traffic as free. A negative figure is never a price.
//
// Nor is a figure OUT OF RANGE for the micros unit every consumer accumulates in —
// at or above pricing.MaxCostMicros, or NaN, or an infinity. This is the validation point:
// the producer's header path accepts any finite non-negative float, and nothing
// downstream re-derives a total once a saturated value has been added to it. Unpriced
// rather than clamped, for the reason in Micros.
//
// Nor is a figure the producer REFUSED. RejectedReason is checked here rather than left to
// the producer's zeroing of CostUSD, because this predicate is the one every consumer
// already asks — the ledger writer's admission guard and the aggregator's Decode both go
// through it — so one line here means a refused figure cannot read as spend anywhere, even
// if a future producer sets the reason and forgets to drop the number.
// ONE RULE, IN ONE PLACE: this is Trust().Spendable(), not a second copy of the same reasoning.
// The four conditions above are still the rule — they are written out in Trust, where the
// combinations they form are enumerable — and keeping a parallel implementation here is exactly
// how two predicates that must agree stop agreeing.
func (e Event) Priced() bool { return e.Trust().Spendable() }

// Record pulls the cost record off a session event, whether or not it carries a price.
//
// PRESENCE, which is a different question from pricedness — and the only one Decode could
// answer. A record whose cost is an unsettled zero is real: it names the endpoint, the
// source and the provenance, and #972 gives it avoided-cost entries too. Under Decode's
// rule that record is indistinguishable from no record at all, so a consumer wanting its
// non-cost fields could not reach them, and a saving on a request that could not be priced
// — exactly the traffic where a saving is most interesting — would silently vanish.
//
// Reads the current key first and falls back to the legacy one. New wins when both are
// present: a transitional deployment can run an old producer alongside a new one, and
// taking the legacy value there would report the figure from the component being retired.
func Record(e *pipeline.SessionEvent) (Event, bool) {
	if e == nil || len(e.Plugins) == 0 {
		return Event{}, false
	}
	raw, ok := e.Plugins[Key]
	if !ok {
		if raw, ok = e.Plugins[PluginName]; !ok {
			return Event{}, false
		}
	}
	var ev Event
	if err := json.Unmarshal(raw, &ev); err != nil {
		return Event{}, false
	}
	return ev, true
}

// Decode pulls a PRICED cost off a session event.
//
// A false return is the normal case, not an error: the producing plugin may not be in the
// pipeline, and a request that charged nothing (cache hit, error) emits nothing. A
// non-positive cost also returns false — "free" and "unknown" must not be conflated, and a
// consumer that treated 0 as priced would render $0.00 for traffic it simply cannot price.
//
// Use Record instead when you want the record regardless of whether it priced anything.
func Decode(e *pipeline.SessionEvent) (Event, bool) {
	ev, ok := Record(e)
	if !ok {
		return Event{}, false
	}
	// One predicate, in one place — see Priced.
	if !ev.Priced() {
		return Event{}, false
	}
	return ev, true
}
