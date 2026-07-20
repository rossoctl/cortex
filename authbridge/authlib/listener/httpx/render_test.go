package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rossoctl/rossocortex/authbridge/authlib/pipeline"
)

// mcpAction matches what mcp-parser populates plus what ibac (or any
// outbound gate) would return when denying the request — the ibac.blocked
// shape with status 403 is the IBAC-deny case that motivates this code
// path.
func mcpAction() pipeline.Action {
	return pipeline.DenyStatus(403, "ibac.blocked", "intent does not align")
}

// pctxWithMCP builds the minimum pctx needed to trigger the JSON-RPC
// rendering path — a populated MCPExtension with a method and an id.
func pctxWithMCP(id any) *pipeline.Context {
	return &pipeline.Context{
		Extensions: pipeline.Extensions{
			MCP: &pipeline.MCPExtension{
				Method: "tools/call",
				RPCID:  id,
			},
		},
	}
}

// TestWriteRejectionForRequest_MCPRequest_RendersJSONRPCError is the
// core assertion: when the rejected request was MCP JSON-RPC, the
// listener must emit HTTP 200 carrying a JSON-RPC 2.0 error frame
// with the original id, not a transport-level 4xx/5xx. The agent's
// MCP client surfaces the failure as a per-tool-call error rather
// than a session break.
func TestWriteRejectionForRequest_MCPRequest_RendersJSONRPCError(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteRejectionForRequest(rec, mcpAction(), pctxWithMCP(float64(42)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (JSON-RPC errors travel over a 200 transport)", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type = %q, want application/json", got)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, rec.Body.String())
	}
	if body["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc = %v, want 2.0", body["jsonrpc"])
	}
	if id, ok := body["id"].(float64); !ok || id != 42 {
		t.Errorf("id = %v (%T), want 42", body["id"], body["id"])
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("error field missing or wrong type: %v", body["error"])
	}
	if errObj["code"] != float64(-32000) {
		t.Errorf("error.code = %v, want -32000", errObj["code"])
	}
	if errObj["message"] != "intent does not align" {
		t.Errorf("error.message = %v, want IBAC reason", errObj["message"])
	}
	data, ok := errObj["data"].(map[string]any)
	if !ok {
		t.Fatalf("error.data missing or wrong type: %v", errObj["data"])
	}
	if data["error"] != "ibac.blocked" {
		t.Errorf("error.data.error = %v, want ibac.blocked", data["error"])
	}
}

// TestWriteRejectionForRequest_MCPStringID_PreservesType verifies the
// id round-trip preserves the JSON-RPC id type. JSON-RPC 2.0 §4
// allows string, number, or null ids; mcp-parser carries whatever the
// caller sent verbatim, and the response MUST echo the same type.
func TestWriteRejectionForRequest_MCPStringID_PreservesType(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteRejectionForRequest(rec, mcpAction(), pctxWithMCP("call-abc"))

	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["id"] != "call-abc" {
		t.Errorf("id = %v (%T), want \"call-abc\"", body["id"], body["id"])
	}
}

// TestWriteRejectionForRequest_NotificationFallsBack covers JSON-RPC
// notifications: requests without an id. Per spec the client expects
// no response, so writing a JSON-RPC error frame would violate the
// notification contract. We fall through to the HTTP-level rejection.
func TestWriteRejectionForRequest_NotificationFallsBack(t *testing.T) {
	rec := httptest.NewRecorder()
	pctx := &pipeline.Context{
		Extensions: pipeline.Extensions{
			MCP: &pipeline.MCPExtension{Method: "tools/call", RPCID: nil},
		},
	}
	WriteRejectionForRequest(rec, mcpAction(), pctx)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (notification → fall through to HTTP-level)", rec.Code)
	}
}

// TestWriteRejectionForRequest_NonMCPFallsBack covers the broad case:
// IBAC denies plenty of non-MCP traffic (plain HTTP, A2A egress,
// inference). Those continue to render as today's HTTP-level errors
// because there's no JSON-RPC client expecting a JSON-RPC response.
func TestWriteRejectionForRequest_NonMCPFallsBack(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteRejectionForRequest(rec, mcpAction(), &pipeline.Context{})

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (no MCP ext → fall through)", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("content-type = %q, want application/json (default Render shape)", got)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if _, isJSONRPC := body["jsonrpc"]; isJSONRPC {
		t.Errorf("non-MCP request must NOT render as JSON-RPC: %s", rec.Body.String())
	}
}

