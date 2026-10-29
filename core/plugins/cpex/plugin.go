// Package cpex bridges AuthBridge requests through the CPEX (Context
// Plugin Execution) framework so operators can drive policy with
// CPEX's APL (Attribute Policy Language) DSL — or any of CPEX's
// built-in plugins (JWT identity, OAuth delegation, the Cedar, CEL
// and OPA decision points) — without writing Go code.
//
// One AuthBridge plugin instance ("cpex") wraps an entire CPEX
// PolicyEngine. Operators declare which CPEX hook names fire on
// each AuthBridge phase via the cpex config's `hooks` block:
//
//	plugins:
//	  - name: cpex
//	    config:
//	      hooks:
//	        on_request:  [cmf.tool_pre_invoke]
//	        on_response: [cmf.tool_post_invoke]
//	      config_file: /etc/cpex/cpex.yaml
//
// Hooks fire in declaration order; the chain short-circuits on the
// first sub-plugin that returns deny. Empty hook lists are valid (the
// phase becomes a no-op) so operators can install the plugin and ship
// a YAML update later to enable it.
//
// # Build tag
//
// The real CPEX backend uses cgo and links libcpex_ffi.a. Only cortex
// built with -tags cpex (the authbridge-cpex image and `make
// cortex-cpex`) links it; other builds do not import this package.
//
// The package itself compiles tag-free for unit tests: the Manager
// interface (manager.go) is satisfied by FakeManager in tests, and
// the real adapter (manager_cpex.go) is gated by //go:build cpex.
// `go test ./core/plugins/cpex/...` with CGO_ENABLED=0 covers
// every behavior except the FFI translation itself.
//
// Without -tags cpex, NewManager returns an error (manager_stub.go)
// so Configure fails loud at boot rather than silently registering
// a no-op cpex plugin in production.
//
// # Operator surface
//
// Plugin name: `cpex`. Config schema is in config.go and is
// surfaced through pipeline.SchemaProvider for agentop and friends.
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

	"github.com/rossoctl/cortex/core/bypass"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins"
	"github.com/rossoctl/cortex/core/spiffe"
)

// CPEX is the AuthBridge plugin chassis around a CPEX PolicyEngine.
// Per-instance state: parsed config, the Manager adapter (Fake in
// tests, real cpex.PolicyEngine in the cpex binary), and a ready
// flag set after Init.
type CPEX struct {
	cfg     cpexConfig
	manager Manager
	ready   atomic.Bool

	// bypassPaths matches URL paths that skip CPEX entirely (no FFI
	// crossing, no policy evaluation). Built from cfg.BypassPaths in
	// Configure using core's shared bypass.Matcher so semantics
	// match jwt-validation and other plugins.
	bypassPaths *bypass.Matcher

	// bypassHosts is the resolved host glob list. Lower-cost than a
	// matcher because we only compare against a single host per
	// request; the goroutine-safe scan happens via matchesAnyHost.
	bypassHosts []string

	// strictHosts is the compiled strict_hosts list: outbound
	// destinations whose unclaimed bodies are checked by
	// unparseableObject.
	strictHosts hostPatterns

	// observeCodes holds the namespaced deny codes recorded as observe
	// rather than enforced (observe_codes).
	observeCodes map[string]bool

	// newManager is the Manager constructor used by Configure. When
	// nil — the production default — Configure calls the
	// package-level NewManager (real cpex adapter when -tags cpex,
	// errNoCpexBuild otherwise). Tests inject a closure here to
	// return a FakeManager and exercise Configure's full path.
	newManager func(ManagerOptions) (Manager, error)

	// spiffe is the process SPIFFE provider, set by the pipeline builder
	// when the runtime config has a spiffe: block. testWorkloadJWT
	// replaces it in tests.
	provider        *spiffe.Provider
	testWorkloadJWT spiffe.JWTSource
}

// SetSPIFFEProvider implements spiffe.ProviderConsumer.
func (p *CPEX) SetSPIFFEProvider(prov *spiffe.Provider) { p.provider = prov }

