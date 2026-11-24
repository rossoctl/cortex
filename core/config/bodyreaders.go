package config

// bodyReaderPlugins are the plugins whose capabilities read the body and write
// no body.
var bodyReaderPlugins = []string{"a2a-parser", "ibac", "inference-parser", "litellm-budget-track", "mcp-parser"}

// BodyReaderPlugins returns the outbound plugins that must come before every
// request-body writer (pipeline.validateCapabilities), sorted. The slice is a new
// copy. A test in core/plugins/all keeps the list in step with the registry.
func BodyReaderPlugins() []string { return append([]string(nil), bodyReaderPlugins...) }
