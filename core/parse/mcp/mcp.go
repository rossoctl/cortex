// Package mcp parses MCP JSON-RPC 2.0 requests and responses from plain values.
// It computes no cost and does not use the plugin engine (#1503).
// The mcp-parser plugin is a thin wrapper over it.
package mcp

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/rossoctl/cortex/core/bypass"
	"github.com/rossoctl/cortex/core/internal/parsercommon"
	"github.com/rossoctl/cortex/core/model"
)

// Synthetic method names emitted on body-less MCP transport-layer
// requests where there's no JSON-RPC method on the wire to report.
//
// The "$" prefix is non-standard — neither MCP nor JSON-RPC 2.0
// formally reserves it (JSON-RPC 2.0 §6 only reserves "rpc.*").
// We chose it because no current MCP spec method uses "$" and
// because it's visually distinct from real category/action method
// names; operators reading agentop can tell at a glance that these
// aren't methods that appeared in the request body. If a future MCP
// revision starts using "$" prefixes, switch this scheme to a less
// likely sentinel (e.g. "_transport/stream") at that time.
const (
	syntheticTransportStream    = "$transport/stream"
	syntheticTransportTerminate = "$transport/terminate"
)

// isMCPAction reports whether a JSON-RPC method name names a user-
// meaningful side-effect operation that guardrails should judge.
// The list is small and grows only when MCP introduces a new method
// that carries user intent on the wire. Everything not in this list
// — protocol setup, capability discovery, subscription management,
// notifications, etc. — is treated as protocol mechanics with
// IsAction=false (the zero value).
//
// Aligned with MCP spec revision 2025-03-26 (Streamable HTTP). When
// MCP adds a new action-shaped method (a hypothetical
// "tools/execute_remote", a "prompts/render_with_data", etc.), update
// this list. The audit anchor is the spec-revision string so future
// maintainers have a date to compare against rather than re-deriving
// the action set from scratch.
func isMCPAction(method string) bool {
	switch method {
	case "tools/call", "prompts/get", "resources/read":
		return true
	}
	return false
}

// isMCPMethod reports whether a JSON-RPC method name belongs to the MCP
// protocol namespace (spec revision 2025-06-18, Streamable HTTP). MCP
// methods are either the bare handshake verbs initialize/ping or are
// slash-namespaced under notifications/, tools/, resources/, prompts/,
// completion/, logging/, sampling/, roots/, and elicitation/. An empty
// method (non-JSON-RPC body) matches nothing and is declined. Gating on
// the namespace is what distinguishes MCP from A2A traffic, which also
// rides JSON-RPC 2.0 but under message/, tasks/, and agent/. When a
// future MCP revision adds a top-level namespace, extend this set (the
// spec-revision string above is the audit anchor).
func isMCPMethod(method string) bool {
	switch method {
	case "initialize", "ping":
		return true
	}
	return strings.HasPrefix(method, "notifications/") ||
		strings.HasPrefix(method, "tools/") ||
		strings.HasPrefix(method, "resources/") ||
		strings.HasPrefix(method, "prompts/") ||
		strings.HasPrefix(method, "completion/") ||
		strings.HasPrefix(method, "logging/") ||
		strings.HasPrefix(method, "sampling/") ||
		strings.HasPrefix(method, "roots/") ||
		strings.HasPrefix(method, "elicitation/")
}

