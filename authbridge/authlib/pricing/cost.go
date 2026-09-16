package pricing

import "math"

// MaxCostMicros bounds ONE request's conversion into the micros unit. It is a
// REPRESENTABILITY bound and nothing more — see "what it does not bound" below, which is
// the half an earlier version of this comment got wrong.
//
// float64 counts every integer exactly only up to 2^53, so a total beyond that cannot
// round-trip through int64 meaningfully even when it fits. $9 billion for one request
// is unreachable by any legitimate traffic and is the point past which a figure is a
// bug rather than a bill.
//
// EXPORTED so the one bound serves every producer of a micros figure. costevent's
// header path accepts any finite non-negative float, and its Micros() conversion was
// unguarded — so a header of 1e13 saturated to MaxInt64 and put a garbage figure where a
// ledger row was expected. A second bound declared over there would be free to drift from
// this one; there is only ever one answer to "past which figure is this a bug".
//
// WHAT IT DOES NOT BOUND: THE ACCUMULATED SUM. This comment used to claim it closed the
// aggregate wrap — "two such requests wrapped usage.Counts.Add to a NEGATIVE total" — and
// that overclaimed. It moved the threshold; it did not remove it. math.MaxInt64 /
// MaxCostMicros is 1023, so 1024 requests each priced at the bound would wrap Counts.Add
// to a large negative total (measured: -9214364837600034816), which then sat in the
// durable ledger for its full retention with no repair path.
//
// NO per-request bound can close that, and the arithmetic says so in one line: for any
// bound C > 0 the sum wraps after ceil(math.MaxInt64/C) requests, and nothing here bounds
// the request count. A smaller C buys distance, not closure. Closing it takes a CHECKED
// ACCUMULATE where the sum is kept, which is a different package's invariant and not this
// constant's to hold. TestNoPerRequestBoundClosesTheAccumulationWrap pins that reasoning.
//
// AND IT IS NOW CLOSED THERE, so read the paragraph above as arithmetic rather than as an
// open defect: usage.Counts.Add routes every field through a checked accumulate that
// saturates instead of wrapping and sets Counts.Saturated to disclose that the figure has
// become a floor. What remains true is the part this constant is responsible for — a
// per-request bound cannot do it, and pushing MaxCostMicros lower would not have.
//
// EXCLUSIVE: a figure of exactly MaxCostMicros is out of range (MicrosFromUSD rejects
// `micros >= MaxCostMicros`). 2^53 is the first integer whose successor float64 cannot
// represent, so it is the one value in the range whose neighbourhood is two micros wide —
// 2^53+1 rounds back onto it, which makes an out-of-range figure indistinguishable from an
// in-range one at exactly that point. Excluding it makes "accepted" mean "exactly
// representable and distinct from its neighbours", which is the property the bound was
// chosen for to begin with. The doc said "above MaxCostMicros" is out of range while the
// code admitted the edge; the doc was the true half.
const MaxCostMicros = 1 << 53

