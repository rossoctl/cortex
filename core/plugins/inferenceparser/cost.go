package inferenceparser

import (
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/cost/settle"
	"github.com/rossoctl/cortex/core/pipeline"
)

// This file makes the parser the one owner of what a request cost.
//
// It belongs here, and not in a plugin that acts on the money, for reasons that only
// become clear from the pipeline's shape:
//
//   - The parser is the ONLY component that knows when usage is final. It owns the
//     three finalization paths below, and it is where the assembled-usage fix lives
//     (Claude Code's `?beta=true` path reports cache counts on message_delta, not
//     message_start). Costing anywhere else duplicates that assembly or races it.
//   - Cost then cannot be missing while tokens are present. Previously the figure came
//     from litellm-budget-track, so a pipeline without it showed token counts and no
//     money — the same field silently meaning "modelled" instead of "authoritative"
//     depending on configuration. No parser means no tokens, so there is nothing to
//     price and nothing to explain.
//   - Consumers already declare the dependency needed to read it.
//     litellm-budget-track has `RequiresLater: inference-parser`, and plugins/registry.go
//     documents that as a hard AND with ordering: the pipeline refuses to build without
//     the parser. Response passes run in reverse, so the parser settles before any
//     consumer looks.
//
// The arithmetic and the gateway's header semantics live in core/cost/settle, not here. A
// provider-shaped body parser has no business knowing one gateway's header names.

// settleCost prices the finalized response and publishes the record, exactly once.
//
// Called from every finalization path. The idempotence guard is not belt-and-braces: a
// listener can dispatch a terminal frame more than once, and charging twice for one request
// is the failure that guard exists to prevent. It mirrors the one litellm-budget-track
// keeps for the same reason, down to WHERE the latch is set: only once there is a figure to
// publish, never on a pass that had nothing to say. See the two comments at the bottom of
// this function.
//
// WHICH LISTENER REPEATS IT: extproc does NOT repeat it once for headers and once for the body —
// the reading its message loop invites. Its header phase returns as soon as NeedsBody() is true,
// which this plugin's ReadsBody makes unconditional, so that dispatch is unreachable here. What it
// can repeat is the WHOLE buffered dispatch, once per ResponseBody message, whenever Envoy is not
// in buffered response mode.
//
// THAT REPETITION IS NOW GATED AT THE LISTENER, which changes what this guard is for rather than
// making it redundant — and the per-message version was worse than a double charge: the first
// message of an Anthropic stream holds message_start alone, so it finalized into a floor, THIS
// LATCH PINNED IT, and the pass carrying message_delta was short-circuited. A latch cannot tell a
// repeated dispatch from a continuing one, so the fix belonged where the repetition is decided.
// What remains this guard's job is a terminal frame repeated by ANY listener.
//
// A NIL Extensions.Inference IS A SUPPORTED INPUT, and that is why this guard reads as it does.
// Returning on nil makes "this parser understood the request" the precondition for charging
// anything, so /v1/embeddings, /v1/rerank and any body the parser could not read would be free
// however much the gateway charged — reaching no ledger, no /v1/usage total and no budget, since
// litellm-budget-track amends a settled record rather than settling its own.
//
// settle.Settle is safe with a nil extension: headerCost runs off the RESPONSE HEADERS alone and
// every read of the extension below it is nil-checked, with UsageFromInference(nil) the zero Usage
// that modelledCost refuses. So a header-only figure settles and a modelled one cannot be
// invented.
func (p *InferenceParser) settleCost(pctx *pipeline.Context) {
	if pctx == nil {
		return
	}
	if st := pipeline.GetState[settledOnce](pctx, costSettledKey); st != nil {
		return
	}

	settled := settle.Settle(pctx, p.rates)
	// Stored even when nothing priced, so a consumer can tell "no figure" from "this pass
	// never ran" — and so the drift check can see both figures. Unconditional and BEFORE the
	// publish gate below, which is what makes settle.Load's false mean "the cost owner's
	// response pass did not run" and nothing about the traffic's shape; see Load.
	settle.Store(pctx, settled)

	// Before the publish gate too, and for a related reason: a gateway whose header omits
	// cache cost is a fact about the endpoint, not about whether this particular response had
	// anything worth publishing. The reporter owns its own preconditions and dedups per
	// endpoint and model, so calling it unconditionally costs one branch per response.
	p.reportCacheBlind(pctx, settled)

	// Published when there is ANYTHING to say — a settled cost, or a saving another
	// component achieved. Gating on the cost alone would drop the saving for a model the
	// rate table cannot price, and the unpriced-model tally is precisely what tells an
	// operator which rate to add. event.Decode still reports such a record as
	// unpriced, so no consumer counts it as spend; that is what Record/Priced separate.
	avoided := settle.Avoided(pctx, p.rates)
	// HasPrompt is checked separately from Priced, because the two really can differ: a
	// table that prices every prompt tier but not a populated output tier yields a prompt
	// figure and no total. Dropping the record there would lose a figure a request row
	// can legitimately show, and the total stays absent rather than invented.
	// A REFUSED figure is also something to say, and the only case here that publishes a record
	// with no money in it: publishing nothing would make a declined header indistinguishable from
	// a response that reported no cost, hiding both the misconfiguration and the forgery. The
	// record is unpriced — event.Priced returns false on a set RejectedReason, which is what
	// the aggregator's Decode and the ledger's admission guard both ask.
	if !settled.Priced && !settled.HasPrompt && settled.RejectedReason == "" && len(avoided) == 0 {
		// NOT LATCHED, as litellm-budget-track's bill() is not when settle.Load finds nothing
		// priced. Nothing was published, so there is nothing to charge twice — and latching anyway
		// would lock this request out of a LATER pass that does have a figure. The latch protects
		// a PUBLISHED figure, not the attempt; a second pass costs a Settle and a Store, both pure
		// and idempotent.
		return
	}
	// Latched immediately before the publish it protects, and after the decision to
	// publish. Publish itself cannot fail — it assigns two map keys — so there is no
	// window here in which the latch is set and the figure is not out.
	pipeline.SetState(pctx, costSettledKey, &settledOnce{})
	settle.Publish(pctx, settle.NewRecord(settled, avoided))
}

// costSettledKey and settledOnce mark this request as already priced.
const costSettledKey = "inference-parser.cost-settled"

type settledOnce struct{}

// SetPricingResolver implements pricing.ResolverConsumer.
//
// Injected by plugins.BuildWithDeps when the process has a rate table. A nil resolver is a
// supported state, not an error: a binary built without pricing wiring reports traffic as
// unpriced rather than refusing to parse it. settle.Settle guards the interface — the trap
// being that a nil INTERFACE panics on call where a nil *pricing.Registry would not.
func (p *InferenceParser) SetPricingResolver(r pricing.Resolver) { p.rates = r }
