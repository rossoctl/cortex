// Package cpex bridges AuthBridge requests through the CPEX (Context
// Plugin Execution) framework so operators can drive policy with
// CPEX's APL (Attribute Policy Language) DSL — or any of the
// pre-built CPEX plugins (Cedar PDP, PII scanner, audit logger,
// JWT identity, OAuth delegation) — without writing Go code.
//
// One AuthBridge plugin instance ("cpex") wraps an entire CPEX
// PluginManager. Operators declare which CPEX hook names fire on
// each AuthBridge phase via the cpex config's `hooks` block:
//
//	plugins:
//	  - name: cpex
//	    config:
//	      hooks:
//	        on_request:  [cmf.tool_pre_invoke]
//	        on_response: [cmf.tool_post_invoke]
//	      apl: { ... operator's APL block ... }
//	      pipelines: { ... operator's CPEX pipelines block ... }
//
// Hooks fire in declaration order; the chain short-circuits on the
// first sub-plugin that returns deny. Empty hook lists are valid (the
// phase becomes a no-op) so operators can install the plugin and ship
// a YAML update later to enable it.
//
// # Build tag
//
// The real CPEX backend uses cgo and links libcpex_ffi.a. The
// authbridge-cpex binary is the only build target that compiles
// this package with -tags cpex; other binaries (authbridge-proxy,
// authbridge-envoy, authbridge-lite) do not import this package.
//
// The package itself compiles tag-free for unit tests: the Manager
// interface (manager.go) is satisfied by FakeManager in tests, and
// the real adapter (manager_cpex.go) is gated by //go:build cpex.
// `go test ./authlib/plugins/cpex/...` with CGO_ENABLED=0 covers
// every behavior except the FFI translation itself.
//
// Without -tags cpex, NewManager returns an error (manager_stub.go)
// so Configure fails loud at boot rather than silently registering
// a no-op cpex plugin in production.
//
// # Operator surface
//
// Plugin name: `cpex`. Config schema is in config.go and is
// surfaced through pipeline.SchemaProvider for abctl and friends.
package cpex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"sync/atomic"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/bypass"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins"
)

// CPEX is the AuthBridge plugin chassis around a CPEX PluginManager.
// Per-instance state: parsed config, the Manager adapter (Fake in
// tests, real cpex.PluginManager in the cpex binary), and a ready
// flag set after Init.
type CPEX struct {
	cfg     cpexConfig
	manager Manager
	ready   atomic.Bool

	// bypassPaths matches URL paths that skip CPEX entirely (no FFI
	// crossing, no policy evaluation). Built from cfg.BypassPaths in
	// Configure using authlib's shared bypass.Matcher so semantics
	// match jwt-validation and other plugins.
	bypassPaths *bypass.Matcher

	// bypassHosts is the resolved host glob list. Lower-cost than a
	// matcher because we only compare against a single host per
	// request; the goroutine-safe scan happens via matchesAnyHost.
	bypassHosts []string

	// newManager is the Manager constructor used by Configure. When
	// nil — the production default — Configure calls the
	// package-level NewManager (real cpex adapter when -tags cpex,
	// errNoCpexBuild otherwise). Tests inject a closure here to
	// return a FakeManager and exercise Configure's full path.
	newManager func(ManagerOptions) (Manager, error)
}

// NewCPEX returns an unconfigured plugin. The registry calls this on
// every pipeline build; Configure populates the rest.
func NewCPEX() *CPEX { return &CPEX{} }

func init() {
	plugins.RegisterPlugin("cpex", func() pipeline.Plugin { return NewCPEX() })
}

// Name is the registry key operators reference in YAML.
func (p *CPEX) Name() string { return "cpex" }

// Capabilities declares body access and content-source requirements.
//
//   - ReadsBody / WritesBody: CPEX policies routinely inspect and
//     mutate tool args, LLM messages, and HTTP headers, so the
//     plugin needs the body buffered and writable. (Normalize()
//     auto-promotes ReadsBody from WritesBody, so this is belt and
//     suspenders.)
//
//   - RequiresAny: the plugin reads through pctx.ContentSources()
//     in the CMF adapter (cmf.go), so at least one parser must
//     populate them. Failing fast at pipeline.Build is better than
//     silently running CPEX policies over empty content.
//
//   - Description is the one-line operator-facing summary abctl
//     surfaces in the catalog.
func (p *CPEX) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		ReadsBody:   true,
		WritesBody:  true,
		RequiresAny: []string{"mcp-parser", "inference-parser", "a2a-parser"},
		Description: "CPEX bridge: APL DSL + named CPEX plugins (Cedar, PII, audit, …) over a single chain step.",
	}
}

