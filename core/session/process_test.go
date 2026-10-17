package session

import (
	"fmt"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// procTestClock is a store clock tests move by hand, so "most recently active" and the
// ambiguity window never depend on how fast the machine is.
type procTestClock struct{ t time.Time }

func (c *procTestClock) now() time.Time          { return c.t }
func (c *procTestClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newProcStore() (*Store, *procTestClock) {
	clk := &procTestClock{t: time.Unix(1_700_000_000, 0)}
	return New(0, 0, 0, WithClock(clk.now)), clk
}

// pchain is a process chain, nearest first. Each pid's start time is derived from it, so
// the same pid always names the same process unless a test says otherwise.
func pchain(pids ...int32) []Proc {
	out := make([]Proc, len(pids))
	for i, pid := range pids {
		out[i] = Proc{PID: pid, Start: int64(pid) * 1000, Exe: fmt.Sprintf("/bin/p%d", pid)}
	}
	return out
}

// recordIn appends one request to id a second after the last, as the listener does once
// it has resolved where the request goes.
func recordIn(s *Store, clk *procTestClock, id string) {
	clk.advance(time.Second)
	s.Append(id, pipeline.SessionEvent{At: clk.now(), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest})
}

// A tool an agent's shell runs belongs to the session that ran it, and keeps it when the
// agent moves on (/clear); a tool started after the move gets the new one.
func TestSessionForProcess_AToolJoinsTheSessionOfTheAgentThatRanIt(t *testing.T) {
	s, clk := newProcStore()
	defer s.Close()
	claude := pchain(100, 50)
	s.ClaimProcess("s1", "claude-code", claude)
	recordIn(s, clk, "s1")

	curl := pchain(300, 200, 100, 50) // curl ← bash ← claude ← login shell
	if got := s.SessionForProcess(curl, ""); got != "s1" {
		t.Fatalf("tool of the agent: %q, want s1", got)
	}
	s.ClaimProcess("s2", "claude-code", claude)
	recordIn(s, clk, "s2")
	if got := s.SessionForProcess(curl, ""); got != "s1" {
		t.Errorf("a tool still running moved to %q after the agent named s2; want it kept in s1", got)
	}
	if got := s.SessionForProcess(pchain(301, 200, 100, 50), ""); got != "s2" {
		t.Errorf("a tool started after the move: %q, want s2", got)
	}
}

// A nested agent — claude -p run from another session's shell — is an agent of its own:
// before its first header its calls wait in its own pending bucket, which its session
// adopts, and neither agent takes the other's session.
func TestSessionForProcess_ANestedAgentIsNotFiledUnderItsParent(t *testing.T) {
	s, clk := newProcStore()
	defer s.Close()
	outer := pchain(100, 50)
	s.ClaimProcess("outer", "claude-code", outer)
	recordIn(s, clk, "outer")
	nested := pchain(400, 200, 100, 50) // claude -p ← bash ← claude

	// Its first request is a CONNECT, which carries no User-Agent: it looks like any
	// tool of the outer session until a request names the agent.
	if got := s.SessionForProcess(nested, ""); got != "outer" {
		t.Fatalf("before it identifies: %q, want outer", got)
	}
	pending := s.SessionForProcess(nested, "claude-code")
	if want := PendingProcessID("claude-code", nested[0]); pending != want {
		t.Fatalf("once it identifies: %q, want its own pending bucket %q", pending, want)
	}
	recordIn(s, clk, pending)
	s.ClaimProcess("inner", "claude-code", nested)
	if v := s.View("inner"); v == nil || len(v.Events) != 1 {
		t.Errorf("inner = %+v, want the adopted pre-header call", v)
	}
	recordIn(s, clk, "inner")
	if got := s.SessionForProcess(nested, "claude-code"); got != "inner" {
		t.Errorf("nested agent: %q, want inner", got)
	}
	if got := s.SessionForProcess(outer, "claude-code"); got != "outer" {
		t.Errorf("outer agent: %q, want outer", got)
	}
}

// One process serving several sessions — OpenCode's background service — answers with the
// one most recently active, and a tool it spawns keeps that answer.
func TestSessionForProcess_ASharedServiceAnswersWithItsNewestSession(t *testing.T) {
	s, clk := newProcStore()
	defer s.Close()
	svc := pchain(500)
	s.ClaimProcess("a", "", svc)
	recordIn(s, clk, "a")
	s.ClaimProcess("b", "", svc)
	recordIn(s, clk, "b")
	recordIn(s, clk, "a") // a's response came last
	s.TouchProcess("a", svc)

	if got := s.SessionForProcess(svc, ""); got != "a" {
		t.Fatalf("service: %q, want a, its most recently active session", got)
	}
	child := pchain(600, 500)
	if got := s.SessionForProcess(child, ""); got != "a" {
		t.Fatalf("its tool: %q, want a", got)
	}
	recordIn(s, clk, "b")
	if got := s.SessionForProcess(child, ""); got != "a" {
		t.Errorf("its tool moved to %q when the other session spoke; want it kept in a", got)
	}
}

// A process of no agent is no agent's: default while an agent is active, and "" — so
// ActiveSession() answers as before — once none has spoken for the ambiguity window.
func TestSessionForProcess_AProcessOfNoAgent(t *testing.T) {
	s, clk := newProcStore()
	defer s.Close()
	tool := pchain(700, 60)
	if got := s.SessionForProcess(tool, ""); got != "" {
		t.Fatalf("no agent yet: %q, want \"\"", got)
	}
	s.ClaimProcess("s1", "claude-code", pchain(100, 50))
	recordIn(s, clk, "s1")
	if got := s.SessionForProcess(tool, ""); got != DefaultSessionID {
		t.Errorf("an agent is active: %q, want default", got)
	}
	clk.advance(ambiguityWindow + time.Second)
	if got := s.SessionForProcess(pchain(701, 60), ""); got != "" {
		t.Errorf("no agent for longer than the window: %q, want \"\"", got)
	}
}

func TestSessionForProcess_AReusedPIDIsANewProcess(t *testing.T) {
	s, clk := newProcStore()
	defer s.Close()
	s.ClaimProcess("s1", "claude-code", []Proc{{PID: 100, Start: 1}})
	recordIn(s, clk, "s1")
	if got := s.SessionForProcess([]Proc{{PID: 100, Start: 2}}, ""); got == "s1" {
		t.Error("a new process with a reused pid inherited the old one's session")
	}
}

// A claim is made before the request that carried the header is recorded, so a request
// that arrives in between must not make the store forget it.
func TestSessionForProcess_AClaimSurvivesUntilItsSessionIsRecorded(t *testing.T) {
	s, clk := newProcStore()
	defer s.Close()
	s.ClaimProcess("s1", "claude-code", pchain(100))
	_ = s.SessionForProcess(pchain(300, 100), "")
	recordIn(s, clk, "s1")
	if got := s.SessionForProcess(pchain(301, 100), ""); got != "s1" {
		t.Errorf("after the session was recorded: %q, want s1", got)
	}
}

// Bob runs as `node bob` above `node bob`. Whichever of the two sends the calls before
// the first X-Task-Id, the first session adopts them, and both answer with it after.
func TestClaimProcess_ALauncherAndItsWorkerAreOneAgent(t *testing.T) {
	launcher, worker := pchain(820, 50), pchain(821, 820, 50)
	for name, preHeader := range map[string][]Proc{"launcher": launcher, "worker": worker} {
		t.Run(name, func(t *testing.T) {
			s, clk := newProcStore()
			defer s.Close()
			_ = s.SessionForProcess(launcher, "bob-shell") // the launcher identifies as Bob too
			pending := s.SessionForProcess(preHeader, "bob-shell")
			recordIn(s, clk, pending)
			s.ClaimProcess("task-1", "bob-shell", worker)
			if v := s.View("task-1"); v == nil || len(v.Events) != 1 {
				t.Fatalf("task-1 = %+v, want the adopted pre-header call from %s", v, pending)
			}
			recordIn(s, clk, "task-1")
			for who, c := range map[string][]Proc{"launcher": launcher, "worker": worker} {
				if got := s.SessionForProcess(c, "bob-shell"); got != "task-1" {
					t.Errorf("%s after the claim: %q, want task-1", who, got)
				}
			}
		})
	}
}

func TestSessionForProcess_TheProcessTableIsBounded(t *testing.T) {
	s, _ := newProcStore()
	defer s.Close()
	for pid := int32(2); pid < maxProcs+200; pid++ {
		_ = s.SessionForProcess(pchain(pid), "")
	}
	if n := len(s.procs); n > maxProcs {
		t.Errorf("%d processes held, want at most %d", n, maxProcs)
	}
}

func TestProcessHintsAndIsAgentProcess(t *testing.T) {
	s, clk := newProcStore()
	defer s.Close()
	s.ClaimProcess("s1", "", pchain(500))
	recordIn(s, clk, "s1")
	_ = s.SessionForProcess(pchain(600, 500), "")
	if !s.IsAgentProcess(pchain(500)[0]) || s.IsAgentProcess(pchain(600)[0]) {
		t.Error("IsAgentProcess: want true for the process that named a session, false for its tool")
	}
	if got := s.ProcessHints(8); len(got) != 1 || got[0] != 500 {
		t.Errorf("ProcessHints = %v, want [500]", got)
	}
}

// Per-process pending buckets belong to one agent for SessionForClient's "two agents are
// active" test, exactly like the agent-wide bucket.
func TestSessionForClient_PerProcessPendingBucketsCountAsOneAgent(t *testing.T) {
	s, clk := newProcStore()
	defer s.Close()
	recordIn(s, clk, PendingProcessID("claude-code", Proc{PID: 1, Start: 1}))
	recordIn(s, clk, PendingProcessID("claude-code", Proc{PID: 2, Start: 2}))
	if got := s.SessionForClient(""); got != "" {
		t.Errorf("two pending buckets of one agent: %q, want \"\" (one agent)", got)
	}
	recordIn(s, clk, PendingSessionID("bob-shell"))
	if got := s.SessionForClient(""); got != DefaultSessionID {
		t.Errorf("with a second agent: %q, want default", got)
	}
}

// The adoption records used to outlive both sessions they named, for the life of the
// process.
func TestEviction_ForgetsAdoptionRecords(t *testing.T) {
	s := New(0, 0, 2)
	defer s.Close()
	s.Append(PendingSessionID("claude-code"), pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest})
	s.Claim("s1", "claude-code") // adopts the pending bucket
	for _, id := range []string{"s2", "s3"} {
		s.Append(id, pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest})
	}
	if s.View("s1") != nil {
		t.Fatal("s1 was not evicted; the test needs it gone")
	}
	if n := len(s.adopted); n != 0 {
		t.Errorf("%d adoption record(s) left after their session was evicted", n)
	}
}

