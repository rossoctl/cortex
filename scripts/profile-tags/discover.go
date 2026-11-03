package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// discoverTagPattern matches `include_plugin_<name>` in a //go:build directive.
// Unanchored at the end so a compound directive still yields the name.
var discoverTagPattern = regexp.MustCompile(`^//go:build\s+include_plugin_(\w+)`)

// discoverPlugins returns every plugin name registered in regDir, derived from
// the plugins_*.go build directives themselves. Deriving from source rather than
// a hand-kept list is what makes the accounting guard meaningful: a plugin cannot
// be added without this seeing it.
func discoverPlugins(regDir string) ([]PluginName, error) {
	files, err := pluginFiles(regDir)
	if err != nil {
		return nil, err
	}
	seen := map[PluginName]bool{}
	for _, path := range files {
		name, err := extractIncludeSuffix(path)
		if err != nil {
			return nil, err
		}
		if name != "" {
			seen[PluginName(name)] = true
		}
	}
	out := make([]PluginName, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// pluginFiles lists every plugins_*.go registration file in dir, excluding test
// files, which match the same glob but are not directives.
func pluginFiles(dir string) ([]string, error) {
	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "plugins_*.go"))
	if err != nil {
		return nil, fmt.Errorf("glob %s: %w", dir, err)
	}
	var out []string
	for _, path := range matches {
		if !strings.HasSuffix(path, "_test.go") {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out, nil
}

// extractIncludeSuffix returns the suffix of an `include_plugin_*` directive, or
// "" if the file has none.
func extractIncludeSuffix(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if m := discoverTagPattern.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			return m[1], nil
		}
	}
	return "", nil
}
