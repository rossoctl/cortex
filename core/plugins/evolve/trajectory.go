package evolve

import (
	"fmt"
	"hash/fnv"
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
	// openCallTurns is how many turns a tool call waits for its result before it
	// is forgotten. A result comes in the next request; a call the agent never
	// answered would otherwise stay open for the rest of the session.
	openCallTurns = 16
	// maxLearnAttempts is how many times one batch is sent before the plugin gives
	// up on it. Some failures never clear, such as a backend that can't process
	// identified batches or a profile that doesn't exist.
	maxLearnAttempts = 6
	// maxRetryWait caps the backoff between sends of a failed batch, which
	// doubles from the sender's tick.
	maxRetryWait = 30 * time.Minute
)

// clean removes Claude Code's system reminders and previously injected
// guidelines from a message's text: neither is the user's words, and learning
// from injected guidelines would feed evolve its own output.
func clean(text string) string {
	text = reminderPattern.ReplaceAllString(text, "")
	text = blockPattern.ReplaceAllString(text, "")
	return strings.TrimSpace(text)
}

// truncate cuts s to at most n bytes, at a rune boundary, so a multibyte
// character isn't cut into invalid UTF-8.
// cleanUser cleans a user message from agent. Codex's own context (its AGENTS.md
// instructions and environment context) comes as user text, so it is removed
// from Codex's messages, and only theirs: another agent's user can write the
// same tags, and those are the user's words.
func cleanUser(text, agent string) string {
	if agent == "codex" {
		text = codexContextPattern.ReplaceAllString(text, "")
	}
	return clean(text)
}

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
	// first and last fingerprint the first and the last of those messages, to
	// notice a history rewritten without getting shorter.
	first, last uint64
	calls       map[string]int // tool-call ids awaiting a result, and the turn each was made in
	turn        int            // turns observed in all, across batches
	batch       int            // batches sent so far
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
	return &recorder{agent: agent, calls: map[string]int{}, gen: gen}
}

// observe folds one completed main-agent exchange into the recorder.
func (r *recorder) observe(inf *pipeline.InferenceExtension) {
	if inf.Model != "" {
		r.model = inf.Model
	}
	// Where this request's new user messages start. A recorder that has seen the
	// history reads on from where it stopped. One that hasn't, because it is new
	// (Cortex restarted, the config was reloaded, or the session was evicted and
	// came back), or whose history was rewritten (compacted, or a changed system
	// message), starts at the latest user message: the ones before it were learned
	// from already, or answered before this recorder existed, and sending them
	// again would have evolve learn twice from prompts it has had, without the
	// replies that gave them context. A rewrite need not make the history
	// shorter, so the first and last messages seen are compared too.
	from := r.seen
	if r.turn == 0 || len(inf.Messages) < r.seen || (r.seen > 0 &&
		(fingerprint(inf.Messages[0]) != r.first || fingerprint(inf.Messages[r.seen-1]) != r.last)) {
		from = latestUserText(inf.Messages, r.agent)
	}
	// The task is the conversation's first user message, kept as context for
	// batches that don't start with it.
	if r.task == "" {
		r.task = firstUserText(inf.Messages, r.agent)
	}
	for _, res := range inf.ToolResults {
		if _, open := r.calls[res.ToolUseID]; !open {
			continue
		}
		delete(r.calls, res.ToolUseID)
		// Cleaned like user text, before the cap: a tool can echo the injected block
		// back, and Claude Code adds its reminders to tool results.
		content := clean(res.Content)
		if res.IsError {
			content = "[error] " + content
		}
		content = truncate(content, maxToolResultChars)
		r.add(map[string]any{"role": "tool", "tool_call_id": res.ToolUseID, "content": content})
	}
	for _, m := range inf.Messages[from:] {
		if m.Role != "user" {
			continue
		}
		if text := cleanUser(m.Content, r.agent); text != "" {
			r.add(map[string]any{"role": "user", "content": truncate(text, maxUserChars)})
		}
	}
	r.seen = len(inf.Messages)
	if r.seen > 0 {
		r.first, r.last = fingerprint(inf.Messages[0]), fingerprint(inf.Messages[r.seen-1])
	}

	// The assistant can quote the injected block, too.
	completion := clean(inf.Completion)
	assistant := map[string]any{"role": "assistant", "content": completion}
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
			r.calls[id] = r.turn
		}
		assistant["tool_calls"] = calls
	}
	if completion != "" || len(inf.ToolCalls) > 0 {
		r.add(assistant)
	}
	r.turns++
	r.turn++
	for id, at := range r.calls {
		if r.turn-at > openCallTurns {
			delete(r.calls, id)
		}
	}
}

// latestUserText is the index of the last user message with text once cleaned,
// or len(messages) if there is none.
func latestUserText(messages []pipeline.InferenceMessage, agent string) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" && cleanUser(messages[i].Content, agent) != "" {
			return i
		}
	}
	return len(messages)
}

func firstUserText(messages []pipeline.InferenceMessage, agent string) string {
	for _, m := range messages {
		if m.Role != "user" {
			continue
		}
		if text := cleanUser(m.Content, agent); text != "" {
			return truncate(text, maxUserChars)
		}
	}
	return ""
}

func fingerprint(m pipeline.InferenceMessage) uint64 {
	h := fnv.New64a()
	h.Write([]byte(m.Role))
	h.Write([]byte{0})
	h.Write([]byte(m.Content))
	return h.Sum64()
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
	// A batch that doesn't start with the task (any after the first, and the first
	// of a recorder that joined the session late) gets it as supporting context.
	if r.task != "" && !startsWithUser(r.pending, r.task) {
		t.ContextMessages = []map[string]any{{"role": "user", "content": r.task}}
	}
	r.pending, r.turns = nil, 0
	return t, true
}

// next returns the batch to send at now, if any. Nothing is sent while the
// session is active. When it is idle: a failed batch once its retry is due, as
// it was; otherwise its new turns, as a new batch.
func (r *recorder) next(sessionID string, minTurns int, idle bool, now time.Time) (Trajectory, bool) {
	if !idle {
		return Trajectory{}, false
	}
	if r.retry != nil {
		if now.Before(r.retryAt) {
			return Trajectory{}, false
		}
		return *r.retry, true
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
