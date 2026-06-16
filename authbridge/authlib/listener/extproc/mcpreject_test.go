package extproc

import (
	"encoding/json"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// pctxWithMCP builds the minimum pctx needed to trigger the JSON-RPC
// rendering path on the extproc listener.
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

func ibacBlocked() pipeline.Action {
	return pipeline.DenyStatus(403, "ibac.blocked", "intent does not align")
}

// immediateBody fishes the ImmediateResponse body out of a
// ProcessingResponse — saves boilerplate in every assertion below.
func immediateBody(t *testing.T, resp *extprocv3.ProcessingResponse) (typev3.StatusCode, []byte, []*corev3.HeaderValueOption) {
	t.Helper()
	imm, ok := resp.GetResponse().(*extprocv3.ProcessingResponse_ImmediateResponse)
	if !ok {
		t.Fatalf("response is not ImmediateResponse: %T", resp.GetResponse())
	}
	var hs []*corev3.HeaderValueOption
	if imm.ImmediateResponse.Headers != nil {
		hs = imm.ImmediateResponse.Headers.SetHeaders
	}
	return imm.ImmediateResponse.Status.Code, imm.ImmediateResponse.Body, hs
}

// TestRejectFromActionForRequest_MCPRequest verifies the extproc
// listener emits an HTTP 200 ImmediateResponse with a JSON-RPC error
// body when the rejected request was MCP — the envoy-sidecar twin of
// the forwardproxy fix.
func TestRejectFromActionForRequest_MCPRequest(t *testing.T) {
	resp := rejectFromActionForRequest(ibacBlocked(), pctxWithMCP(float64(7)))
	status, body, headers := immediateBody(t, resp)

	if status != typev3.StatusCode_OK {
		t.Fatalf("status = %v, want OK (200) — JSON-RPC errors travel over a 200 transport", status)
	}
	gotCT := false
	for _, h := range headers {
		if h.Header.Key == "content-type" && string(h.Header.RawValue) == "application/json" {
			gotCT = true
		}
	}
	if !gotCT {
		t.Errorf("content-type header missing or wrong: %v", headers)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, body)
	}
	if parsed["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc = %v, want 2.0", parsed["jsonrpc"])
	}
	if id, _ := parsed["id"].(float64); id != 7 {
		t.Errorf("id = %v, want 7", parsed["id"])
	}
	errObj, _ := parsed["error"].(map[string]any)
	if errObj["code"] != float64(-32000) {
		t.Errorf("error.code = %v, want -32000", errObj["code"])
	}
	if errObj["message"] != "intent does not align" {
		t.Errorf("error.message = %v, want IBAC reason", errObj["message"])
	}
}

// TestRejectFromActionForRequest_NonMCPFallsBack: non-MCP outbound
// denials keep today's transport-level error shape. Asserts the
// status mirrors the violation, not 200.
func TestRejectFromActionForRequest_NonMCPFallsBack(t *testing.T) {
	resp := rejectFromActionForRequest(ibacBlocked(), &pipeline.Context{})
	status, _, _ := immediateBody(t, resp)
	if status != typev3.StatusCode_Forbidden {
		t.Fatalf("status = %v, want Forbidden (403) — non-MCP request should keep HTTP-level shape", status)
	}
}

// TestRejectFromActionForRequest_NotificationFallsBack: JSON-RPC
// notifications (id == nil) get no JSON-RPC response by spec, so we
// fall through to the HTTP-level shape rather than echoing a null id.
func TestRejectFromActionForRequest_NotificationFallsBack(t *testing.T) {
	pctx := &pipeline.Context{
		Extensions: pipeline.Extensions{
			MCP: &pipeline.MCPExtension{Method: "tools/call", RPCID: nil},
		},
	}
	resp := rejectFromActionForRequest(ibacBlocked(), pctx)
	status, _, _ := immediateBody(t, resp)
	if status != typev3.StatusCode_Forbidden {
		t.Fatalf("status = %v, want Forbidden (403) — notification should keep HTTP-level shape", status)
	}
}
