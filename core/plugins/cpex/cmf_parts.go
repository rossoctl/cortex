package cpex

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/rossoctl/cortex/core/pipeline"
)

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

// pctxToCMFParts is the top-level protocol selector: it turns whichever
// protocol extension is populated into ordered CMF content parts plus the
// (entity_type, entity_name) routing coordinates. Precedence MCP →
// inference → A2A mirrors the priority order parsers populate in, and any
// unpopulated case falls back to a single opaque-text part carrying the
// raw body.
//
// Only MCP tool/prompt/resource calls and inference name an entity; A2A,
// MCP protocol mechanics and unparsed traffic return ("", "") and take the
// fallback identification (see identification). With egress set, an
// outbound request that names no entity is presented as a call to the tool
// http_egress instead (see presentsAsEgress, egressPart); its response
// keeps the protocol mapping.
//
// Splitting this from the cgo adapter keeps the MCP/inference/A2A → CMF
// decision unit-testable under CGO_ENABLED=0; manager_cpex.go converts the
// returned cmfParts into rcpex ContentParts and stamps the entity coords
// onto Extensions.Meta.
func pctxToCMFParts(pctx *pipeline.Context, isResponse, egress bool) (parts []cmfPart, entityType, entityName string) {
	if presentsAsEgress(pctx, isResponse, egress) {
		part := egressPart(pctx)
		return []cmfPart{part}, "tool", part.Name
	}
	switch {
	case pctx.Extensions.MCP != nil:
		part := mcpToCMFPart(pctx.Extensions.MCP, isResponse, pctx.Body, pctx.ResponseBody)
		et, en := cmfEntity(part)
		return []cmfPart{part}, et, en

	case pctx.Extensions.Inference != nil:
		// Inference routes by model (`- llm: <model>`). Only structured
		// text/tool parts cross — no opaque body part. Model persists from
		// the request phase, so entity routing works on responses too.
		if isResponse {
			return readInferenceResponse(pctx).parts, entityTypeLLM, pctx.Extensions.Inference.Model
		}
		return inferenceToCMFParts(pctx.Extensions.Inference), entityTypeLLM, pctx.Extensions.Inference.Model

	case pctx.Extensions.A2A != nil:
		return a2aToCMFParts(pctx.Extensions.A2A, isResponse, pctx.ResponseBody), "", ""

	default:
		body := pctx.Body
		if isResponse {
			body = pctx.ResponseBody
		}
		return []cmfPart{{Kind: cmfPartText, Text: string(body)}}, "", ""
	}
}

// inferenceToCMFParts maps an inference request's InferenceExtension onto
// ordered CMF content parts: one text part per request message with
// non-empty string content, in message order (inference-parser populated
// the extension before cpex on the forward request pass). Flattening the
// whole prompt to text parts makes every message redactable via
// cmf.llm_input, and the strict "non-empty string content, in order" rule
// is the contract applyInferenceRequestBodyMod relies on to map redacted
// text back onto the right body message positionally. Tool output becomes
// tool-result parts instead: an OpenAI chat message with role "tool", and
// each Anthropic tool_result block or Responses-API function_call_output
// item the parser kept in inf.ToolResults. A result with no text (an image
// block only) yields no part.
//
// The response is read from the body instead (readInferenceResponse).
func inferenceToCMFParts(inf *pipeline.InferenceExtension) []cmfPart {
	if inf == nil {
		return nil
	}
	var parts []cmfPart
	for _, m := range inf.Messages {
		switch {
		case m.Content == "":
		case m.Role == roleTool && m.ToolUseID != "":
			// A chat-completions result the parser also recorded in ToolResults; the
			// loop below emits the id-carrying copy, so this one would count twice.
		case m.Role == roleTool:
			parts = append(parts, cmfPart{Kind: cmfPartToolResult, Content: m.Content})
		default:
			parts = append(parts, cmfPart{Kind: cmfPartText, Text: m.Content})
		}
	}
	for _, r := range inf.ToolResults {
		if r.Content == "" {
			continue
		}
		parts = append(parts, cmfPart{
			Kind:          cmfPartToolResult,
			CorrelationID: r.ToolUseID,
			Content:       r.Content,
			IsError:       r.IsError,
		})
	}
	return parts
}

// roleTool is the OpenAI chat role of a message carrying tool output.
const roleTool = "tool"

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
	if err := json.Unmarshal(body, &envelope); err != nil {
		// Benign and common (e.g. a non-JSON tool result), so debug only.
		// Log the size, never the body — it can carry secrets/PII.
		slog.Debug("cpex: tools/call response body is not valid JSON; result.* predicates will not resolve",
			"bytes", len(body))
		return nil
	}
	result, _ := envelope["result"].(map[string]any)
	content := extractToolResultContent(result)
	if content == nil {
		// Valid JSON, but not a tools/call result shape we can project into
		// CMF (wrong types / missing fields). Debug only; no body content.
		slog.Debug("cpex: could not extract a tool result from the response (unexpected shape); result.* predicates will not resolve",
			"bytes", len(body))
	}
	return content
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
