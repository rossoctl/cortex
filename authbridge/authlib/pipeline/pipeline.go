package pipeline

import (
	"context"
	"fmt"
	"log/slog"
)

// Pipeline holds an ordered list of plugins and runs them sequentially.
type Pipeline struct {
	plugins []Plugin
}

// defaultSlots lists the built-in extension slot names.
var defaultSlots = map[string]bool{
	"mcp":        true,
	"a2a":        true,
	"security":   true,
	"delegation": true,
	"inference":  true,
	"custom":     true,
}

// Option configures pipeline construction.
type Option func(*options)

type options struct {
	extraSlots []string
}

// WithSlots registers additional valid extension slot names beyond the built-in set.
// Use this when a bridge plugin (e.g., CPEX) produces extensions not in the default set.
func WithSlots(slots ...string) Option {
	return func(o *options) {
		o.extraSlots = append(o.extraSlots, slots...)
	}
}

// New creates a Pipeline from the given plugins after validating capability wiring.
// Returns an error if any plugin declares a read on a slot that no earlier plugin writes.
func New(plugins []Plugin, opts ...Option) (*Pipeline, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	validSlots := make(map[string]bool, len(defaultSlots)+len(o.extraSlots))
	for k, v := range defaultSlots {
		validSlots[k] = v
	}
	for _, s := range o.extraSlots {
		validSlots[s] = true
	}
	if err := validateCapabilities(plugins, validSlots); err != nil {
		return nil, err
	}
	return &Pipeline{plugins: plugins}, nil
}

// Run executes the request phase of the pipeline sequentially.
// If any plugin returns Reject, the pipeline stops and returns that action
// with Violation.PluginName populated.
//
// Before dispatching into each plugin, Run stamps pctx with the plugin's
// name and the current phase so the plugin's Record / Allow / Skip /
// Observe / Modify / DenyAndRecord helpers can fill Invocation.Plugin
// and Invocation.Phase automatically. The stamp is cleared after each
// plugin returns so a plugin that spawns a goroutine capturing pctx
// won't mis-attribute a late-arriving Record to itself.
func (p *Pipeline) Run(ctx context.Context, pctx *Context) Action {
	for _, plugin := range p.plugins {
		if ctx.Err() != nil {
			slog.Info("pipeline: request cancelled", "plugin", plugin.Name())
			return Deny("pipeline.cancelled", "request cancelled")
		}
		pctx.SetCurrentPlugin(plugin.Name(), InvocationPhaseRequest)
		action := plugin.OnRequest(ctx, pctx)
		pctx.ClearCurrentPlugin()
		if action.Type == Reject {
			stampPluginName(&action, plugin.Name())
			logReject(plugin.Name(), action, "pipeline: plugin rejected request")
			return action
		}
		slog.Debug("pipeline: plugin completed", "plugin", plugin.Name())
	}
	return Action{Type: Continue}
}

// RunResponse executes the response phase in reverse order.
// The last plugin in the chain sees the response first.
//
// See Run for the pctx attribution stamping. Same pattern, phase set
// to InvocationPhaseResponse.
func (p *Pipeline) RunResponse(ctx context.Context, pctx *Context) Action {
	for i := len(p.plugins) - 1; i >= 0; i-- {
		if ctx.Err() != nil {
			slog.Info("pipeline: response cancelled", "plugin", p.plugins[i].Name())
			return Deny("pipeline.cancelled", "request cancelled")
		}
		pctx.SetCurrentPlugin(p.plugins[i].Name(), InvocationPhaseResponse)
		action := p.plugins[i].OnResponse(ctx, pctx)
		pctx.ClearCurrentPlugin()
		if action.Type == Reject {
			stampPluginName(&action, p.plugins[i].Name())
			logReject(p.plugins[i].Name(), action, "pipeline: plugin rejected response")
			return action
		}
	}
	return Action{Type: Continue}
}

// stampPluginName annotates a reject action with the plugin that produced
// it, so listeners and clients can attribute the denial without the
// plugin remembering to set it.
func stampPluginName(action *Action, name string) {
	if action.Violation == nil {
		action.Violation = &Violation{Code: "plugin.unspecified", Reason: "plugin rejected without violation"}
	}
	if action.Violation.PluginName == "" {
		action.Violation.PluginName = name
	}
}

// logReject emits a structured log for a rejected request/response, with
// the violation's code and reason. Keeps the two identical log statements
// in Run/RunResponse in one place.
func logReject(pluginName string, action Action, msg string) {
	status, _, _ := action.Violation.Render()
	slog.Info(msg,
		"plugin", pluginName,
		"status", status,
		"code", action.Violation.Code,
		"reason", action.Violation.Reason)
}

// Plugins returns a copy of the pipeline's plugin list in execution order.
// The copy prevents callers from mutating the backing slice; individual
// Plugin values are interface types and can be inspected freely.
//
// Used by the session events API to expose pipeline composition to
// off-process tools (abctl) and other observability surfaces.
func (p *Pipeline) Plugins() []Plugin {
	out := make([]Plugin, len(p.plugins))
	copy(out, p.plugins)
	return out
}

