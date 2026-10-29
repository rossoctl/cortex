package attribution

import (
	"fmt"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/peerproc"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// testClock is a clock tests move by hand, so "most recently active" and the ambiguity
// window never depend on how fast the machine is.
type testClock struct{ t time.Time }

func (c *testClock) now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }

var testHeaders = []string{session.ClaudeCodeSessionHeader, session.BobSessionHeader}

func newTest() (*Attributor, *testClock) {
	clk := &testClock{t: time.Unix(1_700_000_000, 0)}
	return New(Config{SessionIDHeaders: testHeaders, ClientAffinity: true, Now: clk.now}), clk
}

// pchain is a process chain, nearest first. Each pid's start time is derived from it, so the
// same pid always names the same process unless a test says otherwise.
func pchain(pids ...int32) []Proc {
	out := make([]Proc, len(pids))
	for i, pid := range pids {
		out[i] = Proc{PID: pid, Start: int64(pid) * 1000, Exe: fmt.Sprintf("/bin/p%d", pid)}
	}
	return out
}

const (
	claudeUA = "claude-cli/2.1.286 (external, cli)"
	bobUA    = "bob-shell/2.0.5"
)

// req is a request's headers: a User-Agent, and a session header when sid is not "".
func req(ua, sid string) http.Header {
	h := http.Header{}
	if ua != "" {
		h.Set("User-Agent", ua)
	}
	if sid != "" {
		h.Set(session.ClaudeCodeSessionHeader, sid)
	}
	return h
}

// record is the store appending to id a second after the last, which is how the attributor
// learns a session exists.
func record(a *Attributor, clk *testClock, id string) {
	clk.advance(time.Second)
	a.Record(id, &pipeline.SessionEvent{})
}

func TestResolve_AHeaderNamesTheSessionAndClaimsTheProcess(t *testing.T) {
	a, _ := newTest()
	got := a.Resolve(req(claudeUA, "s1"), pchain(100, 50))
	want := Attribution{Agent: "claude-code", Proc: "100.100000", Session: "s1",
		Header: session.ClaudeCodeSessionHeader, Source: pipeline.SessionSourceHeader}
	if got != want {
		t.Fatalf("Resolve = %+v, want %+v", got, want)
	}
	if !a.IsAgentProcess(pchain(100)[0]) {
		t.Error("the process that named a session is not an agent's")
	}
}

// A tool an agent's shell runs belongs to the session that ran it, and keeps it when the
// agent moves on (/clear); a tool started after the move gets the new one.
func TestResolve_AToolJoinsTheSessionOfTheAgentThatRanIt(t *testing.T) {
	a, clk := newTest()
	claude := pchain(100, 50)
	a.Resolve(req(claudeUA, "s1"), claude)
	record(a, clk, "s1")
	curl := pchain(300, 200, 100, 50)
	if got := a.Resolve(req("curl/8.7.1", ""), curl); got.Session != "s1" || got.Source != pipeline.SessionSourceProcess {
		t.Fatalf("tool of the agent: %+v, want s1 by process", got)
	}
	a.Resolve(req(claudeUA, "s2"), claude)
	record(a, clk, "s2")
	if got := a.Resolve(req("curl/8.7.1", ""), curl).Session; got != "s1" {
		t.Errorf("a tool still running moved to %q after the agent named s2; want it kept in s1", got)
	}
	if got := a.Resolve(req("curl/8.7.1", ""), pchain(301, 200, 100, 50)).Session; got != "s2" {
		t.Errorf("a tool started after the move: %q, want s2", got)
	}
}

// An agent process that has named no session gets a pending answer for itself, never
// another session's — and its calls stay in that bucket when it names one: nothing adopts.
func TestResolve_AnAgentProcessBeforeItsFirstHeaderIsPending(t *testing.T) {
	a, clk := newTest()
	outer := pchain(100, 50)
	a.Resolve(req(claudeUA, "outer"), outer)
	record(a, clk, "outer")
	nested := pchain(400, 200, 100, 50) // claude -p ← bash ← claude

	// Its first request is a CONNECT, which carries no User-Agent: it looks like any tool of
	// the outer session until a request names the agent.
	if got := a.Resolve(nil, nested).Session; got != "outer" {
		t.Fatalf("before it identifies: %q, want outer", got)
	}
	got := a.Resolve(req(claudeUA, ""), nested)
	if got.Source != pipeline.SessionSourcePending || got.Proc != nested[0].Key() || got.Agent != "claude-code" {
		t.Fatalf("once it identifies: %+v, want pending for its own process", got)
	}
	if want := session.PendingID("claude-code", nested[0].Key()); got.RecordID() != want || got.PluginID() != want {
		t.Errorf("RecordID %q PluginID %q, want both %q", got.RecordID(), got.PluginID(), want)
	}
	a.Resolve(req(claudeUA, "inner"), nested)
	record(a, clk, "inner")
	if got := a.Resolve(req(claudeUA, ""), nested).Session; got != "inner" {
		t.Errorf("nested agent after its header: %q, want inner", got)
	}
	if got := a.Resolve(req(claudeUA, ""), outer).Session; got != "outer" {
		t.Errorf("outer agent: %q, want outer", got)
	}
}