// workloadJWT returns the JWT-SVID source for audience, or nil when no
// provider is wired in.
func (p *CPEX) workloadJWT(audience string) spiffe.JWTSource {
	if p.testWorkloadJWT != nil {
		return p.testWorkloadJWT
	}
	if p.provider == nil || audience == "" {
		return nil
	}
	src, err := p.provider.JWTSource(audience)
	if err != nil {
		slog.Warn("cpex: SPIFFE JWT source unavailable", "audience", audience, "error", err)
		return nil
	}
	return src
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
//   - ReadsBody / WritesRequestBody: CPEX policies routinely inspect and
//     mutate tool args, LLM messages, and HTTP headers, so the
//     plugin needs the body buffered and writable. (Normalize()
//     auto-promotes ReadsBody from WritesRequestBody, so this is belt and
//     suspenders.)
//
//   - RequiresAny: the plugin reads through pctx.ContentSources()
//     in the CMF adapter (cmf.go), so at least one parser must
//     populate them. Failing fast at pipeline.Build is better than
//     silently running CPEX policies over empty content.
//
//   - Description is the one-line operator-facing summary agentop
//     surfaces in the catalog.
func (p *CPEX) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		ReadsBody:          true,
		WritesRequestBody:  true,
		WritesResponseBody: true, // cmf_body / cmf_a2a / cmf_inference rewrite responses
		RequiresAny:        []string{"mcp-parser", "inference-parser", "a2a-parser"},
		Description:        "CPEX bridge: APL DSL + named CPEX plugins (Cedar, PII, audit, …) over a single chain step.",
	}
}

// ConfigSchema reflects cpexConfig field metadata for agentop edit
// templates and JSON-Schema generators.
func (p *CPEX) ConfigSchema() []pipeline.FieldSchema {
	return pipeline.SchemaOf(cpexConfig{})
}

