package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/rossoctl/cortex/core/config"
	"gopkg.in/yaml.v3"
)

// The built-in config runs context-guru, and on the local install context-guru's
// toolfilter does the job of tool-prune. writeBuiltinConfig never rewrites an
// existing file, so this step brings an existing config to the same state: it adds
// the context-guru entry, or it moves the tool-prune list into it (issue #2120,
// Part 2). It looks only at pipeline.outbound.plugins.

// contextGuruOnLine is the line printed after each change. It tells the operator
// that the change starts model calls on the agent's own key.
const contextGuruOnLine = "context-guru is on. Its model-backed steps make calls on your agent's own key. " +
	"To stop them, set `on_error: off` on the context-guru entry."

// contextGuruEntryLines is the built-in entry, unindented. KEEP IN STEP with
// builtinConfigYAML (cmd/cortex/local.go): a new install and a migrated one must
// run the same context-guru.
var contextGuruEntryLines = []string{
	"- name: context-guru",
	"  on_error: enforce",
	"  config:",
	"    engine:",
	"      preset: xhigh",
	"      components:",
	"        toolfilter:",
	"          # The prune list: tool definitions to stop sending. Empty does",
	"          # nothing. agentop tools scan --write <this file> fills it, and",
	"          # the config is hot-reloaded, so no restart. mcp__<server>",
	"          # removes every tool of that server.",
	"          remove: []",
}

// contextGuruParsers are the plugins that context-guru goes after when the list
// has no context operator.
var contextGuruParsers = []string{"inference-parser", "mcp-parser", "a2a-parser"}

// contextGuruPresets are the engine presets that have toolfilter in their
// pipeline and no toolfilter block of their own (context-guru v0.7.7
// config/config.go). Under any other engine shape a toolfilter.remove list that
// this step adds would not run as tool-prune's list did.
var contextGuruPresets = []string{"house", "housellm", "conservative", "medium", "high", "xhigh"}

const contextGuruHandFix = "pipeline.outbound.plugins has no place for context-guru after every body reader; " +
	"add the context-guru entry by hand (see docs/laptop-service.md)"

// migrateContextGuru adds context-guru to the config at path, or moves the
// tool-prune list into it. It reports whether it changed the file, and whether
// the running proxy needs a restart for the change.
func migrateContextGuru(path string, stdout io.Writer) (changed, restart bool, err error) {
	raw, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return false, false, err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return false, false, fmt.Errorf("%s does not parse (%w); not touching it", path, err)
	}
	updated, restart, notes, err := planContextGuru(string(raw))
	for _, n := range notes {
		fmt.Fprintf(stdout, "tool-prune stays in %s: %s.\n", path, n)
	}
	if err != nil || updated == "" {
		return false, false, err
	}

	// This step's own backup, overwritten on each run that changes the file.
	bak := path + ".before-agentop-context-guru"
	if werr := os.WriteFile(bak, raw, 0o600); werr != nil {
		return false, false, fmt.Errorf("writing %s: %w", bak, werr)
	}
	moved := toolPruneNames(cfg)
	if err := replaceConfig(path, updated, contextGuruVerify(moved)); err != nil {
		return false, false, err
	}

	fmt.Fprintf(stdout, "Updated %s (previous kept as %s):\n", path, bak)
	if !hasOutbound(cfg, "context-guru") {
		fmt.Fprintf(stdout, "  + pipeline.outbound.plugins: context-guru (preset xhigh)\n")
	}
	if hasOutbound(cfg, "tool-prune") {
		fmt.Fprintf(stdout, "  - pipeline.outbound.plugins: tool-prune (deprecated on the local install)\n")
	}
	if len(moved) > 0 {
		fmt.Fprintf(stdout, "  + context-guru engine.components.toolfilter.remove: %s   (moved from tool-prune)\n",
			strings.Join(moved, ", "))
	}
	fmt.Fprintf(stdout, "  %s\n", contextGuruOnLine)
	if restart {
		fmt.Fprintf(stdout, "  The running proxy needs a restart to start accounting for it.\n")
	} else {
		fmt.Fprintf(stdout, "  A running proxy reloads this from the file; no restart.\n")
	}
	return true, restart, nil
}

