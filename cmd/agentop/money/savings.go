package money

import (
	"fmt"
	"strings"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/usage"
)

// The accountant's figures beside the money totals (Cortex design §5.7a, §5.9), rendered once for
// `agentop cost` and the TUI.
//
// Only AvoidedMicros, PenaltyMicros, InteractionMicros, PluginSpendMicros and KeepAliveMicros are money in the
// window's unit. A subscription saving is in usage-limit units, with its API-equivalent dollars
// beside it and labelled so; a figure whose billing mode is unknown is shown and never added; the
// plugins' own model-call spend has its own line, and so does keep-alive spend, split by basis the
// same way. Keep-alive spend is ALSO in the spend total (SpendMicros) and off every net saving of
// its basis (NetMicros), because the keeper's rescue is in the saving.

// NetMicros is saved minus penalty minus the keep-alive pings' spend, plus the interaction: what
// the context operators saved net of what they caused and of what keeping the caches warm cost.
// The keeper's rescue is in AvoidedMicros, so its pings come off the same figure, or the keeper's
// saving would be counted without its cost. The interaction (usage.Counts.InteractionMicros) is
// the value of the tokens no operator owns on a rescued request: in no operator's figure, but in
// the net, as the accountant metrics count it (rossoctl/cortex#2139). Usd basis only, like every
// term. Not floored — a negative net means they cost more than they saved.
func NetMicros(c usage.Counts) int64 {
	n, _ := event.AddSat(c.AvoidedMicros, -c.PenaltyMicros)
	n, _ = event.AddSat(n, -c.KeepAliveMicros)
	n, _ = event.AddSat(n, c.InteractionMicros)
	return n
}

// netUnits is NetMicros for the subscriptionEquivalent figures, in milli-units: saved minus
// penalty minus the keep-alive pings' units.
func netUnits(c usage.Counts) int64 { return c.AvoidedUnits - c.PenaltyUnits - c.KeepAliveUnits }

// HasSpend reports whether c has a spend figure to show: a priced request, or a keep-alive ping on
// an API key. PricedRequests alone is not the test any more, because a ping is in no request
// counter: an idle hour with only pings is the usual window keep-alive makes, and it has a real
// charge. A genuine zero stays distinguishable from no figure, as with PricedRequests alone.
func HasSpend(c usage.Counts) bool { return c.PricedRequests > 0 || c.KeepAliveMicros != 0 }

// SpendPriced is HasSpend for a whole snapshot: its Priced flag (PricedRequests > 0 on the wire),
// or usd keep-alive spend in its totals.
func SpendPriced(snap *usage.Snapshot) bool {
	return snap != nil && (snap.Priced || snap.Totals.KeepAliveMicros != 0)
}

// SpendMicros is what the window spent, in money: the requests' cost plus the keep-alive pings on
// an API key. A ping is billed on the caller's credential like any request, so a total that left
// it out would understate the bill. The subscription and unknown-billing pings are not money and
// stay out, as they stay out of NetMicros. The plugins' own model calls are not added: they are
// shown on their own line.
func SpendMicros(c usage.Counts) int64 {
	n, _ := event.AddSat(c.CostMicros, c.KeepAliveMicros)
	return n
}

// Signed is In for micros that may be negative: "-$0.12", "-2.50 credits".
func Signed(micros int64, unit string) string {
	if micros < 0 {
		return "-" + In(float64(-micros)/1e6, unit)
	}
	return In(float64(micros)/1e6, unit)
}

// unitsOf renders milli-units as units, one decimal.
func unitsOf(milli int64) string { return fmt.Sprintf("%.1f", float64(milli)/1000) }

// Line is one labelled accountant figure.
type Line struct {
	Label  string
	Figure string
}

