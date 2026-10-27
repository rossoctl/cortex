package cpex

import (
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"golang.org/x/net/http/httpguts"
)

// cpexConfig is the operator-facing schema for the cpex plugin. The
// decode → applyDefaults → validate → build pattern follows
// docs/plugin-reference.md.
//
// Operators express which CPEX hooks fire on each AuthBridge phase
// via the `hooks` block. The CPEX runtime config (the `plugins:` /
// `global:` / `plugin_settings:` YAML that CPEX itself parses) is
// supplied either inline via `config` or by path via `config_file`.
// Operators read CPEX docs verbatim and the YAML format is what they
// would write for CPEX directly — no rossoctl-side re-shaping.
//
// A cpex plugin with no `hooks`, no `config`, and no `config_file` is
// installed but inert — Configure succeeds; OnRequest/OnResponse
// return Continue immediately. That makes "install cpex now, ship the
// CPEX YAML update later" a one-step rollout.
type cpexConfig struct {
	// Hooks selects which CPEX hook names fire on each AuthBridge
	// pipeline phase. Empty slices mean the plugin is a no-op for
	// that phase. Hooks fire in declaration order; the chain
	// short-circuits on the first sub-plugin that returns deny.
	Hooks hooksConfig `json:"hooks" description:"AuthBridge phase → CPEX hook list. on_request fires during pctx.OnRequest; on_response fires during pctx.OnResponse."`

	// Config is the CPEX runtime YAML inline — what CPEX's own docs
	// describe (top-level `plugins:`, `global:`, `plugin_settings:`).
	// Mutually exclusive with ConfigFile. Convenient for small demos;
	// production usually mounts a ConfigMap and references it via
	// ConfigFile so reloads don't require a sidecar redeploy.
	Config string `json:"config,omitempty" description:"CPEX YAML inline (plugins:/global:/...). Mutually exclusive with config_file."`

	// ConfigFile is a path to a file containing the CPEX runtime
	// YAML. Mutually exclusive with Config. The file is read once
	// at Configure time; hot-reload on file change is a follow-up.
	ConfigFile string `json:"config_file,omitempty" description:"Path to CPEX YAML file. Mutually exclusive with config."`

	// FailOpen controls behavior when CPEX itself errors or panics
	// during invoke. Note: a CPEX policy `deny` is a normal outcome,
	// not an error, and is always honored.
	//
	//   false (default) — CPEX error → AuthBridge denies with 502.
	//   true            — CPEX error → request continues, error logged.
	//
	// Default is fail-closed because CPEX errors usually mean a
	// misconfigured policy or a backend (PDP, JWKS) is unreachable —
	// silently allowing traffic in that state is rarely what operators
	// want. Set true only when an upstream layer has its own enforcement.
	FailOpen bool `json:"fail_open" default:"false" description:"Allow traffic when CPEX itself errors (default false: fail closed)."`

	// WorkerThreads sets the size of CPEX's tokio worker pool. Default
	// 0 means CPEX picks based on available CPUs. Set a small positive
	// value (e.g. 2) to bound sidecar CPU usage in dense node
	// deployments.
	WorkerThreads int `json:"worker_threads" default:"0" description:"CPEX tokio worker pool size. 0 = automatic."`

	// BypassHosts is a list of host glob patterns (path.Match syntax)
	// whose requests skip CPEX entirely — no FFI crossing, no policy
	// evaluation. Sensible defaults cover sidecar infrastructure that
	// shouldn't be policy-gated (Keycloak, SPIRE, observability stack).
	// Operators extend this list for their own internal services.
	//
	// Refuse to bypass with `*` or `""` — that's an "I want to disable
	// the plugin" gesture which is better expressed by removing cpex
	// from the pipeline.
	BypassHosts []string `json:"bypass_hosts" description:"Host globs whose requests skip CPEX. Defaults to keycloak/SPIRE/observability."`

	// BypassPaths is a list of URL path glob patterns (path.Match
	// syntax) whose requests skip CPEX entirely. Defaults to liveness/
	// readiness probes + .well-known discovery; operators add their
	// own. Uses rossoctl's shared bypass package so semantics match
	// jwt-validation, ibac, etc.
	BypassPaths []string `json:"bypass_paths" description:"URL path globs whose requests skip CPEX. Defaults to /healthz, /readyz, /livez, /.well-known/*."`

	// StrictHosts names tool servers that may read a body more leniently
	// than the parsers do, each as a `.`-delimited host glob (as in
	// delegated_credential_hosts, matched without the port), an IP
	// address, or a CIDR matched against a host that is an IP literal. An
	// outbound request to one of them whose body no parser claimed is
	// refused as cpex.unparseable_request when it is a JSON object the
	// parsers cannot read (see unparseableObject), before bypass_hosts and
	// bypass_paths are consulted. Empty by default: no host is checked.
	// Not subject to fail_open.
	StrictHosts []string `json:"strict_hosts" description:"Host globs, IP addresses or CIDRs of tool servers whose unclaimed, unreadable JSON-object bodies are refused as cpex.unparseable_request, before the bypass lists. Default empty."`

	// FallbackIdentification decides how a request that names no tool,
	// prompt, resource or model is identified to CPEX: A2A, MCP protocol
	// methods (tools/list, initialize) and unparsed traffic.
	//
	//   global (default) — CPEX's global entity (http/*), so `http:`
	//                      routes and the global policy govern it.
	//   none             — no identification; CPEX denies the request as
	//                      unidentified_request when the config declares
	//                      any policy.
	FallbackIdentification string `json:"fallback_identification" default:"global" description:"Identification for requests with no named entity: global (CPEX's http/* entity) or none (CPEX denies them as unidentified)."`

	// SessionSource picks the session id CPEX keys session state (taint
	// labels) on.
	//
	//   header (default) — the caller's X-Session-Id header, or the A2A
	//                      context id.
	//   resolved         — the session the listener resolved, but only
	//                      when a dedicated agent session header or the
	//                      process chain named it. A guess (most recent
	//                      session, User-Agent affinity, a pending or
	//                      default bucket) and the generic X-Session-Id and
	//                      Session-Id headers give no session id, and the
	//                      caller's X-Session-Id and A2A context id are
	//                      ignored. When a process above the sender named
	//                      a session, that session wins over the sender's
	//                      own, so a child of an agent stays in its taint.
	//                      That holds only where Cortex has process
	//                      attribution (cortex bound to loopback, as the
	//                      laptop install is). The authbridge-cpex sidecar
	//                      has none, so there resolved trusts the dedicated
	//                      session header, which the sender sets: it does
	//                      not stop a process from leaving its session's
	//                      taint.
	SessionSource string `json:"session_source" default:"header" description:"Session id for CPEX session state: header (X-Session-Id / A2A context id) or resolved (the listener's session, when a dedicated agent header or the process chain named it)."`

	// PrivateDestinations lets every CPEX builtin of this instance (JWKS
	// fetches, token exchange, CIBA, Vault) reach loopback, private,
	// link-local and CGNAT addresses, cloud metadata endpoints included.
	// Off by default; turn it on only for trusted policies on a trusted
	// network, such as a local IdP in development.
	PrivateDestinations bool `json:"private_destinations" default:"false" description:"Allow CPEX builtins to call loopback, private and link-local addresses (cloud metadata included). Default false."`

	// SyntheticEgress presents an outbound request that names no tool,
	// prompt, resource or model as a call to the tool http_egress, so
	// `tool: http_egress` routes govern egress. It also turns off outbound
	// path bypass and the default bypass hosts. Its arguments are method,
	// host, path, a bounded body excerpt, truncated and content_encoding; a
	// CONNECT tunnel carries its host and method only. Off by default: such requests take the fallback
	// identification instead.
	SyntheticEgress bool `json:"synthetic_egress" default:"false" description:"Present outbound requests that name no tool, prompt, resource or model as calls to the tool http_egress. Default false."`

	// Principal makes Cortex assert the caller: the token in token_file is
	// handed to CPEX identity resolution under header for outbound requests
	// presented as tool calls (MCP tools and http_egress) or inference
	// requests. It is never written to the forwarded request, and a caller's
	// own copy of header never reaches identity resolution.
	Principal principalConfig `json:"principal" description:"Cortex-held caller token for CPEX identity resolution on outbound tool calls and inference requests; never forwarded."`

	// Workload makes Cortex assert the calling workload: a JWT-SVID for
	// audience, fetched from the SPIFFE provider, is handed to CPEX
	// identity resolution under header on the same requests as the
	// principal. It needs the runtime config's spiffe: block, is never
	// written to the forwarded request, and a caller's own copy of header
	// never reaches identity resolution.
	Workload workloadConfig `json:"workload" description:"The pod's SPIFFE JWT-SVID for CPEX identity resolution (role: caller_workload) on outbound tool calls and inference requests; never forwarded."`

	// ObserveCodes lists policy violation codes the plugin records as
	// observe instead of enforcing. CPEX keeps the labels the denying
	// step set, so a step can taint a session on content without blocking
	// the request: PPE 0.5.0 has no other conditional taint, since a CEL
	// step that is false always denies. Steps after the denying one do
	// not run, so Configure checks the policy: each code must be named by
	// one deny() alone, as the last entry of a route's phase list (see
	// checkObservedCodes). Codes the plugin itself emits cannot be listed.
	ObserveCodes []string `json:"observe_codes" description:"Policy violation codes recorded as observe instead of enforced; the labels the denying step set still apply."`

	// DelegatedCredentials forwards the credentials CPEX delegate() steps
	// mint: each one is written to the request as its header, replacing
	// the caller's value. Off by default, and then a request whose policy
	// delegated is denied with cpex.delegation_unavailable instead, because
	// the engine keeps the minted token. Needs a CPEX library that returns
	// delegated credentials to the host.
	DelegatedCredentials bool `json:"delegated_credentials" default:"false" description:"Forward the credentials CPEX delegate() steps mint, as request headers. Default false: a request whose policy delegated is denied."`

	// DelegatedCredentialHeaders lists the headers a delegated credential
	// may be written to. A credential for any other header denies the
	// request with cpex.delegation_unavailable.
	DelegatedCredentialHeaders []string `json:"delegated_credential_headers" description:"Headers a delegated credential may be written to. Default [Authorization]."`

	// DelegatedCredentialHosts binds each delegation target (the target a
	// delegate() step names, else its audience) to the hosts its
	// credentials may be sent to, as `.`-delimited globs matched against
	// the request's host without its port, or IP addresses and CIDRs
	// matched against a canonical IP literal only. Required with
	// delegated_credentials: the caller picks the tool and the destination,
	// so a credential whose target is not bound to the request's host
	// denies it with cpex.delegation_destination_mismatch and nothing is
	// written.
	DelegatedCredentialHosts map[string][]string `json:"delegated_credential_hosts" description:"Delegation target → host globs its credentials may be sent to. Required with delegated_credentials."`
}

