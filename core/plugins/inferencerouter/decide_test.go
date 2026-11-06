package inferencerouter

import (
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// TestDecideUnpinned pins each rule of decideUnpinned, in its order, and that
// lastServer is called only for a routed agent.
func TestDecideUnpinned(t *testing.T) {
	tools := []pipeline.InferenceTool{{Name: "Read"}}
	user := []pipeline.InferenceMessage{{Role: "user"}}
	history := []pipeline.InferenceMessage{{Role: "user"}, {Role: "assistant"}, {Role: "user"}}
	opening := &pipeline.InferenceExtension{AgentRole: pipeline.AgentRoleMain, Tools: tools, Messages: user}
	continuation := &pipeline.InferenceExtension{AgentRole: pipeline.AgentRoleMain, Tools: tools, Messages: history}
	oneShot := &pipeline.InferenceExtension{AgentRole: pipeline.AgentRoleMain, Messages: history}
	subagent := &pipeline.InferenceExtension{AgentRole: pipeline.AgentRoleSubagent, Tools: tools, Messages: history}
	noRole := &pipeline.InferenceExtension{Tools: tools, Messages: user}
	noRoleHistory := &pipeline.InferenceExtension{Tools: tools, Messages: history}

	const choice = "ete"
	tests := []struct {
		name     string
		choice   string
		last     string // "" for no earlier request
		ext      *pipeline.InferenceExtension
		captured bool

		server, turn string
		decides      bool
		asked        bool // lastServer was called
	}{
		{name: "not routed, with history", last: "glm", ext: continuation, server: "", decides: true},
		{name: "not routed, captured aside", ext: subagent, captured: true, server: "", decides: true},
		{name: "history beats a continuation", choice: choice, last: "glm", ext: continuation, server: "glm", decides: true, asked: true},
		{name: "history beats an aside", choice: choice, last: "glm", ext: subagent, server: "glm", decides: true, asked: true},
		{name: "history beats no parse", choice: choice, last: "glm", server: "glm", decides: true, asked: true},
		{name: "no parse", choice: choice, server: choice, decides: false, asked: true},
		{name: "no parse, captured", choice: choice, captured: true, server: choice, decides: false, asked: true},
		{name: "continuation", choice: choice, ext: continuation, server: "", turn: turnContinuation, decides: true, asked: true},
		{name: "continuation, captured", choice: choice, ext: continuation, captured: true, server: "", turn: turnContinuation, decides: true, asked: true},
		{name: "one-shot", choice: choice, ext: oneShot, server: choice, turn: turnAside, decides: false, asked: true},
		{name: "one-shot, captured", choice: choice, ext: oneShot, captured: true, server: choice, turn: turnAside, decides: false, asked: true},
		{name: "subagent", choice: choice, ext: subagent, server: "", turn: turnAside, decides: false, asked: true},
		{name: "subagent, captured", choice: choice, ext: subagent, captured: true, server: choice, turn: turnAside, decides: false, asked: true},
		{name: "opening turn", choice: choice, ext: opening, server: choice, decides: true, asked: true},
		{name: "opening turn, captured", choice: choice, ext: opening, captured: true, server: choice, decides: true, asked: true},
		{name: "no role", choice: choice, ext: noRole, server: choice, decides: true, asked: true},
		{name: "no role, with history", choice: choice, ext: noRoleHistory, server: choice, decides: true, asked: true},
		{name: "no role, with history, captured", choice: choice, ext: noRoleHistory, captured: true, server: choice, decides: true, asked: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			asked := false
			lastServer := func() (string, bool) {
				asked = true
				return tt.last, tt.last != ""
			}
			server, turn, decides := decideUnpinned(tt.choice, tt.ext, tt.captured, lastServer)
			if server != tt.server || turn != tt.turn || decides != tt.decides {
				t.Errorf("decideUnpinned = (%q, %q, %v), want (%q, %q, %v)",
					server, turn, decides, tt.server, tt.turn, tt.decides)
			}
			if asked != tt.asked {
				t.Errorf("lastServer called = %v, want %v", asked, tt.asked)
			}
		})
	}
}
