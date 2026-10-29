package attribution

import (
	"net/http"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/session"
)

// Ported from session's process_test.go and affinity_test.go, which this package replaced:
// the rules they pinned that are not about adoption, with their expectations unchanged.

// bobReq is a Bob request, naming task when it is not "".
func bobReq(task string) http.Header {
	h := req(bobUA, "")
	if task != "" {
		h.Set(session.BobSessionHeader, task)
	}
	return h
}

// A launcher and its worker are one agent: when the launcher names the session, the worker's
// header-less calls go there, and follow the launcher when it names the next.
func TestResolve_AWorkerJoinsItsLaunchersSession(t *testing.T) {
	a, clk := newTest()
	launcher, worker := pchain(800), pchain(820, 800)
	a.Resolve(bobReq("task-1"), launcher)
	record(a, clk, "task-1")
	if got := a.Resolve(bobReq(""), worker).Session; got != "task-1" {
		t.Fatalf("a worker under a launcher in task-1: %q, want task-1", got)
	}
	a.Resolve(bobReq("task-2"), launcher)
	record(a, clk, "task-2")
	if got := a.Resolve(bobReq(""), worker).Session; got != "task-2" {
		t.Errorf("after the launcher moved on: %q, want task-2", got)
	}
}

// A tool keeps the session it was started under, also when the launcher's worker started it.
func TestResolve_AToolUnderAWorkerIsBoundToTheLaunchersSession(t *testing.T) {
	a, clk := newTest()
	a.Resolve(bobReq("task-1"), pchain(800))
	record(a, clk, "task-1")
	a.Resolve(bobReq(""), pchain(820, 800)) // the worker, seen first
	tool := pchain(900, 820, 800)
	if got := a.Resolve(nil, tool).Session; got != "task-1" {
		t.Fatalf("a tool under the worker: %q, want task-1", got)
	}
	a.Resolve(bobReq("task-2"), pchain(800))
	record(a, clk, "task-2")
	if got := a.Resolve(nil, tool).Session; got != "task-1" {
		t.Errorf("after the launcher moved on: %q, want task-1, the session the tool started under", got)
	}
}

// A tool still writing to an agent's old session does not make it the agent's newest again,
// for the agent's own header-less calls or for a tool started after the move.
func TestResolve_AToolWritingToAnOldSessionDoesNotMakeItTheNewest(t *testing.T) {
	a, clk := newTest()
	claude := pchain(100, 50)
	a.Resolve(req(claudeUA, "s1"), claude)
	record(a, clk, "s1")
	watcher := pchain(300, 200, 100, 50) // gh run watch ← bash ← claude
	if got := a.Resolve(nil, watcher).Session; got != "s1" {
		t.Fatalf("tool: %q, want s1", got)
	}
	record(a, clk, "s1")
	a.Resolve(req(claudeUA, "s2"), claude) // /clear
	record(a, clk, "s2")
	clk.advance(time.Second)
	a.Touch("s1", watcher) // the watcher, still in s1
	record(a, clk, "s1")

	if got := a.Resolve(req(claudeUA, ""), claude).Session; got != "s2" {
		t.Errorf("the agent's own header-less call: %q, want s2", got)
	}
	if got := a.Resolve(nil, pchain(301, 200, 100, 50)).Session; got != "s2" {
		t.Errorf("a tool started after the move: %q, want s2", got)
	}
}

// The same holds for the claims a worker answers with from its launcher.
func TestResolve_AToolWritingToALaunchersOldSessionDoesNotMoveItsWorker(t *testing.T) {
	a, clk := newTest()
	launcher, worker := pchain(800), pchain(820, 800)
	a.Resolve(bobReq("task-1"), launcher)
	record(a, clk, "task-1")
	a.Resolve(bobReq(""), worker)
	tool := pchain(900, 820, 800)
	if got := a.Resolve(nil, tool).Session; got != "task-1" {
		t.Fatalf("tool: %q, want task-1", got)
	}
	a.Resolve(bobReq("task-2"), launcher)
	record(a, clk, "task-2")
	clk.advance(time.Second)
	a.Touch("task-1", tool)
	record(a, clk, "task-1")
	if got := a.Resolve(bobReq(""), worker).Session; got != "task-2" {
		t.Errorf("the worker: %q, want task-2", got)
	}
}

