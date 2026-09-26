// Package settle turns one response's facts into one settled cost.
//
// It exists because that decision was made in two places with two shapes — inside
// litellm-budget-track and again inside the usage aggregator — and the two could disagree
// about the same request. Whichever component owns costing now calls Settle, and there is
// one precedence rule: an authoritative figure the gateway reported beats a figure modelled
// from token counts and a rate table.
//
// Separate from the parser that produces the token counts, and separate from the plugins
// that act on the money, because it is neither: a gateway's cost header is
// vendor-specific knowledge that has no place in a provider-shaped body parser, and a
// ledger has no business deciding what a request cost.
package settle

import (
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/pipeline"
)

// Response cost headers emitted by LiteLLM.
//
// Exported because they are wire names, not internals: a test needs to synthesize them, and
// a future adapter for a gateway that reports cost differently needs to say which of these
// it is standing in for.
//
// MEASURED 2026-09-11 against ete-litellm (LiteLLM 1.85.5), because the semantics are easy
// to state backwards — and stated backwards, they read as though drift detection would
// false-positive on every Anthropic-format request:
//
//	/v1/chat/completions  both headers present, IDENTICAL values
//	/v1/messages          only "-original", same value the other path reports
//
// For 16 input + 4 output tokens of claude-opus-5 both paths reported
// 0.00013680000000000002, which is 0.76 x vendor list (16x$5 + 4x$25 per Mtok = 0.00018).
// So "-original" is the cost the gateway ACTUALLY CHARGED, not a pre-discount list price,
// and falling back to it compares like with like.
//
// WHAT THAT MEASUREMENT DID NOT ESTABLISH: the probe carried 16 input and 4 output tokens and
// NO CACHE TOKENS, so it tested the two tiers an agent barely uses. On cache-bearing requests
// this gateway's header prices the uncached tiers and stops — 114 of 322 sampled responses,
// understating them 17.8x — which is what headerOmitsCache exists to catch. Any re-measurement
// here must include a cache-read-heavy pair.
//
// "Original" refers to LiteLLM's OWN discount/margin layer, reported alongside in
// X-Litellm-Response-Cost-{Discount,Margin}-Amount: original is the figure before that
// layer is applied. This gateway runs neither (both report 0.0), so the two agree. A
// gateway that DOES configure them would see them diverge, which is why checkDrift
// checks those headers before comparing — see driftComparable.
//
// The fallback itself is load-bearing: without it, budget tracking silently records $0
// for every Anthropic-format request, which is the shape Claude Code sends.
const (
	ResponseCostHeader         = "X-Litellm-Response-Cost"
	ResponseCostOriginalHeader = "X-Litellm-Response-Cost-Original"

	// LiteLLM's own adjustment layer, non-zero only where an operator configured it.
	DiscountAmountHeader = "X-Litellm-Response-Cost-Discount-Amount"
	MarginAmountHeader   = "X-Litellm-Response-Cost-Margin-Amount"
)

// headerCostState says what the gateway's cost header actually told us. A bool
// could not carry this, and collapsing these cases caused a real defect: every
// state below except headerPositive returned (0, true), so "the gateway declared
// this call free" was indistinguishable from "the header was garbage" and from
// "this is a stream, where LiteLLM always stamps 0 as a placeholder". Publishing a
// settled zero for all of them counted unpriced traffic as priced.
type headerCostState int

const (
	// headerAbsent: no cost header at all. Price from token usage.
	headerAbsent headerCostState = iota
	// headerUnusable: a header was present but could not be believed — unparseable,
	// negative, NaN, Inf, or past the micros unit every consumer accumulates in. NOT a
	// declaration of anything, so it must not suppress the usage fallback and must never
	// publish a settled zero.
	headerUnusable
	// headerZero: present, parsed, finite and exactly zero. On a NON-streamed
	// response this is the gateway saying the call was free — a cache hit, or an
	// error it declined to charge for. On a streamed response it means nothing:
	// LiteLLM stamps 0 there by design because the total is unknown when headers
	// are sent.
	headerZero
	// headerPositive: a usable figure.
	headerPositive
	// headerImplausible: present, parsed, finite, positive — and larger than one
	// inference call could plausibly cost, on an endpoint this pipeline could not
	// parse. Refused rather than believed, and refused rather than clamped: see
	// implausibleUnparsedCost. Distinct from headerUnusable because it is DISCLOSED —
	// a figure was on the wire and this process declined it, which is a fact an
	// operator needs and a garbage header is not.
	headerImplausible
)

// headerCost returns the cost the gateway reported and what kind of answer it was.
func headerCost(pctx *pipeline.Context) (cost float64, state headerCostState) {
	costStr := pctx.ResponseHeaders.Get(ResponseCostHeader)
	if costStr == "" {
		// Anthropic /v1/messages (and newer LiteLLM) omit the bare header, so this fallback is
		// what keeps Claude Code's shape from recording $0 for every request.
		//
		// PRE-DISCOUNT ON A GATEWAY THAT RUNS THAT LAYER. "Original" is the figure BEFORE
		// LiteLLM's discount/margin layer, so where an operator configures one, /v1/messages
		// bills pre-discount while /v1/chat/completions bills post-discount — and checkDrift
		// cannot see it, because it compares against the modelled figure rather than the
		// discount headers. Pre-existing; closing it means subtracting those headers when
		// non-zero, which needs a gateway configured that way to verify against.
		costStr = pctx.ResponseHeaders.Get(ResponseCostOriginalHeader)
	}
	if costStr == "" {
		return 0, headerAbsent
	}
	c, err := strconv.ParseFloat(costStr, 64)
	if err != nil || math.IsNaN(c) || math.IsInf(c, 0) || c < 0 {
		return 0, headerUnusable
	}
	if c == 0 {
		return 0, headerZero
	}
	if implausibleUnparsedCost(pctx, c) {
		return 0, headerImplausible
	}
	// UNREPRESENTABLE IS THE LAST QUESTION ASKED, and the order is the substance. MicrosFromUSD
	// is the predicate event.Event.Priced() applies downstream, so calling it here makes the
	// producer and the consumer agree BY CONSTRUCTION — written as two independent checks they
	// disagreed over ($9.007 billion, +Inf), this arm setting Priced while the record it produced
	// read as unpriced.
	//
	// AFTER THE PLAUSIBILITY BRANCH, because every figure past the micros unit is also past the
	// plausibility cap: asking first would swallow the hostile-host case as noise and make the
	// disclosure unreachable for the values most likely to be forged.
	//
	// AND UNUSABLE RATHER THAN IMPLAUSIBLE, which is forced. What reaches here is a PARSED
	// endpoint, which has usage to model — and RejectedReason reads as unpriced to every
	// consumer, so disclosing here would refuse the modelled figure along with the header.
	if _, ok := pricing.MicrosFromUSD(c); !ok {
		return 0, headerUnusable
	}
	return c, headerPositive
}

