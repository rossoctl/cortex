package inferencerouter

import (
	"net/http"
	"strings"
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

// TestDecideSettle pins decideSettle over each outcome of the redirect, crossed with
// each pin state. Only a pinMoved pin under observe gets another answer.
func TestDecideSettle(t *testing.T) {
	const server = "glm"
	forgo, stay, routed := [2]any{"", false}, [2]any{"", true}, [2]any{server, true}
	tests := []struct {
		name                                   string
		redirectFailed, redirected, pathFailed bool
		want, wantMoved                        [2]any // (to, settle); wantMoved for pinMoved
	}{
		{name: "observed", want: stay, wantMoved: forgo},
		{name: "observed, path not tried", pathFailed: true, want: stay, wantMoved: forgo},
		{name: "routed", redirected: true, want: routed, wantMoved: routed},
		{name: "path refused", redirected: true, pathFailed: true, want: forgo, wantMoved: forgo},
		{name: "redirect failed", redirectFailed: true, want: forgo, wantMoved: forgo},
		{name: "redirect failed, path not tried", redirectFailed: true, pathFailed: true, want: forgo, wantMoved: forgo},
		{name: "redirect failed, redirected", redirectFailed: true, redirected: true, want: forgo, wantMoved: forgo},
		{name: "redirect failed, redirected, path refused", redirectFailed: true, redirected: true, pathFailed: true, want: forgo, wantMoved: forgo},
	}
	for _, tt := range tests {
		for _, state := range []string{pinNew, pinMoved, pinExisting, pinNone} {
			t.Run(tt.name+"/"+state, func(t *testing.T) {
				want := tt.want
				if state == pinMoved {
					want = tt.wantMoved
				}
				to, settle := decideSettle(state, server, tt.redirectFailed, tt.redirected, tt.pathFailed)
				if got := [2]any{to, settle}; got != want {
					t.Errorf("decideSettle = %v, want %v", got, want)
				}
			})
		}
	}
}

// TestChooseModelFor pins each rule of chooseModelFor, in its order, and that refused
// and substitute are called only when an earlier rule does not decide.
func TestChooseModelFor(t *testing.T) {
	const requested, kept, sub = "exo-free", "glm-4-6", "glm-5-3"
	tests := []struct {
		name    string
		kept    string
		refused bool
		sub     string // what substitute returns

		model                  string
		keep                   bool
		askedRefused, askedSub bool
	}{
		{name: "the session's substitute", kept: kept, refused: true, sub: sub, model: kept},
		{name: "the session's substitute is the name", kept: requested, model: requested},
		{name: "not refused", model: requested, askedRefused: true},
		{name: "refused, no substitute", refused: true, model: requested, askedRefused: true, askedSub: true},
		{name: "refused, the substitute is the name", refused: true, sub: requested, model: requested, keep: true, askedRefused: true, askedSub: true},
		{name: "refused, a substitute", refused: true, sub: sub, model: sub, keep: true, askedRefused: true, askedSub: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			askedRefused, askedSub := false, false
			model, keep := chooseModelFor(requested, tt.kept,
				func() bool { askedRefused = true; return tt.refused },
				func() string { askedSub = true; return tt.sub })
			if model != tt.model || keep != tt.keep {
				t.Errorf("chooseModelFor = (%q, %v), want (%q, %v)", model, keep, tt.model, tt.keep)
			}
			if askedRefused != tt.askedRefused || askedSub != tt.askedSub {
				t.Errorf("refused called = %v, substitute called = %v, want %v, %v",
					askedRefused, askedSub, tt.askedRefused, tt.askedSub)
			}
		})
	}
}

