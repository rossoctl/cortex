package inferenceparser

import (
	"github.com/rossoctl/cortex/authbridge/authlib/costing"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
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
// The arithmetic and the gateway's header semantics live in authlib/costing, not here. A
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
// WHICH LISTENER REPEATS IT — corrected, because this comment used to say extproc dispatches
// once for headers and once for the buffered body, and it does not. Its response-header
// phase returns as soon as Pipeline.NeedsBody() is true (extproc/server.go:616), which this
// plugin's ReadsBody makes unconditionally true, so the header-only dispatch below it is
// unreachable for any pipeline containing this parser. What extproc really does is run the
// WHOLE buffered dispatch — terminal frame included — once per ResponseBody message it
// receives (handleResponseBody -> dispatchBufferedFrames), so a body delivered in more than
// one message settles once per chunk. That is reachable whenever Envoy is not in buffered
// mode for the response: a statically configured STREAMED body mode, or a filter with
// allow_mode_override off, which makes the ModeOverride this listener asks for a no-op.
// The guard is therefore load-bearing on the shipped configuration; only its old
// explanation was wrong.
//
// AND THAT REPETITION IS NOW GATED AT THE LISTENER, which changes what this guard is for
// rather than making it redundant. extproc dispatches the terminal frame only on the body
// message carrying end_of_stream, because the per-message version was worse than a double
// charge: the FIRST message of an Anthropic stream holds message_start alone, so it
// finalized into a floor, THIS LATCH PINNED IT, and the pass carrying message_delta was
// short-circuited — the output tokens went unbilled by the very guard added to stop a
// double charge. A latch cannot distinguish a repeated dispatch from a continuing one, so
// the fix belonged where the repetition is decided. What remains this guard's job is a
// terminal frame repeated by ANY listener, which is a property no plugin can verify from
// the inside.
//
// A NIL Extensions.Inference IS A SUPPORTED INPUT, and that is the whole reason this
// guard reads the way it does. It used to return on nil, which silently made "this
// parser understood the request" the precondition for charging anything — so
// /v1/embeddings, /v1/rerank, /v1/moderations, any endpoint not on the request-side
// allowlist, and any request whose body was empty or unparseable were free of charge
// however much the gateway said they cost. The spend reached no ledger, no /v1/usage
// total and no budget, because litellm-budget-track amends a settled record rather than
// settling its own: no record meant no enforcement.
//
// costing.Settle is safe with a nil extension and needs no model to answer. headerCost
// runs first off the RESPONSE HEADERS alone, and every read of Extensions.Inference below
// it is behind a nil check — pricing.UsageFromInference(nil) is the zero Usage, which
// modelledCost refuses outright, so a header-only figure settles and a modelled one
// cannot be invented. That is what keeps this from charging for traffic nobody priced.
func (p *InferenceParser) settleCost(pctx *pipeline.Context) {
	if pctx == nil {
		return
	}
	if st := pipeline.GetState[settledOnce](pctx, costSettledKey); st != nil {
		return
	}

	settled := costing.Settle(pctx, p.rates)
	// Stored even when nothing priced, so a consumer can tell "no figure" from "this pass
	// never ran" — and so the drift check can see both figures. Unconditional and BEFORE the
	// publish gate below, which is what makes costing.Load's false mean "the cost owner's
	// response pass did not run" and nothing about the traffic's shape; see Load.
	costing.Store(pctx, settled)

	// Published when there is ANYTHING to say — a settled cost, or a saving another
	// component achieved. Gating on the cost alone would drop the saving for a model the
	// rate table cannot price, and the unpriced-model tally is precisely what tells an
	// operator which rate to add. costevent.Decode still reports such a record as
	// unpriced, so no consumer counts it as spend; that is what Record/Priced separate.
	avoided := costing.Avoided(pctx, p.rates)
	// HasPrompt is checked separately from Priced, because the two really can differ: a
	// table that prices every prompt tier but not a populated output tier yields a prompt
	// figure and no total. Dropping the record there would lose a figure a request row
	// can legitimately show, and the total stays absent rather than invented.
	// A REFUSED figure is also something to say, and it is the only case here that
	// publishes a record with no money in it. costing declined a cost header this process
	// could not corroborate (see costing.implausibleUnparsedCost), and publishing nothing
	// would make that response indistinguishable from one that reported no cost at all —
	// hiding both the misconfiguration and the forgery. The record is unpriced, so no
	// consumer counts it as spend: costevent.Priced returns false on a set RejectedReason,
	// which is what the aggregator's Decode and the ledger writer's admission guard both
	// ask.
	if !settled.Priced && !settled.HasPrompt && settled.RejectedReason == "" && len(avoided) == 0 {
		// NOT LATCHED, exactly as litellm-budget-track's bill() does not latch when
		// costing.Load finds nothing priced. Nothing was published here, so there is
		// nothing to charge twice and nothing for a latch to protect — and latching
		// anyway would lock this request out of ever being settled by a LATER pass that
		// does have a figure. The two guards exist for one reason and must agree on the
		// rule: the latch protects a PUBLISHED figure, not the attempt.
		//
		// A second pass costs a Settle and a Store, both pure and both idempotent: Store
		// overwrites the same key with the same or a better outcome.
		return
	}
	// Latched immediately before the publish it protects, and after the decision to
	// publish. Publish itself cannot fail — it assigns two map keys — so there is no
	// window here in which the latch is set and the figure is not out.
	pipeline.SetState(pctx, costSettledKey, &settledOnce{})
	costing.Publish(pctx, costing.NewRecord(settled, avoided))
}

// costSettledKey and settledOnce mark this request as already priced.
const costSettledKey = "inference-parser.cost-settled"

type settledOnce struct{}

// SetPricingResolver implements pricing.ResolverConsumer.
//
// Injected by plugins.BuildWithDeps when the process has a rate table. A nil resolver is a
// supported state, not an error: a binary built without pricing wiring reports traffic as
// unpriced rather than refusing to parse it. costing.Settle guards the interface — the trap
// being that a nil INTERFACE panics on call where a nil *pricing.Registry would not.
func (p *InferenceParser) SetPricingResolver(r pricing.Resolver) { p.rates = r }
