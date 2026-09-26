package pricing

import (
	"math"
	"testing"
)

// This file holds the two bounds this package publishes and the boundary discipline of
// each: MaxCostMicros, which says what figure the micros UNIT can hold, and
// MaxPlausibleRequestCostMicros, which says what figure one inference CALL could be.
//
// They are different questions with different edges — one exclusive for a
// representability reason, one inclusive by construction — and the last test in the file
// is the one that says out loud what neither of them does: bound the accumulated sum.

// TestMicrosFromUSD_BoundIsExclusive walks MaxCostMicros from both sides.
//
// The conversion read `micros > MaxCostMicros` while the doc called anything above the
// bound out of range, so the edge itself was accepted. It is the ONE value in the range
// that cannot be distinguished from a figure past it: 2^53 is the first integer whose
// successor float64 cannot represent, so 2^53+1 rounds back onto 2^53 and an out-of-range
// figure arrives here looking exactly like an in-range one.
//
// Both sides are asserted because an over-correction is equally wrong: rejecting a figure
// under the bound would move a real, exactly-representable price into the coverage gap.
//
// Rows are written in MICROS and divided at the call, because that is the unit the bound is
// stated in — and the arithmetic below is the whole reason the edge is excluded. Two micros
// under the bound round-trips through the USD argument exactly; ONE micro under does not,
// because 2^53 micros is $9,007,199,254.740992 and the float64 spacing at that magnitude is
// wider than a micro, so 2^53-1 rounds back UP onto the edge. That is the ambiguity: the
// edge is reachable from both an in-range figure and an out-of-range one, so it cannot be
// treated as in range.
func TestMicrosFromUSD_BoundIsExclusive(t *testing.T) {
	for _, tc := range []struct {
		name    string
		micros  float64
		wantOK  bool
		wantMic int64
	}{
		{"a dollar under the bound is a figure", MaxCostMicros - 1e6, true, MaxCostMicros - 1e6},
		{"two micros under, the closest figure the USD unit can express", MaxCostMicros - 2, true, MaxCostMicros - 2},
		// One micro under is NOT a figure, and that is not a bug in the guard: the USD
		// argument cannot express it at this magnitude, so it arrives here as the edge.
		{"one micro under rounds onto the edge and goes with it", MaxCostMicros - 1, false, 0},
		{"the edge itself is out of range", MaxCostMicros, false, 0},
		{"past the edge", MaxCostMicros + 1e6, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			micros, ok := MicrosFromUSD(tc.micros / 1e6)
			if ok != tc.wantOK {
				t.Errorf("MicrosFromUSD(%v micros) ok = %v, want %v — the bound is exclusive: the doc calls the edge out of range, and the edge is where 2^53+1 rounds back onto 2^53",
					tc.micros, ok, tc.wantOK)
			}
			if micros != tc.wantMic {
				t.Errorf("MicrosFromUSD(%v micros) = %d, want %d", tc.micros, micros, tc.wantMic)
			}
		})
	}
}

// TestMicrosFromUSD_TheEdgeIsAmbiguousInFloat64 states the language fact the exclusive
// bound rests on, so an edit that relaxes `>=` back to `>` fails with the reason rather
// than with a bare comparison.
func TestMicrosFromUSD_TheEdgeIsAmbiguousInFloat64(t *testing.T) {
	const edge = float64(MaxCostMicros)
	if edge+1 != edge {
		t.Fatal("2^53+1 is representable in this float64; the premise of the exclusive bound is gone and the comment in MaxCostMicros needs rewriting")
	}
	if edge-1 == edge {
		t.Fatal("2^53-1 is NOT distinct from 2^53 here; the test below asserts a figure the unit cannot hold")
	}
	// AND THE BEHAVIOUR THAT PREMISE JUSTIFIES, asserted through the function rather than
	// left as arithmetic: a figure one micro below the bound rounds ONTO the bound, so both
	// are refused. That is what "the edge is ambiguous" means operationally — accepting it
	// would accept a figure that may have come from either side.
	if got := math.Round((edge - 1) / 1e6 * 1e6); got != edge {
		t.Errorf("(2^53-1) micros round-tripped through USD to %.0f, want the edge %.0f — the ambiguity the exclusive bound is chosen for is gone", got, edge)
	}
	if micros, ok := MicrosFromUSD(edge / 1e6); ok {
		t.Errorf("MicrosFromUSD refused nothing at the bound: got %d, ok=true", micros)
	}
	if micros, ok := MicrosFromUSD((edge - 1) / 1e6); ok {
		t.Errorf("MicrosFromUSD accepted %d micros for a figure one micro under the bound; it "+
			"rounds onto the bound, which is the ambiguity the exclusive comparison exists for", micros)
	}
	// One micro under, expressed so it does NOT round onto the edge, is accepted — otherwise
	// the two refusals above would also be satisfied by a function that refuses everything.
	if _, ok := MicrosFromUSD((edge - 1024) / 1e6); !ok {
		t.Error("MicrosFromUSD refused a figure comfortably inside the bound; the assertions " +
			"above would then prove nothing")
	}
}