// AccountantLines is c's accountant figures, one Line per kind that is nonzero, in a fixed order:
// usd savings (saved, penalty, net), usage-limit units (with "API equivalent"), unknown billing,
// plugin model calls. Nil when there is none. unit is the window's money unit.
func AccountantLines(c usage.Counts, unit string) []Line {
	var out []Line
	if c.AvoidedMicros != 0 || c.PenaltyMicros != 0 || c.InteractionMicros != 0 {
		f := Signed(c.AvoidedMicros, unit) + "   penalty " + Signed(c.PenaltyMicros, unit)
		if c.KeepAliveMicros != 0 {
			f += "   keep-alive " + Signed(c.KeepAliveMicros, unit)
		}
		if c.InteractionMicros != 0 {
			f += "   interaction " + Signed(c.InteractionMicros, unit)
		}
		out = append(out, Line{"saved", f + "   net " + Signed(NetMicros(c), unit)})
	}
	if c.AvoidedUnits != 0 || c.PenaltyUnits != 0 {
		f := unitsOf(c.AvoidedUnits) + " saved   penalty " + unitsOf(c.PenaltyUnits)
		if c.KeepAliveUnits != 0 {
			f += "   keep-alive " + unitsOf(c.KeepAliveUnits)
		}
		f += "   net " + unitsOf(netUnits(c))
		if equiv := c.AvoidedAPIEquivalentMicros - c.KeepAliveAPIEquivalentMicros; equiv != 0 {
			// Dollars at API rates for traffic billed on a subscription: labelled, never summed.
			// Net of the subscription pings' API equivalent, as the units are net of their units.
			f += "   API equivalent net " + Signed(equiv, unit)
		}
		out = append(out, Line{"usage-limit units", f})
	}
	if c.AvoidedUnknownMicros != 0 {
		out = append(out, Line{"unknown billing", Signed(c.AvoidedUnknownMicros-c.KeepAliveUnknownMicros, unit) + " net, not in any total"})
	}
	if c.PluginSpendMicros != 0 || c.PluginSpendUnits != 0 || c.PluginSpendUnknownMicros != 0 {
		var parts []string
		if c.PluginSpendMicros != 0 {
			parts = append(parts, Signed(c.PluginSpendMicros, unit))
		}
		if c.PluginSpendUnits != 0 {
			parts = append(parts, unitsOf(c.PluginSpendUnits)+" units")
		}
		if c.PluginSpendUnknownMicros != 0 {
			parts = append(parts, Signed(c.PluginSpendUnknownMicros, unit)+" unknown billing")
		}
		out = append(out, Line{"plugin model calls", strings.Join(parts, "   ")})
	}
	if c.KeepAlivePings != 0 || c.KeepAliveMicros != 0 || c.KeepAliveUnits != 0 ||
		c.KeepAliveAPIEquivalentMicros != 0 || c.KeepAliveUnknownMicros != 0 {
		// Split like plugin spend: only pings on an API key are money. A subscription's pings
		// are units, their API equivalent labelled beside them; unknown billing is apart.
		var parts []string
		if c.KeepAliveMicros != 0 {
			parts = append(parts, Signed(c.KeepAliveMicros, unit))
		}
		// A subscription ping whose units round to zero still has its API equivalent to show.
		if c.KeepAliveUnits != 0 || c.KeepAliveAPIEquivalentMicros != 0 {
			f := unitsOf(c.KeepAliveUnits) + " units"
			if c.KeepAliveAPIEquivalentMicros != 0 {
				f += " (API equivalent " + Signed(c.KeepAliveAPIEquivalentMicros, unit) + ")"
			}
			parts = append(parts, f)
		}
		if c.KeepAliveUnknownMicros != 0 {
			parts = append(parts, Signed(c.KeepAliveUnknownMicros, unit)+" unknown billing")
		}
		parts = append(parts, fmt.Sprintf("%d ping%s", c.KeepAlivePings, plural(c.KeepAlivePings)))
		out = append(out, Line{"keep-alive", strings.Join(parts, "   ")})
	}
	return out
}

func plural(n int64) string {
	if n == 1 {
		return ""
	}
	return "s"
}
