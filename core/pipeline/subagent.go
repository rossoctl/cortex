package pipeline

import (
	"net/http"
	"strings"

	"github.com/rossoctl/cortex/core/model"
)

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

// BobTaskIDHeader and BobParentTaskIDHeader are model's, aliased here: session reads
// them too, and session must not import pipeline (#1502). See their doc in core/model.
const (
	BobTaskIDHeader       = model.BobTaskIDHeader
	BobParentTaskIDHeader = model.BobParentTaskIDHeader
)

// Subagent returns the Claude Code subagent this request came from: the value of
// X-Claude-Code-Agent-Id as the client sent it, or "" when there is none to record.
//
// "" MEANS NOTHING WAS STATED, and there are four ways to get it: a main-conversation
// request, a client that is not Claude Code, a row with no request headers (an opaque
// tunnel, a transparent connection), and a value model.SubagentHeader refused. A reader
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
// a non-subagent request, a row with no headers, and a value model.SubagentHeader refused.
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
	if c.subagent != "" {
		return
	}
	// A Bob child: its own task id is the subagent.
	if own, _ := model.BobChildTask(c.Headers); own != "" {
		c.subagent = own
		c.bobChild = true
		return
	}
	own, parent := otherClientSubagent(c.Headers)
	if c.subagent = subagentHeader(own); c.subagent != "" {
		c.parentSubagent = subagentHeader(parent)
	}
}

// The headers Codex and OpenCode name a subagent with. context-guru reads the same headers
// (agent/codex, agent/opencode, and agent.Client.SubagentID). core/pipeline may not import
// context-guru (core/accounting's boundary test forbids it in accounting's whole dependency
// closure, and accounting imports this package), so the rule is copied here.
// plugins/contextguru's TestSubagentMatchesContextGuru fails when the two disagree.
const (
	// CodexThreadIDHeader is the thread that made a Codex request. A subagent has its own.
	CodexThreadIDHeader = "Thread-Id"
	// CodexParentThreadIDHeader names the parent thread. Only a Codex subagent sends it.
	CodexParentThreadIDHeader = "X-Codex-Parent-Thread-Id"
	// OpenCodeSessionIDHeader is the OpenCode session of the request. A subagent has its own.
	OpenCodeSessionIDHeader = "X-Opencode-Session-Id"
	// OpenCodeParentSessionIDHeader names the parent session. Only an OpenCode subagent sends it.
	OpenCodeParentSessionIDHeader = "X-Opencode-Parent-Session-Id"
)

// otherClientSubagent is the subagent a Codex or an OpenCode request names, and its parent, or
// two "":
//   - Codex (a User-Agent or Originator that starts with "codex"): its own Thread-Id, when it
//     names a parent thread. The ids have no vendor prefix, so they are read only for Codex.
//     The parent is X-Codex-Parent-Thread-Id as sent. Codex names the main conversation's thread
//     there too, and no Codex header tells that thread apart (its Session-Id is a cache key, not
//     the root thread id). So a reader takes a parent that no row of the session has as its
//     subagent to mean the main conversation.
//   - OpenCode, for a client that is not Codex, Claude Code, Bob or pi: its own session id, when it
//     names a parent session. The parent is X-Opencode-Parent-Session-Id, or "" when it is the
//     session the row is filed in (X-Session-Id, OpenCode's affinity id): that parent is the main
//     conversation, which ParentSubagent's "" already means.
func otherClientSubagent(h http.Header) (own, parent string) {
	if hasPrefixFold(h.Get("User-Agent"), "codex") || hasPrefixFold(h.Get("Originator"), "codex") {
		parent = strings.TrimSpace(h.Get(CodexParentThreadIDHeader))
		if parent == "" {
			return "", ""
		}
		return strings.TrimSpace(h.Get(CodexThreadIDHeader)), parent
	}
	parent = strings.TrimSpace(h.Get(OpenCodeParentSessionIDHeader))
	if namedOtherClient(h) || parent == "" {
		return "", ""
	}
	// X-Session-Id is session.SessionIDHeader; session imports pipeline, so it is spelled here.
	if parent == strings.TrimSpace(h.Get("X-Session-Id")) {
		parent = ""
	}
	return strings.TrimSpace(h.Get(OpenCodeSessionIDHeader)), parent
}

// namedOtherClient reports a client that context-guru names before it reads OpenCode's headers:
// Claude Code (a User-Agent with claude-code or claude-cli), Bob (bobshell/) and pi
// (X-Context-Guru-Client: pi).
func namedOtherClient(h http.Header) bool {
	ua := strings.ToLower(h.Get("User-Agent"))
	return strings.Contains(ua, "claude-code") || strings.Contains(ua, "claude-cli") ||
		strings.HasPrefix(strings.TrimSpace(ua), "bobshell/") ||
		strings.EqualFold(strings.TrimSpace(h.Get("X-Context-Guru-Client")), "pi")
}

func hasPrefixFold(s, prefix string) bool {
	s = strings.TrimSpace(s)
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// SubagentKind returns the kind of a Bob child task: SubagentKindSubtask when the
// request's tool list has end_subtask (Bob gives that tool only to a subtask), else
// SubagentKindSubagent. It is "" on every request that is not a Bob child, Claude Code's
// subagents included.
//
// Not memoized: the inference parser fills the tool list after ResolveClient pins
// Subagent, so the answer is read at each call. With no inference parser in the
// pipeline, a Bob child is SubagentKindSubagent.
func (c *Context) SubagentKind() string {
	c.resolveSubagents()
	if !c.bobChild {
		return ""
	}
	if inf := c.Extensions.Inference; inf != nil {
		for _, t := range inf.Tools {
			if t.Name == "end_subtask" {
				return SubagentKindSubtask
			}
		}
	}
	return SubagentKindSubagent
}

// subagentHeader is model.SubagentHeader. See its doc there.
func subagentHeader(v string) string { return model.SubagentHeader(v) }
