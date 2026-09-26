package ledger

import "github.com/rossoctl/cortex/core/cost/pricing"

// repriceTiers fills the modelled per-tier split on rows that arrived without one, from the
// token counters the row already carries.
//
// WHY IT EXISTS. Writer.Record persists the split now, but every row written before it did
// has four token counters and no tier costs — and usage.Counts.ApportionTiers needs a MIX to
// apportion a total by, so those rows render "not known here" for the breakdown on every
// surface. With a retention of days to a month, a fix that only helps new rows leaves the
// panel blank for the whole of the retained history it is being read against. Everything
// needed to rebuild the mix is on disk: the four token counts plus the endpoint and model
// that key the rate table.
//
// A READ-TIME ENRICHMENT, NEVER A REWRITE OF THE FILES. The day files are append-only and
// they are what an operator reads tomorrow; rewriting history to add a derived column would
// put a modelled figure into the durable record and destroy the distinction between what was
// reported and what was computed. This changes only the rows in flight out of Query.
//
// TODAY'S RATE TABLE, YESTERDAY'S TRAFFIC, and that is the one caveat worth stating plainly:
// a row from a day priced under different rates gets a mix drawn from the rates in force now.
// It sits inside the caveat ApportionTiers already makes — "the mix is the rate table's, the
// magnitude is the gateway's" — because the four figures are used as a RATIO against the
// row's own authoritative total and never as a total themselves. A rate change therefore
// shifts the shape of the breakdown and cannot move the money.
//
// A NO-OP WITH A NIL RESOLVER, which is the Kubernetes deployment and every existing test in
// this package: the ledger stays a plain store and the rows come out exactly as they went in.
func repriceTiers(rows []Row, res pricing.Resolver) {
	if res == nil {
		return
	}
	for i := range rows {
		r := &rows[i]
		if hasTierSplit(*r) {
			// A PERSISTED SPLIT IS THE BETTER FIGURE and is left alone. Record copied it from
			// the producer, which priced it at the request's OWN prompt total under the rates
			// in force at the time; this function has neither. Note the test is "any of the
			// four is set", not "all four are": a request that used one tier publishes a split
			// with three zeros in it, and that is a real mix rather than an absent one.
			continue
		}
		if r.Model == "" {
			// Nothing to resolve against. A row with no model is a response the parser could
			// not read, and its spend still counts toward every total — it just contributes no
			// mix, which is a state ApportionTiers already handles.
			continue
		}
		// PER REQUEST, NOT PER ROW, and this division is the whole reason this function is
		// more than four multiplications.
		//
		// A Row is ONE MINUTE'S TOTALS for one key, so its token counters are already summed
		// over Requests requests, while everything pricing does with a usage is scoped to a
		// SINGLE request. Handing it the sum breaks two separate rules:
		//
		//   - Resolve picks the long-context rate off a threshold on one request's prompt. Four
		//     50k calls sum to 200k and would draw a premium that no individual call earned,
		//     and the error grows with how busy the minute was — which is exactly the minute
		//     someone is looking at.
		//   - CostByTier refuses a usage past pricing.MaxPlausibleTokens, a bound of 10M PER
		//     FIELD on one request. A dozen 1M-context calls sum past it, and the refusal is
		//     silent: an empty split is indistinguishable from the defect this change fixes.
		//
		// Truncating integer division, and the lost remainder does not matter: these four are
		// documented as a ratio that need not sum to CostMicros, and ApportionTiers normalises
		// against the authoritative total anyway.
		if r.Requests <= 0 {
			// Guards the division. Requests is decoded from a file, so a hand-edited or
			// truncated row can carry zero — and a row claiming tokens with no requests is not
			// a row to draw conclusions from.
			continue
		}
		mean := pricing.Usage{
			Input:      int(r.InputTokens / r.Requests),
			CacheWrite: int(r.CacheWriteTokens / r.Requests),
			CacheRead:  int(r.CacheReadTokens / r.Requests),
			Output:     int(r.OutputTokens / r.Requests),
		}
		if mean == (pricing.Usage{}) {
			// No tokens to apportion. Reached by a row that priced a body-less response, and by
			// one whose per-request mean truncated to nothing — a Requests count far larger
			// than the tokens it claims, which is corruption rather than traffic.
			continue
		}
		rates, prov := res.Resolve(r.Endpoint, r.Model, mean.PromptTotal())
		if prov == pricing.ProvNone {
			// No rate for this pair. Named nowhere and counted nowhere on purpose: the row's
			// own PricedRequests already reports whether its MONEY was settled, and this
			// function's failure to model a ratio is not a second coverage gap. UnpricedBy is
			// reconstructed from the rows themselves.
			continue
		}
		tiers, _, ok, _ := pricing.CostByTier(rates, mean)
		if !ok {
			continue
		}
		// BACK UP BY Requests, because the columns hold the minute's spend per tier and Fold
		// sums them across rows and windows. Pricing the mean and leaving it there would
		// under-report by a factor of Requests — invisible on a window with one key, and wrong
		// only where two keys have different request counts.
		scaled, fits := scaleTiers(tiers, r.Requests)
		if !fits {
			// THE WHOLE ROW, not a clamped one. A tier is bounded by
			// MaxPlausibleRequestCostMicros and the product cannot overflow for any row this
			// package wrote, so arriving here means a Requests count out of any proportion to
			// the tokens beside it. Clamping would be the worse answer for the reason
			// settle.Settle gives when it clears a split over an implausible total: a ratio
			// carries no magnitude, so a discredited one looks exactly like a sound one on
			// screen. An empty split at least says nothing.
			continue
		}
		r.InputCostMicros = scaled[pricing.TierInput]
		r.CacheWriteCostMicros = scaled[pricing.TierCacheWrite]
		r.CacheReadCostMicros = scaled[pricing.TierCacheRead]
		r.OutputCostMicros = scaled[pricing.TierOutput]
	}
}

// hasTierSplit reports whether a row already carries a modelled split.
//
// ANY TIER, not all of them: see the skip in repriceTiers for why a partial split is still a
// split. Non-zero rather than positive, so a negative figure from a damaged file counts as
// "something is here" and is left for a reader to notice rather than quietly overwritten.
func hasTierSplit(r Row) bool {
	return r.InputCostMicros != 0 || r.CacheWriteCostMicros != 0 ||
		r.CacheReadCostMicros != 0 || r.OutputCostMicros != 0
}

// scaleTiers multiplies a per-request split back up to n requests, reporting whether every
// product is still a figure this package will publish.
//
// BOUNDED AGAINST pricing.MaxCostMicros, the same ceiling usage.Counts.Add saturates at,
// because these four are summed by Fold into aggregates that must not approach the int64
// wrap. Checked by division rather than by multiplying and looking at the sign: signed
// overflow is not something to detect after the fact.
func scaleTiers(tiers [pricing.NumTiers]int64, n int64) (out [pricing.NumTiers]int64, ok bool) {
	for i, v := range tiers {
		if v == 0 {
			continue
		}
		if v > pricing.MaxCostMicros/n {
			return out, false
		}
		out[i] = v * n
	}
	return out, true
}
