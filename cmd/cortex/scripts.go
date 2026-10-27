package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins"
)

// scriptPlugin is the script plugin's registered name. Repeated here rather than
// imported so that this file builds with or without the plugin's tag.
const scriptPlugin = "script"

// addScriptSlot puts the default slot, ~/.cortex/plugins itself, into the outbound
// chain of a local install, so a user who drops a file there has it running without
// editing YAML.
//
// A chain that names no script entry gets it appended. A chain whose script entries
// all name a dir — a slot another tool added, such as the moca-sandbox example's —
// gets it just before the first built one, so adding a slot does not switch the
// user's own scripts off. Before, not after: a slot that rewrites responses must
// come after every response reader, and the default slot's scripts may read them.
// A chain that already has an entry for the default slot, under any on_error,
// including off, is the user's own arrangement and is left alone. A binary built
// without the plugin adds nothing, since the entry could only fail to load.
func addScriptSlot(c *config.Config, local bool) {
	if !local || !slices.Contains(plugins.RegisteredPlugins(), scriptPlugin) {
		return
	}
	chain := c.Pipeline.Outbound.Plugins
	first := -1
	for i, e := range chain {
		if e.Name != scriptPlugin {
			continue
		}
		if entryDir(e) == "" {
			return
		}
		if first < 0 && e.OnError.Resolved() != pipeline.ErrorPolicyOff {
			first = i
		}
	}
	slot := config.PluginEntry{Name: scriptPlugin}
	if first < 0 {
		c.Pipeline.Outbound.Plugins = append(chain, slot)
		return
	}
	c.Pipeline.Outbound.Plugins = slices.Insert(chain, first, slot)
}

// checkScriptSlots refuses a chain with two built `script` entries on one folder:
// each script would run twice, and the two slots would share one copy of a
// streamed response. An entry whose folder does not resolve is left to the
// plugin's own Configure error.
func checkScriptSlots(c *config.Config) error {
	for _, chain := range [][]config.PluginEntry{c.Pipeline.Inbound.Plugins, c.Pipeline.Outbound.Plugins} {
		seen := map[string]bool{}
		for _, e := range chain {
			if e.Name != scriptPlugin || e.OnError.Resolved() == pipeline.ErrorPolicyOff {
				continue
			}
			dir, err := scriptSlotDir(entryDir(e))
			if err != nil {
				continue
			}
			if seen[dir] {
				return fmt.Errorf("two script entries in one chain read %s; give each its own dir", dir)
			}
			seen[dir] = true
		}
	}
	return nil
}

func entryDir(e config.PluginEntry) string {
	var cfg struct {
		Dir string `json:"dir"`
	}
	if len(e.Config) > 0 {
		_ = json.Unmarshal(e.Config, &cfg)
	}
	return cfg.Dir
}

// scriptFolders is every folder a `script` entry of c reads, for the reloader to
// watch. Empty when the binary has no script plugin or c names no entry.
func scriptFolders(c *config.Config) []string {
	if c == nil {
		return nil
	}
	var out []string
	for _, chain := range [][]config.PluginEntry{c.Pipeline.Inbound.Plugins, c.Pipeline.Outbound.Plugins} {
		for _, e := range chain {
			if e.Name != scriptPlugin {
				continue
			}
			dir, err := scriptSlotDir(entryDir(e))
			if err != nil {
				slog.Warn("script plugin: cannot resolve a slot's folder; it is not watched", "dir", entryDir(e), "error", err)
				continue
			}
			if !slices.Contains(out, dir) {
				out = append(out, dir)
			}
		}
	}
	return out
}
