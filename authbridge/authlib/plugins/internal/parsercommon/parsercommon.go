// Package parsercommon holds small helpers shared by the in-tree
// protocol parser plugins (a2a-parser, mcp-parser, inference-parser).
// Lives under internal/ so only packages under authlib/plugins/ can
// import it — third-party plugins are expected to either depend on
// the public pipeline surface or roll their own parsing. The contents
// here are stable but intentionally undocumented as a public API.
package parsercommon

// JSONRPCRequest is the minimal JSON-RPC 2.0 request shape the a2a and
// mcp parsers decode. Fields intentionally match the permissive
// `any`-typed variant of the protocol: the parsers never re-emit these
// on the wire so strict typing here would only cost flexibility.
type JSONRPCRequest struct {
	Method string         `json:"method"`
	ID     any            `json:"id"`
	Params map[string]any `json:"params"`
}

// StringParam returns the Params[key] value cast to string, or "" if
// absent / wrong type.
func (r *JSONRPCRequest) StringParam(key string) string {
	v, _ := r.Params[key].(string)
	return v
}

// MapParam returns the Params[key] value cast to map[string]any, or
// nil if absent / wrong type.
func (r *JSONRPCRequest) MapParam(key string) map[string]any {
	v, _ := r.Params[key].(map[string]any)
	return v
}

// DebugBodyMax caps how many characters of a body/content string a
// parser writes into debug logs. Large enough to capture a short user
// message or a tool_call response verbatim, small enough to keep log
// lines tractable.
const DebugBodyMax = 512

// Truncate clips s to max characters, appending "..." when truncated.
// Shared across parsers for consistent debug-log body formatting.
func Truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