// A launcher and its worker are one agent: whichever sends the pre-header calls, they are the
// launcher's (the root's) pending calls, and a header from the worker claims for both.
func TestResolve_ALauncherAndItsWorkerAreOneAgent(t *testing.T) {
	a, clk := newTest()
	launcher, worker := pchain(820, 50), pchain(821, 820, 50)
	a.Resolve(req(bobUA, ""), launcher)
	if got := a.Resolve(req(bobUA, ""), worker); got.Proc != launcher[0].Key() {
		t.Fatalf("worker's pending process = %q, want the launcher %q", got.Proc, launcher[0].Key())
	}
	h := req(bobUA, "")
	h.Set(session.BobSessionHeader, "task-1")
	if got := a.Resolve(h, worker); got.Proc != launcher[0].Key() {
		t.Errorf("the worker's headered request is attributed to %q, want the root %q", got.Proc, launcher[0].Key())
	}
	record(a, clk, "task-1")
	for who, c := range map[string][]Proc{"launcher": launcher, "worker": worker} {
		if got := a.Resolve(req(bobUA, ""), c).Session; got != "task-1" {
			t.Errorf("%s after the claim: %q, want task-1", who, got)
		}
	}
}

// One process serving several sessions answers with the one whose request finished last,
// and a tool it spawns keeps that answer.
func TestResolve_ASharedServiceAnswersWithItsNewestSession(t *testing.T) {
	a, clk := newTest()
	svc := pchain(500)
	a.Resolve(req("", "a"), svc)
	record(a, clk, "a")
	a.Resolve(req("", "b"), svc)
	record(a, clk, "b")
	clk.advance(time.Second)
	a.Touch("a", svc) // a's response came last
	if got := a.Resolve(nil, svc).Session; got != "a" {
		t.Fatalf("service: %q, want a", got)
	}
	child := pchain(600, 500)
	if got := a.Resolve(nil, child).Session; got != "a" {
		t.Fatalf("its tool: %q, want a", got)
	}
	record(a, clk, "b")
	if got := a.Resolve(nil, child).Session; got != "a" {
		t.Errorf("its tool moved to %q when the other session spoke; want it kept in a", got)
	}
}

// A process of no agent is no agent's: default while an agent is active, and nothing — so
// the store's ActiveSession() answers — once none has spoken for the ambiguity window.
func TestResolve_AProcessOfNoAgent(t *testing.T) {
	a, clk := newTest()
	tool := pchain(700, 60)
	if got := a.Resolve(nil, tool); got.Session != "" || got.RecordID() != "" {
		t.Fatalf("no agent yet: %+v, want nothing", got)
	}
	a.Resolve(req(claudeUA, "s1"), pchain(100, 50))
	record(a, clk, "s1")
	got := a.Resolve(nil, tool)
	if got.Session != session.DefaultSessionID || got.PluginID() != "" || got.RecordID() != session.DefaultSessionID {
		t.Errorf("an agent is active: %+v, want default, which plugins never hear", got)
	}
	clk.advance(ambiguityWindow + time.Second)
	if got := a.Resolve(nil, pchain(701, 60)).Session; got != "" {
		t.Errorf("no agent for longer than the window: %q, want \"\"", got)
	}
}

func TestResolve_AReusedPIDIsANewProcess(t *testing.T) {
	a, clk := newTest()
	a.Resolve(req(claudeUA, "s1"), []Proc{{PID: 100, Start: 1}})
	record(a, clk, "s1")
	if got := a.Resolve(nil, []Proc{{PID: 100, Start: 2}}).Session; got == "s1" {
		t.Error("a new process with a reused pid inherited the old one's session")
	}
}

