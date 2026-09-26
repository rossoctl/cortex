package pipeline

import "context"

// Plugin is the interface that all pipeline extensions implement.
type Plugin interface {
	Name() string
	Capabilities() PluginCapabilities
	OnRequest(ctx context.Context, pctx *Context) Action
	OnResponse(ctx context.Context, pctx *Context) Action
}

// PluginCapabilities declares whether a plugin accesses the request /
// response body and which other plugins it depends on.
//
// Body-access fields drive the listener's body-buffering handshake
// (ext_proc ProcessingMode, net/http read-body). Dependency fields are
// checked at startup by plugins.Build so misconfigured chains fail before
// traffic arrives.
type PluginCapabilities struct {
	// ReadsBody: the plugin reads pctx.Body in OnRequest and/or
	// pctx.ResponseBody in OnResponse. The listener buffers the body
	// when any plugin declares this; without it, pctx.Body is nil and
	// a read silently sees "no body."
	ReadsBody bool

	// WritesRequestBody: the plugin may mutate pctx.Body (call
	// pctx.SetBody). Implies ReadsBody — Normalize() auto-promotes.
	// Listener propagates the mutation to the wire (ext_proc
	// BodyMutation, or the outbound http.Request for proxy listeners).
	//
	// Pipeline.New rejects a pipeline that has more than one
	// WritesRequestBody plugin per direction — mutation ordering would
	// be ambiguous.
	//
	// Declaring this does NOT cost response streaming. Requests are
	// never streamed — they arrive complete with a Content-Length and
	// are read end to end before dispatch — so rewriting one says
	// nothing about whether the response may be relayed incrementally.
	WritesRequestBody bool

	// WritesResponseBody: the plugin may mutate pctx.ResponseBody (call
	// pctx.SetResponseBody). Implies ReadsBody — Normalize() auto-promotes.
	//
	// This is the streaming predicate. A plugin that rewrites a response
	// needs the whole response to rewrite it, so listeners fall back from
	// incremental SSE relay to the buffered path when — and only when —
	// some plugin in the chain declares this. See
	// Pipeline.WritesResponseBody.
	//
	// Pipeline.New rejects more than one WritesResponseBody plugin per
	// direction, for the same ordering reason as the request side.
	WritesResponseBody bool

	// Requires names plugins that MUST be present in the same chain
	// AND appear earlier (lower index). Matches are case-sensitive
	// plugin Name() strings. A missing or misordered name causes
	// plugins.Build to fail at startup.
	//
	// Use Requires when the plugin hardcodes access to a specific
	// other plugin's extension fields — e.g., a tool-allowlist plugin
	// that reads pctx.Extensions.MCP.Params["name"] declares
	// Requires: []string{"mcp-parser"}.
	Requires []string

	// RequiresAny names plugins of which AT LEAST ONE must be present
	// in the same chain, and each named plugin that IS present must
	// appear earlier. Missing-all-of-them or misordered-any-of-them
	// causes plugins.Build to fail at startup.
	//
	// Use RequiresAny for protocol-agnostic plugins that read through
	// pctx.ContentSources(). Example: a guardrail that works against any
	// parser declares RequiresAny: []string{"a2a-parser", "mcp-parser",
	// "inference-parser"} so a chain with no parsers fails loud instead
	// of running the guardrail as silent dead code.
	RequiresAny []string

	// RequiresLater names plugins that MUST be present in the same chain and
	// positioned AFTER this plugin — the mirror of Requires.
	//
	// This exists because the request and response passes run in opposite
	// directions: RunResponse and RunResponseFrame walk the chain in reverse, so a
	// plugin that needs another's RESPONSE-phase output needs that plugin at a
	// HIGHER index, not a lower one. Declaring Requires there would enforce exactly
	// the wrong order.
	//
	// litellm-budget-track is the case: it prices the token counts inference-parser
	// publishes while folding response frames, so the parser has to fold each frame
	// BEFORE budget-track settles the cost — which means later in the chain.
	//
	// Without this, a plausible ordering silently unprices every streamed response:
	// the counts simply are not there yet when the terminal frame arrives, and
	// nothing reports that they were missed.
	RequiresLater []string

	// Description is operator-facing prose, one line, ≤80 chars,
	// describing what this plugin does. Surfaces in `abctl`'s
	// plugin-detail and catalog panes, and in /v1/plugins.
	//
	// Capabilities are static type-level metadata: Capabilities() must
	// return the same value for any instance produced by a given
	// factory. If a plugin's behavior varies enough that its capabilities
	// differ, register it under multiple names.
	Description string
}