// TestMaxPlausibleRequestCostMicros_Derivation checks the cap against the things it is
// derived FROM, so it cannot quietly become a magic literal.
//
// A cap is only as good as its derivation, and both halves of this one are claims about
// the outside world: a token ceiling and a rate ceiling. The rate half is checked against
// the BUNDLED TABLE rather than against a comment, so a future `make pricing-table` that
// pulls in a model dearer than $0.001/token fails here and forces the derivation to be
// revisited instead of silently invalidating it.
func TestMaxPlausibleRequestCostMicros_Derivation(t *testing.T) {
	// No `== maxPlausibleTokens * maxPlausibleMicrosPerToken` assertion: that is the constant's
	// own definition, so it holds by construction and could never fail. The figures below are
	// the ones that can — a literal, and a scan of the real table.
	if MaxPlausibleRequestCostMicros != 10_000_000_000 {
		t.Errorf("MaxPlausibleRequestCostMicros = %d micros, want 1e10 ($10,000); the derivation in cost.go quotes that figure and a reader checks the comment against it", MaxPlausibleRequestCostMicros)
	}

	// FAR below the representability bound, which is the point: MaxCostMicros lets one
	// request name $9 billion, and an endpoint nothing could parse has no business
	// naming a figure of that size. Asserted as an order of magnitude rather than an
	// exact ratio so a future retune of either bound does not fail on arithmetic it does
	// not change.
	if ratio := MaxCostMicros / MaxPlausibleRequestCostMicros; ratio < 1000 {
		t.Errorf("the plausibility cap is only %dx tighter than MaxCostMicros; a cap in the same order of magnitude as the representability bound does not bound a blast radius", ratio)
	}

	// The rate half, against the shipped table. Every tier of every bundled entry,
	// thresholds included — a long-context premium is a rate like any other and is
	// where the dearest number in the table actually lives for some models.
	dearest, dearestModel := 0.0, ""
	for _, e := range Bundled() {
		for i := range e.Rates.Base {
			if e.Rates.Set[i] && e.Rates.Base[i] > dearest {
				dearest, dearestModel = e.Rates.Base[i], e.Model
			}
		}
		for _, th := range e.Rates.Thresholds {
			for i := range th.Rate {
				if th.Set[i] && th.Rate[i] > dearest {
					dearest, dearestModel = th.Rate[i], e.Model
				}
			}
		}
	}
	if dearest == 0 {
		t.Fatal("no rates found in the bundled table; this test proves nothing about the derivation")
	}
	capRate := float64(maxPlausibleMicrosPerToken) / 1e6
	if dearest > capRate {
		t.Errorf("the dearest bundled rate is %v/token (%s), above the derivation's ceiling of %v/token — MaxPlausibleRequestCostMicros is no longer derived from anything true and both halves need revisiting",
			dearest, dearestModel, capRate)
	}

	// AND THE SAME RATE AFTER A MULTIPLIER, which is the assertion that makes this test
	// worth running. A resolved rate is a table rate times MultiplierRule.Factor, and
	// validate accepts a factor up to maxMultiplier — so the raw table is not the ceiling
	// the cap has to clear. The check above compares against 7.5e-05 and reads as 13x of
	// headroom; the effective figure is 7.5e-04 against 1e-03, which is 1.33x.
	//
	// A $100/Mtok model at a legitimate 10x markup therefore REACHES the cap while the raw
	// scan above stays green — a test asserting less than the comment it was pinning. This
	// is the one that fails when the real headroom is gone.
	dearestResolvable := dearest * maxMultiplier
	if dearestResolvable > capRate {
		t.Errorf("the dearest bundled rate (%v/token, %s) scaled by the largest accepted multiplier (%vx) is %v/token, above the derivation's ceiling of %v/token — the effective margin is exhausted, so a legitimate config can price a request past the plausibility cap and have it refused as a forgery",
			dearest, dearestModel, maxMultiplier, dearestResolvable, capRate)
	}

	// The whole derivation as one inequality: the dearest rate a config can RESOLVE TO,
	// applied to EVERY token the ceiling allows, still fits under the cap. If this fails
	// the cap is refusing bills a real call could produce, which is the same coverage gap
	// the refusal path exists to disclose.
	//
	// Uses the scaled rate rather than the raw one, deliberately: the request whose cost
	// this cap judges is priced at the resolved rate, so the raw figure is not the worst
	// case the cap has to admit.
	worstAllowed := float64(maxPlausibleTokens) * dearestResolvable
	if !PlausibleRequestCostUSD(worstAllowed) {
		t.Errorf("a call of %d tokens at the dearest RESOLVABLE rate (%v x %vx = %v) costs $%v and is refused as implausible; the cap is now tighter than its own derivation",
			maxPlausibleTokens, dearest, maxMultiplier, dearestResolvable, worstAllowed)
	}

	// And the headroom over a call that could actually happen — the largest context
	// window on any path we run, plus a full-length completion, at the dearest rate.
	// Documented as coarse on purpose: a cap set near real traffic starts refusing real
	// bills the first time a vendor reprices.
	//
	// AT LIST RATES, unlike the two checks above, and the asymmetry is the point of each.
	// Those ask what a config could legitimately produce, so they have to include the
	// multiplier. This one asks what real traffic costs, and a 10x markup over vendor list
	// is not real traffic — folding it in here would assert the cap has three orders of
	// magnitude over a bill nobody receives, which is a different claim from the one
	// MaxPlausibleRequestCostMicros makes. With the multiplier the same figure is 12.5x,
	// stated here so the number is on the record rather than implied by its absence.
	const largestContextWindow, longCompletion = 1_000_000, 64_000
	worstReal := float64(largestContextWindow+longCompletion) * dearest
	if headroom := float64(MaxPlausibleRequestCostMicros) / 1e6 / worstReal; headroom < 100 {
		t.Errorf("headroom over the worst real call ($%v) is only %vx; the derivation claims roughly three orders of magnitude and a reader will act on that", worstReal, headroom)
	}
}

