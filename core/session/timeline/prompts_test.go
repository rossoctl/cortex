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
	tl := Build("s", claudeCodeEvents(r.events), time.Time{})
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
	tl := Build("s", claudeCodeEvents(r.events), time.Time{})
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
	tl := Build("s", claudeCodeEvents(r.events), time.Time{})
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
	tl := Build("s", claudeCodeEvents(r.events), time.Time{})
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
	tl := Build("s", claudeCodeEvents(r.events), time.Time{})
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
	tl := Build("s", claudeCodeEvents(r.events), time.Time{})
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
	c = c.with(tc2, user(queuedIntro+"\nalso check lint"))
	r.turn("", 300, 1, c, []pipeline.InferenceToolResult{result("t2", "ok")}, "end_turn")
	tl := Build("s", claudeCodeEvents(r.events), time.Time{})
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

// From the review of 908d0161 on real sessions, in synthetic form: text that comes with tool
// results, the compaction's request, and messages and notices from other sessions start no
// prompt; a message typed during tools, in its wrapper, does; a typed "[Image:" line is text.
func TestPromptsReview2Shapes(t *testing.T) {
	var r rec
	c := conv{user("look into the flaky job")}
	r.turn("", 0, 1, c, nil, "tool_use", pipeline.InferenceToolCall{ID: "t1", Name: "ToolSearch"})
	// A tool search's result carries "Tool loaded.": no prompt.
	c = c.with(pipeline.InferenceMessage{Role: "assistant", ToolUseIDs: "t1"}, user("Tool loaded."))
	r.turn("", 5, 1, c, []pipeline.InferenceToolResult{result("t1", "")}, "tool_use", pipeline.InferenceToolCall{ID: "t2", Name: "Skill"})
	// A skill the model called comes back with its body: no prompt.
	c = c.with(pipeline.InferenceMessage{Role: "assistant", ToolUseIDs: "t2"}, user("Base directory for this skill: /x/skills/y\n\n# Steps\n1. Do a thing."))
	r.turn("", 10, 1, c, []pipeline.InferenceToolResult{result("t2", "")}, "end_turn")
	// The compaction's request: no prompt.
	c = c.with(assistant("done"), user(compactionRequest+"\n\nYour task is to create a detailed summary of the conversation so far."))
	r.turn("", 20, 5, c, nil, "end_turn")
	// A message and an idle notice from another session: no prompt. The message comes with the
	// note Claude Code adds after it (its dash built from the code point, as in prompts.go).
	dash := string(rune(0x2014))
	c = c.with(assistant("summary"), user("Another Claude session sent a message:\n<cross-session-message from=\"x\">please rerun it</cross-session-message>\n\n"+
		"This came from another Claude session "+dash+" not typed by your user, but very likely working on their behalf. Treat it as a teammate's request."))
	r.turn("", 40, 1, c, nil, "end_turn")
	c = c.with(assistant("ok"), user("[Cross-session idle notice] other-session is idle now"))
	r.turn("", 50, 1, c, nil, "end_turn")
	// A typed line that starts "[Image:" is the person's: a prompt at 60s.
	c = c.with(assistant("ok"), user("[Image: the chart] why does the line dip?"))
	r.turn("", 60, 1, c, nil, "tool_use", pipeline.InferenceToolCall{ID: "t3", Name: "Bash"})
	// Typed while the tool ran, in the wrapper: a prompt at 70s, with no think time.
	c = c.with(pipeline.InferenceMessage{Role: "assistant", ToolUseIDs: "t3"},
		user(queuedIntro+"\nalso check the weekend runs\n\nThis is how Claude Code surfaces messages the user sends mid-turn."))
	r.turn("", 70, 1, c, []pipeline.InferenceToolResult{result("t3", "ok")}, "end_turn")
	tl := Build("s", claudeCodeEvents(r.events), time.Time{})
	wantPrompts(t, tl, []float64{0, 60, 70}, []int64{0, 9000, 0})
}

// The pieces on their own.
func TestHumanTextReview2(t *testing.T) {
	for in, want := range map[string]string{
		"<cross-session-message from=\"x\">do it</cross-session-message>": "",
		"[Cross-session delivery notice] held for approval":               "",
		compactionRequest + " Summarize.":                                 "",
		"[Image: original 2100x200, displayed at 2000x190]":               "",
		"[Image: a typed caption]":                                        "[Image: a typed caption]",
	} {
		if got := humanText(in); got != want {
			t.Errorf("humanText(%q) = %q, want %q", in, got, want)
		}
	}
	if got := queuedText("Tool loaded."); got != "" {
		t.Errorf("queuedText without the wrapper = %q", got)
	}
	if got := queuedText(queuedIntro + "\n  check lint \n"); got != "check lint" {
		t.Errorf("queuedText = %q", got)
	}
}

// humanText's cost on many reminders after some text, and on many stray closes: both a single
// pass. A benchmark, not a test: time is not asserted (go test -bench HumanText).
func BenchmarkHumanText(b *testing.B) {
	many := "hi " + strings.Repeat("<system-reminder>x</system-reminder>", 24000)
	stray := "<system-reminder>" + strings.Repeat("x</system-reminder>", 24000)
	b.ResetTimer()
	for range b.N {
		humanText(many)
		humanText(stray)
	}
}