// Normalize applies WritesRequestBody-implies-ReadsBody promotion.
// Called by Pipeline.New for every plugin's declared capabilities so the
// rest of the framework reads a normalized form. Plugins never need to
// call this themselves.
func (c PluginCapabilities) Normalize() PluginCapabilities {
	if c.WritesRequestBody || c.WritesResponseBody {
		c.ReadsBody = true
	}
	return c
}

// Initializer is an optional interface a plugin may implement when it
// needs to run work once before the pipeline starts serving traffic.
// Typical uses: load a model, warm a cache, open a database connection,
// register Prometheus metrics, spawn a background goroutine. Init is
// called by Pipeline.Start exactly once, in plugin declaration order.
// If any plugin's Init returns an error the pipeline fails fast —
// Pipeline.Start returns the error without calling Init on later
// plugins (nothing to unwind: earlier plugins succeeded).
//
// Plugins that don't need initialization simply don't implement this
// interface; the pipeline skips them. Keeping it optional preserves
// backward compatibility with every existing plugin.
type Initializer interface {
	Init(ctx context.Context) error
}

// Shutdowner is an optional interface a plugin may implement when it
// needs to release resources on graceful shutdown. Typical uses: flush
// in-flight audit events, close a DB connection, cancel a background
// goroutine it spawned in Init. Shutdown is called by Pipeline.Stop
// exactly once, in reverse declaration order (LIFO — symmetric with
// OnResponse dispatch) so a plugin that depends on an earlier plugin's
// resources can still use them while shutting down.
//
// Shutdown is best-effort: errors are logged but do not prevent other
// plugins from shutting down. The caller-supplied ctx carries a
// shutdown deadline; plugins must respect it and return rather than
// block indefinitely.
type Shutdowner interface {
	Shutdown(ctx context.Context) error
}

// Finisher is an optional interface a plugin may implement when it
// reserves per-request state in OnRequest and needs a guaranteed
// release point — regardless of whether the request was allowed,
// denied by a later plugin, or errored at the upstream. The canonical
// shape is acquire-in-OnRequest / release-in-OnFinish:
//
//	func (p *RateLimiter) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
//	    tenant := pctx.Identity.ClientID()
//	    p.slots.Reserve(tenant)
//	    pipeline.SetState(pctx, "rl", &rlState{tenant: tenant})
//	    return pipeline.Action{Type: pipeline.Continue}
//	}
//
//	func (p *RateLimiter) OnFinish(ctx context.Context, pctx *pipeline.Context) {
//	    s, ok := pipeline.GetState[*rlState](pctx, "rl")
//	    if !ok { return }
//	    p.slots.Release(s.tenant)
//	}
//
// OnFinish runs once per request, after OnResponse has completed (if
// it ran), on every plugin whose OnRequest was dispatched — including
// the plugin that denied, if any. The dispatcher walks in LIFO order,
// symmetric with Shutdowner and OnResponse, so a plugin's cleanup can
// still rely on resources set up by earlier plugins.
//
// The ctx passed to OnFinish is a FRESH context with a framework-set
// deadline (default 2s). It is NOT derived from the original request
// ctx, so a client disconnect during the request does not cancel
// OnFinish's I/O. Plugins that perform network work (flushing audits,
// releasing distributed leases) see a usable ctx by default.
//
// pctx carries the full request + response state observed by
// OnResponse, plus pctx.Outcome() which returns a non-nil *Outcome
// describing the request's terminal outcome (allow / deny / error,
// status code, denying plugin, duration). pctx.Outcome() returns nil
// during OnRequest and OnResponse — the field is populated by the
// framework only before OnFinish dispatches.
//
// OnFinish runs best-effort: panics are recovered and logged, errors
// in one plugin's OnFinish do not prevent later plugins in the LIFO
// chain from running. OnFinish must not call pctx.SetBody /
// SetResponseBody — the response is already on the wire; mutations
// are dropped with a WARN log.
//
// OnFinish emits no automatic Invocation records. Plugins that want
// observability on cleanup publish through their own sink
// (Prometheus, external audit service) or via the pctx.Extensions.Custom
// escape-hatch map documented in plugin-reference.md.
type Finisher interface {
	OnFinish(ctx context.Context, pctx *Context)
}