// TestPlausibleRequestCostUSD covers the predicate's edges, including the one a
// hand-written comparison gets wrong.
//
// `micros <= MaxPlausibleRequestCostMicros` read off a FAILED conversion compares against
// a zero and calls 1e300 plausible, which is why the out-of-range rows are here: the
// predicate has to fold MicrosFromUSD's ok into its answer, not just its value.
func TestPlausibleRequestCostUSD(t *testing.T) {
	capUSD := float64(MaxPlausibleRequestCostMicros) / 1e6
	for _, tc := range []struct {
		name string
		usd  float64
		want bool
	}{
		{"a typical call", 0.0042, true},
		{"an expensive real call", 20, true},
		{"exactly at the cap", capUSD, true},
		{"a micro over the cap", capUSD + 1e-6, false},
		{"ten times the cap", capUSD * 10, false},
		{"the figure that exhausts a daily budget", 1e9, false},
		{"past the micros unit entirely", float64(MaxCostMicros), false},
		{"an infinity", math.Inf(1), false},
		{"not a number", math.NaN(), false},
		{"negative", -1, false},
		{"a free call", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PlausibleRequestCostUSD(tc.usd); got != tc.want {
				t.Errorf("PlausibleRequestCostUSD(%v) = %v, want %v", tc.usd, got, tc.want)
			}
		})
	}
}

