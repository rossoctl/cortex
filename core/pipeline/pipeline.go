package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// DefaultFinishTimeout bounds how long each plugin's OnFinish may run.
// OnFinish commonly does network I/O (flush audits, release
// distributed leases) so the budget needs to be realistic, not
// minimal. 2s matches the order of magnitude of typical side-effect
// I/O without blocking the finish chain indefinitely when a plugin's
// sink hangs. Configurable per-listener via WithFinishTimeout; the
// per-plugin ctx is derived from context.Background() so client
// disconnect during the request never cancels OnFinish.
const DefaultFinishTimeout = 2 * time.Second

// Pipeline holds an ordered list of plugins and runs them sequentially.
// policies[i] holds the on_error ErrorPolicy that wraps plugins[i]; the
// slice is always the same length as plugins (guaranteed by New) so
// PolicyAt is a bounds-safe lookup. An empty ErrorPolicy resolves to
// ErrorPolicyEnforce via the Resolved() method.
type Pipeline struct {
	plugins       []Plugin
	policies      []ErrorPolicy
	finishTimeout time.Duration
	// redirects[i] is plugins[i]'s WritesDestination, read once at New so Run does
	// not ask every plugin for its capabilities on every request.
	redirects []bool
	// requestWriters[i] is plugins[i]'s WritesRequestBody, read once at New for the
	// same reason. SetRequestModel is accepted only from a plugin that declares it.
	requestWriters []bool
}

// Option configures pipeline construction.
type Option func(*options)

type options struct {
	policies      []ErrorPolicy
	finishTimeout time.Duration
}

// WithFinishTimeout overrides the per-plugin OnFinish timeout. Each
// plugin's OnFinish runs under a fresh ctx derived from
// context.Background() with this timeout applied; a zero or negative
// value falls back to DefaultFinishTimeout. Listeners that know their
// deployment's OnFinish I/O patterns can tighten (fast local sinks) or
// relax (remote lease service) this knob.
func WithFinishTimeout(d time.Duration) Option {
	return func(o *options) {
		o.finishTimeout = d
	}
}

// WithPolicies attaches per-plugin on_error policies in parallel with
// the plugin slice passed to New. policies[i] belongs to plugins[i];
// an empty entry defaults to ErrorPolicyEnforce. If fewer policies are
// supplied than plugins, the remaining plugins use the default
// (enforce). Supplying more policies than plugins is a programmer
// error and New returns an error.
func WithPolicies(policies ...ErrorPolicy) Option {
	return func(o *options) {
		o.policies = append(o.policies, policies...)
	}
}

// New creates a Pipeline from the given plugins after validating body-access rules.
func New(plugins []Plugin, opts ...Option) (*Pipeline, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	if err := validateCapabilities(plugins); err != nil {
		return nil, err
	}
	if len(o.policies) > len(plugins) {
		return nil, fmt.Errorf("pipeline: WithPolicies has %d entries but only %d plugins", len(o.policies), len(plugins))
	}
	policies := make([]ErrorPolicy, len(plugins))
	copy(policies, o.policies)
	finishTimeout := o.finishTimeout
	if finishTimeout <= 0 {
		finishTimeout = DefaultFinishTimeout
	}
	redirects := make([]bool, len(plugins))
	requestWriters := make([]bool, len(plugins))
	for i, plugin := range plugins {
		caps := plugin.Capabilities()
		redirects[i] = caps.WritesDestination
		requestWriters[i] = caps.WritesRequestBody
	}
	return &Pipeline{plugins: plugins, policies: policies, finishTimeout: finishTimeout,
		redirects: redirects, requestWriters: requestWriters}, nil
}

