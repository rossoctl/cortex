package chat

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/rossoctl/cortex/core/translate/pivot"
)

var (
	yes      = json.RawMessage("true")
	ttlOneH  = json.RawMessage(`{"type":"ephemeral","ttl":"1h"}`)
	textOnly = func(s string) pivot.Block { return pivot.Block{Kind: pivot.BlockText, Text: s} }
)

func decodeOK(t *testing.T, body string) pivot.Request {
	t.Helper()
	req, err := Protocol{}.DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest(%s): %v", body, err)
	}
	return req
}

func wantRequest(t *testing.T, got, want pivot.Request) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		g, _ := json.MarshalIndent(got, "", "  ")
		w, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("got\n%s\nwant\n%s", g, w)
	}
}

func TestDecodeRequest_System(t *testing.T) {
	got := decodeOK(t, `{"model":"m","messages":[
		{"role":"system","content":"s1"},
		{"role":"developer","content":[{"type":"text","text":"d1"},{"type":"text","text":"d2","cache_control":{"type":"ephemeral"}}]},
		{"role":"user","content":"u"},
		{"role":"developer","content":"later"}]}`)
	wantRequest(t, got, pivot.Request{
		Model: "m",
		System: []pivot.Block{textOnly("s1"), textOnly("d1"),
			{Kind: pivot.BlockText, Text: "d2", CacheControl: ephemeral}},
		Messages: []pivot.Message{
			{Role: "user", Content: []pivot.Block{textOnly("u")}},
			{Role: "system", Content: []pivot.Block{textOnly("later")}},
		},
	})
}

func TestDecodeRequest_UserParts(t *testing.T) {
	got := decodeOK(t, `{"model":"m","messages":[{"role":"user","content":[
		{"type":"text","text":"t","cache_control":{"type":"ephemeral","ttl":"1h"}},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBO"}},
		{"type":"image_url","image_url":{"url":"https://x.example/y.png","detail":"high"}},
		{"type":"image_url","image_url":"http://x.example/z.png"},
		{"type":"file","file":{"file_data":"data:application/pdf;base64,JVBE","filename":"a.pdf"}}]},
		{"role":"user","content":""}]}`)
	wantRequest(t, got, pivot.Request{
		Model: "m",
		Messages: []pivot.Message{{Role: "user", Content: []pivot.Block{
			{Kind: pivot.BlockText, Text: "t", CacheControl: ttlOneH},
			{Kind: pivot.BlockImage, SourceType: "base64", MediaType: "image/png", Data: "iVBO"},
			{Kind: pivot.BlockImage, SourceType: "url", URL: "https://x.example/y.png", Extra: pivot.Extra{"chat-completions.detail": yes}},
			{Kind: pivot.BlockImage, SourceType: "url", URL: "http://x.example/z.png"},
			{Kind: pivot.BlockDocument, SourceType: "base64", MediaType: "application/pdf", Data: "JVBE", Extra: pivot.Extra{"chat-completions.filename": yes}},
		}}, {Role: "user", Content: []pivot.Block{textOnly("")}}},
	})
}

func TestDecodeRequest_Refusals(t *testing.T) {
	const legacy = "Cortex cannot translate legacy function_call messages. Use tool_calls."
	const invalid = "the request body is not a valid Chat Completions request: "
	user := func(part string) string { return `{"messages":[{"role":"user","content":[` + part + `]}]}` }
	for _, tc := range []struct{ name, body, msg string }{
		{"file_id", user(`{"type":"file","file":{"file_id":"file-1"}}`), "this inference server cannot read uploaded files (file_id)"},
		{"input_audio", user(`{"type":"input_audio","input_audio":{"data":"x","format":"wav"}}`), "this inference server cannot read audio"},
		{"assistant function_call", `{"messages":[{"role":"assistant","function_call":{"name":"f","arguments":"{}"}}]}`, legacy},
		{"functions", `{"messages":[],"functions":[{"name":"f"}]}`, legacy},
		{"top function_call", `{"messages":[],"function_call":"auto"}`, legacy},
		{"n 2", `{"messages":[],"n":2}`, "Cortex can translate only one choice (n = 1)."},
		{"response_format", `{"messages":[],"response_format":{"type":"json_object"}}`, "Cortex cannot translate structured output (response_format) yet."},
		{"custom tool", `{"messages":[],"tools":[{"type":"custom","custom":{"name":"c"}}]}`, `Cortex cannot translate a tool of type "custom".`},
		{"bad arguments", `{"messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"Bash","arguments":"{x"}}]}]}`, `the arguments of a call to the tool "Bash" are not valid JSON`},
		{"bad image_url", user(`{"type":"image_url","image_url":{"url":"ftp://x/y.png"}}`), "an image_url must have a base64 data URL or an http(s) URL"},
		{"not base64 image", user(`{"type":"image_url","image_url":{"url":"data:image/png,abc"}}`), "an image_url must have a base64 data URL or an http(s) URL"},
		{"not pdf", user(`{"type":"file","file":{"file_data":"data:text/plain;base64,aGk="}}`), "Cortex can translate only PDF files (file_data as a data:application/pdf;base64 URL)."},
		{"role bot", `{"messages":[{"role":"bot","content":"x"}]}`, invalid + `a message has the role "bot"`},
		{"array body", `[]`, invalid + "must be a JSON object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Protocol{}.DecodeRequest([]byte(tc.body))
			wantRefusal(t, err, tc.msg)
		})
	}
	// The detail of a wrong type comes from encoding/json; check only the
	// prefix.
	_, err := Protocol{}.DecodeRequest([]byte(`{"messages":5}`))
	var re *pivot.RequestError
	if !errors.As(err, &re) || re.Status != 400 || len(re.Message) < len(invalid) || re.Message[:len(invalid)] != invalid {
		t.Errorf(`{"messages":5}: err = %v, want a 400 that starts with %q`, err, invalid)
	}
}

