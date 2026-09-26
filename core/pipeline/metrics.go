package pipeline

// Metric is one operator-facing counter reported by a plugin. Values are
// float64 so a plugin can report a ratio or a per-request average without a
// second type; counts are whole numbers that happen to fit exactly.
//
// Unit is advisory and drives display, not arithmetic: "count", "bytes",
// "tokens", "ratio". Note carries the caveat a number needs to be read
// honestly — most importantly the sample size behind an estimate, e.g.
// "estimate, n=1284". A derived figure with no Note is read as measured, so
// anything inferred must say so here.
type Metric struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
	Unit  string  `json:"unit,omitempty"`
	Note  string  `json:"note,omitempty"`
}

// MetricsProvider is implemented by plugins that expose counters for operator
// display. describePipeline calls it on demand while serving /v1/pipeline, so
// implementations must be safe for concurrent use with the request path and
// must not block — take a mutex, copy, release. Returning nil is fine and
// renders as "(none)".
//
// CONTRACT ON Name AND Note: these are short operator-facing labels, surfaced on
// an endpoint with no authentication. They must never carry request or response
// content — no prompts, no completions, no header or credential values, nothing
// derived from a body. A caveat naming a sample size or a configuration key is
// fine; a caveat quoting the traffic is not. The framework caps their length but
// cannot inspect their meaning, so this is a producer obligation, the same one
// body-mutation events carry when they publish only lengths and hashes.
//
// This is deliberately separate from plugins.StatsSource / auth.Stats, which
// are auth-shaped: they carry typed approval and denial enums and a custom
// MarshalJSON, so routing "bytes removed" through them would distort their
// meaning. Naming the interface here (rather than asserting an inline literal
// at the call site) gives callers a greppable contract and turns future
// signature drift into a compile error rather than a silently-failing
// type assertion — the same reasoning as RawConfigProvider.
type MetricsProvider interface {
	Metrics() []Metric
}
