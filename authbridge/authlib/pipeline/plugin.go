package pipeline

import "context"

// Plugin is the interface that all pipeline extensions implement.
type Plugin interface {
	Name() string
	Capabilities() PluginCapabilities
	OnRequest(ctx context.Context, pctx *Context) Action
	OnResponse(ctx context.Context, pctx *Context) Action
}

// PluginCapabilities declares what extension slots a plugin reads and
// writes, plus whether it accesses the request / response body.
//
// The pipeline validates at startup that all Reads are satisfied by an
// earlier plugin's Writes. Body-access fields drive the listener's
// body-buffering handshake (ext_proc ProcessingMode, net/http read-body).
type PluginCapabilities struct {
	// Reads / Writes name extension slots (A2A, MCP, Inference, Custom
	// map keys). Checked at pipeline.New.
	Reads  []string
	Writes []string

	// ReadsBody: the plugin reads pctx.Body in OnRequest and/or
	// pctx.ResponseBody in OnResponse. The listener buffers the body
	// when any plugin declares this; without it, pctx.Body is nil and
	// a read silently sees "no body."
	ReadsBody bool

	// WritesBody: the plugin may mutate pctx.Body / pctx.ResponseBody
	// (call pctx.SetBody / pctx.SetResponseBody). Implies ReadsBody —
	// Normalize() auto-promotes. Listener propagates the mutation to
	// the wire (ext_proc BodyMutation, or the outbound http.Request /
	// downstream http.Response for proxy listeners).
	//
	// Pipeline.New rejects a pipeline that has more than one WritesBody
	// plugin per direction — mutation ordering would be ambiguous.
	// Waypoint mode (ext_authz) cannot support WritesBody at all:
	// ext_authz has no body-mutation field. main.go enforces this at
	// process boot.
	WritesBody bool

	// BodyAccess is a deprecated alias for ReadsBody, kept so existing
	// plugins compile unchanged through one release. Normalize() folds
	// BodyAccess into ReadsBody before validation and listener
	// negotiation read the normalized fields.
	//
	// Deprecated: use ReadsBody. Will be removed in a future release.
	BodyAccess bool

	// Requires names plugins that MUST be present in the same chain
	// AND appear earlier (lower index). Matches are case-sensitive
	// plugin Name() strings. A missing or misordered name causes
	// plugins.Build to fail at startup.
	//
	// Use Requires when the plugin hardcodes access to a specific
	// other plugin's extension fields — e.g., a tool-allowlist plugin
	// that reads pctx.Extensions.MCP.Params["name"] declares
	// Requires: []string{"mcp-parser"}. If the plugin instead reads
	// through pctx.ContentSources() and works against any parser,
	// see RequiresAny.
	Requires []string

	// RequiresAny names plugins of which AT LEAST ONE must be present
	// in the same chain, and each named plugin that IS present must
	// appear earlier. Missing-all-of-them or misordered-any-of-them
	// causes plugins.Build to fail at startup.
	//
	// Use RequiresAny for protocol-agnostic plugins that read through
	// pctx.ContentSources(). Example: a PII scrubber that consumes
	// fragments from whatever parsers are wired in declares
	// RequiresAny: []string{"a2a-parser", "mcp-parser", "inference-parser"}.
	// That way a chain with no parsers fails loud instead of running
	// the guardrail as silent dead code.
	RequiresAny []string

	// After names plugins that, IF present in the same chain, must
	// appear earlier. Unlike Requires/RequiresAny, a missing name is
	// not an error — After is a soft ordering hint. Useful for
	// plugins that benefit from earlier state being populated but
	// degrade gracefully without it.
	After []string

	// Claims declares semantic resources the plugin takes exclusive
	// ownership of. Within a single chain, at most one plugin may
	// declare any given claim string; two plugins with an overlapping
	// claim cause plugins.Build to fail at startup.
	//
	// Claim strings are arbitrary but authors should prefer the
	// constants in authlib/contracts/ (e.g. contracts.ClaimAuthorizationHeader)
	// so typos are compile errors and the canonical set is greppable.
	// Third-party plugins may declare their own strings; the framework
	// enforces uniqueness of whatever it sees, not "must be from the
	// list." See authlib/contracts/claims.go for the canonical
	// vocabulary.
	Claims []string
}

// Normalize applies compatibility rules to a PluginCapabilities:
//   - BodyAccess (deprecated) is folded into ReadsBody.
//   - WritesBody implies ReadsBody (you can't mutate what you didn't see).
//
// Called by Pipeline.New for every plugin's declared capabilities so the
// rest of the framework reads a normalized form. Plugins never need to
// call this themselves.
func (c PluginCapabilities) Normalize() PluginCapabilities {
	if c.BodyAccess {
		c.ReadsBody = true
	}
	if c.WritesBody {
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

// Readier is an optional interface a plugin may implement when it has
// deferred initialization that matters to a /readyz probe. The host
// ANDs Ready() across all implementers to decide whether the pipeline
// is ready to serve traffic. A plugin whose Configure succeeded but
// whose Init is still waiting (e.g. for a credential file to be
// written by client-registration) returns false — the kubelet keeps
// traffic off the pod until Init completes.
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
