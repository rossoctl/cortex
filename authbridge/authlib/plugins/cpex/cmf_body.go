package cpex

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// sessionIDFromHeaders returns the X-Session-Id request header, the
// session-correlation key AuthBridge threads into CPEX's session resolver
// (tier-0 Agent.SessionID) for non-A2A traffic. MCP and inference requests
// carry no native session id, so an upstream supplies one here to let
// session-scoped CPEX state — e.g. taint labels — persist across the
// separate request/response cycles of one logical session. Returns "" when
// absent. Tag-free so it's unit-tested without cgo (buildCMF, which uses
// it, is cgo-gated).
func sessionIDFromHeaders(h http.Header) string {
	return h.Get("X-Session-Id")
}

// cmfPartKind enumerates the structured CMF content part buildCMF emits
// for a given MCP message. Kept protocol-neutral (no rcpex import) so the
// MCP→CMF decision is unit-testable without cgo/FFI; the cgo adapter
// (manager_cpex.go) converts a cmfPart into the matching rcpex ContentPart.
type cmfPartKind int

const (
	// cmfPartText is the opaque text fallback. Used for protocol
	// mechanics (tools/list, initialize, …) and when no MCP extension
	// was parsed — CPEX gets the raw body as a single text part, no
	// per-tool route matches, and the request flows unchanged. This
	// preserves pre-structured-mapping behavior for non-action traffic.
	cmfPartText cmfPartKind = iota
	cmfPartToolCall
	cmfPartPromptRequest
	cmfPartResourceRef
	cmfPartToolResult
)

// cmfPart is a protocol-neutral description of one CMF content part. The
// cgo adapter maps it onto an rcpex constructor (NewToolCallPart, …). Only
// the fields relevant to Kind are populated.
type cmfPart struct {
	Kind cmfPartKind

	// Name is the tool/prompt name. It is the routing key CPEX matches
	// against per-tool routes (`- tool: get_compensation`), so it MUST be
	// set for ToolCall / PromptRequest / ToolResult parts.
	Name string

	// Arguments is the tool_call / prompt_request argument object that
	// APL `args.*` predicates (and Cedar `${args.*}`) evaluate against.
	Arguments map[string]any

	// URI is the resources/read target (ResourceRef parts).
	URI string

	// CorrelationID threads the JSON-RPC request id through as the
	// part's *_id field so request and response parts correlate.
	CorrelationID string

	// Content is the tool_result payload (ToolResult parts) — the parsed
	// inner object, not the MCP result envelope.
	Content any

	// IsError flags a tool_result built from a JSON-RPC error response.
	IsError bool

	// Text is the opaque-fallback body (cmfPartText only).
	Text string
}

// mcpToCMFPart decides the structured CMF content part for an MCP message.
// `isResponse` selects the phase; requestBody / responseBody supply the
// payload (and the opaque text fallback) for that phase.
//
// On the response phase the tool result is parsed from responseBody, NOT
// from mcp.Result: response hooks run in reverse pipeline order, so the
// cpex plugin executes BEFORE mcp-parser and mcp.Result isn't populated
// yet. The tool name still persists in mcp.Params from the request phase.
func mcpToCMFPart(mcp *pipeline.MCPExtension, isResponse bool, requestBody, responseBody []byte) cmfPart {
	if mcp == nil {
		body := requestBody
		if isResponse {
			body = responseBody
		}
		return cmfPart{Kind: cmfPartText, Text: string(body)}
	}
	corrID := stringifyRPCID(mcp.RPCID)

	if isResponse {
		// Response phase. Only tools/call yields a structured result
		// today; other methods fall back to opaque text.
		if mcp.Method == "tools/call" {
			return cmfPart{
				Kind:          cmfPartToolResult,
				Name:          mcpParamName(mcp.Params),
				CorrelationID: corrID,
				Content:       extractToolResultFromBody(responseBody),
				IsError:       mcp.Err != nil,
			}
		}
		return cmfPart{Kind: cmfPartText, Text: string(responseBody)}
	}

	// Request phase.
	switch mcp.Method {
	case "tools/call":
		return cmfPart{
			Kind:          cmfPartToolCall,
			Name:          mcpParamName(mcp.Params),
			Arguments:     mcpParamArgs(mcp.Params),
			CorrelationID: corrID,
		}
	case "prompts/get":
		return cmfPart{
			Kind:          cmfPartPromptRequest,
			Name:          mcpParamName(mcp.Params),
			Arguments:     mcpParamArgs(mcp.Params),
			CorrelationID: corrID,
		}
	case "resources/read":
		return cmfPart{
			Kind:          cmfPartResourceRef,
			URI:           mcpParamURI(mcp.Params),
			CorrelationID: corrID,
		}
	default:
		// Protocol mechanics (tools/list, initialize, …) — no per-tool
		// route to match. Pass the raw body through as text.
		return cmfPart{Kind: cmfPartText, Text: string(requestBody)}
	}
}