// implausibleUnparsedCost is the CAP on what an endpoint nobody could parse is allowed to
// charge, and the one thing standing between a hostile host and a poisoned ledger.
//
// THE HOLE IT CLOSES. This header is an unauthenticated string, validated for nothing but its
// numeric shape, and nothing in the pipeline considers the HOST it came from. Cost is settled on
// every proxied response now, including those with no inference extension, so without this cap
// any path on any host an agent is proxied to could name its own figure and have it published,
// aggregated, written to the thirty-day ledger and fed to litellm_budgettrack — which denies with
// 429 once the daily total passes MaxBudget. One hostile response could lock an agent out of all
// inference. inference-parser is in the default pipeline, so this is the shipped configuration.
//
// GATED ON A NIL EXTENSION, which is exactly the set of responses whose spend was newly
// admitted — an endpoint off the parser's dialect list, or a body it could not read.
//
// WHY THE PARSED PATH IS LEFT ALONE. Where the extension is present there is a MODELLED figure
// beside the header, so refusing the header would discard the one comparison that can show a rate
// table has gone stale against a gateway. TestSettle_ParsedEndpointIsUnaffectedByTheCap pins that.
//
// AND THE COMPARISON IS NOT WHAT THIS JUSTIFICATION RESTS ON, which is a correction: the drift
// check now returns early whenever the modelled figure is a floor, so on many of these responses it
// says nothing at all. What stays true is narrower — the pair is PUBLISHED, so a consumer holding
// the record can see them disagree — and the reason not to cap here is that capping is not the fix.
//
// TWO LIMITS, which narrow it rather than restate it. DETECTABLE IS NOT PREVENTED: nothing here
// withholds the figure, so it reaches Publish, the aggregate, the ledger and the 429 lockout
// regardless. And A NON-NIL EXTENSION IS NOT A CORROBORATING FIGURE: it is populated on the REQUEST
// pass carrying no counts, so HasModelled is false whenever the model has no rates or the usage
// block is empty, and those responses get neither the cap nor any comparison.
//
// THE RESIDUAL, reachable and not closed here: a hostile host on a path the parser DOES recognise
// can settle up to MaxCostMicros ($9.007 billion), published and aggregated while the drift check
// merely notes it. Bounding it is not the fix — not believing that host's header is, which is the
// allowlist in rossoctl/cortex#1027, and both limits above are why.
//
// WHAT IT IS NOT: a blast-radius cap, NOT AUTHENTICATION. A forged figure UNDER the cap still
// settles, because a plausible number from an unparsed endpoint is exactly what a gateway-priced
// /v1/embeddings response looks like.
//
// REFUSED, NOT CLAMPED. Clamping to the cap would invent a $10,000 charge nobody made and
// publish it wearing the same label a real figure wears. The refusal is published instead,
// as event.RejectedImplausible on an unpriced record, so the coverage gap stays
// nameable — that is the whole doctrine here: a wrong number wearing a right label is the
// worst available outcome.
func implausibleUnparsedCost(pctx *pipeline.Context, usd float64) bool {
	if pctx.Extensions.Inference != nil {
		return false
	}
	if pricing.PlausibleRequestCostUSD(usd) {
		return false
	}
	warnImplausibleCost(pctx, usd)
	return true
}

// implausibleWarnOnce keeps the operator-facing warning to ONE per process.
//
// Not once per host, which is the shape a reader will expect and which cannot be safely
// built here: pctx.Host is caller-controlled, so a map keyed on it is an unbounded
// allocation driven by hostile input. Not unconditional either — the warning fires on a
// path an attacker chooses and would be a log-flood amplifier.
//
// THE SPLIT IS DELIBERATE: Warn once per process, Debug per occurrence. Turning debug on IS
// asking for one line per event, and every occurrence has to be recoverable (pinned by
// TestWarnImplausibleCost_NamesTheHost), while a Warn per hostile request floods a log nobody
// opted into. Residual: with debug enabled this emits a line per attacker-chosen request. The
// bounded trail is the per-request record, event.RejectedImplausible.
// SWAPPED BY A TEST, WHICH MAKES IT A PACKAGE-WIDE CONSTRAINT. implausible_test.go resets this
// and replaces slog.Default() to observe the one warning, so no test in this package may run in
// parallel with another while that is true. TestNoTestInThisPackageRunsInParallel enforces it,
// because a comment would not survive the first t.Parallel someone adds.
var implausibleWarnOnce sync.Once

