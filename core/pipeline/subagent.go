package pipeline

import "strings"

// ClaudeCodeAgentIDHeader is the request header Claude Code sets on a subagent's
// requests, naming that subagent. Verified on claude-cli/2.1.302: the CLI sets it
// whenever the request runs in a subagent's context (and an agent-team teammate's),
// and the main conversation's requests do not carry it. The value is the same id the
// CLI names the subagent's transcript after —
// ~/.claude/projects/<dir>/<session>/subagents/agent-<id>.jsonl, beside an
// agent-<id>.meta.json carrying its agentType and description — so an event keyed on
// it lines up with what the user's own client wrote.
//
// Here rather than beside session.ClaudeCodeSessionHeader because session imports
// pipeline, and the reader of this one is Context. Canonical HTTP casing, for the
// reason session.BobSessionHeader gives.
//
// NOT A SESSION ID HEADER, and it must not be added to session.id_headers. A subagent's
// requests carry their parent's X-Claude-Code-Session-Id, so they are filed in the
// parent's session and counted in its cost; bucketing on this header instead would give
// every subagent a session row of its own and take its spend away from the
// conversation that started it.
//
// The CLI also sends X-Claude-Code-Parent-Agent-Id on a subagent a subagent started.
// It is not read: this records which agent made a request, not the tree they form.
const ClaudeCodeAgentIDHeader = "X-Claude-Code-Agent-Id"

// Subagent returns the Claude Code subagent this request came from: the value of
// X-Claude-Code-Agent-Id as the client sent it, or "" when there is none to record.
//
// "" MEANS NOTHING WAS STATED, and there are four ways to get it: a main-conversation
// request, a client that is not Claude Code, a row with no request headers (an opaque
// tunnel, a transparent connection), and a value validSubagentID refused. A reader
// owes them the same treatment — this request names no subagent — and for "is this
// a subagent at all" should read Inference.AgentRole, which is stated independently
// of this header.
//
// Memoized and pinned by ResolveClient on exactly the terms ClientInfo is; see there,
// including for why a nil receiver panics.
//
// CLIENT-ASSERTED AND UNAUTHENTICATED: any caller of the proxy can send any id, so
// this is a display and grouping key, never a principal. session.IDFromHeaders' TRUST
// section applies unchanged.
func (c *Context) Subagent() string {
	if c.subagentParsed {
		return c.subagent
	}
	c.subagentParsed = true
	if c.Headers != nil {
		if id := strings.TrimSpace(c.Headers.Get(ClaudeCodeAgentIDHeader)); validSubagentID(id) {
			c.subagent = id
		}
	}
	return c.subagent
}

// validSubagentID reports whether a client-supplied subagent id is safe to record.
//
// REFUSED, NOT SANITISED — the opposite choice from RecordStrippedUpgrade, and for a
// reason that is about what the value is for. An Upgrade token is read by a person, so
// a cleaned-up one still answers the question. This id is a join key: its use is
// finding the agent-<id>.meta.json it names, and a sanitised id names no file, so it
// would sit on the event looking like an answer while matching nothing. Refusing it
// leaves the row as one that names no subagent, which is true. validHeaderSessionID
// makes the same call for the same reason.
//
// The rules are the session id's, plus the empty check: no longer than maxClientLen
// bytes, and nothing hasControlRunes objects to, which covers C0, DEL, C1, the
// glyph-reordering runes and invalid UTF-8. That last group matters here and not
// only on the forward proxy: ext_proc hands header bytes over from Envoy unparsed, so
// on that path this function is the only bound on what reaches a terminal and the
// archive. Claude Code itself percent-encodes the value to printable ASCII before
// sending it, so none of this costs a real id anything.
func validSubagentID(id string) bool {
	return id != "" && len(id) <= maxClientLen && !hasControlRunes(id)
}
