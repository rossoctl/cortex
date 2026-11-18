package translate

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/translate/anthropic"
)

// claudeCodeBody gives a request in the shape of a long Claude Code
// session: 40 tools, 400 turns, and a tool result of about 5 KB in each
// turn. It is more than 2 MB.
func claudeCodeBody() []byte {
	str := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	var sb strings.Builder
	sb.WriteString(`{"model":"claude-opus-5-5","max_tokens":32000,"stream":true,` +
		`"thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"metadata":{"user_id":"u"},` +
		`"system":[{"type":"text","text":"You are Claude Code."},` +
		`{"type":"text","text":"Follow the rules.","cache_control":{"type":"ephemeral"}}],"tools":[`)
	for i := range 40 {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"name":"Tool` + strconv.Itoa(i) + `","description":"A tool that does a thing.",` +
			`"input_schema":{"type":"object","properties":{"path":{"type":"string"},"limit":{"type":"integer"}},"required":["path"]}}`)
	}
	sb.WriteString(`],"messages":[`)
	result := str(strings.Repeat(`line "quoted" with a \ backslash `, 140)) // about 5 KB
	for i := range 400 {
		if i > 0 {
			sb.WriteByte(',')
		}
		id := `"toolu_` + strconv.Itoa(i) + `"`
		sb.WriteString(`{"role":"user","content":[{"type":"text","text":"Do step ` + strconv.Itoa(i) + `."}]},` +
			`{"role":"assistant","content":[{"type":"thinking","thinking":"I will read the file.","signature":"EvkBCkYIBxgCKkA"},` +
			`{"type":"text","text":"Reading the file."},` +
			`{"type":"tool_use","id":` + id + `,"name":"Tool1","input":{"path":"/a/b.go","limit":100}}]},` +
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":` + id + `,"content":[{"type":"text","text":` + result + `}]}]}`)
	}
	sb.WriteString(`]}`)
	return []byte(sb.String())
}

// BenchmarkAnthropicDecodeRequest measures the decoder that rule S16 gives
// a budget of 15 ms.
func BenchmarkAnthropicDecodeRequest(b *testing.B) {
	body := claudeCodeBody()
	if len(body) < 2<<20 {
		b.Fatalf("the body is %d bytes, want at least %d", len(body), 2<<20)
	}
	b.SetBytes(int64(len(body)))
	for b.Loop() {
		if _, err := (anthropic.Protocol{}).DecodeRequest(body); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRequestAnthropicToChat measures the full conversion. It has no
// budget.
func BenchmarkRequestAnthropicToChat(b *testing.B) {
	body := claudeCodeBody()
	b.SetBytes(int64(len(body)))
	for b.Loop() {
		if _, err := Request(Anthropic, Chat, body, nil); err != nil {
			b.Fatal(err)
		}
	}
}