// TestNoPerRequestBoundClosesTheAccumulationWrap is the DISCLOSURE, not a fix.
//
// A PER-REQUEST BOUND CANNOT CLOSE THE usage.Counts.Add WRAP, only move it. MaxCostMicros
// moves the threshold to 1,024 requests; the plausibility cap moves it again, by nearly six
// orders of magnitude and only on the path it applies to. Neither closes it. For any bound C > 0 the sum wraps after
// ceil(MaxInt64/C) requests and nothing here bounds the request count.
//
// This test exists so that arithmetic is checked rather than asserted in prose, and so
// anyone who later reads "the cap fixed the wrap" finds the counter-example next to the
// claim. The real fix is a checked accumulate where the sum is kept — usage.Counts.Add and
// the ledger's totals — which is another package's invariant.
func TestNoPerRequestBoundClosesTheAccumulationWrap(t *testing.T) {
	// THE FIGURES MaxCostMicros' DISCLOSURE QUOTES, read off the constants rather than
	// simulated with a loop: a loop that adds int64s until they wrap asserts that Go wraps
	// int64s, which is not a property of this package.
	//
	// If either number moves, the paragraph in cost.go quoting it is wrong and this fails.
	if got := math.MaxInt64 / int64(MaxCostMicros); got != 1023 {
		t.Errorf("math.MaxInt64/MaxCostMicros = %d, want 1023 — MaxCostMicros' disclosure says "+
			"1,024 at-bound requests wrap the aggregate", got)
	}
	// Under the plausibility cap the same wrap is eight orders of magnitude further out. That
	// is the blast-radius reduction the cap claims, and it is the reason the disclosure says
	// the cap moves the wrap without closing it.
	wrapAtCap := math.MaxInt64 / MaxPlausibleRequestCostMicros
	if wrapAtCap < 9e8 {
		t.Errorf("wrap at the cap after %d requests, want at least 9e8 — the cap is looser than "+
			"the disclosure claims", wrapAtCap)
	}
	// And still reachable, which is the honest half: a bound that made it unreachable would
	// mean the disclosure needs rewriting rather than this test deleting.
	if wrapAtCap <= 0 || wrapAtCap == math.MaxInt64 {
		t.Fatal("the wrap is unreachable by arithmetic; MaxCostMicros' disclosure is now wrong " +
			"in the other direction")
	}
}