// TestWriteRejectionForRequest_NilContextSafe is a defensive guard.
// Listeners always pass a non-nil pctx today, but a nil-safe helper
// is one less footgun.
func TestWriteRejectionForRequest_NilContextSafe(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteRejectionForRequest(rec, mcpAction(), nil)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (nil pctx → fall through)", rec.Code)
	}
}

// TestWriteRejectionForRequest_503JudgeUnavailable_StillJSONRPC verifies
// the IBAC-judge-down case. Pre-fix, this surfaced as a transport 503
// — the worst kind of failure for an MCP session. Post-fix, even the
// availability error renders as a JSON-RPC error frame so the agent
// only loses the one tool call.
func TestWriteRejectionForRequest_503JudgeUnavailable_StillJSONRPC(t *testing.T) {
	rec := httptest.NewRecorder()
	action := pipeline.DenyStatus(503, "ibac.judge_unavailable", "judge timed out")
	WriteRejectionForRequest(rec, action, pctxWithMCP("call-1"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — even 503 codes ride a 200 over JSON-RPC", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	errObj, _ := body["error"].(map[string]any)
	if errObj["message"] != "judge timed out" {
		t.Errorf("error.message = %v, want \"judge timed out\"", errObj["message"])
	}
	data, _ := errObj["data"].(map[string]any)
	if data["error"] != "ibac.judge_unavailable" {
		t.Errorf("error.data.error = %v, want ibac.judge_unavailable", data["error"])
	}
}

// TestWriteRejection_Unchanged sanity-checks that the existing
// WriteRejection helper still behaves the same — this fix only adds a
// new entry point; existing callers must keep working unchanged.
func TestWriteRejection_Unchanged(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteRejection(rec, mcpAction())

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "ibac.blocked" {
		t.Errorf("error = %v, want ibac.blocked", body["error"])
	}
}

// TestMarshalMCPRejectionBody_BadDetailsFallsBackToMinimalFrame: a
// plugin can populate Violation.Details with anything (it's
// map[string]any). If the value is unmarshalable (e.g. a channel),
// the full frame fails. The body MUST still be a parseable JSON-RPC
// 2.0 error frame so the MCP client surfaces this as a tool-call
// failure rather than a parse error on a 200 application/json
// response. The fallback drops optional data; everything else
// (jsonrpc version, id, error.code, error.message) survives.
func TestMarshalMCPRejectionBody_BadDetailsFallsBackToMinimalFrame(t *testing.T) {
	action := pipeline.DenyWithDetails("ibac.blocked", "intent does not align", map[string]any{
		"unmarshalable": make(chan int), // json.Marshal returns UnsupportedTypeError
	})
	body := MarshalMCPRejectionBody(action, "call-1")

	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("body must round-trip even when details fail to marshal: %v\n%s", err, body)
	}
	if parsed["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc = %v, want 2.0", parsed["jsonrpc"])
	}
	if parsed["id"] != "call-1" {
		t.Errorf("id = %v, want call-1 (id alone marshals fine, must be preserved)", parsed["id"])
	}
	errObj, ok := parsed["error"].(map[string]any)
	if !ok {
		t.Fatalf("error field missing or wrong type: %v", parsed["error"])
	}
	if errObj["code"] != float64(-32000) {
		t.Errorf("error.code = %v, want -32000", errObj["code"])
	}
	if errObj["message"] != "intent does not align" {
		t.Errorf("error.message = %v, want IBAC reason", errObj["message"])
	}
	if _, hasData := errObj["data"]; hasData {
		t.Errorf("error.data must be omitted on fallback; got %v", errObj["data"])
	}
}

// TestMarshalMCPRejectionBody_BadIDFallsBackToNullID: if the request
// id itself can't be marshaled (defensive — mcp-parser only stores
// string/number/null today, but RPCID is `any`), we fall back to
// id=null per JSON-RPC 2.0 §5.1, which permits null when the
// original id can't be detected. The body must still be a valid
// JSON-RPC error frame.
func TestMarshalMCPRejectionBody_BadIDFallsBackToNullID(t *testing.T) {
	body := MarshalMCPRejectionBody(mcpAction(), make(chan int))

	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("body must round-trip even when id fails to marshal: %v\n%s", err, body)
	}
	if parsed["id"] != nil {
		t.Errorf("id = %v, want null (unmarshalable id must drop to null)", parsed["id"])
	}
	errObj, _ := parsed["error"].(map[string]any)
	if errObj["message"] != "intent does not align" {
		t.Errorf("error.message = %v, want IBAC reason (preserved across fallback)", errObj["message"])
	}
}