// A process that names its own session — OpenCode's service — first seen under another
// session's tree is never filed under that session once it has named its own, even before
// its own is recorded.
func TestResolve_AProcessThatNamesASessionIsNeverBoundToAnother(t *testing.T) {
	a, clk := newTest()
	a.Resolve(req(claudeUA, "outer"), pchain(100))
	record(a, clk, "outer")
	svc := pchain(500, 100)
	if got := a.Resolve(nil, svc).Session; got != "outer" {
		t.Fatalf("first seen as the outer agent's tool: %q, want outer", got)
	}
	a.Resolve(req("", "own"), svc)
	if got := a.Resolve(nil, svc).Session; got != "own" {
		t.Errorf("a process that named its own session: %q, want own", got)
	}
	record(a, clk, "own")
	if got := a.Resolve(nil, svc).Session; got != "own" {
		t.Errorf("once its session is recorded: %q, want own", got)
	}
}

// A tool of a nested agent answers with the OUTER session: the topmost claim wins.
func TestResolve_ChainSessionSkipsANestedAgentsOwnSession(t *testing.T) {
	a, clk := newTest()
	a.Resolve(req(claudeUA, "s1"), pchain(100, 50))
	record(a, clk, "s1")
	if got := a.Resolve(req(claudeUA, ""), pchain(100, 50)).ChainSession; got != "" {
		t.Errorf("the agent itself: ChainSession %q, want none above it", got)
	}
	a.Resolve(req(claudeUA, "inner"), pchain(400, 200, 100, 50))
	record(a, clk, "inner")
	if got := a.Resolve(nil, pchain(500, 450, 400, 200, 100, 50)).ChainSession; got != "s1" {
		t.Errorf("a tool of a nested agent: ChainSession %q, want the outer s1", got)
	}
}

// A worker below its launcher is the same agent, not its child: the launcher holds the
// worker's own claim, which must not count as an ancestor's.
func TestResolve_ChainSessionAnAgentsOwnLauncherIsNotAnAncestor(t *testing.T) {
	a, clk := newTest()
	launcher := pchain(100, 50)
	a.Resolve(req(claudeUA, ""), launcher)
	worker := pchain(110, 100, 50)
	a.Resolve(req(claudeUA, "s1"), worker)
	record(a, clk, "s1")
	got := a.Resolve(req(claudeUA, "s2"), worker)
	record(a, clk, "s2")
	if got.ChainSession != "" {
		t.Errorf("worker: ChainSession %q, want none above the agent", got.ChainSession)
	}
}

// Two of one agent's per-process pending buckets are one agent for the "two agents are
// active" answer, exactly like the agent-wide bucket.
func TestResolve_PerProcessPendingBucketsCountAsOneAgent(t *testing.T) {
	a, clk := newTest()
	record(a, clk, session.PendingID("claude-code", "1.1"))
	record(a, clk, session.PendingID("claude-code", "2.2"))
	if got := a.Resolve(req("", ""), nil).Session; got != "" {
		t.Errorf("two pending buckets of one agent: %q, want \"\" (one agent)", got)
	}
	record(a, clk, session.PendingSessionID("bob"))
	if got := a.Resolve(req("", ""), nil).Session; got != session.DefaultSessionID {
		t.Errorf("with a second agent: %q, want default", got)
	}
}

// Client affinity answers with the agent's most recently active session, and falls back to its
// pending bucket once that session is gone.
func TestResolve_ClientAffinityPicksTheNewestLiveSession(t *testing.T) {
	a, clk := newTest()
	a.Resolve(req(claudeUA, "a"), nil)
	a.Resolve(req(claudeUA, "b"), nil)
	record(a, clk, "a")
	record(a, clk, "b")
	if got := a.Resolve(req(claudeUA, ""), nil).Session; got != "b" {
		t.Fatalf("got %q, want b (updated last)", got)
	}
	record(a, clk, "a")
	if got := a.Resolve(req(claudeUA, ""), nil).Session; got != "a" {
		t.Fatalf("got %q, want a (updated last)", got)
	}
	a.Evicted("a")
	a.Evicted("b")
	if got := a.Resolve(req(claudeUA, ""), nil); got.RecordID() != session.PendingSessionID("claude-code") {
		t.Errorf("every session gone: %+v, want the pending bucket", got)
	}
}