// ConfigSchema reflects cpexConfig field metadata for abctl edit
// templates and JSON-Schema generators.
func (p *CPEX) ConfigSchema() []pipeline.FieldSchema {
	return pipeline.SchemaOf(cpexConfig{})
}

// Configure decodes the plugin's config subtree, builds the Manager,
// re-serializes the operator's apl/pipelines blocks into YAML, and
// hands the result to LoadConfig. Initialize is deferred to Init so
// JWKS-fetching / audit-sink-connecting policies don't slow
// pipeline.Build.
//
// Strict decode (DisallowUnknownFields) rejects misspelled keys
// loudly at boot — operators editing the cpex block in YAML get an
// immediate "unknown field" error rather than silent partial config.
func (p *CPEX) Configure(raw json.RawMessage) error {
	var c cpexConfig
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return fmt.Errorf("cpex config: %w", err)
		}
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return fmt.Errorf("cpex config: %w", err)
	}

	// Build the bypass matcher. config.applyDefaults leaves
	// c.BypassPaths nil so this layer can substitute the
	// canonical authlib default set (kept in one place across the
	// repo by the bypass package).
	bypassPaths := c.BypassPaths
	if bypassPaths == nil {
		bypassPaths = bypass.DefaultPatterns
	}
	matcher, err := bypass.NewMatcher(bypassPaths)
	if err != nil {
		return fmt.Errorf("cpex bypass_paths: %w", err)
	}
	p.bypassPaths = matcher
	p.bypassHosts = c.BypassHosts

	factory := p.newManager
	if factory == nil {
		factory = NewManager
	}
	mgr, err := factory(ManagerOptions{WorkerThreads: c.WorkerThreads})
	if err != nil {
		// Without -tags cpex, manager_stub.go's NewManager surfaces
		// here with a clear "wrong binary" message. Pipeline.Build
		// aborts and operators see it in `kubectl logs`.
		return fmt.Errorf("cpex: %w", err)
	}

	cfgYAML, err := c.resolveYAML()
	if err != nil {
		return fmt.Errorf("cpex config: %w", err)
	}
	if cfgYAML != "" {
		if err := mgr.LoadConfig(cfgYAML); err != nil {
			return fmt.Errorf("cpex: load config: %w", err)
		}
	}

	p.cfg = c
	p.manager = mgr
	return nil
}

// (CPEX YAML resolution lives in cpexConfig.resolveYAML — handles
// both inline `config:` and path-based `config_file:`. The result is
// what we hand to Manager.LoadConfig verbatim.)

// Init starts the CPEX runtime (binds factories to hooks, spawns
// background workers). The ctx carries pipeline.Start's init
// budget — Initialize must respect it.
func (p *CPEX) Init(ctx context.Context) error {
	if p.manager == nil {
		return errors.New("cpex Init: manager nil; Configure not called or failed")
	}
	if err := p.manager.Initialize(ctx); err != nil {
		return fmt.Errorf("cpex initialize: %w", err)
	}
	p.ready.Store(true)
	return nil
}

// Shutdown drains in-flight work and releases CPEX resources. Best
// effort: errors are logged by the framework but don't block other
// plugins' Shutdown.
func (p *CPEX) Shutdown(ctx context.Context) error {
	if p.manager == nil {
		return nil
	}
	p.manager.Shutdown(ctx)
	p.ready.Store(false)
	return nil
}

// Ready reports whether Init has completed. The framework ANDs this
// across every Readier-implementing plugin to gate /readyz.
func (p *CPEX) Ready() bool {
	return p.ready.Load()
}

// Compile-time interface assertions. If any of these break, a
// refactor downstream broke an interface our chassis silently relied
// on; we'd rather hear about it at `go build` than at pipeline start.
var (
	_ pipeline.Plugin       = (*CPEX)(nil)
	_ pipeline.Configurable = (*CPEX)(nil)
	_ pipeline.Initializer  = (*CPEX)(nil)
	_ pipeline.Shutdowner   = (*CPEX)(nil)
	_ pipeline.Readier      = (*CPEX)(nil)
)