// cmfEntity maps a cmfPart to the (entity_type, entity_name) pair CPEX's
// route resolver keys on. cpex-core only dispatches a route's APL policy
// handlers (require / cedar / delegate / field-redaction) when
// Extensions.meta carries BOTH entity_type and entity_name (see
// crates/cpex-core/src/manager.rs filter_entries_by_route); without them
// the per-tool gates never fire and only always-on global plugins run.
// caller leaves meta unset so non-action traffic isn't force-routed.
func cmfEntity(part cmfPart) (entityType, entityName string) {
	switch part.Kind {
	case cmfPartToolCall, cmfPartToolResult:
		return "tool", part.Name
	case cmfPartPromptRequest:
		return "prompt", part.Name
	case cmfPartResourceRef:
		return "resource", part.URI
	default:
		return "", ""
	}
}

// pctxToCMFParts is the top-level protocol selector: it turns whichever
// protocol extension is populated into ordered CMF content parts plus the
// (entity_type, entity_name) routing coordinates. Precedence MCP →
// inference → A2A mirrors the priority order parsers populate in, and any
// unpopulated case falls back to a single opaque-text part carrying the
// raw body — preserving pre-structured-mapping behavior for traffic no
// parser claimed (control-plane RPCs, unknown formats).
//
// Splitting this from the cgo adapter keeps the MCP/inference/A2A → CMF
// decision unit-testable under CGO_ENABLED=0; manager_cpex.go converts the
// returned cmfParts into rcpex ContentParts and stamps the entity coords
// onto Extensions.Meta.
func pctxToCMFParts(pctx *pipeline.Context, isResponse bool) (parts []cmfPart, entityType, entityName string) {
	switch {
	case pctx.Extensions.MCP != nil:
		part := mcpToCMFPart(pctx.Extensions.MCP, isResponse, pctx.Body, pctx.ResponseBody)
		et, en := cmfEntity(part)
		return []cmfPart{part}, et, en

	case pctx.Extensions.Inference != nil:
		// Inference is always an action; route by model so operators can
		// scope per-route APL gates with `- model: <name>`. Only structured
		// text/tool parts cross — no opaque body part. Model persists from
		// the request phase, so entity routing works on responses too.
		return inferenceToCMFParts(pctx.Extensions.Inference, isResponse, pctx.ResponseBody),
			"model", pctx.Extensions.Inference.Model

	case pctx.Extensions.A2A != nil:
		// Route A2A by its JSON-RPC method (message/send, message/stream),
		// which persists from the request phase.
		return a2aToCMFParts(pctx.Extensions.A2A, isResponse, pctx.ResponseBody),
			"a2a_method", pctx.Extensions.A2A.Method

	default:
		body := pctx.Body
		if isResponse {
			body = pctx.ResponseBody
		}
		return []cmfPart{{Kind: cmfPartText, Text: string(body)}}, "", ""
	}
}

