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
	// have had them too. Host.Savings also puts here the long-context re-rate that no op owns
	// (accountant.USDFigure.UnownedReRateUSD). That is rare: the ops that moved the prompt size own
	// the re-rate, so an op that pushes a prompt over a price line pays it.
	UnattributedBreakUSD float64
	// InteractionUSD is the net value of the tokens no operation owns on a rescued request:
	// Result.Interaction minus Result.InteractionCaused, priced (design §6.2). A keep-alive ping kept
	// the cache warm and an operator changed the body, so the client body would have written these
	// tokens cold and the forwarded body read them. Positive saves. Signed. It belongs to no
	// operator's saving, so it is in no Saving: it is counted in the session total only (W7.1's
	// Event.InteractionUSD). The accountant metrics sink books the same figure as
	// llm.savings.interaction.usd (rossoctl/cortex#2139). Set on basis "usd" only, the one basis the
	// metrics price: a subscription request has no figure here, as it has none there.
	InteractionUSD float64
	// Thread is the accountant thread of the request. Savings leaves it empty: a Result does not
	// name its thread. Host.Savings fills it, here and on each Spends entry.
	Thread string
	// OpsVersion is Result.Version, the version of the accountant rules.
	OpsVersion int
	// Spends are the request's spend ops — an operator's own model calls — one record each,
	// priced with the call's own model, host and prompt size. A spend is shown beside the savings,
	// never inside a Saving (W7.1).
	Spends []event.SpendRecord
	// Compaction is Result.Compaction: set on the first request after a client compaction, with
	// the changes it ended (R55). nil on any other request.
	Compaction *event.CompactionRecord
	// Withheld is the W6.5 gate the request's Result failed (accountant.Withhold), or "". With one,
	// Host.Savings books no saving, no UnattributedBreakUSD and no InteractionUSD (the sink books no
	// interaction either): the figures come from a list of
	// operations the library says is wrong, or from an op it did not price. Spends stay: each is
	// priced from its own model call, not from the Result's attribution.
	Withheld string
}

