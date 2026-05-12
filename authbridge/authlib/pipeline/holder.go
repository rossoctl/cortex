package pipeline

import (
	"context"
	"sync/atomic"
)

// Holder is an atomic wrapper over *Pipeline.
//
// Listeners hold a *Holder and call through it on every request; a
// reloader Store()s a newly-built pipeline under the hood, and the
// next Load returns it. atomic.Pointer guarantees a Load-then-use
// sequence sees a fully-initialized pipeline — a request that started
// before a swap finishes against the original *Pipeline, and the
// reloader is responsible for not Stop()ing the old pipeline until a
// drain window expires.
//
// A Holder has exactly one long-lived value; its identity is the slot
// listeners reference, not the pipeline at any given moment.
type Holder struct {
	p atomic.Pointer[Pipeline]
}

// NewHolder wraps p in a Holder. The Pipeline must already be
// non-nil — Holder.Load will return nil if it's unset, and the
// delegating helpers below will panic. Callers build the first
// pipeline at startup just like before and hand it to NewHolder.
func NewHolder(p *Pipeline) *Holder {
	h := &Holder{}
	h.p.Store(p)
	return h
}

// Load returns the current pipeline. Safe for concurrent use. Returns
// nil only if no pipeline was ever stored (NewHolder requires non-nil,
// so this only happens if a caller zero-valued a Holder).
func (h *Holder) Load() *Pipeline { return h.p.Load() }

// Store replaces the current pipeline. Subsequent Loads return the
// new pipeline; in-flight requests that already Loaded the old one
// continue to run against it. The caller owns the lifecycle of the
// replaced pipeline — read the return value of the previous Load and
// call its Stop(ctx) after the intended drain window.
func (h *Holder) Store(p *Pipeline) { h.p.Store(p) }

// Run is equivalent to h.Load().Run(ctx, pctx). Offered as a
// one-liner so listeners can keep their Pipeline-era call sites
// unchanged when they migrate their field type from *Pipeline to
// *Holder. One atomic Load per call.
func (h *Holder) Run(ctx context.Context, pctx *Context) Action {
	return h.p.Load().Run(ctx, pctx)
}

// RunResponse is equivalent to h.Load().RunResponse(ctx, pctx). See Run.
func (h *Holder) RunResponse(ctx context.Context, pctx *Context) Action {
	return h.p.Load().RunResponse(ctx, pctx)
}

// NeedsBody is equivalent to h.Load().NeedsBody(). Hot path on listeners
// that decide whether to buffer the request/response body.
func (h *Holder) NeedsBody() bool { return h.p.Load().NeedsBody() }

// Ready is equivalent to h.Load().Ready().
func (h *Holder) Ready() bool { return h.p.Load().Ready() }

// NotReadyPlugin is equivalent to h.Load().NotReadyPlugin(). Used by
// /readyz handlers to produce a helpful error body.
func (h *Holder) NotReadyPlugin() string { return h.p.Load().NotReadyPlugin() }

// Plugins is equivalent to h.Load().Plugins(). Used by the session
// events API to surface pipeline composition.
func (h *Holder) Plugins() []Plugin { return h.p.Load().Plugins() }
