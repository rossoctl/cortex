package toolprune

import (
	"fmt"
	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
	"sort"
	"strings"
	"sync"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// metrics holds the plugin's counters. In-memory and per-process by design:
// this targets the single-laptop case, and staying free of a storage backend is
// what keeps the plugin dependency-free. Counters reset on restart, which is
// why every derived figure is reported alongside the sample behind it.
type metrics struct {
	mu sync.Mutex

	requestsSeen uint64 // matched the path gate and carried a manifest
	// recovered counts panics the fail-open swallowed.
	//
	// Untracked, this plugin can stop working entirely and still look healthy: the
	// recover() forwards the original body, so pruning silently becomes a no-op with
	// nothing in the metrics to say so. That is not hypothetical — the pricing
	// migration introduced a nil-interface panic on the request path, and this exact
	// blind spot is why it presented as "pruning stopped" rather than as a crash.
	recovered         uint64
	requestsPruned    uint64 // body actually rewritten (enforce)
	requestsProjected uint64 // would have been rewritten (observe)

	toolsRemoved uint64
	perTool      map[string]uint64

	bytesRemoved uint64

	// Estimated tokens saved, split by the prompt tier the saving came out of.
	// Kept apart because providers price the tiers very differently: a blended
	// total cannot be multiplied by any single rate without being wrong by up
	// to ~12x on cache-heavy traffic.
	savedInput      float64
	savedCacheWrite float64
	savedCacheRead  float64
	requestsCosted  uint64

	// Dollars are accumulated at request time, not derived at snapshot time,
	// because the rate depends on which model served the request — a 5x spread
	// across opus/sonnet/haiku on one observed gateway. Multiplying a blended
	// token total by any single rate would be wrong by that factor.
	usdSaved float64

	// usdProjected is what observe mode WOULD have saved. Kept apart from usdSaved
	// because in observe mode SetBody is a no-op: those bytes went upstream and were
	// paid for, so adding them to a realized total reports money that was spent as
	// money that was not. The row is named separately for the same reason.
	usdProjected float64
	// savedProjected is the token half of the same distinction. One bucket, not three:
	// the tier split exists so a real total can be multiplied by the right rate, and a
	// hypothetical needs no such precision.
	savedProjected float64
	// savedUntiered holds tokens whose tier could not be established — a record written
	// by a build with a tier vocabulary this one does not know. Counted, but kept out of
	// the three tier buckets rather than defaulted into input, which would misattribute
	// by up to 12.5x and read as a real input saving.
	savedUntiered float64

	// Requests whose model had no configured rate. Counted and named rather
	// than charged at another model's rate, so an incomplete pricing table
	// shows up as a gap instead of silently under-reporting the total.
	unpriced       uint64
	unpricedModels map[string]uint64

	// usedBundledRates records that at least one request was priced from the
	// built-in table rather than operator config, so the readout can say so.
	// A dollar figure that silently mixes measured and assumed rates invites
	// being quoted as though it were measured.
	usedBundledRates bool
}

func (m *metrics) seen() {
	m.mu.Lock()
	m.requestsSeen++
	m.mu.Unlock()
}

func (m *metrics) pruned(names []string, bytesRemoved int) {
	m.mu.Lock()
	m.requestsPruned++
	m.record(names, bytesRemoved)
	m.mu.Unlock()
}

func (m *metrics) projected(names []string, bytesRemoved int) {
	m.mu.Lock()
	m.requestsProjected++
	m.record(names, bytesRemoved)
	m.mu.Unlock()
}

// record must be called with mu held.
func (m *metrics) record(names []string, bytesRemoved int) {
	if m.perTool == nil {
		m.perTool = make(map[string]uint64)
	}
	for _, n := range names {
		m.perTool[n]++
	}
	m.toolsRemoved += uint64(len(names))
	if bytesRemoved > 0 {
		m.bytesRemoved += uint64(bytesRemoved)
	}
}

// recoveredPanic records that the fail-open swallowed a panic.
func (m *metrics) recoveredPanic() {
	m.mu.Lock()
	m.recovered++
	m.mu.Unlock()
}

// observeSaving records one request's saving.
//
// tier is nil when it could not be established; projected marks a saving that was measured
// but never applied. Both are separate arguments rather than inferred here, because the
// caller reads them off the cost record and this layer must not guess: a fabricated tier and
// a hypothetical counted as real are the two ways this readout can lie about money.
func (m *metrics) observeSaving(tokens float64, tier *pricing.Tier, usd float64, prov pricing.Provenance, model string, projected bool) {
	m.mu.Lock()
	switch {
	case projected:
		// Not attributed to a tier: it was not saved, so there is no billed tier it
		// came out of.
		m.savedProjected += tokens
	case tier == nil:
		m.savedUntiered += tokens
	case *tier == pricing.TierCacheWrite:
		m.savedCacheWrite += tokens
	case *tier == pricing.TierCacheRead:
		m.savedCacheRead += tokens
	default:
		m.savedInput += tokens
	}
	m.requestsCosted++
	if prov != pricing.ProvNone {
		if projected {
			m.usdProjected += usd
		} else {
			m.usdSaved += usd
		}
		if prov == pricing.ProvBundled {
			m.usedBundledRates = true
		}
	} else {
		m.unpriced++
		if m.unpricedModels == nil {
			m.unpricedModels = make(map[string]uint64)
		}
		if model == "" {
			model = "(unknown)"
		}
		m.unpricedModels[model]++
	}
	m.mu.Unlock()
}

// snapshot renders the counters as operator-facing metrics. Every derived row
// carries the sample it was computed from, so a figure can never be read as
// more certain than it is.
func (m *metrics) snapshot() []pipeline.Metric {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Built first, and BEFORE the nothing-happened early return below. A panic on
	// the first request leaves requestsSeen at zero, so returning nil there would
	// hide the row in exactly the case it exists for: a plugin that recovered
	// immediately and has silently done nothing since.
	var recoveredRow []pipeline.Metric
	if m.recovered > 0 {
		recoveredRow = []pipeline.Metric{{
			Name:  "panics recovered",
			Value: float64(m.recovered),
			Unit:  "count",
			Note:  "pruning was SKIPPED for these requests; the original body was forwarded",
		}}
	}
	if m.requestsSeen == 0 && m.requestsPruned == 0 && m.requestsProjected == 0 {
		return recoveredRow
	}

	out := append([]pipeline.Metric(nil), recoveredRow...)
	out = append(out,
		pipeline.Metric{Name: "requests seen", Value: float64(m.requestsSeen), Unit: "count"},
	)
	// Enforce and observe are mutually exclusive in practice (one policy per
	// plugin instance), but report whichever has fired so a mid-flight policy
	// change is visible rather than silently blended.
	if m.requestsPruned > 0 || m.requestsProjected == 0 {
		out = append(out, pipeline.Metric{
			Name: "requests pruned", Value: float64(m.requestsPruned), Unit: "count",
		})
	}
	if m.requestsProjected > 0 {
		out = append(out, pipeline.Metric{
			Name:  "requests projected",
			Value: float64(m.requestsProjected),
			Unit:  "count",
			Note:  "observe mode — body unchanged",
		})
	}
	out = append(out,
		pipeline.Metric{Name: "tools removed", Value: float64(m.toolsRemoved), Unit: "count"},
		pipeline.Metric{Name: "bytes removed", Value: float64(m.bytesRemoved), Unit: "bytes"},
	)

	acted := m.requestsPruned + m.requestsProjected
	if acted > 0 {
		out = append(out, pipeline.Metric{
			Name: "bytes removed / request", Value: float64(m.bytesRemoved) / float64(acted), Unit: "bytes",
		})
	}

	// Tokens saved, per prompt tier. Deliberately not summed: the tiers are
	// priced differently enough (Anthropic: cache write 1.25x input, cache read
	// 0.1x) that one total invites a multiplication that is wrong by >12x.
	note := ""
	if m.requestsCosted > 0 {
		note = fmt.Sprintf("estimate, n=%d", m.requestsCosted)
	}
	for _, t := range []struct {
		name string
		val  float64
	}{
		{"tokens saved: cache write", m.savedCacheWrite},
		{"tokens saved: cache read", m.savedCacheRead},
		{"tokens saved: input", m.savedInput},
	} {
		if t.val > 0 {
			out = append(out, pipeline.Metric{Name: t.name, Value: t.val, Unit: "tokens", Note: note})
		}
	}
	if m.savedProjected > 0 {
		out = append(out, pipeline.Metric{
			Name: "tokens would save", Value: m.savedProjected, Unit: "tokens",
			Note: "observe mode — measured but NOT applied",
		})
	}
	if m.usdSaved == 0 && m.usdProjected > 0 {
		// A deployment running purely in observe mode has no realized row above to
		// hang this off, and silence would read as "nothing to save here".
		out = append(out, pipeline.Metric{
			Name: "$ would save", Value: m.usdProjected, Unit: "usd",
			Note: "observe mode — measured but NOT applied; this money was spent",
		})
	}
	if m.requestsCosted == 0 && acted > 0 {
		out = append(out, pipeline.Metric{
			Name: "tokens saved", Value: 0, Unit: "tokens",
			Note: "no response usage seen yet",
		})
	}

	// Dollars, accumulated per request at that request's model rate.
	if m.usdSaved > 0 {
		costNote := note
		if m.usedBundledRates {
			// Provenance travels with the number. The bundled table is VENDOR
			// LIST, generated from LiteLLM's public map, so the caveat runs the
			// opposite way from the hand-measured gateway defaults it replaced: a
			// deployment on a discounted gateway is now OVERSTATED, not understated.
			// Name that direction, because "built-in rates" alone reads as a
			// rounding caveat rather than a systematic bias with a known sign.
			costNote = "bundled vendor-list rates (a discounted gateway is overstated) — pin it under pricing.endpoints"
			if note != "" {
				costNote = note + "; " + costNote
			}
		}
		// GROSS, not net. Changing the remove list changes the cached prefix, so
		// the next request re-writes the whole prefix at the cache-write rate
		// (~1.25x input) while the recurring saving is at the cache-read rate
		// (~0.1x) on a small delta — tens of requests to break even after each
		// change. Counters also reset on the reload that applies the change, so
		// the re-warm is invisible exactly when it is paid. Say so on the row
		// rather than presenting a gross figure as a net one.
		grossNote := costNote
		if grossNote != "" {
			grossNote += "; "
		}
		grossNote += "gross — excludes cache re-warm after a remove-list change"
		out = append(out, pipeline.Metric{Name: "$ saved", Value: m.usdSaved, Unit: "usd", Note: grossNote})
		if m.usdProjected > 0 {
			out = append(out, pipeline.Metric{
				Name: "$ would save", Value: m.usdProjected, Unit: "usd",
				Note: "observe mode — measured but NOT applied; this money was spent",
			})
		}
		if m.savedUntiered > 0 {
			out = append(out, pipeline.Metric{
				Name: "tokens saved (tier unknown)", Value: m.savedUntiered, Unit: "tokens",
				Note: "a cost record named a prompt tier this build does not know",
			})
		}
		if priced := m.requestsCosted - m.unpriced; priced > 0 {
			out = append(out, pipeline.Metric{
				Name:  "$ saved / request",
				Value: m.usdSaved / float64(priced),
				Unit:  "usd",
				Note:  grossNote,
			})
		}
	}

	// An incomplete pricing table is a gap in the dollar total, so name it.
	if m.unpriced > 0 {
		models := make([]string, 0, len(m.unpricedModels))
		for k := range m.unpricedModels {
			models = append(models, k)
		}
		sort.Strings(models)
		out = append(out, pipeline.Metric{
			Name:  "requests unpriced",
			Value: float64(m.unpriced),
			Unit:  "count",
			Note:  "no rate for: " + strings.Join(models, ", "),
		})
	}

	// Per-tool attribution, sorted by count then name so the readout is
	// stable across calls and the biggest contributors come first.
	type kv struct {
		name string
		n    uint64
	}
	tools := make([]kv, 0, len(m.perTool))
	for k, v := range m.perTool {
		tools = append(tools, kv{k, v})
	}
	sort.Slice(tools, func(i, j int) bool {
		if tools[i].n != tools[j].n {
			return tools[i].n > tools[j].n
		}
		return tools[i].name < tools[j].name
	})
	for _, t := range tools {
		out = append(out, pipeline.Metric{
			Name: "removed: " + t.name, Value: float64(t.n), Unit: "count",
		})
	}
	return out
}
