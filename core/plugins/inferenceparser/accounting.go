package inferenceparser

import (
	"log/slog"
	"math"
	"sync/atomic"

	"github.com/rossoctl/cortex/core/accounting"
	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/pipeline"
)

// accountingComputer is what the parser needs of the accountant host: one priced figure per
// request. An interface so a test can count the calls and hand back a figure; *accounting.Host is
// the one implementation.
type accountingComputer interface {
	Savings(pctx *pipeline.Context) ([]event.Saving, accounting.Extras, bool)
}

// accountedKey latches the accountant call for a request, apart from costSettledKey: the cost
// latch is set only when a record is published, and the accountant is asked whether or not one is.
const accountedKey = "inference-parser.accounted"

type accountedOnce struct{}

// shadowTolerance is the difference, in dollars, below which the two figures agree: a micro-dollar,
// the ledger's unit.
const shadowTolerance = 0.000001

// shadowCounts counts the requests whose figures were compared and those that differed, for the
// plugin's metrics. Per process, reset on reload with the rest of the plugin.
type shadowCounts struct {
	compared, differed atomic.Int64
}

// SetAccounting implements plugins.AccountingConsumer. Injected only on the outbound build, and
// only when accounting is on.
func (p *InferenceParser) SetAccounting(h *accounting.Host) {
	if h == nil {
		p.acct = nil // not a typed nil: that would read as set
		return
	}
	p.acct = h
}

// shadow asks the accountant for this request's savings, once, at the first settle pass that has
// usage (the Result prices the bill, so a pass with no usage has nothing for it), and compares
// them with old, the figure settle.Avoided computed and the record still publishes (design §6,
// shadow agreement). Nothing the accountant computed is written anywhere yet.
//
// A difference above shadowTolerance in any component's net figure (USD minus PenaltyUSD) is
// logged at INFO as "accounting: shadow", one line per component, and counted. A component only
// one side priced counts as a difference against 0. Projected savings are compared like applied
// ones: both sides mark the same op the same way.
func (p *InferenceParser) shadow(pctx *pipeline.Context, old []event.Saving) {
	inf := pctx.Extensions.Inference
	if p.acct == nil || inf == nil || inf.InputTokens+inf.CacheReadTokens+inf.CacheWriteTokens == 0 {
		return
	}
	if pipeline.GetState[accountedOnce](pctx, accountedKey) != nil {
		return
	}
	pipeline.SetState(pctx, accountedKey, &accountedOnce{})
	next, extras, ok := p.acct.Savings(pctx)
	if !ok {
		return
	}
	p.shadowCounts.compared.Add(1)

	type pair struct {
		old, new, penalty float64
		inOld, inNew      bool
	}
	var order []string
	pairs := map[string]*pair{}
	at := func(c string) *pair {
		if pp, ok := pairs[c]; ok {
			return pp
		}
		pp := &pair{}
		pairs[c] = pp
		order = append(order, c)
		return pp
	}
	for _, s := range old {
		pp := at(s.Component)
		pp.old += s.USD - s.PenaltyUSD
		pp.inOld = true
	}
	for _, s := range next {
		pp := at(s.Component)
		pp.new += s.USD - s.PenaltyUSD
		pp.penalty += s.PenaltyUSD
		pp.inNew = true
	}
	differed := false
	for _, c := range order {
		pp := pairs[c]
		diff := pp.new - pp.old
		if math.Abs(diff) <= shadowTolerance {
			continue
		}
		differed = true
		slog.Info("accounting: shadow",
			"session", pctx.SessionID,
			"requestId", pctx.RequestID(),
			"thread", extras.Thread,
			"component", c,
			"oldUSD", pp.old,
			"newUSD", pp.new,
			"newPenaltyUSD", pp.penalty,
			"diffUSD", diff,
			"inOld", pp.inOld,
			"inNew", pp.inNew,
			"unattributedBreakUSD", extras.UnattributedBreakUSD,
			"opsVersion", extras.OpsVersion)
	}
	if differed {
		p.shadowCounts.differed.Add(1)
	}
}

// Metrics implements pipeline.MetricsProvider: the shadow comparison's two counters, once the
// accountant has priced a request. Nil before that, so a parser with accounting off shows none.
func (p *InferenceParser) Metrics() []pipeline.Metric {
	compared := p.shadowCounts.compared.Load()
	if compared == 0 {
		return nil
	}
	return []pipeline.Metric{
		{Name: "accounting shadow compared", Value: float64(compared), Unit: "requests",
			Note: "requests the accountant priced beside the current saving figure"},
		{Name: "accounting shadow differed", Value: float64(p.shadowCounts.differed.Load()), Unit: "requests",
			Note: "requests where the two figures differ by more than $0.000001; logged as accounting: shadow"},
	}
}
