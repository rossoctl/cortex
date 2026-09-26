package pipeline

import "encoding/json"

// Configurable is an optional interface plugins implement when they need
// per-instance configuration. The pipeline builder calls Configure exactly
// once per instance, during pipeline construction, before Start. Plugins
// that don't need config simply omit this interface; the builder skips
// them.
//
// The contract is deliberately narrow:
//   - The raw argument is the plugin's own config subtree from the runtime
//     YAML, as json.RawMessage — the framework does not interpret it.
//   - Plugins decode with DisallowUnknownFields so stale or misspelled keys
//     are rejected loudly at startup rather than silently ignored.
//   - Plugins apply their own defaults and run their own validation, then
//     construct any internal state needed at request time.
//   - An error from Configure aborts pipeline construction and takes the
//     process down; do not partial-initialize and return nil.
//
// See docs/plugin-reference.md for the recommended shape
// of per-plugin config and a worked example.
type Configurable interface {
	Configure(raw json.RawMessage) error
}