// contextGuruVerify is rule 4, the check of a candidate before the swap. The
// context-guru entry must be in the outbound list, after every body reader (the
// reader-after-mutator rule that pipeline.New applies at boot), and its
// toolfilter.remove must hold every name in moved.
func contextGuruVerify(moved []string) func(*config.Config) error {
	return func(c *config.Config) error {
		plugins := c.Pipeline.Outbound.Plugins
		at := slices.IndexFunc(plugins, func(e config.PluginEntry) bool { return e.Name == "context-guru" })
		if at < 0 {
			return fmt.Errorf("the result has no context-guru entry")
		}
		readers := config.BodyReaderPlugins()
		for _, e := range plugins[at+1:] {
			if slices.Contains(readers, e.Name) {
				return fmt.Errorf("the body reader %s comes after context-guru", e.Name)
			}
		}
		var cg struct {
			Engine struct {
				Components struct {
					Toolfilter struct {
						Remove []string `json:"remove"`
					} `json:"toolfilter"`
				} `json:"components"`
			} `json:"engine"`
		}
		if len(plugins[at].Config) > 0 {
			if err := json.Unmarshal(plugins[at].Config, &cg); err != nil {
				return fmt.Errorf("reading the context-guru config: %w", err)
			}
		}
		for _, n := range moved {
			if !slices.Contains(cg.Engine.Components.Toolfilter.Remove, n) {
				return fmt.Errorf("context-guru's toolfilter.remove does not hold %q", n)
			}
		}
		return nil
	}
}

// toolPruneNames returns the remove list of cfg's outbound tool-prune entry.
func toolPruneNames(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	for _, e := range cfg.Pipeline.Outbound.Plugins {
		if e.Name != "tool-prune" {
			continue
		}
		var tp struct {
			Remove []string `json:"remove"`
		}
		if len(e.Config) > 0 {
			_ = json.Unmarshal(e.Config, &tp) // a config that does not decode does not boot either
		}
		return tp.Remove
	}
	return nil
}

func hasOutbound(cfg *config.Config, name string) bool {
	return slices.ContainsFunc(cfg.Pipeline.Outbound.Plugins, func(e config.PluginEntry) bool { return e.Name == name })
}

// lineEdit replaces lines[start:end] (0-based) with repl. start == end inserts.
type lineEdit struct {
	start, end int
	repl       []string
}

// cgItem is one entry of pipeline.outbound.plugins.
type cgItem struct {
	node       *yaml.Node
	name       string
	start, end int // 0-based line span, end exclusive, from the dash line
}

