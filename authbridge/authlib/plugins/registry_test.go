package plugins

import (
	"testing"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/config"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// TestBuiltinsRegistered verifies every in-tree plugin is discoverable
// through the new registry — the list is the public contract that
// operator YAML depends on, so a regression here breaks deployments.
func TestBuiltinsRegistered(t *testing.T) {
	want := map[string]bool{
		"jwt-validation":   true,
		"token-exchange":   true,
		"a2a-parser":       true,
		"mcp-parser":       true,
		"inference-parser": true,
	}
	got := RegisteredPlugins()
	gotSet := make(map[string]bool, len(got))
	for _, n := range got {
		gotSet[n] = true
	}
	for name := range want {
		if !gotSet[name] {
			t.Errorf("built-in plugin %q missing from registry; got: %v", name, got)
		}
	}
}

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

// TestRegisterPlugin_ReservedBuiltin_Panics locks the seal on built-in
// gate names. A custom plugin registering "jwt-validation" or
// "token-exchange" would silently replace the shipped auth gates — an
// authentication bypass. RegisterPlugin must refuse reserved names so
// a mistake at import time fails loud instead of at request time.
func TestRegisterPlugin_ReservedBuiltin_Panics(t *testing.T) {
	for _, name := range []string{"jwt-validation", "token-exchange"} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Errorf("expected panic for reserved name %q", name)
					return
				}
				msg, ok := r.(string)
				if !ok {
					t.Errorf("panic value not a string: %T", r)
					return
				}
				if !containsSubstring(msg, "reserved built-in name") {
					t.Errorf("panic message should explain the reservation: %q", msg)
				}
			}()
			RegisterPlugin(name, func() pipeline.Plugin { return nil })
		})
	}
}

// TestIsReservedBuiltin verifies the exported predicate — higher-level
// validators consume it to avoid duplicating the reserved-names list.
func TestIsReservedBuiltin(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"jwt-validation", true},
		{"token-exchange", true},
		{"a2a-parser", false}, // parsers are intentionally not reserved
		{"mcp-parser", false},
		{"custom-guardrail", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsReservedBuiltin(c.name); got != c.want {
			t.Errorf("IsReservedBuiltin(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestBuildChain_RejectsMisplacedBuiltin covers the placement seal:
// jwt-validation must not appear in outbound, token-exchange must not
// appear in inbound. Silent misplacement would disable auth.
func TestBuildChain_RejectsMisplacedBuiltin(t *testing.T) {
	cases := []struct {
		name      string
		direction ChainDirection
		entry     string
		wantChain string // chain keyword that should appear in the error
	}{
		{"jwt-validation-on-outbound", ChainOutbound, "jwt-validation", "inbound"},
		{"token-exchange-on-inbound", ChainInbound, "token-exchange", "outbound"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := BuildChain(c.direction, []config.PluginEntry{{Name: c.entry}})
			if err == nil {
				t.Fatalf("expected error for %s on %v chain", c.entry, c.direction)
			}
			if !containsSubstring(err.Error(), c.entry) {
				t.Errorf("error should name the misplaced plugin: %q", err)
			}
			if !containsSubstring(err.Error(), c.wantChain) {
				t.Errorf("error should mention the expected chain %q: %q", c.wantChain, err)
			}
		})
	}
}

// TestBuildChain_AcceptsCorrectPlacement is the positive counterpart —
// built-ins on their expected chain must pass placement validation.
// (The plugin's own Configure may still fail because no config is
// provided; we only care that the placement check didn't trip.)
func TestBuildChain_AcceptsCorrectPlacement(t *testing.T) {
	cases := []struct {
		direction ChainDirection
		entry     string
	}{
		{ChainInbound, "jwt-validation"},
		{ChainOutbound, "token-exchange"},
	}
	for _, c := range cases {
		t.Run(c.entry, func(t *testing.T) {
			_, err := BuildChain(c.direction, []config.PluginEntry{{Name: c.entry}})
			if err != nil && containsSubstring(err.Error(), "reserved built-in") {
				t.Errorf("placement check wrongly rejected %s on its own chain: %q", c.entry, err)
			}
		})
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
