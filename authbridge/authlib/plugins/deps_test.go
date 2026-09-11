package plugins

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/config"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
)

// pricedPlugin records what was injected and when, so the tests can assert the
// ordering contract (injection BEFORE Configure) rather than only the end state.
type pricedPlugin struct {
	name           string
	resolver       pricing.Resolver
	hadAtConfigure bool
	configured     bool
}

func (p *pricedPlugin) Name() string { return p.name }
func (p *pricedPlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{Description: "test"}
}
func (p *pricedPlugin) OnRequest(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *pricedPlugin) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *pricedPlugin) SetPricingResolver(r pricing.Resolver) { p.resolver = r }
func (p *pricedPlugin) Configure(json.RawMessage) error {
	p.configured = true
	p.hadAtConfigure = p.resolver != nil
	return nil
}

func registerPriced(t *testing.T, name string) *pricedPlugin {
	t.Helper()
	p := &pricedPlugin{name: name}
	RegisterPlugin(name, func() pipeline.Plugin { return p })
	t.Cleanup(func() { UnregisterPlugin(name) })
	return p
}

func entriesFor(names ...string) []config.PluginEntry {
	out := make([]config.PluginEntry, 0, len(names))
	for _, n := range names {
		out = append(out, config.PluginEntry{Name: n, Config: json.RawMessage(`{}`)})
	}
	return out
}

func TestBuildWithDeps_InjectsResolverBeforeConfigure(t *testing.T) {
	// Ordering is the contract: a plugin's Configure may build rate-dependent state,
	// so an injection that landed afterwards would be silently too late.
	p := registerPriced(t, "test-priced-order")
	reg := pricing.NewRegistry(nil)

	if _, err := BuildWithDeps(entriesFor("test-priced-order"), Deps{Pricing: reg}); err != nil {
		t.Fatalf("BuildWithDeps: %v", err)
	}
	if !p.configured {
		t.Fatal("Configure was never called")
	}
	if !p.hadAtConfigure {
		t.Error("resolver was not injected before Configure ran")
	}
	if p.resolver == nil {
		t.Error("resolver was not injected at all")
	}
}

func TestBuildWithDeps_NoResolverLeavesConsumerUncalled(t *testing.T) {
	// A build that opted out of pricing must not inject a typed-nil Resolver: that
	// would make the plugin's `resolver != nil` check true and its calls panic.
	p := registerPriced(t, "test-priced-none")
	if _, err := BuildWithDeps(entriesFor("test-priced-none"), Deps{}); err != nil {
		t.Fatalf("BuildWithDeps: %v", err)
	}
	if p.resolver != nil {
		t.Errorf("resolver = %v, want nil when no Deps.Pricing was supplied", p.resolver)
	}
}

func TestBuild_IsBuildWithDepsWithNoDeps(t *testing.T) {
	// Build and BuildWithSPIFFE had near-identical 40-line bodies. They now
	// delegate, so this asserts the delegation preserved their behaviour.
	p := registerPriced(t, "test-priced-plainbuild")
	if _, err := Build(entriesFor("test-priced-plainbuild")); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !p.configured {
		t.Error("Build did not configure the plugin")
	}
	if p.resolver != nil {
		t.Error("Build injected a resolver")
	}
}

func TestBuildWithSPIFFE_StillConfiguresAndSkipsNilProvider(t *testing.T) {
	p := registerPriced(t, "test-priced-bws")
	if _, err := BuildWithSPIFFE(entriesFor("test-priced-bws"), nil); err != nil {
		t.Fatalf("BuildWithSPIFFE: %v", err)
	}
	if !p.configured {
		t.Error("BuildWithSPIFFE did not configure the plugin")
	}
}

func TestBuildWithDeps_UnknownPluginStillFailsFast(t *testing.T) {
	_, err := BuildWithDeps(entriesFor("test-priced-nonexistent"), Deps{})
	if err == nil {
		t.Fatal("BuildWithDeps accepted an unknown plugin")
	}
	if !strings.Contains(err.Error(), "unknown plugin") {
		t.Errorf("error %q does not say the plugin is unknown", err)
	}
}

func TestPricingConsumerPlugins_FindsRegisteredConsumers(t *testing.T) {
	// The probe exists so a caller that gates Registry construction on actual need
	// can assert its need-detection covers every consumer — a new consumer slipping
	// past the predicate would otherwise silently receive no resolver and report
	// its traffic as unpriced.
	registerPriced(t, "test-priced-probe")
	found := PricingConsumerPlugins()
	var ok bool
	for _, n := range found {
		if n == "test-priced-probe" {
			ok = true
		}
	}
	if !ok {
		t.Errorf("PricingConsumerPlugins() = %v, missing test-priced-probe", found)
	}
}

// inference-parser IS a pricing consumer, and litellm-budget-track is not — the inverse of
// what this test asserted before cortex #972.
//
// The reason it flipped: the component that knows when token counts are FINAL is the one
// that can price them exactly once, and a budget has no business computing the number it
// enforces against. The old arrangement meant a pipeline without the budget plugin showed
// token counts and no money, with the same field silently changing meaning depending on
// configuration.
func TestPricingConsumerPlugins_TracksTheCostOwner(t *testing.T) {
	consumers := PricingConsumerPlugins()
	var hasParser, hasBudget bool
	for _, n := range consumers {
		switch n {
		case "inference-parser":
			hasParser = true
		case "litellm-budget-track":
			hasBudget = true
		}
	}
	if !hasParser {
		t.Errorf("inference-parser is not reported as a pricing consumer, but it owns costing: %v", consumers)
	}
	if hasBudget {
		t.Errorf("litellm-budget-track is reported as a pricing consumer; it bills a settled figure and computes nothing: %v", consumers)
	}
}