// warnImplausibleCost tells an operator enough to FIND THE HOST: host, path, the figure
// that was refused, and the bound it exceeded. A warning saying only "implausible cost
// rejected" would leave the one question that matters unanswerable.
func warnImplausibleCost(pctx *pipeline.Context, usd float64) {
	slog.Debug("costing: refused an implausible cost header",
		"host", pctx.Host, "path", pctx.Path, "reported_usd", usd,
		"max_plausible_usd", float64(pricing.MaxPlausibleRequestCostMicros)/1e6)
	implausibleWarnOnce.Do(func() {
		slog.Warn("costing: refused a cost header larger than any inference call could plausibly be; this endpoint was not parsed, so nothing corroborates the figure and it is recorded as an unpriced coverage gap rather than as spend. Further occurrences are logged at debug level only",
			"host", pctx.Host, "path", pctx.Path, "reported_usd", usd,
			"max_plausible_usd", float64(pricing.MaxPlausibleRequestCostMicros)/1e6)
	})
}

// IsEventStream reports whether the response is a text/event-stream (SSE) — the
// streamed shape where LiteLLM reports cost 0 in the header, so usage-based
// pricing is the intended fallback.
func IsEventStream(pctx *pipeline.Context) bool {
	ct := pctx.ResponseHeaders.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.EqualFold(strings.TrimSpace(ct), "text/event-stream")
}

// Comparable reports whether the gateway's figure can be compared against a modelled
// one at all.
//
// False when the bare header is absent AND LiteLLM's own discount/margin layer is active:
// the fallback figure is then pre-adjustment while the modelled figure is what the caller
// pays, so any comparison measures the gateway's adjustment rather than the rate table.
func Comparable(pctx *pipeline.Context) bool {
	if pctx.ResponseHeaders.Get(ResponseCostHeader) != "" {
		return true // the effective figure itself; nothing to reconcile
	}
	for _, h := range []string{DiscountAmountHeader, MarginAmountHeader} {
		v := pctx.ResponseHeaders.Get(h)
		if v == "" {
			continue
		}
		if f, err := strconv.ParseFloat(v, 64); err == nil && f != 0 {
			return false
		}
	}
	return true
}

