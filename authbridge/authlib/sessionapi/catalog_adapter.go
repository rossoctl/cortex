package sessionapi

import (
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins"
)

// PluginsCatalog adapts plugins.Catalog() into the wire-shaped
// CatalogEntry slice WithCatalog expects. Three binaries (authbridge-
// proxy, -envoy, -lite) plug it in identically; centralizing the
// conversion here keeps the field list one-place.
//
// Direction is left empty: the catalog describes plugin TYPES, and
// most plugins can be configured into either chain (parsers especially).
// abctl renders direction only for the active pipeline, where the
// answer is positional, not type-level.
//
// Fields is populated for plugins that implement
// pipeline.SchemaProvider (most config-bearing plugins). Plugins
// without configs emit a nil Fields slice and the wire format
// elides it via `omitempty`.
func PluginsCatalog() []CatalogEntry {
	src := plugins.Catalog()
	out := make([]CatalogEntry, len(src))
	for i, e := range src {
		n := e.Capabilities.Normalize()
		out[i] = CatalogEntry{
			Name:        e.Name,
			ReadsBody:   n.ReadsBody,
			Requires:    n.Requires,
			RequiresAny: n.RequiresAny,
			Description: n.Description,
			Fields:      convertFieldSchemas(e.Fields),
		}
	}
	return out
}

// convertFieldSchemas maps the framework-level FieldSchema slice to
// the wire-level FieldSchemaEntry slice. Recurses into nested struct
// schemas. Returns nil for empty input so the JSON marshaller can
// elide the field via `omitempty`.
func convertFieldSchemas(in []pipeline.FieldSchema) []FieldSchemaEntry {
	if len(in) == 0 {
		return nil
	}
	out := make([]FieldSchemaEntry, len(in))
	for i, f := range in {
		out[i] = FieldSchemaEntry{
			Name:        f.Name,
			Type:        f.Type,
			Required:    f.Required,
			Description: f.Description,
			Default:     f.Default,
			Enum:        append([]string(nil), f.Enum...),
			Fields:      convertFieldSchemas(f.Fields),
		}
	}
	return out
}