// A pid is reused, and an adopted pending bucket's id keeps redirecting into the session
// that adopted it. A new process with the old pid must get a bucket of its own.
func TestSessionForProcess_AReusedPIDDoesNotInheritAPendingBucket(t *testing.T) {
	s, clk := newProcStore()
	defer s.Close()
	old := []Proc{{PID: 400, Start: 1}}
	recordIn(s, clk, s.SessionForProcess(old, "claude-code"))
	s.ClaimProcess("yesterday", "claude-code", old)

	reborn := []Proc{{PID: 400, Start: 2}}
	recordIn(s, clk, s.SessionForProcess(reborn, "claude-code"))
	if v := s.View("yesterday"); v == nil || len(v.Events) != 1 {
		t.Errorf("yesterday = %+v; a new process with the old pid wrote into the old process's session", v)
	}
	s.ClaimProcess("today", "claude-code", reborn)
	if v := s.View("today"); v == nil || len(v.Events) != 1 {
		t.Errorf("today = %+v, want its own adopted pre-header call", v)
	}
}

// A process that names its own session — OpenCode's service, whose User-Agent is not yet
// recognised — first seen under another session's tree is never filed under that session
// once it has named its own, even while its own has recorded nothing yet.
func TestSessionForProcess_AProcessThatNamesASessionIsNeverBoundToAnother(t *testing.T) {
	s, clk := newProcStore()
	defer s.Close()
	s.ClaimProcess("outer", "claude-code", pchain(100))
	recordIn(s, clk, "outer")
	svc := pchain(500, 100)
	if got := s.SessionForProcess(svc, ""); got != "outer" {
		t.Fatalf("first seen as the outer agent's tool: %q, want outer", got)
	}
	s.ClaimProcess("own", "", svc)
	if got := s.SessionForProcess(svc, ""); got == "outer" {
		t.Errorf("a process that named its own session fell back to the one it was first bound to")
	}
	recordIn(s, clk, "own")
	if got := s.SessionForProcess(svc, ""); got != "own" {
		t.Errorf("once its session is recorded: %q, want own", got)
	}
}