// ParseRequest parses one MCP request from plain values. It returns nil
// when the parser does not claim the request. paths is the matcher of the
// MCP endpoint paths for body-less requests. A nil paths matches no path.
func ParseRequest(method, path string, headers http.Header, rawBody []byte, paths *bypass.Matcher) *model.MCPExtension {
	// Parse the bytes as sent after any zstd Content-Encoding, as
	// inference-parser does, so a compressed call is claimed rather than
	// passed through unparsed.
	body := parsercommon.DecodeRequestBody(headers, rawBody)
	// Body-less transport-layer detection. Scoped to the configured
	// MCP-endpoint paths because there's no protocol payload to
	// confirm; without the path narrow, every body-less GET in the
	// cluster would risk being mis-classified as an MCP SSE channel.
	if len(body) == 0 {
		if paths != nil && paths.Match(path) {
			switch {
			case method == "DELETE" && headers.Get("Mcp-Session-Id") != "":
				// MCP Streamable HTTP session termination per spec —
				// the Mcp-Session-Id header is set by the MCP client
				// SDK, not user input, so it's a precise distinguisher
				// from a real "DELETE /api/users/42" action call.
				slog.Info("mcp-parser: session terminate", "path", model.SanitizeLogValue(path))
				return &model.MCPExtension{
					Method: syntheticTransportTerminate,
					// IsAction defaults to false — protocol mechanics.
				}

			case method == "GET":
				// MCP Streamable HTTP server-to-client SSE channel-open.
				// Heuristic recognition: any body-less GET on a
				// configured MCP path. If the request turns out not to
				// be MCP, the worst-case effect is that guardrails
				// downstream see a "transport/stream" extension and skip
				// it — same effect as the pre-classification behavior
				// of letting body-less GETs through.
				slog.Info("mcp-parser: transport stream", "path", model.SanitizeLogValue(path))
				return &model.MCPExtension{
					Method: syntheticTransportStream,
					// IsAction defaults to false — protocol mechanics.
				}
			}
		}
		// Empty body, no MCP-shaped transport pattern matched. Don't
		// attach an extension — the parser doesn't claim this request.
		// Operators infer "mcp-parser exists in this pipeline" from
		// config, not per-event rows.
		slog.Debug("mcp-parser: no body, skipping")
		return nil
	}

	// A top-level non-object body (array, string, number, null) is not
	// JSON-RPC; peek the first byte so it takes the calm skip path below
	// rather than logging as malformed.
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	isObject := len(trimmed) > 0 && trimmed[0] == '{'

	var rpc parsercommon.JSONRPCRequest
	if isObject {
		if err := json.Unmarshal(body, &rpc); err != nil {
			slog.Debug("mcp-parser: body is not valid JSON-RPC", "error", err, "bodyLen", len(body), "path", model.SanitizeLogValue(path))
			return nil
		}
	}
	// Claim only MCP-namespace methods. Both MCP and A2A ride JSON-RPC 2.0
	// over HTTP, so "the method is non-empty" cannot tell them apart — the
	// method namespace is the only in-body signal that does. Gating here
	// keeps mcp-parser from attaching an MCPExtension to A2A traffic
	// (message/send, tasks/get, agent/*, ...) or to a non-JSON-RPC body
	// such as an inference /v1/messages call (which unmarshals into
	// JSONRPCRequest with an empty Method) — which would otherwise show a
	// phantom "mcp: {}" on every such event. a2a-parser has the mirror
	// guard for its own namespace.
	if !isObject || !isMCPMethod(rpc.Method) {
		slog.Debug("mcp-parser: not an MCP-namespace method, skipping", "method", model.SanitizeLogValue(rpc.Method),
			"bodyLen", len(body), "path", model.SanitizeLogValue(path))
		return nil
	}

	ext := &model.MCPExtension{
		Method:   rpc.Method,
		RPCID:    rpc.ID,
		Params:   rpc.Params,
		IsAction: isMCPAction(rpc.Method),
	}

	slog.Info("mcp-parser: request", "method", model.SanitizeLogValue(rpc.Method), "isAction", ext.IsAction)
	slog.Debug("mcp-parser: payload", "method", model.SanitizeLogValue(rpc.Method),
		"body", model.SanitizeLogValue(parsercommon.Truncate(string(body), parsercommon.DebugBodyMax)))
	return ext
}

// ParseResponse reads one MCP response into ext: a plain JSON-RPC response,
// or an SSE body or frame (MCP's Streamable HTTP transport). For SSE, the
// first data: line carrying a result or error wins. It sets ext.Err for an
// error and ext.Result for a result. It returns false, and changes nothing,
// when body holds no JSON-RPC result or error. JSON-RPC gives a request one
// response, so a caller that folds frames stops after the first true.
func ParseResponse(body []byte, ext *model.MCPExtension) bool {
	rpc, ok := parseResponse(body)
	if !ok {
		return false
	}
	if rpc.Error != nil {
		ext.Err = &model.MCPError{
			Code:    rpc.Error.Code,
			Message: rpc.Error.Message,
			Data:    rpc.Error.Data,
		}
		return true
	}
	if rpc.Result != nil {
		ext.Result = rpc.Result
	}
	return true
}

// parseResponse handles both plain JSON-RPC responses and SSE event streams
// (used by MCP's Streamable HTTP transport). For SSE, the first data: line
// carrying a result or error wins. Malformed SSE data frames are logged at
// DEBUG so a broken upstream is observable rather than silently skipped.
func parseResponse(body []byte) (jsonRPCResponse, bool) {
	var rpc jsonRPCResponse
	if json.Unmarshal(body, &rpc) == nil && (rpc.Result != nil || rpc.Error != nil) {
		return rpc, true
	}
	// Normalized before the LF split: a leading BOM or CR-only line endings are
	// wire-legal SSE, and a raw split reads such a body as one unparseable line —
	// leaving the extension empty and the exchange unclassified (#1567).
	for _, line := range bytes.Split(parsercommon.NormalizeSSE(body), []byte("\n")) {
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
			slog.Debug("mcp-parser: skipping malformed SSE data frame", "error", err, "data", model.SanitizeLogValue(parsercommon.Truncate(string(data), 128)))
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
