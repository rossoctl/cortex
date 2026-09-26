package pricing

import "math"

// MaxCostMicros bounds ONE request's conversion into the micros unit. It is a
// REPRESENTABILITY bound and nothing more — see "what it does not bound" below, which is the
// half that is easy to overstate.
//
// float64 counts every integer exactly only up to 2^53, so a total beyond that cannot
// round-trip through int64 meaningfully even when it fits. $9 billion for one request
// is unreachable by any legitimate traffic and is the point past which a figure is a
// bug rather than a bill.
//
// EXPORTED so the one bound serves every producer of a micros figure. cost/event's
// header path accepts any finite non-negative float, and its Micros() conversion was
// unguarded — so a header of 1e13 saturated to MaxInt64 and put a garbage figure where a
// ledger row was expected. A second bound declared over there would be free to drift from
// this one; there is only ever one answer to "past which figure is this a bug".
//
// WHAT IT DOES NOT BOUND: THE ACCUMULATED SUM. 1,024 requests at this bound wrap
// usage.Counts.Add to a large negative total, and NO per-request bound can close that — for any
// bound C the sum wraps after MaxInt64/C requests, so a smaller C buys distance, not closure.
// Closing it takes a checked accumulate where the sum is kept, which the aggregate PR in this
// series does. TestNoPerRequestBoundClosesTheAccumulationWrap pins the reasoning; until that PR
// lands this is an open defect and not history, reachable through a header on a parsed path.
//
// EXCLUSIVE: exactly MaxCostMicros is out of range. 2^53 is the first integer whose successor
// float64 cannot represent, so 2^53+1 rounds back onto it — at that one value an out-of-range
// figure is indistinguishable from an in-range one. Excluding it makes "accepted" mean "exactly
// representable and distinct from its neighbours".
const MaxCostMicros = 1 << 53

// The plausibility ceiling for ONE inference call, and the two figures it is derived from.
//
// Unexported halves, exported product: the product is what callers compare against, and
// the halves are here so the number can be argued with instead of merely trusted. Both are
// deliberately literals rather than a scan of the bundled table — a cap computed from the
// live table would move when an operator adds a `pricing:` entry, which makes the bound a
// function of config that an attacker who can reach config could raise.
const (
	// maxPlausibleTokens is the largest token count one request could bill for. The largest
	// context window on any path we run is 1,000,000 tokens (the Claude [1m] beta) and a request
	// bills prompt plus completion, so 2,000,000 covers the worst real call; ten million is 5x
	// that, so window growth cannot turn a legitimate bill into a coverage gap. Exported as
	// MaxPlausibleTokens below so core/cost/usage derives the same bound instead of restating it.
	maxPlausibleTokens = 10_000_000

	// maxPlausibleMicrosPerToken is the highest per-token rate one tier could carry: 1,000
	// micros = $0.001/token = $1,000/Mtok. The dearest RAW rate in the bundled vendor list is
	// $75/Mtok, which reads as 13x of headroom.
	//
	// THE EFFECTIVE MARGIN IS 1.33x, NOT 13x, and quoting the 13x overstates this bound: a
	// resolved rate is the table rate times a multiplier that validate caps at 10, so what this
	// has to clear is 7.5e-04/token against a ceiling of 1e-03. A $100/Mtok model at a 10x markup
	// reaches the cap. TestMaxPlausibleRequestCostMicros_Derivation applies that multiplier to
	// its scan of Bundled() for exactly this reason — a scan of raw rates would stay green
	// through it — and is what will say so if a bundled rate moves past $100/Mtok.
	maxPlausibleMicrosPerToken = 1_000
)

