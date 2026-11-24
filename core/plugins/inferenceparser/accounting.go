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
// has usage OR a reported op, and keeps the answer on the request for any later pass. The zero
// value — no figure — with accounting off or before either arrives: with accounting off, no
// saving is recorded (design §5.3).
//
// A REPORTED OP IS ASKED FOR EVEN AT A ZERO PROMPT TOTAL. Savings prices from the upstream
// prompt-tier counters, so a request with none of them has nothing for Savings to price — but
// Spends does not depend on those counters at all (each spend record prices from its own
// operator call, spendRecord, savings.go), and the one caller of this method, withAccountant,
// copies Spends from the same Extras. Gating on the prompt total alone used to drop every spend
// on an error status with no prompt usage, or on a request a context-guru component billed
// before an inference-router resend took it past one round (review finding): the completed,
// accepted call never reached the ledger, /v1/usage or the sessions API for either. Checking
// ReportedOps too, rather than dropping the prompt-total check outright, keeps this method from
// asking on the pass before ANY data has arrived (TestSettleCost_CallsAccountingOnce's first
// call, inference extension allocated but nothing on it yet) — that pass has no ops either.
// Host.Compute's own checks (PlausibleUsage, SkipReason) still refuse what they always refused;
// this just stops refusing spends for a count Savings alone needed.
//
// NOT ASKED WHEN SETTLE REFUSED THE COUNTS (settled.UsageRefused, or RejectedImplausibleUsage): the
// accountant prices its savings from those same counters, so an impossible report would produce an
// impossible saving — the old settle.Avoided measured 2,000,000 tokens avoided at $20 on a record
// marked refused, and refused it for the same reason. Not latched, so a later pass with plausible
// counts is still asked; Host.Compute refuses implausible counts on its own too.
func (p *InferenceParser) accountant(pctx *pipeline.Context, settled settle.Settled) accounted {
	inf := pctx.Extensions.Inference
	if p.acct == nil || inf == nil {
		return accounted{}
	}
	if inf.InputTokens+inf.CacheReadTokens+inf.CacheWriteTokens == 0 && len(pctx.ReportedOps()) == 0 {
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