// inferenceToCMFParts maps an InferenceExtension onto ordered CMF content
// parts for the given phase.
//
// Request phase: one text part per request message with non-empty string
// content, in message order (read from the extension, which inference-parser
// populated before cpex on the forward request pass). Flattening the whole
// prompt to text parts makes every message redactable via cmf.llm_input,
// and the strict "non-empty string content, in order" rule is the contract
// applyInferenceRequestBodyMod relies on to map redacted text back onto the
// right body message positionally.
//
// Response phase: parsed from responseBody, NOT from inf.Completion.
// Response hooks run in reverse pipeline order, so cpex executes BEFORE
// inference-parser's OnResponse and inf.Completion isn't populated yet —
// exactly the situation mcpToCMFPart handles for tool results. See
// inferenceResponseParts.
func inferenceToCMFParts(inf *pipeline.InferenceExtension, isResponse bool, responseBody []byte) []cmfPart {
	if inf == nil {
		return nil
	}
	if isResponse {
		return inferenceResponseParts(responseBody)
	}
	var parts []cmfPart
	for _, m := range inf.Messages {
		if m.Content == "" {
			continue
		}
		parts = append(parts, cmfPart{Kind: cmfPartText, Text: m.Content})
	}
	return parts
}

// a2aToCMFParts maps an A2AExtension onto ordered CMF content parts.
//
// Request phase: one text part per text-kind message part with non-empty
// content, in order (from the extension). Only kind=="text" parts
// participate — data/file parts carry JSON blobs / URIs that a text
// redactor would corrupt, so they're excluded from BOTH the read mapping
// here and the write-back in applyA2ARequestBodyMod, keeping positional
// alignment exact.
//
// Response phase: parsed from responseBody (not a2a.Artifact, which
// a2a-parser hasn't populated yet on the reverse-order response pass). See
// a2aResponseParts.
func a2aToCMFParts(a2a *pipeline.A2AExtension, isResponse bool, responseBody []byte) []cmfPart {
	if a2a == nil {
		return nil
	}
	if isResponse {
		return a2aResponseParts(responseBody)
	}
	var parts []cmfPart
	for _, p := range a2a.Parts {
		if p.Kind != "text" || p.Content == "" {
			continue
		}
		parts = append(parts, cmfPart{Kind: cmfPartText, Text: p.Content})
	}
	return parts
}

// parseToolArgs decodes an LLM tool call's raw argument string (which the
// model emits as a JSON object string) into the map APL `args.*`
// predicates evaluate against. Malformed model output is preserved rather
// than dropped: on a parse miss the raw string is wrapped as
// {"_raw": <string>} so a policy can still see (and a redactor still
// reach) the content. Empty input yields nil.
func parseToolArgs(raw string) map[string]any {
	if raw == "" {
		return nil
	}
	var args map[string]any
	if json.Unmarshal([]byte(raw), &args) == nil {
		return args
	}
	return map[string]any{"_raw": raw}
}

// inferProvider derives a coarse provider label from a model id for
// LLMExtension.Provider. Best-effort and conservative — it returns ""
// (provider unset) rather than guess for unrecognized model families, so
// a policy keying on provider only ever sees a value we're confident in.
func inferProvider(model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.HasPrefix(m, "gpt-"), strings.HasPrefix(m, "o1"), strings.HasPrefix(m, "o3"),
		strings.HasPrefix(m, "text-"), strings.Contains(m, "davinci"):
		return "openai"
	case strings.HasPrefix(m, "claude"):
		return "anthropic"
	case strings.HasPrefix(m, "gemini"):
		return "google"
	default:
		return ""
	}
}

// inferenceHistory renders the request conversation as role-tagged
// entries for CPEX's AgentExtension.Conversation.History, giving policies
// that need turn/role context (not just flat redactable text) a structured
// view. Tag-free + []map[string]any so it's unit-testable and assigns
// cleanly into the rcpex []any history field. Empty-content messages are
// dropped to match the text-part flattening. Returns nil when empty.
func inferenceHistory(inf *pipeline.InferenceExtension) []map[string]any {
	if inf == nil {
		return nil
	}
	var out []map[string]any
	for _, m := range inf.Messages {
		if m.Content == "" {
			continue
		}
		out = append(out, map[string]any{"role": m.Role, "content": m.Content})
	}
	return out
}