// MaxPlausibleRequestCostMicros is the most ONE inference request could plausibly cost:
// maxPlausibleTokens at maxPlausibleMicrosPerToken, which is 1e10 micros — $10,000.
//
// A BLAST-RADIUS CAP, NOT AUTHENTICATION. It says nothing about who reported the figure
// and cannot: a gateway's cost header is an unauthenticated string on a response, and any
// host a client is proxied to can put any number in it. What the cap does is bound what a
// figure this process could not corroborate is allowed to contribute, so that one forged
// header cannot exhaust a daily budget, poison a thirty-day ledger row, or push an
// aggregate towards the int64 wrap that MaxCostMicros documents. What it does NOT do:
// stop a forgery UNDER the cap (that is spend an operator has to reconcile against the
// gateway's own accounting), authenticate the reporting host (a host allowlist would, and
// remains the stronger fix), or bound the accumulated sum (see MaxCostMicros).
//
// COARSE ON PURPOSE. The worst real call anyone can construct — a 1M-token opus-5 prompt plus a
// 64k completion — is about $7, so this leaves three orders of magnitude of headroom. The cap
// exists to make a forged figure merely wrong rather than catastrophic; set near real traffic it
// would refuse real bills the first time a vendor reprices, which is a coverage gap that looks
// exactly like the defect it guards.
//
// INCLUSIVE, unlike MaxCostMicros: 1e10 micros is exactly representable with representable
// neighbours, so there is no ambiguous edge to exclude, and "the most a request could plausibly
// cost" is by construction still plausible.
const MaxPlausibleRequestCostMicros int64 = maxPlausibleTokens * maxPlausibleMicrosPerToken

// MaxPlausibleTokens is the largest token count one request could plausibly report, per
// field. See maxPlausibleTokens for the derivation — this is that constant, exported.
//
// It exists because core/cost/usage refuses an implausible token report and needs the same
// bound: the token fields arrive as a provider-controlled `int` on the wire, so without one
// a forged response that cannot move the dollar total past $10,000 could still move the
// token total by 9.2e18, and a client renders the two side by side.
//
// NO IMPORTER ON THIS BRANCH: the consumers are usage.maxPlausibleRequestTokens (#1013 of this
// stack) and the ledger writer's admission guard (#1014), so a grep at this commit finds only the
// declaration. Exported anyway because the split put the derivation here and its readers in the
// next two PRs — and because two literals that must agree is the shape that let config's
// retention floor drift from the window it protects.
const MaxPlausibleTokens = maxPlausibleTokens

// PlausibleRequestCostUSD reports whether usd could be what ONE inference request cost.
//
// The predicate lives beside the bound so no caller re-derives the comparison, and so the
// out-of-range case cannot be forgotten: a figure too large for MicrosFromUSD is not
// plausible either, and reading `micros <= MaxPlausibleRequestCostMicros` off a conversion
// that failed would compare against a zero and call 1e300 plausible.
func PlausibleRequestCostUSD(usd float64) bool {
	micros, ok := MicrosFromUSD(usd)
	return ok && micros <= MaxPlausibleRequestCostMicros
}

// MicrosFromUSD converts a dollar figure to integer micros — millionths of a dollar —
// reporting false when the result is not a usable ledger figure.
//
// The one conversion, because there is one bound. `int64(math.Round(usd * 1e6))` written by
// hand at a call site is the shape this replaces — unchecked, it saturates to MaxInt64 and
// puts a garbage figure where a ledger row is expected; see MaxCostMicros.
//
// ok is false for NaN, an infinity, a negative figure, or anything AT OR ABOVE
// MaxCostMicros. A caller must treat that as UNPRICED and not as a large number: a
// clamped figure is a wrong number wearing a right label, and the ring forgets it in
// six hours while the durable ledger keeps it for thirty days with no repair path.
//
// The SIGN IS CHECKED ON THE INPUT, before rounding, because it does not survive the
// rounding. math.Round(-1e-07 * 1e6) is math.Round(-0.1), which is NEGATIVE ZERO, and
// `-0.0 < 0` is false in Go — so this returned (0, true) for every figure in
// (-5e-07, 0), calling a wrong-signed figure a priced zero and contradicting the
// paragraph above. Anything at or beyond -5e-07 rounded to -1 and was caught, which
// is why the hole was only ever the tiny end.
//
// An input of exactly -0.0 IS ZERO and stays priced: `-0.0 < 0` is false here too, and
// that is correct rather than incidental — negative zero is a value of zero, a settled
// zero is a producer saying the call was free, and refusing it would report a genuine
// free call as unpriced traffic.
//
// NaN does not reach this guard (every comparison against NaN is false) and is still
// caught below, where it always was.
func MicrosFromUSD(usd float64) (int64, bool) {
	if usd < 0 {
		return 0, false
	}
	micros := math.Round(usd * 1e6)
	// No `micros < 0` check: a non-negative input cannot round to a negative figure,
	// so the input guard above subsumes it. Keeping both would leave the impression
	// that the rounded sign is load-bearing, which is the belief that produced the bug.
	// `>=`, not `>`. The bound is documented as the point past which a figure is a bug,
	// and the edge itself is the one value that cannot be told apart from a figure past
	// it: 2^53+1 rounds back onto 2^53, so accepting the edge accepts an ambiguity. See
	// MaxCostMicros.
	if math.IsNaN(micros) || math.IsInf(micros, 0) || micros >= MaxCostMicros {
		return 0, false
	}
	return int64(micros), true
}

