package timeline

import (
	"slices"
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
//   - the compaction's own request, and the first request after it, whose newest user message is
//     the summary: the prompt they serve goes on;
//   - text beside tool results that came with the tools ("Tool loaded.", a skill's body): only a
//     message the person typed while the tools ran, in Claude Code's wrapper for it, is theirs;
//   - a message or a notice from another Claude session.
//
// COMPUTED WHEN THE TIMELINE IS BUILT, NEVER STORED: each new injected marker is a rule change,
// and a stored grouping would go stale. The text is not in the timeline: it is content.

// Prompt is one human instruction and the calls it caused.
type Prompt struct {
	// Index is the prompt's number in the session, from 1. A clipped window keeps the session's
	// numbering, so its first prompt need not be 1.
	Index int `json:"index"`
	// Start is the request of the prompt's first call; End the end of its last call to finish,
	// a subagent's included. Absent while one of its calls is still open. A call that runs past
	// the next prompt's start (a background subagent's) carries End past it: prompts can overlap.
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

// claudeCodeCall reports whether a request is Claude Code's: its client names it, or it was
// imported from Claude Code's transcripts. The prompt rule is Claude Code's markers, so another
// agent's calls (Codex, Bob, OpenCode, Pi) start no prompt; they keep their turns.
func claudeCodeCall(e *model.SessionEvent) bool {
	return e.Client != nil && e.Client.Name == claudeCode || e.Source == model.ImportedSource(claudeCode)
}

const claudeCode = "claude-code"

// queuedMessage reports whether the newest user message follows an assistant message that
// called tools: in Anthropic's format that message carries the results. Text in it is the
// person's only inside Claude Code's wrapper for a message typed while the tools ran
// (queuedIntro); any other text there came with the tools: "Tool loaded." after a tool search, a
// skill's body after the model called the skill.
func queuedMessage(msgs []model.InferenceMessage) bool {
	i := newestUser(msgs)
	return i > 0 && msgs[i-1].Role == "assistant" && msgs[i-1].ToolUseIDs != ""
}

// queuedIntro starts the wrapper Claude Code puts around a message the person typed while tools
// ran; queuedText is what they typed, or "" when the message holds no such wrapper.
const queuedIntro = "The user sent a new message while you were working:"

func queuedText(s string) string {
	_, after, ok := strings.Cut(s, queuedIntro)
	if !ok {
		return ""
	}
	return humanText(after)
}

// suggestionPrompt marks Claude Code's suggestion-mode call: the whole context and tool list,
// asking for the person's likely next message. It is no work of the agent's on the prompt.
const suggestionPrompt = "[SUGGESTION MODE"

// compactionSummary starts the user message Claude Code sends after a compaction: the summary
// of the conversation so far, in the person's place.
const compactionSummary = "This session is being continued from a previous conversation"

// compactionRequest starts the user message of the call that makes that summary: sent as the
// main agent, with its tools, so it is no side call, but no one typed it.
const compactionRequest = "CRITICAL: Respond with TEXT ONLY. Do NOT call any tools."

// injectedTags are the blocks Claude Code puts in a user message that the person did not type:
// reminders, a slash command's scaffolding and its local output, a subagent's or a background
// task's notification, and a message from another Claude session. A message holding only these
// is no prompt; one holding them and text is, judged on the text. A cross-session message is
// injected even when the other session relays something on the person's behalf: it was not typed
// here.
var injectedTags = []string{"system-reminder", "command-name", "command-message", "command-args",
	"local-command-stdout", "local-command-stderr", "local-command-caveat", "task-notification",
	"agent-message", "cross-session-message", "user-prompt-submit-hook", "bash-input", "bash-stdout",
	"bash-stderr", "tools-used-before-summary"}

// injectedLines are lines Claude Code writes in the person's place, matched by their start:
// an interrupt notice, the note beside an image a tool returned ("[Image: original 2100x200,
// displayed at ..."; a line the person types starting "[Image:" is theirs), the envelope around a
// subagent's or another session's message (the line that introduces it and the note after it),
// the notices about other sessions, and the note after a message typed while tools ran. Checked
// on a real recording: a subagent's hand-back is a user message of these and tags only.
var injectedLines = []string{"[Request interrupted by user", "[Image: original ", "Another Claude session sent a message",
	`That "other Claude session" is`, crossSessionNote, "[Cross-session idle notice]", "[Cross-session delivery notice]",
	"This is how Claude Code surfaces messages the user sends mid-turn"}

// crossSessionNote opens the note Claude Code puts after another session's message: "This came
// from another Claude session", an em-dash, "not typed by your user, but ...". The dash is built
// from its code point, so this file holds no literal one.
var crossSessionNote = "This came from another Claude session " + string(rune(0x2014)) + " not typed by your user"

// humanText returns what is left of a user message's text once the injected blocks and lines
// are taken out, trimmed: "" when the person typed none of it. Each tag is one pass that copies
// what it keeps, so a message of many reminders costs its length, not its length squared.
func humanText(s string) string {
	for _, tag := range injectedTags {
		// An open or a close: a message can hold only the close of a block cut off before it.
		if !strings.Contains(s, "<"+tag) && !strings.Contains(s, "</"+tag+">") {
			continue
		}
		s = stripTag(s, "<"+tag, "</"+tag+">")
	}
	// After the tags: a summary Claude Code prefixes with a reminder is still a summary, and so
	// is the request that asks for one.
	if t := strings.TrimSpace(s); strings.HasPrefix(t, compactionSummary) || strings.HasPrefix(t, compactionRequest) {
		return ""
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
	// Again after the lines: an injected line can stand before a summary or its request.
	t := strings.TrimSpace(b.String())
	if strings.HasPrefix(t, compactionSummary) || strings.HasPrefix(t, compactionRequest) {
		return ""
	}
	return t
}

// stripTag removes each open...close block from s, pairing them by depth: real messages nest
// reminders, and the inner close comes first. A close with no open before it ends a block
// whose open was cut off, so the text since the last kept point goes with it. An open tag whose
// name goes on ("<agent-messages") is not one.
//
// WHEN THE OPENS OUTNUMBER THE CLOSES, depth would drop the rest of the message, and two real
// shapes do that: a reminder that quotes an open tag (a file diff holding `"<system-reminder>"`)
// and a tag the person typed ("why does <system-reminder> show up?"). So from the first open
// left unmatched, each open is paired with the first close after it instead, and an open with
// no close is kept as text. One pass each.
func stripTag(s, open, close string) string {
	var b strings.Builder
	keep, depth := 0, 0 // keep: where the text not yet written or dropped starts
	unmatched := -1     // where the outermost open still unmatched began
	// The next open and close at or after i, found once each and kept until passed, so the scan
	// stays linear however the tags are mixed.
	nextO, nextC := -2, -2
	find := func(cur *int, tag string, i int) int {
		if *cur == -1 || *cur >= i {
			return *cur
		}
		if k := strings.Index(s[i:], tag); k >= 0 {
			*cur = i + k
		} else {
			*cur = -1
		}
		return *cur
	}
	for i := 0; i < len(s); {
		o, c := find(&nextO, open, i), find(&nextC, close, i)
		switch {
		case o < 0 && c < 0:
			i = len(s)
		case o >= 0 && (c < 0 || o < c):
			at := o
			if j := at + len(open); j < len(s) && s[j] != '>' && s[j] != ' ' && s[j] != '\n' && s[j] != '\t' {
				i = j // another tag's name: text
				continue
			}
			if depth == 0 {
				b.WriteString(s[keep:at])
				unmatched = at
			}
			depth++
			i = at + len(open)
		default:
			end := c + len(close)
			if depth > 0 {
				depth--
			}
			if depth == 0 {
				keep = end // the block, or the cut-off block a stray close ends, is dropped
			}
			i = end
		}
	}
	if depth == 0 {
		b.WriteString(s[keep:])
	} else {
		b.WriteString(stripFirstClose(s[unmatched:], open, close))
	}
	return b.String()
}

// stripFirstClose removes each open...close block, pairing an open with the first close after
// it; an open with no close after it is kept as text.
func stripFirstClose(s, open, close string) string {
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
		j := strings.Index(s[i:], close)
		if j < 0 {
			b.WriteString(s[from:])
			return b.String()
		}
		b.WriteString(s[from:i])
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
// human text. Beside tool results, the person's text is one of three:
//   - inside the queued-message wrapper (typed while the tools ran);
//   - after an interrupt: Claude Code's "[Request interrupted by user" line, then what the person
//     typed, in the same message as the cancelled call's result;
//   - a rejected call's reason: the result says the person did not want the tool used, then
//     "the user said:" and their words (results are the request's, matched to this message's
//     calls).
//
// Anything else there came with the tools: "Tool loaded.", a skill's body.
func startsPrompt(msgs []model.InferenceMessage, prefix int, results []model.InferenceToolResult) bool {
	i := newestUser(msgs)
	if i < 0 || i < prefix {
		return false
	}
	if !queuedMessage(msgs) {
		return humanText(msgs[i].Content) != ""
	}
	if queuedText(msgs[i].Content) != "" || afterInterrupt(msgs[i].Content) != "" {
		return true
	}
	return rejectedWithReason(msgs[i-1].ToolUseIDs, results)
}

// queuedPrompt reports whether the newest user message is a prompt typed while tools ran, in its
// wrapper: its time before was the tools', so it gets no think time. An interrupt or a
// rejection is no such prompt: the person stopped the tools, and their time is theirs.
func queuedPrompt(msgs []model.InferenceMessage) bool {
	i := newestUser(msgs)
	return queuedMessage(msgs) && queuedText(msgs[i].Content) != ""
}

// afterInterrupt is what the person typed after an interrupt: the human text after the last
// interrupt line, or "". Only after it: what comes before is the cancelled tools' (a "Tool
// loaded.", a skill's body).
func afterInterrupt(s string) string {
	k := strings.LastIndex(s, interruptLine)
	if k < 0 {
		return ""
	}
	rest := s[k:]
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		return humanText(rest[nl+1:])
	}
	return ""
}

// interruptLine starts the line Claude Code writes when the person interrupts ("[Request
// interrupted by user]", "... for tool use]"); it is an injected line itself.
const interruptLine = "[Request interrupted by user"

// rejectedIntro starts the result Claude Code sends for a tool call the person rejected;
// rejectedReason introduces what they said with it, when they said anything.
const (
	rejectedIntro  = "The user doesn't want to proceed with this tool use."
	rejectedReason = "the user said:"
)

// rejectedWithReason reports whether a result for one of the calls ids names (", "-joined) is a
// rejection that carries the person's words.
func rejectedWithReason(ids string, results []model.InferenceToolResult) bool {
	if ids == "" {
		return false
	}
	for _, r := range results {
		if !r.IsError || !strings.HasPrefix(strings.TrimSpace(r.Content), rejectedIntro) || !slices.Contains(strings.Split(ids, ", "), r.ToolUseID) {
			continue
		}
		if _, reason, ok := strings.Cut(r.Content, rejectedReason); ok && humanText(reason) != "" {
			return true
		}
	}
	return false
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