// A claim is made before the request that carried the header is recorded, so a request that
// arrives in between must already find the session.
func TestResolve_AClaimCountsBeforeItsSessionIsRecorded(t *testing.T) {
	a, _ := newTest()
	a.Resolve(req(claudeUA, "s1"), pchain(100))
	if got := a.Resolve(nil, pchain(300, 100)).Session; got != "s1" {
		t.Errorf("between the claim and its record: %q, want s1", got)
	}
}

// A tool keeps its session for its life — unless the session is gone, and then it is rebound
// to its agent's live one.
func TestResolve_AToolWhoseSessionIsGoneIsRebound(t *testing.T) {
	a, clk := newTest()
	claude := pchain(100)
	a.Resolve(req(claudeUA, "s1"), claude)
	record(a, clk, "s1")
	tool := pchain(300, 100)
	if got := a.Resolve(nil, tool).Session; got != "s1" {
		t.Fatalf("tool: %q, want s1", got)
	}
	a.Resolve(req(claudeUA, "s2"), claude)
	record(a, clk, "s2")
	a.Evicted("s1")
	if got := a.Resolve(nil, tool).Session; got != "s2" {
		t.Errorf("a tool whose session is gone: %q, want its agent's live s2", got)
	}
}

// ChainSession is the session a process above the sender named; the topmost wins.
func TestResolve_ChainSessionIsTheTopmostAncestorsClaim(t *testing.T) {
	a, clk := newTest()
	a.Resolve(req(claudeUA, "s1"), pchain(100, 50))
	record(a, clk, "s1")
	child := pchain(300, 200, 100, 50)
	got := a.Resolve(req(claudeUA, "fresh"), child)
	if got.Session != "fresh" || got.ChainSession != "s1" {
		t.Errorf("a child that named its own session: %+v, want Session fresh, ChainSession s1", got)
	}
	if got := a.Resolve(nil, pchain(600, 60)).ChainSession; got != "" {
		t.Errorf("an unrelated process: ChainSession %q, want none", got)
	}
}

// Without a process, client affinity files by User-Agent: the agent's newest session, else
// its agent-wide pending bucket, and default for an unknown client between two agents.
func TestResolve_ClientAffinityWithoutAProcess(t *testing.T) {
	a, clk := newTest()
	if got := a.Resolve(req(bobUA, ""), nil); got.RecordID() != session.PendingSessionID("bob") {
		t.Fatalf("known client with no session: %+v, want its agent-wide pending bucket", got)
	}
	a.Resolve(req(claudeUA, "claude-1"), nil)
	record(a, clk, "claude-1")
	if got := a.Resolve(req(claudeUA, ""), nil); got.Session != "claude-1" || got.Source != pipeline.SessionSourceAffinity {
		t.Fatalf("claude-code: %+v, want claude-1 by affinity", got)
	}
	if got := a.Resolve(req("", ""), nil).Session; got != "" {
		t.Fatalf("unknown client beside ONE known agent: %q, want \"\"", got)
	}
	record(a, clk, session.PendingSessionID("bob"))
	if got := a.Resolve(req("", ""), nil).Session; got != session.DefaultSessionID {
		t.Errorf("unknown client beside two known agents: %q, want default", got)
	}
	// First claim wins: quoting claude's id does not make it bob's.
	h := req(bobUA, "")
	h.Set(session.BobSessionHeader, "claude-1")
	a.Resolve(h, nil)
	if got := a.Resolve(req(bobUA, ""), nil); got.RecordID() != session.PendingSessionID("bob") {
		t.Errorf("bob after quoting claude's id: %+v, want still pending", got)
	}
}

// With no session headers configured nothing is any client's, so every answer falls back.
func TestResolve_NoHeadersMeansNoAttribution(t *testing.T) {
	a := New(Config{ClientAffinity: true})
	got := a.Resolve(req(claudeUA, "s1"), pchain(100))
	if got.Session != "" || got.RecordID() != "" || got.Agent != "claude-code" {
		t.Errorf("Resolve = %+v, want no session and the agent named", got)
	}
}

// Clear forgets every claim, so the next request names nothing it was told before.
func TestCleared_ForgetsEveryClaim(t *testing.T) {
	a, clk := newTest()
	a.Resolve(req(claudeUA, "s1"), pchain(100))
	record(a, clk, "s1")
	a.Cleared()
	if got := a.Resolve(nil, pchain(300, 100)).Session; got != "" {
		t.Errorf("after Cleared: %q, want nothing", got)
	}
	if a.IsAgentProcess(pchain(100)[0]) || len(a.ProcessHints(8)) != 0 {
		t.Error("after Cleared the process table still names an agent")
	}
}