// extractToolResultFromBody parses a JSON-RPC tools/call response body and
// returns the inner tool result payload (via extractToolResultContent on
// the `result` object). Used on the response phase, where the cpex plugin
// runs before mcp-parser and so must parse the body itself rather than
// read the not-yet-populated mcp.Result.
func extractToolResultFromBody(body []byte) any {
	if len(body) == 0 {
		return nil
	}
	var envelope map[string]any
	if json.Unmarshal(body, &envelope) != nil {
		return nil
	}
	result, _ := envelope["result"].(map[string]any)
	return extractToolResultContent(result)
}

// extractToolResultContent pulls the inner tool payload out of an MCP
// tools/call result envelope so APL `result.*` predicates resolve against
// the real data (e.g. `result.ssn`), not the MCP content wrapper. Prefers
// the typed structuredContent (MCP 2025-06-18); falls back to parsing the
// first text block as JSON; on parse-miss wraps the raw text as
// {"text": <raw>}.
func extractToolResultContent(result map[string]any) any {
	if result == nil {
		return nil
	}
	if sc, ok := result["structuredContent"]; ok {
		return sc
	}
	content, ok := result["content"].([]any)
	if !ok {
		return nil
	}
	for _, b := range content {
		block, ok := b.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := block["type"].(string); t != "text" {
			continue
		}
		s, ok := block["text"].(string)
		if !ok {
			continue
		}
		var inner any
		if json.Unmarshal([]byte(s), &inner) == nil {
			return inner
		}
		return map[string]any{"text": s}
	}
	return nil
}

// mcpParamName extracts params.name (tool / prompt name) as a string.
func mcpParamName(params map[string]any) string {
	if params == nil {
		return ""
	}
	if n, ok := params["name"].(string); ok {
		return n
	}
	return ""
}

// mcpParamArgs extracts params.arguments as an object, or nil.
func mcpParamArgs(params map[string]any) map[string]any {
	if params == nil {
		return nil
	}
	if a, ok := params["arguments"].(map[string]any); ok {
		return a
	}
	return nil
}

// mcpParamURI extracts params.uri (resources/read target) as a string.
func mcpParamURI(params map[string]any) string {
	if params == nil {
		return ""
	}
	if u, ok := params["uri"].(string); ok {
		return u
	}
	return ""
}

// stringifyRPCID renders a JSON-RPC id (string | number | null) as the
// stable string CPEX uses for the part's correlation id. JSON numbers
// decode to float64; format them without a spurious decimal so id 1 stays
// "1", not "1.000000" or "1e+00".
func stringifyRPCID(id any) string {
	switch v := id.(type) {
	case nil:
		return ""
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case json.Number:
		return v.String()
	default:
		return fmt.Sprintf("%v", v)
	}
}

// MCPRequestBodyMod describes what changes to apply to an MCP JSON-RPC
// request body. The cgo adapter extracts these values from a CPEX-
// modified CMF Message and hands them to applyMCPRequestBodyMod.
//
// Exactly one field is consumed per method:
//
//	tools/call     → NewArguments  → params.arguments
//	prompts/get    → NewArguments  → params.arguments
//	resources/read → NewURI        → params.uri
//
// Other methods are no-ops — the operator's APL policy can't rewrite
// protocol mechanics (initialize, tools/list, etc.) because there's
// no semantically meaningful target for the rewrite.
type MCPRequestBodyMod struct {
	NewArguments map[string]any
	NewURI       string
}

