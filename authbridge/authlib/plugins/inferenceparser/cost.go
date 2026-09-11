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
// listener can dispatch a terminal frame twice — extproc does, once for headers and once
// for the buffered body — and charging twice for one request is the failure that guard
// exists to prevent. It mirrors the one litellm-budget-track keeps for the same reason.
func (p *InferenceParser) settleCost(pctx *pipeline.Context) {
	if pctx == nil || pctx.Extensions.Inference == nil {
		return
	}
	if st := pipeline.GetState[settledOnce](pctx, costSettledKey); st != nil {
		return
	}
	pipeline.SetState(pctx, costSettledKey, &settledOnce{})

	settled := costing.Settle(pctx, p.rates)
	// Stored even when nothing priced, so a consumer can tell "no figure" from "no
	// inference at all" — and so the drift check can see both figures.
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
	if !settled.Priced && !settled.HasPrompt && len(avoided) == 0 {
		return
	}
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