// Cost prices u at r, returning integer micros — millionths of a dollar.
//
// Micros because usage.Counts.CostMicros is already that unit, and integer
// addition across ring buckets is exact where repeated float addition is not.
//
// ok is false when the request is UNPRICED, which is a different answer from a
// cost of zero. Four ways to be unpriced:
//
//   - A tier that carried tokens has no rate. The invariant: a request is priced
//     only if every tier it used had a rate, so a partial table produces a named
//     gap rather than a total that is quietly too low. toolprune holds the same
//     rule on the counting side (metrics.unpriced): name the gap rather than
//     charge one model's tokens at another model's rate.
//   - No tokens were reported at all. That is unknown usage, not a free request;
//     counting it as priced-zero would dilute the coverage denominator.
//   - A negative count, which is a parser bug or a hostile body. A negative cost
//     would corrode a running total that nothing re-derives.
//   - A count above maxPlausibleTokens, on the same reasoning and from the same
//     wire. THE MODELLED FIGURE WAS THE UNBOUNDED ONE: headerCost refuses a
//     gateway's figure past MaxPlausibleRequestCostMicros ($10,000), and
//     the aggregate PR in this series WILL refuse an implausible token report and
//     count it (usage.plausibleTokenReport, Counts.RefusedTokenRequests — neither
//     exists at this commit) — while the money derived from those same counts
//     arrived here and was believed, bounded only by MaxCostMicros, which is
//     $9 billion. So one response could have its tokens called impossible and its
//     dollars kept, in the same aggregate, side by side in the same client.
//   - A modelled figure past MaxPlausibleRequestCostMicros ($10,000), which is the
//     ceiling a gateway's own figure is already held to. Bounding the counts does NOT
//     make this bound follow arithmetically, tempting as that reading is: the token
//     check is per TIER, so a request can carry maxPlausibleTokens several times over,
//     and a base rate has no magnitude bound at all. The two are checked separately
//     because neither implies the other.
//   - No rates at all, which is the ProvNone case reaching here directly.
//   - A rate that is negative or non-finite, wherever it came from. Trusting the
//     rate while checking the count would let a hostile or buggy producer emit
//     negative money or MaxInt64 micros.
//
// A tier with no rate but no tokens is fine: toolprune's table has no output rate
// at all, and refusing there would unprice every request it measures.
func Cost(r Rates, u Usage) (int64, bool) {
	micros, ok, _ := CostWithReason(r, u)
	return micros, ok
}

// Refusal names WHY Cost declined to price a request, for the one caller that has to tell the
// causes apart.
//
// A wire-shaped string like IncompleteReason, but this one is NOT a wire value: it exists so
// settle.Settle can decide whether a refusal is worth DISCLOSING on the published record.
// Two of these are claims about impossible input and belong in front of an operator; the rest
// are ordinary coverage gaps — no rates configured for a model, no counters in the response —
// which a client already renders as unpriced and which would be noise as a disclosure.
type Refusal string

const (
	// RefusalNone: nothing was refused.
	RefusalNone Refusal = ""
	// RefusalNoTokens: the response reported no counters at all, so there is nothing to
	// price. Unknown usage, not a free request.
	RefusalNoTokens Refusal = "no-tokens"
	// RefusalImpossibleCount: a count no request could have reported — negative, or past
	// maxPlausibleTokens. DISCLOSED, because it is a statement about the response's own
	// numbers rather than about our configuration.
	RefusalImpossibleCount Refusal = "impossible-count"
	// RefusalNoRate: a tier that carried tokens had no rate, or the rate was negative or
	// non-finite. A configuration gap, and the reason a partial total is never presented as
	// a whole one.
	RefusalNoRate Refusal = "no-rate"
	// RefusalUnrepresentable: the arithmetic left the micros unit, which MicrosFromUSD
	// refuses rather than return a garbage ledger figure.
	RefusalUnrepresentable Refusal = "unrepresentable"
	// RefusalImplausibleTotal: a figure past MaxPlausibleRequestCostMicros ($10,000), the
	// same ceiling a gateway's own header is held to. DISCLOSED, because the cause is on our
	// side — an operator's typo in a rate, or a rate discovered from a gateway — and it
	// unprices every request that rate touches while looking like traffic nobody had rates
	// for.
	RefusalImplausibleTotal Refusal = "implausible-total"
)