// principalConfig configures principal injection. An empty TokenFile turns
// it off.
type principalConfig struct {
	TokenFile string `json:"token_file,omitempty" description:"File holding the caller token (a JWT). Empty turns principal injection off."`
	Header    string `json:"header,omitempty" default:"x-user-token" description:"Identity header the token is presented under; the CPEX identity plugin reads it."`
}

// defaultPrincipalHeader is the identity header principal injection uses
// when none is configured.
const defaultPrincipalHeader = "x-user-token"

// hooksConfig declares which CPEX hooks the AuthBridge plugin
// invokes on each phase. Lists are evaluated in order; the plugin
// short-circuits on the first sub-plugin that returns deny. Use the
// HookToolPreInvoke / HookToolPostInvoke / HookLLMInput /
// HookLLMOutput constants for hook names — they match what CPEX's
// CMF dispatcher recognizes.
type hooksConfig struct {
	OnRequest  []string `json:"on_request,omitempty" description:"CPEX hook names fired during AuthBridge OnRequest, in order."`
	OnResponse []string `json:"on_response,omitempty" description:"CPEX hook names fired during AuthBridge OnResponse, in order."`
}

// defaultBypassHosts is the conservative starting set: sidecar
// infrastructure that should never see CPEX policy evaluation. Mirrors
// the ibac convention so operators get consistent behavior across
// plugins.
var defaultBypassHosts = []string{
	"keycloak.*",
	"keycloak",
	"spire-server.*",
	"spire-agent.*",
	"otel-collector.*",
	"jaeger.*",
	"prometheus.*",
}

