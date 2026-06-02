package plugins

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/config"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/spiffe"
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

// CatalogEntry pairs a registered plugin's name with the capabilities
// it advertises and the field-level schema of its config (if it
// implements pipeline.SchemaProvider). Surfaces in `abctl`'s catalog
// pane, in the /v1/plugins endpoint, and in `abctl edit`'s template
// renderer so operators can see what plugins exist, what each one
// needs, and what each field means without reading source.
//
// Fields is nil for plugins without a config (or that don't
// implement SchemaProvider) — the wire format omits the field via
// `omitempty`, so existing consumers that don't know about field
// schemas keep working.
type CatalogEntry struct {
	Name         string
	Capabilities pipeline.PluginCapabilities
	Fields       []pipeline.FieldSchema
}

// catalogCache memoizes the Catalog() result on first call. Constructors
// run once at first invocation; the throwaway instance is unreferenced
// after Capabilities() is read but never explicitly torn down.
//
// Caching bounds the constructor side-effect surface to a one-shot per
// process — even a misbehaving plugin that allocates a goroutine in its
// constructor leaks one goroutine, not one per /v1/plugins request.
var (
	catalogCacheMu  sync.RWMutex
	catalogCacheVal []CatalogEntry
)

// Catalog returns a sorted snapshot of every registered plugin's
// capabilities. The result is computed on first call and cached;
// subsequent calls return the cached slice (do not mutate it).
//
// First-call mechanics: each registered factory is invoked once with no
// config; the resulting instance's Capabilities() is the static
// type-level metadata that the rest of the framework also reads.
//
// CONSTRUCTOR CONTRACT: factories called from Catalog MUST NOT allocate
// goroutines, network connections, file handles, or other resources
// that need explicit teardown. Allocate the plugin struct and nothing
// more — heavy work belongs in Init() (after Configure()), where the
// framework owns the lifecycle. The throwaway instance Catalog
// constructs is never Shutdown'd; anything it leaks is process-wide.
//
// The godoc on PluginCapabilities documents the parallel constraint
// that Capabilities() must be instance-state-independent (so the
// cached snapshot from one instance describes every instance the
// factory produces).
func Catalog() []CatalogEntry {
	catalogCacheMu.RLock()
	if catalogCacheVal != nil {
		out := cloneCatalog(catalogCacheVal)
		catalogCacheMu.RUnlock()
		return out
	}
	catalogCacheMu.RUnlock()

	catalogCacheMu.Lock()
	defer catalogCacheMu.Unlock()
	if catalogCacheVal != nil { // double-check under write lock
		return cloneCatalog(catalogCacheVal)
	}

	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]CatalogEntry, 0, len(registry))
	for name, factory := range registry {
		instance := factory()
		entry := CatalogEntry{
			Name:         name,
			Capabilities: instance.Capabilities().Normalize(),
		}
		// Plugins that implement SchemaProvider expose per-field
		// metadata for templating. Constraints carry over from the
		// constructor contract above: the throwaway instance is never
		// Shutdown'd, so SchemaProvider implementations must be
		// instance-state-independent and side-effect free (typically
		// just `return pipeline.SchemaOf(myConfig{})`).
		if sp, ok := instance.(pipeline.SchemaProvider); ok {
			entry.Fields = sp.ConfigSchema()
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	catalogCacheVal = out
	return cloneCatalog(out)
}

// cloneCatalog returns a deep copy of in: each CatalogEntry is copied
// and every []string field on its Capabilities is freshly allocated.
// Catalog returns a clone so callers can mutate the slice (sort, filter,
// extend per-entry slices) without tainting the cached snapshot — and
// without that tainted view leaking into future /v1/plugins responses.
func cloneCatalog(in []CatalogEntry) []CatalogEntry {
	if len(in) == 0 {
		return nil
	}
	out := make([]CatalogEntry, len(in))
	for i := range in {
		caps := in[i].Capabilities
		out[i] = CatalogEntry{
			Name: in[i].Name,
			Capabilities: pipeline.PluginCapabilities{
				ReadsBody:   caps.ReadsBody,
				WritesBody:  caps.WritesBody,
				Description: caps.Description,
				Requires:    append([]string(nil), caps.Requires...),
				RequiresAny: append([]string(nil), caps.RequiresAny...),
			},
			Fields: cloneFieldSchemas(in[i].Fields),
		}
	}
	return out
}

// cloneFieldSchemas deep-copies the per-field schema slice. Fields
// itself is a slice, but each FieldSchema also contains slices
// (Enum, Fields for nested structs) that need their own backing
// arrays to avoid aliasing the cached snapshot.
func cloneFieldSchemas(in []pipeline.FieldSchema) []pipeline.FieldSchema {
	if len(in) == 0 {
		return nil
	}
	out := make([]pipeline.FieldSchema, len(in))
	for i, f := range in {
		out[i] = f
		out[i].Enum = append([]string(nil), f.Enum...)
		out[i].Fields = cloneFieldSchemas(f.Fields) // recurse for nested struct fields
	}
	return out
}

// resetCatalogCache clears the memoized Catalog result. Intended for
// tests that register/unregister plugins and need a fresh view.
func resetCatalogCache() {
	catalogCacheMu.Lock()
	catalogCacheVal = nil
	catalogCacheMu.Unlock()
}

// WarmCatalog triggers Catalog() at boot so any plugin whose factory
// violates the constructor contract (panics, allocates goroutines /
// connections / file handles) surfaces during startup rather than
// silently caching a faulty throwaway instance on the first /v1/plugins
// request. Cheap insurance — the result is already memoized, so calling
// this at boot moves "first call" out of the request hot path.
//
// Intended for the binary's main() to invoke once after all plugin
// init() registrations have run (typically right before sessionapi.New).
func WarmCatalog() { _ = Catalog() }

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
	policies := make([]pipeline.ErrorPolicy, 0, len(entries))
	for _, e := range entries {
		// ErrorPolicyOff removes the plugin from the running pipeline
		// entirely — no Configure, no Init, no dispatch. Operators use
		// off as a kill-switch without deleting the entry from YAML,
		// which makes re-enabling a one-line edit.
		if e.OnError.Resolved() == pipeline.ErrorPolicyOff {
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
			// Wrap so the session API can surface the raw config on /v1/pipeline.
			p = pipeline.WrapConfigured(p, e.Config)
		} else if len(e.Config) > 0 {
			return nil, fmt.Errorf("plugin %q does not accept configuration", e.Name)
		}
		ps = append(ps, p)
		policies = append(policies, e.OnError.Resolved())
	}
	if err := validateRelationships(ps); err != nil {
		return nil, err
	}
	opts = append(opts, pipeline.WithPolicies(policies...))
	return pipeline.New(ps, opts...)
}

