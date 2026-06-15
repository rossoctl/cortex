// Package httpx contains HTTP-listener helpers shared between the
// forwardproxy and reverseproxy listeners. extproc and extauthz speak
// gRPC and don't use this package.
package httpx

import (
	"encoding/json"
	"net/http"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// WriteRejection renders a pipeline.Reject Action onto an HTTP
// ResponseWriter. Status, headers, and body all come from the action's
// Violation — listeners hand the writer + action over and let the
// pipeline-defined contract drive the response shape.
//
// Safe to call only when action.Violation is non-nil (i.e. the action
// was Type=Reject). The forward/reverse proxy listeners only invoke
// this on the Reject branch of their action switch, matching that
// invariant.
func WriteRejection(w http.ResponseWriter, action pipeline.Action) {
	status, headers, body := action.Violation.Render()
	for k, vs := range headers {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// WriteRejectionForRequest renders a Reject the same way as
// WriteRejection EXCEPT when the rejected request was a JSON-RPC
// request that an MCP-aware parser already classified — in that case
// it writes a JSON-RPC 2.0 error frame at HTTP 200 with the original
// id echoed back, so the caller's MCP client surfaces this as a
// failed tool call rather than a transport break.
//
// The MCP-shape detection is conservative: we only rewrite when
// pctx.Extensions.MCP is populated with a non-empty Method AND a
// non-nil RPCID. JSON-RPC notifications (no id) deliberately fall
// through to plain WriteRejection — by spec the client expects no
// response, so emitting a JSON-RPC error frame would violate the
// notification contract.
//
// All non-MCP requests fall through to WriteRejection, so this is a
// safe drop-in replacement at any call site.
func WriteRejectionForRequest(w http.ResponseWriter, action pipeline.Action, pctx *pipeline.Context) {
	if !shouldRenderMCPError(pctx) {
		WriteRejection(w, action)
		return
	}
	writeMCPRejection(w, action, pctx.Extensions.MCP.RPCID)
}

func shouldRenderMCPError(pctx *pipeline.Context) bool {
	if pctx == nil || pctx.Extensions.MCP == nil {
		return false
	}
	mcp := pctx.Extensions.MCP
	if mcp.Method == "" || mcp.RPCID == nil {
		return false
	}
	return true
}

// JSON-RPC 2.0 error code for application errors. -32000..-32099 is
// the "implementation-defined server-error" range reserved by the
// spec; -32000 is the conventional generic value used when there's
// no protocol-level reason for a more specific code. Authbridge's
// denials are policy decisions outside the JSON-RPC layer, so the
// generic server-error code fits — operators read the human reason
// and structured details, not the numeric code, to understand what
// happened.
const jsonRPCServerError = -32000

func writeMCPRejection(w http.ResponseWriter, action pipeline.Action, id any) {
	body := MarshalMCPRejectionBody(action, id)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// mcpRejectionFallback is a precomputed JSON-RPC 2.0 error frame used as
// the last-resort body when both the full and minimal marshal attempts
// fail. It is guaranteed to round-trip through json.Unmarshal so MCP
// clients always see a parseable frame on a 200 application/json
// response.
var mcpRejectionFallback = []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":"request rejected"}}`)

// MarshalMCPRejectionBody renders a Reject Action as a JSON-RPC 2.0
// error frame body. It is guaranteed to return a non-empty, parseable
// JSON byte slice so callers can safely send it on a 200
// application/json response: marshal the full frame first; on error
// (e.g. a plugin populated Violation.Details with an unmarshalable
// value, or id is unmarshalable), retry with a minimal frame that omits
// optional data and falls back to id=null; on further error, return a
// constant precomputed frame.
func MarshalMCPRejectionBody(action pipeline.Action, id any) []byte {
	v := action.Violation
	message := "request rejected"
	var data map[string]any
	if v != nil {
		if v.Reason != "" {
			message = v.Reason
		}
		data = map[string]any{}
		if v.Code != "" {
			data["error"] = v.Code
		}
		if v.PluginName != "" {
			data["plugin"] = v.PluginName
		}
		if v.Description != "" {
			data["description"] = v.Description
		}
		if len(v.Details) > 0 {
			data["details"] = v.Details
		}
		if len(data) == 0 {
			data = nil
		}
	}
	if body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    jsonRPCServerError,
			"message": message,
			"data":    data,
		},
	}); err == nil {
		return body
	}
	// Full frame failed to marshal — retry without optional data, and
	// keep the original id only if it survives marshaling on its own
	// (otherwise drop to null per JSON-RPC 2.0 §5.1).
	safeID := any(nil)
	if id != nil {
		if _, err := json.Marshal(id); err == nil {
			safeID = id
		}
	}
	if body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      safeID,
		"error": map[string]any{
			"code":    jsonRPCServerError,
			"message": message,
		},
	}); err == nil {
		return body
	}
	return mcpRejectionFallback
}