// applyDefaults fills in zero-valued fields with their documented
// defaults. Called by Configure between decode and validate.
//
// Hooks default to empty — operators must opt in explicitly. A no-op
// plugin (no hooks configured) is a valid steady state; the operator
// updates the YAML to enable policies when ready.
//
// BypassHosts and BypassPaths default to the conservative
// infrastructure sets when the operator left them unset; an
// operator who genuinely wants empty bypass lists must opt out
// with `bypass_hosts: []` / `bypass_paths: []`.
func (c *cpexConfig) applyDefaults() {
	if c.FallbackIdentification == "" {
		c.FallbackIdentification = fallbackGlobal
	}
	if c.SessionSource == "" {
		c.SessionSource = sessionSourceHeader
	}
	// The default bypass hosts are wildcards a caller can match with a
	// host of its own (keycloak.attacker.example), so synthetic egress,
	// which gates the caller's destinations, does not take them.
	if c.BypassHosts == nil && !c.SyntheticEgress {
		c.BypassHosts = append([]string(nil), defaultBypassHosts...)
	}
	if c.Principal.TokenFile != "" && c.Principal.Header == "" {
		c.Principal.Header = defaultPrincipalHeader
	}
	if c.Workload.Audience != "" && c.Workload.Header == "" {
		c.Workload.Header = defaultWorkloadHeader
	}
	if c.DelegatedCredentials && c.DelegatedCredentialHeaders == nil {
		c.DelegatedCredentialHeaders = []string{"Authorization"}
	}
	// c.BypassPaths is filled lazily from bypass.DefaultPatterns
	// in plugin.go's Configure (avoids importing bypass here).
}