// BuildWithSPIFFE is Build plus dependency injection of the framework
// SPIFFE Provider. Every plugin satisfying spiffe.ProviderConsumer
// receives p via SetSPIFFEProvider before its Configure is invoked, so
// configuration code can use the provider's sources directly.
//
// Pass nil for p in builds that don't need SPIFFE; in that case the
// behavior is equivalent to Build for non-consuming plugins, and
// consumer plugins are NOT called (their SetSPIFFEProvider is skipped).
//
// Plugins that don't implement ProviderConsumer are unaffected.
func BuildWithSPIFFE(entries []config.PluginEntry, p *spiffe.Provider, opts ...pipeline.Option) (*pipeline.Pipeline, error) {
	ps := make([]pipeline.Plugin, 0, len(entries))
	policies := make([]pipeline.ErrorPolicy, 0, len(entries))
	for _, e := range entries {
		// ErrorPolicyOff removes the plugin from the running pipeline
		// entirely — same kill-switch semantics as Build.
		if e.OnError.Resolved() == pipeline.ErrorPolicyOff {
			continue
		}
		factory, ok := factoryFor(e.Name)
		if !ok {
			return nil, fmt.Errorf("unknown plugin %q (registered: %v)", e.Name, RegisteredPlugins())
		}
		plugin := factory()
		// Inject the framework SPIFFE Provider BEFORE Configure runs so
		// the plugin's Configure logic can reach the provider's sources
		// directly. Skip when no Provider was supplied (nil) — the
		// caller has opted out of SPIFFE for this build.
		if c, ok := plugin.(spiffe.ProviderConsumer); ok && p != nil {
			c.SetSPIFFEProvider(p)
		}
		if c, ok := plugin.(pipeline.Configurable); ok {
			if err := c.Configure(e.Config); err != nil {
				return nil, fmt.Errorf("configure %q: %w", e.Name, err)
			}
			// Wrap so the session API can surface the raw config on /v1/pipeline.
			plugin = pipeline.WrapConfigured(plugin, e.Config)
		} else if len(e.Config) > 0 {
			return nil, fmt.Errorf("plugin %q does not accept configuration", e.Name)
		}
		ps = append(ps, plugin)
		policies = append(policies, e.OnError.Resolved())
	}
	if err := validateRelationships(ps); err != nil {
		return nil, err
	}
	opts = append(opts, pipeline.WithPolicies(policies...))
	return pipeline.New(ps, opts...)
}