// Settled is what one response cost, and how that was arrived at.
//
// Carries BOTH figures, not just the winner. A consumer that only needs the number reads
// CostUSD; the drift check needs the pair, and keeping them here means it compares what
// was actually decided rather than recomputing either half and hoping the two agree.
type Settled struct {
	// CostUSD is the figure to charge. Meaningful only when Priced.
	CostUSD float64
	// Source is event.SourceGatewayHeader or event.SourceUsageFallback.
	Source string
	// Provenance is ProvAuthoritative for a gateway figure, else the rate table's level.
	Provenance pricing.Provenance
	// Priced reports that a figure exists — INCLUDING a settled zero, which is the
	// gateway saying the call was free. Not the same as CostUSD > 0.
	//
	// The in-process twin of event.Event.Priced, which is now defined as
	// Trust().Spendable(); a consumer holding a published RECORD rather than this struct should
	// ask there, and read Trust() when it needs to know how approximate a spendable figure is.
	// NewRecord carries this field across as Settled, and a test asserts the two agree.
	Priced bool
	// DeclaredFree marks a gateway-reported, non-streamed, exactly-zero cost: a genuine
	// free call. Distinguished from an unusable header and from a stream's placeholder
	// zero, because publishing a settled zero for those counted unpriced traffic as
	// priced.
	DeclaredFree bool

	// Incomplete marks CostUSD as not an EXACT total, and IncompleteReason says why —
	// pricing.ReasonOutputUncounted for a figure that is known-low, or
	// pricing.ReasonSplitUnreported for one that is approximate in no known direction.
	//
	// It exists because a truncated stream was priced prompt-only and published as a complete
	// figure: prompt counts land on message_start and the output count only on message_delta, so
	// a stream dying in between yields real prompt tokens with output at zero. A floor presented
	// as a total understates spend by whatever the completion would have cost.
	//
	// Honest by DISCLOSURE, not adjustment: CostUSD keeps the figure and Priced stays TRUE. The
	// figure is the best available — estimating the missing completion is worse than a known-low
	// number that says so — and the request IS priced, so dropping it from a priced count would
	// misuse a counter that answers coverage instead, and send a consumer's own fallback off to
	// recompute the identical figure and label THAT one exact.
	//
	// Never set on a gateway figure, including a declared-free zero: a reported cost is what the
	// call charged whatever our counters saw.
	Incomplete       bool
	IncompleteReason string

	// RejectedReason names a figure that WAS on the wire and was refused —
	// event.RejectedImplausible, set by implausibleUnparsedCost.
	//
	// The counterpart to Incomplete, one step out: that qualifies a figure which stands, this
	// records that there is none BECAUSE one was declined. Priced is false and CostUSD zero
	// whenever it is set, and it travels onto the record so the gap is nameable. Never set
	// alongside HasReported — refusing a figure and then keeping it for the drift check would
	// compare the rate table against a forgery.
	RejectedReason string

	// ReportedUSD is the gateway's own figure, when it gave one.
	ReportedUSD float64
	HasReported bool
	// HeaderOmittedCache says the gateway's figure priced only the uncached tiers while the
	// cache tiers carried real cost, so the modelled figure was charged instead of it.
	//
	// NOT a disagreement this package is guessing at — it is a figure about a different
	// quantity. Measured against ete-litellm, 322 non-streamed responses split cleanly at a 5%
	// tolerance: 208 matched the rate table exactly, 114 matched the input+output cost alone
	// and understated their requests 17.8x, and none landed in between. The gateway's own spend
	// records bill the 114 in full, so the header disagrees with its own gateway rather than
	// reporting a discount. cacheblind_test.go carries the shape of those rows.
	//
	// ReportedUSD still holds the header figure when this is set: refusing it as the CHARGED
	// figure and discarding it are different acts, and the drift reporter needs the pair.
	//
	// Why this needs a named field rather than RejectedReason: that reads as unpriced to every
	// consumer, and this request IS priced — from the table, which on this gateway is the
	// figure that matches the bill.
	HeaderOmittedCache bool
	// ModelledUSD is what the rate table says the same usage costs, when it can say.
	// Computed even when a reported figure won, because the comparison is the only
	// signal that a rate table has gone stale.
	ModelledUSD float64
	HasModelled bool
	// ModelledProv is the provenance behind ModelledUSD.
	ModelledProv pricing.Provenance
	// UsageRefused says the token counters were impossible, whatever happened to the money. Set
	// alongside a REFUSED figure, and — the case it exists for — alongside a gateway figure that
	// was believed anyway: the header is what the call charged, so it stands, while the counts
	// beside it do not. See event.Event.UsageRefused.
	UsageRefused bool

	// ModelledIncomplete says the MODELLED figure is not an exact total, whichever figure
	// won, and ModelledIncompleteReason is pricing.IncompleteReason's answer for it.
	//
	// SEPARATE FROM Incomplete, which is about the figure that was CHARGED. They coincide on the
	// usage-fallback arm and diverge when a header wins: the header is exact by assertion, so
	// Incomplete is correctly false, and the knowledge that the modelled figure beside it is a
	// FLOOR was dropped — leaving the drift check to divide a known-low figure by an
	// authoritative one and report the RATE TABLE as stale for a response whose counters were
	// short.
	ModelledIncomplete       bool
	ModelledIncompleteReason string

	// PromptUSD is the PROMPT half of the modelled figure — output excluded, tiers
	// weighted. It exists because a request row can show what it cost while the
	// response row shows the total, and the two must not be a sum: a cache read bills
	// at ~0.1x the uncached input rate and a write at ~1.25x, so a flat prompt figure
	// overstates a cache-heavy turn by close to an order of magnitude.
	//
	// Modelled, never authoritative: a gateway reports one total for the call and does
	// not break it down, so this is the table's answer even when the total is the
	// gateway's.
	PromptUSD float64
	HasPrompt bool

	// OutputUSD is the OUTPUT half, the same way round: generated tokens at the output
	// rate, prompt tiers excluded, resolved at the REQUEST's prompt size so a
	// long-context premium reaches the completion too. It exists so a response row can
	// show what THAT row cost rather than what the exchange cost, which is the only
	// reading under which a per-row column is row-local.
	//
	// Specifically NOT CostUSD minus PromptUSD. Those two need not share a source —
	// CostUSD may be the gateway's post-discount figure while PromptUSD is always the
	// table's — so their difference is a gateway-vs-table delta wearing a completion's
	// name, and it can go negative: a gateway charging 3.0652 against a modelled 4.0063
	// prompt differences to -0.94.
	//
	// ModelledUSD minus PromptUSD would in fact be sound — same table, same prompt size,
	// complementary tiers — so the objection is to mixing sources, not to subtraction as
	// such. Pricing directly is still preferred: it keeps HasOutput independent of
	// HasModelled, so a response row can show its own cost on a call where the total came
	// from the gateway and the table has no opinion on the whole.
	//
	// PromptUSD + OutputUSD can differ from ModelledUSD IN THE LAST PLACE, so a caller comparing
	// them needs a tolerance, not equality: both halves resolve at the same prompt size over a
	// complementary partition, but each is rounded to micros on its own and the whole is rounded
	// separately. NOT equality "to the micro, by construction", which the rounding contradicts;
	// settle_test.go uses 1e-6.
	//
	// Neither sums to CostUSD, which may be the gateway's; comparing their sum against a
	// reported total is a drift measurement, and ModelledUSD is the figure kept for it.
	OutputUSD float64

	// TierUSD is the modelled cost of each rate tier, indexed by pricing.Tier.
	//
	// Modelled with exactly the standing PromptUSD above has, and for the reason given
	// there: a gateway reports one total and no breakdown. Taken from the WHOLE request's
	// pricing rather than from the two halves below, which re-price with the other tiers
	// zeroed and so resolve the table at a different prompt total.
	TierUSD [pricing.NumTiers]float64
	// HasTiers says TierUSD holds a split. False leaves every element zero, and zero must
	// not be read as a free tier — which is why the PUBLISHED field is a pointer.
	HasTiers  bool
	HasOutput bool
}

// cacheBlindTolerance is how close a header figure must sit to the uncached tiers before it is
// read as having priced only those. It also sets the cache share below which the question cannot
// be asked at all, because headerOmitsCache derives that floor from it rather than from a second
// constant — see the disjointness condition there.
//
// The same 5% the drift reporter uses, for the same reason: it absorbs rounding and the micro
// quantization while still separating the two populations cleanly. Measured on 322 real
// responses, every one landed in "matches the whole" or "matches the uncached half" at this
// tolerance and none in between, so the figure is not a knife edge that a slightly different
// constant would move.
const cacheBlindTolerance = 0.05