func TestDecodeRequest_Assistant(t *testing.T) {
	const id = "call_3833__thought__AY89a1+8zd/x="
	got := decodeOK(t, `{"model":"m","messages":[
		{"role":"assistant","content":"a",
		 "thinking_blocks":[{"type":"thinking","thinking":"th","signature":"sig"},{"type":"redacted_thinking","data":"enc"}],
		 "tool_calls":[{"id":"`+id+`","type":"function","function":{"name":"Bash","arguments":"{\"a\": 1}"}},
		               {"id":"c2","type":"function","function":{"name":"Read","arguments":""}}]},
		{"role":"assistant","content":"b","reasoning_content":"r"},
		{"role":"assistant","content":[{"type":"text","text":"c"},{"type":"refusal","refusal":"no"}],"reasoning":"r"},
		{"role":"assistant","content":[{"type":"refusal","refusal":"no"}]}]}`)
	wantRequest(t, got, pivot.Request{
		Model: "m",
		Messages: []pivot.Message{
			{Role: "assistant", Content: []pivot.Block{
				{Kind: pivot.BlockThinking, Text: "th", Signature: "sig"},
				{Kind: pivot.BlockRedactedThinking, Data: "enc"},
				textOnly("a"),
				{Kind: pivot.BlockToolUse, ID: id, Name: "Bash", Input: json.RawMessage(`{"a": 1}`)},
				{Kind: pivot.BlockToolUse, ID: "c2", Name: "Read"},
			}},
			{Role: "assistant", Content: []pivot.Block{{Kind: pivot.BlockThinking, Text: "r"}, textOnly("b")}},
			{Role: "assistant", Content: []pivot.Block{{Kind: pivot.BlockThinking, Text: "r"}, textOnly("c")},
				Extra: pivot.Extra{"chat-completions.refusal": yes}},
		},
		// The left-out message's refusal is still named (decision 20).
		Extra: pivot.Extra{"chat-completions.refusal": yes},
	})
}

func TestDecodeRequest_ToolResultsJoin(t *testing.T) {
	got := decodeOK(t, `{"model":"m","messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","tool_calls":[
			{"id":"c1","type":"function","function":{"name":"Bash","arguments":"{}"}},
			{"id":"c2","type":"function","function":{"name":"Read","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"out1"},
		{"role":"tool","tool_call_id":"c2","content":[{"type":"text","text":"out2","cache_control":{"type":"ephemeral"}},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBO"},"cache_control":{"type":"ephemeral"}}]},
		{"role":"user","content":"next"},
		{"role":"user","content":"again"}]}`)
	wantRequest(t, got, pivot.Request{
		Model: "m",
		Messages: []pivot.Message{
			{Role: "user", Content: []pivot.Block{textOnly("go")}},
			{Role: "assistant", Content: []pivot.Block{
				{Kind: pivot.BlockToolUse, ID: "c1", Name: "Bash", Input: json.RawMessage(`{}`)},
				{Kind: pivot.BlockToolUse, ID: "c2", Name: "Read", Input: json.RawMessage(`{}`)},
			}},
			{Role: "user", Content: []pivot.Block{
				{Kind: pivot.BlockToolResult, ToolUseID: "c1", Content: []pivot.Block{textOnly("out1")}},
				{Kind: pivot.BlockToolResult, ToolUseID: "c2", CacheControl: ephemeral, Content: []pivot.Block{
					textOnly("out2"),
					{Kind: pivot.BlockImage, SourceType: "base64", MediaType: "image/png", Data: "iVBO"},
				}},
				textOnly("next"),
			}},
			{Role: "user", Content: []pivot.Block{textOnly("again")}},
		},
	})
}

