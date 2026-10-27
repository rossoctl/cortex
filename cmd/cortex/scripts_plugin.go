//go:build include_plugin_script

package main

import (
	"strings"

	"github.com/rossoctl/cortex/core/plugins/script"
)

func scriptSlotDir(dir string) (string, error) { return script.SlotDir(dir) }

// scriptFingerprint digests every script in dirs, so the reloader rebuilds when one
// is added, removed, renamed or edited. Empty when no slot holds a script, so a
// local install without scripts publishes the config file's own sha256 on
// /reload/status, as every other install does.
func scriptFingerprint(dirs []string) string {
	var parts []string
	for _, d := range dirs {
		if fp := script.Fingerprint(d); fp != "" {
			parts = append(parts, d+"="+fp)
		}
	}
	return strings.Join(parts, "\x00")
}
