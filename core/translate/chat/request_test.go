package chat

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/translate/pivot"
)

var ephemeral = json.RawMessage(`{"type":"ephemeral"}`)

func ptr(f float64) *float64 { return &f }

// fullRequest has one block of each kind except document.
func fullRequest() pivot.Request {
	const id = "call_1__thought__a+b/c="
	return pivot.Request{
		Model:  "m",
		System: []pivot.Block{{Kind: pivot.BlockText, Text: "sys", CacheControl: ephemeral}},
		Messages: []pivot.Message{
			{Role: "user", Content: []pivot.Block{
				{Kind: pivot.BlockText, Text: "hi <b>", CacheControl: ephemeral},
				{Kind: pivot.BlockImage, SourceType: "base64", MediaType: "image/png", Data: "AAA"},
				{Kind: pivot.BlockImage, SourceType: "url", URL: "https://x/a.png"},
			}},
			{Role: "assistant", Extra: pivot.Extra{"anthropic.id": json.RawMessage(`"x"`)}, Content: []pivot.Block{
				{Kind: pivot.BlockThinking, Text: "hmm", Signature: "SIG"},
				{Kind: pivot.BlockThinking, Text: "plain"},
				{Kind: pivot.BlockRedactedThinking, Data: "RD"},
				{Kind: pivot.BlockText, Text: "o"},
				{Kind: pivot.BlockText, Text: "k"},
				{Kind: pivot.BlockToolUse, ID: id, Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)},
			}},
			{Role: "user", Content: []pivot.Block{
				{Kind: pivot.BlockToolResult, ToolUseID: id, IsError: true, CacheControl: ephemeral, Content: []pivot.Block{
					{Kind: pivot.BlockText, Text: "out"},
					{Kind: pivot.BlockImage, SourceType: "base64", MediaType: "image/png", Data: "BBB", Extra: pivot.Extra{"anthropic.citations": json.RawMessage(`[]`)}},
				}},
				{Kind: pivot.BlockText, Text: "next"},
			}},
			{Role: "system", Content: []pivot.Block{{Kind: pivot.BlockText, Text: "hook"}}},
		},
		Tools:       []pivot.Tool{{Name: "Bash", Description: "run", InputSchema: json.RawMessage(`{"type":"object"}`), CacheControl: ephemeral}},
		ToolChoice:  &pivot.ToolChoice{Type: "any", DisableParallel: true},
		MaxTokens:   1000,
		Stop:        []string{"END"},
		Temperature: ptr(0.5),
		TopP:        ptr(0.9),
		Stream:      true,
		Effort:      "high",
		Thinking:    &pivot.Thinking{Type: "enabled", BudgetTokens: 1000},
		Extra: pivot.Extra{
			"anthropic.top_k":    json.RawMessage(`5`),
			"anthropic.metadata": json.RawMessage(`{"user_id":"u"}`),
		},
	}
}

func TestEncodeRequest_Shape(t *testing.T) {
	body, dropped, err := Protocol{}.EncodeRequest(fullRequest(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"m","messages":[` +
		`{"role":"system","content":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}]},` +
		`{"role":"user","content":[{"type":"text","text":"hi <b>","cache_control":{"type":"ephemeral"}},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}},{"type":"image_url","image_url":{"url":"https://x/a.png"}}]},` +
		`{"role":"assistant","content":"ok","thinking_blocks":[{"type":"thinking","thinking":"hmm","signature":"SIG"},{"type":"redacted_thinking","data":"RD"}],"tool_calls":[{"id":"call_1__thought__a+b/c=","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"ls\"}"}}]},` +
		`{"role":"tool","content":[{"type":"text","text":"out","cache_control":{"type":"ephemeral"}}],"tool_call_id":"call_1__thought__a+b/c="},` +
		`{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,BBB"}}]},` +
		`{"role":"user","content":[{"type":"text","text":"next"}]},` +
		`{"role":"user","content":[{"type":"text","text":"hook"}]}` +
		`],"max_tokens":1000,"stream":true,"stream_options":{"include_usage":true},` +
		`"tools":[{"type":"function","function":{"name":"Bash","description":"run","parameters":{"type":"object"}}}],` +
		`"tool_choice":"required","parallel_tool_calls":false,"stop":["END"],"temperature":0.5,"top_p":0.9,"reasoning_effort":"high"}`
	if string(body) != want {
		t.Errorf("body:\n got %s\nwant %s", body, want)
	}
	wantDropped := []string{"citations", "id", "is_error", "metadata", "thinking", "top_k"}
	if !reflect.DeepEqual(dropped, wantDropped) {
		t.Errorf("dropped = %q, want %q", dropped, wantDropped)
	}

	// Rule 9a: the same request gives the same bytes.
	again, _, _ := Protocol{}.EncodeRequest(fullRequest(), nil)
	if string(again) != string(body) {
		t.Error("two encodings of one request differ")
	}
}

