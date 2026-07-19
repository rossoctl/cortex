package plugins_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/rossoctl/rossocortex/authbridge/authlib/config"
	"github.com/rossoctl/rossocortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/rossocortex/authbridge/authlib/plugins"
	// Side-effect imports register the bundled plugins. Same pattern
	// main.go uses — each plugin lives in its own subpackage and
	// advertises itself via init(); importing here makes the name
	// resolvable to plugins.Build in these tests.
	_ "github.com/rossoctl/rossocortex/authbridge/authlib/plugins/a2aparser"
	_ "github.com/rossoctl/rossocortex/authbridge/authlib/plugins/inferenceparser"
	jwtvalidation "github.com/rossoctl/rossocortex/authbridge/authlib/plugins/jwtvalidation"
	_ "github.com/rossoctl/rossocortex/authbridge/authlib/plugins/mcpparser"
	_ "github.com/rossoctl/rossocortex/authbridge/authlib/plugins/opa"
	_ "github.com/rossoctl/rossocortex/authbridge/authlib/plugins/tokenbroker"
	tokenexchange "github.com/rossoctl/rossocortex/authbridge/authlib/plugins/tokenexchange"
)

// TestBuiltinsRegistered verifies every in-tree plugin is discoverable
// through the registry after its side-effect import. Lives in the
// external test package because importing the plugin subpackages from
// inside the plugins package would cycle (plugin subpackages import
// plugins for RegisterPlugin). The side-effect imports at the top of
// this file drive what's present in the registry during the test run.
func TestBuiltinsRegistered(t *testing.T) {
	want := map[string]bool{
		"jwt-validation":   true,
		"token-exchange":   true,
		"token-broker":     true,
		"a2a-parser":       true,
		"mcp-parser":       true,
		"inference-parser": true,
		"opa":              true,
	}
	got := plugins.RegisteredPlugins()
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

// --- Stats aggregation ---

func TestCollectStats_CollectsOnlyStatsSources(t *testing.T) {
	jwt := jwtvalidation.NewJWTValidation()
	if err := jwt.Configure([]byte(`{"issuer":"http://ex","audience":"a"}`)); err != nil {
		t.Fatalf("jwt Configure: %v", err)
	}
	tok := tokenexchange.NewTokenExchange()
	if err := tok.Configure([]byte(`{"token_url":"http://t","identity":{"type":"client-secret","client_id":"c","client_secret":"s"}}`)); err != nil {
		t.Fatalf("tok Configure: %v", err)
	}
	// Need at least one non-StatsSource plugin to prove the filter works;
	// Build a pipeline with a2a-parser (registered by side-effect import
	// of plugins package's self-registering parsers).
	entries := []config.PluginEntry{
		{Name: "a2a-parser"},
	}
	withParser, err := plugins.Build(entries)
	if err != nil {
		t.Fatalf("Build(a2a-parser): %v", err)
	}
	// Stitch jwt + a2a-parser + tok into a test pipeline via pipeline.New
	// directly (bypassing the registry for this artificial combo).
	p, err := pipeline.New(append([]pipeline.Plugin{jwt}, append(withParser.Plugins(), tok)...))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := plugins.CollectStats(p)
	if len(got) != 2 {
		t.Errorf("len(CollectStats) = %d, want 2 (jwt + tok, parser skipped)", len(got))
	}
}

func TestCollectStats_NilPipeline(t *testing.T) {
	if got := plugins.CollectStats(nil); got != nil {
		t.Errorf("CollectStats(nil) = %v, want nil", got)
	}
}

// --- Registry / Build ---

func TestBuild_ValidNames(t *testing.T) {
	p, err := plugins.Build([]config.PluginEntry{
		{Name: "a2a-parser"},
		{Name: "mcp-parser"},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil pipeline")
	}
}

func TestBuild_UnknownName(t *testing.T) {
	if _, err := plugins.Build([]config.PluginEntry{{Name: "nonexistent-plugin"}}); err == nil {
		t.Fatal("expected error for unknown plugin name")
	}
}

func TestBuild_EmptyList(t *testing.T) {
	p, err := plugins.Build([]config.PluginEntry{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	action := p.Run(context.Background(), &pipeline.Context{Headers: http.Header{}})
	if action.Type != pipeline.Continue {
		t.Errorf("empty pipeline got %v, want Continue", action.Type)
	}
}

func TestBuild_ConfigForNonConfigurablePlugin(t *testing.T) {
	_, err := plugins.Build([]config.PluginEntry{
		{Name: "a2a-parser", Config: []byte(`{"unused":true}`)},
	})
	if err == nil {
		t.Fatal("expected error for config on non-Configurable plugin")
	}
	if !strings.Contains(err.Error(), "does not accept configuration") {
		t.Errorf("error %q does not match contract", err)
	}
}

func TestBuild_ConfigureError(t *testing.T) {
	_, err := plugins.Build([]config.PluginEntry{
		{Name: "jwt-validation", Config: []byte(`{}`)},
	})
	if err == nil {
		t.Fatal("expected error for invalid jwt-validation config")
	}
	if !strings.Contains(err.Error(), "jwt-validation") {
		t.Errorf("error %q does not name the offending plugin", err)
	}
}