// validate rejects configs that would fail at boot or at first
// request. Each error names the offending JSON field so operators
// can locate the typo in their YAML.
//
// No config combination is REQUIRED — a fully-empty config produces
// a valid no-op plugin. The validation here only catches positively
// broken inputs (negative numbers, `*` bypass patterns, empty hook
// names, Config + ConfigFile both set).
func (c *cpexConfig) validate() error {
	if c.Config != "" && c.ConfigFile != "" {
		return errors.New("`config` and `config_file` are mutually exclusive; set at most one")
	}
	switch c.FallbackIdentification {
	case fallbackGlobal, fallbackNone:
	default:
		return fmt.Errorf("fallback_identification must be %q or %q, got %q",
			fallbackGlobal, fallbackNone, c.FallbackIdentification)
	}
	switch c.SessionSource {
	case sessionSourceHeader, sessionSourceResolved:
	default:
		return fmt.Errorf("session_source must be %q or %q, got %q",
			sessionSourceHeader, sessionSourceResolved, c.SessionSource)
	}
	if c.Principal.TokenFile != "" {
		switch h := strings.ToLower(strings.TrimSpace(c.Principal.Header)); {
		case h == "", strings.ContainsAny(h, " \t:"):
			return fmt.Errorf("principal.header %q is not a header name", c.Principal.Header)
		case h == "authorization" || isProtected(h):
			return fmt.Errorf("principal.header must not be %q", c.Principal.Header)
		}
	}
	if c.Workload.Audience != "" {
		switch h := strings.ToLower(strings.TrimSpace(c.Workload.Header)); {
		case h == "", strings.ContainsAny(h, " \t:"):
			return fmt.Errorf("workload.header %q is not a header name", c.Workload.Header)
		case h == "authorization" || isProtected(h):
			return fmt.Errorf("workload.header must not be %q", c.Workload.Header)
		case c.Principal.TokenFile != "" && h == strings.ToLower(strings.TrimSpace(c.Principal.Header)):
			return fmt.Errorf("workload.header must differ from principal.header")
		}
	}
	if c.DelegatedCredentials && len(c.DelegatedCredentialHosts) == 0 {
		return errors.New("delegated_credentials needs delegated_credential_hosts, the hosts each target's credentials may be sent to")
	}
	if len(c.DelegatedCredentialHeaders) > 0 && !c.DelegatedCredentials {
		return errors.New("delegated_credential_headers needs delegated_credentials: true")
	}
	for _, h := range c.DelegatedCredentialHeaders {
		lower := strings.ToLower(strings.TrimSpace(h))
		switch {
		case lower == "" || !httpguts.ValidHeaderFieldName(lower):
			return fmt.Errorf("delegated_credential_headers: %q is not a header name", h)
		case isFramingOrInternal(lower),
			c.Principal.TokenFile != "" && lower == strings.ToLower(strings.TrimSpace(c.Principal.Header)),
			c.Workload.Audience != "" && lower == strings.ToLower(strings.TrimSpace(c.Workload.Header)):
			return fmt.Errorf("delegated_credential_headers must not name %q", h)
		}
	}
	if c.WorkerThreads < 0 {
		return fmt.Errorf("worker_threads must be >= 0, got %d", c.WorkerThreads)
	}
	for _, h := range c.Hooks.OnRequest {
		if strings.TrimSpace(h) == "" {
			return fmt.Errorf("hooks.on_request: empty hook name")
		}
	}
	for _, h := range c.Hooks.OnResponse {
		if strings.TrimSpace(h) == "" {
			return fmt.Errorf("hooks.on_response: empty hook name")
		}
	}
	for _, code := range c.ObserveCodes {
		n := observeCode(code)
		switch {
		case n == "cpex.unspecified" || strings.ContainsAny(code, "*?"):
			return fmt.Errorf("observe_codes: %q is not a violation code", code)
		case pluginCodes[n]:
			return fmt.Errorf("observe_codes: %q is emitted by the plugin, not by policy", code)
		}
	}
	for _, p := range c.BypassHosts {
		if _, err := path.Match(p, ""); err != nil {
			return fmt.Errorf("invalid bypass_hosts pattern %q: %w", p, err)
		}
		if trimmed := strings.TrimSpace(p); trimmed == "" || trimmed == "*" {
			return fmt.Errorf("bypass_hosts pattern %q matches everything; "+
				"if you mean to disable CPEX, remove it from the pipeline instead", p)
		}
	}
	for _, p := range c.BypassPaths {
		if _, err := path.Match(p, "/"); err != nil {
			return fmt.Errorf("invalid bypass_paths pattern %q: %w", p, err)
		}
		if trimmed := strings.TrimSpace(p); trimmed == "" || trimmed == "*" || trimmed == "/*" {
			return fmt.Errorf("bypass_paths pattern %q matches everything; "+
				"if you mean to disable CPEX, remove it from the pipeline instead", p)
		}
	}
	return nil
}

