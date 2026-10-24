//go:build include_plugin_script

package main

import (
	"slices"
	"testing"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/pipeline"
)

// outboundNames lists the outbound chain as name, or name:dir for a script slot
// that names one.
func outboundNames(c *config.Config) []string {
	var out []string
	for _, e := range c.Pipeline.Outbound.Plugins {
		if d := entryDir(e); d != "" {
			out = append(out, e.Name+":"+d)
		} else {
			out = append(out, e.Name)
		}
	}
	return out
}

func TestAddScriptSlot(t *testing.T) {
	chain := func(entries ...config.PluginEntry) *config.Config {
		c := &config.Config{}
		c.Pipeline.Outbound.Plugins = entries
		return c
	}
	parser := config.PluginEntry{Name: "inference-parser"}
	prune := config.PluginEntry{Name: "tool-prune"}
	moca := config.PluginEntry{Name: "script", Config: []byte(`{"dir":"moca-sandbox","fail_closed":true}`)}
	early := config.PluginEntry{Name: "script", Config: []byte(`{"dir":"early"}`)}
	def := config.PluginEntry{Name: "script"}
	defOff := config.PluginEntry{Name: "script", OnError: pipeline.ErrorPolicyOff}
	emptyDir := config.PluginEntry{Name: "script", Config: []byte(`{"dir":""}`)}

	for _, tc := range []struct {
		name  string
		local bool
		in    []config.PluginEntry
		want  []string
	}{
		{"no script entry: appended last", true, []config.PluginEntry{parser, prune}, []string{"inference-parser", "tool-prune", "script"}},
		{"not a local install: nothing added", false, []config.PluginEntry{parser}, []string{"inference-parser"}},
		{"a named slot: the default goes just before it", true, []config.PluginEntry{parser, prune, moca}, []string{"inference-parser", "tool-prune", "script", "script:moca-sandbox"}},
		{"two named slots: before the first", true, []config.PluginEntry{parser, early, prune, moca}, []string{"inference-parser", "script", "script:early", "tool-prune", "script:moca-sandbox"}},
		{"the user's default after a named slot: left alone", true, []config.PluginEntry{parser, moca, def}, []string{"inference-parser", "script:moca-sandbox", "script"}},
		{"the user's default switched off: left alone", true, []config.PluginEntry{defOff, parser, moca}, []string{"script", "inference-parser", "script:moca-sandbox"}},
		{"an explicit empty dir is the default slot", true, []config.PluginEntry{parser, moca, emptyDir}, []string{"inference-parser", "script:moca-sandbox", "script"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := chain(slices.Clone(tc.in)...)
			addScriptSlot(c, tc.local)
			if got := outboundNames(c); !slices.Equal(got, tc.want) {
				t.Errorf("chain = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestScriptFolders(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := &config.Config{}
	c.Pipeline.Outbound.Plugins = []config.PluginEntry{
		{Name: "script"},
		{Name: "tool-prune"},
		{Name: "script", Config: []byte(`{"dir":"late"}`)},
		{Name: "script", Config: []byte(`{"dir":"../escape"}`)},
	}
	got := scriptFolders(c)
	if len(got) != 2 {
		t.Fatalf("scriptFolders = %v, want the two valid slots", got)
	}
}

func TestScriptFingerprint_EmptyWithoutScripts(t *testing.T) {
	dir := t.TempDir()
	if fp := scriptFingerprint([]string{dir, dir + "/missing"}); fp != "" {
		t.Errorf("slots without scripts fingerprint as %q; /reload/status would stop being the config's sha256", fp)
	}
}

// Two slots in one chain on the same folder would run each script twice and share
// its streamed-response copy, so the chain is refused. A switched-off entry is not
// built and does not count.
func TestCheckScriptSlots(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	chain := func(entries ...config.PluginEntry) *config.Config {
		c := &config.Config{}
		c.Pipeline.Outbound.Plugins = entries
		return c
	}
	late := config.PluginEntry{Name: "script", Config: []byte(`{"dir":"late"}`)}
	for name, c := range map[string]*config.Config{
		"one slot":          chain(config.PluginEntry{Name: "script"}),
		"two folders":       chain(config.PluginEntry{Name: "script"}, late),
		"one switched off":  chain(late, config.PluginEntry{Name: "script", Config: []byte(`{"dir":"late"}`), OnError: pipeline.ErrorPolicyOff}),
		"an invalid folder": chain(config.PluginEntry{Name: "script", Config: []byte(`{"dir":"../x"}`)}, config.PluginEntry{Name: "script", Config: []byte(`{"dir":"../x"}`)}),
	} {
		if err := checkScriptSlots(c); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, c := range map[string]*config.Config{
		"same dir twice":       chain(late, late),
		"default folder twice": chain(config.PluginEntry{Name: "script"}, config.PluginEntry{Name: "script", Config: []byte(`{"dir":""}`)}),
	} {
		if err := checkScriptSlots(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	in := &config.Config{}
	in.Pipeline.Inbound.Plugins = []config.PluginEntry{late, late}
	if err := checkScriptSlots(in); err == nil {
		t.Error("inbound chain with the same folder twice: accepted")
	}
}
