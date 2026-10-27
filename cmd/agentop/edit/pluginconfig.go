package edit

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ConfigChange is one edit to the config: block of a named plugin entry in a
// Cortex config file. It knows nothing about any plugin: what a value means, and
// whether it is allowed, is the caller's to decide.
type ConfigChange struct {
	// Chain is the pipeline chain the entry is in: "outbound" or "inbound".
	Chain string
	// Plugin is the entry's name:.
	Plugin string
	// Path is the key path below config:, outermost first, such as
	// {"agents", "claude-code"}. At least one key.
	Path []string
	// Value is the value to set: a scalar, or a mapping of scalars and mappings
	// (see ScalarValue and MapValue). Nil removes the key, and with it every
	// mapping the removal leaves empty, config: included — and every comment inside
	// what it removes.
	Value *yaml.Node
	// CreatePlugin appends an entry for Plugin at the end of Chain when the chain
	// has none. Without it a missing entry is an error. A removal never creates one.
	CreatePlugin bool
	// Match, when set, picks the entry among those named Plugin whose config: holds
	// every key in it with that string value, such as {"dir": "moca-sandbox"}: the
	// way to address one of several entries sharing a name, as every script slot
	// does. Two matches are still an error. A created entry gets Match's keys in its
	// config:, ahead of Path.
	Match map[string]string
	// RemoveEntry removes the whole entry, comments inside it included, instead of
	// editing its config:. Path and Value are then unused. An absent entry changes
	// nothing.
	RemoveEntry bool
}

// ScalarValue is a string value for ConfigChange.Value. It is written as a YAML
// string whatever it looks like, so "123" or "true" stay strings.
func ScalarValue(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
}

// BoolValue is a boolean value for ConfigChange.Value, written as true or false.
func BoolValue(b bool) *yaml.Node {
	v := "false"
	if b {
		v = "true"
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: v}
}

// MapValue is a mapping of string keys to string values, in the order given:
// MapValue("url", u, "key", k). It panics on an odd number of arguments, which is a
// programming error rather than input.
func MapValue(pairs ...string) *yaml.Node {
	if len(pairs)%2 != 0 {
		panic("edit.MapValue: odd number of arguments")
	}
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for i := 0; i < len(pairs); i += 2 {
		n.Content = append(n.Content, ScalarValue(pairs[i]), ScalarValue(pairs[i+1]))
	}
	return n
}

// SetPluginConfig returns src with ch applied, and src itself when there is
// nothing to change.
//
// Positions come from the YAML parser and the edit is made to plain lines, as
// `agentop config migrate-pricing` does, so every line the change does not touch
// survives byte for byte, comments included. A change does touch the lines it
// replaces or removes, and their comments go with them: a replaced value's, and on
// a removal the key's whole block and every mapping the removal leaves empty, with
// any comment inside, however deep. A comment above a removed key stays, since it may
// be about what follows. Round-tripping the document through a YAML encoder would
// reflow it and drop the comments the local config ships with.
//
// It edits indented (block) YAML. A mapping it has to descend into, or a chain's
// plugins list, written in flow style ({...} or [...]) is refused with an error
// saying so; a value it replaces whole may be in any style.
func SetPluginConfig(src []byte, ch ConfigChange) ([]byte, error) {
	if len(ch.Path) == 0 && !ch.RemoveEntry {
		return nil, errors.New("edit: a config change needs at least one key")
	}
	// Refuse a src holding U+0085 (NEL), U+2028 (LS), U+2029 (PS), or a \r not
	// immediately followed by \n, wherever it is: in a comment or a quoted value as
	// much as between lines. The parser counts each as a line break and the split
	// below does not, so after one, every yaml.Node.Line points further down lines
	// than the line it was read from, and an edit lands there. A CRLF passes: both
	// count it as one break.
	if b := uncountedBreak(string(src)); b != "" {
		return nil, fmt.Errorf("edit: the config contains %s, which YAML counts as a line break and this editor does not; "+
			"remove it or make it a newline, then try again", b)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, fmt.Errorf("reading the config's structure: %w", err)
	}
	if len(doc.Content) == 0 || !blockMapping(doc.Content[0]) {
		return nil, errors.New("the config is not an indented mapping; not editing it")
	}
	lines := strings.Split(string(src), "\n")

	plugins, err := chainPlugins(doc.Content[0], ch)
	if err != nil {
		return nil, err
	}
	item, err := findEntry(lines, plugins, ch.Plugin, ch.Match)
	if err != nil {
		return nil, err
	}

	var out []string
	switch {
	case ch.RemoveEntry && item == nil:
		return src, nil
	case ch.RemoveEntry:
		out = removeEntry(lines, plugins, item)
	case item == nil && ch.Value == nil:
		return src, nil
	case item == nil && !ch.CreatePlugin:
		return nil, fmt.Errorf("pipeline.%s has no %s entry", ch.Chain, ch.Plugin)
	case item == nil:
		out, err = appendEntry(lines, plugins, ch)
	default:
		out, err = editEntry(lines, item, ch)
	}
	if err != nil {
		return nil, err
	}
	if out == nil {
		return src, nil
	}
	return []byte(strings.Join(out, "\n")), nil
}

