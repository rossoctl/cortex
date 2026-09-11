package costing

import (
	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
)

// pruneFacts is the shape this package reads out of tool-prune's event.
//
// Read structurally rather than by importing the plugin, because the dependency must not
// run that way: a plugin that shrinks a body should know nothing about pricing, and this
// package should not link a plugin. Only the fields needed to price a saving are declared,
// so tool-prune can change everything else without touching costing.
type pruneFacts struct {
	BytesRemoved   int
	BodyBytesAfter int
	Projected      bool
}

// factsFrom pulls those fields off whatever a plugin published, via its JSON shape.
//
// The published value is a plugin-defined struct this package cannot name, so it is
// re-decoded through its own wire tags. That indirection is the price of not linking the
// plugin, and it is checked by a test that fails if tool-prune renames a field.
func factsFrom(v any) (pruneFacts, bool) {
	m, ok := asMap(v)
	if !ok {
		return pruneFacts{}, false
	}
	f := pruneFacts{
		BytesRemoved:   intField(m, "bytesRemoved"),
		BodyBytesAfter: intField(m, "bodyBytesAfter"),
		Projected:      boolField(m, "projected"),
	}
	if f.BytesRemoved <= 0 || f.BodyBytesAfter <= 0 {
		return pruneFacts{}, false
	}
	return f, true
}

// Avoided prices what other components kept off the wire, for this request.
//
// Runs HERE rather than in the component that did the pruning, because the dollar amount
// depends on which prompt-cache tier the saving came out of — 1x, 1.25x or 0.1x of the same
// rate — and that is only knowable from the response. The saving is inherently a
// request-fact times a response-fact, so it can only be priced where both are in hand.
//
// Every figure is an ESTIMATE derived from a byte ratio, and every entry says so. Nothing
// returned here is spend: see costevent.Saving.
func Avoided(pctx *pipeline.Context, rates pricing.Resolver) []costevent.Saving {
	if pctx == nil || len(pctx.Extensions.Custom) == 0 {
		return nil
	}
	usage := pricing.UsageFromInference(pctx.Extensions.Inference)
	prompt := usage.PromptTotal()
	if prompt <= 0 {
		return nil // no response usage yet: nothing to calibrate against
	}
	model := ""
	if pctx.Extensions.Inference != nil {
		model = pctx.Extensions.Inference.Model
	}

	var out []costevent.Saving
	for _, component := range savingComponents {
		v, ok := pctx.Extensions.Custom[component+pipeline.PluginEventSuffix]
		if !ok {
			continue
		}
		facts, ok := factsFrom(v)
		if !ok {
			continue
		}
		tokens := pricing.EstimateTokensFromBytes(facts.BytesRemoved, prompt, facts.BodyBytesAfter)
		if tokens <= 0 {
			continue
		}
		saved, tier := pricing.AvoidedUsage(usage, tokens)
		s := costevent.Saving{
			Component:     component,
			TokensAvoided: tokens,
			Tier:          tier.String(),
			Projected:     facts.Projected,
			Estimated:     true,
		}
		if micros, prov, ok := modelledCost(rates, pctx.Host, model, saved); ok {
			s.USD, s.Provenance = float64(micros)/1e6, prov.String()
		}
		// Published even with no dollar figure: the token saving is still known, and
		// reporting 0 dollars where a rate is missing would read as "this saved
		// nothing" rather than "we cannot price it".
		out = append(out, s)
	}
	return out
}

// savingComponents names the plugins whose events describe a body reduction worth pricing.
//
// An explicit list, not a scan for anything with a bytesRemoved field: a saving attributed
// to the wrong component is worse than one not attributed at all, and a plugin should not be
// able to opt itself into the money record by naming a field conveniently. Adding a
// component here is a deliberate act with a test.
var savingComponents = []string{"tool-prune"}
