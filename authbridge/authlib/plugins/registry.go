package plugins

import (
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/config"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// PluginFactory returns a fresh plugin instance. Plugins take no
// construction arguments — they receive their configuration through
// pipeline.Configurable.Configure during Build, and any external
// dependencies (JWKS cache, HTTP client, etc.) are built from that
// local config inside Configure.
type PluginFactory func() pipeline.Plugin

// registry is the dynamic plugin table. Populated by RegisterPlugin,
// typically from each plugin package's init() function. Guarded by a
// mutex because init() order across packages isn't guaranteed to be
// serial under every Go build mode, and tests use UnregisterPlugin
// concurrently with t.Parallel.
var (
	registryMu sync.RWMutex
	registry   = map[string]PluginFactory{}
)

// reservedBuiltinNames are plugin names owned by authlib's built-in
// gate plugins. Third-party plugins may not register or unregister
// them: a custom plugin named "jwt-validation" or "token-exchange"
// would silently replace the shipped auth gates, which is an
// authentication bypass dressed as a feature. Built-ins register
// themselves via RegisterBuiltin (below), which bypasses this set.
//
// Parsers (a2a-parser, mcp-parser, inference-parser) are deliberately
// NOT reserved — replacing a parser is a legitimate extension point
// (a team might ship a dialect-specific A2A parser), and it's
// observational, not security-relevant.
var reservedBuiltinNames = map[string]bool{
	"jwt-validation": true,
	"token-exchange": true,
}

// RegisterPlugin adds a plugin factory under name. Intended to be
// called from package init() functions of plugin implementations:
//
//	func init() {
//	    plugins.RegisterPlugin("rate-limiter", func() pipeline.Plugin {
//	        return &RateLimiter{}
//	    })
//	}
//
// This is the stdlib pattern (database/sql.Register, image codec
// registration, log/slog handler registration): plugins live in their
// own package and advertise themselves by side-effect import:
//
//	import _ "github.com/acme/kagenti-rate-limiter/ratelimit"
//
// Double-registration under the same name panics. Silent last-write-
// wins would let a version mismatch or deployment bug poison the
// registry in ways that only surface as mysterious runtime behaviour;
// failing loud at process start is strictly safer.
//
// Empty name or nil factory also panics — both are programmer errors,
// not recoverable conditions.
func RegisterPlugin(name string, factory PluginFactory) {
	if reservedBuiltinNames[name] {
		panic(fmt.Sprintf("plugins: %q is a reserved built-in name; custom plugins cannot override it (use a different name)", name))
	}
	registerPlugin(name, factory)
}

// RegisterBuiltin is the in-tree equivalent of RegisterPlugin for
// authlib's own gate plugins (jwt-validation, token-exchange). It
// exists so built-ins can register names listed in
// reservedBuiltinNames — which RegisterPlugin refuses for everyone
// else. Not part of the third-party plugin API; only authlib's own
// plugin packages should call it.
//
// The unexported registerPlugin does the actual work; having two
// exported entry points lets RegisterPlugin enforce the reserved-name
// rule while RegisterBuiltin bypasses it.
func RegisterBuiltin(name string, factory PluginFactory) {
	registerPlugin(name, factory)
}

// registerPlugin is the shared implementation behind RegisterPlugin
// and RegisterBuiltin. The panic checks here apply to both.
func registerPlugin(name string, factory PluginFactory) {
	if name == "" {
		panic("plugins: RegisterPlugin called with empty name")
	}
	if factory == nil {
		panic(fmt.Sprintf("plugins: RegisterPlugin(%q) factory is nil", name))
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[name]; exists {
		panic(fmt.Sprintf("plugins: %q already registered", name))
	}
	registry[name] = factory
}

// IsReservedBuiltin reports whether name is a reserved authlib
// built-in gate plugin. Exposed so higher-level validators (config
// loader, chain-placement check) can produce helpful error messages
// without duplicating the reserved-names list.
func IsReservedBuiltin(name string) bool {
	return reservedBuiltinNames[name]
}

// RegisteredPlugins returns the names of every registered plugin in
// sorted order. Intended for diagnostic surfaces (/config, CLI --help,
// Build's "unknown plugin" error message) and for tests that assert a
// plugin is visible to the builder.
func RegisteredPlugins() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// factoryFor looks up a factory by name. Internal to the package.
// Callers under Build use this to resolve config entries into plugin
// instances.
func factoryFor(name string) (PluginFactory, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	f, ok := registry[name]
	return f, ok
}

// ChainDirection identifies which side of the pipeline a Build call is
// producing. The zero value deliberately has no valid meaning — callers
// that don't care about built-in placement validation use Build (which
// uses ChainUnspecified and skips the check). Callers that do care use
// BuildChain with Inbound or Outbound so misplaced gates fail loudly.
type ChainDirection int

const (
	// ChainUnspecified disables built-in placement validation. Used by
	// callers that don't know the chain direction (tests, tools that
	// build a single list without inbound/outbound semantics).
	ChainUnspecified ChainDirection = iota
	ChainInbound
	ChainOutbound
)

// builtinChainExpectation records which chain each reserved built-in
// gate is expected to appear on. A misplaced gate is a silent auth
// bypass (jwt-validation in the outbound chain never runs inbound
// traffic through auth; token-exchange in inbound attaches a useless
// token to the wrong direction). Validated in BuildChain.
var builtinChainExpectation = map[string]ChainDirection{
	"jwt-validation": ChainInbound,
	"token-exchange": ChainOutbound,
}

// BuildChain is Build plus chain-aware placement validation: it rejects
// a built-in gate appearing in the wrong chain (e.g., jwt-validation
// configured on the outbound side). Chain-agnostic callers use Build.
//
// Validation runs BEFORE factory construction so a misplaced gate
// produces an error instead of attempting to Configure an off-chain
// plugin.
func BuildChain(direction ChainDirection, entries []config.PluginEntry, opts ...pipeline.Option) (*pipeline.Pipeline, error) {
	for _, e := range entries {
		// Reserved built-ins cannot run under a non-enforce policy:
		// on_error: observe on jwt-validation is an auth bypass, and
		// on_error: off removes the gate entirely. The placement
		// check below is the companion seal; together they prevent
		// YAML from silently disabling the shipped auth gates.
		if IsReservedBuiltin(e.Name) && e.OnError.Resolved() != pipeline.ErrorPolicyEnforce {
			return nil, fmt.Errorf(
				"plugin %q is a reserved built-in gate and must run with on_error: enforce (got %q); shadow mode and off are not permitted for auth plugins",
				e.Name, e.OnError)
		}
		if direction == ChainUnspecified {
			continue
		}
		want, reserved := builtinChainExpectation[e.Name]
		if !reserved {
			continue
		}
		if want != direction {
			return nil, fmt.Errorf("plugin %q is a reserved built-in for the %s chain; it must not appear in the %s chain",
				e.Name, directionName(want), directionName(direction))
		}
	}
	return Build(entries, opts...)
}

// directionName returns the YAML keyword matching a ChainDirection.
// Internal to the package — only error messages need the string form.
func directionName(d ChainDirection) string {
	switch d {
	case ChainInbound:
		return "inbound"
	case ChainOutbound:
		return "outbound"
	default:
		return "unspecified"
	}
}

// Build constructs a pipeline from an ordered list of plugin entries.
// For every plugin that implements pipeline.Configurable, Build calls
// Configure with the entry's Config bytes (nil when omitted). Passing
// config to a plugin that doesn't implement Configurable is rejected so
// stale or misplaced config blocks fail at startup instead of being
// silently ignored.
//
// Unknown plugin names fail fast with an error that lists every
// currently-registered plugin — typo-catching diagnostic.
//
// Build does NOT validate that reserved built-in gates appear in the
// correct chain — callers that know their direction (inbound vs
// outbound) should call BuildChain instead.
func Build(entries []config.PluginEntry, opts ...pipeline.Option) (*pipeline.Pipeline, error) {
	ps := make([]pipeline.Plugin, 0, len(entries))
	policies := make([]pipeline.ErrorPolicy, 0, len(entries))
	for _, e := range entries {
		// ErrorPolicyOff removes the plugin from the running
		// pipeline entirely — no Configure, no Init, no dispatch.
		// Operators use off as a kill-switch without deleting the
		// entry from YAML (so a later re-enable is a single field
		// flip). A mistyped on_error was already rejected at
		// config.Load time, so reaching here means the value is
		// valid.
		if e.OnError.Resolved() == pipeline.ErrorPolicyOff {
			slog.Info("plugins: skipping plugin (on_error: off)", "plugin", e.Name)
			continue
		}
		factory, ok := factoryFor(e.Name)
		if !ok {
			return nil, fmt.Errorf("unknown plugin %q (registered: %v)", e.Name, RegisteredPlugins())
		}
		p := factory()
		if c, ok := p.(pipeline.Configurable); ok {
			if err := c.Configure(e.Config); err != nil {
				return nil, fmt.Errorf("configure %q: %w", e.Name, err)
			}
		} else if len(e.Config) > 0 {
			return nil, fmt.Errorf("plugin %q does not accept configuration", e.Name)
		}
		ps = append(ps, p)
		policies = append(policies, e.OnError.Resolved())
	}
	opts = append(opts, pipeline.WithPolicies(policies...))
	return pipeline.New(ps, opts...)
}