// A tool keeps its session for its life — unless the session is gone, and then it is
// rebound to its agent's live one.
func TestSessionForProcess_AToolWhoseSessionIsGoneIsRebound(t *testing.T) {
	clk := &procTestClock{t: time.Unix(1_700_000_000, 0)}
	s := New(0, 0, 2, WithClock(clk.now))
	defer s.Close()
	claude := pchain(100)
	s.ClaimProcess("s1", "claude-code", claude)
	recordIn(s, clk, "s1")
	tool := pchain(300, 100)
	if got := s.SessionForProcess(tool, ""); got != "s1" {
		t.Fatalf("tool: %q, want s1", got)
	}
	s.ClaimProcess("s2", "claude-code", claude)
	recordIn(s, clk, "s2")
	recordIn(s, clk, "other") // a third session evicts the oldest, s1
	if s.View("s1") != nil {
		t.Fatal("s1 was not evicted; the test needs it gone")
	}
	if got := s.SessionForProcess(tool, ""); got != "s2" {
		t.Errorf("a tool whose session is gone: %q, want its agent's live s2", got)
	}
}

// Pruning evicts processes that never named a session before an agent's own.
func TestPruneProcs_EvictsToolsBeforeAgents(t *testing.T) {
	clk := &procTestClock{t: time.Unix(1_700_000_000, 0)}
	s := New(0, 0, 0, WithClock(clk.now))
	defer s.Close()

	// Fill the table to (maxProcs - 1) with unclaimed (tool) processes
	// All tools have the same timestamp
	toolTime := clk.t
	toolsAdded := 0
	for pid := int32(1); toolsAdded < maxProcs-1; pid++ {
		tool := []Proc{{PID: pid, Start: int64(pid) * 1000}}
		_ = s.SessionForProcess(tool, "")
		toolsAdded++
	}
	if len(s.procs) != maxProcs-1 {
		t.Fatalf("after initial fill, expected %d procs, got %d", maxProcs-1, len(s.procs))
	}

	// Add a claimed (agent) process at the OLDEST chronological time
	// (before all the tools)
	clk.t = toolTime.Add(-1 * time.Hour)
	agent := []Proc{{PID: maxProcs, Start: int64(maxProcs) * 1000}}
	s.ClaimProcess("agent-session", "some-agent", agent)
	if _, ok := s.procs[agent[0].key()]; !ok {
		t.Fatal("agent not added to procs")
	}
	// Now s.procs should have (maxProcs-1) tools + 1 agent = maxProcs entries
	if len(s.procs) != maxProcs {
		t.Fatalf("after agent add, expected %d procs, got %d", maxProcs, len(s.procs))
	}

	// Add one more unclaimed (tool) process to trigger prune
	// This pushes us over maxProcs and forces prune to run
	clk.t = toolTime.Add(1 * time.Second)
	tool := []Proc{{PID: maxProcs + 1, Start: int64(maxProcs+1) * 1000}}
	_ = s.SessionForProcess(tool, "")

	if want := maxProcs - maxProcs/4 + 1; len(s.procs) != want {
		t.Fatalf("%d processes after the trigger insert, want %d: no prune ran", len(s.procs), want)
	}

	// Verify the agent process (claimed, oldest) survived
	// because unclaimed (tool) processes are evicted first
	agentKey := agent[0].key()
	if _, ok := s.procs[agentKey]; !ok {
		t.Error("agent process was evicted; pruning must evict unclaimed processes first")
	}

	// ProcessHints guard tests
	if hints := s.ProcessHints(0); hints != nil {
		t.Errorf("ProcessHints(0) should return nil, got %v", hints)
	}
	if hints := s.ProcessHints(-1); hints != nil {
		t.Errorf("ProcessHints(-1) should return nil, got %v", hints)
	}
}

