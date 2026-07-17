package pipeline

import (
	"context"
	"encoding/json"
)

// RawConfigProvider is implemented by plugins (or wrappers around them)
// that can surface the raw runtime config bytes the plugin was built
// from. The session API uses this interface in describePipeline to
// populate /v1/pipeline's Config field. Naming it explicitly (rather
// than asserting an inline interface literal at the call site) gives
// callers a greppable contract and turns any future signature drift
// into a compile-time error rather than a silently-failing
// type-assertion.
type RawConfigProvider interface {
	RawConfig() json.RawMessage
}

// configuredPlugin wraps a plugin built via Configurable.Configure with the
// raw config bytes it was constructed from, so the session API can surface
// them on /v1/pipeline. All Plugin interface methods forward to the embedded
// plugin — zero observable behavior change at the request hot path.
//
// Optional plugin interfaces (Initializer, Shutdowner, Finisher, Readier)
// are NOT promoted through the embedded Plugin interface — Go does not
// promote method-set membership through an embedded interface. The wrapper
// implements each of those four interfaces explicitly and forwards
// conditionally; see the methods below.
//
// StreamingResponder is handled differently — see configuredStreamingPlugin
// and WrapConfigured. It must NOT be forwarded unconditionally here, because
// listeners type-assert StreamingResponder to choose the streaming-vs-buffered
// response path (Pipeline.HasStreamingResponders); making every wrapped plugin
// a no-op StreamingResponder would silently flip that path selection for
// non-streaming pipelines.
//
// Side effect of unconditional forwarding: every wrapped (i.e., every
// Configurable) plugin satisfies all four optional interfaces regardless
// of what the inner plugin actually implements. Callers that distinguish
// "implements Initializer" from "doesn't" via type-assertion will treat
// every wrapped plugin as an Initializer (and Shutdowner, Finisher,
// Readier). Behavior at the dispatcher hot path is preserved by the
// no-op fallbacks; callers that need to discriminate must rely on
// inner behavior rather than type-assertions.
type configuredPlugin struct {
	Plugin
	raw json.RawMessage
}

// configuredStreamingPlugin is the StreamingResponder-preserving variant of
// configuredPlugin, returned by WrapConfigured only when the wrapped plugin
// implements StreamingResponder. The base wrapper embeds the Plugin interface,
// which does not promote OnResponseFrame — so without this a Configurable +
// StreamingResponder plugin (e.g. mcp-parser, which has Configure) would lose
// its response-frame dispatch once wrapped: RunResponseFrame type-asserts
// StreamingResponder, fails on the bare wrapper, and skips the plugin, leaving
// MCP/SSE responses unparsed.
//
// Using a distinct type (instead of an unconditional no-op OnResponseFrame on
// configuredPlugin) keeps HasStreamingResponders exact: only plugins that
// genuinely stream are reported as streaming responders.
type configuredStreamingPlugin struct {
	*configuredPlugin
}

// OnResponseFrame forwards to the wrapped plugin's StreamingResponder
// implementation. WrapConfigured constructs this wrapper only when the inner
// plugin implements StreamingResponder, so the assertion always holds.
func (c *configuredStreamingPlugin) OnResponseFrame(ctx context.Context, pctx *Context, frame []byte, last bool) Action {
	return c.Plugin.(StreamingResponder).OnResponseFrame(ctx, pctx, frame, last)
}

// WrapConfigured returns a Plugin whose dynamic type retains the raw
// config bytes the plugin was built from, so the session API can
// surface them on /v1/pipeline. Callers (plugins.Build) invoke this
// only after Configurable.Configure returns nil; non-Configurable
// plugins pass through unwrapped.
//
// The raw bytes are defensively copied before being stored, so a
// caller that holds onto its reference and mutates it (or whose
// surrounding decoder reuses the underlying buffer) cannot perturb
// what RawConfig() later returns. The cost is one allocation per
// Configurable plugin per pipeline build — negligible at startup
// and on the rare hot-reload path.
func WrapConfigured(p Plugin, raw json.RawMessage) Plugin {
	cp := append(json.RawMessage(nil), raw...)
	base := &configuredPlugin{Plugin: p, raw: cp}
	// Preserve StreamingResponder: the embedded Plugin interface does not
	// promote OnResponseFrame, so a Configurable + StreamingResponder plugin
	// (e.g. mcp-parser) would otherwise lose response-frame dispatch.
	if _, ok := p.(StreamingResponder); ok {
		return &configuredStreamingPlugin{configuredPlugin: base}
	}
	return base
}

// RawConfig returns a defensive copy of the raw config bytes the
// wrapped plugin was configured with. Returning a copy means callers
// cannot mutate what subsequent calls return.
func (c *configuredPlugin) RawConfig() json.RawMessage {
	return append(json.RawMessage(nil), c.raw...)
}

// Init forwards to the wrapped plugin if it implements Initializer; otherwise
// returns nil (no deferred initialization). Required because Initializer is
// not declared on the Plugin interface, so the embedded Plugin's Init method
// (if any) is NOT promoted to *configuredPlugin's method set — only methods
// declared on Plugin are. Without this explicit forwarding, the framework's
// Init dispatcher would silently skip wrapped plugins.
func (c *configuredPlugin) Init(ctx context.Context) error {
	if init, ok := c.Plugin.(Initializer); ok {
		return init.Init(ctx)
	}
	return nil
}

// Shutdown forwards to the wrapped plugin if it implements Shutdowner; otherwise
// returns nil (no resources to release).
func (c *configuredPlugin) Shutdown(ctx context.Context) error {
	if sh, ok := c.Plugin.(Shutdowner); ok {
		return sh.Shutdown(ctx)
	}
	return nil
}

// OnFinish forwards to the wrapped plugin if it implements Finisher; otherwise
// no-ops. The framework's OnFinish dispatcher type-asserts against Finisher,
// so wrapped plugins must implement it explicitly to participate.
func (c *configuredPlugin) OnFinish(ctx context.Context, pctx *Context) {
	if fin, ok := c.Plugin.(Finisher); ok {
		fin.OnFinish(ctx, pctx)
	}
}

// Ready forwards to the wrapped plugin if it implements Readier; otherwise
// returns true. This matches the existing semantics in Pipeline.Ready
// (pipeline.go:287-289): plugins without Readier are considered always-ready.
func (c *configuredPlugin) Ready() bool {
	if r, ok := c.Plugin.(Readier); ok {
		return r.Ready()
	}
	return true
}
