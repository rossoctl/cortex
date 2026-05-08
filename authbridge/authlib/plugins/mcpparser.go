package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// MCPParser parses MCP JSON-RPC 2.0 request bodies and populates
// pctx.Extensions.MCP with the method, RPC ID, and raw params for
// downstream policy plugins.
type MCPParser struct{}

func NewMCPParser() *MCPParser { return &MCPParser{} }

func init() {
	RegisterPlugin("mcp-parser", func() pipeline.Plugin { return NewMCPParser() })
}

func (p *MCPParser) Name() string { return "mcp-parser" }

func (p *MCPParser) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		Writes:     []string{"mcp"},
		BodyAccess: true,
	}
}

func (p *MCPParser) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	// No Invocation recorded when the parser doesn't apply to this
	// message — empty body, non-JSON body, or JSON-but-not-JSON-RPC
	// (e.g. an OpenAI chat/completions body). Operators infer "mcp-
	// parser exists in this pipeline" from config, not per-event rows.
	if len(pctx.Body) == 0 {
		slog.Debug("mcp-parser: no body, skipping")
		return pipeline.Action{Type: pipeline.Continue}
	}

	var rpc jsonRPCRequest
	if err := json.Unmarshal(pctx.Body, &rpc); err != nil {
		slog.Debug("mcp-parser: body is not valid JSON-RPC", "error", err, "bodyLen", len(pctx.Body))
		return pipeline.Action{Type: pipeline.Continue}
	}
	// Empty method → body parses as JSON but isn't a JSON-RPC request
	// (e.g. an OpenAI chat/completions body also unmarshals into
	// jsonRPCRequest with zero-value fields). Don't attach a useless
	// MCPExtension to non-MCP traffic — downstream consumers shouldn't
	// see a phantom "mcp: {}" on every inference event.
	if rpc.Method == "" {
		slog.Debug("mcp-parser: body is JSON but not JSON-RPC, skipping", "bodyLen", len(pctx.Body))
		return pipeline.Action{Type: pipeline.Continue}
	}

	pctx.Extensions.MCP = &pipeline.MCPExtension{
		Method: rpc.Method,
		RPCID:  rpc.ID,
		Params: rpc.Params,
	}

	slog.Info("mcp-parser: request", "method", rpc.Method)
	slog.Debug("mcp-parser: payload", "method", rpc.Method, "body", truncate(string(pctx.Body), debugBodyMax))

	pctx.Observe("matched_" + rpc.Method)
	return pipeline.Action{Type: pipeline.Continue}
}

func (p *MCPParser) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	// No Invocation when the parser doesn't apply — request wasn't MCP
	// JSON-RPC or no response body to parse. The unparseable_response
	// case below IS recorded because it's diagnostic: the request WAS
	// MCP but the response couldn't be decoded, which usually signals
	// an upstream protocol bug worth surfacing.
	if len(pctx.ResponseBody) == 0 || pctx.Extensions.MCP == nil {
		return pipeline.Action{Type: pipeline.Continue}
	}

	rpc, ok := parseMCPResponse(pctx.ResponseBody)
	if !ok {
		slog.Debug("mcp-parser: response is not valid JSON-RPC or SSE", "bodyLen", len(pctx.ResponseBody))
		pctx.Skip("unparseable_response")
		return pipeline.Action{Type: pipeline.Continue}
	}

	if rpc.Error != nil {
		pctx.Extensions.MCP.Err = &pipeline.MCPError{
			Code:    rpc.Error.Code,
			Message: rpc.Error.Message,
			Data:    rpc.Error.Data,
		}
		slog.Info("mcp-parser: response error", "method", pctx.Extensions.MCP.Method, "code", rpc.Error.Code, "message", rpc.Error.Message)
		pctx.Observe("response_error")
		return pipeline.Action{Type: pipeline.Continue}
	}

	if rpc.Result != nil {
		pctx.Extensions.MCP.Result = rpc.Result
		slog.Info("mcp-parser: response", "method", pctx.Extensions.MCP.Method, "resultKeys", resultKeys(rpc.Result))
		slog.Debug("mcp-parser: response detail", "method", pctx.Extensions.MCP.Method, "body", truncate(string(pctx.ResponseBody), debugBodyMax))
	}

	pctx.Observe("matched_" + pctx.Extensions.MCP.Method + "_response")
	return pipeline.Action{Type: pipeline.Continue}
}

// parseMCPResponse handles both plain JSON-RPC responses and SSE event streams
// (used by MCP's Streamable HTTP transport). For SSE, the first data: line
// carrying a result or error wins. Malformed SSE data frames are logged at
// DEBUG so a broken upstream is observable rather than silently skipped.
func parseMCPResponse(body []byte) (jsonRPCResponse, bool) {
	var rpc jsonRPCResponse
	if json.Unmarshal(body, &rpc) == nil && (rpc.Result != nil || rpc.Error != nil) {
		return rpc, true
	}
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(data) == 0 {
			continue
		}
		var r jsonRPCResponse
		if err := json.Unmarshal(data, &r); err != nil {
			slog.Debug("mcp-parser: skipping malformed SSE data frame", "error", err, "data", truncate(string(data), 128))
			continue
		}
		if r.Result != nil || r.Error != nil {
			return r, true
		}
	}
	return jsonRPCResponse{}, false
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

type jsonRPCResponse struct {
	ID     any            `json:"id"`
	Result map[string]any `json:"result"`
	Error  *jsonRPCError  `json:"error"`
}

func resultKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

type jsonRPCRequest struct {
	Method string         `json:"method"`
	ID     any            `json:"id"`
	Params map[string]any `json:"params"`
}

// stringParam and mapParam are shared helpers used by both mcp-parser and a2a-parser.
func (r *jsonRPCRequest) stringParam(key string) string {
	v, _ := r.Params[key].(string)
	return v
}

func (r *jsonRPCRequest) mapParam(key string) map[string]any {
	v, _ := r.Params[key].(map[string]any)
	return v
}

// debugBodyMax caps how many characters of a body/content string a parser
// writes into debug logs. Large enough to capture a short user message or
// a tool_call response verbatim, small enough to keep log lines tractable.
const debugBodyMax = 512

// truncate clips s to max characters, appending "..." when truncated.
// Shared across parsers for consistent debug-log body formatting.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