// A launcher and its worker are one agent. When the launcher names the session, the
// worker's header-less calls go there too, not to a pending bucket that the launcher's next
// session would adopt, which would move them into a session they predate.
func TestSessionForProcess_AWorkerJoinsItsLaunchersSession(t *testing.T) {
	s, clk := newProcStore()
	defer s.Close()
	launcher, worker := pchain(800), pchain(820, 800)
	s.ClaimProcess("task-1", "bob-shell", launcher)
	recordIn(s, clk, "task-1")
	if got := s.SessionForProcess(worker, "bob-shell"); got != "task-1" {
		t.Fatalf("a worker under a launcher in task-1: %q, want task-1", got)
	}
	recordIn(s, clk, "task-1") // the worker's call
	s.ClaimProcess("task-2", "bob-shell", launcher)
	recordIn(s, clk, "task-2")
	if got := s.SessionForProcess(worker, "bob-shell"); got != "task-2" {
		t.Errorf("after the launcher moved on: %q, want task-2", got)
	}
	if v := s.View("task-1"); v == nil || len(v.Events) != 2 {
		t.Errorf("task-1 = %+v, want the launcher's request and the worker's call", v)
	}
}

// A tool keeps the session it was started under, also when a launcher's worker started it
// rather than the claimer itself.
func TestSessionForProcess_AToolUnderAWorkerIsBoundToTheLaunchersSession(t *testing.T) {
	s, clk := newProcStore()
	defer s.Close()
	s.ClaimProcess("task-1", "bob-shell", pchain(800))
	recordIn(s, clk, "task-1")
	s.SessionForProcess(pchain(820, 800), "bob-shell") // the worker, seen first
	tool := pchain(900, 820, 800)
	if got := s.SessionForProcess(tool, ""); got != "task-1" {
		t.Fatalf("a tool under the worker: %q, want task-1", got)
	}
	s.ClaimProcess("task-2", "bob-shell", pchain(800))
	recordIn(s, clk, "task-2")
	if got := s.SessionForProcess(tool, ""); got != "task-1" {
		t.Errorf("after the launcher moved on: %q, want task-1, the session the tool started under", got)
	}
}

