package anthropic

import (
	"encoding/base64"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/translate/pivot"
)

const (
	cacheMark = `{"type":"ephemeral"}`
	topCache  = `,"cache_control":{"type":"ephemeral"}}`
)

func encode(t *testing.T, req pivot.Request) (string, []string) {
	t.Helper()
	body, dropped, err := Protocol{}.EncodeRequest(req, nil)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	return string(body), dropped
}

func checkDropped(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dropped = %q, want %q", got, want)
	}
}

func userText(s string) pivot.Message {
	return pivot.Message{Role: "user", Content: []pivot.Block{{Kind: pivot.BlockText, Text: s}}}
}

func TestEncodeRequest_TopLevelOrder(t *testing.T) {
	body, dropped := encode(t, pivot.Request{
		Model:      "m",
		MaxTokens:  100,
		System:     []pivot.Block{{Kind: pivot.BlockText, Text: "s"}},
		Messages:   []pivot.Message{userText("hi")},
		Tools:      []pivot.Tool{{Name: "t", InputSchema: raw(`{"type":"object"}`)}},
		ToolChoice: &pivot.ToolChoice{Type: "auto"},
		Stop:       []string{"x"},
		Stream:     true,
		Effort:     "high",
		Extra:      pivot.Extra{"responses.store": raw(`true`), "anthropic.metadata": raw(`{"user_id":"u"}`)},
	})
	want := `{"model":"m","max_tokens":100,"system":[{"type":"text","text":"s"}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],` +
		`"tools":[{"name":"t","input_schema":{"type":"object"}}],"tool_choice":{"type":"auto"},` +
		`"stop_sequences":["x"],"stream":true,"output_config":{"effort":"high"}` + topCache
	if body != want {
		t.Errorf("body =\n%s\nwant\n%s", body, want)
	}
	checkDropped(t, dropped, "metadata", "store")
}

func TestEncodeRequest_MaxTokens(t *testing.T) {
	for _, tc := range []struct {
		max    int
		stream bool
		want   string
	}{{0, true, "64000"}, {0, false, "16000"}, {500, true, "500"}, {500, false, "500"}} {
		body, _ := encode(t, pivot.Request{Model: "m", MaxTokens: tc.max, Stream: tc.stream})
		if p := `{"model":"m","max_tokens":` + tc.want + `,`; !strings.HasPrefix(body, p) {
			t.Errorf("max %d stream %v: body = %s, want prefix %s", tc.max, tc.stream, body, p)
		}
	}
}

func TestEncodeRequest_System(t *testing.T) {
	body, dropped := encode(t, pivot.Request{
		Model:     "m",
		MaxTokens: 5,
		System: []pivot.Block{
			{Kind: pivot.BlockText, Text: "a", CacheControl: raw(cacheMark)},
			{Kind: pivot.BlockImage, SourceType: "base64", MediaType: "image/png", Data: "iVBO"},
		},
		Messages: []pivot.Message{
			userText("u1"),
			{Role: "assistant", Content: []pivot.Block{{Kind: pivot.BlockText, Text: "a1"}}},
			{Role: "system", Content: []pivot.Block{{Kind: pivot.BlockText, Text: "s2"}}},
		},
	})
	want := `{"model":"m","max_tokens":5,"system":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"u1"}]},` +
		`{"role":"assistant","content":[{"type":"text","text":"a1"}]},` +
		`{"role":"user","content":[{"type":"text","text":"s2"}]}]}`
	if body != want {
		t.Errorf("body =\n%s\nwant\n%s", body, want)
	}
	checkDropped(t, dropped, "image")
}

