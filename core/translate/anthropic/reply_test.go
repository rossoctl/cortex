package anthropic

import (
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/translate/pivot"
)

const replyToolID = "call_1__thought__a+b/c="

// replyEvents is one reply with a signed thinking block, a text block and
// a tool call in two JSON pieces.
var replyEvents = []pivot.Event{
	{Kind: pivot.EventStart, ID: "msg_1", Model: "m"},
	{Kind: pivot.EventBlockStart, Index: 0, Block: pivot.BlockThinking},
	{Kind: pivot.EventDelta, Index: 0, Delta: pivot.DeltaThinking, Value: "391"},
	{Kind: pivot.EventDelta, Index: 0, Delta: pivot.DeltaSignature, Value: "EvkB"},
	{Kind: pivot.EventBlockStop, Index: 0},
	{Kind: pivot.EventBlockStart, Index: 1, Block: pivot.BlockText},
	{Kind: pivot.EventDelta, Index: 1, Delta: pivot.DeltaText, Value: "391 is <not> prime"},
	{Kind: pivot.EventBlockStop, Index: 1},
	{Kind: pivot.EventBlockStart, Index: 2, Block: pivot.BlockToolUse, ToolID: replyToolID, ToolName: "Bash"},
	{Kind: pivot.EventDelta, Index: 2, Delta: pivot.DeltaInputJSON, Value: `{"comma`},
	{Kind: pivot.EventDelta, Index: 2, Delta: pivot.DeltaInputJSON, Value: `nd": "echo done"}`},
	{Kind: pivot.EventBlockStop, Index: 2},
	{Kind: pivot.EventUsage, Usage: pivot.Usage{Input: 5, Output: 55, CacheRead: 3, CacheCreation: 2}},
	{Kind: pivot.EventStop, StopReason: "tool_use"},
}

// encodeAll gives the bytes of each Encode call and of End, joined. It
// fails the test when a not-streamed encoder gives bytes before End.
func encodeAll(t *testing.T, stream bool, events []pivot.Event) string {
	t.Helper()
	enc := Protocol{}.NewReplyEncoder(pivot.Request{}, stream)
	var out []byte
	for _, ev := range events {
		b := enc.Encode(ev)
		if !stream && b != nil {
			t.Fatalf("not-streamed Encode(%v) = %q, want nil", ev.Kind, b)
		}
		out = append(out, b...)
	}
	return string(append(out, enc.End()...))
}

func TestReplyEncoder_Stream(t *testing.T) {
	want := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"391"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"EvkB"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"391 is <not> prime"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"call_1__thought__a+b/c=","name":"Bash","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"comma"}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"nd\": \"echo done\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"input_tokens":5,"cache_creation_input_tokens":2,"cache_read_input_tokens":3,"output_tokens":55}}

event: message_stop
data: {"type":"message_stop"}

`
	if got := encodeAll(t, true, replyEvents); got != want {
		t.Errorf("stream:\ngot  %s\nwant %s", got, want)
	}
}

func TestReplyEncoder_NotStream(t *testing.T) {
	want := `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[` +
		`{"type":"thinking","thinking":"391","signature":"EvkB"},` +
		`{"type":"text","text":"391 is <not> prime"},` +
		`{"type":"tool_use","id":"call_1__thought__a+b/c=","name":"Bash","input":{"command":"echo done"}}],` +
		`"stop_reason":"tool_use","stop_sequence":null,` +
		`"usage":{"input_tokens":5,"cache_creation_input_tokens":2,"cache_read_input_tokens":3,"output_tokens":55}}`
	if got := encodeAll(t, false, replyEvents); got != want {
		t.Errorf("not stream:\ngot  %s\nwant %s", got, want)
	}
}

// TestReplyEncoder_RedactedThinking checks rule R2b: a redacted_thinking
// block carries the data of its block_start, streamed and not streamed.
func TestReplyEncoder_RedactedThinking(t *testing.T) {
	events := []pivot.Event{
		{Kind: pivot.EventStart, ID: "msg_3", Model: "m"},
		{Kind: pivot.EventBlockStart, Index: 0, Block: pivot.BlockRedactedThinking, Data: "cmVk"},
		{Kind: pivot.EventBlockStop, Index: 0},
		{Kind: pivot.EventStop, StopReason: "end_turn"},
	}
	block := `{"type":"redacted_thinking","data":"cmVk"}`
	if got := encodeAll(t, true, events); !strings.Contains(got, `"content_block":`+block) {
		t.Errorf("stream has no %s:\n%s", block, got)
	}
	if got := encodeAll(t, false, events); !strings.Contains(got, `"content":[`+block+`]`) {
		t.Errorf("not stream has no %s:\n%s", block, got)
	}
}

func TestReplyEncoder_NotStreamToolInput(t *testing.T) {
	start := []pivot.Event{
		{Kind: pivot.EventStart, ID: "msg_2", Model: "m"},
		{Kind: pivot.EventBlockStart, Index: 0, Block: pivot.BlockToolUse, ToolID: "t1", ToolName: "Ls"},
	}
	empty := append(append([]pivot.Event{}, start...), pivot.Event{Kind: pivot.EventBlockStop, Index: 0})
	want := `{"id":"msg_2","type":"message","role":"assistant","model":"m","content":[` +
		`{"type":"tool_use","id":"t1","name":"Ls","input":{}}],"stop_reason":null,"stop_sequence":null,` +
		`"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0}}`
	if got := encodeAll(t, false, empty); got != want {
		t.Errorf("empty input:\ngot  %s\nwant %s", got, want)
	}

	bad := append(append([]pivot.Event{}, start...),
		pivot.Event{Kind: pivot.EventDelta, Index: 0, Delta: pivot.DeltaInputJSON, Value: `{"a":`})
	want = `{"type":"error","error":{"type":"api_error","message":"the inference server sent tool Ls input that is not valid JSON"}}`
	if got := encodeAll(t, false, bad); got != want {
		t.Errorf("bad input:\ngot  %s\nwant %s", got, want)
	}
}

func TestReplyEncoder_Error(t *testing.T) {
	events := []pivot.Event{
		{Kind: pivot.EventStart, ID: "msg_1", Model: "m"},
		{Kind: pivot.EventBlockStart, Index: 0, Block: pivot.BlockText},
		{Kind: pivot.EventError, Status: 502, ErrorType: "api_error", Message: "boom"},
		{Kind: pivot.EventStop, StopReason: "end_turn"},
	}
	want := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: error
data: {"type":"error","error":{"type":"api_error","message":"boom"}}

`
	if got := encodeAll(t, true, events); got != want {
		t.Errorf("stream:\ngot  %s\nwant %s", got, want)
	}
	want = `{"type":"error","error":{"type":"api_error","message":"boom"}}`
	if got := encodeAll(t, false, events); got != want {
		t.Errorf("not stream:\ngot  %s\nwant %s", got, want)
	}
}

func TestEncodeError(t *testing.T) {
	got := string(Protocol{}.EncodeError(404, "not_found_error", `model "x" <not> found`))
	want := `{"type":"error","error":{"type":"not_found_error","message":"model \"x\" <not> found"}}`
	if got != want {
		t.Errorf("EncodeError = %s, want %s", got, want)
	}
}