// SpendPrice prices one model call an operator made: its whole bill, output included, at the
// call's own model and host (host is the request's when the call names none) and its own prompt
// size. estimated follows USD's rule. currency is the unit usd is denominated in, empty for the
// default (USD) — event.SpendRecord.Currency's own convention — because the call's own host can
// resolve to a different rate table than the primary request's (a credits-billed gateway beside
// a USD-endpoint operator call, review finding): usd is never assumed to be the request's
// currency just because the field is still named USD.
type SpendPrice func(c *accountant.ModelCall, host string) (usd float64, estimated bool, currency string)

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
	promptOnly := func(c *accountant.ModelCall, host string) (float64, bool, string) {
		usd, est := price(c.Model, host, callTiers(c))
		return usd, est || c.Usage.Output > 0, ""
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
//     for display and never priced. Its ops' Saved only, plus a rescue's Saved for the keeper,
//     which has no op: a split or mark effect moves tokens between tiers (a cache read in place
//     of a write) and removes none, so its dollars are in USD and its tokens are not here.
//     operatorTokens, which matches the proxy's dashboard, counts both sides of every owned
//     effect; the two token figures differ by design.
//   - Estimated is true when any of its ops, its effects or the rates is estimated, and for every
//     basis but "usd".
//   - Units and PenaltyUnits are set only on a subscriptionEquivalent saving.
//
// An operator's dry-run ops are kept apart from its applied ones: they make a second saving for
// the operator with Projected set, so a "would have saved" amount never enters an applied figure.
// An operator with only dry runs has only the projected saving.
//
// A spend op (an operator's own model call) is not a saving: it becomes one Extras.Spends record,
// priced with spend, and the operator gets no Saving for it. The record is flagged Projected for
// a dry run and Refused when res.Unpriced lists it; neither enters a spend total
// (event.SpendRecord.Counted).
//
// A cache effect with InBaseline set is skipped: the client body has it too, so it is not part of
// the bill difference. One no applied op owns goes to Extras.UnattributedBreakUSD. Only an applied
// (non-DryRun) op owns a ChangeID: a dry run changed nothing upstream, so it caused nothing.
//
// Result.Interaction minus Result.InteractionCaused, priced, goes to Extras.InteractionUSD, on
// basis "usd" only. It is in no Saving: no operator owns it (design §6.2).
//
// The saving list is in the order each (operator, projected) pair first appears in res.Ops;
// KeeperOperator, which has no op on the request, comes last.
func SavingsBilled(res accountant.Result, price func(model, host string, tt accountant.TierTokens) (float64, bool),
	spend SpendPrice, reqModel, reqHost string, mode accountant.BillingMode, w config.SubscriptionWeights) ([]event.Saving, Extras) {
	return savingsBilled(res, nil, price, spend, reqModel, reqHost, mode, w)
}

// figured is the dollars of accountant.Figure for one request, grouped the way SavingsBilled
// groups them. Figure prices the tokens an op saved at the prompt size without the ops, the
// tokens it caused at the billed size, and re-rates the tokens that stay when the two sizes are
// on different sides of a price line, so a saving is the whole bill difference
// (rossoctl/cortex#2135).
type figured struct {
	// saved and caused are per operator: its moved tokens by Direction, plus its re-rate share by
	// the share's sign. A spend's call usage is not here: Extras.Spends prices each call.
	saved, caused map[string]float64
	// effects is the value of res.CacheEffects[i], SavedUSD - CausedUSD: positive saves.
	effects []float64
	// unownedReRate is the re-rate no op owns, because no op has a net saving: positive saves.
	unownedReRate float64
	// interaction is USDFigure.InteractionUSD: positive saves.
	interaction float64
}

func newFigured(fig accountant.USDFigure) *figured {
	f := &figured{saved: map[string]float64{}, caused: map[string]float64{}, unownedReRate: fig.UnownedReRateUSD,
		interaction: fig.InteractionUSD}
	for _, l := range fig.Lines {
		if l.Kind == accountant.KindSpend {
			continue
		}
		if l.Direction == accountant.DirSaved {
			f.saved[l.Operator] += l.USD
		} else {
			f.caused[l.Operator] += l.USD
		}
	}
	for _, e := range fig.Effects {
		f.effects = append(f.effects, e.SavedUSD-e.CausedUSD)
	}
	return f
}

// savingsBilled is SavingsBilled. With fig nil, every dollar is priced at one prompt size. With
// fig set, the applied ops and the cache effects take their dollars from fig, and price still
// gives the estimated flags and the dry runs, which Figure does not price.
func savingsBilled(res accountant.Result, fig *figured, price func(model, host string, tt accountant.TierTokens) (float64, bool),
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

	// The ops the library refused to price, by OpID: their spend is recorded, flagged Refused,
	// and counted in no total — the metrics sink books none of it either (docs/metrics.md rule 3).
	refused := map[string]bool{}
	for _, u := range res.Unpriced {
		if u.OpID != "" {
			refused[u.OpID] = true
		}
	}
	owner := map[string]string{} // ChangeID -> operator, from applied ops; the first wins
	for _, op := range res.Ops {
		if op.Kind == accountant.KindSpend || op.Call != nil {
			if op.Call != nil {
				r := spendRecord(op, spend, reqHost, basis, w)
				// A dry run's call is real spend that no total counts, by method (R12): kept,
				// flagged, like a projected saving.
				r.Projected, r.Refused = op.DryRun, refused[op.OpID]
				ex.Spends = append(ex.Spends, r)
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
		if fig != nil && !op.DryRun {
			saved, caused = 0, 0 // fig's dollars, added per operator below
		}
		a.saving.USD += saved
		a.saving.PenaltyUSD += caused
		a.estimated = a.estimated || savedEst || causedEst
		a.saved = a.saved.Add(op.Saved)
		a.units += Units(w, op.Saved, 0)
		a.penUnits += Units(w, op.Caused, 0)
	}

	if fig != nil {
		for opName, usd := range fig.saved {
			get(key{opName, false}).saving.USD += usd
		}
		for opName, usd := range fig.caused {
			get(key{opName, false}).saving.PenaltyUSD += usd
		}
		ex.UnattributedBreakUSD -= fig.unownedReRate
	}

	// The interaction, at the one size Figure prices it at: the size without the ops. With no
	// figure, every dollar is priced at one size, so the interaction is too.
	if basis == BasisUSD {
		if fig != nil {
			ex.InteractionUSD = fig.interaction
		} else if net := res.Interaction.Sub(res.InteractionCaused); !net.IsZero() {
			ex.InteractionUSD, _ = price(reqModel, reqHost, net)
		}
	}

	for i, ce := range res.CacheEffects {
		if ce.InBaseline {
			continue
		}
		net := ce.Net()
		value, est := priceAt(reqModel, reqHost, net)
		if fig != nil && i < len(fig.effects) {
			value = fig.effects[i]
		}
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
		// Each side by its own sign: a model with no rate prices every break at 0, and a rate
		// table whose ratios differ from the weights can price a net saving in dollars that
		// is a net cost in units. Branching the units on the dollar sign booked such a break
		// as a negative saving (review finding).
		if value >= 0 {
			a.saving.USD += value
		} else {
			a.saving.PenaltyUSD -= value
		}
		if u := Units(w, net, 0); u >= 0 {
			a.units += u
		} else {
			a.penUnits -= u
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
		// CostUSD is the provider's own fact, always dollars by the name's own contract
		// (accountant.CallUsage's doc) — never the rate table's currency, so this is the one
		// branch that does not ask spend for one.
		r.USD = c.Usage.CostUSD
	} else if spend != nil {
		usd, est, currency := spend(c, host)
		r.USD, r.Estimated, r.Currency = usd, r.Estimated || est, currency
	}
	if basis == BasisSubscriptionEquivalent {
		r.Units = Units(w, tt, c.Usage.Output)
	}
	return r
}

// CallUSD prices one operator's model call as the ledger's spends[] does: its prompt tiers and
// its output at the call's own model, at host, and at the call's own prompt total
// (callPromptTotal), so a small call never takes the request's long-context premium. The
// metrics sink binds a spend's model to the same host and total (Sink.Observe), so the pushed
// llm.savings.added.usd{kind="spend"} equals this figure. A CostUSD the call stated is the
// ledger's business (spendRecord); this function never reads it.
func CallUSD(r pricing.Resolver, c *accountant.ModelCall, host string) (float64, bool) {
	usd, est, _ := CallUSDCurrency(r, c, host)
	return usd, est
}

// CallUSDCurrency is CallUSD plus the currency its figure is denominated in — host's, which can
// differ from the primary request's (a credits-billed gateway beside a USD-endpoint operator
// call, review finding) — empty for the default (USD), pricing.UnitOf's own convention.
func CallUSDCurrency(r pricing.Resolver, c *accountant.ModelCall, host string) (usd float64, estimated bool, currency string) {
	tt := callTiers(c)
	total := callPromptTotal(c)
	usdIn, est := TierUSD(r, host, c.Model, total, tt)
	out, outEst := USD(r, host, c.Model, total, pricing.TierOutput, int64(c.Usage.Output))
	return usdIn + out, est || outEst, pricing.UnitOf(r, host, c.Model)
}

// callPromptTotal is the call's whole prompt: input, cache write and cache read.
func callPromptTotal(c *accountant.ModelCall) int {
	u := c.Usage.Usage
	return u.Input + u.CacheWrite + u.CacheRead
}

// largestTier names the tier with the largest Saved, in pricing.Tier's spelling (cache write 1h
// has no Tier of its own — see pricing.Tier's doc — so it is named directly); "" when nothing
// was saved. A tie goes to the earlier of input, cache write 5m, cache write 1h, cache read.
func largestTier(t accountant.TierTokens) string {
	best, name := 0, ""
	for _, c := range []struct {
		n    int
		name string
	}{
		{t.Input, pricing.TierInput.String()},
		{t.CacheWrite5m, pricing.TierCacheWrite.String()},
		{t.CacheWrite1h, "cache_write_1h"},
		{t.CacheRead, pricing.TierCacheRead.String()},
	} {
		if c.n > best {
			best, name = c.n, c.name
		}
	}
	return name
}

// Savings is Compute, then SavingsBilled: pctx's request priced with the process rate table, at
// the request's model and the endpoint it was sent to (after any redirect: the server that
// bills). accountant.Figure gives the dollars of the applied ops and the cache effects, so each
// side is priced at its own prompt size and a saving is the whole bill difference
// (rossoctl/cortex#2135). A dry run is priced at the billed prompt total. It is priced in the
// billing mode the client's credential implied, with the configured subscription weights.
// Extras.Thread is the request's accountant thread. ok is Compute's: false
// when there is no Result, so there is no figure — EXCEPT that a request with a reported spend
// (an operator's own model call, pctx.ReportedOps) still answers ok with just that spend, savings
// excluded, when Compute refused for a reason that is about PRICING THE REQUEST rather than about
// the request at all (review finding: an error status with no prompt usage, or a context-guru
// call billed before an inference-router resend took the request past one round, used to lose
// the spend along with the never-priced savings). A spend prices from its own operator call
// (spendRecord) — it does not need the library's Compute, which is what makes this safe to do
// without it: Compute's thread-state transition is skipped entirely, so a multi-round request's
// mismatched usage/body pair cannot corrupt the thread's cache-effect tracking for later
// requests, which is the actual reason Compute refuses one. Latched like Compute, so a repeated
// settle gets the same answer either way.
//
// A Result that fails a W6.5 gate (withholdReason: a fatal check, an op unpriced for a reason
// other than penalty_unknown, or the size bound) books no saving: ok is true, the savings are
// empty, and Extras.Withheld names the gate. Its spends stay. A penalty_unknown op marks every
// saving Estimated instead. SavingsBilled runs no gate: it converts whatever Result it is given.
func (h *Host) Savings(pctx *pipeline.Context) ([]event.Saving, Extras, bool) {
	if h == nil || pctx == nil {
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
	// An operator's model call is a request of its own: its prompt tiers and its output price at
	// its own prompt size, so a small call never takes the request's long-context premium. Its
	// currency is its own host's, which can differ from the primary request's (a credits-billed
	// gateway beside a USD-endpoint operator call, review finding) — pricing.UnitOf, not assumed.
	spend := func(c *accountant.ModelCall, at string) (float64, bool, string) {
		return CallUSDCurrency(h.resolver, c, at)
	}
	res, ok := h.Compute(pctx)
	if !ok {
		ex := Extras{Thread: ThreadOf(pctx).ID}
		for _, op := range pctx.ReportedOps() {
			if op.Kind != accountant.KindSpend || op.DryRun || op.Call == nil {
				continue
			}
			ex.Spends = append(ex.Spends, spendRecord(accountant.PricedOp{
				OpID: op.ID, Kind: op.Kind, Operator: op.Operator, ChangeID: op.ChangeID, Call: op.Call,
			}, spend, host, BasisOf(mode), h.weights))
		}
		if len(ex.Spends) == 0 {
			return nil, Extras{}, false
		}
		for i := range ex.Spends {
			ex.Spends[i].Thread = ex.Thread
		}
		return nil, ex, true
	}
	// The applied ops and the cache effects take their dollars from accountant.Figure: saved
	// tokens at the prompt size without the ops, caused tokens at the billed size, and a re-rate of
	// the tokens that stay when the two sizes are on different sides of a long-context line
	// (rossoctl/cortex#2135). price, at the billed size, gives the estimated flags and prices the
	// dry runs, which Figure leaves out.
	price := func(m, at string, tt accountant.TierTokens) (float64, bool) {
		return TierUSD(h.resolver, at, m, promptTotal, tt)
	}
	inf := pctx.Extensions.Inference
	fig := accountant.Figure(accountant.FigureInput{
		Model: model,
		// The usage Compute was given (Host.Compute), so the two prompt sizes are the library's.
		Billed: accountant.Usage{Input: inf.InputTokens, CacheRead: inf.CacheReadTokens,
			CacheWrite: inf.CacheWriteTokens, CacheWrite1h: inf.CacheWrite1hTokens},
		Output: inf.OutputTokens,
		Result: res,
	}, sizedRates{r: h.resolver, endpoint: host})
	ss, ex := savingsBilled(res, newFigured(fig), price, spend, model, host, mode, h.weights)
	// The W6.5 gates (rossoctl/cortex#2134). Compute ran them once; a failed one books no saving.
	if why := h.withheld(pctx); why != "" {
		ss, ex.UnattributedBreakUSD, ex.InteractionUSD, ex.Withheld = nil, 0, 0, why
	} else if penaltyUnknown(res) {
		// The library could not rule out a break: the figure stands, as an estimate (§6.4).
		for i := range ss {
			ss[i].Estimated = true
		}
	}
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
	ex.Compaction = compactionRecord(res.Compaction)
	if slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		logTokens(pctx, ex.Thread, res)
	}
	return ss, ex, true
}

// tokenFlow is what one operator saved and caused on a request, in tokens by tier.
type tokenFlow struct{ saved, caused accountant.TierTokens }

// operatorTokens is res in tokens by tier per operator: the Saved and Caused of each applied,
// non-spend op, and both sides of each cache effect its applied ops own under a ChangeID, a rescue
// going to KeeperOperator. The owners are SavingsBilled's, but the token rule is not: this counts
// an owned split or mark effect's tokens, which Saving.TokensAvoided leaves out (see SavingsBilled). InBaseline and unowned effects are left out. It is
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
// Token counts only: no content, no credential.
func logTokens(pctx *pipeline.Context, thread string, res accountant.Result) {
	order, flows := operatorTokens(res)
	for _, op := range order {
		f := flows[op]
		// The session and request ids are attribution keys, not credentials: the harness
		// matches these lines to its own sessions by them (as in core/session/store.go).
		// codeql[go/clear-text-logging]
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

// compactionRecord is the cost record's copy of the accountant's compaction record, or nil.
func compactionRecord(c *accountant.CompactionRecord) *event.CompactionRecord {
	if c == nil {
		return nil
	}
	return &event.CompactionRecord{Trigger: string(c.Trigger), Ended: append([]string{}, c.Ended...)}
}
