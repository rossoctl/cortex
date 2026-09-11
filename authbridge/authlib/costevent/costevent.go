// Package costevent is the canonical wire shape of the per-request cost that
// litellm-budget-track publishes onto a session event.
//
// It lives in its own package because the producer is a plugin while the
// consumers are the usage aggregator and abctl, and none of those should import
// each other. Before this package existed the struct was declared once in the
// plugin, again in abctl, and was about to be declared a third time in the
// aggregator — which is the duplication cortex #910 exists to remove.
//
// Dependency-light on purpose: the aggregator links this on every build,
// including the trimmed "lite" images that exclude the plugin entirely.
package costevent

import (
	"encoding/json"
	"math"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// Key is the session-event key the cost record is published under.
//
// It names the CONCERN, not the producer. That distinction is the whole point: the key used
// to be the producing plugin's name, so moving costing to whichever component actually
// knows the tokens — cortex #972 moves it to inference-parser — would have been a breaking
// wire change for every live consumer AND for every event already written to a session
// store. Keyed on the concern, the producer can move and no consumer notices.
//
// The framework set this precedent and stated the reason. pipeline/context.go publishes
// "body-mutation"+PluginEventSuffix from the core, which is not a plugin at all, because
// "a switch of plugin names in a future refactor shouldn't break operators' dashboards".
// pipeline/snapshot.go does not require the key to be a plugin name.
//
// Exactly ONE component may write this key per request. Extensions.Custom is a plain map,
// so a second writer would win silently.
const Key = "cost"

// PluginName is the LEGACY key: the name of the plugin that used to be the sole producer.
//
// Still read by Decode and Record, because session stores hold events written under it and
// a consumer reading history must not see a gap where the rename happened. Still written by
// litellm-budget-track until #972 phase 2 moves the producer, and a test in that package
// asserts it equals BudgetTrack.Name() for as long as that remains true.
//
// Deprecated: write Key. Reads keep working.
const PluginName = "litellm-budget-track"

// Source names how the cost was arrived at.
const (
	// SourceGatewayHeader is the gateway's own post-discount figure, read from
	// x-litellm-response-cost. Authoritative.
	SourceGatewayHeader = "gateway-header"
	// SourceUsageFallback is priced from token counters because the header
	// reported 0 — which every streamed response does, so for a streaming agent
	// this is the common case, not the exception.
	SourceUsageFallback = "usage-fallback"
)

// Event is one request's settled cost. Unlike tool-prune's event, which carries
// rates and leaves the arithmetic to the consumer, this carries a finished
// figure.
type Event struct {
	CostUSD float64 `json:"cost_usd"`
	// Source is SourceGatewayHeader or SourceUsageFallback. A consumer that
	// distinguishes authoritative from modelled figures reads this.
	Source        string  `json:"source"`
	DailyTotalUSD float64 `json:"daily_total_usd"`
	DailyMaxUSD   float64 `json:"daily_max_usd"`

	// Provenance names where the figure came from: "authoritative" when the
	// gateway reported it, otherwise the rate table's own level ("configured",
	// "discovered", "bundled"). See authlib/pricing.Provenance.
	//
	// ADDITIVE, and omitempty, so a consumer written against the four original
	// fields keeps decoding unchanged and an event from an older producer decodes
	// here with an empty Provenance. Source is retained rather than replaced for
	// the same reason: it already ships, and its two values still answer a
	// different question — WHICH PATH priced this (header vs token counts) rather
	// than how much to trust the rates.
	Provenance string `json:"provenance,omitempty"`

	// Settled marks a figure the producer settled deliberately, INCLUDING zero.
	//
	// Cost of zero used to be indistinguishable from "no figure": litellm-budget-track
	// charges nothing for a gateway that reported a present 0 cost — a genuine free
	// call, a cache hit or an error — and then emitted no event, because it gates on
	// cost > 0. Decode rejected CostUSD <= 0 for the same reason. So the usage
	// aggregator saw nothing, fell through to its rate table, and FABRICATED a cost
	// for a call the gateway had explicitly declared free, counting it as priced.
	//
	// A settled zero is now a real answer that suppresses the fallback.
	Settled bool `json:"settled,omitempty"`
}

// Micros converts CostUSD to millionths of a dollar, rounded to nearest.
//
// The integer unit is what usage.Counts accumulates: it keeps bucket addition
// exact and JSON round-tripping lossless, which float dollars are not. A cost
// below half a micro rounds to 0 while still counting as priced — a real
// sub-micro charge, not an unknown one.
func (e Event) Micros() int64 { return int64(math.Round(e.CostUSD * 1e6)) }

// Priced reports whether this record carries a usable dollar figure.
//
// Named and exported because it is the predicate Decode applies, and a consumer that took
// the record through Record needs to ask the same question without reimplementing it. The
// two must agree; a test asserts they do.
//
// A settled zero is priced: the producer means "this call was free". An unsettled zero is
// not: it means nobody priced this, and rendering $0.00 for it would report unpriced
// traffic as free. A negative figure is never a price.
func (e Event) Priced() bool {
	if e.CostUSD < 0 {
		return false
	}
	return e.CostUSD > 0 || e.Settled
}

// Record pulls the cost record off a session event, whether or not it carries a price.
//
// PRESENCE, which is a different question from pricedness — and the only one Decode could
// answer. A record whose cost is an unsettled zero is real: it names the endpoint, the
// source and the provenance, and #972 gives it avoided-cost entries too. Under Decode's
// rule that record is indistinguishable from no record at all, so a consumer wanting its
// non-cost fields could not reach them, and a saving on a request that could not be priced
// — exactly the traffic where a saving is most interesting — would silently vanish.
//
// Reads the current key first and falls back to the legacy one. New wins when both are
// present: a transitional deployment can run an old producer alongside a new one, and
// taking the legacy value there would report the figure from the component being retired.
func Record(e *pipeline.SessionEvent) (Event, bool) {
	if e == nil || len(e.Plugins) == 0 {
		return Event{}, false
	}
	raw, ok := e.Plugins[Key]
	if !ok {
		if raw, ok = e.Plugins[PluginName]; !ok {
			return Event{}, false
		}
	}
	var ev Event
	if err := json.Unmarshal(raw, &ev); err != nil {
		return Event{}, false
	}
	return ev, true
}

// Decode pulls a PRICED cost off a session event.
//
// A false return is the normal case, not an error: the producing plugin may not be in the
// pipeline, and a request that charged nothing (cache hit, error) emits nothing. A
// non-positive cost also returns false — "free" and "unknown" must not be conflated, and a
// consumer that treated 0 as priced would render $0.00 for traffic it simply cannot price.
//
// Use Record instead when you want the record regardless of whether it priced anything.
func Decode(e *pipeline.SessionEvent) (Event, bool) {
	ev, ok := Record(e)
	if !ok {
		return Event{}, false
	}
	// One predicate, in one place — see Priced.
	if !ev.Priced() {
		return Event{}, false
	}
	return ev, true
}