func TestEncodeRequest_Blocks(t *testing.T) {
	const prefix = `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[`
	png := pivot.Block{Kind: pivot.BlockImage, SourceType: "base64", MediaType: "image/png", Data: "iVBO"}
	const pngOut = `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBO"}}`
	for _, tc := range []struct {
		name    string
		in      pivot.Block
		want    string
		dropped []string
	}{
		{"text", pivot.Block{Kind: pivot.BlockText, Text: "hi", CacheControl: raw(cacheMark)},
			`{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}`, nil},
		{"image base64", png, pngOut, nil},
		{"image url", pivot.Block{Kind: pivot.BlockImage, SourceType: "url", URL: "https://example.com/a.png"},
			`{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}`, nil},
		{"document base64", pivot.Block{Kind: pivot.BlockDocument, SourceType: "base64", MediaType: "application/pdf", Data: "JVBE"},
			`{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBE"}}`, nil},
		{"document url", pivot.Block{Kind: pivot.BlockDocument, SourceType: "url", URL: "https://example.com/a.pdf"},
			`{"type":"document","source":{"type":"url","url":"https://example.com/a.pdf"}}`, nil},
		{"tool_use", pivot.Block{Kind: pivot.BlockToolUse, ID: "toolu_1", Name: "f", Input: raw(`{"b":1,"a":[1,2]}`)},
			`{"type":"tool_use","id":"toolu_1","name":"f","input":{"b":1,"a":[1,2]}}`, nil},
		{"tool_use empty input", pivot.Block{Kind: pivot.BlockToolUse, ID: "toolu_1", Name: "f"},
			`{"type":"tool_use","id":"toolu_1","name":"f","input":{}}`, nil},
		{"tool_result", pivot.Block{Kind: pivot.BlockToolResult, ToolUseID: "toolu_1", IsError: true, Content: []pivot.Block{
			{Kind: pivot.BlockText, Text: "r"}, png, {Kind: pivot.BlockDocument, SourceType: "url", URL: "u"},
		}}, `{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"r"},` + pngOut + `],"is_error":true}`,
			[]string{"document"}},
		{"tool_result empty", pivot.Block{Kind: pivot.BlockToolResult, ToolUseID: "toolu_1"},
			`{"type":"tool_result","tool_use_id":"toolu_1"}`, nil},
		{"thinking signed", pivot.Block{Kind: pivot.BlockThinking, Text: "t", Signature: "sig"},
			`{"type":"thinking","thinking":"t","signature":"sig"}`, nil},
		{"thinking signed, empty text", pivot.Block{Kind: pivot.BlockThinking, Signature: "sig"},
			`{"type":"thinking","thinking":"","signature":"sig"}`, nil},
		{"thinking with cache_control", pivot.Block{Kind: pivot.BlockThinking, Text: "t", Signature: "sig", CacheControl: raw(cacheMark)},
			`{"type":"thinking","thinking":"t","signature":"sig"}`, []string{"cache_control"}},
		{"redacted", pivot.Block{Kind: pivot.BlockRedactedThinking, Data: "d"},
			`{"type":"redacted_thinking","data":"d"}`, nil},
		{"redacted with cache_control", pivot.Block{Kind: pivot.BlockRedactedThinking, Data: "d", CacheControl: raw(cacheMark)},
			`{"type":"redacted_thinking","data":"d"}`, []string{"cache_control"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, dropped := encode(t, pivot.Request{Model: "m", MaxTokens: 1, Messages: []pivot.Message{{Role: "user", Content: []pivot.Block{tc.in}}}})
			suffix := "]}]}"
			if tc.in.CacheControl == nil || tc.in.Kind == pivot.BlockThinking || tc.in.Kind == pivot.BlockRedactedThinking {
				suffix = "]}]" + topCache
			}
			if want := prefix + tc.want + suffix; body != want {
				t.Errorf("body =\n%s\nwant\n%s", body, want)
			}
			checkDropped(t, dropped, tc.dropped...)
		})
	}

	t.Run("thinking unsigned", func(t *testing.T) {
		body, dropped := encode(t, pivot.Request{Model: "m", MaxTokens: 1, Messages: []pivot.Message{
			{Role: "user", Content: []pivot.Block{{Kind: pivot.BlockThinking, Text: "t"}}},
		}})
		if want := `{"model":"m","max_tokens":1,"messages":[]` + topCache; body != want {
			t.Errorf("body = %s, want %s", body, want)
		}
		checkDropped(t, dropped, "thinking")
	})
}

func TestEncodeRequest_FirstMessageNotUser(t *testing.T) {
	_, _, err := Protocol{}.EncodeRequest(pivot.Request{Model: "m", Messages: []pivot.Message{
		{Role: "assistant", Content: []pivot.Block{{Kind: pivot.BlockText, Text: "a"}}},
	}}, nil)
	var re *pivot.RequestError
	if !errors.As(err, &re) {
		t.Fatalf("err = %v, want a *pivot.RequestError", err)
	}
	want := pivot.RequestError{Status: 400, Type: "invalid_request_error",
		Message: "an Anthropic Messages server needs a user message first, but the first message of this request is an assistant message"}
	if *re != want {
		t.Errorf("err = %+v, want %+v", *re, want)
	}
}

