package sparc

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// buildClarificationText renders an agent-facing message from SPARC's issues.
// The agent receives this (as a tool result or rewritten completion) and relays
// it — typically asking the user for the missing / corrected information —
// without learning that SPARC intervened.
func buildClarificationText(verdict ReflectVerdict) string {
	var b strings.Builder
	b.WriteString("This tool call was not executed because it could not be verified against the conversation. ")
	if len(verdict.Issues) == 0 {
		b.WriteString("Please confirm the request details with the user before retrying.")
		return b.String()
	}
	b.WriteString("Reason(s):")
	for _, iss := range verdict.Issues {
		if iss.Explanation == "" {
			continue
		}
		fmt.Fprintf(&b, "\n- %s", iss.Explanation)
		if c := correctionHint(iss.Correction); c != "" {
			fmt.Fprintf(&b, " (suggested fix: %s)", c)
		}
	}
	b.WriteString("\nAsk the user for the exact, missing, or corrected information, then retry.")
	return b.String()
}

func correctionHint(correction any) string {
	if correction == nil {
		return ""
	}
	if s, ok := correction.(string); ok {
		return s
	}
	b, err := json.Marshal(correction)
	if err != nil {
		return ""
	}
	return preview(string(b), 200)
}

// --- mcp mode: synthetic JSON-RPC MCP tool result ---

// buildMCPResultBody crafts a JSON-RPC 2.0 MCP tools/call result envelope that
// carries the clarification as tool output. Returned with HTTP 200, the agent's
// MCP client consumes it as a normal tool result.
func buildMCPResultBody(rpcID any, text string) ([]byte, error) {
	if rpcID == nil {
		rpcID = 0
	}
	return json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      rpcID,
		"result": map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
			"isError": false,
			"_meta":   map[string]any{"sparc": map[string]any{"reflected": true}},
		},
	})
}

// mcpResultAction returns a Reject whose Violation renders as an HTTP 200 MCP
// result body. Reject is the only pipeline primitive that short-circuits the
// outbound call and synthesizes a response; the 200 + JSON-RPC result body turn
// that into a normal tool result.
func mcpResultAction(rpcID any, text string) pipeline.Action {
	body, err := buildMCPResultBody(rpcID, text)
	if err != nil {
		return pipeline.DenyStatus(403, "sparc.blocked", text)
	}
	return pipeline.Action{
		Type: pipeline.Reject,
		Violation: &pipeline.Violation{
			Code:     "sparc.reflected",
			Reason:   "tool call replaced with a SPARC clarification result",
			Status:   200,
			Body:     body,
			BodyType: "application/json",
		},
	}
}

// --- inference mode: rewrite the OpenAI chat completion ---

// rewriteInferenceResponse replaces the model's tool-call turn with a plain
// assistant message carrying `text`, in OpenAI chat-completion shape, via
// SetResponseBody. The agent receives a normal completion (no tool_call) and
// relays the clarification to the user. Preserves id/model/object from the
// upstream response when present.
func rewriteInferenceResponse(pctx *pipeline.Context, text string) {
	assistant := map[string]any{"role": "assistant", "content": text}

	var orig map[string]any
	if err := json.Unmarshal(pctx.ResponseBody, &orig); err == nil {
		if choices, ok := orig["choices"].([]any); ok && len(choices) > 0 {
			if c0, ok := choices[0].(map[string]any); ok {
				c0["message"] = assistant
				c0["finish_reason"] = "stop"
				delete(c0, "delta")
				choices[0] = c0
				orig["choices"] = choices[:1]
				if out, err := json.Marshal(orig); err == nil {
					pctx.SetResponseBody(out)
					return
				}
			}
		}
	}
	// Fallback: synthesize a minimal OpenAI completion.
	out, _ := json.Marshal(map[string]any{
		"object":  "chat.completion",
		"choices": []map[string]any{{"index": 0, "message": assistant, "finish_reason": "stop"}},
	})
	pctx.SetResponseBody(out)
}