// TestDecideResend pins decideResend over each fact that changes its answer: what the
// server refused, the request's state, and each attempt already made. substitute must
// be called only for a model refusal that can be resent.
func TestDecideResend(t *testing.T) {
	const model, sub = "exo-free", "glm-5-3"
	small := []byte(`{"model":"exo-free","max_completion_tokens":256000,"messages":[{"role":"user","content":"hi"}]}`)
	large := []byte(`{"model":"exo-free","max_completion_tokens":1000,"messages":[{"role":"user","content":"` + strings.Repeat(pad, 20) + `"}]}`)
	ready := resendFacts{body: small, model: model, hasModel: true, routed: true, parsed: true, redirected: true, switched: true}
	notFound := `{"error":{"message":"model 'exo-free' not found","type":"not_found_error"}}`
	// A refusal of the model whose text also reads as a limit.
	both := `{"error":{"message":"Invalid model name passed in model=exo-free; maximum context length is 2048 tokens"}}`
	tooHigh := `{"error":{"message":"The maximum tokens you requested exceeds the model limit of 300000."}}`
	none, noSub := resendStep{}, resendStep{reason: "model"}
	toSub := resendStep{reason: "model", sub: sub}
	capped := resendStep{reason: "limit", kind: limitOutput, n: 128000, stated: true}
	tests := []struct {
		name    string
		status  int
		refusal string
		edit    func(*resendFacts)
		sub     string // what substitute returns

		want  resendStep
		asked bool // substitute was called
	}{
		{name: "M7 model refused", status: http.StatusForbidden, refusal: refusal, sub: sub, want: toSub, asked: true},
		{name: "M7 invalid model", status: http.StatusBadRequest, refusal: both, sub: sub, want: toSub, asked: true},
		{name: "M7 local server's missing model", status: http.StatusNotFound, refusal: notFound, sub: sub, want: toSub, asked: true},
		{name: "M6 no substitute", status: http.StatusForbidden, refusal: refusal, want: noSub, asked: true},
		{name: "M6 the substitute is the model", status: http.StatusForbidden, refusal: refusal, sub: model, want: noSub, asked: true},
		{name: "M1 not redirected", status: http.StatusForbidden, refusal: refusal, sub: sub, edit: func(f *resendFacts) { f.redirected = false }, want: none},
		{name: "M2 not a server's host", status: http.StatusForbidden, refusal: refusal, sub: sub, edit: func(f *resendFacts) { f.routed = false }, want: none},
		{name: "M3 not parsed", status: http.StatusForbidden, refusal: refusal, sub: sub, edit: func(f *resendFacts) { f.parsed = false }, want: none},
		{name: "M4 sent with a substitute", status: http.StatusForbidden, refusal: refusal, sub: sub, edit: func(f *resendFacts) { f.substituted = true }, want: none},
		{name: "M4 a refused substitute is no limit", status: http.StatusBadRequest, refusal: both, sub: sub, edit: func(f *resendFacts) { f.substituted = true; f.body = large }, want: none},
		{name: "M5 no model", status: http.StatusForbidden, refusal: refusal, sub: sub, edit: func(f *resendFacts) { f.model, f.hasModel = "", false }, want: none},
		{name: "another name not found", status: http.StatusNotFound, refusal: notFound, sub: sub, edit: func(f *resendFacts) { f.model = "other" }, want: none},
		{name: "a bad key", status: http.StatusUnauthorized, refusal: `{"error":{"type":"token_not_found_in_db"}}`, sub: sub, want: none},
		{name: "another 403", status: http.StatusForbidden, refusal: `{"error":{"type":"budget_exceeded"}}`, sub: sub, want: none},
		{name: "a server error", status: http.StatusInternalServerError, refusal: refusal, sub: sub, want: none},
		{name: "L8 output cap stated", status: http.StatusBadRequest, refusal: bedrockCap, want: capped},
		{name: "L9 window not stated", status: http.StatusBadRequest, refusal: bedrockTooLong, edit: func(f *resendFacts) { f.body = large },
			want: resendStep{reason: "limit", kind: limitWindow, n: estimate(large) * 9 / 10}},
		{name: "L8 a model resend first", status: http.StatusBadRequest, refusal: bedrockCap, edit: func(f *resendFacts) { f.substituted = true }, want: capped},
		{name: "L1 a limit at another status", status: http.StatusForbidden, refusal: bedrockCap, want: none},
		{name: "L2 not switched", status: http.StatusBadRequest, refusal: bedrockCap, edit: func(f *resendFacts) { f.switched = false }, want: none},
		{name: "L3 resent for a limit already", status: http.StatusBadRequest, refusal: bedrockCap, edit: func(f *resendFacts) { f.limitResent = true }, want: none},
		{name: "L4 not a server's host", status: http.StatusBadRequest, refusal: bedrockCap, edit: func(f *resendFacts) { f.routed = false }, want: none},
		{name: "L5 no model", status: http.StatusBadRequest, refusal: bedrockCap, edit: func(f *resendFacts) { f.model, f.hasModel = "", false }, want: none},
		{name: "L6 implausible stated cap", status: http.StatusBadRequest, refusal: tooHigh, want: none},
		{name: "L7 implausible window not stated", status: http.StatusBadRequest, refusal: bedrockTooLong, want: none},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := ready
			if tt.edit != nil {
				tt.edit(&f)
			}
			f.status, f.refusal = tt.status, []byte(tt.refusal)
			asked := false
			got := decideResend(f, func() string { asked = true; return tt.sub })
			if got != tt.want {
				t.Errorf("decideResend = %+v, want %+v", got, tt.want)
			}
			if asked != tt.asked {
				t.Errorf("substitute called = %v, want %v", asked, tt.asked)
			}
		})
	}
}