// A tool bound to an agent's old session keeps writing there after the agent names a new
// one. Those writes are the tool's: they do not make the old session the agent's newest
// again, for the agent's own header-less calls or for a tool started after the move.
func TestSessionForProcess_AToolWritingToAnOldSessionDoesNotMakeItTheNewest(t *testing.T) {
	s, clk := newProcStore()
	defer s.Close()
	claude := pchain(100, 50)
	s.ClaimProcess("s1", "claude-code", claude)
	recordIn(s, clk, "s1")
	watcher := pchain(300, 200, 100, 50) // gh run watch ← bash ← claude
	if got := s.SessionForProcess(watcher, ""); got != "s1" {
		t.Fatalf("tool: %q, want s1", got)
	}
	recordIn(s, clk, "s1")
	s.ClaimProcess("s2", "claude-code", claude) // /clear
	recordIn(s, clk, "s2")
	recordIn(s, clk, "s1") // the watcher, still in s1

	if got := s.SessionForProcess(claude, "claude-code"); got != "s2" {
		t.Errorf("the agent's own header-less call: %q, want s2", got)
	}
	if got := s.SessionForProcess(pchain(301, 200, 100, 50), ""); got != "s2" {
		t.Errorf("a tool started after the move: %q, want s2", got)
	}
}