func TestDecodeRequest_Fields(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	for _, tc := range []struct {
		name, fields string
		want         pivot.Request
	}{
		{"model and stream", `"model":"m","stream":true`, pivot.Request{Model: "m", Stream: true}},
		{"max_completion_tokens wins", `"max_completion_tokens":100,"max_tokens":50`, pivot.Request{MaxTokens: 100}},
		{"max_tokens", `"max_tokens":50`, pivot.Request{MaxTokens: 50}},
		{"stop string", `"stop":"END"`, pivot.Request{Stop: []string{"END"}}},
		{"stop array", `"stop":["a","b"]`, pivot.Request{Stop: []string{"a", "b"}}},
		{"sampling", `"temperature":0.5,"top_p":0.9`, pivot.Request{Temperature: f(0.5), TopP: f(0.9)}},
		{"effort", `"reasoning_effort":"minimal"`, pivot.Request{Effort: "minimal"}},
		{"tools", `"tools":[{"type":"function","function":{"name":"Bash","description":"run","parameters":{"type":"object", "b":1},"strict":true}}]`,
			pivot.Request{Tools: []pivot.Tool{{Name: "Bash", Description: "run", InputSchema: json.RawMessage(`{"type":"object", "b":1}`), Extra: pivot.Extra{"chat-completions.strict": yes}}}}},
		{"choice auto", `"tool_choice":"auto"`, pivot.Request{ToolChoice: &pivot.ToolChoice{Type: "auto"}}},
		{"choice none", `"tool_choice":"none"`, pivot.Request{ToolChoice: &pivot.ToolChoice{Type: "none"}}},
		{"choice required", `"tool_choice":"required"`, pivot.Request{ToolChoice: &pivot.ToolChoice{Type: "any"}}},
		{"choice function", `"tool_choice":{"type":"function","function":{"name":"Bash"}}`, pivot.Request{ToolChoice: &pivot.ToolChoice{Type: "tool", Name: "Bash"}}},
		{"choice other", `"tool_choice":"validated"`, pivot.Request{Extra: pivot.Extra{"chat-completions.tool_choice": json.RawMessage(`"validated"`)}}},
		{"parallel false", `"parallel_tool_calls":false`, pivot.Request{ToolChoice: &pivot.ToolChoice{Type: "auto", DisableParallel: true}}},
		{"parallel false with choice", `"tool_choice":"required","parallel_tool_calls":false`, pivot.Request{ToolChoice: &pivot.ToolChoice{Type: "any", DisableParallel: true}}},
		{"stream_options", `"stream_options":{"include_usage":true,"x":1}`, pivot.Request{Extra: pivot.Extra{
			"chat-completions.include_usage": yes, "chat-completions.stream_options.x": json.RawMessage("1")}}},
		{"nulls", `"model":null,"stop":null,"tools":null,"seed":null`, pivot.Request{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeOK(t, `{"messages":[],`+tc.fields+`}`)
			wantRequest(t, got, tc.want)
		})
	}
}

func TestDecodeRequest_Extra(t *testing.T) {
	fields := map[string]string{
		"logprobs": `true`, "top_logprobs": `2`, "seed": `7`, "user": `"u"`, "metadata": `{"a":"b"}`,
		"store": `false`, "prediction": `{"type":"content","content":"x"}`, "modalities": `["text"]`,
		"audio": `{"voice":"v"}`, "service_tier": `"auto"`, "frequency_penalty": `0.1`,
		"presence_penalty": `0.2`, "logit_bias": `{"1":2}`, "prompt_cache_key": `"k"`,
		"safety_identifier": `"s"`, "web_search_options": `{}`, "zzz_unknown": `[1, 2]`,
	}
	body := `{"messages":[{"role":"user","content":"hi","name":"bob"}],"n":1,"response_format":{"type":"text"},"parallel_tool_calls":true`
	want := pivot.Extra{}
	for k, v := range fields {
		body += `,"` + k + `":` + v
		want["chat-completions."+k] = json.RawMessage(v)
	}
	got := decodeOK(t, body+"}")
	if !reflect.DeepEqual(got.Extra, want) {
		t.Errorf("Extra = %s, want %s", raw(got.Extra), raw(want))
	}
	if got.ToolChoice != nil {
		t.Errorf("ToolChoice = %+v, want nil", got.ToolChoice)
	}
	wantMsg := pivot.Message{Role: "user", Content: []pivot.Block{textOnly("hi")}, Extra: pivot.Extra{"chat-completions.name": json.RawMessage(`"bob"`)}}
	if !reflect.DeepEqual(got.Messages, []pivot.Message{wantMsg}) {
		t.Errorf("Messages = %+v, want %+v", got.Messages, wantMsg)
	}
}

func raw(x pivot.Extra) string { b, _ := json.Marshal(x); return string(b) }

func TestRemoveSourceHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("OpenAI-Beta", "v")
	h["openai-organization"] = []string{"o"}
	h.Set("OpenAI-Project", "p")
	h.Set("Authorization", "Bearer k")
	h.Set("X-Keep", "1")
	Protocol{}.RemoveSourceHeaders(h)
	want := http.Header{"Authorization": {"Bearer k"}, "X-Keep": {"1"}}
	if !reflect.DeepEqual(h, want) {
		t.Errorf("headers = %v, want %v", h, want)
	}
}