func TestEncodeRequest_EmptyMessageLeftOut(t *testing.T) {
	body, dropped := encode(t, pivot.Request{Model: "m", MaxTokens: 1, Messages: []pivot.Message{
		userText("u1"),
		{Role: "assistant", Content: []pivot.Block{{Kind: pivot.BlockThinking, Text: "t"}}},
		userText("u2"),
	}})
	want := `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"text","text":"u1"}]},` +
		`{"role":"user","content":[{"type":"text","text":"u2"}]}]` + topCache
	if body != want {
		t.Errorf("body =\n%s\nwant\n%s", body, want)
	}
	checkDropped(t, dropped, "thinking")
}

func TestToolID(t *testing.T) {
	if got := toolID("toolu_01A-b_c"); got != "toolu_01A-b_c" {
		t.Errorf("toolID(valid) = %q", got)
	}
	want := "cx_" + base64.RawURLEncoding.EncodeToString([]byte(thoughtID))
	for range 2 {
		if got := toolID(thoughtID); got != want {
			t.Errorf("toolID(%q) = %q, want %q", thoughtID, got, want)
		}
	}
}

func TestEncodeRequest_ToolIDPairs(t *testing.T) {
	body, _ := encode(t, pivot.Request{Model: "m", MaxTokens: 1, Messages: []pivot.Message{
		userText("u"),
		{Role: "assistant", Content: []pivot.Block{{Kind: pivot.BlockToolUse, ID: thoughtID, Name: "f", Input: raw(`{}`)}}},
		{Role: "user", Content: []pivot.Block{{Kind: pivot.BlockToolResult, ToolUseID: thoughtID}}},
	}})
	id := toolID(thoughtID)
	for _, s := range []string{`"id":"` + id + `"`, `"tool_use_id":"` + id + `"`} {
		if !strings.Contains(body, s) {
			t.Errorf("body %s has no %s", body, s)
		}
	}
}

func TestEncodeRequest_Tools(t *testing.T) {
	body, dropped := encode(t, pivot.Request{Model: "m", MaxTokens: 1, Tools: []pivot.Tool{
		{Name: "f", Description: "d", InputSchema: raw(`{"type":"object","properties":{}}`), CacheControl: raw(cacheMark)},
		{Name: "g"},
		{Name: "web_search", Type: "web_search_20250305"},
	}})
	want := `{"model":"m","max_tokens":1,"messages":[],"tools":[` +
		`{"name":"f","description":"d","input_schema":{"type":"object","properties":{}},"cache_control":{"type":"ephemeral"}},` +
		`{"name":"g","input_schema":{"type":"object"}}]}`
	if body != want {
		t.Errorf("body =\n%s\nwant\n%s", body, want)
	}
	checkDropped(t, dropped, "web_search_20250305")
}

func TestEncodeRequest_ToolChoice(t *testing.T) {
	for _, tc := range []struct {
		in   pivot.ToolChoice
		want string
	}{
		{pivot.ToolChoice{Type: "auto"}, `{"type":"auto"}`},
		{pivot.ToolChoice{Type: "any"}, `{"type":"any"}`},
		{pivot.ToolChoice{Type: "tool", Name: "f"}, `{"type":"tool","name":"f"}`},
		{pivot.ToolChoice{Type: "none"}, `{"type":"none"}`},
		{pivot.ToolChoice{Type: "auto", DisableParallel: true}, `{"type":"auto","disable_parallel_tool_use":true}`},
		{pivot.ToolChoice{Type: "any", DisableParallel: true}, `{"type":"any","disable_parallel_tool_use":true}`},
		{pivot.ToolChoice{Type: "tool", Name: "f", DisableParallel: true}, `{"type":"tool","name":"f","disable_parallel_tool_use":true}`},
		{pivot.ToolChoice{Type: "none", DisableParallel: true}, `{"type":"none"}`},
	} {
		body, _ := encode(t, pivot.Request{Model: "m", MaxTokens: 1, ToolChoice: &tc.in})
		if want := `"tool_choice":` + tc.want + `,`; !strings.Contains(body, want) {
			t.Errorf("%+v: body = %s, want %s", tc.in, body, want)
		}
	}
}