// The same holds for the claims a worker answers with from its launcher.
func TestSessionForProcess_AToolWritingToALaunchersOldSessionDoesNotMoveItsWorker(t *testing.T) {
	s, clk := newProcStore()
	defer s.Close()
	launcher, worker := pchain(800), pchain(820, 800)
	s.ClaimProcess("task-1", "bob-shell", launcher)
	recordIn(s, clk, "task-1")
	_ = s.SessionForProcess(worker, "bob-shell")
	if got := s.SessionForProcess(pchain(900, 820, 800), ""); got != "task-1" {
		t.Fatalf("tool: %q, want task-1", got)
	}
	s.ClaimProcess("task-2", "bob-shell", launcher)
	recordIn(s, clk, "task-2")
	recordIn(s, clk, "task-1") // the tool, still in task-1

	if got := s.SessionForProcess(worker, "bob-shell"); got != "task-2" {
		t.Errorf("the worker: %q, want task-2", got)
	}
}

// ChainSession is the session a process above the sender named, so a child that names
// a session of its own still answers with the one its parent agent is in. The topmost
// claim wins: a nested agent's own session does not stand between a tool and the outer one.
func TestChainSession_AnAncestorsClaimWins(t *testing.T) {
	s, clk := newProcStore()
	defer s.Close()
	claude := pchain(100, 50)
	s.ClaimProcess("s1", "claude-code", claude)
	recordIn(s, clk, "s1")

	if got := s.ChainSession(claude); got != "" {
		t.Errorf("the agent itself: %q, want none above it", got)
	}
	child := pchain(300, 200, 100, 50) // child ← bash ← claude ← login shell
	s.ClaimProcess("fresh", "claude-code", child)
	recordIn(s, clk, "fresh")
	if got := s.ChainSession(child); got != "s1" {
		t.Errorf("a child that named its own session: %q, want s1", got)
	}
	if got := s.ChainSession(pchain(301, 200, 100, 50)); got != "s1" {
		t.Errorf("a tool that named nothing: %q, want s1", got)
	}

	nested := pchain(400, 200, 100, 50)
	s.ClaimProcess("inner", "claude-code", nested)
	recordIn(s, clk, "inner")
	if got := s.ChainSession(pchain(500, 450, 400, 200, 100, 50)); got != "s1" {
		t.Errorf("a tool of a nested agent: %q, want the outer session s1", got)
	}
	if got := s.ChainSession(pchain(600, 60)); got != "" {
		t.Errorf("an unrelated process: %q, want none", got)
	}
}

// A worker below its launcher is the same agent, not a child of it: the launcher holds
// the worker's own claim, which must not count as an ancestor's.
func TestChainSession_AnAgentsOwnLauncherIsNotAnAncestor(t *testing.T) {
	s, clk := newProcStore()
	defer s.Close()
	launcher := pchain(100, 50)
	s.SessionForProcess(launcher, "claude-code")
	worker := pchain(110, 100, 50)
	s.ClaimProcess("s1", "claude-code", worker)
	recordIn(s, clk, "s1")
	s.ClaimProcess("s2", "claude-code", worker)
	recordIn(s, clk, "s2")
	if got := s.ChainSession(worker); got != "" {
		t.Errorf("worker: %q, want none above the agent", got)
	}
}
