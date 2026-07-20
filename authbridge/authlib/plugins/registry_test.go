package plugins

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/config"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/spiffe"
)

// TestRegisterPlugin_DoubleRegistration_Panics locks the strict-fail
// policy. Silent last-write-wins would let a deployment with two
// incompatible copies of the same plugin corrupt the pipeline
// composition; panic on registration catches it at process start.
func TestRegisterPlugin_DoubleRegistration_Panics(t *testing.T) {
	name := "test-double-register"
	RegisterPlugin(name, func() pipeline.Plugin { return nil })
	t.Cleanup(func() { UnregisterPlugin(name) })

	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on double-registration")
		}
	}()
	RegisterPlugin(name, func() pipeline.Plugin { return nil })
}

func TestRegisterPlugin_EmptyName_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on empty name")
		}
	}()
	RegisterPlugin("", func() pipeline.Plugin { return nil })
}

func TestRegisterPlugin_NilFactory_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on nil factory")
		}
	}()
	RegisterPlugin("test-nil-factory", nil)
}

// TestUnregisterPlugin verifies the test-isolation helper. After
// registering + unregistering, the name is absent from RegisteredPlugins
// and Build rejects it as unknown.
func TestUnregisterPlugin(t *testing.T) {
	name := "test-unregister"
	RegisterPlugin(name, func() pipeline.Plugin { return nil })
	if !contains(RegisteredPlugins(), name) {
		t.Fatalf("plugin not registered after RegisterPlugin")
	}
	if !UnregisterPlugin(name) {
		t.Errorf("UnregisterPlugin returned false for a registered name")
	}
	if contains(RegisteredPlugins(), name) {
		t.Errorf("plugin still in registry after UnregisterPlugin")
	}
	// Second unregister should be a no-op (returns false).
	if UnregisterPlugin(name) {
		t.Errorf("UnregisterPlugin returned true for an unregistered name")
	}
}

