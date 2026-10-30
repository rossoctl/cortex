package inferenceparser

import "testing"

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
