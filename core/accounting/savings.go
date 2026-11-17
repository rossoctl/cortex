package accounting

import (
	"context"
	"log/slog"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/pipeline"
)

// The values of event.Saving.Basis (accountant design §7.1, Cortex design §5.7a).
const (
	BasisUSD                    = event.BasisUSD
	BasisSubscriptionEquivalent = event.BasisSubscriptionEquivalent
	BasisUnknown                = event.BasisUnknown
)

// KeeperOperator is the operator a keep-alive rescue is credited to: the name accountant.PingOps
// gives the keeper's spend op. A rescue's ChangeID is the ping's window ("ka:<thread>:<window>"),
// which no op of the request it lands on carries, so it is credited by kind, not by ChangeID.
const KeeperOperator = "keeper"

// Extras are the request-level figures of a Result that belong to no operator's saving (W7.1).
type Extras struct {
	// UnattributedBreakUSD is the net cost of every cache effect no applied op owns — not only
	// breaks, despite the name, which is kept to match W7.1's Event.UnattributedBreakUSD: a break
	// with no ChangeID (a change that had already ended), an effect whose ChangeID only a dry run
	// reported, and a split, TTL or mark effect with no owning op. Positive is a cost; an unowned
	// effect that saved makes it smaller. InBaseline effects are not in it: the client body would
	// have had them too.
	UnattributedBreakUSD float64
	// Thread is the accountant thread of the request. Savings leaves it empty: a Result does not
	// name its thread. Host.Savings fills it, here and on each Spends entry.
	Thread string
	// OpsVersion is Result.Version, the version of the accountant rules.
	OpsVersion int
	// Spends are the request's spend ops — an operator's own model calls — one record each,
	// priced with the call's own model, host and prompt size. A spend is shown beside the savings,
	// never inside a Saving (W7.1).
	Spends []event.SpendRecord
}

// SpendPrice prices one model call an operator made: its whole bill, output included, at the
// call's own model and host (host is the request's when the call names none) and its own prompt
// size. estimated follows USD's rule.
type SpendPrice func(c *accountant.ModelCall, host string) (usd float64, estimated bool)

// BasisOf is event.Saving.Basis for a billing mode: "usd" for api, "subscriptionEquivalent" for
// subscription, "unknown" for anything else.
func BasisOf(m accountant.BillingMode) string {
	switch m {
	case accountant.BillingAPI:
		return BasisUSD
	case accountant.BillingSubscription:
		return BasisSubscriptionEquivalent
	}
	return BasisUnknown
}

// Units values tokens in usage-limit units: each tier times its weight, plus output times the
// output weight. A negative count (a CacheEffect's Net) gives negative units.
func Units(w config.SubscriptionWeights, tt accountant.TierTokens, output int) float64 {
	return float64(tt.Input)*w.Input + float64(tt.CacheWrite5m)*w.CacheWrite5m +
		float64(tt.CacheWrite1h)*w.CacheWrite1h + float64(tt.CacheRead)*w.CacheRead +
		float64(output)*w.Output
}

// Savings converts a Result into one event.Saving per operator, priced with price, as for a
// request billed per token (basis "usd"). It is SavingsBilled with accountant.BillingAPI, the
// default weights, and spend calls priced by price over their prompt tiers alone: price has no
// output tier, so a call with output tokens and no stated cost is priced low and marked
// estimated. Host.Savings prices the output.
func Savings(res accountant.Result, price func(model, host string, tt accountant.TierTokens) (float64, bool),
	reqModel, reqHost string) ([]event.Saving, Extras) {
	promptOnly := func(c *accountant.ModelCall, host string) (float64, bool) {
		usd, est := price(c.Model, host, callTiers(c))
		return usd, est || c.Usage.Output > 0
	}
	return SavingsBilled(res, price, promptOnly, reqModel, reqHost, accountant.BillingAPI, config.DefaultSubscriptionWeights())
}

