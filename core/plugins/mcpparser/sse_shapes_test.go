package mcpparser

import (
	"context"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// The two tests in this file pin issue #1567 item 2 for MCP. The event-stream format
// allows a leading UTF-8 BOM and CR-only line endings, and the buffered response
// scanner used to split on LF over the raw body — so a Streamable-HTTP response
// shaped that way matched no "data:" line, the MCP extension stayed empty, and IBAC
// treated the exchange as unclassified, which passes by default. inferenceparser
// already normalizes these shapes (normalizeSSE); these tests hold MCP to the same
// bar.

// mcpStreamBody builds a Streamable-HTTP response in the wire-legal shape named by
// bom and crOnly. The body starts directly with a data: line when bom is set — that
// is the case the BOM breaks (see the a2a fixture for the reasoning).
func mcpStreamBody(bom, crOnly bool) []byte {
	eol := "\n"
	if crOnly {
		eol = "\r"
	}
	body := `data: {"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"get_weather"}]}}` + eol + eol
	// A SECOND frame that is not a response — a notification. One data: frame
	// alone happens to survive a raw LF split (the whole body is one line that
	// starts with "data:", and TrimSpace eats the trailing CR), and under a BOM
	// the LATER frames still parse, so a second result frame would let both
	// shapes pass for the wrong reason. A notification makes the first frame the
	// only parseable one, which is the failure shape that matters.
	body += `data: {"jsonrpc":"2.0","method":"notifications/progress","params":{"progress":50}}` + eol + eol
	if bom {
		body = "\xef\xbb\xbf" + body
	}
	return []byte(body)
}

func TestMCPParser_OnResponse_SSELeadingBOM(t *testing.T) {
	p := NewMCPParser()
	pctx := &pipeline.Context{
		Extensions:   pipeline.Extensions{MCP: &pipeline.MCPExtension{Method: "tools/list"}},
		ResponseBody: mcpStreamBody(true, false),
	}
	if action := p.OnResponse(context.Background(), pctx); action.Type != pipeline.Continue {
		t.Fatalf("action = %v, want Continue", action.Type)
	}
	if pctx.Extensions.MCP.Result == nil {
		t.Fatal("Result should be populated from the SSE data frame — a leading BOM is " +
			"wire-legal SSE and must not hide the frame from the buffered scanner")
	}
	if _, ok := pctx.Extensions.MCP.Result["tools"]; !ok {
		t.Error("Result should contain tools from the SSE data")
	}
}

func TestMCPParser_OnResponse_SSECROnlyLineEndings(t *testing.T) {
	p := NewMCPParser()
	pctx := &pipeline.Context{
		Extensions:   pipeline.Extensions{MCP: &pipeline.MCPExtension{Method: "tools/list"}},
		ResponseBody: mcpStreamBody(false, true),
	}
	if action := p.OnResponse(context.Background(), pctx); action.Type != pipeline.Continue {
		t.Fatalf("action = %v, want Continue", action.Type)
	}
	if pctx.Extensions.MCP.Result == nil {
		t.Fatal("Result should be populated from the SSE data frame — CR-only line endings " +
			"are wire-legal SSE; a body so shaped is ONE line to an LF split and the data: " +
			"check missed it")
	}
	if _, ok := pctx.Extensions.MCP.Result["tools"]; !ok {
		t.Error("Result should contain tools from the SSE data")
	}
}
