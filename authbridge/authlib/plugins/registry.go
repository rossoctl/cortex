package plugins

import (
	"fmt"
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

// Build constructs a pipeline from an ordered list of plugin entries.
// For every plugin that implements pipeline.Configurable, Build calls
// Configure with the entry's Config bytes (nil when omitted). Passing
// config to a plugin that doesn't implement Configurable is rejected so
// stale or misplaced config blocks fail at startup instead of being
// silently ignored.
//
// Unknown plugin names fail fast with an error that lists every
// currently-registered plugin — typo-catching diagnostic.
func Build(entries []config.PluginEntry, opts ...pipeline.Option) (*pipeline.Pipeline, error) {
	ps := make([]pipeline.Plugin, 0, len(entries))
	for _, e := range entries {
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
	}
	return pipeline.New(ps, opts...)
}

// Made with Bob