func TestEncodeRequest_ToolChoice(t *testing.T) {
	for _, tc := range []struct {
		choice pivot.ToolChoice
		want   string
	}{
		{pivot.ToolChoice{Type: "auto"}, `"tool_choice":"auto"}`},
		{pivot.ToolChoice{Type: "none"}, `"tool_choice":"none"}`},
		{pivot.ToolChoice{Type: "tool", Name: "Bash"}, `"tool_choice":{"type":"function","function":{"name":"Bash"}}}`},
	} {
		req := pivot.Request{Model: "m", ToolChoice: &tc.choice}
		body, _, err := Protocol{}.EncodeRequest(req, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := `{"model":"m","messages":[],` + tc.want
		if string(body) != want {
			t.Errorf("%s: got %s, want %s", tc.choice.Type, body, want)
		}
	}
}

func wantRefusal(t *testing.T, err error, msg string) {
	t.Helper()
	var re *pivot.RequestError
	if !errors.As(err, &re) {
		t.Fatalf("err = %v, want *pivot.RequestError", err)
	}
	if re.Status != 400 || re.Type != "invalid_request_error" || re.Message != msg {
		t.Errorf("err = %+v, want 400 invalid_request_error %q", re, msg)
	}
}

func TestEncodeRequest_RefusesDocument(t *testing.T) {
	doc := pivot.Block{Kind: pivot.BlockDocument, SourceType: "base64", MediaType: "application/pdf", Data: "JVBE"}
	for name, content := range map[string][]pivot.Block{
		"user":        {doc},
		"tool_result": {{Kind: pivot.BlockToolResult, ToolUseID: "t", Content: []pivot.Block{doc}}},
	} {
		req := pivot.Request{Model: "m", Messages: []pivot.Message{{Role: "user", Content: content}}}
		_, _, err := Protocol{}.EncodeRequest(req, nil)
		t.Run(name, func(t *testing.T) { wantRefusal(t, err, "this inference server cannot read documents (PDF)") })
	}
}

func TestEncodeRequest_RefusesServerTool(t *testing.T) {
	req := pivot.Request{Model: "m", Tools: []pivot.Tool{{Name: "web_search", Type: "web_search_20250305"}}}
	_, _, err := Protocol{}.EncodeRequest(req, nil)
	wantRefusal(t, err, `this inference server cannot run the server tool "web_search" (type web_search_20250305)`)
}

func TestEncodeRequest_DeletesAnthropicHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Anthropic-Version", "2023-06-01")
	h.Set("Anthropic-Beta", "x")
	h.Set("X-Keep", "1")
	if _, _, err := (Protocol{}).EncodeRequest(pivot.Request{Model: "m"}, h); err != nil {
		t.Fatal(err)
	}
	if len(h) != 1 || h.Get("X-Keep") != "1" {
		t.Errorf("headers = %v, want only X-Keep", h)
	}
}

func TestEncodeRequest_Effort(t *testing.T) {
	for _, tc := range []struct {
		effort, thinking, want string
	}{
		{"low", "", "low"},
		{"medium", "", "medium"},
		{"high", "", "high"},
		{"max", "", "high"},
		{"xhigh", "disabled", "high"},
		{"", "enabled", "medium"},
		{"", "adaptive", "medium"},
		{"", "disabled", ""},
		{"", "", ""},
	} {
		req := pivot.Request{Model: "m", Effort: tc.effort}
		if tc.thinking != "" {
			req.Thinking = &pivot.Thinking{Type: tc.thinking}
		}
		body, _, err := Protocol{}.EncodeRequest(req, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := `{"model":"m","messages":[]}`
		if tc.want != "" {
			want = `{"model":"m","messages":[],"reasoning_effort":"` + tc.want + `"}`
		}
		if string(body) != want {
			t.Errorf("effort %q thinking %q: got %s, want %s", tc.effort, tc.thinking, body, want)
		}
	}
}

// Rule 9a: a new turn keeps the bytes before it, and a moved cache
// marker changes only the marker's bytes.
func TestEncodeRequest_PrefixStable(t *testing.T) {
	req := fullRequest()
	req.Stream = false
	base, _, _ := Protocol{}.EncodeRequest(req, nil)

	req.Messages = append(req.Messages, pivot.Message{Role: "assistant", Content: []pivot.Block{{Kind: pivot.BlockText, Text: "done"}}})
	longer, _, _ := Protocol{}.EncodeRequest(req, nil)
	idx := strings.Index(string(base), `],"max_tokens"`)
	if idx < 0 || string(longer[:idx]) != string(base[:idx]) {
		t.Errorf("bytes before the new turn changed:\n%s\n%s", base, longer)
	}

	moved := fullRequest()
	moved.Stream = false
	moved.Messages[0].Content[0].CacheControl = nil
	moved.Messages[2].Content[1].CacheControl = ephemeral
	got, _, _ := Protocol{}.EncodeRequest(moved, nil)
	marker := `,"cache_control":{"type":"ephemeral"}`
	wantMoved := strings.Replace(strings.Replace(string(base), `"hi <b>"`+marker, `"hi <b>"`, 1), `"next"}`, `"next"`+marker+`}`, 1)
	if string(got) != wantMoved {
		t.Errorf("moved marker:\n got %s\nwant %s", got, wantMoved)
	}
}

func TestEncodeRequest_UnknownBlocksDropped(t *testing.T) {
	req := pivot.Request{Model: "m", MaxTokens: 10, Messages: []pivot.Message{
		{Role: "user", Content: []pivot.Block{
			{Kind: pivot.BlockText, Text: "hi"},
			{Kind: "search_result"},
			{Kind: pivot.BlockToolResult, ToolUseID: "t", Content: []pivot.Block{{Kind: "web_fetch_result"}}},
		}},
		{Role: "assistant", Content: []pivot.Block{{Kind: "server_tool_use"}}},
	}}
	_, dropped, err := Protocol{}.EncodeRequest(req, http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"search_result", "server_tool_use", "web_fetch_result"} {
		if !strings.Contains(strings.Join(dropped, ","), k) {
			t.Errorf("dropped = %v, want it to name %s", dropped, k)
		}
	}
}
