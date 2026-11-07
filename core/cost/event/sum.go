package event

import (
	"math"

	"github.com/rossoctl/cortex/core/model"
)

// AddSat is a + b, clamped to the int64 range instead of wrapping, and whether it clamped.
//
// The test is written as `a > math.MaxInt64-b` rather than by inspecting the sign of the
// result, because the wrapped sum is the very number this function exists to avoid. Both
// directions are guarded, so the lower clamp holds for any caller of an exported accumulator.
func AddSat(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64, true
	}
	if b < 0 && a < math.MinInt64-b {
		return math.MinInt64, true
	}
	return a + b, false
}

// CostSum is a running total of money in micros that clamps instead of wrapping, and
// remembers that it clamped.
//
// It serves the money totals derived from usage.Counts rather than held in one: a label
// breakdown's sum, the ungrouped residual of a snapshot or a ledger fold, and a session's
// spend. A wrapped residual goes negative, which a snapshot publishes as the series having
// overshot its own total, so the clamp carries its disclosure with it.
//
// A type rather than a bare saturating add, so one call writes both the figure and the flag
// and no caller can keep the clamped sum while dropping the fact that it clamped.
type CostSum struct {
	// Micros is the total so far, clamped to the int64 range.
	Micros int64
	// Saturated says Micros is a bound rather than a sum. Callers surface it as
	// usage.Counts.Saturated on the totals the sum belongs to.
	Saturated bool
}

// Add accumulates micros into s.
func (s *CostSum) Add(micros int64) {
	sum, saturated := AddSat(s.Micros, micros)
	s.Micros = sum
	if saturated {
		s.Saturated = true
	}
}

// Sub subtracts micros from s, which is how a residual is computed: a total minus what its
// breakdown accounted for.
//
// math.MinInt64 has no positive counterpart, so Add(-micros) would add it. Subtracting it
// is s + 2^63: exact for a negative s, and clamped to the ceiling otherwise.
func (s *CostSum) Sub(micros int64) {
	if micros == math.MinInt64 {
		if s.Micros < 0 {
			s.Micros = s.Micros + math.MaxInt64 + 1
			return
		}
		s.Micros, s.Saturated = math.MaxInt64, true
		return
	}
	s.Add(-micros)
}

// MaxLabelLen bounds one label in bytes, in the usage ring and in the cost ledger alike, so a
// label is one series key in both. Model names and endpoints are request-controlled, and the
// bound keeps a label's memory and the ledger's line length predictable. Long enough for any
// real model id, including provider prefixes and dated suffixes.
const MaxLabelLen = 96

// AgentLabel is the label per-agent figures key traffic from c by: versionless (see
// model.AgentName), sanitized, and capped at MaxLabelLen. A nil client is
// model.UnknownClientLabel.
//
// Sanitized before capping, because each replaced byte becomes a 3-byte U+FFFD and capping
// first would let MaxLabelLen control bytes grow to three times the bound.
func AgentLabel(c *model.EventClient) string {
	return model.TruncateLabel(model.SanitizeControlRunes(model.AgentName(c.Label())), MaxLabelLen)
}
