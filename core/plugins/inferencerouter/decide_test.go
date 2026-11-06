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

// TestDecideServer pins decideServer over each input that changes its answer: a
// session to pin, an override, a pin and whose it is, a pin to a removed server, and
// the unpinned answer. unpinned must be called only when no override applies and the
// agent holds no pin.
func TestDecideServer(t *testing.T) {
	const agent, choice = "claude-code", "ete"
	own := func(server string) *pin { return &pin{agent: agent, server: server} }
	other := &pin{agent: "opencode", server: "glm"}
	tests := []struct {
		name       string
		noSession  bool
		over       *string // nil for no sessions: entry
		had        *pin    // nil for no pin
		u          string  // the unpinned answer
		uTurn      string
		uDecides   bool
		server     string
		state      string
		from, turn string
		override   bool
		keep       bool
		asked      bool // unpinned was called
	}{
		{name: "no session ignores override and pin", noSession: true, over: new("glm"), had: own("bob"), server: choice, state: pinNone},
		{name: "override, no pin", over: new("glm"), server: "glm", state: pinNew, override: true},
		{name: "override, own pin to it", over: new("glm"), had: own("glm"), server: "glm", state: pinExisting, override: true, keep: true},
		{name: "override, own pin elsewhere", over: new("glm"), had: own("bob"), server: "glm", state: pinMoved, from: "bob", override: true},
		{name: "override, own pin unrouted", over: new("glm"), had: own(""), server: "glm", state: pinMoved, override: true},
		{name: "override, another agent's pin", over: new("bob"), had: other, server: "bob", state: pinNew, override: true},
		{name: "own override, own pin elsewhere", over: new(""), had: own("bob"), server: "", state: pinMoved, from: "bob", override: true},
		{name: "own override, own pin unrouted", over: new(""), had: own(""), server: "", state: pinExisting, override: true, keep: true},
		{name: "own pin", had: own("bob"), u: "glm", uDecides: true, server: "bob", state: pinExisting, keep: true},
		{name: "own pin to a removed server", had: own("gone"), u: "glm", uDecides: true, server: "gone", state: pinExisting, keep: true},
		{name: "own pin unrouted", had: own(""), u: "glm", uDecides: true, server: "", state: pinExisting, keep: true},
		{name: "another agent's pin", had: other, u: choice, uTurn: turnAside, server: choice, state: pinNone, asked: true},
		{name: "no pin, decides", u: choice, uDecides: true, server: choice, state: pinNew, asked: true},
		{name: "no pin, continuation", u: "", uTurn: turnContinuation, uDecides: true, server: "", state: pinNew, turn: turnContinuation, asked: true},
		{name: "no pin, decides nothing", u: choice, uTurn: turnAside, server: choice, state: pinNone, turn: turnAside, asked: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			asked := false
			unpinned := func() (string, string, bool) {
				asked = true
				return tt.u, tt.uTurn, tt.uDecides
			}
			over, overridden := "", tt.over != nil
			if overridden {
				over = *tt.over
			}
			had, pinned := pin{}, tt.had != nil
			if pinned {
				had = *tt.had
			}
			server, pn, keep := decideServer(agent, choice, !tt.noSession, over, overridden, had, pinned, unpinned)
			if server != tt.server || pn.state != tt.state || pn.from != tt.from || pn.turn != tt.turn ||
				pn.override != tt.override || keep != tt.keep {
				t.Errorf("decideServer = (%q, state %q from %q turn %q override %v, keep %v), want (%q, state %q from %q turn %q override %v, keep %v)",
					server, pn.state, pn.from, pn.turn, pn.override, keep,
					tt.server, tt.state, tt.from, tt.turn, tt.override, tt.keep)
			}
			if asked != tt.asked {
				t.Errorf("unpinned called = %v, want %v", asked, tt.asked)
			}
		})
	}
}
