package inferenceparser

import (
	"context"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// THE BODIES BELOW ARE THE LIVE SHAPES, headers included verbatim from three measured sessions.
// Both dialects go through OnRequest rather than through agentRole directly, because the wiring is
// half of what is being asserted: the field is set in one place for two parsers, and a test that
// called the helper would still pass if that call were deleted.
func TestInferenceParser_AgentRole(t *testing.T) {
	const mainHeader = "x-anthropic-billing-header: cc_version=2.1.270.119; cc_entrypoint=cli;"
	const subHeader = "x-anthropic-billing-header: cc_version=2.1.270.658; cc_entrypoint=cli; cc_is_subagent=true;"

	for _, tc := range []struct {
		name string
		path string
		body string
		want pipeline.AgentRole
	}{
		{
			name: "anthropic, system as a string, no marker",
			path: "/v1/messages",
			body: `{"model":"claude-opus-5","system":"` + mainHeader + `\nYou are Claude Code.",
				"messages":[{"role":"user","content":"hi"}]}`,
			want: pipeline.AgentRoleMain,
		},
		{
			// Claude Code's real shape: `system` is a text-block array, and the header is the
			// first line of the first block.
			name: "anthropic, system as a text-block array, marker",
			path: "/v1/messages?beta=true",
			body: `{"model":"claude-opus-5","system":[{"type":"text","text":"` + subHeader +
				`\nYou are a Claude agent, built on Anthropic's Claude Agent SDK."}],
				"messages":[{"role":"user","content":"search the repo"}]}`,
			want: pipeline.AgentRoleSubagent,
		},
		{
			name: "anthropic, a system prompt with no billing header at all",
			path: "/v1/messages",
			body: `{"model":"claude-opus-5","system":"You are a helpful assistant.",
				"messages":[{"role":"user","content":"hi"}]}`,
			want: "",
		},
		{
			name: "anthropic, no system prompt",
			path: "/v1/messages",
			body: `{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`,
			want: "",
		},
		{
			// The first line is the whole of what is read. A marker further down the prompt is
			// text, not a declaration.
			name: "anthropic, marker on a later line of the system prompt",
			path: "/v1/messages",
			body: `{"model":"claude-opus-5","system":"` + mainHeader +
				`\nYou are Claude Code.\ncc_is_subagent=true",
				"messages":[{"role":"user","content":"hi"}]}`,
			want: pipeline.AgentRoleMain,
		},
		{
			// The permission monitor's real failure mode: a one-shot carrying a transcript of an
			// agent that was discussing subagent detection. Its own header says main.
			name: "anthropic, marker quoted inside a user message",
			path: "/v1/messages",
			body: `{"model":"claude-sonnet-5","system":"` + mainHeader + `\nYou are a security monitor.",
				"messages":[{"role":"user","content":"<transcript>cc_is_subagent=true</transcript>"}]}`,
			want: pipeline.AgentRoleMain,
		},
		{
			// No header anywhere, and the marker only in user text: nothing is stated, and the
			// quote must not become a declaration.
			name: "anthropic, marker in a user message and no header",
			path: "/v1/messages",
			body: `{"model":"claude-sonnet-5","system":"You are a security monitor.",
				"messages":[{"role":"user","content":"cc_is_subagent=true"}]}`,
			want: "",
		},
		{
			name: "openai, system first, marker",
			path: "/v1/chat/completions",
			body: `{"model":"gpt-4o","messages":[
				{"role":"system","content":"` + subHeader + `\nYou are a file search specialist."},
				{"role":"user","content":"find it"}]}`,
			want: pipeline.AgentRoleSubagent,
		},
		{
			// An OpenAI-dialect client may put the system message anywhere, which is why the
			// scan looks for the role instead of indexing.
			name: "openai, system not first",
			path: "/v1/chat/completions",
			body: `{"model":"gpt-4o","messages":[
				{"role":"user","content":"find it"},
				{"role":"system","content":"` + mainHeader + `\nYou are Claude Code."}]}`,
			want: pipeline.AgentRoleMain,
		},
		{
			// A pseudo-header's casing is not promised, so neither comparison relies on it.
			name: "anthropic, uppercased header and marker",
			path: "/v1/messages",
			body: `{"model":"claude-opus-5",
				"system":"X-Anthropic-Billing-Header: cc_version=2.1.270.658; CC_IS_SUBAGENT=TRUE;\nYou are an agent.",
				"messages":[{"role":"user","content":"hi"}]}`,
			want: pipeline.AgentRoleSubagent,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewInferenceParser()
			pctx := &pipeline.Context{Path: tc.path, Body: []byte(tc.body)}
			if action := p.OnRequest(context.Background(), pctx); action.Type != pipeline.Continue {
				t.Fatalf("expected Continue, got %v", action.Type)
			}
			ext := pctx.Extensions.Inference
			if ext == nil {
				t.Fatal("Extensions.Inference is nil — the body did not parse")
			}
			if ext.AgentRole != tc.want {
				t.Errorf("AgentRole = %q, want %q", ext.AgentRole, tc.want)
			}
		})
	}
}

// The role is read off the request and has to reach the RESPONSE event, because that is the only
// side that carries token counts and therefore the only side a context reader can use. Nothing in
// the response phase replaces the extension — it is mutated in place — and this is what says so.
func TestInferenceParser_AgentRoleSurvivesTheResponse(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/v1/messages",
		Body: []byte(`{"model":"claude-opus-5",
			"system":"x-anthropic-billing-header: cc_version=2.1.270.658; cc_is_subagent=true;\nAgent.",
			"messages":[{"role":"user","content":"hi"}]}`),
	}
	if action := p.OnRequest(context.Background(), pctx); action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}

	pctx.ResponseBody = []byte(`{"model":"claude-opus-5","stop_reason":"end_turn",
		"content":[{"type":"text","text":"ok"}],
		"usage":{"input_tokens":10,"output_tokens":2}}`)
	if action := p.OnResponse(context.Background(), pctx); action.Type != pipeline.Continue {
		t.Fatalf("expected Continue on the response, got %v", action.Type)
	}

	if got := pctx.Extensions.Inference.AgentRole; got != pipeline.AgentRoleSubagent {
		t.Errorf("AgentRole = %q after the response, want %q — the response phase must not "+
			"replace the request's extension", got, pipeline.AgentRoleSubagent)
	}
	if pctx.Extensions.Inference.OutputTokens == 0 {
		t.Error("the response was not parsed at all, so this test proved nothing")
	}
}
