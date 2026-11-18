package anthropic

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/rossoctl/cortex/core/translate/pivot"
)

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func ptr(f float64) *float64 { return &f }

// thoughtID is a tool id with a thought signature in it (rule 20a). It has
// "+", "/" and "=", and must pass byte-exact.
const thoughtID = "call_3833__thought__AY89a1+8zd/x="

func TestDecodeRequest_Fields(t *testing.T) {
	body := `{"model":"claude-opus-5-5","max_tokens":32000,"stream":true,"temperature":0.5,"top_p":0.9,` +
		`"top_k":40,"metadata":{"user_id":"u1"},"stop_sequences":["END"],` +
		`"system":[{"type":"text","text":"You are Claude Code.","cache_control":{"type":"ephemeral"}}],` +
		`"output_config":{"effort":"xhigh","format":{"type":"json"}},` +
		`"thinking":{"type":"enabled","budget_tokens":1024},` +
		`"tool_choice":{"type":"auto","disable_parallel_tool_use":true},` +
		`"tools":[{"name":"Bash","description":"Run a command","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}},` +
		`{"type":"custom","name":"Read","input_schema":{"type":"object"}},` +
		`{"type":"web_search_20250305","name":"web_search","max_uses":5}],` +
		`"messages":[` +
		`{"role":"user","content":"Is 391 prime?"},` +
		`{"role":"system","content":"hook output"},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"391 = 17*23","signature":"EvkB"},` +
		`{"type":"redacted_thinking","data":"cmVk"},{"type":"text","text":"Let me check."},` +
		`{"type":"tool_use","id":"` + thoughtID + `","name":"Bash","input":{"command":"factor 391"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + thoughtID + `","content":"391: 17 23","is_error":true},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBO"}},` +
		`{"type":"image","source":{"type":"url","url":"https://example.com/a.png","foo":1}},` +
		`{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBE"},"title":"a.pdf"},` +
		`{"type":"text","text":"Go on.","cache_control":{"type":"ephemeral","ttl":"1h"}}],"turn":7}]}`

	want := pivot.Request{
		Model: "claude-opus-5-5",
		System: []pivot.Block{
			{Kind: pivot.BlockText, Text: "You are Claude Code.", CacheControl: raw(`{"type":"ephemeral"}`)},
		},
		Messages: []pivot.Message{
			{Role: "user", Content: []pivot.Block{{Kind: pivot.BlockText, Text: "Is 391 prime?"}}},
			{Role: "system", Content: []pivot.Block{{Kind: pivot.BlockText, Text: "hook output"}}},
			{Role: "assistant", Content: []pivot.Block{
				{Kind: pivot.BlockThinking, Text: "391 = 17*23", Signature: "EvkB"},
				{Kind: pivot.BlockRedactedThinking, Data: "cmVk"},
				{Kind: pivot.BlockText, Text: "Let me check."},
				{Kind: pivot.BlockToolUse, ID: thoughtID, Name: "Bash", Input: raw(`{"command":"factor 391"}`)},
			}},
			{Role: "user", Content: []pivot.Block{
				{Kind: pivot.BlockToolResult, ToolUseID: thoughtID, IsError: true,
					Content: []pivot.Block{{Kind: pivot.BlockText, Text: "391: 17 23"}}},
				{Kind: pivot.BlockImage, SourceType: "base64", MediaType: "image/png", Data: "iVBO"},
				{Kind: pivot.BlockImage, SourceType: "url", URL: "https://example.com/a.png",
					Extra: pivot.Extra{"anthropic.source": raw(`{"foo":1}`)}},
				{Kind: pivot.BlockDocument, SourceType: "base64", MediaType: "application/pdf", Data: "JVBE",
					Extra: pivot.Extra{"anthropic.title": raw(`"a.pdf"`)}},
				{Kind: pivot.BlockText, Text: "Go on.", CacheControl: raw(`{"type":"ephemeral","ttl":"1h"}`)},
			}, Extra: pivot.Extra{"anthropic.turn": raw(`7`)}},
		},
		Tools: []pivot.Tool{
			{Name: "Bash", Description: "Run a command", InputSchema: raw(`{"type":"object"}`), CacheControl: raw(`{"type":"ephemeral"}`)},
			{Name: "Read", InputSchema: raw(`{"type":"object"}`)},
			{Name: "web_search", Type: "web_search_20250305", Extra: pivot.Extra{"anthropic.max_uses": raw(`5`)}},
		},
		ToolChoice:  &pivot.ToolChoice{Type: "auto", DisableParallel: true},
		MaxTokens:   32000,
		Stop:        []string{"END"},
		Temperature: ptr(0.5),
		TopP:        ptr(0.9),
		Stream:      true,
		Effort:      "xhigh",
		Thinking:    &pivot.Thinking{Type: "enabled", BudgetTokens: 1024},
		Extra: pivot.Extra{
			"anthropic.top_k":         raw(`40`),
			"anthropic.metadata":      raw(`{"user_id":"u1"}`),
			"anthropic.output_config": raw(`{"format":{"type":"json"}}`),
		},
	}

	got, err := Protocol{}.DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		g, _ := json.MarshalIndent(got, "", " ")
		w, _ := json.MarshalIndent(want, "", " ")
		t.Errorf("DecodeRequest:\ngot  %s\nwant %s", g, w)
	}
}

func TestDecodeRequest_SystemString(t *testing.T) {
	got, err := Protocol{}.DecodeRequest([]byte(`{"model":"m","system":"Be brief.","output_config":{"effort":"low"}}`))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	want := pivot.Request{Model: "m", System: []pivot.Block{{Kind: pivot.BlockText, Text: "Be brief."}}, Effort: "low"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DecodeRequest = %+v, want %+v", got, want)
	}
}

func TestDecodeRequest_BadJSON(t *testing.T) {
	for _, body := range []string{
		`{"model":`,
		`[]`,
		`null`,
		`{"model":5}`,
		`{"messages":[{"role":"user","content":5}]}`,
		`{"messages":[{"role":"user","content":[{"text":"no type"}]}]}`,
	} {
		_, err := Protocol{}.DecodeRequest([]byte(body))
		var re *pivot.RequestError
		if !errors.As(err, &re) {
			t.Errorf("%s: err = %v, want *pivot.RequestError", body, err)
			continue
		}
		if re.Status != 400 || re.Type != "invalid_request_error" {
			t.Errorf("%s: got %d %s, want 400 invalid_request_error", body, re.Status, re.Type)
		}
	}
}
