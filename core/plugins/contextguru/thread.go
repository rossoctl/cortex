package contextguru

import (
	"log/slog"

	agentall "github.com/rossoctl/context-guru/agent/all"
	"github.com/rossoctl/context-guru/thread"
	"github.com/rossoctl/cortex/core/pipeline"
)

// threadOf is apply.Opts.ThreadOf for one request: the engine's thread of the session this
// request belongs to (context-guru's package thread). It is the rule context-guru-proxy uses
// (thread.ResolveSession):
//   - A request whose agent names itself is that agent's thread, "a:<agent id>". The agent id is
//     pctx.Subagent() (X-Claude-Code-Agent-Id), the same value keeperKey reads, so the engine's
//     thread and the keeper's key agree on every named agent.
//   - Every other request goes by prefix continuity: a request that continues a thread's history
//     is on that thread, and one that continues none starts its own ("p:<hash>"). A session's
//     one-shot calls (a title, a permission check) so get threads of their own.
//   - A request with no usable message list is the session's primary thread, as before threads.
//
// body is the request as this plugin received it, before the engine rewrites it: a rewritten body
// would break the very prefix continuity matches on.
//
// The engine calls the result once per request (apply's resolveThread), and each call records the
// request as its thread's newest turn. It fails open: ResolveSession recovers a panic and gives the
// primary thread, and the panic is only logged.
func (p *ContextGuru) threadOf(pctx *pipeline.Context, body []byte) func(session string) thread.Result {
	if p.threads == nil {
		return nil
	}
	agentID, ok := thread.HeaderAgentID(pctx.Subagent())
	if !ok {
		slog.Debug("context-guru thread header refused; using prefix continuity",
			"session", pipeline.SanitizeLogValue(pctx.SessionID))
	}
	compaction := agentall.IsClientCompaction(pctx.Headers, body)
	tracker := p.threads
	return func(session string) thread.Result {
		// Tenant "": one process, one deployment (oneHourFirstFor passes "" to the keeper the same way).
		res, err := tracker.ResolveSession("", session, agentID, body, compaction)
		if err != nil {
			slog.Error("context-guru recovered from a panic in thread identification; using the session", "err", err)
		}
		return res
	}
}

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

// cgThread holds, on pctx, the engine thread OnRequest resolved, so OnFinish records the billed
// input under the thread the engine read it from (apply.Opts.ThreadOf's host contract).
type cgThread struct{ id string }

// cgThreadStateKey is this plugin's key for cgThread (see pipeline.SetState).
const cgThreadStateKey = "context-guru-thread"