// ImpossibleFigure reports whether a refusal was about the SIZE of the derived figure rather than a
// missing or unusable input.
//
// EXPORTED FOR THE SAME REASON AS PlausibleUsage: the question is asked in two places and must have
// one answer. settle.Settle asks it to refuse both HALVES of a request whose whole was refused, and
// again to disclose that refusal on the record.
//
// TWO REFUSALS SAY IT, AND WHICH ONE APPEARS IS DECIDED BY CHECK ORDER, NOT BY MAGNITUDE. Cost tests
// MicrosFromUSD before MaxPlausibleRequestCostMicros, so a figure is RefusalImplausibleTotal only
// while it stays under MaxCostMicros (~$9.007e9); past that it is RefusalUnrepresentable, which is
// the LARGER and more obviously broken case. Keying a caller on the plausibility label alone
// therefore covers the smaller half of the range and silently drops the rest — measured at $1e6 per
// input token: the whole and the prompt half were refused as unrepresentable, the $5,000 output half
// came back priced, and because the disclosure keyed on the same label the record carried NO
// RejectedReason at all. An unnameable coverage gap is the failure refusing-rather-than-clamping
// exists to prevent, so both labels are named here, once.
func (r Refusal) ImpossibleFigure() bool {
	return r == RefusalImplausibleTotal || r == RefusalUnrepresentable
}

// PlausibleUsage reports whether every counter in u could be a real report: non-negative, and no
// larger than one request could bill for.
//
// EXPORTED BECAUSE THE QUESTION IS ASKED IN TWO PLACES AND MUST HAVE ONE ANSWER. Cost asks it while
// pricing; settle.Settle asks it BEFORE deriving anything, because a figure derived from an
// impossible count has to be refused whole — and inferring that from Cost's refusal reason does not
// work: Cost returns the FIRST problem it meets, so a missing rate on an earlier tier masks the
// count entirely. Measured with a table that has no input rate: {Input: 1000, CacheRead: -1,
// Output: 50} refused as "no-rate", and the output half — which zeroes the offending tier along
// with the prompt — came back priced at $0.0005 on a record disclosing nothing.
//
// These counters are provider-controlled integers on the wire, which is why the bound exists at
// all; see maxPlausibleTokens for where the ceiling comes from.
func PlausibleUsage(u Usage) bool {
	for _, n := range u.tokens() {
		if n < 0 || n > maxPlausibleTokens {
			return false
		}
	}
	return true
}

// MicrosOrZero is MicrosFromUSD for a caller with nothing useful to do with a refusal.
//
// For figures that are a RATIO rather than a total: an unrepresentable component makes the
// ratio less complete and nothing else, where the same failure in a total would be a
// garbage ledger figure. Do not use it for money anyone is charged.
func MicrosOrZero(usd float64) int64 {
	m, ok := MicrosFromUSD(usd)
	if !ok {
		return 0
	}
	return m
}