// headerOmitsCache reports whether the gateway's figure priced the uncached tiers and nothing
// else, on a request whose cache tiers carried real cost. See Settled.HeaderOmittedCache.
//
// THREE CONDITIONS, and each one rules out a case that would otherwise be misread:
//
//   - A modelled figure with a tier split must exist. Without it there is nothing to charge
//     instead, and firing would trade a low figure for no figure — which is worse, and is what
//     a guard keyed on the header alone would do for a model that has no rates.
//   - The two candidate readings must be TELLABLE APART at this tolerance: the match window
//     around the uncached figure must not reach the discounted whole. On a cache-free request
//     the uncached figure IS the whole figure, so "the header equals the uncached tiers" is true
//     of every perfectly correct header ever sent — and a materiality floor alone does not fix
//     that, it only moves it. At a 6% cache share the window around $0.0185 uncached is
//     [0.017575, 0.019425], and 4% off the $0.019682 whole lands at $0.0188947, inside it: one
//     interval meaning two things, which drift.go would call agreement at the same 5%. Refusing
//     it is this bug inverted — charging a figure the gateway never said. The windows are
//     disjoint when uncached*(1+t) < total*(1-t), i.e. above a cache share of 2t/(1+t) ~= 9.5%,
//     so the tolerance sets its own floor instead of a second constant doing it.
//   - The header must MATCH the uncached figure, not merely fall below it. A header that is low
//     for some other reason is a disagreement this package cannot adjudicate — it may be a
//     deeper discount than the table models — so it stands, and the drift reporter says so.
//     Substituting there would be the "your table must be wrong" mistake written into code.
//
// The 9.5% floor costs no coverage that matters: the observed rows sit near a 99% cache share,
// 90 input tokens against 55k-416k cached ones. What it gives up is the low-cache band, where an
// omission is worth cents and is genuinely indistinguishable from a discount.
func headerOmitsCache(out Settled, header float64) bool {
	if !out.HasModelled || !out.HasTiers {
		return false
	}
	uncached := out.TierUSD[pricing.TierInput] + out.TierUSD[pricing.TierOutput]
	cached := out.TierUSD[pricing.TierCacheWrite] + out.TierUSD[pricing.TierCacheRead]
	total := uncached + cached
	// A zero uncached figure cannot be recognised either — every positive header sits outside a
	// window of zero width — so this only spares the arithmetic a meaningless comparison.
	if total <= 0 || uncached <= 0 {
		return false
	}
	if uncached*(1+cacheBlindTolerance) >= total*(1-cacheBlindTolerance) {
		return false
	}
	// Relative to the uncached figure, which is the quantity being recognised.
	d := header - uncached
	return d <= uncached*cacheBlindTolerance && d >= -uncached*cacheBlindTolerance
}

