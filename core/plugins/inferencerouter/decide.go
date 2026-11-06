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

// decideServer is the server for a request, "" for not routed, the session pin it
// leaves, and whether the session's loaded pin, had, is kept (renewed). session is
// whether the request has a session to pin: an id that is not synthetic, and a pin
// store. over is the session's sessions: entry when overridden, and had the pin the
// store holds for the session when pinned. unpinned is decideUnpinned for the
// request, and is called only when no override applies and the agent holds no pin.
// The returned pinning has no store, key or agent: the caller sets them.
//
// Without a session to pin, the agent's choice applies and nothing is pinned: a
// synthetic session's history is many conversations', not this one's. Otherwise, in
// order:
//
//  1. A sessions: override is the user's explicit move: it beats the pin and every
//     unpinned rule, and the pin follows it, so removing the entry later does not
//     move the session back. A session already pinned to the override keeps its pin
//     (pinExisting). A session pinned elsewhere moves (pinMoved, from its old
//     server). A session that held no pin of its agent's is pinNew.
//  2. A session its agent already pinned uses its pin, "not routed" included, so
//     routing an agent later does not move the sessions it already has running. Every
//     request renews the pin. A pin to a server no longer configured is still the
//     answer: OnRequest denies the request rather than move the conversation.
//  3. A pin that is another agent's is neither followed nor overwritten: the request
//     goes where unpinned says, and pins nothing.
//  4. Otherwise unpinned decides, and the pin comes back pinNew for OnRequest to
//     settle by where the request went (see the package doc), or pinNone when the
//     request decides nothing.
//
// Two first requests of one session racing can both miss and both store. They store
// the same outcome unless a reload lands between them, which is the case the pin
// cannot rule out and does not need to.
func decideServer(agent, choice string, session bool, over string, overridden bool, had pin, pinned bool,
	unpinned func() (server, turn string, decides bool)) (server string, pn pinning, keep bool) {
	if !session {
		return choice, pinning{state: pinNone}, false
	}
	if overridden {
		pn = pinning{state: pinNew, override: true}
		if pinned && had.belongsTo(agent) {
			if had.server == over {
				pn.state, keep = pinExisting, true
			} else {
				pn.state, pn.from = pinMoved, had.server
			}
		}
		return over, pn, keep
	}
	if pinned {
		if !had.belongsTo(agent) {
			server, _, _ := unpinned()
			return server, pinning{state: pinNone}, false
		}
		return had.server, pinning{state: pinExisting}, true
	}
	server, turn, decides := unpinned()
	if !decides {
		return server, pinning{state: pinNone, turn: turn}, false
	}
	return server, pinning{state: pinNew, turn: turn}, false
}

// decideSettle is what a routed request's outcome does to its session's pin: settle
// it to to, the server the request went to or "" for where the client sent it, or,
// when settle is false, forgo it. state is the pin's state from decideServer, and
// settle and forgo change only a pinNew or pinMoved pin. redirectFailed is whether
// Redirect failed, redirected whether it moved the request (false under on_error:
// observe), and pathFailed whether SetRedirectPath failed. In order:
//
//  1. A request whose Redirect failed went nowhere, so it pins nothing: the next
//     request decides again.
//  2. A request that stayed where the client sent it settles there, so a session
//     that started under observe stays put once enforce is on. A move an override
//     would make is forgone: nothing moved, and the pin stands until the move,
//     once enforce is on, replaces it.
//  3. A request whose path SetRedirectPath refused went nowhere, as in rule 1.
//  4. Otherwise the session settles on server.
func decideSettle(state, server string, redirectFailed, redirected, pathFailed bool) (to string, settle bool) {
	switch {
	case redirectFailed:
		return "", false
	case !redirected:
		return "", state != pinMoved
	case pathFailed:
		return "", false
	}
	return server, true
}
