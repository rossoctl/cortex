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
//
// Run also pins the loaded pipeline on pctx: every later phase on the
// same request (RunResponse, RunResponseFrame, RunFinish, Resend)
// dispatches against THAT pipeline, not whatever the Holder holds by
// then. A reload can Store a new pipeline while the request is open,
// and pctx.dispatched indexes the pipeline that ran OnRequest — without
// the pin, a shorter new chain panics in RunFinish and an equal-length
// one runs hooks on plugins that never saw the request.
func (h *Holder) Run(ctx context.Context, pctx *Context) Action {
	p := h.p.Load()
	pctx.pinned = p
	return p.Run(ctx, pctx)
}

// ForPctx returns the pipeline a pctx-carrying phase should run on: the
// one pinned by Run when the request started, else the current one (a
// phase that runs without a preceding Run on this pctx — e.g. a
// response nobody dispatched a request for — has nothing to stay loyal
// to).
//
// Listeners must also ask this for every capability probe they make
// AFTER Run — the streaming and body-buffering decisions on the response
// path (HasStreamingResponders, WritesResponseBody, NeedsBody and
// friends). A probe on the current pipeline there picks the new
// pipeline's path while the phases still dispatch on the pinned old one:
// a StreamingResponder the reload removed never sees its frames, and a
// body reader never gets the body — the same silent skip the pin exists
// to prevent. Probes before Run (request-time buffering) have no pin to
// consult and keep reading the current pipeline.
func (h *Holder) ForPctx(pctx *Context) *Pipeline {
	if pctx.pinned != nil {
		return pctx.pinned
	}
	return h.p.Load()
}

// RunResponse is equivalent to h.Load().RunResponse(ctx, pctx). See Run.
func (h *Holder) RunResponse(ctx context.Context, pctx *Context) Action {
	return h.ForPctx(pctx).RunResponse(ctx, pctx)
}

// RunResponseFrame is equivalent to h.Load().RunResponseFrame(ctx, pctx, frame, last).
// See Pipeline.RunResponseFrame.
func (h *Holder) RunResponseFrame(ctx context.Context, pctx *Context, frame []byte, last bool) Action {
	return h.ForPctx(pctx).RunResponseFrame(ctx, pctx, frame, last)
}

// HasStreamingResponders is equivalent to h.Load().HasStreamingResponders().
// Listeners read this to decide whether to take the streaming code path.
func (h *Holder) HasStreamingResponders() bool {
	return h.p.Load().HasStreamingResponders()
}

// HasResenders is equivalent to h.Load().HasResenders(). See Resender.
func (h *Holder) HasResenders() bool { return h.p.Load().HasResenders() }

// Resend is equivalent to h.Load().Resend(ctx, pctx, status, body). See Resender.
func (h *Holder) Resend(ctx context.Context, pctx *Context, status int, body []byte) bool {
	return h.ForPctx(pctx).Resend(ctx, pctx, status, body)
}

// RunFinish is equivalent to h.Load().RunFinish(ctx, pctx, outcome).
// Listeners call this in a defer at request entry to guarantee
// Finisher dispatch on every exit path. See Pipeline.RunFinish for
// the contract (LIFO over dispatched plugins, fresh ctx with timeout,
// pctx.Outcome() populated, silent phase).
func (h *Holder) RunFinish(ctx context.Context, pctx *Context, outcome Outcome) {
	h.ForPctx(pctx).RunFinish(ctx, pctx, outcome)
}

// NeedsBody is equivalent to h.Load().NeedsBody(). Hot path on listeners
// that decide whether to buffer the request/response body.
func (h *Holder) NeedsBody() bool { return h.p.Load().NeedsBody() }

// NeedsRequestBody is equivalent to h.Load().NeedsRequestBody().
func (h *Holder) NeedsRequestBody() bool { return h.p.Load().NeedsRequestBody() }

// NeedsResponseBody is equivalent to h.Load().NeedsResponseBody().
func (h *Holder) NeedsResponseBody() bool { return h.p.Load().NeedsResponseBody() }

// WritesRequestBody is equivalent to h.Load().WritesRequestBody().
// Listeners read this when deciding whether to propagate a rewritten
// request body to the wire.
func (h *Holder) WritesRequestBody() bool { return h.p.Load().WritesRequestBody() }

// WritesResponseBody is equivalent to h.Load().WritesResponseBody().
// Listeners read this when deciding whether streaming responses are safe
// — a pipeline with a response mutator can't stream, because the proxy
// can't rewrite a body it has already started forwarding. A request-only
// mutator does not disable streaming.
func (h *Holder) WritesResponseBody() bool { return h.p.Load().WritesResponseBody() }

// Ready is equivalent to h.Load().Ready().
func (h *Holder) Ready() bool { return h.p.Load().Ready() }

// NotReadyPlugin is equivalent to h.Load().NotReadyPlugin(). Used by
// /readyz handlers to produce a helpful error body.
func (h *Holder) NotReadyPlugin() string { return h.p.Load().NotReadyPlugin() }

// Plugins is equivalent to h.Load().Plugins(). Used by the session
// events API to surface pipeline composition.
func (h *Holder) Plugins() []Plugin { return h.p.Load().Plugins() }
