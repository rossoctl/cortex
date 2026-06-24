package forwardproxy

import (
	"bytes"
	"strings"
	"testing"
)

// TestSSEEventName verifies the event-name derivation: Anthropic stream
// frames (top-level JSON "type") yield the SSE event name; data-only
// protocol frames (MCP/A2A JSON-RPC, OpenAI chat chunks) and non-JSON
// frames yield "" so no event: line is emitted for them.
func TestSSEEventName(t *testing.T) {
	cases := []struct {
		name  string
		frame string
		want  string
	}{
		{"anthropic message_start", `{"type":"message_start","message":{}}`, "message_start"},
		{"anthropic content_block_delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`, "content_block_delta"},
		{"anthropic message_stop", `{"type":"message_stop"}`, "message_stop"},
		{"anthropic ping", `{"type":"ping"}`, "ping"},
		{"mcp/a2a jsonrpc data-only", `{"jsonrpc":"2.0","id":1,"result":{}}`, ""},
		{"openai chat chunk", `{"id":"x","object":"chat.completion.chunk","choices":[]}`, ""},
		{"a2a kind not type", `{"kind":"status-update","taskId":"t1"}`, ""},
		{"non-json [DONE]", `[DONE]`, ""},
		{"empty", ``, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sseEventName([]byte(tc.frame)); got != tc.want {
				t.Fatalf("sseEventName(%q) = %q, want %q", tc.frame, got, tc.want)
			}
		})
	}
}

// TestWriteSSEFrameAnthropicAddsEventLine is the regression guard for the
// duplicate-call bug: the sseframe reader strips the SSE event: line, and
// the Anthropic client needs it to finalize the stream — without it the
// CLI falls back to a second, non-streaming request (doubling the call).
// writeSSEFrame must reconstruct event: <type> before the data: line.
func TestWriteSSEFrameAnthropicAddsEventLine(t *testing.T) {
	var buf bytes.Buffer
	frame := []byte(`{"type":"message_start","message":{"id":"msg_1"}}`)
	if !writeSSEFrame(&buf, frame) {
		t.Fatal("writeSSEFrame returned false")
	}
	want := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1"}}` + "\n\n"
	if got := buf.String(); got != want {
		t.Fatalf("anthropic frame:\n got %q\nwant %q", got, want)
	}
}

// TestWriteSSEFrameDataOnlyHasNoEventLine guards the other direction:
// data-only protocols must keep their data-only framing (no event: line),
// otherwise we'd corrupt MCP/A2A/OpenAI streams.
func TestWriteSSEFrameDataOnlyHasNoEventLine(t *testing.T) {
	var buf bytes.Buffer
	frame := []byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	if !writeSSEFrame(&buf, frame) {
		t.Fatal("writeSSEFrame returned false")
	}
	got := buf.String()
	if strings.Contains(got, "event:") {
		t.Fatalf("data-only frame must not get an event: line, got %q", got)
	}
	want := `data: {"jsonrpc":"2.0","id":1,"result":{"ok":true}}` + "\n\n"
	if got != want {
		t.Fatalf("jsonrpc frame:\n got %q\nwant %q", got, want)
	}
}

// TestWriteSSEFrameMultiLineData ensures the event: line is not emitted for
// a multi-line (non-JSON) folded data payload, and that each data line is
// re-prefixed.
func TestWriteSSEFrameMultiLineData(t *testing.T) {
	var buf bytes.Buffer
	frame := []byte("line-a\nline-b")
	if !writeSSEFrame(&buf, frame) {
		t.Fatal("writeSSEFrame returned false")
	}
	want := "data: line-a\ndata: line-b\n\n"
	if got := buf.String(); got != want {
		t.Fatalf("multi-line frame:\n got %q\nwant %q", got, want)
	}
}
