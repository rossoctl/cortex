package usage

import "github.com/rossoctl/cortex/core/cost/pricing"

// ApportionTiers splits CostMicros across the rate tiers by the modelled mix.
//
// THE MIX IS THE RATE TABLE'S, THE MAGNITUDE IS THE GATEWAY'S. A request priced from a
// response header has an authoritative total and no breakdown, because no gateway publishes
// one — so the modelled figures cannot be shown as dollars beside it without putting two
// disagreeing totals on screen. Used as a ratio they answer the question anyway, and the
// column adds up to the headline a reader checks first.
//
// ok is false when there is no mix (nothing to apportion by) or no usable total (nothing to
// apportion). Callers render the "not known here" cell — NOT four zeros, which would report
// the traffic as free, and not a guess.
//
// NO COVERAGE THRESHOLD, deliberately. A mix drawn from one request of forty is a weak key,
// but every particular floor is a number nobody can defend — 50% and 10% are equally
// arbitrary — and a constant whose value is unjustifiable is worse than the behaviour it
// guards. The display marks every figure inexact instead, and a reader who wants coverage
// has `abctl cost`, which reports priced against priceable already.
//
// THE ONE PLACE THIS ARITHMETIC LIVES. The drawer, the text command and the JSON all call
// it, so three surfaces cannot disagree about a figure derived three times.
func (c Counts) ApportionTiers() (tiers [pricing.NumTiers]int64, ok bool) {
	mix := [pricing.NumTiers]int64{
		pricing.TierInput:      c.InputCostMicros,
		pricing.TierCacheWrite: c.CacheWriteCostMicros,
		pricing.TierCacheRead:  c.CacheReadCostMicros,
		pricing.TierOutput:     c.OutputCostMicros,
	}
	var mixTotal int64
	for _, v := range mix {
		if v > 0 {
			mixTotal += v
		}
	}
	// A negative total is not a total, the refusal every money surface in this repo makes.
	if mixTotal <= 0 || c.CostMicros <= 0 {
		return tiers, false
	}

	// THE RATIO IN FLOAT, THE RESULT BOUNDED BY THE TOTAL. The integer form,
	// CostMicros*mix[i]/mixTotal, overflows int64 on the MULTIPLY for a large total against a
	// large mix — and both are sums over a whole window, so neither is small. Scaling keeps
	// every intermediate at or under CostMicros, a quantity already known to fit.
	var sum int64
	largest := -1
	for i, v := range mix {
		if v <= 0 {
			// ABSENT FROM THE MIX MEANS ABSENT FROM THE ANSWER. A tier that priced nothing
			// gets nothing, rather than a share of the rounding: inventing a cache-write
			// charge for traffic that wrote no cache is the kind of small lie this surface
			// refuses everywhere else.
			continue
		}
		tiers[i] = int64(float64(c.CostMicros) * (float64(v) / float64(mixTotal)))
		sum += tiers[i]
		if largest < 0 || v > mix[largest] {
			largest = i
		}
	}
	// THE REMAINDER GOES TO THE LARGEST TIER. Four truncations need not sum to one total, and
	// the display's whole claim is that they do. Largest because that is where a micro is
	// proportionally smallest and cannot flip a rank — giving it to the smallest tier could
	// reorder the bars, which is a visible lie in the service of an invisible one.
	if r := c.CostMicros - sum; r != 0 && largest >= 0 {
		tiers[largest] += r
	}
	return tiers, true
}

// ApportionReasoning is the reasoning share of an already-apportioned output figure,
// in micros.
//
// HERE RATHER THAN IN A RENDERER, for the reason ApportionTiers gives for itself: one
// place, so the surfaces cannot disagree about a figure derived more than once. Kept
// in this package is also what lets `abctl cost --json` publish it, instead of leaving
// a consumer to reimplement the rule — which is what costJSON.Tiers refuses for the
// tier split.
//
// outputMicros is the DISPLAYED output figure, not c.OutputCostMicros: the displayed
// one is already scaled to the gateway's authoritative total, so deriving from the raw
// mix would produce a child that does not divide into the parent beside it.
//
// A TOKEN RATIO APPLIED TO A COST FIGURE, which is a second approximation on top of
// ApportionTiers' own. Reasoning's share of output COST equals its share of output
// TOKENS only where every model in the window bills output at one rate. Across a mixed
// window it can be off by the spread between those rates — an expensive model that did
// no reasoning beside a cheap one that was nearly all reasoning is the worst case, and
// the error there is an order of magnitude, not a rounding.
//
// Accepted because there is no better source: nothing reports a ReasoningCostMicros,
// and the alternative is showing no figure at all for the component this feature exists
// to expose. Callers must present it as modelled — `abctl cost` and the drawer already
// mark the tier split that way, and the README says so for the child specifically.
//
// ok is false when there is no defensible figure, and the caller renders "not known
// here" — never $0.00, which would assert the reasoning was free: a count that is zero
// or negative, a missing denominator, or a share that truncates below one micro.
//
// THE PRESENT BIT IS NOT CONSULTED HERE, because neither "nothing reported" nor
// "reported zero" has a figure to apportion. The bit separates them, and a caller that
// needs to — the spend drawer does, to choose between the not-known cell and an exact
// "$0.00" — reads PresentKinds itself. A positive count with the bit CLEAR does
// apportion: that is a producer predating PresentKinds, where the value is the only
// evidence there is.
//
// The result is clamped to outputMicros: a provider reporting reasoning above output
// must not yield a child figure above its parent, though the counts themselves are left
// as reported — see Counts.ReasoningTokens.
func (c Counts) ApportionReasoning(outputMicros int64) (micros int64, ok bool) {
	// Guarded HERE and not left to plausibleTokenReport's ingest screen, which is the
	// defence addSat refuses for itself in this package: "unreachable today" is how the
	// wrap arrived, and a figure guarded on one side invites a reader to conclude the
	// other was ruled out. Nothing below catches a negative — the clamp is upper-only and
	// the truncation escape tests for zero.
	if c.ReasoningTokens <= 0 {
		return 0, false
	}
	if c.OutputTokens <= 0 || outputMicros <= 0 {
		return 0, false
	}
	// Float ratio bounded by the parent, the form ApportionTiers uses and for its
	// reason: the integer product of two window-sized sums overflows int64.
	micros = int64(float64(outputMicros) *
		(float64(c.ReasoningTokens) / float64(c.OutputTokens)))
	if micros > outputMicros {
		micros = outputMicros
	}
	if micros == 0 {
		return 0, false
	}
	return micros, true
}
