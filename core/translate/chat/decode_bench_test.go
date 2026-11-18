package chat

import (
	"fmt"
	"strings"
	"testing"
)

// chatBody builds an OpenCode-shaped request of more than 2 MB: a system
// message, 40 tools and 400 turns, each with thinking, a tool call and
// about 5 KB of tool output with quotes and backslashes.
func chatBody() []byte {
	var b strings.Builder
	b.WriteString(`{"model":"claude-opus-5-5","messages":[{"role":"system","content":[{"type":"text","text":"You are a coding agent."},{"type":"text","text":"Follow the rules.","cache_control":{"type":"ephemeral"}}]}`)
	output := strings.Repeat(`line \"quoted\" C:\\path\\file.go: some output text `, 100)
	for i := range 400 {
		fmt.Fprintf(&b, `,{"role":"user","content":"turn %d: fix the bug"}`, i)
		fmt.Fprintf(&b, `,{"role":"assistant","content":"I run the tests.","thinking_blocks":[{"type":"thinking","thinking":"The tests come first.","signature":"c2lnbmF0dXJl%d"}],"tool_calls":[{"id":"call_%d","type":"function","function":{"name":"bash","arguments":"{\"command\":\"go test ./...\"}"}}]}`, i, i)
		fmt.Fprintf(&b, `,{"role":"tool","tool_call_id":"call_%d","content":"%s"}`, i, output)
	}
	b.WriteString(`],"tools":[`)
	for i := range 40 {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"type":"function","function":{"name":"tool_%d","description":"Tool number %d.","parameters":{"type":"object","properties":{"command":{"type":"string","description":"The command."}},"required":["command"]}}}`, i, i)
	}
	b.WriteString(`],"stream":true,"stream_options":{"include_usage":true}}`)
	return []byte(b.String())
}

// BenchmarkDecodeRequest measures rule K11: one 2 MB request in at most
// 15 ms.
func BenchmarkDecodeRequest(b *testing.B) {
	body := chatBody()
	if len(body) < 2<<20 {
		b.Fatalf("the body is %d bytes, not 2 MB", len(body))
	}
	b.SetBytes(int64(len(body)))
	for b.Loop() {
		if _, err := (Protocol{}).DecodeRequest(body); err != nil {
			b.Fatal(err)
		}
	}
}