// pair is a key node and its value node.
type pair struct{ k, v *yaml.Node }

// chainPlugins is the plugins: list of pipeline.<Chain>.
func chainPlugins(root *yaml.Node, ch ConfigChange) (*yaml.Node, error) {
	_, pipe := mappingEntry(root, "pipeline")
	_, chain := mappingEntry(pipe, ch.Chain)
	_, plugins := mappingEntry(chain, "plugins")
	if plugins == nil || plugins.Kind != yaml.SequenceNode || plugins.Style&yaml.FlowStyle != 0 || len(plugins.Content) == 0 {
		return nil, fmt.Errorf("pipeline.%s.plugins is not an indented list with at least one entry; add the %s entry by hand",
			ch.Chain, ch.Plugin)
	}
	return plugins, nil
}

// findEntry is the plugins list item named name whose config: holds match, or nil
// when there is none.
func findEntry(lines []string, plugins *yaml.Node, name string, match map[string]string) (*yaml.Node, error) {
	var found *yaml.Node
	for _, item := range plugins.Content {
		if item.Kind == yaml.ScalarNode && item.Value == name {
			if len(match) > 0 {
				continue // a bare name has no config:, so it matches nothing
			}
			return nil, fmt.Errorf("the %s entry is written as a bare name; write it as `- name: %s` to give it config", name, name)
		}
		if item.Kind != yaml.MappingNode {
			continue
		}
		if _, n := mappingEntry(item, "name"); n == nil || n.Value != name {
			continue
		}
		if !configHolds(item, match) {
			continue
		}
		if found != nil {
			if len(match) > 0 {
				return nil, fmt.Errorf("two %s entries in one chain match %s; edit the config by hand", name, describeMatch(match))
			}
			return nil, fmt.Errorf("two %s entries in one chain; edit the config by hand", name)
		}
		if item.Style&yaml.FlowStyle != 0 || !strings.HasPrefix(strings.TrimSpace(lines[item.Line-1]), "-") {
			return nil, fmt.Errorf("the %s entry is not written as `- name: %s` with its keys indented below; edit it by hand", name, name)
		}
		found = item
	}
	return found, nil
}

// configHolds reports whether item's config: has each key of match set to that
// string value. An empty match holds for every entry.
func configHolds(item *yaml.Node, match map[string]string) bool {
	if len(match) == 0 {
		return true
	}
	_, cfg := mappingEntry(item, "config")
	for k, want := range match {
		_, v := mappingEntry(cfg, k)
		if v == nil || v.Kind != yaml.ScalarNode || v.Value != want {
			return false
		}
	}
	return true
}

// describeMatch is match as `dir: x`, keys sorted, for errors.
func describeMatch(match map[string]string) string {
	keys := make([]string, 0, len(match))
	for k := range match {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + ": " + match[k]
	}
	return strings.Join(parts, ", ")
}

// removeEntry drops item's lines from the plugins list. It refuses nothing: an
// entry the list holds can always be cut, since findEntry only returns one written
// as `- name:` with its keys below. A chain left with no entries keeps its
// plugins: key, with an empty list, so the file still loads.
func removeEntry(lines []string, plugins *yaml.Node, item *yaml.Node) []string {
	start := item.Line - 1
	end := blockEnd(lines, start, plugins.Column-1, false)
	if len(plugins.Content) == 1 {
		return splice(lines, start, end+1, []string{pad(plugins.Column-1) + "[]"})
	}
	return splice(lines, start, end+1, nil)
}

// appendEntry adds `- name: <Plugin>` and its config: after the chain's last entry,
// at the indentation that entry uses. Match's keys come first in config:.
func appendEntry(lines []string, plugins *yaml.Node, ch ConfigChange) ([]string, error) {
	dash := plugins.Column - 1
	last := plugins.Content[len(plugins.Content)-1]
	keys := dash + 2
	if last.Kind == yaml.MappingNode {
		keys = last.Column - 1
	}
	name, err := scalarText(ch.Plugin)
	if err != nil {
		return nil, err
	}
	var body []string
	mkeys := make([]string, 0, len(ch.Match))
	for k := range ch.Match {
		mkeys = append(mkeys, k)
	}
	sort.Strings(mkeys)
	for _, k := range mkeys {
		line, err := renderPath([]string{k}, ScalarValue(ch.Match[k]), keys+2)
		if err != nil {
			return nil, err
		}
		body = append(body, line...)
	}
	if len(ch.Path) > 0 {
		rest, err := renderPath(ch.Path, ch.Value, keys+2)
		if err != nil {
			return nil, err
		}
		body = append(body, rest...)
	}
	add := append([]string{
		pad(dash) + "-" + pad(keys-dash-1) + "name: " + name,
		pad(keys) + "config:",
	}, body...)
	end := blockEnd(lines, last.Line-1, dash, false)
	return splice(lines, end+1, end+1, add), nil
}