// StreamingResponder is an optional interface a plugin may implement
// when its response handling is naturally per-message rather than over
// a fully-buffered body. Listeners that detect a streaming response
// (today: text/event-stream) deliver each complete protocol message
// to the pipeline as a frame; plugins that opted in see one
// OnResponseFrame call per frame and a final empty-frame call with
// last=true so aggregating plugins (inference-parser, a2a-parser)
// can finalize their running state.
//
// Plugins without streaming awareness are unaffected: the listener
// either falls back to the buffered path (Content-Type: application/json
// or any non-streaming response) and runs OnResponse as today, or — for
// streaming responses — skips OnResponse entirely. Aggregating plugins
// that want to support both shapes implement OnResponseFrame and treat
// the buffered application/json case as a single last=true frame; the
// listener delivers it that way for them so one code path covers both
// shapes.
//
// Contract:
//   - Per-frame ordering matches the wire: frames are dispatched to
//     plugins in the order the listener parsed them off the upstream
//     response, in pipeline reverse order (symmetric with RunResponse).
//   - The frame slice is owned by the listener and is valid only for
//     the duration of the call. Plugins that need to retain bytes
//     must copy.
//   - A non-Continue Action returned mid-stream stops further
//     dispatch for that frame and rejects the response. Listener
//     ordering varies: forwardproxy invokes OnResponseFrame before
//     emitting frame bytes, while reverseproxy emits frame bytes
//     first (FlushInterval=-1 ferries each Read straight to the
//     client) and then dispatches. Either way previously-emitted
//     frames cannot be un-sent, so a mid-stream Reject results in
//     a truncated stream rather than a 4xx/5xx response. Today no
//     in-tree plugin returns Reject from this hook; the contract
//     leaves the door open for per-message enforcement to be added
//     later (a listener doing enforcement would inspect-before-forward
//     at that point).
//   - last=true is always called exactly once at end-of-stream, even
//     for an empty/zero-frame stream. Plugins finalize on last=true.
//   - For application/json responses the listener calls
//     OnResponseFrame once with the full body and last=true.
//     pipeline.RunResponse skips plugins implementing this interface
//     so OnResponse is not called for them — the framework picks one
//     path so a single response body is never delivered through both
//     hooks.
type StreamingResponder interface {
	OnResponseFrame(ctx context.Context, pctx *Context, frame []byte, last bool) Action
}

// Readier is an optional interface a plugin may implement when it has
// deferred initialization that matters to a /readyz probe. The host
// ANDs Ready() across all implementers to decide whether the pipeline
// is ready to serve traffic. A plugin whose Configure succeeded but
// whose Init is still waiting (e.g. for a credential file to be
// mounted by the operator from a Secret) returns false — the kubelet
// keeps traffic off the pod until Init completes.
//
// Plugins without deferred state don't implement this interface and
// are treated as always-ready. Pipeline.Ready() returns true when
// every Readier-implementing plugin returns true.
//
// Ready is expected to be cheap (pointer read / atomic load). The
// /readyz handler calls it on every probe (~10s cadence from kubelet).
type Readier interface {
	Ready() bool
}