// A full process table drops processes that never named a session before an agent's own,
// however old the agent's is.
func TestPruneProcs_EvictsToolsBeforeAgents(t *testing.T) {
	a, clk := newTest()
	start := clk.t
	clk.t = start.Add(-time.Hour)
	agent := []Proc{{PID: maxProcs, Start: int64(maxProcs) * 1000}}
	a.Resolve(req("", "agent-session"), agent)
	clk.t = start
	for pid := int32(1); pid < maxProcs; pid++ {
		a.Resolve(nil, []Proc{{PID: pid, Start: int64(pid) * 1000}})
	}
	a.Resolve(nil, []Proc{{PID: maxProcs + 1, Start: int64(maxProcs+1) * 1000}}) // forces a prune
	a.mu.Lock()
	n, survived := len(a.procs), a.procs[agent[0].key()] != nil
	a.mu.Unlock()
	if want := maxProcs - maxProcs/4 + 1; n != want {
		t.Fatalf("%d processes after the trigger insert, want %d: no prune ran", n, want)
	}
	if !survived {
		t.Error("the agent process was evicted; pruning must drop tools first")
	}
	if a.ProcessHints(0) != nil || a.ProcessHints(-1) != nil {
		t.Error("ProcessHints with no room returned hints")
	}
}

// A renamed session's claims and owner follow it (the in-cluster A2A default → contextId).
func TestRekeyed_ClaimsFollowTheSession(t *testing.T) {
	a, clk := newTest()
	a.Resolve(req(claudeUA, "ctx-old"), nil)
	record(a, clk, "ctx-old")
	a.Rekeyed("ctx-old", "ctx-new")
	if got := a.Resolve(req(claudeUA, ""), nil).Session; got != "ctx-new" {
		t.Errorf("after the rename: %q, want ctx-new", got)
	}
}

// A process that names its own sessions is no other session's: with all of them gone it falls
// back rather than joining the session of the agent above it (rule 3 in process.go).
func TestResolve_AClaimerWhoseSessionsAreGoneFallsBack(t *testing.T) {
	a, clk := newTest()
	a.Resolve(req(claudeUA, "outer"), pchain(100))
	record(a, clk, "outer")
	svc := pchain(500, 100)
	a.Resolve(nil, svc) // first seen as the outer agent's tool, and bound to outer
	a.Resolve(req("", "own"), svc)
	record(a, clk, "own")
	a.Evicted("own")
	if got := a.Resolve(nil, svc).Session; got == "outer" {
		t.Errorf("a process whose own sessions are gone was filed under the outer agent's %q", got)
	}
}

// A tool started by another tool takes its parent's session on first sight, and keeps it after
// the parent exits — for a nohup'd child, past the agent naming a new session.
func TestResolve_AToolOfABoundToolKeepsItsSessionAfterItsParentExits(t *testing.T) {
	a, clk := newTest()
	claude := pchain(100)
	a.Resolve(req(claudeUA, "s1"), claude)
	record(a, clk, "s1")
	parent := pchain(300, 100)
	a.Resolve(nil, parent)
	a.Resolve(req(claudeUA, "s2"), claude)
	record(a, clk, "s2")
	child := pchain(400, 300, 100)
	if got := a.Resolve(nil, child).Session; got != "s1" {
		t.Fatalf("child of a tool bound to s1: %q, want s1", got)
	}
	a.mu.Lock()
	delete(a.procs, parent[0].key()) // the parent exited and was pruned
	a.mu.Unlock()
	if got := a.Resolve(nil, child).Session; got != "s1" {
		t.Errorf("after its parent exited: %q, want s1, the session it started under", got)
	}
}