// editEntry applies ch inside an existing entry, adding config: when it has none.
func editEntry(lines []string, item *yaml.Node, ch ConfigChange) ([]string, error) {
	ck, cv := mappingEntry(item, "config")
	if ck == nil {
		if ch.Value == nil {
			return nil, nil
		}
		keys := item.Column - 1
		body, err := renderPath(ch.Path, ch.Value, keys+2)
		if err != nil {
			return nil, err
		}
		end := blockEnd(lines, item.Line-1, indentOf(lines[item.Line-1]), false)
		return splice(lines, end+1, end+1, append([]string{pad(keys) + "config:"}, body...)), nil
	}
	return setIn(lines, []pair{{ck, cv}}, ch.Path, ch.Value, ch.Plugin)
}

// setIn applies the change below the mapping that is the value of the last pair in
// chain. chain runs from config: down, so a removal can take the mappings it
// empties with it. A nil result means nothing changed.
func setIn(lines []string, chain []pair, path []string, value *yaml.Node, plugin string) ([]string, error) {
	parent := chain[len(chain)-1]
	if emptyValue(lines, parent) {
		if value == nil {
			return nil, nil
		}
		body, err := renderPath(path, value, parent.k.Column-1+2)
		if err != nil {
			return nil, err
		}
		return splice(lines, parent.k.Line, parent.k.Line, body), nil
	}
	if !blockMapping(parent.v) {
		return nil, fmt.Errorf("%s %s is not an indented mapping; rewrite it as one, or edit it by hand", plugin, dotted(chain))
	}

	k, v := mappingEntry(parent.v, path[0])
	if k == nil {
		if value == nil {
			return nil, nil
		}
		body, err := renderPath(path, value, parent.v.Column-1)
		if err != nil {
			return nil, err
		}
		end := valueEnd(lines, parent)
		return splice(lines, end+1, end+1, body), nil
	}
	if len(path) > 1 {
		return setIn(lines, append(chain, pair{k, v}), path[1:], value, plugin)
	}

	if value == nil {
		// Remove the highest mapping that holds nothing but the way down to this key.
		top := pair{k, v}
		for i := len(chain) - 1; i >= 0 && len(chain[i].v.Content) == 2; i-- {
			top = chain[i]
		}
		return splice(lines, top.k.Line-1, valueEnd(lines, top)+1, nil), nil
	}
	if sameValue(v, value) {
		return nil, nil
	}
	body, err := renderPath(path, value, k.Column-1)
	if err != nil {
		return nil, err
	}
	return splice(lines, k.Line-1, valueEnd(lines, pair{k, v})+1, body), nil
}

// renderPath writes path as nested keys at indent, the last holding value.
func renderPath(path []string, value *yaml.Node, indent int) ([]string, error) {
	key, err := scalarText(path[0])
	if err != nil {
		return nil, err
	}
	if len(path) == 1 {
		return renderKey(key, value, indent)
	}
	rest, err := renderPath(path[1:], value, indent+2)
	if err != nil {
		return nil, err
	}
	return append([]string{pad(indent) + key + ":"}, rest...), nil
}

// renderKey writes `key: value`, or `key:` and value's pairs indented below it.
func renderKey(key string, value *yaml.Node, indent int) ([]string, error) {
	switch value.Kind {
	case yaml.ScalarNode:
		if value.Tag == "!!bool" && (value.Value == "true" || value.Value == "false") {
			return []string{pad(indent) + key + ": " + value.Value}, nil
		}
		s, err := scalarText(value.Value)
		if err != nil {
			return nil, err
		}
		return []string{pad(indent) + key + ": " + s}, nil
	case yaml.MappingNode:
		if len(value.Content) == 0 {
			return nil, errors.New("edit: cannot write an empty mapping; it would read back as null")
		}
		out := []string{pad(indent) + key + ":"}
		for i := 0; i+1 < len(value.Content); i += 2 {
			k, err := scalarText(value.Content[i].Value)
			if err != nil {
				return nil, err
			}
			child, err := renderKey(k, value.Content[i+1], indent+2)
			if err != nil {
				return nil, err
			}
			out = append(out, child...)
		}
		return out, nil
	}
	return nil, fmt.Errorf("edit: cannot write a YAML value of kind %d; only scalars and mappings", value.Kind)
}

