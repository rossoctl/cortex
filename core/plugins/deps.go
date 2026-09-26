package plugins

import (
	"fmt"
	"log/slog"
	"sort"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/pricing"
	"github.com/rossoctl/cortex/core/spiffe"
)

// Deps are the process-wide dependencies a pipeline build can inject into plugins.
//
// Plugin factories take no construction arguments (see PluginFactory), so anything
// shared across the process has to arrive this way. A struct rather than a growing
// parameter list: BuildWithSPIFFE existed because SPIFFE needed injecting, and
// adding pricing the same way would have meant a third near-identical builder.
//
// Every field is optional. A zero Deps builds exactly what Build always did.
type Deps struct {
	// SPIFFE is injected into plugins implementing spiffe.ProviderConsumer.
	SPIFFE *spiffe.Provider

	// Pricing is injected into plugins implementing pricing.ResolverConsumer.
	// Long-lived: the reloader rebuilds pipelines but the resolver is swapped in
	// place, so a plugin from any build sees current rates. See pricing.Registry.
	Pricing *pricing.Registry
}

// BuildWithDeps constructs a pipeline from an ordered list of plugin entries,
// injecting each supplied dependency into the plugins that consume it.
//
// For every plugin implementing pipeline.Configurable, Configure is called with the
// entry's Config bytes (nil when omitted). Passing config to a plugin that does not
// implement Configurable is rejected, so stale or misplaced config blocks fail at
// startup instead of being silently ignored. Unknown plugin names fail fast with an
// error listing every registered plugin.
//
// Dependencies are injected BEFORE Configure runs, so configuration code can use
// them. That ordering is the contract, not an implementation detail: a plugin's
// Configure may build rate- or identity-dependent state, and an injection landing
// afterwards would be silently too late.
//
// A nil dependency is NOT injected, rather than injected as nil. Handing a plugin a
// typed-nil interface would make its own `!= nil` guard true while every call
// through it panicked.
//
// This is the single builder. Build and BuildWithSPIFFE delegate here; before this
// they were two copies of the same forty lines, differing only in one injection.
func BuildWithDeps(entries []config.PluginEntry, deps Deps, opts ...pipeline.Option) (*pipeline.Pipeline, error) {
	ps := make([]pipeline.Plugin, 0, len(entries))
	policies := make([]pipeline.ErrorPolicy, 0, len(entries))

	for _, e := range entries {
		// ErrorPolicyOff removes the plugin from the running pipeline entirely — no
		// Configure, no Init, no dispatch. Operators use off as a kill-switch
		// without deleting the entry from YAML, which makes re-enabling a one-line
		// edit.
		if e.OnError.Resolved() == pipeline.ErrorPolicyOff {
			continue
		}
		factory, ok := factoryFor(e.Name)
		if !ok {
			pluginNames := RegisteredPlugins()
			if len(pluginNames) == 0 {
				slog.Warn("No registered plugins -- Build with --tags or use `go run .` to enable")
			}
			return nil, fmt.Errorf("unknown plugin %q (registered: %v)", e.Name, pluginNames)
		}
		p := factory()

		if c, ok := p.(spiffe.ProviderConsumer); ok && deps.SPIFFE != nil {
			c.SetSPIFFEProvider(deps.SPIFFE)
		}
		if c, ok := p.(pricing.ResolverConsumer); ok && deps.Pricing != nil {
			c.SetPricingResolver(deps.Pricing)
		}

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

// PricingConsumerPlugins lists the registered plugins whose instances implement
// pricing.ResolverConsumer — the plugins BuildWithDeps would inject the Registry
// into.
//
// Callers that gate Registry construction on actual need use this to assert their
// need-detection covers every consumer; a new consumer slipping past the predicate
// would otherwise silently receive no resolver and report its traffic as unpriced,
// which looks identical to having no rates configured.
//
// Probes by constructing each plugin and type-asserting, which is safe because
// PluginFactory is contractually a cheap, side-effect-free constructor. Mirrors
// SPIFFEConsumerPlugins; if a factory ever gains construction-time side effects,
// both switch to a static capability tag instead of a live probe.
func PricingConsumerPlugins() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	var names []string
	for name, factory := range registry {
		if _, ok := factory().(pricing.ResolverConsumer); ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