// planContextGuru is the text step of migrateContextGuru. updated is "" when
// nothing changes. restart is true when the config had no context operator, so
// that the change turns accounting on (the reloader refuses that). notes name
// each condition that keeps tool-prune.
func planContextGuru(src string) (updated string, restart bool, notes []string, err error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		return "", false, nil, fmt.Errorf("reading the config's structure: %w", err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return "", false, nil, fmt.Errorf("the config is not a mapping; not editing it")
	}
	lines := strings.Split(src, "\n")
	seq := outboundPlugins(doc.Content[0])
	if seq == nil {
		return "", false, nil, fmt.Errorf("%s", contextGuruHandFix)
	}
	if seq.Style&yaml.FlowStyle != 0 {
		return "", false, nil, fmt.Errorf("pipeline.outbound.plugins is written in flow style; " +
			"rewrite it as an indented list, or add the context-guru entry by hand (see docs/laptop-service.md)")
	}
	dashCol := seq.Column - 1
	items := make([]cgItem, 0, len(seq.Content))
	for _, n := range seq.Content {
		it := cgItem{node: n, name: n.Value}
		if n.Kind == yaml.MappingNode {
			if _, v := mappingEntry(n, "name"); v != nil {
				it.name = v.Value
			}
		}
		it.start = n.Line - 1
		for it.start > 0 && !strings.HasPrefix(strings.TrimSpace(lines[it.start]), "-") {
			it.start--
		}
		it.end = blockEnd(lines, it.start, dashCol)
		items = append(items, it)
	}
	find := func(name string) int { return slices.IndexFunc(items, func(i cgItem) bool { return i.name == name }) }
	cg, tp := find("context-guru"), find("tool-prune")

	switch {
	case tp < 0 && cg >= 0:
		return "", false, nil, nil
	case tp < 0:
		// Rule 2: insert directly after the last parser, ahead of every body reader.
		last := -1
		for i, it := range items {
			if slices.Contains(contextGuruParsers, it.name) {
				last = i
			}
		}
		if last < 0 {
			return "", false, nil, fmt.Errorf("%s", contextGuruHandFix)
		}
		readers := config.BodyReaderPlugins()
		for _, it := range items[last+1:] {
			if slices.Contains(readers, it.name) {
				return "", false, nil, fmt.Errorf("%s", contextGuruHandFix)
			}
		}
		at := items[last].end
		return applyEdits(lines, []lineEdit{{at, at, indented(contextGuruEntry(nil), dashCol)}}), true, nil, nil
	}

	// Rule 3: replace tool-prune, when every condition holds.
	t := items[tp]
	names, notes := toolPruneMovable(t.node)
	if cg >= 0 {
		notes = append(notes, contextGuruTakes(items[cg].node)...)
	}
	if len(notes) > 0 {
		return "", false, notes, nil
	}

	// The comment block directly above tool-prune goes with it.
	delStart := t.start
	for delStart > 0 && strings.HasPrefix(strings.TrimSpace(lines[delStart-1]), "#") {
		delStart--
	}
	if cg < 0 {
		return applyEdits(lines, []lineEdit{{delStart, t.end, indented(contextGuruEntry(names), dashCol)}}), false, nil, nil
	}
	edits := []lineEdit{{delStart, t.end, nil}}
	c := items[cg]
	if c.node.Kind != yaml.MappingNode {
		// A bare `- context-guru` runs the defaults; the built-in entry is the same
		// with the list added.
		edits = append(edits, lineEdit{c.start, c.start + 1, indented(contextGuruEntry(names), dashCol)})
	} else {
		e, err := mergeRemove(lines, c.node, c.end, names)
		if err != nil {
			return "", false, nil, err
		}
		edits = append(edits, e...)
	}
	return applyEdits(lines, edits), false, nil, nil
}

// outboundPlugins returns the pipeline.outbound.plugins sequence, or nil.
func outboundPlugins(root *yaml.Node) *yaml.Node {
	node := root
	for _, key := range []string{"pipeline", "outbound", "plugins"} {
		if node.Kind != yaml.MappingNode {
			return nil
		}
		if _, node = mappingEntry(node, key); node == nil {
			return nil
		}
	}
	if node.Kind != yaml.SequenceNode {
		return nil
	}
	return node
}

// toolPruneMovable returns tool-prune's remove names, and a note for each
// condition that keeps it.
func toolPruneMovable(tp *yaml.Node) (names, notes []string) {
	if tp.Kind != yaml.MappingNode {
		return nil, nil // a bare `- tool-prune` removes nothing
	}
	if _, v := mappingEntry(tp, "on_error"); v != nil && v.Value != "" && v.Value != "enforce" {
		notes = append(notes, fmt.Sprintf("tool-prune's on_error is %s, so its list was not applied", v.Value))
	}
	_, conf := mappingEntry(tp, "config")
	if conf == nil || conf.Kind != yaml.MappingNode {
		return nil, notes
	}
	if _, p := mappingEntry(conf, "paths"); p != nil && len(p.Content) > 0 {
		notes = append(notes, "tool-prune has config.paths, and context-guru's toolfilter has no paths setting")
	}
	_, rm := mappingEntry(conf, "remove")
	if rm == nil || rm.Kind != yaml.SequenceNode {
		return nil, notes
	}
	for _, n := range rm.Content {
		name := n.Value
		switch {
		case strings.HasPrefix(name, "mcp__") && !strings.Contains(name[len("mcp__"):], "__"):
			notes = append(notes, fmt.Sprintf("the name %q is one tool to tool-prune but every tool of a server to toolfilter", name))
		case strings.HasPrefix(name, "skill__"):
			notes = append(notes, fmt.Sprintf("the name %q is a skill to toolfilter, not a tool", name))
		case !validDeclName(name):
			notes = append(notes, fmt.Sprintf("context-guru does not accept the name %q", name))
		}
		names = append(names, name)
	}
	return names, notes
}

