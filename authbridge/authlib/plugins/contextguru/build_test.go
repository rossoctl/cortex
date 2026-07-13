package contextguru

import (
	"encoding/json"
	"testing"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/config"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins"

	// Register inference-parser so context-guru's RequiresAny is satisfiable.
	_ "github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/inferenceparser"
)

// TestBuild_InChainAfterInferenceParser confirms the plugin assembles on the
// outbound chain when a parser precedes it (RequiresAny + the single-WritesBody
// slot are accepted together).
func TestBuild_InChainAfterInferenceParser(t *testing.T) {
	p, err := plugins.Build([]config.PluginEntry{
		{Name: "inference-parser"},
		{Name: "context-guru", Config: json.RawMessage(collapseEngine)},
	})
	if err != nil {
		t.Fatalf("pipeline should build with [inference-parser, context-guru]: %v", err)
	}
	if got := len(p.Plugins()); got != 2 {
		t.Fatalf("expected 2 plugins, got %d", got)
	}
}

// TestBuild_FailsWhenBeforeParser confirms RequiresAny ordering is enforced:
// context-guru must appear after the parser it depends on.
func TestBuild_FailsWhenBeforeParser(t *testing.T) {
	if _, err := plugins.Build([]config.PluginEntry{
		{Name: "context-guru", Config: json.RawMessage(`{}`)},
		{Name: "inference-parser"},
	}); err == nil {
		t.Fatal("expected build failure: context-guru requires inference-parser earlier in the chain")
	}
}