// A MODELLED FIGURE IS HELD TO THE SAME PER-REQUEST CEILING AS A GATEWAY'S OWN.
//
// cost/settle applies PlausibleRequestCostUSD ($10,000) to a cost header and names the refusal;
// bounded only at MaxCostMicros ($9 billion), Cost's own arithmetic would admit what this file
// calls a garbage ledger figure. Two independent ways to reach the gap, both exercised here:
// the token check inside Cost is PER TIER, so one request can carry maxPlausibleTokens
// several times over, and a base RATE has no magnitude bound at all — config.Build validates
// only sign and finiteness, and the ProvDiscovered /model/info path is not config.
//
// The refusal is what makes the two paths agree. Priced, it would put an operator typo or a
// remote gateway's number into a thirty-day ledger row as fact.
func TestCost_RefusesAModelledFigurePastThePerRequestCeiling(t *testing.T) {
	perToken := func(usd float64) Rates {
		return Rates{
			Base: [numTiers]float64{TierInput: usd, TierOutput: usd},
			Set:  [numTiers]bool{TierInput: true, TierOutput: true},
		}
	}

	// An absurd rate on a perfectly ordinary request: 2M tokens at $0.01 each is $20,000.
	if micros, ok := Cost(perToken(0.01), Usage{Input: 2_000_000}); ok {
		t.Errorf("Cost reported priced at %d micros ($%.0f) for one request; anything past "+
			"MaxPlausibleRequestCostMicros ($%.0f) is refused on the header path and has to be "+
			"refused here too", micros, float64(micros)/1e6,
			float64(MaxPlausibleRequestCostMicros)/1e6)
	}

	// And the per-tier route to the same place: each tier is inside maxPlausibleTokens, the
	// request as a whole is not, at a rate that is plausible per token.
	huge := Usage{Input: maxPlausibleTokens, Output: maxPlausibleTokens}
	if micros, ok := Cost(perToken(float64(maxPlausibleMicrosPerToken)/1e6), huge); ok {
		t.Errorf("Cost reported priced at %d micros for %d tokens spread across two tiers; the "+
			"token bound is per tier, so it does not imply the cost bound", micros,
			huge.Input+huge.Output)
	}

	// The ceiling itself still prices, so the refusal is a bound and not an off-by-one that
	// unprices legitimate traffic. maxPlausibleTokens at the dearest plausible per-token rate
	// IS MaxPlausibleRequestCostMicros by construction.
	atTheBound := Usage{Input: maxPlausibleTokens}
	micros, ok := Cost(perToken(float64(maxPlausibleMicrosPerToken)/1e6), atTheBound)
	if !ok {
		t.Fatal("a request at exactly the plausibility ceiling reported unpriced; the bound is " +
			"inclusive by construction — see MaxPlausibleRequestCostMicros")
	}
	if micros != MaxPlausibleRequestCostMicros {
		t.Errorf("micros = %d, want exactly MaxPlausibleRequestCostMicros %d", micros,
			MaxPlausibleRequestCostMicros)
	}
}

// TestPlausibilityCeilingIsTenThousandDollars pins the MAGNITUDE, which every other test in this
// file takes as given.
//
// Enforcement is well covered — figures either side of the bound, the inclusive edge, the derivation
// from its two halves — but all of it is relative to the constant, so halving maxPlausibleTokens
// moves the ceiling to $1,000 and leaves the suite green. The number is a JUDGEMENT ("what could one
// inference call plausibly cost"), and the whole point of the cap is its distance from real traffic:
// the worst call anyone can construct is about $7, so a ceiling that quietly slid two orders of
// magnitude would start refusing real bills as forgeries and record them as coverage gaps.
//
// So this asserts the dollars, in the units an operator reads, exactly once.
func TestPlausibilityCeilingIsTenThousandDollars(t *testing.T) {
	const wantUSD = 10_000.0
	if got := float64(MaxPlausibleRequestCostMicros) / 1e6; got != wantUSD {
		t.Errorf("the plausibility ceiling is $%.2f, want $%.2f — moving it is a judgement about how far a bound should sit from real traffic (~$7 worst case), not a refactor",
			got, wantUSD)
	}
	// Either side of it, in dollars rather than in constants, so the predicate is pinned to the
	// same figure a reader would quote.
	if !PlausibleRequestCostUSD(wantUSD) {
		t.Errorf("$%.2f is refused: the derivation reads \"the most a request could plausibly cost\", so the ceiling itself is plausible", wantUSD)
	}
	if PlausibleRequestCostUSD(wantUSD + 0.01) {
		t.Errorf("$%.2f is accepted: the bound has stopped bounding", wantUSD+0.01)
	}
}

// TestIncompleteReasonWireStringsArePinned covers what travels, which is the string and not the
// identifier.
//
// These reach abctl as incomplete_reason JSON and cost/event switches on them to decide whether a
// figure is a floor or an approximation, so a rename is a wire break. ReasonOutputUncounted was
// pinned by a marshalling test; the other two were not — renaming either to "MUTANT" left
// ./core/... green.
func TestIncompleteReasonWireStringsArePinned(t *testing.T) {
	for want, got := range map[string]string{
		"output-uncounted":     ReasonOutputUncounted,
		"split-unreported":     ReasonSplitUnreported,
		"counters-below-total": ReasonCountersBelowTotal,
	} {
		if got != want {
			t.Errorf("reason string = %q, want %q: it travels as incomplete_reason JSON and a consumer switches on it", got, want)
		}
	}
}
