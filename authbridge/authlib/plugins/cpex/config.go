package cpex

import (
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
)

// cpexConfig is the operator-facing schema for the cpex plugin. The
// decode → applyDefaults → validate → build pattern follows
// authbridge/docs/plugin-reference.md.
//
// Operators express which CPEX hooks fire on each AuthBridge phase
// via the `hooks` block. The CPEX runtime config (the `plugins:` /
// `global:` / `plugin_settings:` YAML that CPEX itself parses) is
// supplied either inline via `config` or by path via `config_file`.
// Operators read CPEX docs verbatim and the YAML format is what they
// would write for CPEX directly — no kagenti-side re-shaping.
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
	// own. Uses kagenti's shared bypass package so semantics match
	// jwt-validation, ibac, etc.
	BypassPaths []string `json:"bypass_paths" description:"URL path globs whose requests skip CPEX. Defaults to /healthz, /readyz, /livez, /.well-known/*."`
}

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
	if c.BypassHosts == nil {
		c.BypassHosts = append([]string(nil), defaultBypassHosts...)
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
