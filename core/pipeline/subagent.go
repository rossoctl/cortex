package pipeline

import "strings"

// ClaudeCodeAgentIDHeader is the request header Claude Code sets on the requests of an
// agent other than the main conversation, naming that agent. Verified on
// claude-cli/2.1.302: the CLI sets it from any non-main agent context that holds an id —
// a subagent's, and an agent-team teammate's — and the main conversation's requests do
// not carry it. A teammate is NOT a subagent to the rest of this package: the CLI writes
// the cc_is_subagent marker only for agentType "subagent", so a teammate's rows have this
// header and Inference.AgentRole "main". For a subagent the value is the same id the
// CLI names its transcript after —
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
const ClaudeCodeAgentIDHeader = "X-Claude-Code-Agent-Id"

// ClaudeCodeParentAgentIDHeader is the request header Claude Code sets on a subagent's
// requests when another AGENT started it, naming that agent. Verified on
// claude-cli/2.1.302: a subagent's context takes its parent's agentId at spawn, and the
// main conversation has none, so the header appears only from the second level down —
// a subagent a subagent started, or one an agent-team teammate started. A subagent the
// main conversation started sends ClaudeCodeAgentIDHeader alone.
//
// That is why there is no value naming the main conversation: its parent IS the
// session, so a reader building the tree takes an absent parent to mean "the session
// this row is filed in". The same id is the parentAgentId field of the child's
// agent-<id>.meta.json, written under the same rule.
//
// Not a session id header either, for the reason ClaudeCodeAgentIDHeader gives.
const ClaudeCodeParentAgentIDHeader = "X-Claude-Code-Parent-Agent-Id"

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
	c.resolveSubagents()
	return c.subagent
}

// ParentSubagent returns the agent that started this request's subagent: the value of
// X-Claude-Code-Parent-Agent-Id as the client sent it, or "" when there is none to
// record. Read with Subagent and pinned with it, under the same rules.
//
// "" is the common answer and says the parent is the CONVERSATION, not that it is
// unknown: Claude Code sends the header only when the spawner was itself an agent, so
// on a subagent the main conversation started — every subagent there is, in a session
// that nests none — it is absent by design. The other three empty cases are Subagent's:
// a non-subagent request, a row with no headers, and a value validSubagentID refused.
//
// Recorded independently of Subagent rather than dropped when Subagent is empty. A
// parent with no child is not a shape the CLI produces, so if one ever appears it is
// evidence of a client this code was not written against, and keeping it is what lets
// someone see that.
func (c *Context) ParentSubagent() string {
	c.resolveSubagents()
	return c.parentSubagent
}

// resolveSubagents fills Subagent's and ParentSubagent's memo from one read of Headers,
// so the two answers always describe the same set of headers.
func (c *Context) resolveSubagents() {
	if c.subagentParsed {
		return
	}
	c.subagentParsed = true
	if c.Headers == nil {
		return
	}
	c.subagent = subagentHeader(c.Headers.Get(ClaudeCodeAgentIDHeader))
	c.parentSubagent = subagentHeader(c.Headers.Get(ClaudeCodeParentAgentIDHeader))
}

// subagentHeader is a header value with its padding trimmed and checked by
// validSubagentID, or "" when it fails the check.
//
// PADDING IS SP AND HTAB ONLY — RFC 9110's optional whitespace around a field value —
// and not strings.TrimSpace, which also strips \n, \r, \v, \f and U+0085 from either
// end. Those are control characters, not padding: trimming them would hand
// validSubagentID a clean id and record "a1a2…\n" as "a1a2…", which is sanitising by
// another route. Left in place, they reach the check and refuse the value, which is
// the rule validSubagentID states.
func subagentHeader(v string) string {
	if id := strings.Trim(v, " \t"); validSubagentID(id) {
		return id
	}
	return ""
}

// validSubagentID reports whether a client-supplied subagent id — either header's — is
// safe to record.
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
