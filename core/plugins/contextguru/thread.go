package contextguru

import (
	"log/slog"

	agentall "github.com/rossoctl/context-guru/agent/all"
	"github.com/rossoctl/context-guru/thread"
	"github.com/rossoctl/cortex/core/pipeline"
)

// resolveThread finds the thread of this request in its session (context-guru's package thread),
// once, and keeps it on pctx (reqThread). It is the rule context-guru-proxy uses
// (thread.ResolveSession):
//   - A request whose agent names itself is that agent's thread, "a:<agent id>". The agent id is
//     pctx.Subagent(): Claude Code's X-Claude-Code-Agent-Id, a Bob child's task id, or a Codex or
//     OpenCode subagent's own id.
//   - Every other request goes by prefix continuity: a request that continues a thread's history
//     is on that thread, and one that continues none starts its own ("p:<hash>"). A session's
//     one-shot calls (a title, a permission check) so get threads of their own.
//   - A request with no usable message list is the session's primary thread, as before threads.
//
// OnRequest calls it before the keeper's Arrive, so every user of the thread reads this one
// answer: the keeper's key (keeperKey), the 1-hour choice (oneHourFirstFor), the engine
// (apply.Opts.ThreadOf, threadOfFor), the accountant (accountingThread) and OnFinish (finishEngine,
// recordFor). Each Resolve call records the request as its thread's newest turn, so it must run
// once per request.
//
// body is the request as this plugin received it, before the engine rewrites it: a rewritten body
// would break the very prefix continuity matches on. scope is the Cortex session id (sessionID):
// the tracker only needs one stable key per session.
//
// It fails open: ResolveSession recovers a panic and gives the primary thread, and the panic is
// only logged.
func (p *ContextGuru) resolveThread(pctx *pipeline.Context, scope string, body []byte) (thread.Result, bool) {
	if p.threads == nil || scope == "" {
		return thread.Result{}, false
	}
	if rt := pipeline.GetState[reqThread](pctx, reqThreadStateKey); rt != nil {
		return rt.res, true
	}
	agentID, ok := thread.HeaderAgentID(pctx.Subagent())
	if !ok {
		// No session attribute: it comes from a request header, and CodeQL's go/clear-text-logging
		// fails the build on it. The fallback is visible without it.
		slog.Debug("context-guru thread header refused; using prefix continuity. " +
			"Accounting keeps the subagent id, so it names this agent apart from the engine and the keeper")
	}
	// Tenant "": one process, one deployment (oneHourFirstFor passes "" to the keeper the same way).
	res, err := p.threads.ResolveSession("", scope, agentID, body, agentall.IsClientCompaction(pctx.Headers, body))
	if err != nil {
		slog.Error("context-guru recovered from a panic in thread identification; using the session", "err", err)
	}
	pipeline.SetState(pctx, reqThreadStateKey, &reqThread{res: res})
	return res, true
}

// requestThread is the thread resolveThread kept on pctx, and false when it did not run (no
// session, or a request OnRequest never saw).
func requestThread(pctx *pipeline.Context) (thread.Result, bool) {
	if rt := pipeline.GetState[reqThread](pctx, reqThreadStateKey); rt != nil {
		return rt.res, true
	}
	return thread.Result{}, false
}

// threadOfFor is apply.Opts.ThreadOf: it gives the engine the thread resolveThread already found,
// and calls Resolve no second time. nil when there is none, so the engine uses the primary thread.
func threadOfFor(pctx *pipeline.Context) func(session string) thread.Result {
	res, ok := requestThread(pctx)
	if !ok {
		return nil
	}
	return func(string) thread.Result { return res }
}

// reqThread holds, on pctx, the thread resolveThread found for this request.
type reqThread struct{ res thread.Result }

// reqThreadStateKey is this plugin's key for reqThread (see pipeline.SetState).
const reqThreadStateKey = "context-guru-request-thread"

// accountingThread maps the engine's thread to the accountant thread that pctx.SetAccountingThread
// takes, or ok false to leave core/accounting.ThreadOf's default rule in place.
//
//   - The primary thread ("") keeps the default: "main".
//   - A thread an agent named with its header keeps the default too: the subagent id, with its
//     parent from X-Claude-Code-Parent-Agent-Id or "main". The engine knows no parent for it.
//   - A prefix-continuity thread is the engine's id. When it continues another thread by content,
//     that thread is its parent, and the primary thread's accountant id is "main".
func accountingThread(res thread.Result) (id, parent string, ok bool) {
	if res.ID == thread.Primary || res.Source == thread.SourceHeader {
		return "", "", false
	}
	if from, inherits := res.InheritsFrom(); inherits {
		parent = from
		if parent == thread.Primary {
			parent = "main"
		}
	}
	return res.ID, parent, true
}

// cgThread holds, on pctx, the engine session key (Trace.Session) and the thread of this
// request, so OnFinish (finishEngine) writes under the pair the engine read from
// (apply.Opts.ThreadOf's host contract).
type cgThread struct{ session, id string }

// cgThreadStateKey is this plugin's key for cgThread (see pipeline.SetState).
const cgThreadStateKey = "context-guru-thread"