// The plausibility ceiling for ONE inference call, and the two figures it is derived from.
//
// Unexported halves, exported product: the product is what callers compare against, and
// the halves are here so the number can be argued with instead of merely trusted. Both are
// deliberately literals rather than a scan of the bundled table — a cap computed from the
// live table would move when an operator adds a `pricing:` entry, which makes the bound a
// function of config that an attacker who can reach config could raise.
const (
	// maxPlausibleTokens is the largest token count one request could bill for. The
	// largest context window on any path we run is 1,000,000 tokens (the Claude [1m]
	// beta), a request bills prompt plus completion, and no completion approaches a
	// whole window — so 2,000,000 already covers the worst real call. Ten million is
	// 5x that, so a future window growth cannot turn a legitimate bill into a
	// coverage gap.
	//
	// EXPORTED AS MaxPlausibleTokens below, because authlib/usage needs the same bound to
	// refuse an implausible token report and had restated it as a second literal. Two
	// literals that must agree is the shape that let the retention floor drift from the
	// window it protects; one definition and one importer is the fix.
	maxPlausibleTokens = 10_000_000

	// maxPlausibleMicrosPerToken is the highest per-token rate one tier could carry, in
	// micros: 1,000 micros = $0.001/token = $1,000 per million tokens. The most
	// expensive tier in the bundled VENDOR LIST table is $7.5e-05/token (Claude 3 Opus
	// / Opus 4 output, $75/Mtok), so this is 13.3x the dearest RAW rate that ships today.
	//
	// THE EFFECTIVE MARGIN IS 1.33x, NOT 13x, and the difference is a multiplier. A
	// resolved rate is the table rate times MultiplierRule.Factor, which validate caps at
	// maxMultiplier = 10 — so what this ceiling has to clear is not the dearest rate in
	// the table but the dearest rate a legitimate config can PRODUCE from it: 7.5e-05 x 10
	// = 7.5e-04/token, against a ceiling of 1e-03. That is 1.33x of headroom.
	//
	// The comment used to claim the 13x and stop there, which overstated its own
	// protection: a $100/Mtok model at a 10x markup reaches the cap, and
	// TestMaxPlausibleRequestCostMicros_Derivation only ever scanned the raw Bundled()
	// rates, so it would have gone green while it happened. It now applies maxMultiplier
	// to the scan, and fails when the REAL headroom is exhausted rather than when the raw
	// rate is.
	//
	// 1.33x is thin but it is not the number to raise on its own: this ceiling times
	// maxPlausibleTokens is what gives the cap three orders of magnitude over the worst
	// call anyone can actually construct (see MaxPlausibleRequestCostMicros), and a 10x
	// markup over vendor list is arguably not a legitimate config in the first place. If a
	// bundled rate ever moves past $100/Mtok, the derivation test is what will say so, and
	// both halves get revisited then.
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
// The margin is honest about being coarse: the worst real call we can construct — a
// 1M-token opus-5 prompt plus a 64k completion — is about $7, so both roundings together
// leave roughly three orders of magnitude of headroom. That is chosen on purpose. The cap
// exists to make a forged figure merely wrong rather than catastrophic; setting it near
// real traffic would start refusing real bills the first time a vendor reprices, and a
// refused real bill is a coverage gap that looks exactly like this defect.
//
// INCLUSIVE, unlike MaxCostMicros, and the asymmetry is not an oversight: 1e10 micros is
// exactly representable in float64 with exactly-representable neighbours, so there is no
// ambiguity at the edge to exclude, and the derivation reads "the most a request could
// plausibly cost" — that figure is by construction still plausible.
const MaxPlausibleRequestCostMicros int64 = maxPlausibleTokens * maxPlausibleMicrosPerToken

// MaxPlausibleTokens is the largest token count one request could plausibly report, per
// field. See maxPlausibleTokens for the derivation — this is that constant, exported.
//
// It exists because authlib/usage refuses an implausible token report and needs the same
// bound: the token fields arrive as a provider-controlled `int` on the wire, so without one
// a forged response that cannot move the dollar total past $10,000 could still move the
// token total by 9.2e18, and a client renders the two side by side.
//
// ONE DEFINITION, ONE IMPORTER, deliberately. usage restated this as a second literal with a
// comment recording the duplication as debt — and it was right to: two literals that must
// agree is precisely the shape that let config's retention floor drift from the window span
// it protects, shipping a floor of 7 against a window that opens 8 files. Exporting the bound
// here and deriving there means a future change to the plausible ceiling cannot move one
// without the other.
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
// The one conversion, because there is one bound. Both callers previously wrote
// `int64(math.Round(usd * 1e6))` by hand and only this package's checked the range;
// see MaxCostMicros for what the unchecked one produced.
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
//     gap rather than a total that is quietly too low. This is toolprune's
//     rateFor rule (plugin.go:155-174) promoted — see the plan for the failure it
//     was written against.
//   - No tokens were reported at all. That is unknown usage, not a free request;
//     counting it as priced-zero would dilute the coverage denominator.
//   - A negative count, which is a parser bug or a hostile body. A negative cost
//     would corrode a running total that nothing re-derives.
//   - No rates at all, which is the ProvNone case reaching here directly.
//   - A rate that is negative or non-finite, wherever it came from. Trusting the
//     rate while checking the count would let a hostile or buggy producer emit
//     negative money or MaxInt64 micros.
//
// A tier with no rate but no tokens is fine: toolprune's table has no output rate
// at all, and refusing there would unprice every request it measures.
func Cost(r Rates, u Usage) (int64, bool) {
	if u == (Usage{}) {
		return 0, false
	}
	eff := r.At(u.PromptTotal())
	var usd float64
	for i, n := range u.tokens() {
		if n < 0 {
			return 0, false
		}
		if n == 0 {
			continue
		}
		if !eff.Set[i] {
			return 0, false
		}
		// The RATE is validated here, not only at config time. Cost previously
		// checked the token count and trusted the rate, so a negative rate yielded
		// ok=true with negative micros, +Inf yielded MaxInt64, and NaN was
		// architecture-dependent. Only config.Build validated rates, which leaves
		// every other producer unguarded — including the ProvDiscovered /model/info
		// path, where the numbers come from a remote gateway.
		if r := eff.Base[i]; r < 0 || math.IsNaN(r) || math.IsInf(r, 0) {
			return 0, false
		}
		usd += float64(n) * eff.Base[i]
	}
	// Bound-checked before conversion, in MicrosFromUSD. Each RATE is validated above,
	// but a finite rate times a large token count still accumulates past int64: the
	// conversion would then be undefined and return ok=true with a garbage ledger
	// figure, which is worse than reporting the request unpriced.
	return MicrosFromUSD(usd)
}
