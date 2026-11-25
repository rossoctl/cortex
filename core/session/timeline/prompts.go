package timeline

import (
	"sort"
	"strings"
	"time"

	"github.com/rossoctl/cortex/core/model"
)

// A PROMPT is the model calls one human instruction caused: from the call that carried what the
// person typed, through the tool loop, the subagents it spawned and the retries, to the call
// before the next thing they typed (#1922). A Turn is one model call; a prompt groups them.
//
// DERIVED FROM THE REQUEST BODIES, Claude Code only for now. Claude Code resends the whole
// conversation on every call, so a main-agent call starts a prompt when its newest user message
// is new since the agent's previous call and still holds text once the messages Claude Code
// injects in the user's place are taken out (injectedTags, injectedLines). Not a prompt:
//   - a tool result: Content keeps text blocks only, so a tool result's message is "";
//   - a retry (Turn.Resent), which adds nothing;
//   - a suggestion-mode call ("[SUGGESTION MODE"), and the side calls toolpair already drops
//     (title, topic probe, progress label, away recap);
//   - the first request after a compaction, whose newest user message is the summary: the
//     prompt it serves goes on.
//
// COMPUTED WHEN THE TIMELINE IS BUILT, NEVER STORED: each new injected marker is a rule change,
// and a stored grouping would go stale. The text is not in the timeline: it is content.

// Prompt is one human instruction and the calls it caused.
type Prompt struct {
	// Index is the prompt's number in the session, from 1.
	Index int `json:"index"`
	// Start is the request of the prompt's first call; End the end of its last call to finish,
	// a subagent's included. Absent while one of its calls is still open.
	Start time.Time  `json:"start"`
	End   *time.Time `json:"end,omitempty"`
	// FirstSeq is the first call's request; LastSeq the last call's.
	FirstSeq uint64 `json:"firstSeq"`
	LastSeq  uint64 `json:"lastSeq,omitempty"`
	// ThinkMs is the person's time before this prompt: from the end of the previous prompt's
	// last main-agent call to this prompt's start. Absent for the first prompt.
	ThinkMs int64 `json:"thinkMs,omitempty"`
	// Source is how the prompt was found: "derived", from the bodies. Telemetry (#1924) will
	// add "telemetry".
	Source string `json:"source"`
}

// promptCall is what buildAgents notes about each main-agent call for buildPrompts: its turn,
// and whether it starts a prompt.
type promptCall struct {
	turn   int
	starts bool
	side   bool // a suggestion-mode call: neither starts nor ends a prompt
	// queued is a prompt typed while tools ran: its text came beside the tool results. The time
	// before it was the tools', not the person's, so it has no think time.
	queued bool
}

// queuedMessage reports whether the newest user message follows an assistant message that
// called tools: in Anthropic's format that message carries the results, so any text in it was
// typed while the tools ran.
func queuedMessage(msgs []model.InferenceMessage) bool {
	i := newestUser(msgs)
	return i > 0 && msgs[i-1].Role == "assistant" && msgs[i-1].ToolUseIDs != ""
}

// suggestionPrompt marks Claude Code's suggestion-mode call: the whole context and tool list,
// asking for the person's likely next message. It is no work of the agent's on the prompt.
const suggestionPrompt = "[SUGGESTION MODE"

// compactionSummary starts the user message Claude Code sends after a compaction: the summary
// of the conversation so far, in the person's place.
const compactionSummary = "This session is being continued from a previous conversation"

// injectedTags are the blocks Claude Code puts in a user message that the person did not type:
// reminders, a slash command's scaffolding and its local output, a subagent's or a background
// task's notification. A message holding only these is no prompt; one holding them and text is,
// judged on the text.
var injectedTags = []string{"system-reminder", "command-name", "command-message", "command-args",
	"local-command-stdout", "local-command-stderr", "local-command-caveat", "task-notification",
	"agent-message", "user-prompt-submit-hook", "bash-input", "bash-stdout", "bash-stderr",
	"tools-used-before-summary"}

// injectedLines are lines Claude Code writes in the person's place, matched by their start:
// an interrupt notice, the note beside an image a tool returned, and the envelope around a
// subagent's or another session's <agent-message> (the line that introduces it and the note
// after it). Checked on a real recording: a subagent's hand-back is a user message of these and
// tags only.
var injectedLines = []string{"[Request interrupted by user", "[Image:", "Another Claude session sent a message",
	`That "other Claude session" is`}

