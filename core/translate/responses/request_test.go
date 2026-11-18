package responses

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/translate/chat"
	"github.com/rossoctl/cortex/core/translate/pivot"
)

func decode(t *testing.T, body string) pivot.Request {
	t.Helper()
	req, err := Protocol{}.DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func encodeChat(t *testing.T, in []byte) string {
	t.Helper()
	req, err := Protocol{}.DecodeRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	body, _, err := chat.Protocol{}.EncodeRequest(req, nil)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// TestDeterministic checks rule 9a of #1911 on the full multi-turn
// request.
func TestDeterministic(t *testing.T) {
	in, err := os.ReadFile("testdata/request/full_multiturn.json")
	if err != nil {
		t.Fatal(err)
	}
	first := encodeChat(t, in)
	if again := encodeChat(t, in); again != first {
		t.Fatalf("two encodings differ:\n%s\n%s", first, again)
	}

	const lastIn = `{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "Thanks. Now run the tests."}]}`
	const lastOut = `{"role":"user","content":[{"type":"text","text":"Thanks. Now run the tests."}]}`
	end := strings.Index(first, lastOut)
	if end < 0 {
		t.Fatalf("the last old message is not in the output:\n%s", first)
	}
	end += len(lastOut)
	longer := strings.Replace(string(in), lastIn, lastIn+
		`,{"type":"message","role":"assistant","content":[{"type":"output_text","text":"All pass."}]}`+
		`,{"type":"message","role":"user","content":[{"type":"input_text","text":"Good."}]}`, 1)
	if longer == string(in) {
		t.Fatal("the fixture does not hold the last user turn")
	}
	grown := encodeChat(t, []byte(longer))
	if !strings.HasPrefix(grown, first[:end]) {
		t.Errorf("one more turn changed the old bytes:\n old %s\n new %s", first[:end], grown)
	}
}

// TestFlatName checks rule R9.
func TestFlatName(t *testing.T) {
	for _, c := range [][3]string{
		{"functions", "shell", "shell"},
		{"", "shell", "shell"},
		{"mcp__github", "create_issue", "mcp__github__create_issue"},
	} {
		if got := flatName(c[0], c[1]); got != c[2] {
			t.Errorf("flatName(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
	valid := regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	long := strings.Repeat("a", 70)
	seen := map[string]string{}
	for _, in := range [][2]string{{"", long}, {"", long + "b"}, {"", "my.tool"}, {"", "my.tool2"}, {"mcp.x", "y"}} {
		got := flatName(in[0], in[1])
		if !valid.MatchString(got) {
			t.Errorf("flatName(%q, %q) = %q is not a valid name", in[0], in[1], got)
		}
		if again := flatName(in[0], in[1]); again != got {
			t.Errorf("flatName(%q, %q) is not stable: %q, %q", in[0], in[1], got, again)
		}
		if prev, ok := seen[got]; ok {
			t.Errorf("%q and %q both give %q", prev, in[1], got)
		}
		seen[got] = in[1]
	}
}

// TestOriginMap checks that Origin maps each flat name to its tool
// (rules R2a, R9).
func TestOriginMap(t *testing.T) {
	long := strings.Repeat("x", 70)
	req := decode(t, `{"model":"m","input":"hi","tools":[
		{"type":"function","name":"shell","parameters":{}},
		{"type":"custom","name":"apply_patch"},
		{"type":"namespace","name":"mcp__github","tools":[{"type":"function","name":"create_issue"},{"type":"function","name":"`+long+`"}]},
		{"type":"web_search"}]}`)
	o, ok := req.Origin.(*origin)
	if !ok {
		t.Fatalf("Origin is %T", req.Origin)
	}
	want := map[string]toolRef{
		"shell":                       {kind: "function", name: "shell"},
		"apply_patch":                 {kind: "custom", name: "apply_patch"},
		"mcp__github__create_issue":   {kind: "function", namespace: "mcp__github", name: "create_issue"},
		flatName("mcp__github", long): {kind: "function", namespace: "mcp__github", name: long},
	}
	if !reflect.DeepEqual(o.tools, want) {
		t.Errorf("got %+v\nwant %+v", o.tools, want)
	}
	var names []string
	for _, tl := range req.Tools {
		names = append(names, tl.Name)
	}
	if want := []string{"shell", "apply_patch", "mcp__github__create_issue", flatName("mcp__github", long)}; !reflect.DeepEqual(names, want) {
		t.Errorf("tool names %v, want %v", names, want)
	}
}

// TestRemoveSourceHeaders checks rule R3.
func TestRemoveSourceHeaders(t *testing.T) {
	h := http.Header{}
	for _, k := range []string{"OpenAI-Beta", "OpenAI-Organization", "OpenAI-Project", "ChatGPT-Account-Id", "Originator", "X-Codex-Turn", "X-OpenAI-Foo", "X-Keep", "Authorization"} {
		h.Set(k, "v")
	}
	h["x-codex-raw"] = []string{"v"} // not in canonical form
	Protocol{}.RemoveSourceHeaders(h)
	var left []string
	for k := range h {
		left = append(left, k)
	}
	if len(left) != 2 || h.Get("X-Keep") == "" || h.Get("Authorization") == "" {
		t.Errorf("headers left: %v", left)
	}
}

// TestDecode_ParallelCallsGroup checks rule R7: calls in a row make one
// assistant message, and their outputs one user message.
func TestDecode_ParallelCallsGroup(t *testing.T) {
	req := decode(t, `{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]},
		{"type":"function_call","call_id":"c1","name":"a","arguments":"{}"},
		{"type":"function_call","call_id":"c2","name":"b","arguments":"{\"x\":1}"},
		{"type":"function_call_output","call_id":"c1","output":"one"},
		{"type":"function_call_output","call_id":"c2","output":"two"},
		{"type":"message","role":"user","content":"and"}]}`)
	var shape []string
	for _, m := range req.Messages {
		s := m.Role + ":"
		for _, b := range m.Content {
			s += " " + string(b.Kind) + "(" + b.ID + b.ToolUseID + b.Text + string(b.Input) + ")"
		}
		shape = append(shape, s)
	}
	want := []string{
		"user: text(go)",
		`assistant: tool_use(c1{}) tool_use(c2{"x":1})`,
		"user: tool_result(c1) tool_result(c2) text(and)",
	}
	if !reflect.DeepEqual(shape, want) {
		t.Errorf("got  %q\nwant %q", shape, want)
	}
}

// TestDecode_StringContent checks decision 8: string content is one text
// block.
func TestDecode_StringContent(t *testing.T) {
	req := decode(t, `{"model":"m","input":[{"role":"user","content":"hi"},{"type":"message","role":"assistant","content":"yo"}]}`)
	want := []pivot.Message{
		{Role: "user", Content: []pivot.Block{{Kind: pivot.BlockText, Text: "hi"}}},
		{Role: "assistant", Content: []pivot.Block{{Kind: pivot.BlockText, Text: "yo"}}},
	}
	if !reflect.DeepEqual(req.Messages, want) {
		t.Errorf("got %+v", req.Messages)
	}
}

// TestDecode_DropsOutputOfDroppedCallOnly checks rule R7: only the output
// of the dropped call goes.
func TestDecode_DropsOutputOfDroppedCallOnly(t *testing.T) {
	req := decode(t, `{"model":"m","input":[
		{"type":"message","role":"user","content":"go"},
		{"type":"function_call","call_id":"keep","name":"a","arguments":"{}"},
		{"type":"local_shell_call","call_id":"drop","action":{"type":"exec","command":["ls"]}},
		{"type":"function_call_output","call_id":"keep","output":"ok"},
		{"type":"function_call_output","call_id":"drop","output":"files"}]}`)
	if n := len(req.Messages); n != 3 {
		t.Fatalf("got %d messages: %+v", n, req.Messages)
	}
	if c := req.Messages[1].Content; len(c) != 1 || c[0].ID != "keep" {
		t.Errorf("assistant: %+v", c)
	}
	if c := req.Messages[2].Content; len(c) != 1 || c[0].ToolUseID != "keep" {
		t.Errorf("user: %+v", c)
	}
	if _, ok := req.Extra["responses.local_shell_call"]; !ok {
		t.Errorf("local_shell_call is not recorded: %v", req.Extra)
	}
}

// TestRefusalsAre400 checks that each refusal is a 400 RequestError.
func TestRefusalsAre400(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","previous_response_id":"resp_1","input":"hi"}`,
		`{"model":"m","conversation":{"id":"conv_1"},"input":"hi"}`,
		`{"model":"m","input":[{"type":"compaction_trigger"}]}`,
		`not json`,
	} {
		_, err := Protocol{}.DecodeRequest([]byte(body))
		var re *pivot.RequestError
		if !errors.As(err, &re) || re.Status != 400 || re.Type != "invalid_request_error" {
			t.Errorf("%s: got %v", body, err)
		}
	}
}

// TestEncodeError checks that the error body has the OpenAI shape that
// the stub gave.
func TestEncodeError(t *testing.T) {
	got := Protocol{}.EncodeError(400, "invalid_request_error", "a <b>")
	want := chat.Protocol{}.EncodeError(400, "invalid_request_error", "a <b>")
	if string(got) != string(want) {
		t.Errorf("got %s, want %s", got, want)
	}
	if !json.Valid(got) {
		t.Errorf("not JSON: %s", got)
	}
}

// TestDecode_BadBodies checks that a body that is not one request object
// is a 400, and never a panic.
func TestDecode_BadBodies(t *testing.T) {
	for _, body := range []string{``, `[]`, `{"input"`, `{"input":`, `{"input":[`, `{"model":"m"} {}`, `{"input":[{"type":"message","role":"user","content":7}]}`, `{"input":[1]}`} {
		_, err := Protocol{}.DecodeRequest([]byte(body))
		var re *pivot.RequestError
		if !errors.As(err, &re) || re.Status != 400 {
			t.Errorf("%q: got %v", body, err)
		}
	}
}