// Ready reports whether every plugin implementing pipeline.Readier
// currently reports ready. Plugins without Readier are considered
// always-ready (no deferred state). Called per /readyz probe, so the
// implementation is one cheap type-assert + bool read per plugin.
func (p *Pipeline) Ready() bool {
	for _, plugin := range p.plugins {
		r, ok := plugin.(Readier)
		if !ok {
			continue
		}
		if !r.Ready() {
			return false
		}
	}
	return true
}

// NotReadyPlugin returns the first plugin whose Ready() returned
// false, or "" when the pipeline is ready. Used by /readyz to
// produce a helpful error body.
func (p *Pipeline) NotReadyPlugin() string {
	for _, plugin := range p.plugins {
		r, ok := plugin.(Readier)
		if !ok {
			continue
		}
		if !r.Ready() {
			return plugin.Name()
		}
	}
	return ""
}

// NeedsBody returns true if any plugin in the pipeline declares BodyAccess.
func (p *Pipeline) NeedsBody() bool {
	for _, plugin := range p.plugins {
		if plugin.Capabilities().BodyAccess {
			return true
		}
	}
	return false
}

// Start invokes Init on every plugin that implements the Initializer
// interface, in declaration order. Returns the first error encountered;
// on error, later plugins are not initialized. Plugins without Init are
// silently skipped.
//
// If Init fails on plugin N, Start invokes Shutdown on plugins
// [0..N-1] (those whose Init succeeded) in reverse order before
// returning the error. This cleans up any background goroutines the
// earlier plugins spawned, so the plugin chain doesn't leak when a
// downstream peer rejects its config at boot. Shutdown errors during
// unwind are logged but do not mask the original Init failure.
//
// Callers should invoke Start after Pipeline construction (pipeline.New)
// and before the listener accepts traffic. Safe to call at most once per
// Pipeline — plugins may assume Init runs exactly once per process.
func (p *Pipeline) Start(ctx context.Context) error {
	for i, plugin := range p.plugins {
		init, ok := plugin.(Initializer)
		if !ok {
			continue
		}
		slog.Debug("pipeline: initializing plugin", "plugin", plugin.Name())
		if err := init.Init(ctx); err != nil {
			p.unwindStart(ctx, i)
			return fmt.Errorf("plugin %q Init: %w", plugin.Name(), err)
		}
	}
	return nil
}

// unwindStart invokes Shutdown on plugins [0..failedIdx-1] in reverse
// order after a Start failure at index failedIdx. Best-effort — errors
// are logged.
func (p *Pipeline) unwindStart(ctx context.Context, failedIdx int) {
	for i := failedIdx - 1; i >= 0; i-- {
		sh, ok := p.plugins[i].(Shutdowner)
		if !ok {
			continue
		}
		slog.Debug("pipeline: unwinding plugin after Start failure",
			"plugin", p.plugins[i].Name())
		if err := sh.Shutdown(ctx); err != nil {
			slog.Warn("pipeline: plugin Shutdown during Start unwind returned error",
				"plugin", p.plugins[i].Name(), "error", err)
		}
	}
}

// Stop invokes Shutdown on every plugin that implements the Shutdowner
// interface, in reverse declaration order (LIFO). Errors are logged but
// do not stop the sequence — every Shutdowner is given a chance to flush.
// The caller-supplied ctx carries the shutdown deadline; plugins are
// expected to respect it.
//
// Callers should invoke Stop after the listener has drained / stopped
// accepting new requests so in-flight work is allowed to complete first.
// Safe to call at most once per Pipeline.
func (p *Pipeline) Stop(ctx context.Context) {
	for i := len(p.plugins) - 1; i >= 0; i-- {
		sh, ok := p.plugins[i].(Shutdowner)
		if !ok {
			continue
		}
		slog.Debug("pipeline: shutting down plugin", "plugin", p.plugins[i].Name())
		if err := sh.Shutdown(ctx); err != nil {
			slog.Warn("pipeline: plugin Shutdown returned error",
				"plugin", p.plugins[i].Name(), "error", err)
		}
	}
}

// validateCapabilities checks that every slot a plugin reads has been written
// by an earlier plugin in the chain.
func validateCapabilities(plugins []Plugin, validSlots map[string]bool) error {
	written := make(map[string]bool)
	for _, plugin := range plugins {
		caps := plugin.Capabilities()
		for _, slot := range caps.Reads {
			if !validSlots[slot] {
				return fmt.Errorf("plugin %q declares read on unknown slot %q", plugin.Name(), slot)
			}
			if !written[slot] {
				return fmt.Errorf("plugin %q reads slot %q but no earlier plugin writes it", plugin.Name(), slot)
			}
		}
		for _, slot := range caps.Writes {
			if !validSlots[slot] {
				return fmt.Errorf("plugin %q declares write on unknown slot %q", plugin.Name(), slot)
			}
			written[slot] = true
		}
	}
	return nil
}
