package session

import "github.com/rossoctl/cortex/core/cost/event"

// figureSum is a running total of event.Figures — a session's savings, penalties, plugin
// spends and keep-alive spend by basis — that clamps each field instead of wrapping and remembers that it clamped,
// by event.CostSum's rule. One flag for every field, as SessionSummary.Saturated is one flag for
// every money figure on a row.
type figureSum struct {
	Figures   event.Figures
	Saturated bool
}

// fields lists f's fields, so Add and Sub cannot miss one.
func fields(f *event.Figures) [13]*int64 {
	return [13]*int64{&f.AvoidedMicros, &f.PenaltyMicros, &f.AvoidedUnits, &f.PenaltyUnits, &f.AvoidedAPIEquivalentMicros,
		&f.AvoidedUnknownMicros, &f.PluginSpendMicros, &f.PluginSpendUnits, &f.PluginSpendUnknownMicros,
		&f.KeepAliveMicros, &f.KeepAliveUnits, &f.KeepAliveAPIEquivalentMicros, &f.KeepAliveUnknownMicros}
}

// Add accumulates o into s.
func (s *figureSum) Add(o event.Figures) { s.apply(o, (*event.CostSum).Add) }

// Sub subtracts o from s, for a trim.
func (s *figureSum) Sub(o event.Figures) { s.apply(o, (*event.CostSum).Sub) }

// Merge adds another sum, carrying its flag: a total that clamped on either side is a bound.
func (s *figureSum) Merge(o figureSum) {
	s.Add(o.Figures)
	s.Saturated = s.Saturated || o.Saturated
}

func (s *figureSum) apply(o event.Figures, op func(*event.CostSum, int64)) {
	dst, src := fields(&s.Figures), fields(&o)
	for i := range dst {
		c := event.CostSum{Micros: *dst[i]}
		op(&c, *src[i])
		*dst[i] = c.Micros
		if c.Saturated {
			s.Saturated = true
		}
	}
}

// setOn writes the sum's figures on a SessionSummary.
func (s figureSum) setOn(sum *SessionSummary) {
	f := s.Figures
	sum.AvoidedMicros = f.AvoidedMicros
	sum.PenaltyMicros = f.PenaltyMicros
	sum.AvoidedUnits = f.AvoidedUnits
	sum.PenaltyUnits = f.PenaltyUnits
	sum.AvoidedAPIEquivalentMicros = f.AvoidedAPIEquivalentMicros
	sum.AvoidedUnknownMicros = f.AvoidedUnknownMicros
	sum.PluginSpendMicros = f.PluginSpendMicros
	sum.PluginSpendUnits = f.PluginSpendUnits
	sum.PluginSpendUnknownMicros = f.PluginSpendUnknownMicros
	sum.KeepAliveMicros = f.KeepAliveMicros
	sum.KeepAliveUnits = f.KeepAliveUnits
	sum.KeepAliveAPIEquivalentMicros = f.KeepAliveAPIEquivalentMicros
	sum.KeepAliveUnknownMicros = f.KeepAliveUnknownMicros
}