// validDeclName is context-guru toolfilter's name check (components/reformat):
// 1 to 128 characters from [A-Za-z0-9._:/-]. context-guru refuses a config with
// any other name, and the proxy would then not boot.
func validDeclName(n string) bool {
	if n == "" || len(n) > 128 {
		return false
	}
	for _, r := range n {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '.' || r == '_' || r == ':' || r == '-' || r == '/'
		if !ok {
			return false
		}
	}
	return true
}

// contextGuruTakes returns a note for each condition under which an existing
// context-guru entry would not apply a moved list (decision 3 of the plan).
func contextGuruTakes(cg *yaml.Node) (notes []string) {
	if cg.Kind != yaml.MappingNode {
		return nil
	}
	if _, v := mappingEntry(cg, "on_error"); v != nil && v.Value != "" && v.Value != "enforce" {
		notes = append(notes, fmt.Sprintf("context-guru's on_error is %s, so nothing would apply the list", v.Value))
	}
	_, conf := mappingEntry(cg, "config")
	if conf == nil || conf.Kind != yaml.MappingNode {
		return notes
	}
	_, eng := mappingEntry(conf, "engine")
	if eng == nil || eng.Kind != yaml.MappingNode {
		return notes
	}
	_, preset := mappingEntry(eng, "preset")
	switch {
	case preset == nil:
		notes = append(notes, "context-guru's engine has no preset, so it runs no toolfilter")
	case !slices.Contains(contextGuruPresets, preset.Value):
		notes = append(notes, fmt.Sprintf("context-guru's engine.preset is %s, not one of %s",
			preset.Value, strings.Join(contextGuruPresets, ", ")))
	}
	if k, _ := mappingEntry(eng, "pipeline"); k != nil {
		notes = append(notes, "context-guru's engine has its own pipeline")
	}
	return notes
}

// mergeRemove adds names to config.engine.components.toolfilter.remove of the
// context-guru entry m, whose lines end before index end. It creates each
// missing level at the indent of its siblings, and keeps the names that are
// there.
func mergeRemove(lines []string, m *yaml.Node, end int, names []string) ([]lineEdit, error) {
	path := []string{"config", "engine", "components", "toolfilter"}
	node := m
	for i, key := range path {
		if node.Style&yaml.FlowStyle != 0 {
			return nil, flowHandFix(path[:i])
		}
		k, v := mappingEntry(node, key)
		switch {
		case k == nil:
			return []lineEdit{{end, end, removePath(path[i:], names, node.Column-1)}}, nil
		case v.Kind == yaml.ScalarNode && v.Tag == "!!null":
			// `key:` with no value, or `key: ~`: write the key bare, children below.
			repl := append([]string{pad(k.Column-1) + key + ":"}, removePath(path[i:], names, k.Column-1)[1:]...)
			if isEmptyValue(lines, k, v) {
				repl[0] = lines[k.Line-1]
			}
			return []lineEdit{{k.Line - 1, k.Line, repl}}, nil
		case v.Kind != yaml.MappingNode:
			return nil, fmt.Errorf("context-guru %s is not a mapping; add the names to toolfilter.remove by hand "+
				"(see docs/laptop-service.md)", strings.Join(path[:i+1], "."))
		}
		end = blockEnd(lines, k.Line-1, k.Column-1)
		node = v
	}
	if node.Style&yaml.FlowStyle != 0 {
		return nil, flowHandFix(path)
	}
	k, v := mappingEntry(node, "remove")
	switch {
	case k == nil:
		return []lineEdit{{end, end, removePath(nil, names, node.Column-1)}}, nil
	case v.Kind == yaml.ScalarNode && v.Tag == "!!null":
		return []lineEdit{{k.Line - 1, k.Line, removePath(nil, names, k.Column-1)}}, nil
	case v.Kind != yaml.SequenceNode:
		return nil, fmt.Errorf("context-guru toolfilter.remove is not a list; add the names by hand (see docs/laptop-service.md)")
	}
	have := make([]string, 0, len(v.Content))
	for _, n := range v.Content {
		have = append(have, n.Value)
	}
	var add []string
	for _, n := range names {
		if !slices.Contains(have, n) && !slices.Contains(add, n) {
			add = append(add, n)
		}
	}
	if len(add) == 0 {
		return nil, nil
	}
	if v.Style&yaml.FlowStyle != 0 {
		for _, n := range v.Content {
			if n.Line != k.Line {
				return nil, fmt.Errorf("context-guru toolfilter.remove spans more than one line; " +
					"write it on one line, or add the names by hand (see docs/laptop-service.md)")
			}
		}
		return []lineEdit{{k.Line - 1, k.Line, removePath(nil, append(have, add...), k.Column-1)}}, nil
	}
	last := v.Content[len(v.Content)-1].Line
	items := make([]string, len(add))
	for i, n := range add {
		items[i] = pad(v.Column-1) + "- " + yamlName(n)
	}
	return []lineEdit{{last, last, items}}, nil
}