// Settle prices one response.
//
// The precedence, in one place: a positive header figure wins outright. Otherwise the
// token counts are priced through the rate table — but NOT when the gateway declared the
// call free, because inventing a cost for a call it explicitly charged nothing for is how
// a cache hit came to be billed. A stream's zero is a placeholder, not an answer, so it
// falls back like an absent header.
//
// rates may be nil: a binary with no pricing wired reports unpriced rather than crashing.
func Settle(pctx *pipeline.Context, rates pricing.Resolver) Settled {
	var out Settled
	if pctx == nil {
		return out
	}

	cost, state := headerCost(pctx)
	if state == headerPositive {
		out.ReportedUSD, out.HasReported = cost, true
	} else if state == headerZero && !IsEventStream(pctx) {
		out.ReportedUSD, out.HasReported = 0, true
	} else if state == headerImplausible {
		// DISCLOSED and not charged. Deliberately not fed to ReportedUSD: the figure was
		// refused, and keeping it as "the gateway's own figure" would let the drift check
		// measure the rate table against a number nothing corroborates and report the
		// table as stale. Nothing else about this response changes — a modelled figure
		// would still win below if one existed, which on this path it never can, since
		// the cap only applies where the extension is nil and there is therefore no usage
		// to model.
		out.RejectedReason = event.RejectedImplausible
	}

	// Modelled alongside, always, so the pair is available for drift even when the
	// gateway's figure wins.
	usage := pricing.UsageFromInference(pctx.Extensions.Inference)
	model := ""
	if pctx.Extensions.Inference != nil {
		model = pctx.Extensions.Inference.Model
	}
	// promptTotal is the request's own prompt size, and every figure below resolves its
	// rates at it — the whole request and both halves alike. That is what makes the two
	// halves a partition of the total instead of three unrelated lookups: a long-context
	// threshold flattens the same way for all three, so no premium can land on one and
	// miss another.
	promptTotal := usage.PromptTotal()
	micros, tiers, prov, ok, refusal := modelledCost(rates, pctx.Host, model, usage, promptTotal)
	if ok {
		out.ModelledUSD, out.HasModelled, out.ModelledProv = float64(micros)/1e6, true, prov
		// From the WHOLE request's pricing, under one resolved rate set, so the four
		// figures are mutually consistent. The halves below cannot serve: each zeroes the
		// other's tiers, which changes Usage.PromptTotal and so the rates that apply.
		for i, m := range tiers {
			out.TierUSD[i] = float64(m) / 1e6
		}
		out.HasTiers = true
	}

	// AN IMPOSSIBLE COUNT REFUSES ALL THREE FIGURES, HERE, BEFORE THE HALVES ARE COMPUTED.
	//
	// The halves are not a partition of the counts — each is another pass over the same usage
	// with some tiers ZEROED — so zeroing is exactly what erases the counter that caused the
	// refusal. A negative Output, or one past what any request could report, makes Cost refuse
	// the whole request and the output half, while promptOnly drops Output to zero and comes
	// back PRICED. Measured at 3.8 micros/token: whole refused, promptOnly $0.0038.
	//
	// And a lone prompt half is enough to publish: settleCost skips only when
	// `!Priced && !HasPrompt && RejectedReason == ""`, and abctl's renderer tests PromptUSD > 0
	// rather than Priced(). So this shipped a figure derived from a count this package had just
	// declared impossible, on a record that said unpriced. Reachable from the wire, where these
	// counters are provider-controlled ints with no floor.
	//
	// SEPARATE FROM THE SUM GUARD BELOW, which keys on both halves existing because a lone
	// survivor is legitimate when a tier has no RATE (see TestSettle_OnePricedHalfSurvivesAlone).
	// An impossible count is the other case: the contract is that such a report is refused
	// whole, because a believed figure beside a refused one in the same row is a breakdown
	// nobody can reconcile.
	//
	// ASKED OF THE COUNTERS, NOT OF Cost's REFUSAL REASON, which is the same question one level too
	// far away: Cost returns the FIRST problem it meets, so with a table missing an earlier tier's
	// rate the report refused as "no-rate" and this branch never ran — leaving the output half,
	// which zeroes the offending tier, priced at $0.0005 on a record disclosing nothing.
	if !pricing.PlausibleUsage(usage) {
		out.UsageRefused = true
		return withRefusal(out, cost, state, pctx, event.RejectedImplausibleUsage)
	}

	// The prompt half, for a request row. Output zeroed rather than subtracted, because
	// Cost refuses to price a tier that carried tokens with no rate — and a partial total
	// presented as a whole one is worse than none.
	promptOnly := usage
	promptOnly.Output = 0
	if micros, _, _, ok, _ := modelledCost(rates, pctx.Host, model, promptOnly, promptTotal); ok {
		out.PromptUSD, out.HasPrompt = float64(micros)/1e6, true
	}

	// The output half, for a response row, by the same construction: zero the prompt
	// tiers instead of subtracting the prompt figure from the total.
	outputOnly := usage
	outputOnly.Input, outputOnly.CacheWrite, outputOnly.CacheRead = 0, 0, 0
	if micros, _, _, ok, _ := modelledCost(rates, pctx.Host, model, outputOnly, promptTotal); ok {
		out.OutputUSD, out.HasOutput = float64(micros)/1e6, true
	}

	// THE PAIR IS HELD TO THE PLAUSIBILITY CEILING ITS OWN HALVES ESCAPE. pricing.Cost applies
	// that ceiling per call and this function makes three of them, so a request whose whole is
	// refused as impossible can have both halves come back priced just under it: the record then
	// reads unpriced, cost $0, prompt $10,000, output $10,000, and anything adding the request
	// row to the response row — which is what these two fields are for — charges the total that
	// was refused. Halving a forged figure does not produce two real ones.
	//
	// KEYED ON THE SUM, NOT ON "WAS THE WHOLE PRICED", because a single surviving half is
	// deliberate: where the output tier carried tokens with no rate, Cost refuses the whole and
	// the prompt half is the only figure anyone can attribute. Only the pair can smuggle a figure
	// past a bound neither half broke.
	//
	// AND THE SUM TEST ALONE LEFT THE LONE SURVIVOR OF A REFUSED WHOLE STANDING. It requires BOTH
	// halves, so it closed only the case where each is individually under the ceiling. When one half
	// itself breaks the ceiling, Cost refuses that half, this test cannot fire, and the SIBLING is
	// published on a record that reads unpriced. Measured at $1/token: input 15,000 / output 5,000
	// refuses the whole ($20,000) and the prompt half ($15,000), while the output half comes back
	// priced at $5,000 — so the record said cost $0, RejectedReason cost-implausible, output
	// $5,000, and abctl's renderer, which tests OutputUSD > 0 rather than Priced(), displayed
	// $5,000.00 for a request this package had refused. Symmetric with the tiers swapped.
	//
	// So a MAGNITUDE refusal of the whole refuses both halves. What the ceiling condemns is the RATE
	// TABLE — no request costs $20,000, so the rate that says it did is wrong — and both halves are
	// drawn from that same table at the same prompt total, which is exactly the reasoning already
	// applied to an impossible COUNT above. It stays distinct from RefusalNoRate, where a lone
	// survivor is the legitimate case (TestSettle_OnePricedHalfSurvivesAlone).
	//
	// ASKED OF ImpossibleFigure RATHER THAN OF ONE LABEL, because the two magnitude refusals are
	// ordered by CHECK ORDER and not by size: Cost tests representability first, so anything past
	// ~$9.007e9 is RefusalUnrepresentable and never carries the plausibility label at all. Keyed on
	// RefusalImplausibleTotal alone this clause covered $10,000 to $9 billion and let everything
	// above it through — measured at $1e6 per input token, the output half came back priced at
	// $5,000 with the record carrying NO RejectedReason, because the disclosure below keyed on the
	// same single label. Worse than the leak it was written to close, and unnameable.
	if refusal.ImpossibleFigure() ||
		out.HasPrompt && out.HasOutput && !pricing.PlausibleRequestCostUSD(out.PromptUSD+out.OutputUSD) {
		out.PromptUSD, out.HasPrompt = 0, false
		out.OutputUSD, out.HasOutput = 0, false
		// SAME REASONING, SAME TABLE. What the ceiling condemns is the RATE TABLE, and the
		// split is drawn from it at the same prompt total. Unlike the halves it would be
		// condemned SILENTLY: a ratio carries no magnitude, so a discredited one looks
		// exactly like a sound one on screen.
		out.TierUSD, out.HasTiers = [pricing.NumTiers]float64{}, false
	}

	streamedPlaceholder := state == headerZero && IsEventStream(pctx)
	out.DeclaredFree = state == headerZero && !streamedPlaceholder

	// Asked here, where both figures and the tier split exist, and BEFORE the precedence
	// below — it is the one exception to "a positive header figure wins outright".
	out.HeaderOmittedCache = state == headerPositive && headerOmitsCache(out, cost)

	switch {
	case state == headerPositive && !out.HeaderOmittedCache:
		out.CostUSD, out.Source, out.Provenance, out.Priced =
			cost, event.SourceGatewayHeader, pricing.ProvAuthoritative, true
	case out.DeclaredFree:
		// A real answer of zero. Nothing is charged, but it must be published as
		// PRICED so no consumer re-prices it from the usage block.
		out.CostUSD, out.Source, out.Provenance, out.Priced =
			0, event.SourceGatewayHeader, pricing.ProvAuthoritative, true
	case out.HasModelled:
		out.CostUSD, out.Source, out.Provenance, out.Priced =
			out.ModelledUSD, event.SourceUsageFallback, out.ModelledProv, true
	}

	// Qualify a MODELLED figure whose counters cannot support an exact total. Nothing is
	// adjusted — see Settled.Incomplete; the figure stands and the claim about it does
	// not.
	//
	// This is the only place in the system that holds both the header state and the
	// usage, so it is the only place that knows WHICH figure won — and therefore the
	// only place that can gate the disclosure on the answer being modelled. Gated on the
	// source rather than on "the header was not positive" because DeclaredFree reaches
	// here as SourceGatewayHeader too: the gateway stating it charged nothing is an
	// exact total, and publishing a floor of zero would be a lower bound on nothing.
	if out.HasModelled {
		if reason := pricing.IncompleteReason(pctx.Extensions.Inference); reason != "" {
			out.ModelledIncomplete, out.ModelledIncompleteReason = true, reason
			// The charged figure is qualified only when the modelled one IS the charged one.
			// Gated on the source rather than on "the header was not positive" because
			// DeclaredFree reaches here as SourceGatewayHeader too: a gateway stating it
			// charged nothing is an exact total, and publishing a floor of zero would be a
			// lower bound on nothing.
			if out.Priced && out.Source == event.SourceUsageFallback {
				out.Incomplete, out.IncompleteReason = true, reason
			}
		}
	}

	// A MODELLED FIGURE PAST THE CEILING IS DISCLOSED TOO, on the same rule as a header's.
	// Both say no one inference call could have cost this; the difference is only which
	// number was wrong, and this one's cause is ours — an operator's typo in a rate, or a
	// rate discovered from a gateway — so it unprices every request that rate touches while
	// looking exactly like traffic nobody had rates for.
	//
	// GATED ON NOTHING ELSE HAVING SETTLED, which is not hygiene. RejectedReason reads as
	// unpriced to every consumer, so disclosing a derived figure's refusal on a response the
	// GATEWAY priced would discard an authoritative charge in order to complain about a
	// number that lost anyway. See TestSettle_AHeaderFigureSurvivesAnImpossibleTokenReport.
	//
	// BOTH MAGNITUDE REFUSALS ARE DISCLOSED, under the one reason. Keyed on
	// RefusalImplausibleTotal alone, a figure past representability (~$9.007e9) came back with
	// RejectedReason EMPTY — refused, unpriced, and silent about why, which is the one outcome
	// this field exists to rule out. Both are published as RejectedImplausible rather than
	// splitting the wire value: an operator needs to know the figure was impossible, and WHICH
	// bound tripped is a pricing-layer detail that a second reason would make every consumer
	// learn. See pricing.Refusal.ImpossibleFigure for why the labels split where they do.
	if !out.Priced && out.RejectedReason == "" && refusal.ImpossibleFigure() {
		out.RejectedReason = event.RejectedImplausible
	}
	return out
}