// SavingsBilled converts a Result into one event.Saving per operator (accountant wiring W7.1),
// for a request billed in mode, with w valuing a subscription saving in usage-limit units.
//
// For each operator:
//   - USD is the priced Saved of its ops, plus the priced Net of each cache effect with a positive
//     Net under one of its ChangeIDs. A keep-alive rescue goes to KeeperOperator.
//   - PenaltyUSD is the priced Caused of its ops, plus the cost of each cache effect with a
//     negative Net under one of its ChangeIDs — a break is ONE net cost, Caused priced minus Saved
//     priced, never a saving of its Saved.
//   - Neither is floored or netted against the other: a negative net is kept as two figures.
//   - Tier is the tier with the largest Saved; TokensAvoided the sum of its Saved tiers, a size
//     for display and never priced.
//   - Estimated is true when any of its ops, its effects or the rates is estimated, and for every
//     basis but "usd".
//   - Units and PenaltyUnits are set only on a subscriptionEquivalent saving.
//
// An operator's dry-run ops are kept apart from its applied ones: they make a second saving for
// the operator with Projected set, so a "would have saved" amount never enters an applied figure.
// An operator with only dry runs has only the projected saving.
//
// A spend op (an operator's own model call) is not a saving: it becomes one Extras.Spends record,
// priced with spend, and the operator gets no Saving for it.
//
// A cache effect with InBaseline set is skipped: the client body has it too, so it is not part of
// the bill difference. One no applied op owns goes to Extras.UnattributedBreakUSD. Only an applied
// (non-DryRun) op owns a ChangeID: a dry run changed nothing upstream, so it caused nothing.
//
// The saving list is in the order each (operator, projected) pair first appears in res.Ops;
// KeeperOperator, which has no op on the request, comes last.
func SavingsBilled(res accountant.Result, price func(model, host string, tt accountant.TierTokens) (float64, bool),
	spend SpendPrice, reqModel, reqHost string, mode accountant.BillingMode, w config.SubscriptionWeights) ([]event.Saving, Extras) {
	basis := BasisOf(mode)
	ex := Extras{OpsVersion: res.Version}
	type key struct {
		op        string
		projected bool
	}
	type acc struct {
		saving    event.Saving
		saved     accountant.TierTokens // Saved of the ops, for Tier and TokensAvoided
		units     float64
		penUnits  float64
		estimated bool
	}
	var order []key
	byKey := map[key]*acc{}
	get := func(k key) *acc {
		a, ok := byKey[k]
		if !ok {
			a = &acc{saving: event.Saving{Component: k.op, Operator: k.op, OpsVersion: res.Version, Basis: basis, Projected: k.projected}}
			byKey[k] = a
			order = append(order, k)
		}
		return a
	}
	priceAt := func(model, host string, tt accountant.TierTokens) (float64, bool) {
		if tt.IsZero() {
			return 0, false
		}
		return price(model, host, tt)
	}

	owner := map[string]string{} // ChangeID -> operator, from applied ops; the first wins
	for _, op := range res.Ops {
		if op.Kind == accountant.KindSpend || op.Call != nil {
			if op.Call != nil {
				ex.Spends = append(ex.Spends, spendRecord(op, spend, reqHost, basis, w))
			}
			continue
		}
		if !op.DryRun && op.ChangeID != "" {
			if _, dup := owner[op.ChangeID]; !dup {
				owner[op.ChangeID] = op.Operator
			}
		}
		a := get(key{op.Operator, op.DryRun})
		a.estimated = a.estimated || op.Estimated
		saved, savedEst := priceAt(reqModel, reqHost, op.Saved)
		caused, causedEst := priceAt(reqModel, reqHost, op.Caused)
		a.saving.USD += saved
		a.saving.PenaltyUSD += caused
		a.estimated = a.estimated || savedEst || causedEst
		a.saved = a.saved.Add(op.Saved)
		a.units += Units(w, op.Saved, 0)
		a.penUnits += Units(w, op.Caused, 0)
	}

	for _, ce := range res.CacheEffects {
		if ce.InBaseline {
			continue
		}
		net := ce.Net()
		value, est := priceAt(reqModel, reqHost, net)
		op, owned := owner[ce.ChangeID]
		if ce.Kind == accountant.EffectRescue {
			op, owned = KeeperOperator, true
		}
		if !owned { // owner holds no empty ChangeID, so a break with none lands here
			ex.UnattributedBreakUSD -= value
			continue
		}
		a := get(key{op, false})
		a.estimated = a.estimated || ce.Estimated || est
		if value >= 0 {
			a.saving.USD += value
			a.units += Units(w, net, 0)
		} else {
			a.saving.PenaltyUSD -= value
			a.penUnits -= Units(w, net, 0)
		}
		if ce.Kind == accountant.EffectRescue {
			// The keeper has no op on this request, so its Tier comes from what the rescue saved.
			a.saved = a.saved.Add(ce.Saved)
		}
	}

	out := make([]event.Saving, 0, len(order))
	for _, k := range order {
		a := byKey[k]
		s := a.saving
		s.Tier = largestTier(a.saved)
		s.TokensAvoided = a.saved.Input + a.saved.CacheWrite5m + a.saved.CacheWrite1h + a.saved.CacheRead
		s.Estimated = a.estimated || basis != BasisUSD
		if basis == BasisSubscriptionEquivalent {
			s.Units, s.PenaltyUnits = a.units, a.penUnits
		}
		out = append(out, s)
	}
	return out, ex
}