// observeCode normalizes an observe_codes entry to the namespaced code
// a deny carries, with or without its "cpex." prefix.
func observeCode(code string) string {
	return "cpex." + sanitizeReason(strings.TrimPrefix(strings.TrimSpace(code), "cpex."))
}

// pluginCodes are deny codes the plugin emits itself; they guard against
// forwarding a request CPEX cannot vouch for, so they are never observed.
var pluginCodes = map[string]bool{
	"cpex." + codePrincipalUnavailable:          true,
	"cpex." + codeDelegationUnavailable:         true,
	"cpex." + codeDelegationDestinationMismatch: true,
	"cpex." + codeAmbiguousRequest:              true,
	"cpex." + codeUnnamedEntity:                 true,
	"cpex." + codeUnparseableRequest:            true,
}

// resolveYAML returns the CPEX YAML string the operator supplied —
// either Config verbatim or the contents of ConfigFile. Called once
// during Configure, after validate. Returns "" when neither is set
// (a valid no-op install).
//
// The file read happens here rather than in validate so a transient
// I/O error surfaces as a Configure error (caught by Pipeline.Build,
// fails the boot loudly) rather than a validation error (which
// operators read as "your YAML is wrong").
func (c *cpexConfig) resolveYAML() (string, error) {
	if c.Config != "" {
		return c.Config, nil
	}
	if c.ConfigFile == "" {
		return "", nil
	}
	b, err := os.ReadFile(c.ConfigFile)
	if err != nil {
		return "", fmt.Errorf("read config_file %q: %w", c.ConfigFile, err)
	}
	return string(b), nil
}
