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