func TestEncodeRequest_Sampling(t *testing.T) {
	body, dropped := encode(t, pivot.Request{Model: "m", MaxTokens: 1, Stop: []string{"a", "b"}, Temperature: ptr(0.5), TopP: ptr(0.9)})
	if want := `{"model":"m","max_tokens":1,"messages":[],"stop_sequences":["a","b"]` + topCache; body != want {
		t.Errorf("body = %s, want %s", body, want)
	}
	checkDropped(t, dropped, "temperature", "top_p")
}

func TestEffortOf(t *testing.T) {
	for in, want := range map[string]string{
		"none": "low", "minimal": "low", "low": "low", "medium": "medium",
		"high": "high", "xhigh": "xhigh", "max": "max",
	} {
		if got, ok := effortOf(in); got != want || !ok {
			t.Errorf("effortOf(%q) = %q, %v; want %q, true", in, got, ok, want)
		}
	}
	if got, ok := effortOf("auto"); ok {
		t.Errorf(`effortOf("auto") = %q, true; want false`, got)
	}
}

func TestEncodeRequest_Effort(t *testing.T) {
	body, dropped := encode(t, pivot.Request{Model: "m", MaxTokens: 1})
	if strings.Contains(body, "output_config") {
		t.Errorf("no effort: body = %s", body)
	}
	checkDropped(t, dropped)

	body, dropped = encode(t, pivot.Request{Model: "m", MaxTokens: 1, Effort: "minimal", Thinking: &pivot.Thinking{Type: "enabled", BudgetTokens: 1024}})
	if strings.Contains(body, `"thinking"`) || !strings.Contains(body, `"output_config":{"effort":"low"}`) {
		t.Errorf("thinking: body = %s", body)
	}
	checkDropped(t, dropped, "thinking")

	body, dropped = encode(t, pivot.Request{Model: "m", MaxTokens: 1, Effort: "auto"})
	if strings.Contains(body, "output_config") {
		t.Errorf("effort auto: body = %s", body)
	}
	checkDropped(t, dropped, "effort")
}

func TestEncodeRequest_CacheControl(t *testing.T) {
	body, _ := encode(t, pivot.Request{Model: "m", MaxTokens: 1, Messages: []pivot.Message{userText("u")}})
	if !strings.HasSuffix(body, topCache) {
		t.Errorf("no marker: body = %s", body)
	}
	for name, req := range map[string]pivot.Request{
		"tool": {Tools: []pivot.Tool{{Name: "f", CacheControl: raw(cacheMark)}}},
		"tool_result content": {Messages: []pivot.Message{{Role: "user", Content: []pivot.Block{{Kind: pivot.BlockToolResult, ToolUseID: "t",
			Content: []pivot.Block{{Kind: pivot.BlockText, Text: "r", CacheControl: raw(cacheMark)}}}}}}},
		"system": {System: []pivot.Block{{Kind: pivot.BlockText, Text: "s", CacheControl: raw(cacheMark)}}},
	} {
		req.Model, req.MaxTokens = "m", 1
		body, _ := encode(t, req)
		if strings.HasSuffix(body, topCache) || strings.Count(body, `"cache_control":`+cacheMark) != 1 {
			t.Errorf("%s: body = %s, want one marker, where it was", name, body)
		}
	}
}

func TestEncodeRequest_Headers(t *testing.T) {
	h := http.Header{"Authorization": {"Bearer k"}, "Anthropic-Beta": {"b"}}
	if _, _, err := (Protocol{}).EncodeRequest(pivot.Request{Model: "m"}, h); err != nil {
		t.Fatal(err)
	}
	want := http.Header{"Authorization": {"Bearer k"}, "Anthropic-Version": {"2023-06-01"}}
	if !reflect.DeepEqual(h, want) {
		t.Errorf("headers = %v, want %v", h, want)
	}
	if _, _, err := (Protocol{}).EncodeRequest(pivot.Request{Model: "m"}, nil); err != nil {
		t.Fatal(err)
	}
}
