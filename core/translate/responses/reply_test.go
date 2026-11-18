package responses

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/translate/chat"
	"github.com/rossoctl/cortex/core/translate/pivot"
)

func start() pivot.Event { return pivot.Event{Kind: pivot.EventStart, ID: "resp-1", Model: "claude"} }
func open(i int, k pivot.BlockKind) pivot.Event {
	return pivot.Event{Kind: pivot.EventBlockStart, Index: i, Block: k}
}
func openTool(i int, id, name string) pivot.Event {
	return pivot.Event{Kind: pivot.EventBlockStart, Index: i, Block: pivot.BlockToolUse, ToolID: id, ToolName: name}
}
func delta(i int, k pivot.DeltaKind, v string) pivot.Event {
	return pivot.Event{Kind: pivot.EventDelta, Index: i, Delta: k, Value: v}
}
func closeBlock(i int) pivot.Event    { return pivot.Event{Kind: pivot.EventBlockStop, Index: i} }
func usage(u pivot.Usage) pivot.Event { return pivot.Event{Kind: pivot.EventUsage, Usage: u} }
func stop(r string) pivot.Event       { return pivot.Event{Kind: pivot.EventStop, StopReason: r} }
func apiError(status int, typ, msg string) pivot.Event {
	return pivot.Event{Kind: pivot.EventError, Status: status, ErrorType: typ, Message: msg}
}