// CostByTier is Cost's arithmetic with the per-tier amounts kept instead of summed away.
//
// ONE PASS, ONE `eff`. The alternative is what Settle does for its prompt and output
// halves: call Cost again with the other tiers zeroed. That re-resolves the rate table
// against a MODIFIED prompt total on each call and re-applies the per-request
// plausibility ceiling to each fragment, which is forty lines of guard reasoning in
// settle.go. This returns a decomposition of the same float sum the total is built
// from, so the tiers are mutually consistent by construction and no fragment can pass
// or fail a bound the whole did not.
//
// THE TIERS NEED NOT SUM EXACTLY TO total. Each is converted to micros independently, so
// four truncations can differ from one by a few micros. Callers use them as a RATIO — see
// usage.Counts.ApportionTiers — where truncation is invisible. Nothing may present this
// array as a total.
//
// The refusals below are Cost's own, unchanged: this function IS Cost's body, and
// CostWithReason now delegates to it.
func CostByTier(r Rates, u Usage) (tiers [NumTiers]int64, total int64, ok bool, refusal Refusal) {
	if u == (Usage{}) {
		return tiers, 0, false, RefusalNoTokens
	}
	// FIRST, so the reason is stable. Asked before the per-tier loop below because the loop
	// returns on the first problem it meets, and an impossible COUNT is the one refusal a caller
	// acts on differently — costing refuses every figure derived from the report. Checked here, a
	// missing rate can no longer mask it.
	if !PlausibleUsage(u) {
		return tiers, 0, false, RefusalImpossibleCount
	}
	eff := r.At(u.PromptTotal())
	var usd float64
	// perTier keeps what the loop below used to discard.
	var perTier [NumTiers]float64
	for i, n := range u.tokens() {
		if n == 0 {
			continue
		}
		if !eff.Set[i] {
			return [NumTiers]int64{}, 0, false, RefusalNoRate
		}
		// The RATE is validated here, not only at config time. Checking the token
		// count and trusting the rate is not enough: a negative rate yields ok=true
		// with negative micros, +Inf yields MaxInt64, and NaN is
		// architecture-dependent. config.Build validates rates it loads, which leaves
		// every other producer unguarded — including the ProvDiscovered /model/info
		// path, where the numbers come from a remote gateway.
		if r := eff.Base[i]; r < 0 || math.IsNaN(r) || math.IsInf(r, 0) {
			return [NumTiers]int64{}, 0, false, RefusalNoRate
		}
		perTier[i] = float64(n) * eff.Base[i]
		usd += perTier[i]
	}
	// Bound-checked before conversion, in MicrosFromUSD. Each RATE is validated above,
	// but a finite rate times a large token count still accumulates past int64: the
	// conversion would then be undefined and return ok=true with a garbage ledger
	// figure, which is worse than reporting the request unpriced.
	micros, converted := MicrosFromUSD(usd)
	if !converted {
		return [NumTiers]int64{}, 0, false, RefusalUnrepresentable
	}
	// AND HELD TO THE SAME PER-REQUEST CEILING AS A GATEWAY'S OWN FIGURE. MicrosFromUSD
	// alone bounds this at MaxCostMicros — $9 billion, the figure this file calls a garbage
	// ledger figure — while costing applies PlausibleRequestCostUSD ($10,000) to the header
	// path. Two ways to reach the gap: the token check above is PER TIER, so a request can
	// carry maxPlausibleTokens several times over, and a base RATE has no magnitude bound at
	// all (config.Build validates only sign and finiteness, and the ProvDiscovered
	// /model/info path is not config). So an operator typo or a remote gateway's number
	// could produce a modelled figure between $10,000 and $9 billion, priced and settled,
	// where the same figure in a response header would have been refused and named.
	//
	// Refused rather than clamped, like every other implausible input here: the request
	// stays priceable-and-unpriced, which is a coverage gap a client already renders.
	if micros > MaxPlausibleRequestCostMicros {
		return [NumTiers]int64{}, 0, false, RefusalImplausibleTotal
	}
	for i, v := range perTier {
		// A tier that will not convert leaves THAT tier at zero rather than refusing the
		// request: the whole has already been bounded and accepted above, so the ratio is
		// merely less complete — the case ApportionTiers already handles — and refusing
		// here would unprice a request Cost priced.
		if m, converted := MicrosFromUSD(v); converted {
			tiers[i] = m
		}
	}
	return tiers, micros, true, RefusalNone
}

// CostWithReason is Cost, and says why when it refuses.
//
// Split out rather than folded into Cost's signature because three call sites want the figure
// and one wants the cause: settle.Settle publishes a refusal an operator can act on, and
// telling "this rate table is wrong" from "this model has no rates" is the whole difference
// between a signal and a shrug. Every refusal below is one of Cost's documented four-plus ways
// to be unpriced, named rather than collapsed into a bare false.
//
// A two-line delegation to CostByTier, which holds the arithmetic and every refusal
// above. Signature unchanged: three call sites want the figure and one wants the cause,
// and none of them needs the split.
func CostWithReason(r Rates, u Usage) (int64, bool, Refusal) {
	_, micros, ok, refusal := CostByTier(r, u)
	return micros, ok, refusal
}
