package usage

import (
	"math"
	"sort"

	"github.com/rossoctl/cortex/core/cost/event"
)

// MaxSeriesInResponse bounds how many label series ONE RESPONSE carries, per bucket.
//
// A SEPARATE BOUND FROM maxLabelsPerBucket, and the reason both exist is that they bound
// different things. That one is a MEMORY bound at record time: 64 per axis, so a caller
// varying a label per request cannot grow this process. This one is a RESPONSE bound, and
// nothing was enforcing it — the two multiply. Measured on the endpoint as shipped:
// window=6h&resolution=1m&group=session is 360 buckets times up to 64 series, which produced
// a 4.7 MB response from a single unauthenticated GET.
//
// SIXTEEN, because the response is something a person reads. A by-model or by-agent breakdown
// past a dozen rows is not a breakdown anyone consumes; the rest belongs in the (other) band,
// which every client already renders because the record-time cap has always produced it. That
// is the property that makes this cheap: no new wire vocabulary, and a client written against
// the old shape shows a correct total with one extra row.
//
// RANKED ACROSS THE WHOLE WINDOW, not per bucket, which is the part that has to be got right.
// Capping each bucket independently would let the SAME label be a series in one minute and
// part of (other) in the next, so a chart would show a line appearing and vanishing while the
// traffic behind it was steady. The ranking is by cost, because this is a cost API and the
// question "where did the money go" is the one the top of the list has to answer.
const MaxSeriesInResponse = 16

// CapSeries folds everything past the n costliest entries into the (other) band.
//
// EXPORTED FOR cost/ledger, which serves the symbolic windows from disk and had no bound of its
// own at all: its rows come from day files, so the number of distinct (endpoint, model, agent)
// keys in a response is however many a month of traffic produced. One map rather than a whole
// snapshot, because that source answers with a single bucket.
//
// Ranked by cost, ties broken by requests and then by label, so the answer is deterministic —
// a breakdown whose membership changed between two identical requests would be worse than an
// unbounded one, because a client could not tell a traffic change from a tie-break.
//
// The (other) entry is Counts.Add-ed, so every field it carries is summed by the one
// summation point and a field added to Counts is carried here with no edit.
func CapSeries(series map[string]Counts, n int) map[string]Counts {
	if len(series) <= n {
		return series
	}
	keys := make([]string, 0, len(series))
	for k := range series {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := series[keys[i]], series[keys[j]]
		if a.CostMicros != b.CostMicros {
			return a.CostMicros > b.CostMicros
		}
		if a.Requests != b.Requests {
			return a.Requests > b.Requests
		}
		return keys[i] < keys[j]
	})
	out := make(map[string]Counts, n+1)
	var other Counts
	for i, k := range keys {
		if i < n {
			out[k] = series[k]
			continue
		}
		other.Add(series[k])
	}
	// Merged rather than assigned: a label spelled "(other)" by a caller is already in the
	// map, and overwriting it would drop that traffic. The band is not a reserved key — see
	// cost/ledger's overflowLabel, which makes the same point about spoofability.
	cur := out[overflowLabel]
	cur.Add(other)
	out[overflowLabel] = cur
	return out
}

// FoldSeriesAcrossWindow sums each series label's Counts over every bucket, giving one total
// per label for the whole window.
//
// EXTRACTED FROM capSeriesAcrossWindow, which carried this loop inline to build the ranking it
// caps by, and exported because two consumers outside this package need the same answer:
// agentop's AGENTS pane, which renders one row per agent for the window, and
// `agentop cost --agent`, which needs one label's total plus the set of labels to name in a
// "no such agent" error. Either writing the loop again would be a second definition of what a
// window total means.
//
// THROUGH Counts.Add, which is the reason it is worth sharing rather than retyping: Add
// saturates and records it in Saturated, where a hand-written `+=` wraps negative. The
// consumers that RANK OR COMPARE the result are the ones that argument is for: a wrapped cost
// total sorts BELOW a ten-micro one, so in the pane that orders agents by spend it silently
// moves the biggest spender to the bottom. rankSeriesByCost's godoc in agentop records the same
// trap being hit for real. `agentop cost --agent` looks up a single label and does neither, and
// wants the same saturating answer anyway — one definition of a window total, not two.
//
// Returns an empty map rather than nil for no buckets, so "no traffic" is an empty answer and
// not something a caller has to nil-check.
func FoldSeriesAcrossWindow(buckets []Bucket) map[string]Counts {
	window := map[string]Counts{}
	for _, b := range buckets {
		for k, v := range b.Series {
			cur := window[k]
			cur.Add(v)
			window[k] = cur
		}
	}
	return window
}

