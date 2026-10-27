//go:build include_plugin_script && include_plugin_toolprune && include_plugin_inferenceparser && include_plugin_mcpparser && include_plugin_a2aparser

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/listener/forwardproxy"
	"github.com/rossoctl/cortex/core/plugins"
)

// The shipped local config, with the script slot appended, builds whatever hooks
// the dropped-in script declares. The slot sits after tool-prune, a request
// writer, so a script that only reads responses must not count as a request reader.
func TestLocalChain_BuildsWithEachKindOfScript(t *testing.T) {
	for name, handshake := range map[string]string{
		"response reader": `{"cortex":1,"hooks":["response"]}`,
		"response writer": `{"cortex":1,"hooks":["response"],"rewrite_response":true}`,
		"request script":  `{"cortex":1,"hooks":["request"]}`,
		"both hooks":      `{"cortex":1,"hooks":["request","response"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("SHELL", "")
			dir := filepath.Join(home, ".cortex", "plugins")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			body := "#!/bin/sh\necho '" + handshake + "'\ncat >/dev/null\n"
			if err := os.WriteFile(filepath.Join(dir, "watch"), []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
			p, err := writeBuiltinConfig(filepath.Join(home, ".cortex"), filepath.Join(home, ".cortex", "ca"))
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(p)
			if err != nil {
				t.Fatal(err)
			}
			config.ApplyPreset(cfg)
			addScriptSlot(cfg, true)
			pipe, err := plugins.BuildWithDeps(cfg.Pipeline.Outbound.Plugins,
				plugins.Deps{Listener: forwardproxy.Support(false), LocalInstall: true})
			if err != nil {
				t.Fatalf("the local chain did not build: %v", err)
			}
			pipe.Stop(t.Context())
		})
	}
}

// A user's response-reading script in the default slot, next to a slot that
// rewrites responses (the moca-sandbox example's shape), still builds: the default
// slot goes first, because a response reader must precede every response writer.
func TestLocalChain_BuildsWithAResponseRewritingSlotAdded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "")
	root := filepath.Join(home, ".cortex", "plugins")
	scripts := map[string]string{
		filepath.Join(root, "watch"):                 `{"cortex":1,"hooks":["response"]}`,
		filepath.Join(root, "moca-sandbox", "guard"): `{"cortex":1,"hooks":["request","response"],"enforce":true,"rewrite_response":true}`,
	}
	for path, handshake := range scripts {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\necho '"+handshake+"'\ncat >/dev/null\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	p, err := writeBuiltinConfig(filepath.Join(home, ".cortex"), filepath.Join(home, ".cortex", "ca"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	config.ApplyPreset(cfg)
	cfg.Pipeline.Outbound.Plugins = append(cfg.Pipeline.Outbound.Plugins,
		config.PluginEntry{Name: "script", Config: []byte(`{"dir":"moca-sandbox","fail_closed":true}`)})
	addScriptSlot(cfg, true)
	pipe, err := plugins.BuildWithDeps(cfg.Pipeline.Outbound.Plugins,
		plugins.Deps{Listener: forwardproxy.Support(false), LocalInstall: true})
	if err != nil {
		t.Fatalf("the local chain did not build: %v", err)
	}
	pipe.Stop(t.Context())
}

// The mirror of the case above: a response rewriter in the default slot cannot go
// before a named slot that only reads responses, so the inserted chain is refused.
// Listing the default slot after the named one builds.
func TestLocalChain_ADefaultSlotRewriterBeforeANamedReader(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "")
	root := filepath.Join(home, ".cortex", "plugins")
	scripts := map[string]string{
		filepath.Join(root, "rewrite"):        `{"cortex":1,"hooks":["response"],"rewrite_response":true}`,
		filepath.Join(root, "audit", "watch"): `{"cortex":1,"hooks":["response"]}`,
	}
	for path, handshake := range scripts {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\necho '"+handshake+"'\ncat >/dev/null\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	chain := func(extra ...config.PluginEntry) *config.Config {
		p, err := writeBuiltinConfig(filepath.Join(home, ".cortex"), filepath.Join(home, ".cortex", "ca"))
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(p)
		if err != nil {
			t.Fatal(err)
		}
		config.ApplyPreset(cfg)
		cfg.Pipeline.Outbound.Plugins = append(cfg.Pipeline.Outbound.Plugins, extra...)
		addScriptSlot(cfg, true)
		return cfg
	}
	build := func(cfg *config.Config) error {
		pipe, err := plugins.BuildWithDeps(cfg.Pipeline.Outbound.Plugins,
			plugins.Deps{Listener: forwardproxy.Support(false), LocalInstall: true})
		if err == nil {
			pipe.Stop(t.Context())
		}
		return err
	}
	audit := config.PluginEntry{Name: "script", Config: []byte(`{"dir":"audit"}`)}
	if err := build(chain(audit)); err == nil || !strings.Contains(err.Error(), "reads body after mutator") {
		t.Errorf("the default slot inserted before a named response reader: err = %v, want the reader-after-mutator refusal", err)
	}
	if err := build(chain(audit, config.PluginEntry{Name: "script"})); err != nil {
		t.Errorf("the default slot listed after the named reader did not build: %v", err)
	}
}