// callTiers is a model call's prompt usage as the library's tiers (CacheWrite1h is a part of
// CacheWrite, so the 5m tier is the rest).
func callTiers(c *accountant.ModelCall) accountant.TierTokens {
	u := c.Usage.Usage
	return accountant.TierTokens{Input: u.Input, CacheWrite5m: u.CacheWrite - u.CacheWrite1h, CacheWrite1h: u.CacheWrite1h, CacheRead: u.CacheRead}
}

// spendRecord is one spend op's record: the CostUSD its response stated, when it stated one (a
// fact, R1), else the call priced with spend. Units are its tokens, output included, in
// usage-limit units, on a subscriptionEquivalent record only.
func spendRecord(op accountant.PricedOp, spend SpendPrice, reqHost, basis string, w config.SubscriptionWeights) event.SpendRecord {
	c := op.Call
	tt := callTiers(c)
	host := c.Host
	if host == "" {
		host = reqHost
	}
	r := event.SpendRecord{
		ChangeID: op.ChangeID, Operator: op.Operator, Model: c.Model, Host: host,
		Input: tt.Input, CacheWrite5m: tt.CacheWrite5m, CacheWrite1h: tt.CacheWrite1h, CacheRead: tt.CacheRead,
		Output: c.Usage.Output, Basis: basis, Estimated: op.Estimated || basis != BasisUSD,
	}
	if c.Usage.CostUSD > 0 {
		r.USD = c.Usage.CostUSD
	} else if spend != nil {
		usd, est := spend(c, host)
		r.USD, r.Estimated = usd, r.Estimated || est
	}
	if basis == BasisSubscriptionEquivalent {
		r.Units = Units(w, tt, c.Usage.Output)
	}
	return r
}

// largestTier names the tier with the largest Saved, in pricing.Tier's spelling; "" when nothing
// was saved. A tie goes to the earlier of input, cache write 5m, cache write 1h, cache read.
func largestTier(t accountant.TierTokens) string {
	best, name := 0, ""
	for _, c := range []struct {
		n    int
		tier pricing.Tier
	}{
		{t.Input, pricing.TierInput},
		{t.CacheWrite5m, pricing.TierCacheWrite},
		{t.CacheWrite1h, pricing.TierCacheWrite1h},
		{t.CacheRead, pricing.TierCacheRead},
	} {
		if c.n > best {
			best, name = c.n, c.tier.String()
		}
	}
	return name
}

