package accounting

import (
	"net/http"
	"testing"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/pipeline"
)

func TestThreadOf(t *testing.T) {
	cases := []struct {
		name string
		h    http.Header
		want accountant.Thread
	}{
		{"primary agent, no headers", nil, accountant.Thread{ID: "main"}},
		{
			"subagent of the main conversation",
			http.Header{pipeline.ClaudeCodeAgentIDHeader: []string{"a1"}},
			accountant.Thread{ID: "a1", Parent: "main"},
		},
		{
			"subagent with a recorded parent",
			http.Header{
				pipeline.ClaudeCodeAgentIDHeader:       []string{"a1"},
				pipeline.ClaudeCodeParentAgentIDHeader: []string{"a0"},
			},
			accountant.Thread{ID: "a1", Parent: "a0"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pctx := &pipeline.Context{Headers: c.h}
			if got := ThreadOf(pctx); got != c.want {
				t.Errorf("ThreadOf() = %+v, want %+v", got, c.want)
			}
		})
	}
}
