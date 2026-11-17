package pi

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestRequest_Messages checks the request messages of the entry kinds the fixtures do not hold.
func TestRequest_Messages(t *testing.T) {
	msg := func(id, role, content string) *entry {
		return &entry{Type: "message", ID: id, Message: &message{Role: role, Content: json.RawMessage(content), ToolCallID: "t1"}}
	}
	edit := func(target, replacement string) *entry {
		return &entry{Type: "context_edit", TargetID: target, Replacement: json.RawMessage(replacement)}
	}
	summary := `[{"type":"text","text":"` + `The conversation history before this point was compacted into the following summary:\n\n<summary>\nNEW\n</summary>"}]`
	tests := []struct {
		name string
		ctx  []*entry
		want string // the JSON of the request messages
	}{{
		name: "only the compaction at index 0 gives a summary",
		ctx:  []*entry{{Type: "compaction", Summary: "NEW"}, msg("1", "user", `"u"`), {Type: "compaction", Summary: "OLD"}},
		want: `[{"role":"user","content":` + summary + `},{"role":"user","content":"u"}]`,
	}, {
		name: "branch_summary has no newline before its close tag",
		ctx:  []*entry{{Type: "branch_summary", Summary: "B"}},
		want: `[{"role":"user","content":[{"type":"text","text":"The following is a summary of a branch that this conversation came back from:\n\n<summary>\nB</summary>"}]}]`,
	}, {
		name: "custom_message string content",
		ctx:  []*entry{{Type: "custom_message", Content: json.RawMessage(`"c"`)}},
		want: `[{"role":"user","content":[{"type":"text","text":"c"}]}]`,
	}, {
		name: "image block",
		ctx:  []*entry{msg("1", "user", `[{"type":"image","data":"AAA","mimeType":"image/png"}]`)},
		want: `[{"role":"user","content":[{"source":{"data":"AAA","media_type":"image/png","type":"base64"},"type":"image"}]}]`,
	}, {
		name: "context_edit replacements: user string stays a string, others become one text block",
		ctx: []*entry{
			msg("1", "user", `"u"`), msg("2", "assistant", `[]`), msg("3", "toolResult", `[]`),
			edit("1", `{"content":"U"}`), edit("2", `{"content":"A"}`), edit("3", `{"content":"R"}`),
		},
		want: `[{"role":"user","content":"U"},{"role":"assistant","content":[{"type":"text","text":"A"}]},` +
			`{"role":"user","content":[{"content":[{"type":"text","text":"R"}],"is_error":false,"tool_use_id":"t1","type":"tool_result"}]}]`,
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got struct {
				Messages any `json:"messages"`
			}
			var want any
			if err := json.Unmarshal(marshal(request("m", tc.ctx)), &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Messages, want) {
				t.Errorf("messages =\n%v\nwant\n%v", got.Messages, want)
			}
		})
	}
}

// TestRequest_System checks that the request replays every system message in the context in
// order, as getCurrentSystemMessage (Pi 1.4.0 pi-ai utils/transcript.js) does.
func TestRequest_System(t *testing.T) {
	line := func(s string) *entry {
		var e entry
		if err := json.Unmarshal([]byte(s), &e); err != nil {
			t.Fatal(err)
		}
		return &e
	}
	sys := func(m string) *entry { return line(`{"type":"message","id":"s","message":` + m + `}`) }
	tests := []struct {
		name   string
		ctx    []*entry
		system string
		tools  []string
	}{{
		name:   "prompt in sections, content empty",
		ctx:    []*entry{sys(`{"role":"system","content":"","sections":{"preamble":"P","tools":"T","cwd":"C"},"toolsAdded":[{"name":"read"},{"name":"bash"}]}`)},
		system: "P\n\nT\n\nC", tools: []string{"read", "bash"},
	}, {
		name: "a delta patches one section and removes one tool",
		ctx: []*entry{
			sys(`{"role":"system","content":"","sections":{"preamble":"P","tools":"T"},"toolsAdded":[{"name":"read"},{"name":"bash"}]}`),
			sys(`{"role":"system","content":"","sections":{"skills":"S","preamble":"P2"},"toolsRemoved":[{"name":"bash"}]}`),
		},
		system: "P2\n\nT\n\nS", tools: []string{"read"},
	}, {
		name: "later content is appended and later tools are added",
		ctx: []*entry{
			sys(`{"role":"system","content":"BASE","toolsAdded":[{"name":"read"},{"name":"bash"}]}`),
			sys(`{"role":"system","content":"","toolsAdded":[{"name":"write"}]}`),
			sys(`{"role":"system","content":[{"type":"text","text":"MORE"},{"type":"image"}]}`),
		},
		system: "BASE\n\nMORE", tools: []string{"read", "bash", "write"},
	}, {
		name: "a null section is removed; a changed tool moves to the end",
		ctx: []*entry{
			sys(`{"role":"system","content":"","sections":{"a":"A","b":"B"},"toolsAdded":[{"name":"read"},{"name":"bash"}]}`),
			sys(`{"role":"system","content":"","sections":{"a":null},"toolsRemoved":[{"name":"read"}],"toolsAdded":[{"name":"read"}]}`),
		},
		system: "B", tools: []string{"bash", "read"},
	}, {
		name: "the compaction's system message is the base of a later delta",
		ctx: []*entry{
			line(`{"type":"compaction","summary":"X","systemMessage":{"role":"system","content":"","sections":{"preamble":"P"},"toolsAdded":[{"name":"read"}]}}`),
			sys(`{"role":"system","content":"","sections":{"skills":"S"},"toolsAdded":[{"name":"write"}]}`),
		},
		system: "P\n\nS", tools: []string{"read", "write"},
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got struct {
				System string `json:"system"`
				Tools  []struct {
					Name string `json:"name"`
				} `json:"tools"`
			}
			if err := json.Unmarshal(marshal(request("m", tc.ctx)), &got); err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, tl := range got.Tools {
				names = append(names, tl.Name)
			}
			if got.System != tc.system || !reflect.DeepEqual(names, tc.tools) {
				t.Errorf("system = %q, tools = %v; want %q, %v", got.System, names, tc.system, tc.tools)
			}
		})
	}
}
