package pricing

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// strictDecode decodes a YAML mapping into out, rejecting keys that out does not
// declare.
//
// The top-level loader is deliberately NOT strict, because tightening it would
// change behaviour for every config in the platform. This section is scoped
// strictly anyway, because two plausible typos here are severe and silent:
//
//   - `hosts:` instead of `host:` leaves Host empty, which means ANY endpoint —
//     and since a configured entry outranks anything bundled, one stray letter
//     reprices the whole process at rates meant for one gateway.
//   - a misspelled tier (`input_cost_per_milion`) still leaves the entry with a
//     rate for some other tier, so it builds; then Cost refuses every request that
//     carried input tokens, and the endpoint's entire traffic drops out of the
//     total with no diagnostic at all.
//
// Both report as "nothing is priced", which is indistinguishable from having
// configured nothing — the failure mode this package exists to eliminate.
func strictDecode(node *yaml.Node, out any) error {
	if node.Kind != yaml.MappingNode {
		return node.Decode(out)
	}
	known := yamlKeys(reflect.TypeOf(out).Elem())
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if _, ok := known[key]; !ok {
			return fmt.Errorf("line %d: unknown field %q (valid: %s)",
				node.Content[i].Line, key, strings.Join(sortedKeys(known), ", "))
		}
	}
	return node.Decode(out)
}

// yamlKeys collects the yaml tag names a struct declares, following embedded
// structs so inlined tier rates count as the outer type's own fields.
func yamlKeys(t reflect.Type) map[string]struct{} {
	out := map[string]struct{}{}
	if t.Kind() != reflect.Struct {
		return out
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if f.Anonymous || strings.Contains(f.Tag.Get("yaml"), ",inline") {
			for k := range yamlKeys(f.Type) {
				out[k] = struct{}{}
			}
			continue
		}
		if tag == "" || tag == "-" {
			continue
		}
		out[tag] = struct{}{}
	}
	return out
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// The shadow types below exist only so UnmarshalYAML can decode without recursing
// into itself.

func (c *Config) UnmarshalYAML(node *yaml.Node) error {
	type shadow Config
	var s shadow
	if err := strictDecode(node, &s); err != nil {
		return fmt.Errorf("pricing: %w", err)
	}
	*c = Config(s)
	return nil
}

func (e *EndpointConfig) UnmarshalYAML(node *yaml.Node) error {
	type shadow EndpointConfig
	var s shadow
	if err := strictDecode(node, &s); err != nil {
		return fmt.Errorf("pricing.endpoints: %w", err)
	}
	*e = EndpointConfig(s)
	return nil
}

func (m *ModelConfig) UnmarshalYAML(node *yaml.Node) error {
	type shadow ModelConfig
	var s shadow
	if err := strictDecode(node, &s); err != nil {
		return fmt.Errorf("pricing model rates: %w", err)
	}
	*m = ModelConfig(s)
	return nil
}

func (t *ThresholdConfig) UnmarshalYAML(node *yaml.Node) error {
	type shadow ThresholdConfig
	var s shadow
	if err := strictDecode(node, &s); err != nil {
		return fmt.Errorf("pricing above[]: %w", err)
	}
	*t = ThresholdConfig(s)
	return nil
}