// SortSeriesLabels orders labels IN PLACE by what they cost, descending, breaking ties on the
// label.
//
// ONE DEFINITION FOR EVERY SURFACE THAT RANKS A BREAKDOWN. agentop's AGENTS pane and
// `agentop cost --by` show the same series in the same order, and the rule lives here so the two
// cannot drift — the spend drawer's rankSeriesByCost records what it cost the last time a
// consumer wrote its own.
//
// THE TIE-BREAK IS LOAD-BEARING, not tidiness. Every UNPRICED series has CostMicros 0, so until
// billing units land the label is the entire order for all of them; and these rows are rebuilt
// on every poll, so without it equal-cost entries swap places between reads under a reader
// trying to compare them.
//
// A SLICE, NOT A MAP, and the signature is the point. Ranking straight out of a map takes the
// tie order from Go's randomised walk, and sort.Slice is unstable, so the tied block is permuted
// by the sort itself — a test for the tie-break could then only catch its deletion when the
// random order happened to be wrong. Given a slice the result is a pure function of the input and
// the assertion holds every run.
//
// A label absent from series sorts as zero cost: the map lookup yields the zero Counts, which
// keeps the ordering total rather than panicking, and the label tie-break still places it
// stably.
func SortSeriesLabels(labels []string, series map[string]Counts) {
	sort.Slice(labels, func(i, j int) bool {
		a, b := series[labels[i]].CostMicros, series[labels[j]].CostMicros
		if a != b {
			return a > b
		}
		return labels[i] < labels[j]
	})
}

// capSeriesAcrossWindow applies MaxSeriesInResponse to every bucket using ONE ranking taken
// over the whole window, so a label is either a series everywhere or (other) everywhere.
//
// See MaxSeriesInResponse for why per-bucket capping is the wrong shape. The ranking is built
// from the same Counts.Add the totals use, so "costliest" means the same thing here as in the
// figure the client renders above the chart.
func capSeriesAcrossWindow(buckets []Bucket, n int) {
	window := FoldSeriesAcrossWindow(buckets)
	if len(window) <= n {
		return
	}
	keep := make(map[string]bool, n)
	for k := range CapSeries(window, n) {
		keep[k] = true
	}
	for i := range buckets {
		if len(buckets[i].Series) == 0 {
			continue
		}
		out := make(map[string]Counts, n+1)
		var other Counts
		for k, v := range buckets[i].Series {
			if keep[k] {
				out[k] = v
				continue
			}
			other.Add(v)
		}
		if other != (Counts{}) {
			cur := out[overflowLabel]
			cur.Add(other)
			out[overflowLabel] = cur
		}
		buckets[i].Series = out
	}
}

// residualOf splits a residual into the two fields that publish it: shortfall for a positive
// one, overshoot for the magnitude of a negative one. At most one is non-nil.
//
// RETURNS THEM RATHER THAN WRITING THROUGH **int64 OUT-PARAMS, which is what it did first and
// is a hazard worth naming. Two `**int64` parameters are type-identical and positionally
// interchangeable, so `setResidual(sum, &s.UngroupedAvoidedMicros, &s.SeriesOvershootMicros,
// …)` — the avoided shortfall wired to the COST overshoot — compiled and passed the entire
// suite. Returning the pair puts the destination field names on the left of an assignment
// inside the setter that owns them, where a cross-field write has to be written out to happen
// and the positive tests for both fields catch it.
//
// Extracted rather than duplicated because the MinInt64 sign trap below is the one piece of
// arithmetic here that must be obviously right, and a copy is a second place to get it wrong.
func residualOf(sum event.CostSum) (shortfall, overshoot *int64) {
	micros := sum.Micros
	switch {
	case micros > 0:
		return &micros, nil
	case micros < 0:
		// Negated into a magnitude, with the MinInt64 case guarded: negating it blindly
		// returns the same negative number and publishes the sign confusion the overshoot
		// field exists to avoid. Reachable only from a saturated total, itself disclosed.
		over := int64(math.MaxInt64)
		if micros != math.MinInt64 {
			over = -micros
		}
		return nil, &over
	}
	return nil, nil
}

