package inferenceparser

import "testing"

// ParseResponse picks the parser from the bytes, so an SSE body and a
// JSON body for the same dialect both yield the tool call, and a path no
// dialect matches yields nil.
func TestParseResponse(t *testing.T) {
	const jsonBody = `{"type":"message","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}`
	const sseBody = `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"Bash","input":{}}}` + "\n\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ls\"}"}}` + "\n\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n"
	for name, body := range map[string]string{"json": jsonBody, "sse": sseBody} {
		ext := ParseResponse("/v1/messages?beta=true", []byte(body))
		if ext == nil || len(ext.ToolCalls) != 1 || ext.ToolCalls[0].Name != "Bash" || ext.ToolCalls[0].Arguments != `{"command":"ls"}` {
			t.Fatalf("%s: %+v", name, ext)
		}
		if ext.StreamedResponse != (name == "sse") {
			t.Fatalf("%s: StreamedResponse = %v", name, ext.StreamedResponse)
		}
	}
	if ext := ParseResponse("/v1/embeddings", []byte(jsonBody)); ext != nil {
		t.Fatalf("unknown path: %+v", ext)
	}
}