// withRefusal finishes a Settled whose MODELLED figures were all refused, keeping whatever the
// gateway's header already established.
//
// The header arm still runs, and that ordering is the point: a bad token report says nothing
// about what the call actually charged, so a gateway figure — or a declared-free zero — must
// survive it. The disclosure is attached only when nothing else settled, because a set
// RejectedReason reads as unpriced everywhere.
func withRefusal(out Settled, cost float64, state headerCostState, pctx *pipeline.Context, reason string) Settled {
	switch {
	case state == headerPositive:
		out.CostUSD, out.Source, out.Provenance, out.Priced =
			cost, event.SourceGatewayHeader, pricing.ProvAuthoritative, true
	case state == headerZero && !IsEventStream(pctx):
		out.DeclaredFree = true
		out.CostUSD, out.Source, out.Provenance, out.Priced =
			0, event.SourceGatewayHeader, pricing.ProvAuthoritative, true
	}
	if !out.Priced && out.RejectedReason == "" {
		out.RejectedReason = reason
	}
	return out
}

// modelledCost prices usage through the rate table.
//
// promptTotal is THE REQUEST'S prompt size, passed separately from u rather than derived
// from it. A context threshold is a property of the request — Rates.At puts it that way:
// the premium is priced on how much prompt was sent, not on what came back — and u is
// often not the whole request. It may be one half of it (prompt or output), or a
// counterfactual (the tokens a plugin avoided). Deriving the threshold from u.PromptTotal()
// looked right and silently dropped the premium for every such slice: an output half has no
// prompt tokens at all, so it resolved at At(0) and priced a 641k-token turn's completion at
// the base output rate while the whole-request figure used the above-200k one.
//
// The nil guard is on the INTERFACE, which is the trap: an un-injected consumer holds a
// nil interface and calling a method on it panics, where a nil *pricing.Registry would
// have been safe. tool-prune hit exactly this and its fail-open masked the panic, so
// pruning silently stopped.
func modelledCost(rates pricing.Resolver, host, model string, u pricing.Usage, promptTotal int) (
	int64, [pricing.NumTiers]int64, pricing.Provenance, bool, pricing.Refusal) {
	var none [pricing.NumTiers]int64
	if rates == nil || u == (pricing.Usage{}) {
		// No resolver is a deployment with no rate table at all, and no counters is unknown
		// usage. Neither is a refusal of anything: nothing was on the wire to refuse.
		return 0, none, pricing.ProvNone, false, pricing.RefusalNoTokens
	}
	r, prov := rates.Resolve(host, model, promptTotal)
	if prov == pricing.ProvNone {
		return 0, none, pricing.ProvNone, false, pricing.RefusalNoRate
	}
	tiers, micros, ok, refusal := pricing.CostByTier(r, u)
	if !ok {
		return 0, none, pricing.ProvNone, false, refusal
	}
	return micros, tiers, prov, true, pricing.RefusalNone
}

