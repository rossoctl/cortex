// Package costing turns one response's facts into one settled cost.
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
package costing

import (
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
)

// Response cost headers emitted by LiteLLM.
//
// Exported because they are wire names, not internals: a test needs to synthesize them, and
// a future adapter for a gateway that reports cost differently needs to say which of these
// it is standing in for.
//
// MEASURED 2026-09-11 against ete-litellm (LiteLLM 1.85.5), because an earlier version of
// this comment had the semantics backwards and a reviewer reasonably concluded from it
// that drift detection would false-positive on every Anthropic-format request:
//
//	/v1/chat/completions  both headers present, IDENTICAL values
//	/v1/messages          only "-original", same value the other path reports
//
// For 16 input + 4 output tokens of claude-opus-5 both paths reported
// 0.00013680000000000002, which is 0.76 x vendor list (16x$5 + 4x$25 per Mtok = 0.00018).
// So "-original" is the cost the gateway ACTUALLY CHARGED, not a pre-discount list price,
// and falling back to it compares like with like.
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
	// negative, NaN or Inf. NOT a declaration of anything, so it must not suppress
	// the usage fallback and must never publish a settled zero.
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
		// Anthropic /v1/messages (and newer LiteLLM) omit the bare header.
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
	return c, headerPositive
}

// implausibleUnparsedCost is the CAP on what an endpoint nobody could parse is allowed to
// charge, and the one thing standing between a hostile host and a poisoned ledger.
//
// THE HOLE IT CLOSES. This header is an unauthenticated string on a response, validated
// here for nothing but its numeric shape, and nothing in the pipeline considers the HOST it
// came from — inference-parser dispatches on the path alone. Cost is now settled on every
// proxied response including the ones with no inference extension, so before this cap ANY
// path on ANY host an agent was proxied to could name its own figure and have it published,
// aggregated, written to the thirty-day ledger, and fed to litellm_budgettrack, which
// denies with HTTP 429 once the daily total passes MaxBudget. One response from one hostile
// site could therefore lock an agent out of all further inference and corrupt durable cost
// reporting. inference-parser is in the default local pipeline, so this is the shipped
// configuration.
//
// GATED ON A NIL EXTENSION, which is exactly the set of responses whose spend was newly
// admitted — an endpoint off the parser's dialect list, or a body it could not read. Where
// the extension is present the request WAS parsed: we sent an inference-shaped body to an
// inference-shaped path and read a model out of it, and today's behaviour is kept unchanged
// there. That narrower hole — a hostile host serving /v1/chat/completions — predates this
// and is a host-allowlist problem, which this is not pretending to be.
//
// WHAT IT IS NOT. It is a blast-radius cap, NOT AUTHENTICATION. A forged figure UNDER the
// cap still settles, because a plausible number from an unparsed endpoint is exactly what a
// gateway-priced /v1/embeddings response looks like and refusing it would break the feature
// that opened the hole. The stronger fix is an allowlist of hosts whose cost headers are
// believed at all; it was considered and deprioritised, and this cap was preferred because
// it also bounds a second disclosed gap (see pricing.MaxCostMicros on the aggregate wrap,
// which it moves and does not close).
//
// REFUSED, NOT CLAMPED. Clamping to the cap would invent a $10,000 charge nobody made and
// publish it wearing the same label a real figure wears. The refusal is published instead,
// as costevent.RejectedImplausible on an unpriced record, so the coverage gap stays
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
// path an attacker chooses and would be a log-flood amplifier. One warning names the
// mechanism and the first host; every occurrence is on the record as
// costevent.RejectedImplausible, and the Debug line below carries the full trail for
// whoever is already investigating.
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
	// Source is costevent.SourceGatewayHeader or costevent.SourceUsageFallback.
	Source string
	// Provenance is ProvAuthoritative for a gateway figure, else the rate table's level.
	Provenance pricing.Provenance
	// Priced reports that a figure exists — INCLUDING a settled zero, which is the
	// gateway saying the call was free. Not the same as CostUSD > 0.
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
	// It exists because a truncated stream was priced prompt-only and published as a
	// complete figure. Prompt counts land on Anthropic's message_start and the output
	// count only on message_delta, so a stream that dies in between yields real prompt
	// tokens with output at zero — and Settle priced exactly what it was given, set
	// Priced, and every consumer downstream read the result as an exact figure: the
	// usage aggregator counted it in PricedRequests and CostMicros, and a budget
	// enforced against it. A floor presented as a total understates spend by however
	// much the completion would have cost, which on a long generation is most of it.
	//
	// Honest by DISCLOSURE, not by adjustment. CostUSD keeps the figure and Priced stays
	// TRUE, deliberately and on both counts:
	//
	//   - The figure is the best available. Estimating the missing completion would be
	//     worse than reporting a known-low number and saying it is low.
	//   - The request IS priced, so removing it from a priced count would misuse a
	//     counter that answers a different question — coverage, "did anything price
	//     this" — and would disclose the same fact twice in two vocabularies. It would
	//     also send a consumer's own fallback down a rate table to recompute the
	//     identical prompt-only figure and label THAT one exact.
	//
	// Never set on a gateway figure, including a declared-free zero: a reported cost is
	// what the call actually charged whatever our counters saw, so completeness there is
	// the gateway's assertion rather than an inference from token tallies.
	Incomplete       bool
	IncompleteReason string

	// RejectedReason names a figure that WAS on the wire and was refused —
	// costevent.RejectedImplausible, set by implausibleUnparsedCost.
	//
	// The counterpart to Incomplete, one step further out: Incomplete qualifies a figure
	// that stands, this one records that there is no figure BECAUSE one was declined.
	// Priced is false and CostUSD is zero whenever it is set — a refused figure is not a
	// figure — and it travels onto the record so the gap is nameable instead of silent.
	// Never set alongside HasReported: refusing the figure and then keeping it for the
	// drift check would compare the rate table against a forgery.
	RejectedReason string

	// ReportedUSD is the gateway's own figure, when it gave one.
	ReportedUSD float64
	HasReported bool
	// ModelledUSD is what the rate table says the same usage costs, when it can say.
	// Computed even when a reported figure won, because the comparison is the only
	// signal that a rate table has gone stale.
	ModelledUSD float64
	HasModelled bool
	// ModelledProv is the provenance behind ModelledUSD.
	ModelledProv pricing.Provenance

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
	// PromptUSD + OutputUSD can differ from ModelledUSD IN THE LAST PLACE, and a caller
	// comparing them needs a tolerance rather than equality. Both halves resolve at the
	// same prompt size over a complementary tier partition, so they agree on the
	// arithmetic — but each is rounded to micros on its own and the whole is rounded
	// separately, so two rounded halves need not sum to the rounded whole. costing_test.go
	// uses a 1e-6 tolerance for exactly this reason.
	//
	// This paragraph used to open by claiming equality "to the micro, by construction" and
	// then deny it two lines later. The denial was the true half.
	// Neither sums to CostUSD, which may be the gateway's; comparing their sum against a
	// reported total is a drift measurement, and ModelledUSD is the figure kept for it.
	OutputUSD float64
	HasOutput bool
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
		out.RejectedReason = costevent.RejectedImplausible
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
	if micros, prov, ok := modelledCost(rates, pctx.Host, model, usage, promptTotal); ok {
		out.ModelledUSD, out.HasModelled, out.ModelledProv = float64(micros)/1e6, true, prov
	}

	// The prompt half, for a request row. Output zeroed rather than subtracted, because
	// Cost refuses to price a tier that carried tokens with no rate — and a partial total
	// presented as a whole one is worse than none.
	promptOnly := usage
	promptOnly.Output = 0
	if micros, _, ok := modelledCost(rates, pctx.Host, model, promptOnly, promptTotal); ok {
		out.PromptUSD, out.HasPrompt = float64(micros)/1e6, true
	}

	// The output half, for a response row, by the same construction: zero the prompt
	// tiers instead of subtracting the prompt figure from the total.
	outputOnly := usage
	outputOnly.Input, outputOnly.CacheWrite, outputOnly.CacheRead = 0, 0, 0
	if micros, _, ok := modelledCost(rates, pctx.Host, model, outputOnly, promptTotal); ok {
		out.OutputUSD, out.HasOutput = float64(micros)/1e6, true
	}

	streamedPlaceholder := state == headerZero && IsEventStream(pctx)
	out.DeclaredFree = state == headerZero && !streamedPlaceholder

	switch {
	case state == headerPositive:
		out.CostUSD, out.Source, out.Provenance, out.Priced =
			cost, costevent.SourceGatewayHeader, pricing.ProvAuthoritative, true
	case out.DeclaredFree:
		// A real answer of zero. Nothing is charged, but it must be published as
		// PRICED so no consumer re-prices it from the usage block.
		out.CostUSD, out.Source, out.Provenance, out.Priced =
			0, costevent.SourceGatewayHeader, pricing.ProvAuthoritative, true
	case out.HasModelled:
		out.CostUSD, out.Source, out.Provenance, out.Priced =
			out.ModelledUSD, costevent.SourceUsageFallback, out.ModelledProv, true
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
	if out.Priced && out.Source == costevent.SourceUsageFallback {
		if reason := pricing.IncompleteReason(pctx.Extensions.Inference); reason != "" {
			out.Incomplete, out.IncompleteReason = true, reason
		}
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
func modelledCost(rates pricing.Resolver, host, model string, u pricing.Usage, promptTotal int) (int64, pricing.Provenance, bool) {
	if rates == nil || u == (pricing.Usage{}) {
		return 0, pricing.ProvNone, false
	}
	r, prov := rates.Resolve(host, model, promptTotal)
	if prov == pricing.ProvNone {
		return 0, pricing.ProvNone, false
	}
	micros, ok := pricing.Cost(r, u)
	if !ok {
		return 0, pricing.ProvNone, false
	}
	return micros, prov, true
}

// StateKey is where the full Settled outcome is stashed for the rest of the request.
//
// In-process only — deliberately not on the wire. A consumer that needs the number reads
// the published costevent record; the drift check needs BOTH figures, and putting them in
// the session event would widen a public shape for one diagnostic's benefit. Keeping them
// here also means drift compares what was actually decided instead of recomputing a half
// and hoping the two agree.
const StateKey = "costing.settled"

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
// Under costevent.Key, and ALSO under the legacy plugin-name key: abctl is a separate
// binary that can lag the proxy, and an older one reads only the legacy key, so writing
// just the new one would blank the cost column for anyone who has not upgraded both. The
// legacy write comes out a release later.
//
// Ledger fields are left zero. They belong to whoever enforces a budget, which is not this
// package's business; litellm-budget-track amends them in place when it is present. That
// amendment is the one sanctioned second write to this key.
func Publish(pctx *pipeline.Context, ev costevent.Event) {
	if pctx.Extensions.Custom == nil {
		pctx.Extensions.Custom = map[string]any{}
	}
	pctx.Extensions.Custom[costevent.Key+pipeline.PluginEventSuffix] = ev
	pctx.Extensions.Custom[costevent.PluginName+pipeline.PluginEventSuffix] = ev
}

// Amend rewrites the published record, for a consumer adding fields it owns.
//
// Returns false when nothing has been published, so a caller cannot resurrect a record for
// a request that was never priced.
func Amend(pctx *pipeline.Context, f func(*costevent.Event)) bool {
	if pctx.Extensions.Custom == nil {
		return false
	}
	ev, ok := pctx.Extensions.Custom[costevent.Key+pipeline.PluginEventSuffix].(costevent.Event)
	if !ok {
		return false
	}
	f(&ev)
	Publish(pctx, ev)
	return true
}

// NewRecord builds the wire record from a settled outcome.
//
// Named NewRecord, not Record, because costevent.Record READS a record off a session event
// and these two packages are imported together — the cost owner imports costing, every
// consumer imports costevent. Two functions with one name pointing opposite directions is a
// coin flip at each call site, and the New prefix says which way this one goes.
func NewRecord(s Settled, avoided []costevent.Saving) costevent.Event {
	return costevent.Event{
		CostUSD:    s.CostUSD,
		Source:     s.Source,
		Provenance: s.Provenance.String(),
		Settled:    s.Priced,
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
		PromptUSD:      s.PromptUSD,
		OutputUSD:      s.OutputUSD,
		Avoided:        avoided,
	}
}