// codexReq is a decoded request with a function tool, a custom tool and
// a namespaced tool, so that its Origin holds all three kinds.
func codexReq(t testing.TB) pivot.Request {
	t.Helper()
	req, err := Protocol{}.DecodeRequest([]byte(`{"model":"gpt-5.5-codex","input":"hi","tools":[` +
		`{"type":"function","name":"shell","parameters":{"type":"object"}},` +
		`{"type":"custom","name":"apply_patch","format":{"type":"grammar","syntax":"lark","definition":"start: x"}},` +
		`{"type":"namespace","name":"mcp__github","tools":[{"type":"function","name":"create_issue","parameters":{"type":"object"}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func encode(req pivot.Request, stream bool, evs []pivot.Event) []byte {
	enc := Protocol{}.NewReplyEncoder(req, stream)
	var out []byte
	for _, ev := range evs {
		out = append(out, enc.Encode(ev)...)
	}
	return append(out, enc.End()...)
}

var usageCache = pivot.Usage{Input: 100, Output: 20, CacheRead: 300, CacheCreation: 50}

func textBlock(i int, text string) []pivot.Event {
	return []pivot.Event{open(i, pivot.BlockText), delta(i, pivot.DeltaText, text), closeBlock(i)}
}

func events(parts ...any) []pivot.Event {
	var out []pivot.Event
	for _, p := range parts {
		switch v := p.(type) {
		case pivot.Event:
			out = append(out, v)
		case []pivot.Event:
			out = append(out, v...)
		}
	}
	return out
}

// replyCases are the hand-written pivot event lists of TestReplyGolden.
func replyCases() map[string][]pivot.Event {
	u := usage(pivot.Usage{Input: 10, Output: 5})
	return map[string][]pivot.Event{
		"text_only": events(start(), open(0, pivot.BlockText), delta(0, pivot.DeltaText, "Hello"),
			delta(0, pivot.DeltaText, " world"), closeBlock(0), u, stop("end_turn")),
		"thinking_signature": events(start(), open(0, pivot.BlockThinking), delta(0, pivot.DeltaThinking, "Let me think."),
			delta(0, pivot.DeltaSignature, "EvkBsig"), closeBlock(0), textBlock(1, "391 = 17 x 23"), u, stop("end_turn")),
		"redacted_thinking": events(start(), pivot.Event{Kind: pivot.EventBlockStart, Index: 0, Block: pivot.BlockRedactedThinking, Data: "EmwKAhgB"},
			closeBlock(0), textBlock(1, "Done."), u, stop("end_turn")),
		"function_call": events(start(), openTool(0, "call_1", "shell"), delta(0, pivot.DeltaInputJSON, `{"command":`),
			delta(0, pivot.DeltaInputJSON, `["ls"]}`), closeBlock(0), u, stop("tool_use")),
		"namespaced_function_call": events(start(), openTool(0, "call_2", "mcp__github__create_issue"),
			delta(0, pivot.DeltaInputJSON, `{"title":"x"}`), closeBlock(0), u, stop("tool_use")),
		"custom_tool_call": events(start(), openTool(0, "call_3", "apply_patch"), delta(0, pivot.DeltaInputJSON, `{"input":"*** Begin Patch`),
			delta(0, pivot.DeltaInputJSON, `\n*** End Patch"}`), closeBlock(0), u, stop("tool_use")),
		"custom_bad_input": events(start(), openTool(0, "call_4", "apply_patch"), delta(0, pivot.DeltaInputJSON, `{"patch":"x"}`),
			closeBlock(0), u, stop("tool_use")),
		"max_tokens":       events(start(), textBlock(0, "The answer is"), u, stop("max_tokens")),
		"refusal":          events(start(), textBlock(0, "I cannot"), u, stop("refusal")),
		"error_event":      events(start(), open(0, pivot.BlockText), delta(0, pivot.DeltaText, "Hal"), apiError(429, "rate_limit_error", "Slow down.")),
		"error_overloaded": events(apiError(529, "overloaded_error", "Overloaded")),
		"error_context":    events(apiError(400, "invalid_request_error", "prompt is too long: 210000 tokens > 200000 maximum")),
		"usage_cache":      events(start(), textBlock(0, "ok"), usage(usageCache), stop("end_turn")),
	}
}

// TestReplyGolden encodes each pivot event list as a Responses reply,
// streamed and not streamed (rules R11-R13).
func TestReplyGolden(t *testing.T) {
	req := codexReq(t)
	for name, evs := range replyCases() {
		for _, stream := range []bool{true, false} {
			path := "testdata/reply/" + name + ".json.golden"
			if stream {
				path = "testdata/reply/" + name + ".sse.golden"
			}
			t.Run(filepath.Base(path), func(t *testing.T) {
				checkGolden(t, path, encode(req, stream, evs))
			})
		}
	}
}

// chatCase is one chat-completions reply of the #1911 fixtures.
type chatCase struct {
	path   string
	status int
	stream bool
}

func chatCases(t *testing.T) []chatCase {
	t.Helper()
	var inputs []string
	for _, g := range []string{"../testdata/reply/bob_shape*", "../testdata/reply/error_*"} {
		m, _ := filepath.Glob(g)
		for _, p := range m {
			if !strings.HasSuffix(p, ".golden") {
				inputs = append(inputs, p)
			}
		}
	}
	if len(inputs) == 0 {
		t.Fatal("no chat reply inputs")
	}
	var out []chatCase
	for _, in := range inputs {
		c := chatCase{path: in, status: 200, stream: strings.HasSuffix(in, ".sse")}
		if rest, ok := strings.CutPrefix(filepath.Base(in), "error_"); ok {
			n, err := strconv.Atoi(strings.SplitN(rest, "_", 2)[0])
			if err != nil {
				t.Fatalf("%s: no status in the name", in)
			}
			c.status = n
		}
		out = append(out, c)
	}
	return out
}

// convertChat decodes a chat reply and encodes it as Responses, as
// translate.Reply does. feeds are the pieces of the body.
func convertChat(c chatCase, agentStream bool, feeds ...[]byte) (int, []byte) {
	dec := chat.Protocol{}.NewReplyDecoder(c.status, c.stream)
	enc := Protocol{}.NewReplyEncoder(pivot.Request{Model: "claude"}, agentStream)
	status := c.status
	var evs []pivot.Event
	for _, f := range feeds {
		evs = append(evs, dec.Feed(f)...)
	}
	evs = append(evs, dec.End()...)
	var out []byte
	for _, ev := range evs {
		if ev.Kind == pivot.EventError && ev.Status != 0 {
			status = ev.Status
		}
		out = append(out, enc.Encode(ev)...)
	}
	return status, append(out, enc.End()...)
}

// TestChatRepliesAsResponses converts the chat reply fixtures of #1911 to
// Responses replies (rules R11-R13).
func TestChatRepliesAsResponses(t *testing.T) {
	for _, c := range chatCases(t) {
		body, err := os.ReadFile(c.path)
		if err != nil {
			t.Fatal(err)
		}
		forms := []bool{c.stream}
		if c.status/100 != 2 {
			forms = []bool{true, false}
		}
		for _, agentStream := range forms {
			name := "testdata/reply/" + filepath.Base(c.path) + ".to-responses-json.golden"
			if agentStream {
				name = "testdata/reply/" + filepath.Base(c.path) + ".to-responses-sse.golden"
			}
			t.Run(filepath.Base(name), func(t *testing.T) {
				status, out := convertChat(c, agentStream, body)
				checkGolden(t, name, out)
				if c.status/100 == 2 {
					return
				}
				// R12: the same status, and the OpenAI error body.
				if status != c.status {
					t.Errorf("status %d, want %d", status, c.status)
				}
				if !agentStream {
					var e struct {
						Error struct{ Message, Type, Code string }
					}
					if err := json.Unmarshal(out, &e); err != nil || e.Error.Message == "" || e.Error.Code == "" {
						t.Errorf("not an OpenAI error body: %s (%v)", out, err)
					}
				}
			})
		}
	}
}

// TestSplitAtEveryByte feeds each Bob stream split in two at each byte,
// and the reply is the same as for one whole feed.
func TestSplitAtEveryByte(t *testing.T) {
	inputs, _ := filepath.Glob("../testdata/reply/bob_shape*.sse")
	if len(inputs) == 0 {
		t.Fatal("no bob_shape streams")
	}
	for _, in := range inputs {
		body, err := os.ReadFile(in)
		if err != nil {
			t.Fatal(err)
		}
		c := chatCase{path: in, status: 200, stream: true}
		_, whole := convertChat(c, true, body)
		for i := 1; i < len(body); i++ {
			if _, got := convertChat(c, true, body[:i], body[i:]); !bytes.Equal(got, whole) {
				t.Fatalf("%s split at %d:\n got %s\nwant %s", in, i, got, whole)
			}
		}
	}
}

// TestEncode_UsageShape checks rule R11: input_tokens includes both cache
// counts, and the details name each one.
func TestEncode_UsageShape(t *testing.T) {
	out := encode(pivot.Request{}, false, replyCases()["usage_cache"])
	var r struct {
		Usage struct {
			InputTokens        int `json:"input_tokens"`
			InputTokensDetails struct {
				CachedTokens     int `json:"cached_tokens"`
				CacheWriteTokens int `json:"cache_write_tokens"`
			} `json:"input_tokens_details"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		}
	}
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatal(err)
	}
	u := r.Usage
	if u.InputTokens != 450 || u.InputTokensDetails.CachedTokens != 300 || u.InputTokensDetails.CacheWriteTokens != 50 ||
		u.OutputTokens != 20 || u.TotalTokens != 470 {
		t.Errorf("usage %+v", u)
	}
}

// TestEncode_ItemIDs checks that each item id is its kind's prefix and
// its output index.
func TestEncode_ItemIDs(t *testing.T) {
	evs := events(start(), textBlock(0, "a"),
		open(1, pivot.BlockThinking), delta(1, pivot.DeltaThinking, "t"), delta(1, pivot.DeltaSignature, "s"), closeBlock(1),
		openTool(2, "call_a", "shell"), delta(2, pivot.DeltaInputJSON, `{}`), closeBlock(2),
		openTool(3, "call_b", "apply_patch"), delta(3, pivot.DeltaInputJSON, `{"input":"p"}`), closeBlock(3),
		stop("tool_use"))
	var r struct {
		Output []struct{ ID string }
	}
	if err := json.Unmarshal(encode(codexReq(t), false, evs), &r); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, it := range r.Output {
		ids = append(ids, it.ID)
	}
	if want := []string{"msg_0", "rs_1", "fc_2", "ctc_3"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("ids %v, want %v", ids, want)
	}
}

// doneItems gives the item of each response.output_item.done frame.
func doneItems(t *testing.T, sse []byte) []json.RawMessage {
	t.Helper()
	var items []json.RawMessage
	for _, line := range strings.Split(string(sse), "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || !strings.Contains(data, `"type":"response.output_item.done"`) {
			continue
		}
		var ev struct{ Item json.RawMessage }
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatal(err)
		}
		items = append(items, ev.Item)
	}
	return items
}

// TestEncode_ReasoningRoundTrip checks rule R14: the encrypted_content of
// a streamed reasoning item, sent back in a request, gives the same block.
func TestEncode_ReasoningRoundTrip(t *testing.T) {
	want := []pivot.Block{
		{Kind: pivot.BlockThinking, Text: "Let me think.", Signature: "EvkBsig"},
		{Kind: pivot.BlockRedactedThinking, Data: "EmwKAhgB"},
	}
	for _, name := range []string{"thinking_signature", "redacted_thinking"} {
		items := doneItems(t, encode(pivot.Request{}, true, replyCases()[name]))
		if len(items) == 0 {
			t.Fatalf("%s: no items", name)
		}
		body := `{"model":"m","input":[` + string(items[0]) + `]}`
		req, err := Protocol{}.DecodeRequest([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if len(req.Messages) != 1 || len(req.Messages[0].Content) != 1 {
			t.Fatalf("%s: messages %+v", name, req.Messages)
		}
		w := want[0]
		if name == "redacted_thinking" {
			w = want[1]
		}
		if got := req.Messages[0].Content[0]; !reflect.DeepEqual(got, w) {
			t.Errorf("%s: block %+v, want %+v", name, got, w)
		}
	}
}

// TestEncode_StreamEndsEarly checks decision 11: a stream with no stop
// event ends with response.failed, never response.completed.
func TestEncode_StreamEndsEarly(t *testing.T) {
	out := string(encode(pivot.Request{}, true, events(start(), open(0, pivot.BlockText), delta(0, pivot.DeltaText, "Hal"))))
	if !strings.Contains(out, "event: response.failed\n") ||
		!strings.Contains(out, `"error":{"code":"api_error","message":"the inference server's stream ended before the reply was complete"}`) {
		t.Errorf("no response.failed:\n%s", out)
	}
	if strings.Contains(out, "response.completed") {
		t.Errorf("an incomplete stream looks complete:\n%s", out)
	}
}

// TestEncode_UnknownToolName checks that a tool name that is not in the
// request's map gives a function_call with that name and no namespace.
func TestEncode_UnknownToolName(t *testing.T) {
	evs := events(start(), openTool(0, "call_9", "made_up"), delta(0, pivot.DeltaInputJSON, `{"a":1}`), closeBlock(0), stop("tool_use"))
	items := doneItems(t, encode(codexReq(t), true, evs))
	want := `{"type":"function_call","id":"fc_0","call_id":"call_9","name":"made_up","arguments":"{\"a\":1}"}`
	if len(items) != 1 || string(items[0]) != want {
		t.Errorf("items %s, want %s", items, want)
	}
}