// humanText returns what is left of a user message's text once the injected blocks and lines
// are taken out, trimmed: "" when the person typed none of it. Each tag is one pass that copies
// what it keeps, so a message of many reminders costs its length, not its length squared.
func humanText(s string) string {
	if strings.HasPrefix(strings.TrimSpace(s), compactionSummary) {
		return ""
	}
	for _, tag := range injectedTags {
		if !strings.Contains(s, "<"+tag) {
			continue
		}
		s = stripTag(s, "<"+tag, "</"+tag+">")
	}
	var b strings.Builder
	for line := range strings.Lines(s) {
		t := strings.TrimSpace(line)
		injected := false
		for _, l := range injectedLines {
			injected = injected || strings.HasPrefix(t, l)
		}
		if !injected {
			b.WriteString(line)
		}
	}
	return strings.TrimSpace(b.String())
}

// stripTag removes each open...close block from s. An open tag whose name goes on
// ("<agent-messages") is kept; an unclosed block runs to the end.
func stripTag(s, open, close string) string {
	var b strings.Builder
	for from := 0; ; {
		k := strings.Index(s[from:], open)
		if k < 0 {
			b.WriteString(s[from:])
			return b.String()
		}
		i := from + k
		if j := i + len(open); j < len(s) && s[j] != '>' && s[j] != ' ' && s[j] != '\n' && s[j] != '\t' {
			b.WriteString(s[from:j])
			from = j
			continue
		}
		b.WriteString(s[from:i])
		j := strings.Index(s[i:], close)
		if j < 0 {
			return b.String()
		}
		from = i + j + len(close)
	}
}

// newestUser is the index of a request's newest user message, or -1. It reads from the end, so
// a conversation resent whole on every call costs a few messages per call, not all of them.
func newestUser(msgs []model.InferenceMessage) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return i
		}
	}
	return -1
}

// startsPrompt reports whether a request's newest user message was added since the agent's
// previous call (at index prefix or later, prefix being the messages the two share) and holds
// human text.
func startsPrompt(msgs []model.InferenceMessage, prefix int) bool {
	i := newestUser(msgs)
	return i >= 0 && i >= prefix && humanText(msgs[i].Content) != ""
}

// isSuggestionCall reports whether the newest user message asks for a suggestion.
func isSuggestionCall(msgs []model.InferenceMessage) bool {
	i := newestUser(msgs)
	return i >= 0 && strings.Contains(msgs[i].Content, suggestionPrompt)
}

// buildPrompts groups the turns into prompts. mains are the main agent's calls in order. Each
// turn, a subagent's too, belongs to the prompt open when it started: a background subagent
// still working in a later prompt counts there, so an old prompt never stretches over the ones
// after it.
func buildPrompts(t *Timeline, mains []promptCall) {
	var ps []Prompt
	lastMainEnd := map[int]*time.Time{} // per prompt index: its last main call's end
	side := map[int]bool{}              // the turns of suggestion-mode calls
	for _, c := range mains {
		tu := &t.Turns[c.turn]
		if c.side {
			side[c.turn] = true
			continue
		}
		if c.starts && !tu.Resent {
			p := Prompt{Index: len(ps) + 1, Start: tu.Start, FirstSeq: tu.Seq, Source: "derived"}
			if len(ps) > 0 && !c.queued {
				if e := lastMainEnd[len(ps)]; e != nil && tu.Start.After(*e) {
					p.ThinkMs = tu.Start.Sub(*e).Milliseconds()
				}
			}
			ps = append(ps, p)
		}
		if len(ps) == 0 {
			continue // calls before the first prompt this recording saw
		}
		if e := tu.End; e != nil {
			if prev := lastMainEnd[len(ps)]; prev == nil || e.After(*prev) {
				lastMainEnd[len(ps)] = e
			}
		}
	}
	if len(ps) == 0 {
		return
	}
	// Each turn's prompt: the latest prompt started at or before it. The prompts are in start
	// order, so this is a binary search.
	promptAt := func(at time.Time) int {
		return sort.Search(len(ps), func(i int) bool { return ps[i].Start.After(at) })
	}
	stillOpen := map[int]bool{}
	for i := range t.Turns {
		tu := &t.Turns[i]
		n := promptAt(tu.Start)
		if n == 0 || side[i] {
			continue
		}
		p := &ps[n-1]
		if tu.Seq > p.LastSeq {
			p.LastSeq = tu.Seq
		}
		switch {
		case tu.End == nil:
			stillOpen[n] = true
		case p.End == nil || tu.End.After(*p.End):
			e := *tu.End
			p.End = &e
		}
	}
	for n := range stillOpen {
		ps[n-1].End = nil
	}
	t.Prompts = ps
}