// OnRequest fires every CPEX hook listed in cfg.Hooks.OnRequest, in
// order. Short-circuits on the first sub-plugin that returns deny.
// Empty hook list → immediate Continue (no FFI crossing).
func (p *CPEX) OnRequest(ctx context.Context, pctx *pipeline.Context) pipeline.Action {
	return p.runHooks(ctx, pctx, p.cfg.Hooks.OnRequest)
}

// OnResponse fires every CPEX hook listed in cfg.Hooks.OnResponse, in
// order. Same short-circuit + bypass semantics as OnRequest.
func (p *CPEX) OnResponse(ctx context.Context, pctx *pipeline.Context) pipeline.Action {
	return p.runHooks(ctx, pctx, p.cfg.Hooks.OnResponse)
}

// runHooks is the shared OnRequest/OnResponse path: bypass gates,
// iterate over the operator-configured hook list, invoke each on
// CPEX, short-circuit on the first deny.
//
// Skip rules (Continue without invoking CPEX):
//   - manager is nil / ready is false — pipeline misconfigured;
//     defensive no-op (Pipeline.Build should have already failed).
//   - len(hooks) == 0 — operator left this phase empty.
//   - path or host matches the bypass lists.
//   - manager.HasHook(hook) is false — operator listed a hook in
//     config but no sub-plugin is wired for it on the CPEX side.
//     Logged once per request via Invocation Skip rather than failing
//     loudly: the same hook list may apply to multiple operator-
//     configured plugin variants.
//
// Error handling honors cfg.FailOpen:
//   - FailOpen=true  → Observe + Continue, error logged.
//   - FailOpen=false → DenyAndRecord with code "cpex.error".
func (p *CPEX) runHooks(ctx context.Context, pctx *pipeline.Context, hooks []string) pipeline.Action {
	if p.manager == nil || !p.ready.Load() {
		return pipeline.Action{Type: pipeline.Continue}
	}
	if len(hooks) == 0 {
		return pipeline.Action{Type: pipeline.Continue}
	}

	// Fast-skip bypass: traffic to infrastructure (Keycloak, SPIRE,
	// liveness probes) shouldn't pay the FFI round-trip. Check paths
	// first since most bypass traffic is health/discovery rather
	// than hostname-based.
	if p.bypassPaths != nil && p.bypassPaths.Match(pctx.Path) {
		pctx.Skip("path_bypass")
		return pipeline.Action{Type: pipeline.Continue}
	}
	if matchesAnyHost(p.bypassHosts, pctx.Host) {
		pctx.Skip("host_bypass")
		return pipeline.Action{Type: pipeline.Continue}
	}

	for _, hook := range hooks {
		if !p.manager.HasHook(hook) {
			pctx.Skip(fmt.Sprintf("no cpex sub-plugin wired for %s", hook))
			continue
		}
		res, err := p.manager.Invoke(ctx, hook, pctx)
		if err != nil {
			return p.handleInvokeError(pctx, hook, err)
		}
		if action, stop := p.applyDecision(pctx, res); stop {
			return action
		}
	}
	return pipeline.Action{Type: pipeline.Continue}
}

// handleInvokeError converts a CPEX internal error (FFI failure,
// runtime panic) into a pipeline action per cfg.FailOpen.
func (p *CPEX) handleInvokeError(pctx *pipeline.Context, hook string, err error) pipeline.Action {
	if p.cfg.FailOpen {
		slog.Warn("cpex: invoke errored; allowing per fail_open=true",
			"hook", hook, "error", err)
		pctx.Observe(fmt.Sprintf("cpex error (fail_open): %v", err))
		return pipeline.Action{Type: pipeline.Continue}
	}
	// A CPEX-internal failure (FFI error, runtime panic) is an upstream
	// fault, not a policy decision, so reject with 502. Status is set
	// explicitly for the same reason as the deny path: cpex.error is not
	// in the listener's static codeToStatus table.
	return denyWithStatus(pctx, http.StatusBadGateway,
		fmt.Sprintf("cpex error on %s", hook),
		"cpex.error",
		err.Error(),
	)
}

// matchesAnyHost reports whether host matches any glob in patterns
// using path.Match. The host is stripped of its port first so
// "keycloak.local:8081" matches "keycloak.*" naturally. Duplicated
// from ibac (small enough that the cross-plugin dependency isn't
// worth it; the bypass vocabulary may diverge over time).
func matchesAnyHost(patterns []string, host string) bool {
	if host == "" {
		return false
	}
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	for _, p := range patterns {
		if ok, _ := path.Match(p, host); ok {
			return true
		}
	}
	return false
}

