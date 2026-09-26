package pricing

import "sync/atomic"

// Registry is the long-lived Resolver every consumer holds.
//
// It exists because of a lifetime mismatch in the proxy. buildPipelines is a
// closure the config reloader re-invokes on every change
// (cmd/authbridge-proxy/main.go:296,300), so plugins are reconstructed; but the
// usage aggregator that shares the same rates is created once, outside that
// closure (main.go:370), and outlives every rebuild. A resolver reconstructed per
// build would leave the aggregator holding a stale table forever, and pricing
// would silently diverge between /v1/usage and the plugins.
//
// So the table is swapped in place and the Registry pointer never changes. Readers
// hold the Registry; the reloader hands it a new Table.
type Registry struct{ tab atomic.Pointer[Table] }

// NewRegistry returns a Registry serving t. A nil t is legal and prices nothing —
// an operator with no pricing config and the bundled slice disabled.
func NewRegistry(t *Table) *Registry {
	r := &Registry{}
	r.Swap(t)
	return r
}

// Swap replaces the live table.
//
// Callers hand over ownership: a Table must not be mutated after Swap, because
// every concurrent reader holds the same pointer. Atomic, so a reload never makes
// a request observe a half-built table.
func (r *Registry) Swap(t *Table) {
	if r == nil {
		return
	}
	r.tab.Store(t)
}

// Resolve implements Resolver.
//
// A nil Registry resolves to ProvNone rather than panicking: a binary built
// without pricing wiring must report its traffic as unpriced, not crash on the
// response path. Same posture as a nil Table.
func (r *Registry) Resolve(endpoint, model string, promptTotal int) (Rates, Provenance) {
	if r == nil {
		return Rates{}, ProvNone
	}
	return r.tab.Load().Resolve(endpoint, model, promptTotal)
}

// Cost resolves and prices in one call — the shape every consumer actually wants.
//
// Provenance collapses to ProvNone whenever ok is false, including when a row DID
// match but its tiers do not cover the request. Reporting "bundled" alongside a
// figure of zero would label an absent answer as a sourced one, which is the
// mislabelling this package's provenance exists to prevent.
func (r *Registry) Cost(endpoint, model string, u Usage) (micros int64, prov Provenance, ok bool) {
	rates, prov := r.Resolve(endpoint, model, u.PromptTotal())
	if prov == ProvNone {
		return 0, ProvNone, false
	}
	micros, ok = Cost(rates, u)
	if !ok {
		return 0, ProvNone, false
	}
	return micros, prov, true
}

var _ Resolver = (*Registry)(nil)
