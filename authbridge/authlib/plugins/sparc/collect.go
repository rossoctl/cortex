package sparc

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/rossoctl/rossocortex/authbridge/authlib/pipeline"
)

// correlateInferenceContext pulls the conversation (incl. system prompt) and
// the tool inventory from the session's most recent outbound inference request
// — i.e. exactly what the agent sent to its LLM. Used in mcp mode, where the
// concrete tool call comes from the MCP request but the context must be sourced
// from the agent's LLM call. Returns ok=false when no inference event exists
// (no inference-parser, or session tracking off) so the caller can skip rather
// than reflect on partial data.
func correlateInferenceContext(pctx *pipeline.Context) (messages, toolSpecs []map[string]any, ok bool) {
	if pctx.Session == nil {
		return nil, nil, false
	}
	infs := pctx.Session.InferenceRequests()
	if len(infs) == 0 {
		return nil, nil, false
	}
	inf := infs[len(infs)-1].Inference
	if inf == nil || len(inf.Messages) == 0 {
		return nil, nil, false
	}
	messages, toolSpecs = inferenceMessagesAndTools(inf)
	return messages, toolSpecs, true
}

// inferenceInputs extracts messages + tool specs directly from the inference
// extension on the current request/response (inference mode — all inputs are
// co-located, no correlation needed).
func inferenceInputs(inf *pipeline.InferenceExtension) (messages, toolSpecs []map[string]any, ok bool) {
	if inf == nil || len(inf.Messages) == 0 {
		return nil, nil, false
	}
	messages, toolSpecs = inferenceMessagesAndTools(inf)
	return messages, toolSpecs, true
}

func inferenceMessagesAndTools(inf *pipeline.InferenceExtension) (messages, toolSpecs []map[string]any) {
	for _, m := range inf.Messages {
		messages = append(messages, map[string]any{"role": m.Role, "content": m.Content})
	}
	for _, t := range inf.Tools {
		toolSpecs = append(toolSpecs, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.Parameters,
			},
		})
	}
	return messages, toolSpecs
}

// buildToolCall renders the MCP tools/call as an OpenAI-style tool call (the
// shape SPARC expects). Arguments are serialized to a JSON string.
func buildToolCall(mcp *pipeline.MCPExtension, toolName string) map[string]any {
	return openAIToolCall(fmt.Sprintf("%v", mcpRPCID(mcp)), toolName, extractMCPToolArgs(mcp))
}

// openAIToolCall builds the canonical OpenAI function tool-call object. args is
// a JSON string (defaulted to "{}").
func openAIToolCall(id, name, args string) map[string]any {
	if args == "" {
		args = "{}"
	}
	return map[string]any{
		"id":       id,
		"type":     "function",
		"function": map[string]any{"name": name, "arguments": args},
	}
}

func extractMCPToolName(mcp *pipeline.MCPExtension) string {
	if mcp == nil || mcp.Method != "tools/call" {
		return ""
	}
	if name, ok := mcp.Params["name"].(string); ok {
		return name
	}
	return ""
}

func extractMCPToolArgs(mcp *pipeline.MCPExtension) string {
	if mcp == nil || mcp.Method != "tools/call" {
		return ""
	}
	args, ok := mcp.Params["arguments"]
	if !ok {
		return ""
	}
	b, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	return string(b)
}

func mcpRPCID(mcp *pipeline.MCPExtension) any {
	if mcp == nil {
		return nil
	}
	return mcp.RPCID
}

func sessionID(pctx *pipeline.Context) string {
	if pctx.Session != nil {
		return pctx.Session.ID
	}
	return ""
}

func firstExplanation(verdict ReflectVerdict) string {
	for _, iss := range verdict.Issues {
		if iss.Explanation != "" {
			return iss.Explanation
		}
	}
	return "tool call not grounded in the conversation"
}

func scoreString(score *float64) string {
	if score == nil {
		return ""
	}
	return fmt.Sprintf("%.2f", *score)
}

// anyGlobMatch reports whether name matches any path.Match glob in patterns.
func anyGlobMatch(patterns []string, name string) bool {
	for _, p := range patterns {
		if matched, _ := path.Match(p, name); matched {
			return true
		}
	}
	return false
}

// matchesAnyHost reports whether host matches any glob (path.Match), port stripped.
func matchesAnyHost(patterns []string, host string) bool {
	if host == "" {
		return false
	}
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	return anyGlobMatch(patterns, host)
}

// preview truncates s to n runes, appending an ellipsis when truncated.
func preview(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
