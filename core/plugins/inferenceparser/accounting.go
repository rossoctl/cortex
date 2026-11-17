package inferenceparser

import (
	"context"
	"log/slog"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/accounting"
	"github.com/rossoctl/cortex/core/pipeline"
)

// accountingComputer is what the parser needs of the accountant host: one Result per request.
// An interface so a test can count the calls; *accounting.Host is the one implementation.
type accountingComputer interface {
	Compute(pctx *pipeline.Context) (accountant.Result, bool)
}

// accountedKey latches the accountant call for a request, apart from costSettledKey: the cost
// latch is set only when a record is published, and the accountant is asked whether or not one is.
const accountedKey = "inference-parser.accounted"

type accountedOnce struct{}

// SetAccounting implements plugins.AccountingConsumer. Injected only on the outbound build, and
// only when accounting is on.
func (p *InferenceParser) SetAccounting(h *accounting.Host) {
	if h == nil {
		p.acct = nil // not a typed nil: that would read as set
		return
	}
	p.acct = h
}

// account asks the accountant for this request's Result, once, at the first settle pass that has
// usage: the Result prices the bill, so a pass with no usage has nothing for it.
//
// DEBUG output only, for now: no figure is written anywhere yet. The ledger keeps today's
// settle.Avoided path until the accountant's figures replace it (design §7).
func (p *InferenceParser) account(pctx *pipeline.Context) {
	inf := pctx.Extensions.Inference
	if p.acct == nil || inf == nil || inf.InputTokens+inf.CacheReadTokens+inf.CacheWriteTokens == 0 {
		return
	}
	if pipeline.GetState[accountedOnce](pctx, accountedKey) != nil {
		return
	}
	pipeline.SetState(pctx, accountedKey, &accountedOnce{})
	res, ok := p.acct.Compute(pctx)
	if !ok || !slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	var saved accountant.TierTokens
	for _, op := range res.Ops {
		saved = saved.Add(op.Saved)
	}
	slog.Debug("accounting: computed",
		"requestId", pctx.RequestID(),
		"ops", len(res.Ops),
		"savedInput", saved.Input,
		"savedCacheWrite5m", saved.CacheWrite5m,
		"savedCacheWrite1h", saved.CacheWrite1h,
		"savedCacheRead", saved.CacheRead,
		"phase", res.Phase,
		"reliable", res.Reliable())
}
