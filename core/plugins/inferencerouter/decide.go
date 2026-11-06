package inferencerouter

import "github.com/rossoctl/cortex/core/pipeline"

// decideUnpinned is the server for a request in a session its agent holds no pin on,
// and whether the request decides the session's pin. ext is the request's parse, nil
// when no parser read it as inference. lastServer is the server the session's history
// shows the agent already uses, and is called only when the agent is routed. In order:
//
//  1. An agent that is not routed is left alone, whatever its history.
//  2. The server the session's history shows the agent already uses.
//  3. A request no parser read as inference goes to the agent's current choice and
//     decides nothing.
//  4. A Claude Code request's own turn (see turnOf): a conversation's opening turn
//     goes to the agent's current choice; a continuation, a conversation the router
//     did not see begin, is not routed; a side request decides nothing, and goes to
//     the agent's current choice when it is a one-shot or was captured, and is not
//     routed when it is a subagent's to a server's host. turn names the last two,
//     for the record.
//  5. The agent's current choice.
func decideUnpinned(choice string, ext *pipeline.InferenceExtension, captured bool, lastServer func() (string, bool)) (server, turn string, decides bool) {
	if choice == "" {
		return "", "", true
	}
	if server, ok := lastServer(); ok {
		return server, "", true
	}
	if ext == nil {
		return choice, "", false
	}
	switch turnOf(ext) {
	case turnContinuation:
		return "", turnContinuation, true
	case turnAside:
		// A one-shot is sent beside the main conversation and is no part of it, so the
		// agent's server answering it moves nothing. A subagent's request is a
		// conversation of its own, which may be running already, so it is left where
		// Claude Code sent it unless captured: then leaving it would send the user's
		// credential to the provider the agent was routed away from.
		if captured || ext.AgentRole == pipeline.AgentRoleMain {
			return choice, turnAside, false
		}
		return "", turnAside, false
	}
	return choice, "", true
}
