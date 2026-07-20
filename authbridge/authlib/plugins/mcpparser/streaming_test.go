package mcpparser

import (
	"context"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

func TestMCPParser_OnResponseFrame_PerMessageRecording(t *testing.T) {
	p := NewMCPParser()
	pctx := &pipeline.Context{
		Extensions: pipeline.Extensions{
			MCP: &pipeline.MCPExtension{Method: "tools/call"},
		},
	}

	// First frame: a complete JSON-RPC result.
	frame := []byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"hello"}]}}`)
	action := p.OnResponseFrame(context.Background(), pctx, frame, false)
	if action.Type != pipeline.Continue {
		t.Fatalf("action = %v, want Continue", action.Type)
	}
	if pctx.Extensions.MCP.Result == nil {
		t.Fatal("Result not populated by OnResponseFrame")
	}
	// last=true with empty frame finalizes (no-op for already-populated Result).
	p.OnResponseFrame(context.Background(), pctx, nil, true)
	if pctx.Extensions.MCP.Result == nil {
		t.Error("Result cleared on last=true")
	}
}

func TestMCPParser_OnResponseFrame_ApplicationJSONOneShot(t *testing.T) {
	p := NewMCPParser()
	pctx := &pipeline.Context{
		Extensions: pipeline.Extensions{
			MCP: &pipeline.MCPExtension{Method: "tools/call"},
		},
	}
	body := []byte(`{"jsonrpc":"2.0","id":2,"result":{"ok":true}}`)
	// application/json: single last=true frame containing whole body.
	action := p.OnResponseFrame(context.Background(), pctx, body, true)
	if action.Type != pipeline.Continue {
		t.Fatalf("action = %v, want Continue", action.Type)
	}
	if pctx.Extensions.MCP.Result == nil {
		t.Fatal("Result not populated")
	}
	if pctx.Extensions.MCP.Result["ok"] != true {
		t.Errorf("Result[ok] = %v, want true", pctx.Extensions.MCP.Result["ok"])
	}
}

func TestMCPParser_OnResponseFrame_ErrorFrame(t *testing.T) {
	p := NewMCPParser()
	pctx := &pipeline.Context{
		Extensions: pipeline.Extensions{
			MCP: &pipeline.MCPExtension{Method: "tools/call"},
		},
	}
	frame := []byte(`{"jsonrpc":"2.0","id":3,"error":{"code":-32601,"message":"method not found"}}`)
	p.OnResponseFrame(context.Background(), pctx, frame, true)
	if pctx.Extensions.MCP.Err == nil {
		t.Fatal("Err not populated")
	}
	if pctx.Extensions.MCP.Err.Code != -32601 {
		t.Errorf("Err.Code = %d, want -32601", pctx.Extensions.MCP.Err.Code)
	}
}

func TestMCPParser_OnResponseFrame_NoExtensionMeansNoOp(t *testing.T) {
	p := NewMCPParser()
	pctx := &pipeline.Context{}
	action := p.OnResponseFrame(context.Background(), pctx, []byte(`{}`), true)
	if action.Type != pipeline.Continue {
		t.Errorf("action = %v, want Continue", action.Type)
	}
	if pctx.Extensions.MCP != nil {
		t.Error("MCPExtension created when request side never participated")
	}
}

func TestMCPParser_OnResponseFrame_EmptyStreamRecordsSkip(t *testing.T) {
	p := NewMCPParser()
	pctx := &pipeline.Context{
		Extensions: pipeline.Extensions{
			MCP: &pipeline.MCPExtension{Method: "tools/call"},
		},
	}
	// Streaming response with zero data frames: only the last=true call.
	pctx.SetCurrentPlugin("mcp-parser", pipeline.InvocationPhaseResponse)
	p.OnResponseFrame(context.Background(), pctx, nil, true)
	pctx.ClearCurrentPlugin()
	if pctx.Extensions.Invocations == nil {
		t.Fatal("no invocation recorded")
	}
}

// TestMCPParser_OnResponseFrame_RawSSEBlob reproduces the live tools/list
// bug: the forward proxy's buffered dispatch hands the WHOLE response body as
// one last=true frame, and for a Streamable HTTP (MCP) server that body is a
// raw "data: {...}" SSE blob — not pre-stripped JSON. The bare json.Unmarshal
// failed on it and silently dropped the result (response recorded with no
// result, no invocation). OnResponseFrame must parse the embedded JSON-RPC
// result and record an observe.
func TestMCPParser_OnResponseFrame_RawSSEBlob(t *testing.T) {
	p := NewMCPParser()
	pctx := &pipeline.Context{
		Extensions: pipeline.Extensions{MCP: &pipeline.MCPExtension{Method: "tools/list"}},
	}
	blob := []byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"tools\":[{\"name\":\"get_weather\"}]}}\n\n")
	pctx.SetCurrentPlugin("mcp-parser", pipeline.InvocationPhaseResponse)
	p.OnResponseFrame(context.Background(), pctx, blob, true)
	pctx.ClearCurrentPlugin()

	if pctx.Extensions.MCP.Result == nil {
		t.Fatal("Result not populated from raw SSE blob — the bug")
	}
	inv := pctx.Extensions.Invocations
	if inv == nil || len(inv.Inbound)+len(inv.Outbound) == 0 {
		t.Fatal("no mcp-parser observe recorded for the SSE response")
	}
}

func TestMCPParser_OnResponseFrame_MalformedFrameSkipped(t *testing.T) {
	p := NewMCPParser()
	pctx := &pipeline.Context{
		Extensions: pipeline.Extensions{
			MCP: &pipeline.MCPExtension{Method: "tools/call"},
		},
	}
	action := p.OnResponseFrame(context.Background(), pctx, []byte("not json"), false)
	if action.Type != pipeline.Continue {
		t.Errorf("action = %v, want Continue (malformed frame should skip silently)", action.Type)
	}
	if pctx.Extensions.MCP.Result != nil || pctx.Extensions.MCP.Err != nil {
		t.Error("malformed frame populated Result/Err")
	}
}
