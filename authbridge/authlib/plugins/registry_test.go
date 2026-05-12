package plugins

import (
	"context"
	"testing"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/config"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
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

// TestValidateRelationships_After covers the soft-ordering rule:
// silent when named plugin is absent, error when present but later.
func TestValidateRelationships_After(t *testing.T) {
	tests := []struct {
		name      string
		plugins   []pipeline.Plugin
		wantErr   bool
		wantInMsg []string
	}{
		{
			name: "named plugin absent — no constraint",
			plugins: []pipeline.Plugin{
				mkRelPlugin("request-counter", pipeline.PluginCapabilities{
					After: []string{"mcp-parser"},
				}),
			},
			wantErr: false,
		},
		{
			name: "named plugin present earlier — ok",
			plugins: []pipeline.Plugin{
				mkRelPlugin("mcp-parser", pipeline.PluginCapabilities{}),
				mkRelPlugin("request-counter", pipeline.PluginCapabilities{
					After: []string{"mcp-parser"},
				}),
			},
			wantErr: false,
		},
		{
			name: "named plugin present but later — error says reorder",
			plugins: []pipeline.Plugin{
				mkRelPlugin("request-counter", pipeline.PluginCapabilities{
					After: []string{"mcp-parser"},
				}),
				mkRelPlugin("mcp-parser", pipeline.PluginCapabilities{}),
			},
			wantErr:   true,
			wantInMsg: []string{"request-counter", "After", "mcp-parser", "reorder"},
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

// TestValidateRelationships_Claims covers the mutual-exclusion rule:
// exactly one plugin per claim string per chain.
func TestValidateRelationships_Claims(t *testing.T) {
	tests := []struct {
		name      string
		plugins   []pipeline.Plugin
		wantErr   bool
		wantInMsg []string
	}{
		{
			name: "single claimant — ok",
			plugins: []pipeline.Plugin{
				mkRelPlugin("token-exchange", pipeline.PluginCapabilities{
					Claims: []string{"authorization_header"},
				}),
			},
			wantErr: false,
		},
		{
			name: "distinct claims on different plugins — ok",
			plugins: []pipeline.Plugin{
				mkRelPlugin("token-exchange", pipeline.PluginCapabilities{
					Claims: []string{"authorization_header"},
				}),
				mkRelPlugin("jwt-validation", pipeline.PluginCapabilities{
					Claims: []string{"identity_resolution"},
				}),
			},
			wantErr: false,
		},
		{
			name: "two plugins claim the same string — error names both",
			plugins: []pipeline.Plugin{
				mkRelPlugin("token-exchange", pipeline.PluginCapabilities{
					Claims: []string{"authorization_header"},
				}),
				mkRelPlugin("token-broker", pipeline.PluginCapabilities{
					Claims: []string{"authorization_header"},
				}),
			},
			wantErr:   true,
			wantInMsg: []string{"token-exchange", "token-broker", "authorization_header", "configure only one"},
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
		mkRelPlugin("a-claims-x", pipeline.PluginCapabilities{
			Claims: []string{"x"},
		}),
		mkRelPlugin("b-claims-x", pipeline.PluginCapabilities{
			Claims: []string{"x"}, // conflicts with a-claims-x
		}),
		mkRelPlugin("c-requires-missing", pipeline.PluginCapabilities{
			Requires: []string{"does-not-exist"},
		}),
	}
	err := validateRelationships(plugins)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{
		"a-claims-x",
		"b-claims-x",
		"c-requires-missing",
		"does-not-exist",
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