// TestBuild_UnknownPlugin_ListsRegistered verifies the "unknown plugin"
// error includes the list of registered names so operators get a
// typo-catching diagnostic instead of a generic not-found.
func TestBuild_UnknownPlugin_ListsRegistered(t *testing.T) {
	_, err := Build([]config.PluginEntry{{Name: "not-a-real-plugin"}})
	if err == nil {
		t.Fatalf("expected error for unknown plugin")
	}
	msg := err.Error()
	if !containsSubstring(msg, "not-a-real-plugin") {
		t.Errorf("error should name the unknown plugin: %q", msg)
	}
	if !containsSubstring(msg, "jwt-validation") {
		t.Errorf("error should list registered plugins (for typo diagnostics): %q", msg)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func containsSubstring(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// --- Plugin relationship validation tests ---

// relPlugin is a minimal pipeline.Plugin with configurable Capabilities
// used to drive validateRelationships through its branches. Lives here
// rather than in plugintesting because it's relationship-specific — no
// body / identity / dispatch behavior to share with other tests.
type relPlugin struct {
	name string
	caps pipeline.PluginCapabilities
}

func (p *relPlugin) Name() string                              { return p.name }
func (p *relPlugin) Capabilities() pipeline.PluginCapabilities { return p.caps }
func (p *relPlugin) OnRequest(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *relPlugin) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// mkRelPlugin is the one-liner builder used across the table-driven
// tests below. Name is required; caps drives whichever relationship
// rule the test is exercising.
func mkRelPlugin(name string, caps pipeline.PluginCapabilities) *relPlugin {
	return &relPlugin{name: name, caps: caps}
}

// TestValidateRelationships_Requires exercises the hard dependency
// rule: missing named plugin → error; named plugin present but later
// → error; named plugin present and earlier → ok.
func TestValidateRelationships_Requires(t *testing.T) {
	tests := []struct {
		name      string
		plugins   []pipeline.Plugin
		wantErr   bool
		wantInMsg []string
	}{
		{
			name: "required plugin present and earlier — ok",
			plugins: []pipeline.Plugin{
				mkRelPlugin("mcp-parser", pipeline.PluginCapabilities{}),
				mkRelPlugin("tool-allowlist", pipeline.PluginCapabilities{
					Requires: []string{"mcp-parser"},
				}),
			},
			wantErr: false,
		},
		{
			name: "required plugin missing — error names missing plugin",
			plugins: []pipeline.Plugin{
				mkRelPlugin("tool-allowlist", pipeline.PluginCapabilities{
					Requires: []string{"mcp-parser"},
				}),
			},
			wantErr:   true,
			wantInMsg: []string{"tool-allowlist", "requires", "mcp-parser", "not configured"},
		},
		{
			name: "required plugin present but later — error shows positions",
			plugins: []pipeline.Plugin{
				mkRelPlugin("tool-allowlist", pipeline.PluginCapabilities{
					Requires: []string{"mcp-parser"},
				}),
				mkRelPlugin("mcp-parser", pipeline.PluginCapabilities{}),
			},
			wantErr:   true,
			wantInMsg: []string{"tool-allowlist", "mcp-parser", "position 1", "this plugin is at 0"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRelationships(tc.plugins)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if err != nil {
				msg := err.Error()
				for _, want := range tc.wantInMsg {
					if !containsSubstring(msg, want) {
						t.Errorf("error %q missing %q", msg, want)
					}
				}
			}
		})
	}
}

// TestValidateRelationships_RequiresAny covers the OR-dependency:
// at least one of the named plugins must be present + earlier.
func TestValidateRelationships_RequiresAny(t *testing.T) {
	tests := []struct {
		name      string
		plugins   []pipeline.Plugin
		wantErr   bool
		wantInMsg []string
	}{
		{
			name: "one of the alternatives present earlier — ok",
			plugins: []pipeline.Plugin{
				mkRelPlugin("a2a-parser", pipeline.PluginCapabilities{}),
				mkRelPlugin("pii-scrubber", pipeline.PluginCapabilities{
					RequiresAny: []string{"a2a-parser", "mcp-parser", "inference-parser"},
				}),
			},
			wantErr: false,
		},
		{
			name: "multiple alternatives present earlier — ok",
			plugins: []pipeline.Plugin{
				mkRelPlugin("a2a-parser", pipeline.PluginCapabilities{}),
				mkRelPlugin("mcp-parser", pipeline.PluginCapabilities{}),
				mkRelPlugin("pii-scrubber", pipeline.PluginCapabilities{
					RequiresAny: []string{"a2a-parser", "mcp-parser", "inference-parser"},
				}),
			},
			wantErr: false,
		},
		{
			name: "no alternatives present — error names the set",
			plugins: []pipeline.Plugin{
				mkRelPlugin("pii-scrubber", pipeline.PluginCapabilities{
					RequiresAny: []string{"a2a-parser", "mcp-parser", "inference-parser"},
				}),
			},
			wantErr:   true,
			wantInMsg: []string{"pii-scrubber", "at least one", "none are configured"},
		},
		{
			name: "alternative present but later — error per-offender",
			plugins: []pipeline.Plugin{
				mkRelPlugin("pii-scrubber", pipeline.PluginCapabilities{
					RequiresAny: []string{"a2a-parser", "mcp-parser"},
				}),
				mkRelPlugin("a2a-parser", pipeline.PluginCapabilities{}),
			},
			wantErr:   true,
			wantInMsg: []string{"pii-scrubber", "RequiresAny", "a2a-parser", "must appear earlier"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRelationships(tc.plugins)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if err != nil {
				msg := err.Error()
				for _, want := range tc.wantInMsg {
					if !containsSubstring(msg, want) {
						t.Errorf("error %q missing %q", msg, want)
					}
				}
			}
		})
	}
}

// TestValidateRelationships_CollectsAllErrors verifies the collector
// policy: a chain with multiple problems reports them all in one
// error, rather than short-circuiting on the first. Operators iterate
// on a single YAML fix rather than a sequence of startups.
func TestValidateRelationships_CollectsAllErrors(t *testing.T) {
	plugins := []pipeline.Plugin{
		mkRelPlugin("a-requires-missing", pipeline.PluginCapabilities{
			Requires: []string{"does-not-exist"},
		}),
		mkRelPlugin("b-requires-any-missing", pipeline.PluginCapabilities{
			RequiresAny: []string{"also-missing"},
		}),
	}
	err := validateRelationships(plugins)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{
		"a-requires-missing",
		"does-not-exist",
		"b-requires-any-missing",
		"also-missing",
	} {
		if !containsSubstring(msg, want) {
			t.Errorf("error message should mention %q: %s", want, msg)
		}
	}
}

// TestValidateRelationships_EmptyChain is a safety check that no-plugin
// chains don't panic or error — the check is vacuously true.
func TestValidateRelationships_EmptyChain(t *testing.T) {
	if err := validateRelationships(nil); err != nil {
		t.Errorf("empty chain should not error, got: %v", err)
	}
	if err := validateRelationships([]pipeline.Plugin{}); err != nil {
		t.Errorf("empty chain should not error, got: %v", err)
	}
}

// --- BuildWithSPIFFE tests ---

// consumerPlugin is a test double implementing pipeline.Plugin AND
// spiffe.ProviderConsumer. It records the Provider it was injected with
// for assertion.
type consumerPlugin struct {
	name        string
	setProvider *spiffe.Provider
}

func (p *consumerPlugin) Name() string { return p.name }
func (p *consumerPlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (p *consumerPlugin) OnRequest(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *consumerPlugin) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *consumerPlugin) SetSPIFFEProvider(prov *spiffe.Provider) { p.setProvider = prov }

// nonConsumerPlugin is a plugin that does NOT implement ProviderConsumer.
// Used to assert BuildWithSPIFFE leaves it untouched.
type nonConsumerPlugin struct {
	name string
}

func (p *nonConsumerPlugin) Name() string { return p.name }
func (p *nonConsumerPlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (p *nonConsumerPlugin) OnRequest(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *nonConsumerPlugin) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

func TestBuildWithSPIFFE_InjectsProviderIntoConsumer(t *testing.T) {
	fake := &consumerPlugin{name: "test-spiffe-consumer-1"}
	RegisterPlugin("test-spiffe-consumer-1", func() pipeline.Plugin { return fake })
	t.Cleanup(func() { UnregisterPlugin("test-spiffe-consumer-1") })

	prov := &spiffe.Provider{} // OK to use zero-value here for identity test
	_, err := BuildWithSPIFFE([]config.PluginEntry{{Name: "test-spiffe-consumer-1"}}, prov)
	if err != nil {
		t.Fatalf("BuildWithSPIFFE: %v", err)
	}
	if fake.setProvider != prov {
		t.Errorf("provider not injected; got %v, want %v", fake.setProvider, prov)
	}
}

func TestBuildWithSPIFFE_NilProvider_NoCallToSetSPIFFEProvider(t *testing.T) {
	fake := &consumerPlugin{name: "test-spiffe-consumer-2"}
	RegisterPlugin("test-spiffe-consumer-2", func() pipeline.Plugin { return fake })
	t.Cleanup(func() { UnregisterPlugin("test-spiffe-consumer-2") })

	_, err := BuildWithSPIFFE([]config.PluginEntry{{Name: "test-spiffe-consumer-2"}}, nil)
	if err != nil {
		t.Fatalf("BuildWithSPIFFE: %v", err)
	}
	if fake.setProvider != nil {
		t.Errorf("provider should be nil for nil-provider call, got %v", fake.setProvider)
	}
}

func TestBuildWithSPIFFE_NonConsumerPlugin_Unaffected(t *testing.T) {
	// A plugin that does NOT implement ProviderConsumer should still
	// build cleanly when BuildWithSPIFFE is called with a non-nil provider.
	RegisterPlugin("test-non-consumer", func() pipeline.Plugin {
		return &nonConsumerPlugin{name: "test-non-consumer"}
	})
	t.Cleanup(func() { UnregisterPlugin("test-non-consumer") })

	prov := &spiffe.Provider{}
	_, err := BuildWithSPIFFE([]config.PluginEntry{{Name: "test-non-consumer"}}, prov)
	if err != nil {
		t.Fatalf("BuildWithSPIFFE: %v", err)
	}
}

// cfgPlugin is a Configurable wrapper around relPlugin used to verify
// that the registry wraps Configurable plugins in pipeline.WrapConfigured
// and that non-Configurable plugins (relPlugin alone) are NOT wrapped.
type cfgPlugin struct {
	relPlugin
	configured json.RawMessage
}

func (c *cfgPlugin) Configure(raw json.RawMessage) error {
	c.configured = raw
	return nil
}

// TestRegistryWrapsConfigurablePluginsForRawConfig verifies that plugins
// built through Build expose their raw config bytes via type-assertion
// to pipeline.RawConfigProvider. This is the contract
// /v1/pipeline relies on.
func TestRegistryWrapsConfigurablePluginsForRawConfig(t *testing.T) {
	// Register a Configurable plugin and a non-Configurable plugin under
	// throwaway names so this test doesn't fight the global registry.
	cfgName := "rawcfg-test-configurable"
	relName := "rawcfg-test-relational"
	RegisterPlugin(cfgName, func() pipeline.Plugin {
		return &cfgPlugin{relPlugin: relPlugin{name: cfgName}}
	})
	defer UnregisterPlugin(cfgName)
	RegisterPlugin(relName, func() pipeline.Plugin {
		return &relPlugin{name: relName}
	})
	defer UnregisterPlugin(relName)

	configRaw := json.RawMessage(`{"hello":"world"}`)
	pipe, err := Build([]config.PluginEntry{
		{Name: cfgName, Config: configRaw},
		{Name: relName},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	plugins := pipe.Plugins()
	if len(plugins) != 2 {
		t.Fatalf("want 2 plugins, got %d", len(plugins))
	}

	// First plugin (Configurable) should expose RawConfig().
	rc, ok := plugins[0].(pipeline.RawConfigProvider)
	if !ok {
		t.Fatal("Configurable plugin should be wrapped (RawConfig type-assert)")
	}
	if string(rc.RawConfig()) != `{"hello":"world"}` {
		t.Fatalf("RawConfig: got %q want %q", string(rc.RawConfig()), `{"hello":"world"}`)
	}
	// Plugin's Name() still works through the wrapper.
	if plugins[0].Name() != cfgName {
		t.Fatalf("Name through wrapper: %q", plugins[0].Name())
	}

	// Second plugin (non-Configurable) must NOT be wrapped.
	_, ok = plugins[1].(pipeline.RawConfigProvider)
	if ok {
		t.Fatal("non-Configurable plugin should NOT be wrapped")
	}
}

// TestBuildWithSPIFFEWrapsConfigurablePluginsForRawConfig is the parity test
// for BuildWithSPIFFE — the second registry entry point shares the wrap site
// with Build. Lock both paths so the wrapping invariant doesn't drift.
func TestBuildWithSPIFFEWrapsConfigurablePluginsForRawConfig(t *testing.T) {
	cfgName := "rawcfg-spiffe-test-configurable"
	relName := "rawcfg-spiffe-test-relational"
	RegisterPlugin(cfgName, func() pipeline.Plugin {
		return &cfgPlugin{relPlugin: relPlugin{name: cfgName}}
	})
	defer UnregisterPlugin(cfgName)
	RegisterPlugin(relName, func() pipeline.Plugin {
		return &relPlugin{name: relName}
	})
	defer UnregisterPlugin(relName)

	configRaw := json.RawMessage(`{"hello":"world"}`)
	// nil provider is valid for plugins that don't implement
	// spiffe.ProviderConsumer (BuildWithSPIFFE skips SetSPIFFEProvider in
	// that case).
	pipe, err := BuildWithSPIFFE([]config.PluginEntry{
		{Name: cfgName, Config: configRaw},
		{Name: relName},
	}, nil)
	if err != nil {
		t.Fatalf("BuildWithSPIFFE: %v", err)
	}
	plugins := pipe.Plugins()
	if len(plugins) != 2 {
		t.Fatalf("want 2 plugins, got %d", len(plugins))
	}

	rc, ok := plugins[0].(pipeline.RawConfigProvider)
	if !ok {
		t.Fatal("Configurable plugin should be wrapped (RawConfig type-assert)")
	}
	if string(rc.RawConfig()) != `{"hello":"world"}` {
		t.Fatalf("RawConfig: got %q want %q", string(rc.RawConfig()), `{"hello":"world"}`)
	}
	if plugins[0].Name() != cfgName {
		t.Fatalf("Name through wrapper: %q", plugins[0].Name())
	}
	_, ok = plugins[1].(pipeline.RawConfigProvider)
	if ok {
		t.Fatal("non-Configurable plugin should NOT be wrapped")
	}
}

// TestCatalog_IncludesRegisteredPlugins verifies Catalog() walks the
// registry, calls each factory once, and returns sorted CatalogEntries
// carrying the static capabilities each plugin advertises.
func TestCatalog_IncludesRegisteredPlugins(t *testing.T) {
	resetCatalogCache()
	t.Cleanup(resetCatalogCache)
	const a, b = "test-catalog-a", "test-catalog-b"
	RegisterPlugin(a, func() pipeline.Plugin {
		return &relPlugin{
			name: a,
			caps: pipeline.PluginCapabilities{
				Description: "A plugin",
			},
		}
	})
	t.Cleanup(func() { UnregisterPlugin(a) })
	RegisterPlugin(b, func() pipeline.Plugin {
		return &relPlugin{
			name: b,
			caps: pipeline.PluginCapabilities{
				Description: "B plugin",
				Requires:    []string{a},
			},
		}
	})
	t.Cleanup(func() { UnregisterPlugin(b) })

	entries := Catalog()
	got := map[string]pipeline.PluginCapabilities{}
	for _, e := range entries {
		got[e.Name] = e.Capabilities
	}

	if got[a].Description != "A plugin" {
		t.Fatalf("Catalog missing %s with Description: %+v", a, got[a])
	}
	if got[b].Description != "B plugin" {
		t.Fatalf("Catalog missing %s with Description: %+v", b, got[b])
	}
	if len(got[b].Requires) != 1 || got[b].Requires[0] != a {
		t.Fatalf("Catalog %s.Requires lost: %+v", b, got[b].Requires)
	}

	// Sorted order: walk entries and confirm a < b appear in that order
	// (relative position only — other tests may register plugins).
	idxA, idxB := -1, -1
	for i, e := range entries {
		if e.Name == a {
			idxA = i
		}
		if e.Name == b {
			idxB = i
		}
	}
	if idxA == -1 || idxB == -1 || idxA > idxB {
		t.Fatalf("Catalog not sorted: a@%d b@%d", idxA, idxB)
	}
}

// TestCatalog_FactoryInvariant locks the load-bearing claim that every
// plugin's factory produces instances with byte-identical Capabilities().
// Catalog() reads metadata once (from the first instance) and caches the
// result, so a future plugin that varies Capabilities() based on instance
// state would silently produce wrong catalog entries.
//
// Walks every registered plugin, calls its factory twice, and asserts the
// two Capabilities() return values are equal. Failure points to a plugin
// that needs to either (a) make its Capabilities() purely static, or
// (b) split into multiple registered names per behavioral variant.
func TestCatalog_FactoryInvariant(t *testing.T) {
	registryMu.RLock()
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	registryMu.RUnlock()

	for _, name := range names {
		name := name
		t.Run(name, func(t *testing.T) {
			factory, ok := factoryFor(name)
			if !ok {
				t.Fatalf("factory missing")
			}
			capsA := factory().Capabilities().Normalize()
			capsB := factory().Capabilities().Normalize()
			if !reflect.DeepEqual(capsA, capsB) {
				t.Fatalf("Capabilities() differs across factory instances:\n  A: %+v\n  B: %+v",
					capsA, capsB)
			}
		})
	}
}

// TestCatalog_ReturnsDefensiveCopy verifies callers can mutate the
// returned slice (and its nested capability slices) without tainting
// the cached snapshot or future Catalog() reads.
func TestCatalog_ReturnsDefensiveCopy(t *testing.T) {
	resetCatalogCache()
	t.Cleanup(resetCatalogCache)
	const name = "test-defensive-copy"
	RegisterPlugin(name, func() pipeline.Plugin {
		return &relPlugin{name: name, caps: pipeline.PluginCapabilities{
			Requires: []string{"dep-a"},
		}}
	})
	t.Cleanup(func() { UnregisterPlugin(name) })

	first := Catalog()
	// Find our entry.
	var idx int = -1
	for i, e := range first {
		if e.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("seeded plugin missing from Catalog")
	}

	// Mutate the returned slice and the nested Requires slice.
	first[idx].Capabilities.Requires[0] = "MUTATED"
	first[idx].Capabilities.Description = "MUTATED"

	// A second call must still see the original values.
	second := Catalog()
	var idx2 int = -1
	for i, e := range second {
		if e.Name == name {
			idx2 = i
			break
		}
	}
	if idx2 < 0 {
		t.Fatal("seeded plugin missing from second Catalog call")
	}
	if got := second[idx2].Capabilities.Requires[0]; got != "dep-a" {
		t.Errorf("cache tainted by caller mutation: Requires[0] = %q, want dep-a", got)
	}
	if got := second[idx2].Capabilities.Description; got != "" {
		t.Errorf("cache tainted by caller mutation: Description = %q, want empty", got)
	}
}
