package timeline

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

func assistant(s string) pipeline.InferenceMessage {
	return pipeline.InferenceMessage{Role: "assistant", Content: s}
}

// toolResultMsg is a user message holding only a tool result: its text is empty.
func toolResultMsg() pipeline.InferenceMessage { return pipeline.InferenceMessage{Role: "user"} }

// conv grows a conversation the way Claude Code resends it: each call sends all of it.
type conv []pipeline.InferenceMessage

func (c conv) with(m ...pipeline.InferenceMessage) conv {
	return append(append(conv{}, c...), m...)
}

// wantPrompts checks the prompts' starts (in seconds) and think times (in ms; 0 = none).
func wantPrompts(t *testing.T, tl *Timeline, starts []float64, think []int64) {
	t.Helper()
	if len(tl.Prompts) != len(starts) {
		t.Fatalf("%d prompts, want %d: %+v", len(tl.Prompts), len(starts), tl.Prompts)
	}
	for i, p := range tl.Prompts {
		if p.Index != i+1 || !p.Start.Equal(at(starts[i])) || p.ThinkMs != think[i] || p.Source != "derived" {
			t.Errorf("prompt %d = %+v, want start %vs, thinkMs %d", i+1, p, starts[i], think[i])
		}
	}
}

// A tool loop is one prompt; the next thing the person types starts another, after their think
// time: from the end of the previous prompt's last call.
func TestPromptsToolLoopAndThinkTime(t *testing.T) {
	var r rec
	c := conv{user("fix the failing test")}
	r.turn("", 0, 2, c, nil, "tool_use", pipeline.InferenceToolCall{ID: "t1", Name: "Bash"})
	c = c.with(assistant(""), toolResultMsg())
	r.turn("", 5, 3, c, []pipeline.InferenceToolResult{result("t1", "ok")}, "end_turn")
	c = c.with(assistant("done"), user("now add a test for it"))
	r.turn("", 20, 2, c, nil, "end_turn")
	tl := Build("s", r.events, time.Time{})
	wantPrompts(t, tl, []float64{0, 20}, []int64{0, 12000})
	if p := tl.Prompts[0]; p.End == nil || !p.End.Equal(at(8)) || p.FirstSeq != 1 || p.LastSeq != 3 {
		t.Errorf("prompt 1 = %+v, want it to end at 8s with the loop's second call", p)
	}
}

// The messages Claude Code injects in the person's place start no prompt alone; with text, the
// text does.
func TestPromptsInjectedMessages(t *testing.T) {
	var r rec
	c := conv{user("start")}
	r.turn("", 0, 1, c, nil, "end_turn")
	for i, injected := range []string{
		"<system-reminder>The date changed.</system-reminder>",
		"<task-notification><task-id>x</task-id><status>completed</status></task-notification>",
		"<command-name>/clear</command-name>\n<command-message>clear</command-message>\n<command-args></command-args>",
		"<local-command-stdout>Model set</local-command-stdout>",
		"[Request interrupted by user]",
		compactionSummary + " that ran out of context. The summary below covers ...",
	} {
		c = c.with(assistant("ok"), user(injected))
		r.turn("", float64(10*(i+1)), 1, c, nil, "end_turn")
	}
	c = c.with(assistant("ok"), user("<system-reminder>todo list</system-reminder>\nplease also update the docs"))
	r.turn("", 100, 1, c, nil, "end_turn")
	tl := Build("s", r.events, time.Time{})
	wantPrompts(t, tl, []float64{0, 100}, []int64{0, 39000})
}

// A retry resends the same conversation: no prompt. A suggestion-mode call between prompts
// neither starts one nor ends the previous one, so think time runs from the real last call.
func TestPromptsRetryAndSuggestionCall(t *testing.T) {
	var r rec
	c := conv{user("deploy it")}
	r.turn("", 0, 1, c, nil, "end_turn")
	r.turn("", 2, 3, c, nil, "end_turn") // the same conversation again: a retry
	r.turn("", 30, 1, c.with(assistant("done"), user("[SUGGESTION MODE: Suggest what the user might type next]")), nil, "end_turn")
	c = c.with(assistant("done"), user("thanks, now tag the release"))
	r.turn("", 50, 1, c, nil, "end_turn")
	tl := Build("s", r.events, time.Time{})
	wantPrompts(t, tl, []float64{0, 50}, []int64{0, 45000})
}