func flowHandFix(path []string) error {
	at := "the context-guru entry"
	if len(path) > 0 {
		at = "context-guru " + strings.Join(path, ".")
	}
	return fmt.Errorf("%s is written in flow style; rewrite it as an indented block, "+
		"or add the names to toolfilter.remove by hand (see docs/laptop-service.md)", at)
}

// removePath renders keys as nested blocks at indent, with remove: [names] at the
// bottom. A new engine level gets preset: xhigh, which is the plugin's default:
// an engine with components and no preset runs no preset at all.
func removePath(keys, names []string, indent int) []string {
	var out []string
	for j, k := range keys {
		out = append(out, pad(indent+2*j)+k+":")
		if k == "engine" {
			out = append(out, pad(indent+2*j+2)+"preset: xhigh")
		}
	}
	return append(out, pad(indent+2*len(keys))+"remove: "+flowNames(names))
}

// contextGuruEntry is contextGuruEntryLines with names as the remove list.
func contextGuruEntry(names []string) []string {
	out := slices.Clone(contextGuruEntryLines)
	last := len(out) - 1
	out[last] = strings.Replace(out[last], "[]", flowNames(names), 1)
	return out
}

func flowNames(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = yamlName(n)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// yamlName writes n plain when it reads back as the same string in a flow list,
// and quoted otherwise (for example `true` or `1`).
func yamlName(n string) string {
	var v []any
	if yaml.Unmarshal([]byte("["+n+"]"), &v) == nil && len(v) == 1 && v[0] == n {
		return n
	}
	return strconv.Quote(n)
}

// blockEnd returns the end (0-based, exclusive) of the block that starts at line
// index from and owns every later line indented past col. Blank and comment lines
// at its end belong to what comes next.
func blockEnd(lines []string, from, col int) int {
	i := from + 1
	for ; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if len(lines[i])-len(strings.TrimLeft(lines[i], " ")) <= col {
			break
		}
	}
	for i-1 > from {
		if t := strings.TrimSpace(lines[i-1]); t != "" && !strings.HasPrefix(t, "#") {
			break
		}
		i--
	}
	return i
}

// applyEdits applies non-overlapping edits from the bottom up, so that each
// edit's line positions still hold. At one position a replace goes before an
// insert, so the insert is not replaced.
func applyEdits(lines []string, edits []lineEdit) string {
	sort.Slice(edits, func(a, b int) bool {
		if edits[a].start != edits[b].start {
			return edits[a].start > edits[b].start
		}
		return edits[a].end > edits[b].end
	})
	out := slices.Clone(lines)
	for _, e := range edits {
		out = slices.Replace(out, e.start, e.end, e.repl...)
	}
	return strings.Join(out, "\n")
}