// seriesAvoided is the avoided cost a label breakdown accounts for.
//
// Reads Counts.AvoidedMicros per entry for the reason seriesCost reads CostMicros: the series
// is what a client sums, and this has to be exactly that arithmetic.
func seriesAvoided(series map[string]Counts) event.CostSum {
	var total event.CostSum
	for _, v := range series {
		total.Add(v.AvoidedMicros)
	}
	return total
}

// seriesCost is the dollars a label breakdown accounts for.
//
// Reads Counts.CostMicros per entry rather than any running total, because the series
// is what a client sums and this has to be exactly that arithmetic.
//
// Returns an event.CostSum rather than an int64 because two saturated entries wrapped it, and a
// wrap here does not stay here: the caller subtracts this from a bucket total, so a
// negative sum inflates the residual past the total it is a residual of. See event.CostSum.
func seriesCost(series map[string]Counts) event.CostSum {
	var total event.CostSum
	for _, v := range series {
		total.Add(v.CostMicros)
	}
	return total
}

// series copies the requested label map. Copied, not aliased: the caller holds
// only a read lock, and handing out the live map would let a JSON encoder read
// it while a later Record mutates it.
func (b *bucket) series(g Group) map[string]Counts {
	var src map[string]Counts
	switch g {
	// Both spellings read the same map. GroupMethod is the name this series
	// shipped under; see its godoc.
	case GroupModel, GroupMethod:
		src = b.byMethod
	case GroupEndpoint:
		src = b.byEndpoint
	case GroupSession:
		src = b.bySession
	case GroupAgent:
		src = b.byAgent
	case GroupProject:
		src = b.byProject
	case GroupCurrency:
		src = b.byCurrency
	case GroupStatus:
		src = b.byStatus
	case GroupPlugin:
		src = b.byPlugin
	case GroupHost:
		src = b.byHost
	default:
		return nil
	}
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]Counts, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// unionUnits folds one bucket's per-agent units into dst, allocating on first use.
func unionUnits(dst, src map[string]map[string]struct{}) map[string]map[string]struct{} {
	for agent, us := range src {
		if dst == nil {
			dst = make(map[string]map[string]struct{}, len(src))
		}
		if dst[agent] == nil {
			dst[agent] = make(map[string]struct{}, len(us))
		}
		for u := range us {
			dst[agent][u] = struct{}{}
		}
	}
	return dst
}

// sortedUnits is a unit set as Snapshot.Currencies spells it: sorted, nil when empty, and without
// overflowLabel, which is a cap marker and not a billing unit (see ledger.CurrenciesIn).
func sortedUnits(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for u := range set {
		if u != overflowLabel {
			out = append(out, u)
		}
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// seriesUnits is Snapshot.SeriesCurrencies for the series the response actually carries.
//
// A LABEL THE CAP FOLDED AWAY lends its units to overflowLabel, the series its figures went
// into, rather than keeping an entry of its own: SeriesCurrencies describes the series on the
// wire, and a key naming a series that is not there would label nothing.
func seriesUnits(labelUnits map[string]map[string]struct{}, buckets []Bucket) map[string][]string {
	if len(labelUnits) == 0 {
		return nil
	}
	present := map[string]bool{}
	for _, b := range buckets {
		for label := range b.Series {
			present[label] = true
		}
	}
	merged := map[string]map[string]struct{}{}
	for label, us := range labelUnits {
		key := label
		if !present[label] {
			key = overflowLabel
		}
		if merged[key] == nil {
			merged[key] = map[string]struct{}{}
		}
		for u := range us {
			merged[key][u] = struct{}{}
		}
	}
	out := make(map[string][]string, len(merged))
	for label, us := range merged {
		if !present[label] {
			continue
		}
		if list := sortedUnits(us); list != nil {
			out[label] = list
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