// A subagent's calls belong to the prompt that was open when it started, and extend its end.
func TestPromptsSubagentCallsJoinTheirPrompt(t *testing.T) {
	var r rec
	c := conv{user("survey the code")}
	r.turn("", 0, 2, c, nil, "tool_use",
		pipeline.InferenceToolCall{ID: "spawn1", Name: "Agent", Arguments: `{"description":"Survey","prompt":"..."}`})
	r.turn("a7f3", 1, 30, conv{user("...")}, nil, "end_turn")
	c = c.with(assistant(""), toolResultMsg())
	r.turn("", 32, 1, c, []pipeline.InferenceToolResult{result("spawn1", "done\nagentId: a7f3")}, "end_turn")
	tl := Build("s", r.events, time.Time{})
	wantPrompts(t, tl, []float64{0}, []int64{0})
	if p := tl.Prompts[0]; p.End == nil || !p.End.Equal(at(33)) {
		t.Errorf("prompt = %+v, want its end at the main call after the subagent", p)
	}
}

// An away recap between prompts is no call of either: think time runs from the last real call.
func TestPromptsAwayRecapIsNotWork(t *testing.T) {
	var r rec
	c := conv{user("run the benchmarks")}
	r.turn("", 0, 4, c, nil, "end_turn")
	r.turn("", 600, 5, c.with(assistant("done"), user("The user stepped away and is coming back. Recap ...")), nil, "end_turn")
	c = c.with(assistant("done"), user("what were the numbers?"))
	r.turn("", 700, 2, c, nil, "end_turn")
	tl := Build("s", r.events, time.Time{})
	wantPrompts(t, tl, []float64{0, 700}, []int64{0, 696000})
}

// A resumed session sends its old history first. The history is not a new prompt; what the
// person types after the resume is.
func TestPromptsResume(t *testing.T) {
	var r rec
	old := conv{user("first task"), assistant("done"), user("second task"), assistant("done")}
	r.turn("", 0, 2, old[:1], nil, "end_turn")
	r.turn("", 10, 2, old[:3], nil, "end_turn")
	// Later, --resume: the same history, then the new text.
	r.turn("", 5000, 3, old.with(user("pick up where we left off")), nil, "end_turn")
	tl := Build("s", r.events, time.Time{})
	wantPrompts(t, tl, []float64{0, 10, 5000}, []int64{0, 8000, 4988000})
}

// From the review of 1559c4a8: a hand-back's intro line is injected; a suggestion call does not
// stretch the prompt before it; a background subagent's later turns count in the later prompt;
// a message queued beside tool results has no think time; an image note is injected.
func TestPromptsReviewShapes(t *testing.T) {
	var r rec
	c := conv{user("survey it")}
	r.turn("", 0, 1, c, nil, "end_turn")
	// A suggestion call at 30s: prompt 1 still ends at 1s.
	r.turn("", 30, 5, c.with(assistant("done"), user("[SUGGESTION MODE: next?]")), nil, "end_turn")
	// A hand-back at 40s: no prompt.
	c = c.with(assistant("done"), user("Another Claude session sent a message:\n<agent-message from=\"a7f3\">done</agent-message>"))
	r.turn("", 40, 1, c, nil, "end_turn")
	// Prompt 2 at 50s calls a tool; a background subagent works from 52s to 200s.
	c = c.with(assistant("ok"), user("now write it up"))
	r.turn("", 50, 1, c, nil, "tool_use", pipeline.InferenceToolCall{ID: "t1", Name: "Bash"})
	r.turn("bg", 52, 148, conv{user("...")}, nil, "end_turn")
	// The tool's result comes with an image note, and later with a queued message.
	tc := pipeline.InferenceMessage{Role: "assistant", ToolUseIDs: "t1"}
	c = c.with(tc, user("[Image: original 2100x200, displayed at 2000x190]"))
	r.turn("", 60, 1, c, []pipeline.InferenceToolResult{result("t1", "ok")}, "tool_use", pipeline.InferenceToolCall{ID: "t2", Name: "Bash"})
	tc2 := pipeline.InferenceMessage{Role: "assistant", ToolUseIDs: "t2"}
	c = c.with(tc2, user("also check lint"))
	r.turn("", 300, 1, c, []pipeline.InferenceToolResult{result("t2", "ok")}, "end_turn")
	tl := Build("s", r.events, time.Time{})
	// Think time runs from the hand-back's call (the agent's work, ending at 41s), not the
	// suggestion call's end at 35s.
	wantPrompts(t, tl, []float64{0, 50, 300}, []int64{0, 9000, 0})
	if p := tl.Prompts[0]; p.End == nil || !p.End.Equal(at(41)) {
		t.Errorf("prompt 1 = %+v, want it to end at 41s, with the hand-back's call, not the suggestion's", p)
	}
	if p := tl.Prompts[1]; p.End == nil || !p.End.Equal(at(200)) {
		t.Errorf("prompt 2 = %+v, want it to end at 200s, with the background subagent", p)
	}
}