// validateRelationships checks every plugin's Requires / RequiresAny
// declarations against the chain it's about to run in. Collects all
// errors across the chain into one joined error rather than
// short-circuiting on the first — friendlier for operators iterating on
// a freshly-edited YAML.
//
// Semantics:
//
//   - Requires: every named plugin must appear at a lower index in
//     the chain. Missing or misordered is an error.
//   - RequiresAny: at least one named plugin must appear at a lower
//     index. Any named plugin that IS present must also be at a
//     lower index.
//
// Each rule uses the per-plugin Name() as the identity key (case-sensitive).
// If a plugin name is duplicated in a chain (rare — requires
// config.PluginEntry.ID differentiation), the earliest-occurrence index
// is authoritative for position checks.
func validateRelationships(ps []pipeline.Plugin) error {
	if len(ps) == 0 {
		return nil
	}
	// Build a name->first-occurrence-index map once.
	positions := make(map[string]int, len(ps))
	for i, p := range ps {
		if _, seen := positions[p.Name()]; !seen {
			positions[p.Name()] = i
		}
	}

	var errs []string

	for i, p := range ps {
		caps := p.Capabilities().Normalize()

		// Requires — hard AND with ordering.
		for _, req := range caps.Requires {
			j, present := positions[req]
			switch {
			case !present:
				errs = append(errs, fmt.Sprintf(
					"plugin %q requires %q earlier in the chain, but %q is not configured",
					p.Name(), req, req))
			case j >= i:
				errs = append(errs, fmt.Sprintf(
					"plugin %q requires %q earlier in the chain, but %q appears at position %d (this plugin is at %d)",
					p.Name(), req, req, j, i))
			}
		}

		// RequiresAny — hard OR with ordering.
		if len(caps.RequiresAny) > 0 {
			anyPresentAndEarlier := false
			for _, req := range caps.RequiresAny {
				j, present := positions[req]
				if !present {
					continue
				}
				if j >= i {
					errs = append(errs, fmt.Sprintf(
						"plugin %q lists %q under RequiresAny; %q must appear earlier (found at position %d, this plugin is at %d)",
						p.Name(), req, req, j, i))
					continue
				}
				anyPresentAndEarlier = true
			}
			if !anyPresentAndEarlier {
				errs = append(errs, fmt.Sprintf(
					"plugin %q requires at least one of %v earlier in the chain, but none are configured",
					p.Name(), caps.RequiresAny))
			}
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("plugin relationship validation failed:\n  - %s", strings.Join(errs, "\n  - "))
}