// sanitizeReason flattens an arbitrary string (a sub-plugin name, a
// reason code from CPEX, etc.) into the dotted form AuthBridge uses
// for violation codes — lowercase alphanumerics with underscores
// substituting for anything else. Keeps operator-visible codes stable
// across CPEX-side cosmetic changes.
func sanitizeReason(s string) string {
	if s == "" {
		return "unspecified"
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// applyDecision translates a Manager Result into a pipeline.Action +
// a `stop` flag the runHooks loop uses to decide whether to continue
// to the next hook. Deny stops the chain (returns Reject); other
// outcomes record the Invocation and let the chain proceed.
//
// DecisionModify records a modify Invocation; header/label and MCP body
// mutations are already applied by manager_cpex.go before we see the
// Result.
//
// DecisionError and any unrecognized decision fail CLOSED unless
// fail_open is set: a Result the plugin can't interpret must not
// silently allow traffic the policy may have meant to block. The
// fail_open routing matches handleInvokeError so an error surfaced as a
// Result behaves identically to one surfaced as a returned error.
func (p *CPEX) applyDecision(pctx *pipeline.Context, res Result) (action pipeline.Action, stop bool) {
	switch res.Decision {
	case DecisionAllow:
		pctx.Allow(res.Reason)
		return pipeline.Action{Type: pipeline.Continue}, false
	case DecisionDeny:
		// Namespace CPEX-emitted codes under cpex.* so operator
		// dashboards group them distinctly from AuthBridge-native
		// plugins. sanitizeReason normalizes whatever CPEX put in
		// the violation Code to lower_underscore form.
		code := "cpex.denied"
		if res.Code != "" {
			code = "cpex." + sanitizeReason(res.Code)
		}
		return denyWithStatus(pctx, http.StatusForbidden, res.Reason, code, res.Reason), true
	case DecisionModify:
		pctx.Modify(res.Reason)
		return pipeline.Action{Type: pipeline.Continue}, false
	case DecisionObserve:
		pctx.Observe(res.Reason)
		return pipeline.Action{Type: pipeline.Continue}, false
	case DecisionError:
		reason := res.Reason
		if reason == "" {
			reason = "cpex returned an error decision"
		}
		return p.handleDecisionError(pctx, errors.New(reason)), true
	}
	// Any unrecognized decision is a contract violation between the
	// Manager and this chassis — fail closed (honoring fail_open)
	// rather than allowing unconditionally.
	return p.handleDecisionError(pctx,
		fmt.Errorf("cpex: unknown decision %s", res.Decision)), true
}

// handleDecisionError applies the fail_open policy to a DecisionError /
// unknown-decision Result. Mirrors handleInvokeError (which handles the
// returned-error path) so the two failure surfaces behave identically:
// fail_open=true → Observe + Continue; fail_open=false → Deny with code
// cpex.error.
func (p *CPEX) handleDecisionError(pctx *pipeline.Context, err error) pipeline.Action {
	if p.cfg.FailOpen {
		slog.Warn("cpex: error decision; allowing per fail_open=true", "error", err)
		pctx.Observe(fmt.Sprintf("cpex error (fail_open): %v", err))
		return pipeline.Action{Type: pipeline.Continue}
	}
	return denyWithStatus(pctx, http.StatusBadGateway,
		fmt.Sprintf("cpex error: %v", err),
		"cpex.error",
		err.Error(),
	)
}

// denyWithStatus records a deny Invocation and returns a Reject carrying an
// explicit HTTP status. It exists because CPEX violation codes are
// namespaced and dynamic (cpex.denied, cpex.pii_detected,
// cpex.cedar_default_deny, …) and so never match the listener's static
// codeToStatus table; without an explicit status, Violation.Render would
// fall back to 500 for every CPEX outcome. Callers pass 403 for a policy
// deny (an authorization decision) and 502 for a CPEX-internal error (an
// upstream fault). recordReason is the machine-stable Invocation reason;
// message becomes the Violation's human-readable reason.
func denyWithStatus(pctx *pipeline.Context, status int, recordReason, code, message string) pipeline.Action {
	pctx.Record(pipeline.Invocation{Action: pipeline.ActionDeny, Reason: recordReason})
	return pipeline.DenyStatus(status, code, message)
}