// humanText costs the message's length, not its square: 24,000 reminders after some text.
func TestHumanTextIsLinear(t *testing.T) {
	small := "hi " + strings.Repeat("<system-reminder>x</system-reminder>", 6000)
	large := "hi " + strings.Repeat("<system-reminder>x</system-reminder>", 24000)
	ds, dl := bestOfTimes(func() { humanText(small) }), bestOfTimes(func() { humanText(large) })
	// timing: compares the cost at two sizes; only their ratio is asserted
	if ratio := float64(dl) / float64(max(ds, time.Microsecond)); ratio > 10 && dl > 50*time.Millisecond {
		t.Errorf("humanText: %v at 6,000 reminders but %v at 24,000 (x%.1f): not linear", ds, dl, ratio)
	}
}

// bestOfTimes runs f twice and returns the shorter time.
func bestOfTimes(f func()) time.Duration {
	best := time.Duration(1<<63 - 1)
	for range 2 {
		start := time.Now()
		f()
		// timing: measures how humanText's cost scales with its input
		best = min(best, time.Since(start))
	}
	return best
}

// An open call leaves its prompt open.
func TestPromptsOpenCallLeavesThePromptOpen(t *testing.T) {
	var r rec
	r.add(pipeline.SessionEvent{At: at(0), Phase: pipeline.SessionRequest, RequestID: "r1", Host: "llm.example",
		Inference: &pipeline.InferenceExtension{AgentRole: pipeline.AgentRoleMain, Tools: tools, Messages: conv{user("go")}, Stream: true}})
	tl := Build("s", r.events, time.Time{})
	if len(tl.Prompts) != 1 || tl.Prompts[0].End != nil {
		t.Errorf("prompts = %+v, want one, still open", tl.Prompts)
	}
}

// A session with no human text has no prompts, and its JSON has no "prompts" key, so an older
// agentop decodes it unchanged.
func TestPromptsOmittedWhenEmpty(t *testing.T) {
	var r rec
	r.turn("", 0, 1, conv{user("<system-reminder>x</system-reminder>")}, nil, "end_turn")
	tl := Build("s", r.events, time.Time{})
	b, err := json.Marshal(tl)
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Prompts) != 0 || strings.Contains(string(b), `"prompts"`) {
		t.Errorf("prompts = %+v, json %s", tl.Prompts, b)
	}
}

func TestHumanText(t *testing.T) {
	for in, want := range map[string]string{
		"hello":                                "hello",
		"<system-reminder>a</system-reminder>": "",
		"<system-reminder>a</system-reminder>\n  ship it ":             "ship it",
		"<agent-message from=\"x\">done</agent-message>":               "",
		"<agent-messages>are a word</agent-messages> here":             "<agent-messages>are a word</agent-messages> here",
		"<system-reminder>unclosed":                                    "",
		"[Request interrupted by user for tool use]":                   "",
		"<user-prompt-submit-hook>ctx</user-prompt-submit-hook>fix it": "fix it",
		// a subagent's hand-back, shaped as a real recording carried it
		"<system-reminder>r</system-reminder>\n<tools-used-before-summary>x</tools-used-before-summary>\nAnother Claude session sent a message:\n<agent-message from=\"a1\">done</agent-message>\nThat \"other Claude session\" is an agent working inside this same session.": "",
	} {
		if got := humanText(in); got != want {
			t.Errorf("humanText(%q) = %q, want %q", in, got, want)
		}
	}
}