// Configure decodes the plugin's config subtree, builds the Manager,
// resolves the CPEX YAML from `config` or `config_file`, and hands it
// to LoadConfig verbatim. Initialize is deferred to Init so
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
	// canonical core default set (kept in one place across the
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
	strictHosts, err := newHostPatterns("strict_hosts", c.StrictHosts)
	if err != nil {
		return fmt.Errorf("cpex config: %w", err)
	}
	p.strictHosts = strictHosts
	p.observeCodes = nil
	for _, code := range c.ObserveCodes {
		if p.observeCodes == nil {
			p.observeCodes = map[string]bool{}
		}
		p.observeCodes[observeCode(code)] = true
	}

	// bypass_hosts is honored on the outbound phase only and ignored on the
	// inbound reverse-proxy phase, where the Host header is attacker-
	// controlled and identity is resolved inside the bypassed chain (Finding
	// 1). Direction isn't known at Configure time (one instance can serve
	// either phase), so warn unconditionally when it's set rather than
	// rejecting the config.
	if len(c.BypassHosts) > 0 {
		slog.Warn("cpex: bypass_hosts is honored on the outbound phase only; "+
			"it is ignored on the inbound reverse-proxy phase where the Host "+
			"header is attacker-controlled", "bypass_hosts", c.BypassHosts)
	}

	// A missing token file does not fail the boot: requests that need
	// the principal are denied until it can be read.
	principal, err := newPrincipalSource(c.Principal)
	if err != nil {
		slog.Warn("cpex: principal token unavailable; tool calls and inference requests that resolve identity will be denied",
			"token_file", c.Principal.TokenFile, "error", err)
	}

	cfgYAML, err := c.resolveYAML()
	if err != nil {
		return fmt.Errorf("cpex config: %w", err)
	}
	if len(c.ObserveCodes) > 0 && cfgYAML != "" {
		if err := checkObservedCodes(cfgYAML, c.ObserveCodes); err != nil {
			return fmt.Errorf("cpex config: %w", err)
		}
	}

	credentialHosts, err := newCredentialHosts(c.DelegatedCredentialHosts)
	if err != nil {
		return fmt.Errorf("cpex config: %w", err)
	}

	factory := p.newManager
	if factory == nil {
		factory = NewManager
	}
	mgr, err := factory(ManagerOptions{
		WorkerThreads:          c.WorkerThreads,
		FallbackIdentification: c.FallbackIdentification,
		SessionSource:          c.SessionSource,
		PrivateDestinations:    c.PrivateDestinations,
		SyntheticEgress:        c.SyntheticEgress,
		Principal:              principal,
		Workload:               newWorkloadSource(c.Workload, p.workloadJWT(c.Workload.Audience)),

		DelegatedCredentials:       c.DelegatedCredentials,
		DelegatedCredentialHeaders: c.DelegatedCredentialHeaders,
		DelegatedCredentialHosts:   credentialHosts,
	})
	if err != nil {
		// Without -tags cpex, manager_stub.go's NewManager surfaces
		// here with a clear "wrong binary" message. Pipeline.Build
		// aborts and operators see it in `kubectl logs`.
		return fmt.Errorf("cpex: %w", err)
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
//   - manager is nil — pipeline misconfigured; defensive no-op
//     (Pipeline.Build should have already failed).
//   - len(hooks) == 0 — operator left this phase empty. Checked before
//     the ready gate so a no-hooks plugin stays a no-op even when the
//     engine is down.
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
	if p.manager == nil {
		// Never configured — Pipeline.Build should have refused this
		// config already; a defensive no-op.
		return pipeline.Action{Type: pipeline.Continue}
	}
	if len(hooks) == 0 {
		// Before the ready check: a no-hooks plugin is a valid steady
		// state (see applyDefaults), in the pipeline for its other
		// features, so an engine that is down must not deny traffic the
		// operator configured no hook to judge.
		return pipeline.Action{Type: pipeline.Continue}
	}
	if !p.ready.Load() {
		// Not ready is reachable, not hypothetical: Shutdown clears
		// ready, and the reloader stops the old pipeline 30s after a
		// swap — a stream that started before the swap and answers
		// later runs here with the manager torn down. Continuing
		// silently skipped redaction and deny for the rest of that
		// stream, so follow the same fail_open policy as every other
		// CPEX failure instead of inventing an unconditional allow.
		if p.cfg.FailOpen {
			slog.Warn("cpex: not ready (init incomplete or shutdown); allowing per fail_open=true")
			pctx.Observe("cpex not ready (fail_open)")
			return pipeline.Action{Type: pipeline.Continue}
		}
		slog.Error("cpex: not ready (init incomplete or shutdown); denying per fail_open=false")
		return denyWithStatus(pctx, http.StatusBadGateway,
			"cpex not ready",
			"cpex.not_ready",
			"cpex policy engine unavailable",
			nil,
		)
	}

	// strict_hosts wins over both bypass lists: a strict destination's
	// unreadable body is refused even when its host or path is also
	// bypassed, so a bypass entry cannot reopen what strict_hosts closes.
	// Outbound only: inbound, the Host header is the caller's.
	strict := pctx.Direction == pipeline.Outbound && p.strictHosts.match(pctx.Host)
	if strict && pctx.CurrentPhase() != pipeline.InvocationPhaseResponse {
		if code := unpresentableRequest(pctx, true); code == codeUnparseableRequest {
			code = "cpex." + code
			return denyWithStatus(pctx, http.StatusForbidden, code, code, deniedMessage, nil)
		}
	}

	// Fast-skip bypass: traffic to infrastructure (Keycloak, SPIRE,
	// liveness probes) shouldn't pay the FFI round-trip. Check paths
	// first since most bypass traffic is health/discovery rather
	// than hostname-based.
	// With synthetic egress the outbound path is the caller's choice for
	// any host, so path bypass applies inbound only.
	pathBypass := pctx.Direction != pipeline.Outbound || !p.cfg.SyntheticEgress
	if pathBypass && p.bypassPaths != nil && p.bypassPaths.Match(pctx.Path) {
		pctx.Skip("path_bypass")
		return pipeline.Action{Type: pipeline.Continue}
	}
	// bypass_hosts is honored on OUTBOUND only. On the inbound reverse-proxy
	// phase the Host header is attacker-controlled and identity has NOT been
	// pre-validated (it is resolved inside this very chain), so a spoofed Host
	// must not skip CPEX. Path bypass (above) still applies inbound for
	// health/discovery. See security review Finding 1.
	if pctx.Direction == pipeline.Outbound && matchesAnyHost(p.bypassHosts, pctx.Host) {
		pctx.Skip("host_bypass")
		return pipeline.Action{Type: pipeline.Continue}
	}

	if pctx.CurrentPhase() != pipeline.InvocationPhaseResponse {
		if code := unpresentableRequest(pctx, strict); code != "" {
			code = "cpex." + code
			return denyWithStatus(pctx, http.StatusForbidden, code, code, deniedMessage, nil)
		}
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
		pctx.Observe("cpex error (fail_open)")
		return pipeline.Action{Type: pipeline.Continue}
	}
	// A CPEX-internal failure (FFI error, runtime panic) is an upstream
	// fault, not a policy decision, so reject with 502. Status is set
	// explicitly for the same reason as the deny path: cpex.error is not
	// in the listener's static codeToStatus table. The detailed error
	// (which may name FFI symbols, backends, or config paths) is logged
	// for operators; the client only sees a generic message.
	slog.Error("cpex: invoke failed", "hook", hook, "error", err)
	return denyWithStatus(pctx, http.StatusBadGateway,
		fmt.Sprintf("cpex error on %s", hook),
		"cpex.error",
		"cpex invocation failed",
		nil,
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
// A deny records and returns only the namespaced violation code and a
// static message. CPEX's reason is policy-authored free text that can
// quote the request (a matched secret, say); it goes to the debug log, not
// to the client or the session API.
//
// DecisionError and any unrecognized decision fail CLOSED unless
// fail_open is set: a Result the plugin can't interpret must not
// silently allow traffic the policy may have meant to block. The
// fail_open routing matches handleInvokeError so an error surfaced as a
// Result behaves identically to one surfaced as a returned error.
func (p *CPEX) applyDecision(pctx *pipeline.Context, res Result) (action pipeline.Action, stop bool) {
	switch res.Decision {
	case DecisionAllow:
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionAllow, Reason: res.Reason, Details: res.Details})
		return pipeline.Action{Type: pipeline.Continue}, false
	case DecisionDeny:
		if res.Elicitation != nil {
			return suspend(pctx, res.Elicitation, res.Details), true
		}
		// Namespace CPEX-emitted codes under cpex.* so operator
		// dashboards group them distinctly from AuthBridge-native
		// plugins. sanitizeReason normalizes whatever CPEX put in
		// the violation Code to lower_underscore form.
		code := "cpex.denied"
		if res.Code != "" {
			code = "cpex." + sanitizeReason(res.Code)
		}
		slog.Debug("cpex: policy denied", "code", code, "reason", res.Reason)
		if p.observeCodes[code] {
			pctx.Record(pipeline.Invocation{Action: pipeline.ActionObserve, Reason: code, Details: res.Details})
			return pipeline.Action{Type: pipeline.Continue}, false
		}
		return denyWithStatus(pctx, http.StatusForbidden, code, code, deniedMessage, res.Details), true
	case DecisionModify:
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionModify, Reason: res.Reason, Details: res.Details})
		return pipeline.Action{Type: pipeline.Continue}, false
	case DecisionObserve:
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionObserve, Reason: res.Reason, Details: res.Details})
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
		pctx.Observe("cpex error (fail_open)")
		return pipeline.Action{Type: pipeline.Continue}
	}
	// Log the detailed error for operators; record and return generic text
	// so internal CPEX/runtime details don't leak in the deny response or
	// the session API.
	slog.Error("cpex: decision handling failed", "error", err)
	return denyWithStatus(pctx, http.StatusBadGateway,
		"cpex.error",
		"cpex.error",
		"cpex decision handling failed",
		nil,
	)
}

// deniedMessage is the client-facing message of every policy deny.
const deniedMessage = "request denied by policy"

// denyWithStatus records a deny Invocation and returns a Reject carrying an
// explicit HTTP status. It exists because CPEX violation codes are
// namespaced and dynamic (cpex.denied, cpex.pii_detected,
// cpex.cedar_default_deny, …) and so never match the listener's static
// codeToStatus table; without an explicit status, Violation.Render would
// fall back to 500 for every CPEX outcome. Callers pass 403 for a policy
// deny (an authorization decision) and 502 for a CPEX-internal error (an
// upstream fault). recordReason is the machine-stable Invocation reason;
// message becomes the Violation's human-readable reason.
func denyWithStatus(pctx *pipeline.Context, status int, recordReason, code, message string, details map[string]string) pipeline.Action {
	pctx.Record(pipeline.Invocation{Action: pipeline.ActionDeny, Reason: recordReason, Details: details})
	return pipeline.DenyStatus(status, code, message)
}
