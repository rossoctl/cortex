package tui

import (
	"strings"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// pendingBucket splits a pending bucket's id — pending:<agent>, or pending:<agent>@<pid>.<start>
// for one process's — into the agent and the pid, and reports false for any other id.
//
// A pending bucket is where an agent's requests that name no session collect until one does
// (core/session/affinity.go). The server lists it like any session, with no title and no agent,
// so as a bare row it was "pending:openc…": a truncated id that named nothing and read as
// unfinished. The id stays what every request and header keys on; only its rendering changes.
func pendingBucket(id string) (agent, pid string, ok bool) {
	agent, proc, ok := session.ParsePending(id)
	pid, _, _ = strings.Cut(proc, ".")
	return agent, pid, ok
}

// pendingTitle is what a pending bucket's title says when nothing else names it.
func pendingTitle(pid string) string {
	if pid == "" {
		return "calls outside any session"
	}
	return "calls outside any session · pid " + pid
}

// sessionIDCell is the SESSION cell: the id, or a pending bucket's agent. The agent and not
// agent·pid, because SESSION is 14 columns and "claude-code·89949" would lose the pid to the
// cut anyway; the pid is in the title.
func sessionIDCell(id string, w int) string {
	if agent, _, ok := pendingBucket(id); ok {
		return trunc(sanitizeLabel(agent), w)
	}
	return trunc(id, w)
}

// sessionAgent is the agent a row belongs to: the server's Agent, or — for a pending bucket,
// which the server leaves blank — the agent its id names, by canonical name so a bucket
// recorded under a retired spelling (bob-shell) lands under today's row. The default bucket
// names no agent and stays Other's.
//
// THE ONE PLACE A ROW'S AGENT IS DECIDED. The AGENTS pane, the Other partition, a scope's
// sessions list and the AGENT cell all read it, so a bucket is listed and counted under the
// agent whose spend it is — which the server already attributes by User-Agent — and never
// under Other while its spend sits under its agent.
func sessionAgent(s session.SessionSummary) string {
	if s.Agent != "" {
		return s.Agent
	}
	if a, _, ok := pendingBucket(s.ID); ok {
		return pipeline.AgentName(a)
	}
	return ""
}

// sessionAgentCell is the AGENT cell: sessionAgent, truncated to w.
func sessionAgentCell(s session.SessionSummary, w int) string {
	return trunc(sanitizeLabel(sessionAgent(s)), w)
}
