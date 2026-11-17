package inferenceparser

import (
	"github.com/rossoctl/cortex/core/accounting"
	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/settle"
	"github.com/rossoctl/cortex/core/pipeline"
)

// accountingComputer is what the parser needs of the accountant host: one priced figure per
// request. An interface so a test can count the calls and hand back a figure; *accounting.Host is
// the one implementation.
type accountingComputer interface {
	Savings(pctx *pipeline.Context) ([]event.Saving, accounting.Extras, bool)
}

// accountedKey holds the accountant's answer for a request, apart from costSettledKey: the cost
// latch is set only when a record is published, and the accountant is asked whether or not one
// is. A later settle pass reuses the answer rather than asking again.
const accountedKey = "inference-parser.accounted"

// accounted is the accountant's answer for one request. ok is false when it had no figure.
type accounted struct {
	savings []event.Saving
	extras  accounting.Extras
	ok      bool
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

// accountant asks the accountant for this request's savings, once, at the first settle pass that
// has usage (the Result prices the bill, so a pass with no usage has nothing for it), and keeps
// the answer on the request for any later pass. The zero value — no figure — with accounting off
// or before the usage arrives: with accounting off, no saving is recorded (design §5.3).
//
// NOT ASKED WHEN SETTLE REFUSED THE COUNTS (settled.UsageRefused, or RejectedImplausibleUsage): the
// accountant prices its savings from those same counters, so an impossible report would produce an
// impossible saving — the old settle.Avoided measured 2,000,000 tokens avoided at $20 on a record
// marked refused, and refused it for the same reason. Not latched, so a later pass with plausible
// counts is still asked; Host.Compute refuses implausible counts on its own too.
func (p *InferenceParser) accountant(pctx *pipeline.Context, settled settle.Settled) accounted {
	inf := pctx.Extensions.Inference
	if p.acct == nil || inf == nil || inf.InputTokens+inf.CacheReadTokens+inf.CacheWriteTokens == 0 {
		return accounted{}
	}
	if settled.UsageRefused || settled.RejectedReason == event.RejectedImplausibleUsage {
		return accounted{}
	}
	if st := pipeline.GetState[accounted](pctx, accountedKey); st != nil {
		return *st
	}
	var a accounted
	a.savings, a.extras, a.ok = p.acct.Savings(pctx)
	pipeline.SetState(pctx, accountedKey, &a)
	return a
}

// withAccountant writes the accountant's request-level figures on a cost record: the savings are
// already its Avoided, and these are what belongs to no operator's saving (W7.1).
func withAccountant(rec event.Event, a accounted) event.Event {
	if !a.ok {
		return rec
	}
	rec.UnattributedBreakUSD = a.extras.UnattributedBreakUSD
	rec.Thread = a.extras.Thread
	rec.Spends = a.extras.Spends
	return rec
}
