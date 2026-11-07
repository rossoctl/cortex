package session

import (
	"net/http"
	"strings"

	"github.com/rossoctl/cortex/core/model"
)

// TurnOf's answers. The inference-router records them as the turn detail on a
// not-routed record, where the request's own turn decided an unpinned Claude Code
// session.
const (
	TurnContinuation = "continuation" // a conversation the router did not see begin
	TurnAside        = "aside"        // a one-shot or a subagent's request, which decides nothing
)

// TurnOf is a Claude Code request's place in its session's conversation, read from
// inference-parser's parse: TurnContinuation for the main conversation with earlier
// assistant turns, TurnAside for any other request, and "" for the main
// conversation's opening turn or a request that states no role.
//
// The role is stated only by Claude Code's system prompt (see
// model.InferenceExtension.AgentRole), and only Claude Code is read this way:
// another agent's earlier turns may have gone to another provider — OpenCode starts
// on Zen and addresses a server later — so its turns say nothing about whether its
// use of the servers began before this request. The main conversation carries its
// tool manifest; the one-shots Claude Code interleaves with it, its title request,
// auto mode's classifier and the permission monitor, carry none, and neither they
// nor a subagent's requests say whether the session began now.
func TurnOf(ext *model.InferenceExtension) string {
	if ext == nil || ext.AgentRole == "" {
		return ""
	}
	if ext.AgentRole != model.AgentRoleMain || len(ext.Tools) == 0 {
		return TurnAside
	}
	for _, m := range ext.Messages {
		if m.Role == "assistant" {
			return TurnContinuation
		}
	}
	return ""
}

// capturedPaths are the paths a captured request is sent to, by how the path the
// agent named ends: the API format, whoever the agent and whatever its provider.
var capturedPaths = []struct{ suffix, path string }{
	{"/chat/completions", "/v1/chat/completions"},
	{"/v1/messages", "/v1/messages"},
	{"/responses", "/v1/responses"},
}

// CapturePath is the server path an inference request addressed to a host that is no
// server is sent to, and whether the inference-router captures it at all: a POST, with
// a path ending in one of capturedPaths. The parse is checked by the caller; see the
// router's capture.
func CapturePath(method, path string) (string, bool) {
	if method != http.MethodPost {
		return "", false
	}
	for _, c := range capturedPaths {
		if strings.HasSuffix(path, c.suffix) {
			return c.path, true
		}
	}
	return "", false
}
