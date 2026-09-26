package plugins

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/pipeline"
)

// noopNamed and laterNeeds are minimal plugins: one plain, one declaring a
// response-pass dependency on a plugin that must sit after it.
type noopNamed struct{ name string }

func (p *noopNamed) Name() string { return p.name }
func (p *noopNamed) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{Description: "test"}
}
func (p *noopNamed) OnRequest(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *noopNamed) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

type laterNeeds struct {
	name  string
	needs string
}

func (p *laterNeeds) Name() string { return p.name }
func (p *laterNeeds) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{RequiresLater: []string{p.needs}, Description: "test"}
}
func (p *laterNeeds) OnRequest(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *laterNeeds) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *laterNeeds) Configure(json.RawMessage) error { return nil }

// TestRequiresLater guards the ordering rule that a plain Requires gets exactly
// backwards.
//
// litellm-budget-track prices what inference-parser publishes while folding
// response frames, and the response pass walks the chain in REVERSE. So the parser
// must be LATER in the chain. Before RequiresLater existed, the wrong order built
// cleanly and silently unpriced every streamed response — the counts were simply
// not there yet when the terminal frame arrived.
func TestRequiresLater(t *testing.T) {
	RegisterPlugin("test-later-dep", func() pipeline.Plugin { return &noopNamed{"test-later-dep"} })
	t.Cleanup(func() { UnregisterPlugin("test-later-dep") })
	RegisterPlugin("test-later-consumer", func() pipeline.Plugin {
		return &laterNeeds{name: "test-later-consumer", needs: "test-later-dep"}
	})
	t.Cleanup(func() { UnregisterPlugin("test-later-consumer") })

	// No Config: noopNamed is not Configurable, and Build rightly rejects config
	// aimed at a plugin that cannot take it.
	entry := func(n string) config.PluginEntry {
		return config.PluginEntry{Name: n}
	}

	t.Run("dependency later is accepted", func(t *testing.T) {
		if _, err := Build([]config.PluginEntry{
			entry("test-later-consumer"), entry("test-later-dep"),
		}); err != nil {
			t.Fatalf("correct order rejected: %v", err)
		}
	})

	t.Run("dependency earlier is rejected", func(t *testing.T) {
		_, err := Build([]config.PluginEntry{
			entry("test-later-dep"), entry("test-later-consumer"),
		})
		if err == nil {
			t.Fatal("the reversed order built cleanly — this is the silent misconfiguration RequiresLater exists to stop")
		}
		if !strings.Contains(err.Error(), "later in the chain") {
			t.Errorf("error %q does not explain the ordering requirement", err)
		}
	})

	t.Run("dependency absent is rejected", func(t *testing.T) {
		_, err := Build([]config.PluginEntry{entry("test-later-consumer")})
		if err == nil {
			t.Fatal("a missing dependency built cleanly")
		}
		if !strings.Contains(err.Error(), "not configured") {
			t.Errorf("error %q does not say the dependency is missing", err)
		}
	})
}
