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
// Only AvoidedMicros, PenaltyMicros and PluginSpendMicros are money in the window's unit. A
// subscription saving is in usage-limit units, with its API-equivalent dollars beside it and
// labelled so; a figure whose billing mode is unknown is shown and never added; keep-alive spend
// has its own line.

// NetMicros is saved minus penalty: what the context operators saved net of what they caused. Not
// floored — a negative net means they cost more than they saved.
func NetMicros(c usage.Counts) int64 {
	n, _ := event.AddSat(c.AvoidedMicros, -c.PenaltyMicros)
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
// plugin model calls, keep-alive. Nil when there is none. unit is the window's money unit.
func AccountantLines(c usage.Counts, unit string) []Line {
	var out []Line
	if c.AvoidedMicros != 0 || c.PenaltyMicros != 0 {
		out = append(out, Line{"saved", Signed(c.AvoidedMicros, unit) + "   penalty " + Signed(c.PenaltyMicros, unit) +
			"   net " + Signed(NetMicros(c), unit)})
	}
	if c.AvoidedUnits != 0 || c.PenaltyUnits != 0 {
		f := unitsOf(c.AvoidedUnits) + " saved   penalty " + unitsOf(c.PenaltyUnits) +
			"   net " + unitsOf(c.AvoidedUnits-c.PenaltyUnits)
		if c.AvoidedAPIEquivalentMicros != 0 {
			// Dollars at API rates for traffic billed on a subscription: labelled, never summed.
			f += "   API equivalent net " + Signed(c.AvoidedAPIEquivalentMicros, unit)
		}
		out = append(out, Line{"usage-limit units", f})
	}
	if c.AvoidedUnknownMicros != 0 {
		out = append(out, Line{"unknown billing", Signed(c.AvoidedUnknownMicros, unit) + " net, not in any total"})
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
	return out
}
