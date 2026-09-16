// Package costevent is the canonical wire shape of the per-request cost that
// litellm-budget-track publishes onto a session event.
//
// It lives in its own package because the producer is a plugin while the
// consumers are the usage aggregator and abctl, and none of those should import
// each other. Before this package existed the struct was declared once in the
// plugin, again in abctl, and was about to be declared a third time in the
// aggregator — which is the duplication cortex #910 exists to remove.
//
// Dependency-light on purpose: the aggregator links this on every build,
// including the trimmed "lite" images that exclude the plugin entirely. authlib/pricing
// is the one non-pipeline dependency, for the single micros bound both packages must
// agree on (see Micros); every importer of this package already links it, so nothing
// grew a new transitive dependency.
package costevent

import (
	"encoding/json"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
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
// SessionEvent.Plugins. Use costing.Publish rather than assembling the key by hand.
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
	// RejectedImplausible: a response reported a cost larger than one inference call
	// could plausibly be (pricing.MaxPlausibleRequestCostMicros) on an endpoint this
	// pipeline could not parse, so nothing corroborated the figure and it was not
	// believed. See costing.headerCost for the rule and what it does and does not
	// protect against.
	RejectedImplausible = "cost-implausible"
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
	// "discovered", "bundled"). See authlib/pricing.Provenance.
	//
	// ADDITIVE, and omitempty, so a consumer written against the four original
	// fields keeps decoding unchanged and an event from an older producer decodes
	// here with an empty Provenance. Source is retained rather than replaced for
	// the same reason: it already ships, and its two values still answer a
	// different question — WHICH PATH priced this (header vs token counts) rather
	// than how much to trust the rates.
	Provenance string `json:"provenance,omitempty"`

	// Settled marks a figure the producer settled deliberately, INCLUDING zero.
	//
	// Cost of zero used to be indistinguishable from "no figure": litellm-budget-track
	// charges nothing for a gateway that reported a present 0 cost — a genuine free
	// call, a cache hit or an error — and then emitted no event, because it gates on
	// cost > 0. Decode rejected CostUSD <= 0 for the same reason. So the usage
	// aggregator saw nothing, fell through to its rate table, and FABRICATED a cost
	// for a call the gateway had explicitly declared free, counting it as priced.
	//
	// A settled zero is now a real answer that suppresses the fallback.
	Settled bool `json:"settled,omitempty"`

	// Incomplete marks CostUSD as not an EXACT total, and IncompleteReason says which
	// way it is inexact: pricing.ReasonOutputUncounted for a known-LOW figure (a stream
	// that died before the event carrying its output count — the producer's logs already
	// said "token counts will be incomplete"; this is that knowledge on the wire, where
	// the totals are actually read), or pricing.ReasonSplitUnreported for one that is
	// approximate in no known direction (a gateway reporting only total_tokens).
	//
	// A reason string beside the flag rather than one undifferentiated boolean, because
	// the two are different claims and a consumer acts on them differently. A floor is
	// bounded on one side and usually a transient failure worth chasing; an approximation
	// is unbounded and a standing property of the gateway. Collapsed into one bit they
	// would be indistinguishable, and a permanent caveat reads as an incident.
	//
	// DISCLOSURE, not adjustment. CostUSD keeps the figure, Settled stays true, and
	// Priced below still returns true — so a consumer must keep these dollars in its
	// total. The figure is the best available; estimating the missing completion would be
	// worse than reporting a known-low number and saying it is low. What the flag
	// withdraws is the claim of EXACTNESS, nothing else.
	//
	// The obligation on a consumer: do not render this as an exact figure. It stays in
	// every priced count it was already in — usage.Counts keeps it in PricedRequests,
	// which answers a different question (coverage: did anything price this) — and is
	// disclosed alongside in usage.Counts.IncompleteRequests.
	//
	// ADDITIVE and omitempty, like Provenance and OutputUSD: an event from an older
	// producer decodes here with Incomplete false, which is the pre-fix reading — every
	// figure claimed exact — so a consumer that ignores these fields is exactly as
	// correct as it was before, just less informed.
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

	// RejectedReason names a cost figure the producer REFUSED — RejectedImplausible
	// today — and is the ONLY evidence that a figure was on the wire at all.
	//
	// It exists so a refusal is a DISCLOSED COVERAGE GAP rather than silence. The
	// alternative to publishing this record is publishing nothing, and then a response
	// that reported $9,000,000,000 and a response that reported nothing are the same
	// event to every consumer — which hides both the misconfiguration and the attack.
	//
	// NOT A FIGURE, and never clamped to the bound. CostUSD stays 0 with Settled false,
	// and Priced() returns false whenever this is set, so no consumer can read a refused
	// figure as money however the other fields arrive. A clamped figure would be a wrong
	// number wearing a right label, which is worse than a named gap: the ring forgets a
	// gap in six hours, while the durable ledger keeps a wrong number for thirty days
	// with no repair path.
	//
	// LIMIT OF THE DISCLOSURE, stated because a reader will otherwise assume more: this
	// gap does not reach usage.Snapshot.UnpricedBy. That map keys on "<endpoint>
	// <model>" and counts requests that COULD have been priced, and the traffic this
	// refusal lands on carries no model at all — the extension is nil, which is
	// precisely why the figure could not be corroborated. Naming it there would mean
	// widening what "priceable" counts, in a package this record only travels to. The
	// evidence available today is this field on the record plus the operator warning
	// costing emits with the host on it.
	//
	// ADDITIVE and omitempty, like Provenance and Incomplete: an older consumer decodes
	// an event carrying it and sees an unsettled zero, which is the pre-fix reading of a
	// refused figure — unpriced — so it is exactly as correct as it was before.
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

// Micros converts CostUSD to millionths of a dollar, rounded to nearest.
//
// The integer unit is what usage.Counts accumulates: it keeps bucket addition
// exact and JSON round-tripping lossless, which float dollars are not. A cost
// below half a micro rounds to 0 while still counting as priced — a real
// sub-micro charge, not an unknown one.
//
// BOUNDED, through the same pricing.MicrosFromUSD that prices a token tally. This
// was `int64(math.Round(e.CostUSD * 1e6))` with no bound at all while pricing.Cost
// guarded the identical conversion and called the unguarded form "a garbage ledger
// figure". The header path accepts any finite non-negative float, so a gateway
// reporting 1e13 saturated to MaxInt64 and two such requests wrapped usage.Counts.Add
// to −2 micros — an aggregate that then sat in the durable ledger for thirty days
// with no repair path.
//
// The bound removed the SATURATION, not the wrap: 1024 figures at the bound still wrap
// the same sum, and no per-request bound can fix that. See MaxCostMicros, which used to
// claim otherwise.
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
func (e Event) Priced() bool {
	if e.CostUSD < 0 {
		return false
	}
	if e.RejectedReason != "" {
		return false
	}
	if _, ok := pricing.MicrosFromUSD(e.CostUSD); !ok {
		return false
	}
	return e.CostUSD > 0 || e.Settled
}

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