// Run executes the request phase of the pipeline sequentially.
// If any plugin returns Reject, the pipeline stops and returns that action
// with Violation.PluginName populated.
//
// Plugins configured with ErrorPolicyOff are skipped entirely — they
// are not dispatched and contribute no Invocation. Plugins under
// ErrorPolicyObserve are dispatched normally, but a Reject return is
// converted into a pass-through: the Violation is recorded as a
// shadow Invocation and the pipeline continues to the next plugin.
// Body mutations under observe are also suppressed — see
// Context.SetBody / SetResponseBody.
//
// Before dispatching into each plugin, Run stamps pctx with the plugin's
// name, the current phase, and the current policy so the plugin's
// Record / Allow / Skip / Observe / Modify / DenyAndRecord helpers can
// fill Invocation.Plugin and Invocation.Phase automatically. The stamp
// is cleared after each plugin returns so a plugin that spawns a
// goroutine capturing pctx won't mis-attribute a late-arriving Record
// to itself.
func (p *Pipeline) Run(ctx context.Context, pctx *Context) Action {
	for i, plugin := range p.plugins {
		policy := p.PolicyAt(i)
		if policy == ErrorPolicyOff {
			slog.Debug("pipeline: plugin disabled (on_error: off)", "plugin", plugin.Name())
			continue
		}
		if ctx.Err() != nil {
			slog.Info("pipeline: request cancelled", "plugin", plugin.Name())
			return Deny("pipeline.cancelled", "request cancelled")
		}
		pctx.setCurrent(plugin.Name(), InvocationPhaseRequest, policy)
		pctx.currentMayRedirect = p.redirectsAt(i)
		pctx.currentMayWriteRequestBody = p.writesRequestAt(i)
		pctx.dispatched = append(pctx.dispatched, i)
		action := plugin.OnRequest(ctx, pctx)
		pctx.clearCurrent()
		if action.Type == Reject {
			stampPluginName(&action, plugin.Name())
			if policy == ErrorPolicyObserve {
				markShadowAndLog(pctx, plugin.Name(), InvocationPhaseRequest, action, "request")
				continue
			}
			pctx.setRejectingPlugin(plugin.Name())
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
// Plugins implementing StreamingResponder are skipped here — the
// framework picks one path per the StreamingResponder contract:
// streaming-aware plugins receive a final OnResponseFrame(last=true)
// from the listener (single dispatch on the buffered application/json
// path; per-frame + last=true on the SSE path) instead of OnResponse,
// so the same body is never delivered through both hooks.
//
// See Run for the pctx attribution stamping, the off-policy skip, and
// the observe-policy shadow conversion. Same pattern, phase set to
// InvocationPhaseResponse.
func (p *Pipeline) RunResponse(ctx context.Context, pctx *Context) Action {
	for i := len(p.plugins) - 1; i >= 0; i-- {
		policy := p.PolicyAt(i)
		if policy == ErrorPolicyOff {
			continue
		}
		if _, ok := p.plugins[i].(StreamingResponder); ok {
			continue
		}
		if ctx.Err() != nil {
			slog.Info("pipeline: response cancelled", "plugin", p.plugins[i].Name())
			return Deny("pipeline.cancelled", "request cancelled")
		}
		pctx.setCurrent(p.plugins[i].Name(), InvocationPhaseResponse, policy)
		action := p.plugins[i].OnResponse(ctx, pctx)
		pctx.clearCurrent()
		if action.Type == Reject {
			stampPluginName(&action, p.plugins[i].Name())
			if policy == ErrorPolicyObserve {
				markShadowAndLog(pctx, p.plugins[i].Name(), InvocationPhaseResponse, action, "response")
				continue
			}
			pctx.setRejectingPlugin(p.plugins[i].Name())
			logReject(p.plugins[i].Name(), action, "pipeline: plugin rejected response")
			return action
		}
	}
	return Action{Type: Continue}
}

// RunResponseFrame dispatches a single response frame to every plugin
// implementing StreamingResponder, in reverse declaration order
// (symmetric with RunResponse). Plugins that don't implement the
// interface are skipped — they're handled by the buffered RunResponse
// path the listener still calls when the response is non-streaming.
//
// The off-policy skip and observe-policy shadow conversion are
// applied identically to RunResponse — see Run for the contract.
//
// Frames are dispatched in wire-arrival order: callers invoke this
// once per frame as they arrive off the upstream, then once with
// last=true (typically with an empty frame) at end-of-stream so
// aggregating plugins can finalize. Application/json responses are
// delivered as a single last=true frame so streaming-aware plugins
// have one code path.
//
// A plugin that returns Reject mid-stream causes the listener to
// short-circuit. Today no in-tree plugin returns Reject here (the
// listeners forward+flush before invoking the hook for observability
// only); the contract leaves room for per-message enforcement later.
func (p *Pipeline) RunResponseFrame(ctx context.Context, pctx *Context, frame []byte, last bool) Action {
	for i := len(p.plugins) - 1; i >= 0; i-- {
		policy := p.PolicyAt(i)
		if policy == ErrorPolicyOff {
			continue
		}
		if ctx.Err() != nil {
			slog.Info("pipeline: response frame cancelled", "plugin", p.plugins[i].Name())
			return Deny("pipeline.cancelled", "request cancelled")
		}
		sr, ok := p.plugins[i].(StreamingResponder)
		if !ok {
			continue
		}
		pctx.setCurrent(p.plugins[i].Name(), InvocationPhaseResponse, policy)
		action := sr.OnResponseFrame(ctx, pctx, frame, last)
		pctx.clearCurrent()
		if action.Type == Reject {
			stampPluginName(&action, p.plugins[i].Name())
			if policy == ErrorPolicyObserve {
				markShadowAndLog(pctx, p.plugins[i].Name(), InvocationPhaseResponse, action, "response-frame")
				continue
			}
			pctx.setRejectingPlugin(p.plugins[i].Name())
			logReject(p.plugins[i].Name(), action, "pipeline: plugin rejected response frame")
			return action
		}
	}
	return Action{Type: Continue}
}

// HasStreamingResponders reports whether any plugin in the pipeline
// implements StreamingResponder. Listeners use this to decide whether
// the streaming code path is worth taking — without any opt-in plugin
// the buffered path delivers the same result for less complexity.
func (p *Pipeline) HasStreamingResponders() bool {
	for _, plugin := range p.plugins {
		if _, ok := plugin.(StreamingResponder); ok {
			return true
		}
	}
	return false
}

// PolicyAt returns the resolved policy for plugins[i] — Plugins()[i] —
// the one Run dispatches it under. The policies slice is always the
// same length as plugins (New guarantees this), but we check
// defensively so a zero-value Pipeline (constructed outside New, e.g.
// in a test) doesn't panic.
//
// Exported for the session API, which serves it on /v1/pipeline: a
// plugin under observe runs and records, but its rejections, body
// writes and redirects are dropped — header writes still apply — and
// nothing else a reader can see says so. Read it from the
// same *Pipeline as Plugins(), not through a Holder twice, or a reload
// between the two calls pairs a plugin with another pipeline's policy.
func (p *Pipeline) PolicyAt(i int) ErrorPolicy {
	if i < len(p.policies) {
		return p.policies[i].Resolved()
	}
	return ErrorPolicyEnforce
}

// redirectsAt reports whether plugins[i] declares WritesDestination. Bounds-safe,
// like PolicyAt, so a Pipeline not built by New never panics.
func (p *Pipeline) redirectsAt(i int) bool {
	return i < len(p.redirects) && p.redirects[i]
}

// writesRequestAt reports whether plugins[i] declares WritesRequestBody. Bounds-safe,
// like redirectsAt.
func (p *Pipeline) writesRequestAt(i int) bool {
	return i < len(p.requestWriters) && p.requestWriters[i]
}

// markShadowAndLog records the would-have-denied Invocation as
// Shadow=true and emits a WARN log. If the plugin already appended a
// deny Invocation (typical for gate plugins that call
// DenyAndRecord / Record before returning Reject), we mark that
// record instead of appending a duplicate — otherwise dashboards
// would double-count a single decision. Synthesize a record only
// when the plugin returned Reject without having recorded its own
// invocation (rare: plugin bug or non-recording denial helper).
func markShadowAndLog(pctx *Context, pluginName string, phase InvocationPhase, action Action, phaseLabel string) {
	status, _, _ := action.Violation.Render()
	marked := pctx.markLastInvocationShadow(pluginName, phase)
	if !marked {
		// Use the Violation's machine-stable code as Reason so
		// dashboards grouping denials by reason see the plugin's
		// actual deny code for both recorded and synthesized paths.
		// The "synthesized" signal lives in Details so operators can
		// still distinguish "plugin Recorded then Deny'd" from
		// "plugin Deny'd without Recording" when debugging.
		reason := "plugin.unspecified"
		if action.Violation != nil && action.Violation.Code != "" {
			reason = action.Violation.Code
		}
		inv := Invocation{
			Plugin: pluginName,
			Phase:  phase,
			Action: ActionDeny,
			Reason: reason,
			Path:   pctx.Path,
			Shadow: true,
		}
		if action.Violation != nil {
			inv.Details = map[string]string{
				"synthesized":       "true",
				"would_deny_reason": action.Violation.Reason,
			}
		}
		pctx.Record(inv)
	}
	slog.Warn("pipeline: plugin would have denied (shadow)",
		"plugin", pluginName,
		"phase", phaseLabel,
		"status", status,
		"code", action.Violation.Code,
		"reason", action.Violation.Reason)
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
// off-process tools (agentop) and other observability surfaces.
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

// NeedsBody returns true if any plugin in the pipeline needs the body
// buffered — either to read it (ReadsBody) or to mutate it (WritesRequestBody).
func (p *Pipeline) NeedsBody() bool {
	return p.NeedsRequestBody() || p.NeedsResponseBody()
}

// NeedsRequestBody reports whether the request body must be buffered.
//
// Split from NeedsBody because the undirected version made each write flag force
// the other direction's buffering: a response-only mutator had the request body
// buffered for nothing, and a request-only mutator had non-SSE responses
// buffered for nothing — the mirror image of the waste the directional
// capabilities exist to remove.
//
// ReadsBody still counts toward both, and deliberately: it is itself undirected
// ("reads pctx.Body and/or pctx.ResponseBody").
func (p *Pipeline) NeedsRequestBody() bool {
	for _, plugin := range p.plugins {
		// RAW capabilities, not Normalize(). The ReadsBody promotion means "you
		// may read the body you write", which is inherently directional — so
		// reading it back through the undirected ReadsBody field would let
		// WritesResponseBody imply a need for the REQUEST body and undo the
		// split. An explicitly declared ReadsBody still counts for both, because
		// that field genuinely does not say which body.
		caps := plugin.Capabilities()
		if caps.ReadsBody || caps.WritesRequestBody {
			return true
		}
	}
	return false
}

// NeedsResponseBody reports whether the response body must be buffered. See
// NeedsRequestBody for why ReadsBody counts toward both.
func (p *Pipeline) NeedsResponseBody() bool {
	for _, plugin := range p.plugins {
		caps := plugin.Capabilities() // raw — see NeedsRequestBody
		if caps.ReadsBody || caps.ReadsResponseBody || caps.WritesResponseBody {
			return true
		}
	}
	return false
}

// WritesRequestBody returns true if any plugin in the pipeline declares
// WritesRequestBody. Listeners use this to decide whether to diff-and-emit a
// body mutation on the wire. A pipeline with no WritesRequestBody plugins
// bypasses the mutation path entirely — zero overhead for the common
// read-only case.
func (p *Pipeline) WritesRequestBody() bool {
	for _, plugin := range p.plugins {
		if plugin.Capabilities().Normalize().WritesRequestBody {
			return true
		}
	}
	return false
}

// WritesResponseBody returns true if any plugin in the pipeline declares
// WritesResponseBody. This is the SSE streaming predicate: a response
// mutator needs the whole response to rewrite it, so listeners fall back
// from incremental relay to the buffered path only when this is true.
//
// A request-only mutator (tool-prune, context-guru) keeps streaming: the
// request body is already complete before dispatch, so rewriting it has
// no bearing on how the response is relayed.
func (p *Pipeline) WritesResponseBody() bool {
	for _, plugin := range p.plugins {
		if plugin.Capabilities().Normalize().WritesResponseBody {
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

// RunFinish dispatches the OnFinish hook on every Finisher-implementing
// plugin whose OnRequest was invoked during this request (Pipeline.Run
// tracks the dispatched set on pctx). Iteration is LIFO — reverse of
// OnRequest order — symmetric with Shutdowner and RunResponse.
//
// Called by the listener after the response has been written to the
// wire (or after the terminal error has been recorded, for denied or
// errored requests). Before the first plugin's OnFinish runs, the
// framework populates pctx.outcome from the supplied Outcome so
// pctx.Outcome() returns non-nil for the duration of the finish chain
// and nil everywhere else.
//
// Each plugin's OnFinish runs under a context derived from
// context.WithoutCancel(ctx) with p.finishTimeout applied. That means:
//   - Cancellation of the caller-supplied ctx (client disconnect,
//     listener shutdown signal) does NOT abort OnFinish's I/O.
//   - Values carried on the caller-supplied ctx (slog fields, request
//     ID, tracing span) ARE propagated into OnFinish.
//   - Deadlines from the caller-supplied ctx are NOT inherited; the
//     per-plugin timeout is authoritative.
//
// OnFinish is best-effort: a panicking plugin is recovered and logged,
// a returning plugin's errors (there is no error return on the
// interface by design — see Finisher godoc) are not observed by the
// framework. The LIFO chain continues regardless so one misbehaving
// plugin does not leak state in earlier plugins.
//
// RunFinish is safe to call at most once per request. A second call
// on the same pctx is rejected with a WARN log rather than double-
// releasing Finisher state (defensive against a listener bug where
// two defers end up registered, or a handler refactor routes the
// finish call through two paths). Listeners MUST call it in a defer
// wrapping the response-produce block so a panic in response-writing
// still reaches cleanup.
func (p *Pipeline) RunFinish(ctx context.Context, pctx *Context, outcome Outcome) {
	if pctx.finished {
		slog.Warn("pipeline: RunFinish called twice on the same pctx — second call dropped")
		return
	}
	pctx.finished = true
	if len(pctx.dispatched) == 0 {
		return
	}
	// Derive Duration from pctx.StartedAt if the caller didn't set it.
	if outcome.Duration == 0 && !pctx.StartedAt.IsZero() {
		outcome.Duration = time.Since(pctx.StartedAt)
	}
	pctx.outcome = &outcome
	defer func() { pctx.outcome = nil }()

	// LIFO over the dispatched indices. Skip the off-policy check:
	// plugins configured on_error: off never have their OnRequest
	// invoked so their index will not be in pctx.dispatched.
	for i := len(pctx.dispatched) - 1; i >= 0; i-- {
		idx := pctx.dispatched[i]
		plugin := p.plugins[idx]
		finisher, ok := plugin.(Finisher)
		if !ok {
			continue
		}
		p.dispatchFinish(ctx, plugin.Name(), finisher, pctx)
	}
}

// dispatchFinish runs OnFinish on one plugin under a detached ctx
// (context.WithoutCancel(parent) + finishTimeout) so the parent's
// cancellation does not abort cleanup I/O but values and tracing
// spans propagate. Panics are recovered into a WARN log so later
// plugins in the LIFO chain still run. Isolated in its own method so
// the recover block's scope is exactly one plugin's dispatch.
func (p *Pipeline) dispatchFinish(parent context.Context, name string, f Finisher, pctx *Context) {
	base := context.WithoutCancel(parent)
	ctx, cancel := context.WithTimeout(base, p.finishTimeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("pipeline: plugin OnFinish panicked",
				"plugin", name,
				"panic", r)
		}
	}()
	pctx.inFinish = true
	defer func() { pctx.inFinish = false }()
	f.OnFinish(ctx, pctx)
}

// validateCapabilities enforces body-mutation ordering rules and the destination rule:
//   - Any number of body mutators of either direction may share a pipeline. Request
//     mutators run in chain order, each seeing pctx.Body as the one before it left it.
//     Response mutators run in reverse, as the whole response pass does, each seeing
//     pctx.ResponseBody as the one after it in the chain left it. In both directions
//     the listener sends what the last to run left.
//   - A body reader (ReadsBody) must not follow a body mutator of either direction, so
//     readers see what the agent saw: the request the client sent, and the response
//     the client receives. A response reader (ReadsResponseBody) must not follow a
//     response mutator.
//   - At most one WritesDestination plugin per pipeline — a request goes to one
//     place, and a second redirect would silently override the first.
func validateCapabilities(plugins []Plugin) error {
	var destinationWriter string
	var firstMutator, firstResponseMutator, readerAfterMutator string
	for _, plugin := range plugins {
		raw := plugin.Capabilities()
		caps := raw.Normalize()
		if caps.WritesDestination {
			if destinationWriter != "" {
				return fmt.Errorf("pipeline: two plugins declare WritesDestination: %q and %q — a request goes to one place; at most one destination writer per pipeline is allowed", destinationWriter, plugin.Name())
			}
			destinationWriter = plugin.Name()
		}
		// A response reader — one that reads response bodies without rewriting
		// them — must not follow a response mutator: the mutator runs after it on
		// the reverse response pass, so the reader would see the upstream's bytes
		// instead of the client's. Declared capabilities, not Normalize's:
		// promotion turns a request writer into a response reader, and a plugin
		// may be both.
		if raw.ReadsResponseBody && !raw.WritesResponseBody && firstResponseMutator != "" && readerAfterMutator == "" {
			readerAfterMutator, firstMutator = plugin.Name(), firstResponseMutator
		}
		if caps.WritesRequestBody || caps.WritesResponseBody {
			if firstMutator == "" {
				firstMutator = plugin.Name()
			}
			if caps.WritesResponseBody && firstResponseMutator == "" {
				firstResponseMutator = plugin.Name()
			}
			continue
		}
		// A reader after any mutator would no longer see what the agent saw. A request
		// mutator before it changes the request it reads. A response mutator before it
		// runs AFTER it on the reverse response pass, so the reader would see the
		// upstream's bytes instead of the client's.
		//
		// With every reader ahead of every mutator, the response pass runs each response
		// mutator before any reader. That holds for the parsers too: every listener runs
		// their per-frame pass after RunResponse on a buffered response. Parser output,
		// the session event built from it and the cost settled from it therefore
		// describe the response the client received. The price: a response mutator
		// cannot use what the parsers found in this response, and parses
		// pctx.ResponseBody itself.
		if caps.ReadsBody && firstMutator != "" && readerAfterMutator == "" {
			readerAfterMutator = plugin.Name()
		}
	}
	if readerAfterMutator != "" {
		return fmt.Errorf("pipeline: plugin %q reads body after mutator %q — body readers must precede every mutator so they see the original bytes", readerAfterMutator, firstMutator)
	}
	return nil
}
