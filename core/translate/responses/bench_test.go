package responses

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/translate/pivot"
)

// codexBody makes a Codex-shaped request of about 2 MB: instructions, a
// developer message, 400 turns and 40 tools.
func codexBody() []byte {
	var b strings.Builder
	q := func(s string) string { j, _ := json.Marshal(s); return string(j) }
	b.WriteString(`{"model":"gpt-5.5-codex","instructions":` + q(strings.Repeat("You are Codex. ", 800)) + `,"input":[`)
	b.WriteString(`{"type":"message","role":"developer","content":[{"type":"input_text","text":` + q(strings.Repeat("sandbox rules ", 200)) + `}]}`)
	reasoning := q(encodeReasoning(pivot.Block{Kind: pivot.BlockThinking, Text: strings.Repeat("think ", 40), Signature: strings.Repeat("S", 300)}))
	output := q(strings.Repeat("line of tool output with \"quotes\" and \\ slashes\n", 80))
	for i := range 400 {
		id := strconv.Itoa(i)
		b.WriteString(`,{"type":"message","role":"user","content":[{"type":"input_text","text":"step ` + id + `"}]}`)
		b.WriteString(`,{"type":"reasoning","id":"rs_` + id + `","summary":[],"encrypted_content":` + reasoning + `}`)
		b.WriteString(`,{"type":"function_call","id":"fc_` + id + `","call_id":"call_` + id + `","name":"shell","arguments":"{\"command\":[\"ls\",\"-la\"],\"workdir\":\"/repo\"}"}`)
		b.WriteString(`,{"type":"function_call_output","call_id":"call_` + id + `","output":` + output + `}`)
		b.WriteString(`,{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done with step ` + id + `."}]}`)
	}
	b.WriteString(`],"tools":[`)
	for i := range 40 {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"type":"function","name":"tool_` + strconv.Itoa(i) + `","description":"A tool.","strict":false,"parameters":{"type":"object","properties":{"command":{"type":"array","items":{"type":"string"}}},"required":["command"]}}`)
	}
	b.WriteString(`],"tool_choice":"auto","parallel_tool_calls":false,"reasoning":{"effort":"medium","summary":"auto"},"store":false,"stream":true,"include":["reasoning.encrypted_content"],"prompt_cache_key":"k"}`)
	return []byte(b.String())
}

// BenchmarkDecodeRequest measures rule R15: at most 15 ms for a 2 MB
// request (decision 13).
func BenchmarkDecodeRequest(b *testing.B) {
	body := codexBody()
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

// BenchmarkEncodeReplyStream measures rule R15 on the reply side: a stream
// of 2,000 pivot events, text and tool-input deltas.
func BenchmarkEncodeReplyStream(b *testing.B) {
	evs := []pivot.Event{start(), open(0, pivot.BlockText)}
	for range 1000 {
		evs = append(evs, delta(0, pivot.DeltaText, "some words of text "))
	}
	evs = append(evs, closeBlock(0), openTool(1, "call_1", "shell"))
	for range 994 {
		evs = append(evs, delta(1, pivot.DeltaInputJSON, `"piece",`))
	}
	evs = append(evs, closeBlock(1), usage(usageCache), stop("tool_use"))
	req := codexReq(b)
	for b.Loop() {
		enc := Protocol{}.NewReplyEncoder(req, true)
		for _, ev := range evs {
			enc.Encode(ev)
		}
		enc.End()
	}
}