func TestProcessTablesAreBounded(t *testing.T) {
	a, clk := newTest()
	for pid := int32(2); pid < maxProcs+200; pid++ {
		a.Resolve(nil, pchain(pid))
	}
	for i := range maxSeen + 200 {
		record(a, clk, fmt.Sprintf("s%d", i))
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.procs) > maxProcs || len(a.seen) > maxSeen {
		t.Errorf("procs %d (max %d), seen %d (max %d)", len(a.procs), maxProcs, len(a.seen), maxSeen)
	}
}

// fakeResolver is a peerproc.Resolver over a fixed table: one connection owner and parents.
type fakeResolver struct {
	owner peerproc.Proc
	procs map[int32]peerproc.Proc
	hints []int32
	err   error
}

func (f *fakeResolver) ConnOwner(_, _ netip.AddrPort, hints ...int32) (peerproc.Proc, error) {
	f.hints = hints
	return f.owner, f.err
}

func (f *fakeResolver) ListenerOwner(netip.AddrPort, ...int32) (peerproc.Proc, error) {
	return peerproc.Proc{}, peerproc.ErrNotFound
}

func (f *fakeResolver) Ancestry(pid int32, max int) ([]peerproc.Proc, error) {
	var out []peerproc.Proc
	for p, ok := f.procs[pid]; ok && len(out) < max; p, ok = f.procs[p.PPID] {
		out = append(out, p)
	}
	return out, nil
}

// Lookup turns a connection into the chain Resolve takes, nearest first, and asks the kernel
// about the processes that named a session first.
func TestLookup_NamesTheClientsChain(t *testing.T) {
	a, _ := newTest()
	a.Resolve(req(claudeUA, "s1"), pchain(100))
	claude := peerproc.Proc{PID: 100, PPID: 50, Start: time.Unix(100, 0), Exe: "/bin/claude"}
	curl := peerproc.Proc{PID: 300, PPID: 100, Start: time.Unix(300, 0), Exe: "/usr/bin/curl"}
	r := &fakeResolver{owner: curl, procs: map[int32]peerproc.Proc{100: claude, 300: curl}}
	addr := netip.MustParseAddrPort("127.0.0.1:1")
	chain, err := a.Lookup(r, addr, addr)
	if err != nil {
		t.Fatal(err)
	}
	if len(chain) != 2 || chain[0].PID != 300 || chain[1].PID != 100 || chain[0].Start != time.Unix(300, 0).UnixNano() {
		t.Errorf("chain = %+v, want curl then claude", chain)
	}
	if len(r.hints) != 1 || r.hints[0] != 100 {
		t.Errorf("hints = %v, want the process that named a session", r.hints)
	}
	r.err = peerproc.ErrNotFound
	if chain, err := a.Lookup(r, addr, addr); err == nil || chain != nil {
		t.Errorf("an unnamed connection: %v %v, want an error and no chain", chain, err)
	}
}

// The worker's headered request names the launcher as its process and itself as the client.
func TestResolve_AWorkersClaimCarriesBothProcesses(t *testing.T) {
	a, _ := newTest()
	launcher, worker := pchain(820, 50), pchain(821, 820, 50)
	a.Resolve(req(bobUA, ""), launcher)
	h := req(bobUA, "")
	h.Set(session.BobSessionHeader, "task-1")
	got := a.Resolve(h, worker)
	if got.Proc != launcher[0].Key() || got.ClientProc != worker[0].Key() {
		t.Errorf("Proc %q ClientProc %q, want %q and %q", got.Proc, got.ClientProc, launcher[0].Key(), worker[0].Key())
	}
}

// A full seen table frees its oldest quarter in one prune, so the next new ids insert without
// pruning again: one deletion per new id left the table full and every insert scanning all of it.
func TestSeenTable_PruneFreesTheOldestQuarter(t *testing.T) {
	a, clk := newTest()
	for i := range maxSeen {
		record(a, clk, fmt.Sprintf("s%d", i))
	}
	record(a, clk, "new")
	a.mu.Lock()
	defer a.mu.Unlock()
	if want := maxSeen - maxSeen/4 + 1; len(a.seen) != want {
		t.Errorf("seen holds %d after one prune, want %d", len(a.seen), want)
	}
	if _, ok := a.seen["s0"]; ok {
		t.Error("the oldest id survived the prune")
	}
	if _, ok := a.seen[fmt.Sprintf("s%d", maxSeen-1)]; !ok {
		t.Error("the newest id was pruned")
	}
}