// applyMCPRequestBodyMod rewrites pctx.Body, which is expected to be
// an MCP JSON-RPC request. Returns mutated=true when the body was
// changed and SetBody was called; returns mutated=false (with no
// error) when the original body didn't match the expected shape
// (e.g., parsed JSON has no `params` object, method is unsupported,
// or the mod struct's relevant field is empty for the method).
func applyMCPRequestBodyMod(pctx *pipeline.Context, method string, mod MCPRequestBodyMod) (mutated bool, err error) {
	if len(pctx.Body) == 0 {
		return false, nil
	}
	var envelope map[string]any
	if err := json.Unmarshal(pctx.Body, &envelope); err != nil {
		return false, fmt.Errorf("decode request body as JSON: %w", err)
	}
	params, _ := envelope["params"].(map[string]any)
	if params == nil {
		return false, nil
	}

	switch method {
	case "tools/call", "prompts/get":
		if len(mod.NewArguments) == 0 {
			return false, nil
		}
		params["arguments"] = mod.NewArguments
	case "resources/read":
		if mod.NewURI == "" {
			return false, nil
		}
		params["uri"] = mod.NewURI
	default:
		return false, nil
	}

	newBody, err := json.Marshal(envelope)
	if err != nil {
		return false, fmt.Errorf("re-serialize request body: %w", err)
	}
	pctx.SetBody(newBody)
	return true, nil
}

// applyMCPResponseBodyMod rewrites pctx.ResponseBody for a tools/call
// JSON-RPC response. The new content replaces:
//
//   - result.content[].text — the canonical MCP text block, stringified
//     as JSON (pretty-printed when possible).
//   - result.structuredContent — only when the original response had it.
//     Don't introduce structuredContent on a response that didn't carry
//     it; clients sniffing for the new shape would be surprised.
//
// Returns mutated=true when SetResponseBody was called; mutated=false
// when the body wasn't a tools/call response, had no result.content,
// or the response had no replaceable text block.
func applyMCPResponseBodyMod(pctx *pipeline.Context, method string, newContent any) (mutated bool, err error) {
	if method != "tools/call" {
		return false, nil
	}
	if len(pctx.ResponseBody) == 0 || newContent == nil {
		return false, nil
	}
	var envelope map[string]any
	if err := json.Unmarshal(pctx.ResponseBody, &envelope); err != nil {
		return false, fmt.Errorf("decode response body as JSON: %w", err)
	}
	result, ok := envelope["result"].(map[string]any)
	if !ok {
		return false, nil
	}

	replaced := false

	// Primary path: rewrite the first text block in result.content[].
	// This is what every MCP client we see today reads from.
	if contentArr, ok := result["content"].([]any); ok {
		for _, block := range contentArr {
			blockObj, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := blockObj["type"].(string); ok && t == "text" {
				blockObj["text"] = stringifyForTextBlock(newContent)
				replaced = true
				break
			}
		}
	}

	// Secondary path: update structuredContent when it was already
	// present. Operators can opt clients into the newer
	// structuredContent shape by populating it in the upstream
	// response; CPEX rewrites it consistently. We never introduce
	// it on a response that lacked it.
	if _, has := result["structuredContent"]; has {
		result["structuredContent"] = newContent
		replaced = true
	}

	if !replaced {
		return false, nil
	}

	newBody, err := json.Marshal(envelope)
	if err != nil {
		return false, fmt.Errorf("re-serialize response body: %w", err)
	}
	pctx.SetResponseBody(newBody)
	return true, nil
}

// stringifyForTextBlock turns the modified content value back into the
// string the MCP text block expects. We try pretty-printed JSON first
// (matches the upstream MCP server's typical output, makes diffs
// readable in session events); fall back to compact JSON on error;
// fall back to %v on impossible-to-marshal types.
func stringifyForTextBlock(v any) string {
	if s, ok := v.(string); ok {
		// Common case: the redactor handed back a string verbatim.
		return s
	}
	if b, err := json.MarshalIndent(v, "", "  "); err == nil {
		return string(b)
	}
	if b, err := json.Marshal(v); err == nil {
		return string(b)
	}
	return fmt.Sprintf("%v", v)
}
