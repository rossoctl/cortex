package evolve

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rossoctl/cortex/core/pipeline"
)

const (
	maxToolResultChars = 4000
	maxUserChars       = 8000
	// maxPendingMessages bounds one session's unsent turns. Past it the oldest
	// are dropped: evolve renders a bounded window of a trajectory anyway.
	maxPendingMessages = 400
	// maxLearnAttempts is how many times one batch is sent before the plugin gives
	// up on it. Some failures never clear, such as a backend that can't process
	// identified batches or a profile that doesn't exist.
	maxLearnAttempts = 6
	// maxRetryWait caps the backoff between sends of a failed batch, which
	// doubles from the sender's tick.
	maxRetryWait = 30 * time.Minute
)

// clean removes harness context (Claude Code's system reminders, Codex's
// AGENTS.md instructions and environment context) and previously injected
// guidelines from a message's text: neither is the user's words, and learning
// from injected guidelines would feed evolve its own output.
func clean(text string) string {
	text = reminderPattern.ReplaceAllString(text, "")
	text = codexContextPattern.ReplaceAllString(text, "")
	text = blockPattern.ReplaceAllString(text, "")
	return strings.TrimSpace(text)
}

// truncate cuts s to at most n bytes, at a rune boundary, so a multibyte
// character isn't cut into invalid UTF-8.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// recorder accumulates one session's conversation as OpenAI chat messages,
// one completed main-agent turn at a time.
//
// Cortex's parsed request keeps only the text of each message, so the
// conversation's tool calls are taken from each turn's own response and its
// tool results from the next request's toolResults, matched by id.
type recorder struct {
	agent   string
	model   string
	task    string           // first user message, kept as context for later batches
	pending []map[string]any // turns not yet sent to evolve
	turns   int              // completed turns in pending
	seen    int              // request messages already accounted for
	calls   map[string]bool  // tool-call ids awaiting a result
	batch   int              // batches sent so far
	// gen distinguishes this recorder's batch ids from those of any other
	// recorder of the same session, which evolve would otherwise skip as already
	// processed: one in an earlier Cortex process (the epoch), or an earlier one
	// in this process, before the session was evicted and resumed.
	gen string
	// retry is a batch whose send failed. It is sent again as it was, under its
	// own id, before any new batch is taken, so a batch evolve did accept but
	// whose answer was lost is skipped, and new turns aren't skipped with it.
	retry    *Trajectory
	attempts int // failed sends of retry
	retryAt  time.Time
}

func newRecorder(agent, gen string) *recorder {
	return &recorder{agent: agent, calls: map[string]bool{}, gen: gen}
}

// observe folds one completed main-agent exchange into the recorder.
func (r *recorder) observe(inf *pipeline.InferenceExtension) {
	if inf.Model != "" {
		r.model = inf.Model
	}
	if len(inf.Messages) < r.seen { // the agent compacted its context
		r.seen = 0
	}
	for _, res := range inf.ToolResults {
		if !r.calls[res.ToolUseID] {
			continue
		}
		delete(r.calls, res.ToolUseID)
		content := res.Content
		if res.IsError {
			content = "[error] " + content
		}
		content = truncate(content, maxToolResultChars)
		r.add(map[string]any{"role": "tool", "tool_call_id": res.ToolUseID, "content": content})
	}
	for _, m := range inf.Messages[r.seen:] {
		if m.Role != "user" {
			continue
		}
		if text := clean(m.Content); text != "" {
			text = truncate(text, maxUserChars)
			if r.task == "" {
				r.task = text
			}
			r.add(map[string]any{"role": "user", "content": text})
		}
	}
	r.seen = len(inf.Messages)

	assistant := map[string]any{"role": "assistant", "content": inf.Completion}
	if len(inf.ToolCalls) > 0 {
		calls := make([]map[string]any, 0, len(inf.ToolCalls))
		for i, c := range inf.ToolCalls {
			id := c.ID
			if id == "" {
				id = fmt.Sprintf("call_%d_%d", r.turns, i)
			}
			args := c.Arguments
			if args == "" {
				args = "{}"
			}
			calls = append(calls, map[string]any{
				"id": id, "type": "function",
				"function": map[string]any{"name": c.Name, "arguments": args},
			})
			r.calls[id] = true
		}
		assistant["tool_calls"] = calls
	}
	if inf.Completion != "" || len(inf.ToolCalls) > 0 {
		r.add(assistant)
	}
	r.turns++
}

func (r *recorder) add(m map[string]any) {
	r.pending = append(r.pending, m)
	if over := len(r.pending) - maxPendingMessages; over > 0 {
		r.pending = r.pending[over:]
	}
}

// take returns the pending turns as one trajectory batch and resets the
// recorder for the next batch, or ok=false when there is too little to learn
// from. The batch id is a per-session sequence number, so evolve skips a batch
// it has already processed if a send is retried.
func (r *recorder) take(sessionID string, minTurns int) (t Trajectory, ok bool) {
	if r.turns < minTurns || len(r.pending) == 0 {
		return Trajectory{}, false
	}
	r.batch++
	id := fmt.Sprintf("%s-%d", r.gen, r.batch)
	t = Trajectory{
		Messages: r.pending,
		Batch:    &Batch{Source: "cortex", ConversationID: sessionID, BatchID: id, Scope: "cortex"},
		TraceID:  fmt.Sprintf("cortex:%s:%s", sessionID, id),
		Model:    r.model,
		Metadata: map[string]any{"creation_mode": "auto-cortex", "agent_id": r.agent, "session_id": sessionID},
	}
	// After the first batch, the task is supporting context rather than new material.
	if r.batch > 1 && r.task != "" && !startsWithUser(r.pending, r.task) {
		t.ContextMessages = []map[string]any{{"role": "user", "content": r.task}}
	}
	r.pending, r.turns = nil, 0
	return t, true
}

// next returns the batch to send at now, if any: a failed batch once its retry
// is due; otherwise, when the session is idle, its new turns as a new batch.
func (r *recorder) next(sessionID string, minTurns int, idle bool, now time.Time) (Trajectory, bool) {
	if r.retry != nil {
		if now.Before(r.retryAt) {
			return Trajectory{}, false
		}
		return *r.retry, true
	}
	if !idle {
		return Trajectory{}, false
	}
	return r.take(sessionID, minTurns)
}

// sent records how sending t went. A failed batch is kept to be sent again after
// a backoff that doubles from tick, and dropped after maxLearnAttempts sends,
// which sent reports. New turns meanwhile stay in pending, bounded as always.
func (r *recorder) sent(t Trajectory, err error, now time.Time, tick time.Duration) (dropped bool) {
	if err == nil {
		r.retry, r.attempts = nil, 0
		return false
	}
	r.attempts++
	if r.attempts >= maxLearnAttempts {
		r.retry, r.attempts = nil, 0
		return true
	}
	r.retry = &t
	r.retryAt = now.Add(min(tick<<(r.attempts-1), maxRetryWait))
	return false
}

func startsWithUser(msgs []map[string]any, text string) bool {
	for _, m := range msgs {
		if m["role"] == "user" {
			return m["content"] == text
		}
	}
	return false
}