// Savings is Compute, then SavingsBilled: pctx's request priced with the process rate table, at
// the request's model, the endpoint it was sent to (after any redirect: the server that bills)
// and its prompt total, in the billing mode the client's credential implied, with the configured
// subscription weights. Extras.Thread is the request's accountant thread. ok is Compute's: false
// when there is no Result, so there is no figure. Latched like Compute, so a repeated settle gets
// the same answer.
func (h *Host) Savings(pctx *pipeline.Context) ([]event.Saving, Extras, bool) {
	if h == nil || pctx == nil {
		return nil, Extras{}, false
	}
	res, ok := h.Compute(pctx)
	if !ok {
		return nil, Extras{}, false
	}
	model := ""
	if inf := pctx.Extensions.Inference; inf != nil {
		model = inf.Model
	}
	host := pctx.Host
	if _, redirected, ok := pctx.RedirectTarget(); ok {
		host = redirected
	}
	promptTotal := pricing.UsageFromInference(pctx.Extensions.Inference).PromptTotal()
	mode := accountant.BillingUnknown
	if b := pipeline.GetState[accountant.Billing](pctx, billingKey); b != nil {
		mode = b.Mode
	}
	// The request's own ops price at the request's prompt size, so its long-context tier applies.
	price := func(m, at string, tt accountant.TierTokens) (float64, bool) {
		return TierUSD(h.resolver, at, m, promptTotal, tt)
	}
	// An operator's model call is a request of its own: its prompt tiers and its output price at
	// its own prompt size, so a small call never takes the request's long-context premium.
	spend := func(c *accountant.ModelCall, at string) (float64, bool) {
		tt := callTiers(c)
		total := tt.Input + tt.CacheWrite5m + tt.CacheWrite1h + tt.CacheRead
		usd, est := TierUSD(h.resolver, at, c.Model, total, tt)
		out, outEst := USD(h.resolver, at, c.Model, total, pricing.TierOutput, int64(c.Usage.Output))
		return usd + out, est || outEst
	}
	ss, ex := SavingsBilled(res, price, spend, model, host, mode, h.weights)
	// The rate table's level for the request's own rates, on every saving that has a dollar
	// figure: event.Saving.Provenance, which a reader (tool-prune's pane, agentop) uses to say
	// whether the figure came from configured or bundled rates.
	if h.resolver != nil {
		if _, prov := h.resolver.Resolve(host, model, promptTotal); prov != pricing.ProvNone {
			for i := range ss {
				if ss[i].USD != 0 || ss[i].PenaltyUSD != 0 {
					ss[i].Provenance = prov.String()
				}
			}
		}
	}
	ex.Thread = ThreadOf(pctx).ID
	for i := range ex.Spends {
		ex.Spends[i].Thread = ex.Thread
	}
	if slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		logTokens(pctx, ex.Thread, res)
	}
	return ss, ex, true
}

// tokenFlow is what one operator saved and caused on a request, in tokens by tier.
type tokenFlow struct{ saved, caused accountant.TierTokens }

// operatorTokens is res in tokens by tier per operator, attributed by SavingsBilled's rule: the
// Saved and Caused of each applied, non-spend op, and both sides of each cache effect its applied ops own under a
// ChangeID, a rescue going to KeeperOperator. InBaseline and unowned effects are left out. It is
// the form the context-guru proxy's dashboard stores per component (CompRow.Tokens), so the
// cross-host harness can compare the two hosts token for token.
func operatorTokens(res accountant.Result) ([]string, map[string]tokenFlow) {
	var order []string
	flows := map[string]tokenFlow{}
	add := func(op string, saved, caused accountant.TierTokens) {
		f, ok := flows[op]
		if !ok {
			order = append(order, op)
		}
		f.saved, f.caused = f.saved.Add(saved), f.caused.Add(caused)
		flows[op] = f
	}
	owner := map[string]string{}
	for _, op := range res.Ops {
		if op.Kind == accountant.KindSpend || op.Call != nil {
			continue // a spend is not a saving, here as in SavingsBilled
		}
		if op.DryRun {
			continue // a dry run is never in an applied figure; the proxy's dashboard skips it too
		}
		if !op.DryRun && op.ChangeID != "" {
			if _, dup := owner[op.ChangeID]; !dup {
				owner[op.ChangeID] = op.Operator
			}
		}
		add(op.Operator, op.Saved, op.Caused)
	}
	for _, ce := range res.CacheEffects {
		if ce.InBaseline {
			continue
		}
		op, owned := owner[ce.ChangeID]
		if ce.Kind == accountant.EffectRescue {
			op, owned = KeeperOperator, true
		}
		if owned {
			add(op, ce.Saved, ce.Caused)
		}
	}
	return order, flows
}

// logTokens writes one DEBUG line per operator with its tokens by tier ("accounting: tokens").
// Token counts only: no content, no credential. The cross-host harness
// (scripts/dev/compare-contextguru-savings.py, check P1) reads these lines.
func logTokens(pctx *pipeline.Context, thread string, res accountant.Result) {
	order, flows := operatorTokens(res)
	for _, op := range order {
		f := flows[op]
		slog.Debug("accounting: tokens",
			"session", pctx.SessionID,
			"requestId", pctx.RequestID(),
			"thread", thread,
			"operator", op,
			"savedInput", f.saved.Input,
			"savedCacheWrite5m", f.saved.CacheWrite5m,
			"savedCacheWrite1h", f.saved.CacheWrite1h,
			"savedCacheRead", f.saved.CacheRead,
			"causedInput", f.caused.Input,
			"causedCacheWrite5m", f.caused.CacheWrite5m,
			"causedCacheWrite1h", f.caused.CacheWrite1h,
			"causedCacheRead", f.caused.CacheRead,
			"phase", res.Phase)
	}
}