// scalarText is s as a one-line YAML string, quoted only when it has to be. The
// error never repeats s: the values written here include API keys.
//
// It refuses s holding any character YAML counts as a line break: \n, \r, U+0085
// (NEL), U+2028 (LS) or U+2029 (PS). yaml.v3 would write a \n as a block over
// several lines, and an LS or PS raw, which the next SetPluginConfig would refuse
// the file for; it would escape a \r or NEL, but those are refused as well, so no
// value carries a line break of any kind.
func scalarText(s string) (string, error) {
	if strings.ContainsAny(s, "\n\r\u0085\u2028\u2029") {
		return "", errors.New("edit: a value with a line break cannot be written on one line")
	}
	b, err := yaml.Marshal(ScalarValue(s))
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(string(b), "\n"), nil
}

// uncountedBreak names the first character in src that the YAML parser counts as
// a line break and strings.Split(src, "\n") does not split on, or is "" when there
// is none. Those are U+0085 (NEL), U+2028 (LS), U+2029 (PS), and a \r not
// immediately followed by \n. The name is all an error may say: src can hold API keys.
func uncountedBreak(src string) string {
	for i, r := range src {
		switch {
		case r == '\u0085':
			return "U+0085 (NEXT LINE)"
		case r == '\u2028':
			return "U+2028 (LINE SEPARATOR)"
		case r == '\u2029':
			return "U+2029 (PARAGRAPH SEPARATOR)"
		case r == '\r' && !strings.HasPrefix(src[i+1:], "\n"):
			return "a carriage return not followed by a newline"
		}
	}
	return ""
}

// sameValue reports whether the node in the file already holds want, whatever its
// style: `{url: x, key: y}` holds what an indented url and key hold.
func sameValue(have, want *yaml.Node) bool {
	var a, b any
	if have.Decode(&a) != nil || want.Decode(&b) != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}

// valueEnd is the index of the last line of p's value.
func valueEnd(lines []string, p pair) int {
	compactList := p.v.Kind == yaml.SequenceNode && p.v.Style&yaml.FlowStyle == 0
	return blockEnd(lines, p.k.Line-1, p.k.Column-1, compactList)
}

// blockEnd is the index of the last line of the block that starts at lines[start]:
// every following line indented deeper than indent, and with listAtIndent also the
// `- ` items of a list written at indent itself (`key:` then `- a` below it, which
// YAML allows). Blank lines and comments shallower than the block are passed over
// rather than counted, so a comment introducing the next key stays with that key.
func blockEnd(lines []string, start, indent int, listAtIndent bool) int {
	end := start
	for i := start + 1; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if t == "" {
			continue
		}
		ind := indentOf(lines[i])
		inside := ind > indent || (listAtIndent && ind == indent && (t == "-" || strings.HasPrefix(t, "- ")))
		if !inside {
			if strings.HasPrefix(t, "#") {
				continue
			}
			break
		}
		end = i
	}
	return end
}

// emptyValue reports whether p's key has no value at all — `agents:` with nothing
// after it but a comment — so children can go on the following lines.
func emptyValue(lines []string, p pair) bool {
	if p.v.Kind != yaml.ScalarNode || p.v.Tag != "!!null" {
		return false
	}
	line := lines[p.k.Line-1]
	rest := line[min(len(line), p.k.Column-1+len(p.k.Value)):]
	rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), ":"))
	return rest == "" || strings.HasPrefix(rest, "#")
}

func blockMapping(n *yaml.Node) bool {
	return n != nil && n.Kind == yaml.MappingNode && n.Style&yaml.FlowStyle == 0
}

// mappingEntry is the key and value nodes for key in m, or nils. m may be nil.
func mappingEntry(m *yaml.Node, key string) (k, v *yaml.Node) {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i], m.Content[i+1]
		}
	}
	return nil, nil
}

// dotted names the mapping chain ends at, for errors: config.agents.
func dotted(chain []pair) string {
	keys := make([]string, len(chain))
	for i, p := range chain {
		keys[i] = p.k.Value
	}
	return strings.Join(keys, ".")
}

// splice replaces lines[from:to] with add.
func splice(lines []string, from, to int, add []string) []string {
	out := make([]string, 0, len(lines)-(to-from)+len(add))
	out = append(out, lines[:from]...)
	out = append(out, add...)
	return append(out, lines[to:]...)
}

func indentOf(line string) int { return len(line) - len(strings.TrimLeft(line, " ")) }

func pad(n int) string { return strings.Repeat(" ", n) }
