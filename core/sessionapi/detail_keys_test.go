package sessionapi

import (
	"slices"
	"testing"
)

// The detail view hides a response event's request fields, and a request event's response
// fields. The search route reads the same lists, so these pins are for both.
func TestDetailKeys(t *testing.T) {
	for _, tc := range []struct {
		ext      string
		response bool
		has      []string
		hasNot   []string
	}{
		{"inference", false, []string{"messages", "tools", "toolResults", "historyToolCalls"}, []string{"completion", "toolCalls"}},
		{"inference", true, []string{"completion", "toolCalls", "reasoningTokens"}, []string{"messages", "tools"}},
		{"mcp", false, []string{"params"}, []string{"result"}},
		{"mcp", true, []string{"result", "error"}, []string{"params"}},
		{"a2a", false, []string{"parts"}, []string{"artifact"}},
		{"a2a", true, []string{"artifact"}, []string{"parts"}},
	} {
		keys := DetailKeys(tc.ext, tc.response)
		for _, k := range tc.has {
			if !slices.Contains(keys, k) || !detailShows(tc.ext, tc.response, k) {
				t.Errorf("%s response=%v: %q missing", tc.ext, tc.response, k)
			}
		}
		for _, k := range tc.hasNot {
			if slices.Contains(keys, k) || detailShows(tc.ext, tc.response, k) {
				t.Errorf("%s response=%v: %q must not be shown", tc.ext, tc.response, k)
			}
		}
	}
	if DetailKeys("other", false) != nil {
		t.Error("an unknown extension has no list")
	}
	DetailKeys("mcp", false)[0] = "changed"
	if DetailKeys("mcp", false)[0] == "changed" {
		t.Error("DetailKeys must return a copy")
	}
}
