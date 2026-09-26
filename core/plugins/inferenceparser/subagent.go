package inferenceparser

import (
	"strings"

	"github.com/rossoctl/cortex/core/pipeline"
)

// billingHeaderPrefix and subagentMarker are what Claude Code writes as the FIRST LINE of its
// system prompt — a pseudo-header, inlined in the prompt rather than sent as an HTTP header:
//
//	main:     x-anthropic-billing-header: cc_version=2.1.270.119; cc_entrypoint=cli;
//	subagent: x-anthropic-billing-header: cc_version=2.1.270.658; cc_entrypoint=cli; cc_is_subagent=true;
//
// Both strings are compared lowercased, because a pseudo-header's casing is not something the
// client promises.
const (
	billingHeaderPrefix = "x-anthropic-billing-header:"
	subagentMarker      = "cc_is_subagent=true"
)

// agentRole reports which caller under one client session made a request: its interactive
// conversation, or a subagent that conversation spawned. Empty when the request says nothing,
// which is every client that is not Claude Code — see pipeline.AgentRole for what a reader owes
// that case.
//
// WHY THIS IS WORTH PARSING AT ALL: a session id is not a thread. Claude Code sends a
// conversation, its subagents and its own one-shot completions under one
// X-Claude-Code-Session-Id, and a consumer that has to pick out the conversation — abctl's
// CONTEXT gauge — was left inferring it from the message count, which the two populations
// overlap on (subagent turns at 177 and 186 messages against main threads at 188 and 288).
// Measured on 19 probed requests across three live sessions, the marker separated them without
// exception, and it survives a compaction, which rewrites the messages and leaves the system
// prompt alone.
//
// THE FIRST LINE ONLY, AND THE PREFIX IS REQUIRED. Both are the same guard: a request body can
// contain this marker as ordinary text and must not be read as declaring it. Claude Code's own
// permission monitor is the live example — it is a one-shot whose user message carries a rendered
// transcript of the agent under review, so an agent that so much as discusses subagent detection
// quotes the marker into the next monitor call's payload. A bare Contains over the body would
// misfire on exactly the calls this field exists to classify.
//
// THE FIRST system MESSAGE, not Messages[0]. The Anthropic arm prepends the top-level `system`
// field, so it is index 0 there; an OpenAI-dialect client can put a system message at any index,
// and /v1/completions has no messages at all. Scanning for the role costs nothing and is right
// for all three.
//
// IF A REAL HTTP HEADER EVER CARRIES THIS, prefer it: pipeline.Context.Headers is in hand at the
// call site. On every path measured the line arrived inside the prompt, so that is what this
// reads, and a header source can be added beside it without changing what callers see.
func agentRole(msgs []pipeline.InferenceMessage) pipeline.AgentRole {
	for _, m := range msgs {
		if m.Role != "system" {
			continue
		}
		line, _, _ := strings.Cut(m.Content, "\n")
		line = strings.ToLower(strings.TrimSpace(line))
		if !strings.HasPrefix(line, billingHeaderPrefix) {
			return ""
		}
		if strings.Contains(line, subagentMarker) {
			return pipeline.AgentRoleSubagent
		}
		return pipeline.AgentRoleMain
	}
	return ""
}
