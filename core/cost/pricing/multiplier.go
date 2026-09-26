package pricing

import (
	"fmt"
	"math"
	"strings"
)

// MultiplierRule scales every rate that resolves for an endpoint.
//
// It exists because a gateway discount is a SCALAR, not a rate card. Some gateways bill a
// uniform fraction of vendor list — one was measured at exactly 0.7600 across three
// models and all four tiers, twelve figures agreeing to four decimal places.
// Expressing that as twelve copied numbers has two costs a multiplier does not:
//
//   - Twelve chances to fumble a decimal place, in a file nothing validates against
//     the gateway.
//   - It goes stale silently the moment Anthropic reprices. The gateway's price is
//     DERIVED from list, so a multiplier tracks a repricing automatically once the
//     bundled table is refreshed, while copied rates keep reporting last quarter's
//     numbers with no indication they are wrong.
type MultiplierRule struct {
	// Host is a glob matched against the request's target host, port stripped. Empty
	// or "*" scales every endpoint.
	Host string
	// Factor multiplies each resolved rate. 1.0 means list.
	Factor float64
	// Prov records where the factor came from. A scaled figure reports the STRONGER of
	// its two sources, and it also ranks these rules against each other — an operator's
	// factor for an endpoint beats the shipped one. See Resolve for why stronger.
	Prov Provenance
}

// maxMultiplier bounds the factor.
//
// The typo this catches is `multiplier: 76` for 0.76, which inflates every figure a
// hundredfold and reads as entirely plausible in a config file. Nothing legitimate
// marks a gateway up tenfold over vendor list, so anything past 10 is a mistake and
// worth refusing at startup rather than reporting as spend.
const maxMultiplier = 10.0

func (m MultiplierRule) validate(what string) error {
	switch {
	case math.IsNaN(m.Factor) || math.IsInf(m.Factor, 0):
		return fmt.Errorf("%s: multiplier must be a finite number", what)
	case m.Factor <= 0:
		// Zero would price the endpoint's entire traffic at nothing, which reads as
		// "this gateway is free" rather than as the misconfiguration it is.
		return fmt.Errorf("%s: multiplier must be > 0 (got %v); omit it to mean 1.0", what, m.Factor)
	case m.Factor > maxMultiplier:
		return fmt.Errorf("%s: multiplier %v exceeds the sanity bound of %v — did you mean %v? (it is a FRACTION of list, so a 24%% discount is 0.76)",
			what, m.Factor, maxMultiplier, m.Factor/100)
	}
	if !anyHost(m.Host) {
		if err := validHostPattern(strings.ToLower(m.Host)); err != nil {
			return fmt.Errorf("%s: multiplier host pattern %q: %w", what, m.Host, err)
		}
	}
	return nil
}

// scale returns r with every set rate multiplied — Base and any long-context override.
//
// Resolve calls this on ALREADY-FLATTENED rates, so in that path Thresholds is empty and
// the loop below does nothing. It is still written to scale them, because the alternative
// readings are both wrong for any other caller: passing thresholds through unscaled
// leaves one Rates value carrying two different scales, so a later At() would fold a
// list-price premium onto a discounted base; dropping them loses the premium entirely and
// silently underprices long requests. A discounted gateway discounts its long-context
// tier too.
//
// Unreachable from Resolve is not the same as untested — TestRates_ScaleScalesThresholds
// calls it directly, so the branch has coverage independent of its caller.
func (r Rates) scale(f float64) Rates {
	if f == 1 {
		return r
	}
	out := Rates{Set: r.Set}
	for i := range r.Base {
		out.Base[i] = r.Base[i] * f
	}
	if len(r.Thresholds) > 0 {
		out.Thresholds = make([]ContextThreshold, len(r.Thresholds))
		for i, th := range r.Thresholds {
			scaled := ContextThreshold{AbovePromptTokens: th.AbovePromptTokens, Set: th.Set}
			for tier := range th.Rate {
				scaled.Rate[tier] = th.Rate[tier] * f
			}
			out.Thresholds[i] = scaled
		}
	}
	return out
}
