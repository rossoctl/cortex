package pipeline

import "testing"

func TestWithinCeiling(t *testing.T) {
	ceiling := PluginCapabilities{
		ReadsBody:          true,
		WritesRequestBody:  true,
		WritesResponseBody: true,
		Requires:           []string{"inference-parser"},
		RequiresAny:        []string{"mcp-parser", "a2a-parser"},
		Description:        "the ceiling",
	}
	cases := []struct {
		name string
		caps PluginCapabilities
		want string
	}{
		{"the ceiling itself", ceiling, ""},
		{"nothing at all", PluginCapabilities{}, ""},
		{"one flag dropped", PluginCapabilities{ReadsBody: true, WritesRequestBody: true}, ""},
		// WritesResponseBody implies ReadsBody, so a narrowed set that keeps only the writer
		// is still inside a ceiling that lists both.
		{"implied flag", PluginCapabilities{WritesResponseBody: true}, ""},
		{"writers dropped, reading kept", PluginCapabilities{ReadsBody: true}, "ReadsBody without a body writer"},
		{"a requirement dropped", PluginCapabilities{RequiresAny: ceiling.RequiresAny}, ""},
		{"description differs", PluginCapabilities{Description: "configured"}, ""},
		{"flag added", PluginCapabilities{WritesDestination: true}, "WritesDestination"},
		{"requirement added", PluginCapabilities{Requires: []string{"mcp-parser"}}, `Requires "mcp-parser"`},
		{"later requirement added", PluginCapabilities{RequiresLater: []string{"x"}}, `RequiresLater "x"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.caps.WithinCeiling(ceiling); got != tc.want {
				t.Errorf("WithinCeiling = %q, want %q", got, tc.want)
			}
		})
	}
}

// RequiresAny is met by any one of its names, and each one present must also come
// earlier, so dropping a name and adding one each can refuse a chain the ceiling
// accepts: only the same set, or none, is within it.
func TestWithinCeiling_RequiresAny(t *testing.T) {
	ceiling := PluginCapabilities{RequiresAny: []string{"mcp-parser", "a2a-parser"}}
	cases := []struct {
		name string
		got  []string
		want string
	}{
		{"same names", []string{"mcp-parser", "a2a-parser"}, ""},
		{"same names reordered", []string{"a2a-parser", "mcp-parser"}, ""},
		{"same names repeated", []string{"a2a-parser", "mcp-parser", "a2a-parser"}, ""},
		{"none", nil, ""},
		{"one name dropped", []string{"a2a-parser"}, `RequiresAny without "mcp-parser"`},
		{"alternative added", []string{"mcp-parser", "a2a-parser", "inference-parser"}, `RequiresAny "inference-parser"`},
		{"names replaced", []string{"inference-parser"}, `RequiresAny "inference-parser"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (PluginCapabilities{RequiresAny: tc.got}).WithinCeiling(ceiling); got != tc.want {
				t.Errorf("WithinCeiling = %q, want %q", got, tc.want)
			}
		})
	}
	if got := (PluginCapabilities{RequiresAny: []string{"mcp-parser"}}).WithinCeiling(PluginCapabilities{}); got != `RequiresAny "mcp-parser"` {
		t.Errorf("a RequiresAny the ceiling lacks: WithinCeiling = %q", got)
	}
}

// A ceiling that does not list ReadsBody still admits it when it lists a writer: the
// ceiling is normalized too, so it means what Pipeline.New will treat it as.
func TestWithinCeiling_NormalizesTheCeiling(t *testing.T) {
	ceiling := PluginCapabilities{WritesRequestBody: true}
	if got := (PluginCapabilities{ReadsBody: true, WritesRequestBody: true}).WithinCeiling(ceiling); got != "" {
		t.Errorf("ReadsBody refused under a WritesRequestBody ceiling: %q", got)
	}
}
