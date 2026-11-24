package contextguru

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/toolprune"
)

// TestToolfilterRemovesWhatToolPruneRemoves pins the #2120 move of the
// tool-prune list into toolfilter.remove: for the same list and body, both
// plugins must keep the same tools. The bodies are small, so no model-backed
// step fires and no network call occurs.
func TestToolfilterRemovesWhatToolPruneRemoves(t *testing.T) {
	remove := []string{"Bash", "mcp__gh__issue"}
	all := []string{"Bash", "Read", "mcp__gh__issue", "mcp__gh__pr"}
	want := []string{"Read", "mcp__gh__pr"}

	anthropicTools := make([]map[string]any, 0, len(all))
	chatTools := make([]map[string]any, 0, len(all))
	for _, n := range all {
		anthropicTools = append(anthropicTools, map[string]any{
			"name": n, "description": "tool " + n, "input_schema": map[string]any{"type": "object"},
		})
		chatTools = append(chatTools, map[string]any{"type": "function", "function": map[string]any{
			"name": n, "description": "tool " + n, "parameters": map[string]any{"type": "object"},
		}})
	}
	cases := []struct {
		name, path string
		body       map[string]any
	}{
		{"anthropic messages", "/v1/messages", map[string]any{
			"model": "claude-opus-5", "max_tokens": 1024, "tools": anthropicTools,
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		}},
		{"openai chat", "/v1/chat/completions", map[string]any{
			"model": "gpt-4o-mini", "tools": chatTools,
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		}},
	}

	cfgRaw, err := json.Marshal(map[string]any{"engine": map[string]any{
		"preset": "xhigh", "components": map[string]any{"toolfilter": map[string]any{"remove": remove}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	pruneRaw, err := json.Marshal(map[string]any{"remove": remove})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(tc.body)
			if err != nil {
				t.Fatal(err)
			}

			tp := toolprune.New()
			if err := tp.Configure(pruneRaw); err != nil {
				t.Fatalf("tool-prune configure: %v", err)
			}
			// tool-prune reads the tool names from inference-parser's extension.
			tpCtx := &pipeline.Context{Path: tc.path, Body: slices.Clone(body)}
			tools := make([]pipeline.InferenceTool, 0, len(all))
			for _, n := range all {
				tools = append(tools, pipeline.InferenceTool{Name: n})
			}
			tpCtx.Extensions.Inference = &pipeline.InferenceExtension{Tools: tools}
			tpPipe, err := pipeline.New([]pipeline.Plugin{tp})
			if err != nil {
				t.Fatalf("pipeline.New: %v", err)
			}
			if act := tpPipe.Run(t.Context(), tpCtx); act.Type != pipeline.Continue {
				t.Fatalf("tool-prune action = %v, want Continue", act.Type)
			}

			cg := configured(t, string(cfgRaw))
			cgCtx := inferencePctx(tc.path, slices.Clone(body))
			if act := invokeReq(cg, cgCtx); act.Type != pipeline.Continue {
				t.Fatalf("context-guru action = %v, want Continue", act.Type)
			}

			gotTP, gotCG := toolNames(t, tpCtx.Body), toolNames(t, cgCtx.Body)
			if !slices.Equal(gotTP, want) {
				t.Errorf("tool-prune kept %v, want %v", gotTP, want)
			}
			if !slices.Equal(gotCG, gotTP) {
				t.Errorf("context-guru kept %v, tool-prune kept %v: the lists must match", gotCG, gotTP)
			}
		})
	}
}

// toolNames reads the tool names of an Anthropic or OpenAI Chat body, in order.
func toolNames(t *testing.T, body []byte) []string {
	t.Helper()
	var doc struct {
		Tools []struct {
			Name     string `json:"name"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	names := make([]string, 0, len(doc.Tools))
	for _, tl := range doc.Tools {
		if tl.Name != "" {
			names = append(names, tl.Name)
		} else {
			names = append(names, tl.Function.Name)
		}
	}
	return names
}
