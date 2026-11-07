package inference

import (
	"testing"

	"github.com/rossoctl/cortex/core/model"
)

// A chat-completions tool result is a "role":"tool" message naming the call it answers by
// tool_call_id. Those ids are the pairing key the timeline's tool spans close on, so they
// must reach InferenceExtension.ToolResults; the result text stays in Messages, where
// settle's cost math reads it.
func TestChatCompletionsToolResultsCarryTheCallID(t *testing.T) {
	body := []byte(`{"model":"premium-ide","messages":[
		{"role":"user","content":"list the files"},
		{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function",
			"function":{"name":"read_dir","arguments":"{\"path\":\"/tmp\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"file_a\nfile_b"},
		{"role":"tool","tool_call_id":"","content":"no id"}]}`)
	ext := parseOpenAIRequest(body)
	if ext == nil {
		t.Fatal("request not parsed")
	}
	if len(ext.ToolResults) != 1 {
		t.Fatalf("ToolResults = %+v, want only the call_1 result (an empty id pairs with nothing)", ext.ToolResults)
	}
	r := ext.ToolResults[0]
	if r.ToolUseID != "call_1" || r.Content != "file_a\nfile_b" {
		t.Fatalf("ToolResults[0] = %+v, want {call_1 file_a\\nfile_b}", r)
	}
	if got := ext.Messages[1].ToolCallNames; got != "read_dir" {
		t.Fatalf("assistant ToolCallNames = %q, want read_dir", got)
	}
	if got := ext.Messages[1].ToolUseIDs; got != "call_1" {
		t.Fatalf("assistant ToolUseIDs = %q, want call_1", got)
	}
	want := model.InferenceToolCall{ID: "call_1", Name: "read_dir", Arguments: `{"path":"/tmp"}`}
	if len(ext.HistoryToolCalls) != 1 || ext.HistoryToolCalls[0] != want {
		t.Fatalf("HistoryToolCalls = %+v, want [%+v]", ext.HistoryToolCalls, want)
	}
	var toolMsg bool
	for _, m := range ext.Messages {
		if m.Role == "tool" && m.Content == "file_a\nfile_b" {
			toolMsg = true
			if m.ToolUseID != "call_1" {
				t.Fatalf("role:tool message ToolUseID = %q, want call_1", m.ToolUseID)
			}
		}
	}
	if !toolMsg {
		t.Fatal("role:tool message content missing from Messages")
	}
}

// The history tool calls are read for display only, so a value of an unexpected JSON type
// must not fail the decode of the whole request: before they were read, these bodies parsed.
// Such a value is kept as its raw JSON text.
func TestHistoryToolCallsOfAnUnexpectedTypeKeepTheRequestParsed(t *testing.T) {
	chat := parseOpenAIRequest([]byte(`{"model":"m","messages":[
		{"role":"user","content":"list the files"},
		{"role":"assistant","content":null,"tool_calls":[{"id":7,"type":"function",
			"function":{"name":"read_dir","arguments":{"path":"/tmp"}}}]},
		{"role":"tool","tool_call_id":"7","content":"file_a"}]}`))
	if chat == nil {
		t.Fatal("chat-completions request not parsed")
	}
	want := model.InferenceToolCall{ID: "7", Name: "read_dir", Arguments: `{"path":"/tmp"}`}
	if len(chat.HistoryToolCalls) != 1 || chat.HistoryToolCalls[0] != want {
		t.Errorf("chat HistoryToolCalls = %+v, want [%+v]", chat.HistoryToolCalls, want)
	}

	anth := parseAnthropicRequest([]byte(`{"model":"m","max_tokens":8,"messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":7,"name":"Read","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"7","content":"ok"}]}]}`))
	if anth == nil {
		t.Fatal("Anthropic request not parsed")
	}
	if len(anth.ToolResults) != 1 || anth.ToolResults[0].Content != "ok" {
		t.Errorf("Anthropic ToolResults = %+v, want the one result", anth.ToolResults)
	}
	if len(anth.HistoryToolCalls) != 1 || anth.HistoryToolCalls[0].ID != "7" {
		t.Errorf("Anthropic HistoryToolCalls = %+v, want id 7", anth.HistoryToolCalls)
	}
}