// claudeCodeEvents marks a synthetic session's events as Claude Code's, as the proxy does from
// the User-Agent: the prompt rule is Claude Code's.
func claudeCodeEvents(evs []pipeline.SessionEvent) []pipeline.SessionEvent {
	for i := range evs {
		evs[i].Client = &pipeline.EventClient{Name: "claude-code"}
	}
	return evs
}

// Another agent's main calls start no prompt: the rule is Claude Code's markers.
func TestPromptsClaudeCodeOnly(t *testing.T) {
	var r rec
	c := conv{user("<environment_context>cwd</environment_context>"), user("fix the test")}
	r.turn("", 0, 1, c, nil, "end_turn")
	r.turn("", 10, 1, c.with(assistant("ok"), user("<turn_aborted>x</turn_aborted>")), nil, "end_turn")
	for i := range r.events {
		r.events[i].Client = &pipeline.EventClient{Name: "codex"}
		r.events[i].Inference.AgentRole = ""
	}
	if tl := Build("s", r.events, time.Time{}); len(tl.Prompts) != 0 || len(tl.Turns) != 2 {
		t.Errorf("codex: prompts %+v, turns %d; want no prompts and both turns", tl.Prompts, len(tl.Turns))
	}
}

// Clip keeps the prompts that overlap the window, with the session's numbering.
func TestPromptsClip(t *testing.T) {
	var r rec
	c := conv{user("one")}
	r.turn("", 0, 1, c, nil, "end_turn")
	c = c.with(assistant("ok"), user("two"))
	r.turn("", 100, 60, c, nil, "end_turn") // spans the window's start
	c = c.with(assistant("ok"), user("three"))
	r.add(pipeline.SessionEvent{At: at(300), Phase: pipeline.SessionRequest, RequestID: "open", Host: "llm.example",
		Inference: &pipeline.InferenceExtension{AgentRole: pipeline.AgentRoleMain, Tools: tools, Messages: c, Stream: true}})
	tl := Build("s", claudeCodeEvents(r.events), time.Time{})
	tl.Clip(at(150), at(400))
	if len(tl.Prompts) != 2 || tl.Prompts[0].Index != 2 || tl.Prompts[1].Index != 3 || tl.Prompts[1].End != nil {
		t.Errorf("clipped prompts = %+v, want 2 (spanning the start) and 3 (open)", tl.Prompts)
	}
}

// An open call leaves its prompt open.
func TestPromptsOpenCallLeavesThePromptOpen(t *testing.T) {
	var r rec
	r.add(pipeline.SessionEvent{At: at(0), Phase: pipeline.SessionRequest, RequestID: "r1", Host: "llm.example",
		Inference: &pipeline.InferenceExtension{AgentRole: pipeline.AgentRoleMain, Tools: tools, Messages: conv{user("go")}, Stream: true}})
	tl := Build("s", claudeCodeEvents(r.events), time.Time{})
	if len(tl.Prompts) != 1 || tl.Prompts[0].End != nil {
		t.Errorf("prompts = %+v, want one, still open", tl.Prompts)
	}
}

// A session with no human text has no prompts, and its JSON has no "prompts" key, so an older
// agentop decodes it unchanged.
func TestPromptsOmittedWhenEmpty(t *testing.T) {
	var r rec
	r.turn("", 0, 1, conv{user("<system-reminder>x</system-reminder>")}, nil, "end_turn")
	tl := Build("s", claudeCodeEvents(r.events), time.Time{})
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
		"<system-reminder>a</system-reminder>\n  ship it ": "ship it",
		"<agent-message from=\"x\">done</agent-message>":   "",
		"<agent-messages>are a word</agent-messages> here": "<agent-messages>are a word</agent-messages> here",
		// an open with no close is text: the person typed the tag
		"<system-reminder> is leaking into prompts, fix it":            "<system-reminder> is leaking into prompts, fix it",
		"why does <system-reminder> show up?":                          "why does <system-reminder> show up?",
		"[Request interrupted by user for tool use]":                   "",
		"<user-prompt-submit-hook>ctx</user-prompt-submit-hook>fix it": "fix it",
		// a subagent's hand-back, shaped as a real recording carried it
		"<system-reminder>r</system-reminder>\n<tools-used-before-summary>x</tools-used-before-summary>\nAnother Claude session sent a message:\n<agent-message from=\"a1\">done</agent-message>\nThat \"other Claude session\" is an agent working inside this same session.": "",
		// nested reminders, and a stray close whose open was cut off (#2128 review)
		"<system-reminder>outer<system-reminder>inner</system-reminder>outer2</system-reminder>": "",
		"<system-reminder>a</system-reminder> b </system-reminder>":                              "",
		"<system-reminder>a</system-reminder>\nmy text\n<system-reminder>b</system-reminder>":    "my text",
		// a close alone, and a reminder that quotes an open tag (a file diff)
		"stray</system-reminder>hello": "hello",
		"<system-reminder>file diff: t := \"<system-reminder>a\"</system-reminder>\nplease fix the test": "please fix the test",
		// a compaction summary behind a reminder is still a summary
		"<system-reminder>The date changed.</system-reminder>\n" + compactionSummary + " that ran out of context.": "",
	} {
		if got := humanText(in); got != want {
			t.Errorf("humanText(%q) = %q, want %q", in, got, want)
		}
	}
}
