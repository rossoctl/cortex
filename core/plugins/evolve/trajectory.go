package evolve

import (
	"fmt"
	"strings"

	"github.com/rossoctl/cortex/core/pipeline"
)

const (
	maxToolResultChars = 4000
	maxUserChars       = 8000
	// maxPendingMessages bounds one session's unsent turns. Past it the oldest
	// are dropped: evolve renders a bounded window of a trajectory anyway.
	maxPendingMessages = 400
)

// clean removes harness context and previously injected guidelines from a
// message's text: neither is the user's words, and learning from injected
// guidelines would feed evolve its own output.
func clean(text string) string {
	text = reminderPattern.ReplaceAllString(text, "")
	text = blockPattern.ReplaceAllString(text, "")
	return strings.TrimSpace(text)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
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
	// epoch distinguishes this recorder's batch ids from those of an earlier
	// Cortex process recording the same session, which evolve would otherwise
	// skip as already processed.
	epoch int64
	taken int // turns in the batch last taken, for restore
}

func newRecorder(agent string, epoch int64) *recorder {
	return &recorder{agent: agent, calls: map[string]bool{}, epoch: epoch}
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
		content := truncate(res.Content, maxToolResultChars)
		if res.IsError {
			content = "[error] " + content
		}
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
	id := fmt.Sprintf("%d-%d", r.epoch, r.batch)
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
	r.taken = r.turns
	r.pending, r.turns = nil, 0
	return t, true
}

// restore puts the batch last taken back after a failed send, so the next
// attempt resends it under the same batch id.
func (r *recorder) restore(t Trajectory) {
	r.pending = append(t.Messages, r.pending...)
	r.turns += r.taken
	r.batch--
}

func startsWithUser(msgs []map[string]any, text string) bool {
	for _, m := range msgs {
		if m["role"] == "user" {
			return m["content"] == text
		}
	}
	return false
}
