// Schema introspection for plugin config structs.
//
// Plugins decode their config from JSON via Configurable.Configure
// (see configurable.go). The framework has long known JSON field
// names (`json:"foo_bar"` tags) but nothing else about each field.
// Tools that want to render config UIs, generate templates, or
// publish JSON Schema have had to read source.
//
// SchemaOf walks a config struct via reflection and emits one
// FieldSchema per JSON-tagged field. Plugin authors annotate fields
// with three additional tags:
//
//	required:"true"               // boot fails if empty/zero
//	description:"prose, one line" // shown in templates / hover
//	default:"5000"                // documented default (cosmetic)
//	enum:"a,b,c"                  // allowed values
//
// All four tags are optional. Absence of any tag means "no
// metadata" — the field appears in the schema but with empty fields.
//
// This package owns the shape; presentation (template YAML, hover
// formatting, JSON Schema) lives in the consumer (abctl, rossoctl UI,
// future generators).

package pipeline

import (
	"reflect"
	"strings"
)

// SchemaProvider is implemented by plugins whose config field
// metadata should appear in the catalog and downstream tooling
// (abctl edit templates, future rossoctl-UI forms, JSON Schema
// generators). Plugins without configs (a2a-parser,
// inference-parser today) can omit this interface.
//
// The convention is a one-line method that delegates to SchemaOf:
//
//	func (p *MyPlugin) ConfigSchema() []FieldSchema {
//	    return SchemaOf(myPluginConfig{})
//	}
//
// The framework's catalog adapter type-asserts against this
// interface; absence is silently treated as "no field metadata."
type SchemaProvider interface {
	ConfigSchema() []FieldSchema
}

// FieldSchema describes one config field's metadata for tooling.
type FieldSchema struct {
	// Name is the JSON key (snake_case) — what operators type in YAML.
	Name string `json:"name"`

	// Type is a coarse-grained category sufficient to render templates
	// and pick value placeholders. One of:
	//   "string", "int", "bool", "[]string", "object", "unknown".
	// "object" indicates a nested struct whose fields populate Fields.
	// "unknown" covers shapes the helper hasn't been taught (maps,
	// slice-of-struct, etc.); the field still renders but without a
	// type-specific placeholder.
	Type string `json:"type"`

	// Required reports the `required:"true"` tag. Boot semantics are
	// the plugin's own concern — this field is just metadata.
	Required bool `json:"required,omitempty"`

	// Description is the `description:"..."` tag verbatim. Single-line.
	Description string `json:"description,omitempty"`

	// Default is the `default:"..."` tag verbatim. Cosmetic — the
	// authoritative default is whatever applyDefaults sets at runtime.
	Default string `json:"default,omitempty"`

	// Enum is the `enum:"a,b,c"` tag split on commas. Empty when the
	// field is not enum-shaped.
	Enum []string `json:"enum,omitempty"`

	// Fields is populated when Type is "object" (nested struct). The
	// outer field's Description applies to the struct as a whole; the
	// nested fields each carry their own metadata.
	Fields []FieldSchema `json:"fields,omitempty"`
}

// SchemaOf walks the given struct value and returns its field schemas.
// Pass a zero value (e.g. `SchemaOf(ibacConfig{})`) — the value is
// inspected for type only, not for runtime field values.
//
// Returns nil if the argument isn't a struct (or pointer to struct).
// Fields without a `json:` tag are skipped (including untagged
// anonymous/embedded structs — unlike encoding/json, which promotes
// them). Explicit JSON tagging is the existing wire convention, and
// untagged fields don't appear in the operator-facing YAML, so they
// have nothing to surface in the schema either. No current plugin
// uses untagged embedding; if one ever needs that, this helper would
// need to grow flattening support.
func SchemaOf(configType any) []FieldSchema {
	t := reflect.TypeOf(configType)
	if t == nil {
		return nil
	}
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	return schemaOfType(t, 0)
}

// maxSchemaDepth caps recursion in schemaOfType. Plugin configs in
// practice nest one or two levels (tokenexchange's identity + routes
// blocks are the deepest today); the cap is defensive against a
// future config struct with an inadvertent self-referential field
// (e.g. a *Self pointer) which would otherwise stack-overflow.
const maxSchemaDepth = 10

func schemaOfType(t reflect.Type, depth int) []FieldSchema {
	if depth > maxSchemaDepth {
		return nil
	}
	var out []FieldSchema
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		jsonTag := f.Tag.Get("json")
		if jsonTag == "" || jsonTag == "-" {
			continue
		}
		name := strings.SplitN(jsonTag, ",", 2)[0]
		if name == "" || name == "-" {
			continue
		}
		schema := FieldSchema{
			Name:        name,
			Type:        kindOf(f.Type),
			Required:    f.Tag.Get("required") == "true",
			Description: f.Tag.Get("description"),
			Default:     f.Tag.Get("default"),
		}
		if enumTag := f.Tag.Get("enum"); enumTag != "" {
			parts := strings.Split(enumTag, ",")
			schema.Enum = make([]string, 0, len(parts))
			for _, p := range parts {
				if v := strings.TrimSpace(p); v != "" {
					schema.Enum = append(schema.Enum, v)
				}
			}
		}
		if schema.Type == "object" {
			// Recurse with a depth bound (see maxSchemaDepth). Plugin
			// configs nest one or two levels in practice; the cap
			// guards against an inadvertent self-referential field
			// (e.g. *Self) silently blowing the stack.
			schema.Fields = schemaOfType(unwrap(f.Type), depth+1)
		}
		out = append(out, schema)
	}
	return out
}

// kindOf returns the coarse-grained Type tag for a field.
func kindOf(t reflect.Type) string {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "bool"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "int"
	case reflect.Slice:
		// Only []string gets a typed tag; other slices are "unknown"
		// (slice-of-struct, slice-of-map, etc. are rare in plugin
		// configs and don't render to a single YAML scalar template).
		if t.Elem().Kind() == reflect.String {
			return "[]string"
		}
		return "unknown"
	case reflect.Struct:
		return "object"
	default:
		return "unknown"
	}
}

// unwrap returns the underlying struct type for a struct or *struct.
// Returns t unchanged if it's not a struct shape.
func unwrap(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t
}
