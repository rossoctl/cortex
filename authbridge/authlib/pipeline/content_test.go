package pipeline

import (
	"reflect"
	"sort"
	"testing"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/contracts"
)

func TestA2AExtension_Fragments(t *testing.T) {
	tests := []struct {
		name string
		ext  *A2AExtension
		want []contracts.Fragment
	}{
		{
			name: "nil_receiver",
			ext:  nil,
			want: nil,
		},
		{
			name: "user_text_part",
			ext: &A2AExtension{
				Role: "user",
				Parts: []A2APart{
					{Kind: "text", Content: "What's the weather in SF?"},
				},
			},
			want: []contracts.Fragment{
				{Role: contracts.RoleUser, Text: "What's the weather in SF?"},
			},
		},
		{
			name: "agent_role_normalized_to_assistant",
			ext: &A2AExtension{
				Role: "agent",
				Parts: []A2APart{
					{Kind: "text", Content: "The weather is sunny."},
				},
			},
			want: []contracts.Fragment{
				{Role: contracts.RoleAssistant, Text: "The weather is sunny."},
			},
		},
		{
			name: "data_part_emitted",
			ext: &A2AExtension{
				Role: "user",
				Parts: []A2APart{
					{Kind: "data", Content: `{"lat":37.7}`},
				},
			},
			want: []contracts.Fragment{
				{Role: contracts.RoleUser, Text: `{"lat":37.7}`},
			},
		},
		{
			name: "file_part_skipped",
			ext: &A2AExtension{
				Role: "user",
				Parts: []A2APart{
					{Kind: "file", Content: "https://example.com/doc.pdf"},
					{Kind: "text", Content: "Summarize this"},
				},
			},
			want: []contracts.Fragment{
				{Role: contracts.RoleUser, Text: "Summarize this"},
			},
		},
		{
			name: "empty_text_content_filtered",
			ext: &A2AExtension{
				Role: "user",
				Parts: []A2APart{
					{Kind: "text", Content: ""},
					{Kind: "text", Content: "real content"},
				},
			},
			want: []contracts.Fragment{
				{Role: contracts.RoleUser, Text: "real content"},
			},
		},
		{
			name: "artifact_emitted_as_assistant",
			ext: &A2AExtension{
				Role:     "user",
				Parts:    []A2APart{{Kind: "text", Content: "Tell me a joke"}},
				Artifact: "Why did the chicken cross the road?",
			},
			want: []contracts.Fragment{
				{Role: contracts.RoleUser, Text: "Tell me a joke"},
				{Role: contracts.RoleAssistant, Text: "Why did the chicken cross the road?"},
			},
		},
		{
			name: "unknown_role_passes_through",
			ext: &A2AExtension{
				Role: "custom-role",
				Parts: []A2APart{
					{Kind: "text", Content: "hi"},
				},
			},
			want: []contracts.Fragment{
				{Role: "custom-role", Text: "hi"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.ext.Fragments()
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestMCPExtension_Fragments(t *testing.T) {
	tests := []struct {
		name string
		ext  *MCPExtension
		want []contracts.Fragment
	}{
		{
			name: "nil_receiver",
			ext:  nil,
			want: nil,
		},
		{
			name: "tools_list_no_content",
			ext:  &MCPExtension{Method: "tools/list"},
			want: nil,
		},
		{
			name: "tools_call_with_string_arg",
			ext: &MCPExtension{
				Method: "tools/call",
				Params: map[string]any{
					"name":      "fetch_url",
					"arguments": map[string]any{"url": "https://example.com"},
				},
			},
			want: []contracts.Fragment{
				{Role: contracts.RoleTool, Text: "fetch_url"},
				{Role: contracts.RoleToolArgs, Text: "https://example.com"},
			},
		},
		{
			name: "tools_call_non_string_arg_json_stringified",
			ext: &MCPExtension{
				Method: "tools/call",
				Params: map[string]any{
					"name":      "set_preferences",
					"arguments": map[string]any{"prefs": map[string]any{"theme": "dark"}},
				},
			},
			want: []contracts.Fragment{
				{Role: contracts.RoleTool, Text: "set_preferences"},
				{Role: contracts.RoleToolArgs, Text: `{"theme":"dark"}`},
			},
		},
		{
			name: "tools_call_missing_name",
			ext: &MCPExtension{
				Method: "tools/call",
				Params: map[string]any{
					"arguments": map[string]any{"q": "hello"},
				},
			},
			want: []contracts.Fragment{
				{Role: contracts.RoleToolArgs, Text: "hello"},
			},
		},
		{
			name: "tools_call_nil_params",
			ext:  &MCPExtension{Method: "tools/call"},
			want: nil,
		},
		{
			name: "response_text_item",
			ext: &MCPExtension{
				Method: "tools/call",
				Result: map[string]any{
					"content": []any{
						map[string]any{"type": "text", "text": "Result line"},
					},
				},
			},
			want: []contracts.Fragment{
				{Role: contracts.RoleToolResult, Text: "Result line"},
			},
		},
		{
			name: "response_non_text_item_skipped",
			ext: &MCPExtension{
				Method: "tools/call",
				Result: map[string]any{
					"content": []any{
						map[string]any{"type": "image", "data": "base64..."},
						map[string]any{"type": "text", "text": "caption"},
					},
				},
			},
			want: []contracts.Fragment{
				{Role: contracts.RoleToolResult, Text: "caption"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.ext.Fragments()
			// Map iteration over Params["arguments"] is non-deterministic;
			// sort both sides by (Role, Text) before comparing so multi-arg
			// cases compare stably. Single-arg cases already line up.
			sortFragments(got)
			sortFragments(tc.want)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestInferenceExtension_Fragments(t *testing.T) {
	tests := []struct {
		name string
		ext  *InferenceExtension
		want []contracts.Fragment
	}{
		{
			name: "nil_receiver",
			ext:  nil,
			want: nil,
		},
		{
			name: "system_and_user_messages",
			ext: &InferenceExtension{
				Messages: []InferenceMessage{
					{Role: "system", Content: "You are helpful."},
					{Role: "user", Content: "Hi"},
				},
			},
			want: []contracts.Fragment{
				{Role: contracts.RoleSystem, Text: "You are helpful."},
				{Role: contracts.RoleUser, Text: "Hi"},
			},
		},
		{
			name: "tool_role_remapped_to_tool_result",
			ext: &InferenceExtension{
				Messages: []InferenceMessage{
					{Role: "user", Content: "weather?"},
					{Role: "assistant", Content: "let me check"},
					{Role: "tool", Content: "18°C sunny"},
				},
			},
			want: []contracts.Fragment{
				{Role: contracts.RoleUser, Text: "weather?"},
				{Role: contracts.RoleAssistant, Text: "let me check"},
				{Role: contracts.RoleToolResult, Text: "18°C sunny"},
			},
		},
		{
			name: "empty_content_filtered",
			ext: &InferenceExtension{
				Messages: []InferenceMessage{
					{Role: "user", Content: ""},
					{Role: "user", Content: "real"},
				},
			},
			want: []contracts.Fragment{
				{Role: contracts.RoleUser, Text: "real"},
			},
		},
		{
			name: "completion_emitted_as_assistant",
			ext: &InferenceExtension{
				Completion: "The answer is 42.",
			},
			want: []contracts.Fragment{
				{Role: contracts.RoleAssistant, Text: "The answer is 42."},
			},
		},
		{
			name: "tool_calls_emit_name_and_args",
			ext: &InferenceExtension{
				ToolCalls: []InferenceToolCall{
					{Name: "get_weather", Arguments: `{"city":"SF"}`},
				},
			},
			want: []contracts.Fragment{
				{Role: contracts.RoleTool, Text: "get_weather"},
				{Role: contracts.RoleToolArgs, Text: `{"city":"SF"}`},
			},
		},
		{
			name: "tool_call_empty_name_or_args_skipped",
			ext: &InferenceExtension{
				ToolCalls: []InferenceToolCall{
					{Name: "", Arguments: `{}`},
					{Name: "call_me", Arguments: ""},
				},
			},
			want: []contracts.Fragment{
				{Role: contracts.RoleToolArgs, Text: "{}"},
				{Role: contracts.RoleTool, Text: "call_me"},
			},
		},
		{
			name: "empty_extension_returns_nil",
			ext:  &InferenceExtension{},
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.ext.Fragments()
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestContentSources(t *testing.T) {
	// Empty context → empty slice
	c := &Context{}
	if got := c.ContentSources(); len(got) != 0 {
		t.Errorf("empty context: want 0 sources, got %d", len(got))
	}

	// Populated with all three
	c = &Context{
		Extensions: Extensions{
			A2A:       &A2AExtension{Role: "user", Parts: []A2APart{{Kind: "text", Content: "hi"}}},
			MCP:       &MCPExtension{Method: "tools/list"},
			Inference: &InferenceExtension{Messages: []InferenceMessage{{Role: "user", Content: "x"}}},
		},
	}
	srcs := c.ContentSources()
	if len(srcs) != 3 {
		t.Fatalf("want 3 sources, got %d", len(srcs))
	}

	// Sanity: iterating and collecting user fragments works uniformly
	var userTexts []string
	for _, s := range srcs {
		for _, f := range s.Fragments() {
			if f.Role == contracts.RoleUser {
				userTexts = append(userTexts, f.Text)
			}
		}
	}
	sort.Strings(userTexts)
	want := []string{"hi", "x"}
	if !reflect.DeepEqual(userTexts, want) {
		t.Errorf("user texts: got %v, want %v", userTexts, want)
	}
}

// sortFragments sorts by (Role, Text) so map-order non-determinism in
// MCP's arguments iteration doesn't flake the test comparisons.
func sortFragments(f []contracts.Fragment) {
	sort.Slice(f, func(i, j int) bool {
		if f[i].Role != f[j].Role {
			return f[i].Role < f[j].Role
		}
		return f[i].Text < f[j].Text
	})
}
