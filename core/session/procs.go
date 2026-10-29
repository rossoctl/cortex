package session

import (
	"slices"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// ProcSpan is when one process named a session through its own header: its first such
// request and its last. Link reads it; the fold keeps one per process per session.
type ProcSpan struct {
	First time.Time `json:"first"`
	Last  time.Time `json:"last"`
}

// maxProcsPerSession bounds a session's procs map. A session is named by a handful of
// processes — one per launch, resume or parallel run — so past this the oldest is dropped
// rather than the map growing with whatever a client asserts.
const maxProcsPerSession = 64

// applyProcs records e in procs when e is a request that NAMED its session (a session header
// carried the id) from a known process, into a conversation (never a bucket). Shared by appendLocked and SummaryFold, so a resident
// session and an archived one can never disagree on which processes named them.
//
// Requests only: a response, a tunnel's close or a denial is not the process naming anything,
// and a response arriving after a later claim would stretch the span past it.
func applyProcs(procs map[string]ProcSpan, sessionID string, e *pipeline.SessionEvent) map[string]ProcSpan {
	if e.SessionHeader == "" || e.Phase != pipeline.SessionRequest || !IsConversation(sessionID) {
		return procs
	}
	for _, p := range []string{e.Proc, e.ClientProc} {
		if p != "" {
			procs = applyProc(procs, p, e.At)
		}
	}
	return procs
}

// applyProc widens p's span in procs to cover at.
func applyProc(procs map[string]ProcSpan, p string, at time.Time) map[string]ProcSpan {
	sp, ok := procs[p]
	if !ok {
		if procs == nil {
			procs = make(map[string]ProcSpan, 1)
		}
		if len(procs) >= maxProcsPerSession {
			delete(procs, oldestProc(procs))
		}
		sp.First = at
	}
	if at.After(sp.Last) {
		sp.Last = at
	}
	if at.Before(sp.First) {
		sp.First = at
	}
	procs[p] = sp
	return procs
}

func oldestProc(procs map[string]ProcSpan) string {
	var oldest string
	for p, sp := range procs {
		if oldest == "" || sp.Last.Before(procs[oldest].Last) {
			oldest = p
		}
	}
	return oldest
}

// mergeProcs is earlier and later as one map, each process's span widened to cover both.
// Neither argument is modified.
func mergeProcs(earlier, later map[string]ProcSpan) map[string]ProcSpan {
	if len(earlier) == 0 {
		return later
	}
	out := make(map[string]ProcSpan, len(earlier)+len(later))
	for p, sp := range earlier {
		out[p] = sp
	}
	for p, sp := range later {
		if e, ok := out[p]; ok {
			if e.First.Before(sp.First) {
				sp.First = e.First
			}
			if e.Last.After(sp.Last) {
				sp.Last = e.Last
			}
		}
		out[p] = sp
	}
	return out
}

// applyOwner is the agent that first NAMED the session through its own header: the
// session's agent ahead of every other label, as a claim owner was before attribution left
// the store. First-wins, like Title and agents.
func applyOwner(owner, sessionID, agentName string, e *pipeline.SessionEvent) string {
	if owner != "" || e.SessionHeader == "" || agentName == "" || !IsConversation(sessionID) {
		return owner
	}
	return agentName
}

// pickAgent is SessionSummary.Agent: the label owner first sent, else the first recognised
// agent's, else the first unrecognised client's. earlier is the history before this entry,
// searched first. The default and pending buckets name no agent.
func pickAgent(sessionID, owner string, earlier, agents []sessionAgent) string {
	if !IsConversation(sessionID) {
		return ""
	}
	if owner != "" {
		if l := agentLabelIn(earlier, owner); l != "" {
			return l
		}
		return agentLabelIn(agents, owner)
	}
	if l := firstAgentLabel(earlier, agents, true); l != "" {
		return l
	}
	return firstAgentLabel(earlier, agents, false)
}

// procsList is a procs map's keys, sorted, for SessionSummary.Procs.
func procsList(procs map[string]ProcSpan) []string {
	if len(procs) == 0 {
		return nil
	}
	out := make([]string, 0, len(procs))
	for p := range procs {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}