// StateKey is where the full Settled outcome is stashed for the rest of the request.
//
// In-process only — deliberately not on the wire. A consumer that needs the number reads
// the published costevent record; the drift check needs BOTH figures, and putting them in
// the session event would widen a public shape for one diagnostic's benefit. Keeping them
// here also means drift compares what was actually decided instead of recomputing a half
// and hoping the two agree.
const StateKey = "settle.settled"

// Store stashes the outcome for later plugins in the same request.
func Store(pctx *pipeline.Context, s Settled) {
	pipeline.SetState(pctx, StateKey, &s)
}

// Load retrieves the outcome. False means the cost owner's RESPONSE PASS never ran for this
// request — the parser is absent from the pipeline, the request was rejected before the
// response phase, or no listener delivered a terminal frame.
//
// It does NOT mean the request carried no inference, and no longer says anything about the
// traffic's shape. Store now runs on EVERY proxied response, including the ones this parser
// has no dialect for, because a gateway reports its own cost in a response header that needs
// neither a model nor a body — so /v1/embeddings, /mcp, a health check and a CONNECT tunnel
// all Load TRUE. True therefore says only that a decision was reached; whether the decision
// was a figure is Settled.Priced, which is false for most of that traffic.
//
// The old reading — false means no inference at all — held only while Store was reached from
// the parsed paths alone, and it is the reading under which a gateway-priced response the
// parser could not read escaped the ledger entirely.
func Load(pctx *pipeline.Context) (Settled, bool) {
	if s := pipeline.GetState[Settled](pctx, StateKey); s != nil {
		return *s, true
	}
	return Settled{}, false
}

// Publish writes the wire record onto the session event.
//
// Under event.Key, and ALSO under the legacy plugin-name key: abctl is a separate
// binary that can lag the proxy, and an older one reads only the legacy key, so writing
// just the new one would blank the cost column for anyone who has not upgraded both. The
// legacy write comes out a release later.
//
// Ledger fields are left zero. They belong to whoever enforces a budget, which is not this
// package's business; litellm-budget-track amends them in place when it is present. That
// amendment is the one sanctioned second write to this key.
func Publish(pctx *pipeline.Context, ev event.Event) {
	if pctx.Extensions.Custom == nil {
		pctx.Extensions.Custom = map[string]any{}
	}
	pctx.Extensions.Custom[event.Key+pipeline.PluginEventSuffix] = ev
	pctx.Extensions.Custom[event.PluginName+pipeline.PluginEventSuffix] = ev
}

// Amend rewrites the published record, for a consumer adding fields it owns.
//
// Returns false when nothing has been published, so a caller cannot resurrect a record for
// a request that was never priced.
func Amend(pctx *pipeline.Context, f func(*event.Event)) bool {
	if pctx.Extensions.Custom == nil {
		return false
	}
	ev, ok := pctx.Extensions.Custom[event.Key+pipeline.PluginEventSuffix].(event.Event)
	if !ok {
		return false
	}
	f(&ev)
	Publish(pctx, ev)
	return true
}

// tierCostOf publishes the modelled split, or nil when there is none.
//
// NIL RATHER THAN AN EMPTY STRUCT: the consumer apportions a real total by these numbers,
// so "no split" read as four zeros divides by zero and read as a real split reports every
// request free. See event.Event.Tiers.
func tierCostOf(s Settled) *event.TierCost {
	if !s.HasTiers {
		return nil
	}
	return &event.TierCost{
		Input:      s.TierUSD[pricing.TierInput],
		CacheWrite: s.TierUSD[pricing.TierCacheWrite],
		CacheRead:  s.TierUSD[pricing.TierCacheRead],
		Output:     s.TierUSD[pricing.TierOutput],
	}
}

// gatewayFigureOf is the gateway's own figure, and only where that figure LOST.
//
// Zero elsewhere so omitempty drops it: on the ordinary path the gateway's figure is CostUSD
// and Source says as much, and publishing it twice would cost every event bytes to say nothing.
// See event.Event.GatewayUSD for why it is never summed.
func gatewayFigureOf(s Settled) float64 {
	if !s.HeaderOmittedCache || !s.HasReported {
		return 0
	}
	return s.ReportedUSD
}

// NewRecord builds the wire record from a settled outcome.
//
// Named NewRecord, not Record, because event.Record READS a record off a session event
// and these two packages are imported together — the cost owner imports costing, every
// consumer imports event. Two functions with one name pointing opposite directions is a
// coin flip at each call site, and the New prefix says which way this one goes.
func NewRecord(s Settled, avoided []event.Saving) event.Event {
	return event.Event{
		CostUSD:    s.CostUSD,
		Source:     s.Source,
		Provenance: s.Provenance.String(),
		Settled:    s.Priced,
		// Carried for the reason the whole list below is: a refusal recorded nowhere is
		// indistinguishable from a response that never had a figure. The gateway's number rides
		// along ONLY here, where it is not already CostUSD — see event.Event.GatewayUSD.
		HeaderOmittedCache: s.HeaderOmittedCache,
		GatewayUSD:         gatewayFigureOf(s),
		// Carried, not derived. A Settled.Incomplete that NewRecord dropped would be
		// knowledge that reaches nothing — which is exactly the state this fix found the
		// parser's "token counts will be incomplete" log line in.
		Incomplete:       s.Incomplete,
		IncompleteReason: s.IncompleteReason,
		// Carried for the same reason: a refusal that reached no record is a coverage gap
		// nobody can see, which is the state this fix found the implausible-header path
		// in — it published nothing at all and looked identical to a response that
		// reported no cost.
		RejectedReason: s.RejectedReason,
		// Carried on its own, because it survives a believed header: see Settled.UsageRefused.
		UsageRefused: s.UsageRefused,
		PromptUSD:    s.PromptUSD,
		OutputUSD:    s.OutputUSD,
		Tiers:        tierCostOf(s),
		Avoided:      avoided,
	}
}
