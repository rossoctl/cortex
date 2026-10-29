# Link, don't adopt: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop renaming pending buckets into sessions. Attribution becomes a store-free package, events record which process sent them, and a process's pre-session calls are linked to its session when the list is read.

**Architecture:** `core/attribution` owns the claims table (moved out of `session.Store`) and answers `Resolve(headers, chain) → Attribution`. The store records `proc` and `sessionHeader` on events and folds, per session, which processes named it and when (`procs`). `session.Link` turns those spans into `linkedTo`/`linkedAll` on pending buckets, and agentop folds linked rows into their session's timeline.

**Tech Stack:** Go 1.26.10, `net/http`, bubbletea (agentop). No new dependencies.

**Spec:** `docs/superpowers/specs/2026-10-28-link-not-adopt-attribution-design.md`

## Deviations from the spec, decided while planning

1. **Commit order.** The spec puts a pure move first. Moving `ClaimProcess`/`Claim` out of
   the store while they still adopt would need store internals (`adoptLocked`,
   `rekeyLocked`) exported for one commit and deleted two commits later. So
   `core/attribution` is written as a port with adoption already removed (Task 1), and the
   switch-over deletes the store side in one step (Task 3). It is still one PR.
2. **Two event fields, not one.** Events carry `sessionHeader` (the header that named
   the session) as well as `proc`. That lets the fold derive both `procs` and the session's
   owner from recorded data, replacing `Store.owners`, which leaves with attribution.
3. **`procs` holds a span, not a first time.** `{first, last}` per process. The link rule
   uses `last`, so a process that named a session yesterday, lost its claim in a proxy
   restart and made calls today before naming it again still links.
4. **A linked row's events merge by time** into the session's timeline, not prepend. For a
   fresh launch that is the same thing; for `--resume` the calls land at the moment they
   happened, not at the top of a session days older.
5. **`linkedAll`** joins `linkedTo` on the wire, so agentop hides a row only when every one
   of its requests is linked.
6. **The server keeps `agent` blank on pending buckets.** agentop reads the agent from the id
   (`sessionAgent`). Filling it server-side would make `sessionsStaying`
   (`cmd/agentop/tui/server_picker.go:482`) count a bucket with an inference host as a running
   session, and the router never pins buckets.
7. **`SelfTraffic` and `Program` stay in the forward proxy.** They decide how the proxy
   forwards, not which session a row is in. The relay needs `Resolve` and the process lookup,
   so only the lookup (`attribution.Lookup`) moves.

## Global Constraints

- Go 1.26.10. Run each module with `GOWORK=off` except `core` CI-style; always `gofmt -l <path>`.
- Commits: `git commit -s`, trailer `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. Never `Co-Authored-By`.
- Work only in `.worktrees/link-not-adopt` (branch `design/link-not-adopt`). Never run `make dev-install`, `agentop setup --restart` or `agentop service restart`: they replace the shared proxy on `:47600`.
- In-cluster behaviour is unchanged: ext_proc and the reverse proxy keep `ActiveSession()` and A2A `Rekey`.
- `default` stays the bucket for non-agent traffic while an agent is active.
- Pending bucket ids keep today's format: `pending:<agent>@<pid>.<start>` and `pending:<agent>`.
- Wire headers: `X-Cortex-Agent`, `X-Cortex-Proc`, `X-Cortex-Session`, `X-Cortex-Source`. Always stripped from client requests; decoded only from a trusted hop, and none is configured in this PR.
- Old `session.json` files (no `procs`, `owner`, `lastRequestAt`) must decode and link nothing.
- Every new non-trivial branch gets a test. Follow the repo's comment style: say why, in full sentences, no more than the code needs.

## Review Focus

1. **Proxy restart between a process's calls and its header.** Its new bucket's calls must
   link to the session it names again, and the archive-only buckets from before must still
   link. Tests: Task 4 `TestLink_AfterARestartTheResumedSessionStillTakesTheCalls`; verified
   live (Validation, step 3).
2. **`--resume` into a long session.** The linked calls show at their own time inside the
   timeline, not as its first rows. Test: Task 5 `TestSnapshot_MergesLinkedCallsByTime`.
3. **A client forging `X-Cortex-*` headers.** They never reach the upstream and never change
   attribution. Test: Task 7 `TestForwardProxy_StripsCortexAttributionHeaders`.
4. **Archive written before this change.** Decodes; its buckets link nothing and list under
   the agent their id names. Tests: Task 2 `TestSummaryFold_PersistsProcsAndDecodesAnOldDocument`,
   Task 5 `TestSessionAgent_APendingBucketBelongsToItsAgent`.
5. **A response recorded after the claim for a request made before it** (the MCP SSE stream
   that closes minutes later). It goes where its request went. Test: Task 4
   `TestLinkedEvents_AResponseFollowsItsRequest`.

A session longer than one snapshot merges only the linked calls inside the loaded page
(`inWindow`, `TestInWindow_KeepsTheLinkedCallsTheSnapshotCovers`). Older ones are not merged
when `[o]` pages back — a reviewer should decide whether that matters; the calls a resume
links are always in the newest page.

## File map

| File | Change |
|---|---|
| `core/attribution/attribution.go` | **new.** `Proc`, `Attribution`, `Config`, `Attributor`, `Resolve`, `Touch`, `ProcessHints`, `IsAgentProcess` |
| `core/attribution/process.go` | **new.** Ported process rules (from `core/session/process.go`), store-free |
| `core/attribution/affinity.go` | **new.** Ported client-affinity rules (from `core/session/affinity.go`), store-free |
| `core/attribution/lookup.go` | **new (Task 6).** `ConnKey`, `Memo`, `Chain`, `Program`, `SelfTraffic` (from `forwardproxy/process.go`) |
| `core/attribution/wire.go` | **new (Task 7).** `Encode`, `Decode`, `Strip` |
| `core/pipeline/session.go` | `SessionEvent.Proc`, `SessionEvent.SessionHeader` + wire fields |
| `core/pipeline/context.go` | `Context.ClientProc` |
| `core/listener/internal/sessionevent/sessionevent.go` | `Deny` copies `ClientProc` |
| `core/session/fold.go` | `ProcSpan`, `applyProcs`, `applyOwner`, `pickAgent`; fold state + JSON |
| `core/session/store.go` | entry `owner`/`procs`/`lastRequestAt`; summary `Procs`/`LinkedTo`/`LinkedAll`/`LastRequestAt`; remove owners/adopted/procs maps and `Adopted` |
| `core/session/history.go` | `withPriorLocked` merges the new fields; `Clear` drops the removed maps |
| `core/session/pending.go` | **new.** `PendingPrefix`, `PendingSessionID`, `ParsePending`, `IsConversation`, `Rekeyer` (survivors of `affinity.go`) |
| `core/session/link.go` | **new (Task 4).** `Link`, `LinkedEvents` |
| `core/session/affinity.go`, `core/session/process.go` | **deleted (Task 3)** |
| `core/listener/forwardproxy/server.go`, `process.go`, `transparent.go` | use `core/attribution`; record `proc`/`sessionHeader` |
| `core/sessionapi/server.go` | `handleList` calls `session.Link` |
| `core/plugins/inferencerouter/plugin.go`, `core/plugins/cpex/cmf_body.go`, `cmd/agentop/cmd_server_write.go` | use `session.IsConversation` |
| `cmd/agentop/tui/*` | group pending rows by agent; hide `linkedAll`; merge linked events; detail fetch by event's session |
| `CLAUDE.md`, `docs/session-api.md`, `cmd/agentop/README.md`, `docs/agents/opencode.md`, `docs/agents/codex.md` | docs |

---

### Task 1: `core/attribution`, a store-free port of the resolution rules

The rules in `core/session/process.go` and `affinity.go`, minus adoption, owned by a type
that never calls the session store. Liveness, which the store used to answer, comes from
the attributor's own `seen` table: a session is live from the last time the attributor
saw it claimed or recorded, until the store says it is gone: it is a `session.Recorder`,
`Clearer`, `Deleter`, `Rekeyer` and — new in Task 3 — `Evicter`, so the store tells it.
Nothing calls it yet.

**Files:**
- Create: `core/attribution/attribution.go`, `core/attribution/process.go`, `core/attribution/affinity.go`
- Create: `core/session/pending.go`
- Test: `core/attribution/attribution_test.go`, `core/session/pending_test.go`

**Interfaces:**
- Consumes: `session.IDHeaderFrom`, `session.MaxSessionIDLen`, `session.PendingPrefix`, `session.PendingSessionID`, `session.DefaultSessionID`, `pipeline.ParseUserAgent`, `pipeline.SessionSource*`.
- Produces (later tasks rely on these exact names):
  - `type Proc struct{ PID int32; Start int64; Exe string }`, `func (Proc) Key() string` → `"<pid>.<start>"`
  - `type Attribution struct{ Agent, Proc, Session, Header string; Source pipeline.SessionSource; ChainSession string }`
  - `func (Attribution) RecordID() string`, `func (Attribution) PluginID() string`
  - `type Config struct{ SessionIDHeaders []string; ClientAffinity bool; Now func() time.Time }`
  - `func New(Config) *Attributor`
  - `func (*Attributor) Resolve(h http.Header, chain []Proc) Attribution`
  - `func (*Attributor) Touch(sessionID string, chain []Proc)`
  - `func (*Attributor) ProcessHints(limit int) []int32`, `func (*Attributor) IsAgentProcess(Proc) bool`
  - `func (*Attributor) Record(string, *pipeline.SessionEvent)`, `Cleared()`, `Deleted(string)`, `Evicted(string)`, `Rekeyed(old, new string)`
  - `func AgentOf(h http.Header) string`
  - `session.PendingID(agent, proc string) string`

- [ ] **Step 1: Write the failing `PendingID` test**

`core/session/pending_test.go`:

```go
package session

import "testing"

// PendingID is the bucket id the recorder files a not-yet-named process's calls under. Its
// format is the one the archive already holds, so rows written before this change still read.
func TestPendingID(t *testing.T) {
	for _, tc := range []struct{ agent, proc, want string }{
		{"claude-code", "100.5", "pending:claude-code@100.5"},
		{"bob", "", "pending:bob"},
	} {
		if got := PendingID(tc.agent, tc.proc); got != tc.want {
			t.Errorf("PendingID(%q, %q) = %q, want %q", tc.agent, tc.proc, got, tc.want)
		}
	}
	if got, want := PendingID("claude-code", "100.5"), PendingProcessID("claude-code", Proc{PID: 100, Start: 5}); got != want {
		t.Errorf("PendingID = %q, PendingProcessID = %q: the two formats must agree until Task 3 removes the second", got, want)
	}
}
```

- [ ] **Step 2: Run it to see it fail**

Run: `cd core && go test ./session -run TestPendingID`
Expected: FAIL, `undefined: PendingID`

- [ ] **Step 3: Write `core/session/pending.go`**

```go
package session

// PendingID is the pending bucket for agent's calls from one process, proc being that
// process as "<pid>.<start>", or the agent-wide bucket when proc is "" — the process
// could not be looked up. It is a recording id only: nothing renames a pending bucket,
// and its calls are linked to the session the process names when the list is read (see
// Link).
func PendingID(agent, proc string) string {
	if proc == "" {
		return PendingSessionID(agent)
	}
	return PendingSessionID(agent) + "@" + proc
}
```

- [ ] **Step 4: Run it to see it pass**

Run: `cd core && go test ./session -run TestPendingID`
Expected: PASS

- [ ] **Step 5: Write the failing attribution tests**

These are the store's `process_test.go` and `affinity_test.go` cases, minus adoption,
against the new API. `core/attribution/attribution_test.go`:

```go
package attribution

import (
	"fmt"
	"net/http"
	"testing"
	"time"

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
```

- [ ] **Step 6: Run them to see them fail**

Run: `cd core && go test ./attribution`
Expected: FAIL, `undefined: New` (the package has no non-test files yet)

- [ ] **Step 7: Write `core/attribution/attribution.go`**

```go
// Package attribution decides which agent, process and session a request belongs to.
//
// It owns no events and never calls the session store: everything it knows it was told,
// through Resolve, Touch, and the session.Recorder hooks the store calls on every append.
// That is what lets the same code run outside the proxy — a local relay that resolves at
// accept time and stamps the answer on the request as headers (wire.go) — while the store
// only records. The answer is final: nothing renames a row after it is written. A process's
// calls before it names a session are linked to that session when the list is read
// (session.Link), not moved into it.
package attribution

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// Proc is one process in a request's chain, nearest first.
type Proc struct {
	PID int32
	// Start is the process's start time in Unix nanoseconds. PIDs are reused; PID and
	// Start together name one process for its lifetime.
	Start int64
	// Exe is the process's executable path, "" when unknown.
	Exe string
}

// Key is the process as events and bucket ids carry it: "<pid>.<start>".
func (p Proc) Key() string {
	return strconv.Itoa(int(p.PID)) + "." + strconv.FormatInt(p.Start, 10)
}

func (p Proc) key() procKey { return procKey{pid: p.PID, start: p.Start} }

type procKey struct {
	pid   int32
	start int64
}

// Attribution is one request's answer.
type Attribution struct {
	// Agent is the coding agent the request is attributed to, by canonical name: the one its
	// User-Agent names, or — for a pending answer — the agent whose process the calls wait
	// for. "" for none.
	Agent string
	// Proc is the agent process the request belongs to, as Proc.Key: the agent root of the
	// client's chain when the client is an agent's, the pending bucket's process for a
	// pending answer, else the client itself. "" when the client could not be looked up.
	Proc string
	// Session is a session the request belongs to, DefaultSessionID for a process of no
	// agent while an agent is active, or "": not named yet (Source pending) or nothing known
	// (fall back to the store's ActiveSession).
	Session string
	// Header is the session-id header that named Session when Source is header.
	Header string
	// Source says how Session was decided. pipeline.SessionSourcePending means "an agent
	// process that has named no session yet".
	Source pipeline.SessionSource
	// ChainSession is the live session the topmost process above the client named; see
	// pipeline.Context.ChainSession.
	ChainSession string
}

// PluginID is the session handed to plugins: a named session, the pending bucket for a
// pending answer, else "". The default bucket is never a plugin's session.
func (a Attribution) PluginID() string {
	switch {
	case a.Session != "" && a.Session != session.DefaultSessionID:
		return a.Session
	case a.Source == pipeline.SessionSourcePending:
		return session.PendingID(a.Agent, a.Proc)
	}
	return ""
}

// RecordID is the bucket a row is recorded under, or "" when the caller must fall back to
// the store's ActiveSession: the default bucket included, which plugins never hear.
func (a Attribution) RecordID() string {
	if a.Session != "" {
		return a.Session
	}
	if a.Source == pipeline.SessionSourcePending {
		return session.PendingID(a.Agent, a.Proc)
	}
	return ""
}

// Config is what an Attributor is built with.
type Config struct {
	// SessionIDHeaders are the headers that name a session, in order. Empty turns attribution
	// off: no session is any client's, so every answer falls back.
	SessionIDHeaders []string
	// ClientAffinity files a header-less request by its coding agent when its process is not
	// known.
	ClientAffinity bool
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Attributor resolves requests. Safe for concurrent use. Its lock is a leaf: it calls
// nothing that locks, so the store may call its Recorder hooks under the store's own lock.
type Attributor struct {
	cfg Config

	mu sync.Mutex
	// procs and lastProcClaim serve process attribution; see process.go.
	procs         map[procKey]*procState
	lastProcClaim time.Time
	// owners is client affinity's: the agent whose header first named each session.
	owners map[string]string
	// seen is when each session id was last claimed or recorded. It is attribution's own
	// answer to "does this session exist", which it used to ask the store.
	seen map[string]time.Time
}

// New returns an Attributor.
func New(cfg Config) *Attributor { return &Attributor{cfg: cfg} }

func (a *Attributor) now() time.Time {
	if a.cfg.Now != nil {
		return a.cfg.Now()
	}
	return time.Now()
}

// AgentOf is the coding agent a request's User-Agent names, by canonical name, else "".
func AgentOf(h http.Header) string {
	if h == nil {
		return ""
	}
	return pipeline.ParseUserAgent(h.Get("User-Agent")).AffinityName()
}

// Resolve attributes one request. h is the request's headers as the client sent them, nil
// where there are none (a transparent connection); chain is the client's process chain,
// nil when it is not known, and then process attribution does not apply.
//
// Not a pure query: a session header claims the session for the client's process and agent,
// which is what later header-less requests are filed by. It writes only the attributor's
// own state.
func (a *Attributor) Resolve(h http.Header, chain []Proc) Attribution {
	agent := AgentOf(h)
	if len(a.cfg.SessionIDHeaders) == 0 {
		out := Attribution{Agent: agent}
		if len(chain) > 0 {
			out.Proc = chain[0].Key()
		}
		return out
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	out := a.resolveLocked(h, agent, chain, now)
	if len(chain) > 0 {
		out.ChainSession = a.chainSessionLocked(chain, now)
	}
	return out
}

func (a *Attributor) resolveLocked(h http.Header, agent string, chain []Proc, now time.Time) Attribution {
	out := Attribution{Agent: agent}
	if len(chain) > 0 {
		out.Proc = chain[0].Key()
	}
	if sid, header := session.IDHeaderFrom(h, a.cfg.SessionIDHeaders); sid != "" {
		out.Session, out.Header, out.Source = sid, header, pipeline.SessionSourceHeader
		if len(chain) > 0 {
			out.Proc = a.claimProcessLocked(sid, agent, chain, now)
		}
		if a.cfg.ClientAffinity {
			a.claimLocked(sid, agent)
		}
		return out
	}
	if len(chain) > 0 {
		switch sid, pending := a.sessionForProcessLocked(chain, agent, now); {
		case pending != nil:
			out.Agent, out.Proc, out.Source = pending.agent, pending.proc, pipeline.SessionSourcePending
			return out
		case sid == session.DefaultSessionID:
			out.Session = sid
			return out
		case sid != "":
			out.Session, out.Source = sid, pipeline.SessionSourceProcess
			return out
		}
	}
	if a.cfg.ClientAffinity {
		switch sid := a.sessionForClientLocked(agent, now); {
		case sid == pendingAnswer:
			out.Proc, out.Source = "", pipeline.SessionSourcePending
		case sid == session.DefaultSessionID:
			out.Session = sid
		case sid != "":
			out.Session, out.Source = sid, pipeline.SessionSourceAffinity
		}
	}
	return out
}

// Touch records that a request chain[0] sent under sessionID has finished: a claim the
// client or its agent root holds on sessionID counts as made now, so a process serving
// several sessions answers with the one whose request finished last.
func (a *Attributor) Touch(sessionID string, chain []Proc) {
	if len(chain) == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.touchLocked(sessionID, chain, a.now())
}

// Record implements session.Recorder: a session the store appended to exists.
func (a *Attributor) Record(sessionID string, _ *pipeline.SessionEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seeLocked(sessionID, a.now())
}

// Cleared implements session.Clearer: every session is gone, and with them every claim.
func (a *Attributor) Cleared() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.procs, a.lastProcClaim, a.owners, a.seen = nil, time.Time{}, nil, nil
}

// Deleted implements session.Deleter.
func (a *Attributor) Deleted(sessionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.seen, sessionID)
	delete(a.owners, sessionID)
}

// Evicted implements session.Evicter: the store dropped id for max_sessions or session.ttl.
// A tool bound to it is rebound to its agent's live session on its next request.
func (a *Attributor) Evicted(sessionID string) { a.Deleted(sessionID) }

// Rekeyed implements session.Rekeyer, for the in-cluster default → A2A contextId rename.
func (a *Attributor) Rekeyed(oldID, newID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if t, ok := a.seen[oldID]; ok {
		delete(a.seen, oldID)
		a.seen[newID] = t
	}
	if o, ok := a.owners[oldID]; ok {
		delete(a.owners, oldID)
		a.owners[newID] = o
	}
}

// maxSeen bounds the seen table. Past it, ids not seen for procIdleTTL go first, then the
// oldest quarter.
const maxSeen = 4096

func (a *Attributor) seeLocked(id string, now time.Time) {
	if a.seen == nil {
		a.seen = make(map[string]time.Time)
	}
	if _, ok := a.seen[id]; !ok && len(a.seen) >= maxSeen {
		a.pruneSeenLocked(now)
	}
	a.seen[id] = now
}

func (a *Attributor) pruneSeenLocked(now time.Time) {
	for id, t := range a.seen {
		if now.Sub(t) > procIdleTTL {
			delete(a.seen, id)
		}
	}
	for len(a.seen) >= maxSeen {
		var oldest string
		var at time.Time
		for id, t := range a.seen {
			if oldest == "" || t.Before(at) {
				oldest, at = id, t
			}
		}
		delete(a.seen, oldest)
	}
}

// liveLocked reports whether id exists for attribution: claimed or recorded, and not since
// deleted, evicted or expired from the store.
func (a *Attributor) liveLocked(id string, _ time.Time) bool {
	_, ok := a.seen[id]
	return ok
}
```

- [ ] **Step 8: Write `core/attribution/process.go`**

Ported from `core/session/process.go`: same walk, same table bounds. What changed: no
`adoptLocked`, no `claimGrace` (a claim marks the session seen at once), and liveness is
`liveLocked`, not the store's map.

```go
package attribution

import (
	"sort"
	"strings"
	"time"

	"github.com/rossoctl/cortex/core/session"
)

// Process attribution files a request that carries no session header under the session of
// the PROCESS that sent it. The rules are the ones session.Store applied before this package
// existed, less adoption: a process that has named no session yet gets a pending answer, and
// its calls stay in their bucket. Walking the chain from the client up, at each process it
// knows:
//
//  1. Named a session through its own session header: its newest live one. A process that
//     never named one — a tool an agent's shell ran — is bound to that answer the first
//     time it is seen, and keeps it for its life.
//  2. An agent's (its requests carry a known coding agent's User-Agent) that has named no
//     session yet: a pending answer for that agent's root. Stopping here is what keeps a
//     nested agent — a claude -p run from another session's shell — out of its parent's
//     session.
//  3. Named sessions, none of them live now: "", so resolution falls back.
//  4. Bound earlier: that session.
//
// No agent anywhere in the chain is the default bucket while some process has named a
// session within ambiguityWindow, and "" otherwise.

// procState is what the attributor knows about one process.
type procState struct {
	// claims are the sessions this process named through its own session header — or a
	// process of the same agent directly below it named (see agentRootIndexLocked) — each
	// with when it was last active.
	claims map[string]time.Time
	// bound is the session a process that never named one was filed under the first time
	// it was seen; "" until then.
	bound string
	// agent is the coding agent this process's requests identify as by User-Agent.
	agent string
	// claimed is set when the process — or a process of the same agent below it — names a
	// session, and stays set for as long as the table keeps the process.
	claimed bool
	seen    time.Time
}

// maxProcs caps the process table; procIdleTTL is how long a quiet process is kept once it
// is full. A live agent re-names its session on every headered request, so only a process
// that has gone quiet, almost always one that has exited, is that old.
const (
	maxProcs    = 4096
	procIdleTTL = 24 * time.Hour
)

// ambiguityWindow is how recently a known agent must have sent traffic to count toward the
// ambiguous answers. Expiry cannot serve: session.ttl defaults to never.
const ambiguityWindow = 5 * time.Minute

// pendingRef is a pending answer: the agent whose process has named no session, and that
// process as Proc.Key.
type pendingRef struct{ agent, proc string }

// claimProcessLocked records that chain[0] named sessionID, on chain[0] and on its agent
// root, so a launcher and its worker answer with one session. It returns the root's key,
// the process the request is attributed to.
func (a *Attributor) claimProcessLocked(sessionID, agent string, chain []Proc, now time.Time) string {
	if len(sessionID) > session.MaxSessionIDLen {
		sessionID = sessionID[:session.MaxSessionIDLen]
	}
	a.lastProcClaim = now
	self := a.procLocked(chain[0], now)
	// A process that names a session is never a tool bound to another's.
	self.bound = ""
	if agent != "" {
		self.agent = agent
	}
	root := a.agentRootIndexLocked(chain, self.agent)
	for _, i := range []int{0, root} {
		st := a.procLocked(chain[i], now)
		if st.claims == nil {
			st.claims = make(map[string]time.Time, 1)
		}
		st.claims[sessionID] = now
		st.claimed = true
	}
	// A claim makes the session exist for attribution before the store has recorded the
	// request that carried it, which is what claimGrace used to cover.
	a.seeLocked(sessionID, now)
	return chain[root].Key()
}

func (a *Attributor) touchLocked(sessionID string, chain []Proc, now time.Time) {
	self := a.procs[chain[0].key()]
	if self == nil {
		return
	}
	for _, i := range []int{0, a.agentRootIndexLocked(chain, self.agent)} {
		if st := a.procs[chain[i].key()]; st != nil {
			if _, ok := st.claims[sessionID]; ok {
				st.claims[sessionID] = now
			}
		}
	}
}

// sessionForProcessLocked is where a header-less request from chain[0] goes, by the rules at
// the top of this file: a session, DefaultSessionID, a pending answer, or "" to fall back.
func (a *Attributor) sessionForProcessLocked(chain []Proc, agent string, now time.Time) (string, *pendingRef) {
	self := a.procLocked(chain[0], now)
	if agent != "" {
		// An agent's process is never a tool bound to another session. Its first request
		// may have been a CONNECT, which carries no User-Agent, and been bound before it
		// said whose it was.
		self.agent, self.bound = agent, ""
	}
	for i, p := range chain {
		st := self
		if i > 0 {
			if st = a.procs[p.key()]; st == nil {
				continue
			}
		}
		if sid := a.newestClaimLocked(st, now); sid != "" {
			if i > 0 && self.agent == "" && !self.claimed {
				self.bound = sid
			}
			return sid, nil
		}
		if st.agent != "" {
			root := i + a.agentRootIndexLocked(chain[i:], st.agent)
			if root != i {
				if rs := a.procs[chain[root].key()]; rs != nil {
					if sid := a.newestClaimLocked(rs, now); sid != "" {
						if i > 0 && self.agent == "" && !self.claimed {
							self.bound = sid
						}
						return sid, nil
					}
				}
			}
			return "", &pendingRef{agent: st.agent, proc: chain[root].Key()}
		}
		if st.claimed {
			// A process that names its own sessions is no other session's, so with none
			// of them live the walk stops here and resolution falls back.
			return "", nil
		}
		if st.bound != "" {
			if a.liveLocked(st.bound, now) {
				if i > 0 && self.agent == "" && !self.claimed {
					self.bound = st.bound
				}
				return st.bound, nil
			}
			st.bound = ""
		}
	}
	if !a.lastProcClaim.IsZero() && now.Sub(a.lastProcClaim) <= ambiguityWindow {
		return session.DefaultSessionID, nil
	}
	return "", nil
}

// chainSessionLocked is the live session the topmost process above chain[0] named, "" when
// none above it holds one. chain[0]'s agent root and the processes of that agent below it
// are chain[0]'s own and are not consulted.
func (a *Attributor) chainSessionLocked(chain []Proc, now time.Time) string {
	if len(chain) < 2 {
		return ""
	}
	agent := ""
	if self := a.procs[chain[0].key()]; self != nil {
		agent = self.agent
	}
	for i := len(chain) - 1; i > a.agentRootIndexLocked(chain, agent); i-- {
		if st := a.procs[chain[i].key()]; st != nil {
			if sid := a.newestClaimLocked(st, now); sid != "" {
				return sid
			}
		}
	}
	return ""
}

// ProcessHints are the PIDs of processes that have named a session, most recently seen
// first and at most limit of them: where a lookup for a new connection looks first.
func (a *Attributor) ProcessHints(limit int) []int32 {
	if limit <= 0 {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	type seenProc struct {
		pid  int32
		seen time.Time
	}
	var claimers []seenProc
	for k, st := range a.procs {
		if st.claimed {
			claimers = append(claimers, seenProc{pid: k.pid, seen: st.seen})
		}
	}
	sort.Slice(claimers, func(i, j int) bool { return claimers[i].seen.After(claimers[j].seen) })
	if len(claimers) > limit {
		claimers = claimers[:limit]
	}
	out := make([]int32, len(claimers))
	for i, c := range claimers {
		out[i] = c.pid
	}
	return out
}

// IsAgentProcess reports whether p — or a process of the same agent it runs — has named a
// session through its own header.
func (a *Attributor) IsAgentProcess(p Proc) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.procs[p.key()]
	return st != nil && st.claimed
}

// procLocked is p's state, created on first sight and stamped as seen now.
func (a *Attributor) procLocked(p Proc, now time.Time) *procState {
	if a.procs == nil {
		a.procs = make(map[procKey]*procState)
	}
	k := p.key()
	st, ok := a.procs[k]
	if !ok {
		if len(a.procs) >= maxProcs {
			a.pruneProcsLocked(now)
		}
		st = &procState{}
		a.procs[k] = st
	}
	st.seen = now
	return st
}

// pruneProcsLocked makes room in the process table: every process not heard from in
// procIdleTTL, then, if that freed nothing below the cap, the least recently seen quarter,
// processes that never named a session first.
func (a *Attributor) pruneProcsLocked(now time.Time) {
	for k, st := range a.procs {
		if now.Sub(st.seen) > procIdleTTL {
			delete(a.procs, k)
		}
	}
	if len(a.procs) < maxProcs {
		return
	}
	type aged struct {
		k       procKey
		claimed bool
		seen    time.Time
	}
	all := make([]aged, 0, len(a.procs))
	for k, st := range a.procs {
		all = append(all, aged{k: k, claimed: st.claimed, seen: st.seen})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].claimed != all[j].claimed {
			return !all[i].claimed
		}
		return all[i].seen.Before(all[j].seen)
	})
	for _, x := range all[:len(all)/4] {
		delete(a.procs, x.k)
	}
}

// agentRootIndexLocked is the index in chain of the topmost process of agent reached from
// chain[0] through processes of that same agent only. A claude -p started from a shell is
// its own root, because the shell between it and the outer claude is no agent's.
func (a *Attributor) agentRootIndexLocked(chain []Proc, agent string) int {
	if agent == "" {
		return 0
	}
	i := 0
	for i+1 < len(chain) {
		st := a.procs[chain[i+1].key()]
		if st == nil || st.agent != agent {
			break
		}
		i++
	}
	return i
}

// newestClaimLocked is the live session st has named most recently, "" when none is.
func (a *Attributor) newestClaimLocked(st *procState, now time.Time) string {
	var best string
	var at time.Time
	for id, claimedAt := range st.claims {
		if !a.liveLocked(id, now) {
			delete(st.claims, id)
			continue
		}
		if best == "" || claimedAt.After(at) {
			best, at = id, claimedAt
		}
	}
	return best
}

// pendingOwner is the agent a pending bucket belongs to — claude-code for both
// pending:claude-code and pending:claude-code@4242.1 — and false for any other id.
func pendingOwner(id string) (string, bool) {
	rest, ok := strings.CutPrefix(id, session.PendingPrefix)
	if !ok {
		return "", false
	}
	agent, _, _ := strings.Cut(rest, "@")
	return agent, true
}
```

- [ ] **Step 9: Write `core/attribution/affinity.go`**

Ported from `core/session/affinity.go`, minus `Adopt` and `followAdoptedLocked`.

```go
package attribution

import (
	"time"

	"github.com/rossoctl/cortex/core/session"
)

// Client affinity files a header-less request whose process is not known under a session
// of the SAME coding agent, by User-Agent, rather than under the store's ActiveSession().

// pendingAnswer is sessionForClientLocked's "this agent has named nothing yet".
const pendingAnswer = "\x00pending"

// claimLocked records that client named sessionID through its own header. First claim
// wins: a second agent quoting the same id does not take it over.
func (a *Attributor) claimLocked(sessionID, client string) {
	if sessionID == "" || client == "" {
		return
	}
	if len(sessionID) > session.MaxSessionIDLen {
		sessionID = sessionID[:session.MaxSessionIDLen]
	}
	if a.owners == nil {
		a.owners = make(map[string]string)
	}
	if _, owned := a.owners[sessionID]; !owned {
		if len(a.owners) >= maxSeen {
			for id := range a.owners {
				if _, ok := a.seen[id]; !ok {
					delete(a.owners, id)
				}
			}
		}
		a.owners[sessionID] = client
	}
	a.seeLocked(sessionID, a.now())
}

// sessionForClientLocked is where a request with no session header and no known process
// goes under client affinity:
//
//  1. A known client: its newest live session, else pendingAnswer.
//  2. An unknown client while two or more known clients have each sent traffic within
//     ambiguityWindow: the default bucket.
//  3. Anything else: "", so ActiveSession() answers. This is the in-cluster case.
func (a *Attributor) sessionForClientLocked(client string, now time.Time) string {
	if client != "" {
		var newest string
		var at time.Time
		for id, owner := range a.owners {
			if owner != client || !a.liveLocked(id, now) {
				continue
			}
			if t := a.seen[id]; newest == "" || t.After(at) {
				newest, at = id, t
			}
		}
		if newest != "" {
			return newest
		}
		return pendingAnswer
	}
	seen := make(map[string]struct{}, 2)
	for id, t := range a.seen {
		if now.Sub(t) > ambiguityWindow {
			continue
		}
		owner := a.owners[id]
		if owner == "" {
			var ok bool
			if owner, ok = pendingOwner(id); !ok {
				continue
			}
		}
		seen[owner] = struct{}{}
		if len(seen) >= 2 {
			return session.DefaultSessionID
		}
	}
	return ""
}
```

- [ ] **Step 10: Run the tests, vet and format**

Run: `cd core && gofmt -l attribution session && go vet ./attribution ./session && go test -race ./attribution ./session`
Expected: no gofmt output; `ok` for both packages.

- [ ] **Step 11: Commit**

```bash
git add core/attribution core/session/pending.go core/session/pending_test.go
git commit -s -m "feat: Add core/attribution, the resolution rules without the store

Ported from session.Store's process and client-affinity rules, minus adoption.
It keeps its own claims table and learns which sessions exist from the store's
Recorder hooks, so it never calls the store. Nothing uses it yet.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 2: Events carry their process; the fold records who named each session

Additive only: the listener does not fill the new fields until Task 3, so nothing visible
changes. `agentOfLocked` and `Adopted` stay until Task 3 too.

**Files:**
- Modify: `core/pipeline/session.go` (two `SessionEvent` fields, their wire fields, both marshal directions)
- Create: `core/session/procs.go`
- Modify: `core/session/pending.go` (`IsConversation`)
- Modify: `core/session/store.go` (entry fields, append fold, `SessionSummary` fields, `ListSessions`)
- Modify: `core/session/fold.go` (fold state, `Add`, `Summary`, `foldJSON`, `Clone`)
- Modify: `core/session/history.go` (`withPriorLocked`)
- Modify: `core/sessionapi/summary_test.go` (the shape guard: 28 → 30 fields, both kept by `view=summary`)
- Test: `core/pipeline/proc_test.go`, `core/session/procs_test.go`

**Interfaces:**
- Consumes: `session.PendingID` (Task 1).
- Produces:
  - `pipeline.SessionEvent.Proc string`, `.SessionHeader string` (wire `proc`, `sessionHeader`)
  - `session.ProcSpan{First, Last time.Time}`
  - `session.SessionSummary.Procs []string`, `.LastRequestAt time.Time`, `.LinkedTo []string`, `.LinkedAll bool`, unexported `.procSpans map[string]ProcSpan`
  - `session.IsConversation(id string) bool`
  - `applyProcs(procs, sessionID, e)`, `applyOwner(owner, sessionID, agentName, e)`, `pickAgent(sessionID, owner, earlier, agents)`, `mergeProcs`, `procsList` (package-private, used by Tasks 3–4)

- [ ] **Step 1: Write the failing event round-trip test**

`core/pipeline/proc_test.go`:

```go
package pipeline

import (
	"strings"
	"testing"
)

// The process and the naming header ride every event to agentop and into the archive, which
// is where session.Link reads them back from.
func TestSessionEvent_ProcAndSessionHeaderRoundTrip(t *testing.T) {
	in := SessionEvent{Phase: SessionRequest, Proc: "100.5", SessionHeader: "X-Claude-Code-Session-Id"}
	raw, err := in.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"proc":"100.5"`, `"sessionHeader":"X-Claude-Code-Session-Id"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("wire form %s lacks %s", raw, key)
		}
	}
	var out SessionEvent
	if err := out.UnmarshalJSON(raw); err != nil {
		t.Fatal(err)
	}
	if out.Proc != in.Proc || out.SessionHeader != in.SessionHeader {
		t.Errorf("after the round trip: Proc %q SessionHeader %q", out.Proc, out.SessionHeader)
	}
	if bare, _ := (SessionEvent{Phase: SessionRequest}).MarshalJSON(); strings.Contains(string(bare), "proc") || strings.Contains(string(bare), "sessionHeader") {
		t.Errorf("an event naming no process carries the keys anyway: %s", bare)
	}
}
```

- [ ] **Step 2: Run it to see it fail**

Run: `cd core && go test ./pipeline -run ProcAndSessionHeader`
Expected: FAIL, `unknown field Proc in struct literal`

- [ ] **Step 3: Add the fields to `SessionEvent`**

In `core/pipeline/session.go`, after `ParentSubagent string` in `SessionEvent`:

```go
	// Proc is the agent process the request belongs to, "<pid>.<start>", as process
	// attribution named it (attribution.Attribution.Proc): the agent root of the client's
	// chain, so a launcher and its worker are one. Empty where the listener could not name
	// the process, which is everywhere but a loopback-bound install. It is what links a
	// process's calls from before it named a session to that session when the list is read
	// (session.Link), so it is recorded on rows in a session as well as on pending ones.
	Proc string

	// SessionHeader is the session-id header that named this row's session, "" when the
	// session was not named by the request (process, affinity, ActiveSession). Recorded so
	// the fold can tell which processes NAMED a session from those merely filed into it,
	// which Link needs and the session's agent is decided by.
	SessionHeader string
```

After `ParentSubagent string \`json:"parentSubagent,omitempty"\`` in `sessionEventWire`:

```go
	// omitempty: absent wherever the listener could not name the client's process, and on
	// every event recorded before the field existed.
	Proc          string `json:"proc,omitempty"`
	SessionHeader string `json:"sessionHeader,omitempty"`
```

And `Proc: e.Proc, SessionHeader: e.SessionHeader,` in `MarshalJSON`'s literal,
`Proc: w.Proc, SessionHeader: w.SessionHeader,` in `UnmarshalJSON`'s.

- [ ] **Step 4: Run it to see it pass, then the sessionapi shape guard to see it fail**

Run: `cd core && go test ./pipeline -run ProcAndSessionHeader && go test ./sessionapi -run TestSummarizeEvent_ShapeIsGuarded`
Expected: pipeline PASS; sessionapi FAIL, `SessionEvent has 30 fields, this guard was written against 28`

- [ ] **Step 5: Keep both fields in `view=summary` and update the guard**

They are timeline data: agentop's fold (Task 5) reads them off projected events. In
`core/sessionapi/summary_test.go`, add `Proc: "100.5", SessionHeader: "X-Claude-Code-Session-Id",`
to the fixture beside `ParentSubagent`, this assertion after the `parentSubagent` one:

```go
	// proc is kept: agentop merges a session's linked process rows into its timeline by it.
	if got.Proc != full.Proc || got.SessionHeader != full.SessionHeader {
		t.Errorf("proc %q sessionHeader %q, want %q %q — the linked-row fold reads them",
			got.Proc, got.SessionHeader, full.Proc, full.SessionHeader)
	}
```

and change the guard row to:

```go
		// 30 since Proc and SessionHeader, TIMELINE data: agentop folds a session's linked
		// process rows by them. Asserted above.
		{"SessionEvent", reflect.TypeOf(pipeline.SessionEvent{}), 30},
```

`summarizeEvent` itself needs no change: it copies the struct and drops payloads only.

- [ ] **Step 6: Write the failing fold tests**

`core/session/procs_test.go`:

```go
package session

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// named is a request from proc that named its session through a header, at t.
func named(proc string, t time.Time) pipeline.SessionEvent {
	return pipeline.SessionEvent{At: t, Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
		Proc: proc, SessionHeader: ClaudeCodeSessionHeader}
}

// A session records the processes that NAMED it, each with its first and last such request.
// A response, a row filed by process, and a pending bucket's rows name nothing.
func TestListSessions_RecordsTheProcessesThatNamedTheSession(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	t0 := time.Unix(1_700_000_000, 0)
	s.Append("s1", named("100.5", t0))
	s.Append("s1", named("100.5", t0.Add(time.Minute)))
	s.Append("s1", named("200.7", t0.Add(2*time.Minute)))
	resp := named("300.9", t0.Add(3*time.Minute))
	resp.Phase = pipeline.SessionResponse
	s.Append("s1", resp)
	byProcess := named("400.1", t0.Add(4*time.Minute))
	byProcess.SessionHeader = ""
	s.Append("s1", byProcess)

	sum := summaryOf(t, s, "s1")
	if !slices.Equal(sum.Procs, []string{"100.5", "200.7"}) {
		t.Errorf("Procs = %v, want the two processes whose headers named s1", sum.Procs)
	}
	if got := sum.procSpans["100.5"]; !got.First.Equal(t0) || !got.Last.Equal(t0.Add(time.Minute)) {
		t.Errorf("100.5's span = %+v, want its first and last headered request", got)
	}
	if !sum.LastRequestAt.Equal(t0.Add(4 * time.Minute)) {
		t.Errorf("LastRequestAt = %v, want the newest request", sum.LastRequestAt)
	}
	s.Append(PendingID("claude-code", "100.5"), named("100.5", t0))
	if got := summaryOf(t, s, PendingID("claude-code", "100.5")).Procs; got != nil {
		t.Errorf("a pending bucket's Procs = %v, want none: a bucket names no session", got)
	}
}

// The fold persists procs, owner and lastRequestAt in session.json, and a document written
// before they existed decodes to none of them.
func TestSummaryFold_PersistsProcsAndDecodesAnOldDocument(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0).UTC()
	f := NewSummaryFold()
	e := named("100.5", t0)
	e.Client = &pipeline.EventClient{Name: "claude-code"}
	f.Add("s1", &e)
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var back SummaryFold
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	sum := back.Summary("s1")
	if !slices.Equal(sum.Procs, []string{"100.5"}) || !sum.LastRequestAt.Equal(t0) || back.owner != "claude-code" {
		t.Errorf("restored: Procs %v LastRequestAt %v owner %q", sum.Procs, sum.LastRequestAt, back.owner)
	}

	var old SummaryFold
	if err := json.Unmarshal([]byte(`{"eventCount":3,"title":"x"}`), &old); err != nil {
		t.Fatal(err)
	}
	if got := old.Summary("s1"); got.Procs != nil || !got.LastRequestAt.IsZero() {
		t.Errorf("an old document: Procs %v LastRequestAt %v, want neither", got.Procs, got.LastRequestAt)
	}
}

// A session resumed after a restart reports the processes that named it before, widened by
// the ones naming it now, so Link can reach calls made before the restart.
func TestListSessions_ProcsContinueFromTheEarlierHistory(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	earlier := NewSummaryFold()
	e := named("100.5", t0)
	earlier.Add("s1", &e)
	st := New(0, 0, 0)
	defer st.Close()
	st.AddRecorder(&priorKeeper{last: map[string]uint64{"s1": 1}, prior: map[string]Prior{"s1": {Fold: earlier}}})
	st.Append("s1", named("100.5", t0.Add(time.Hour)))
	st.Append("s1", named("200.7", t0.Add(2*time.Hour)))
	sum := summaryOf(t, st, "s1")
	if !slices.Equal(sum.Procs, []string{"100.5", "200.7"}) {
		t.Fatalf("Procs = %v", sum.Procs)
	}
	if got := sum.procSpans["100.5"]; !got.First.Equal(t0) || !got.Last.Equal(t0.Add(time.Hour)) {
		t.Errorf("100.5's span = %+v, want it to run from before the restart to after", got)
	}
}

// The procs map is bounded: a client that sends a new process key on every request cannot
// grow one session's summary without limit.
func TestApplyProcs_IsBounded(t *testing.T) {
	var procs map[string]ProcSpan
	t0 := time.Unix(1_700_000_000, 0)
	for i := range maxProcsPerSession + 10 {
		e := named(string(rune('A'+i%26))+time.Duration(i).String(), t0.Add(time.Duration(i)*time.Second))
		procs = applyProcs(procs, "s1", &e)
	}
	if len(procs) != maxProcsPerSession {
		t.Errorf("%d processes held, want %d", len(procs), maxProcsPerSession)
	}
}
```

- [ ] **Step 7: Run them to see them fail**

Run: `cd core && go test ./session -run 'Procs|ApplyProcs'`
Expected: FAIL, `undefined: applyProcs` / `sum.Procs undefined`

- [ ] **Step 8: Write `core/session/procs.go`**

```go
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
	if e.Proc == "" || e.SessionHeader == "" || e.Phase != pipeline.SessionRequest || !IsConversation(sessionID) {
		return procs
	}
	sp, ok := procs[e.Proc]
	if !ok {
		if procs == nil {
			procs = make(map[string]ProcSpan, 1)
		}
		if len(procs) >= maxProcsPerSession {
			delete(procs, oldestProc(procs))
		}
		sp.First = e.At
	}
	if e.At.After(sp.Last) {
		sp.Last = e.At
	}
	if e.At.Before(sp.First) {
		sp.First = e.At
	}
	procs[e.Proc] = sp
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
```

Append to `core/session/pending.go` (and add `import "strings"`):

```go
// IsConversation reports whether id is one conversation's session: false for "", the
// default bucket, and any pending bucket, each of which holds many conversations' calls or
// none. Plugins that key state on a session (the inference-router's pins, cpex's taint)
// skip the others.
func IsConversation(id string) bool {
	return id != "" && id != DefaultSessionID && !strings.HasPrefix(id, PendingPrefix)
}
```

- [ ] **Step 9: Fold the fields in the store, the fold and the list**

Apply this diff (it is the whole of the store/fold/history change; `go test` in the next step
checks it):

```diff
diff --git a/core/pipeline/session.go b/core/pipeline/session.go
index f29d6c7e..ea5c695c 100644
--- a/core/pipeline/session.go
+++ b/core/pipeline/session.go
@@ -340,6 +340,20 @@ type SessionEvent struct {
 	//
 	// Client-asserted, with the same caveat as Client above.
 	ParentSubagent string
+
+	// Proc is the agent process the request belongs to, "<pid>.<start>", as process
+	// attribution named it (attribution.Attribution.Proc): the agent root of the client's
+	// chain, so a launcher and its worker are one. Empty where the listener could not name
+	// the process, which is everywhere but a loopback-bound install. It is what links a
+	// process's calls from before it named a session to that session when the list is read
+	// (session.Link), so it is recorded on rows in a session as well as on pending ones.
+	Proc string
+
+	// SessionHeader is the session-id header that named this row's session, "" when the
+	// session was not named by the request (process, affinity, ActiveSession). Recorded so
+	// the fold can tell which processes NAMED a session from those merely filed into it,
+	// which Link needs and the session's agent is decided by.
+	SessionHeader string
 }
 
 // TunnelReason is why an opaque tunnel stayed opaque.
@@ -547,6 +561,10 @@ type sessionEventWire struct {
 	Subagent string `json:"subagent,omitempty"`
 	// omitempty: absent on every row but those of an agent another agent started.
 	ParentSubagent string `json:"parentSubagent,omitempty"`
+	// omitempty: absent wherever the listener could not name the client's process, and on
+	// every event recorded before the field existed.
+	Proc          string `json:"proc,omitempty"`
+	SessionHeader string `json:"sessionHeader,omitempty"`
 }
 
 func (e SessionEvent) MarshalJSON() ([]byte, error) {
@@ -579,6 +597,8 @@ func (e SessionEvent) MarshalJSON() ([]byte, error) {
 		Client:          e.Client,
 		Subagent:        e.Subagent,
 		ParentSubagent:  e.ParentSubagent,
+		Proc:            e.Proc,
+		SessionHeader:   e.SessionHeader,
 	})
 }
 
@@ -619,6 +639,8 @@ func (e *SessionEvent) UnmarshalJSON(data []byte) error {
 		Client:          w.Client,
 		Subagent:        w.Subagent,
 		ParentSubagent:  w.ParentSubagent,
+		Proc:            w.Proc,
+		SessionHeader:   w.SessionHeader,
 	}
 	return nil
 }
diff --git a/core/session/fold.go b/core/session/fold.go
index f4f3fe4b..148f5bbf 100644
--- a/core/session/fold.go
+++ b/core/session/fold.go
@@ -5,6 +5,7 @@ import (
 	"maps"
 	"slices"
 	"strings"
+	"time"
 
 	"github.com/rossoctl/cortex/core/cost/usage"
 	"github.com/rossoctl/cortex/core/pipeline"
@@ -181,6 +182,11 @@ type SummaryFold struct {
 	restoredContext *pipeline.PromptContext
 	// inferenceHost is SessionSummary.InferenceHost: the latest inference request's host.
 	inferenceHost string
+	// procs, owner and lastRequestAt are the entry's fields of the same names, by the same
+	// rules (applyProcs, applyOwner).
+	procs         map[string]ProcSpan
+	owner         string
+	lastRequestAt time.Time
 }
 
 // NewSummaryFold returns an empty fold.
@@ -191,6 +197,7 @@ func (f *SummaryFold) Clone() *SummaryFold {
 	c := *f
 	c.units = maps.Clone(f.units)
 	c.agents = slices.Clone(f.agents)
+	c.procs = maps.Clone(f.procs)
 	return &c
 }
 
@@ -217,6 +224,11 @@ func (f *SummaryFold) Add(sessionID string, e *pipeline.SessionEvent) {
 	if h := inferenceHostOf(e); h != "" {
 		f.inferenceHost = h
 	}
+	f.procs = applyProcs(f.procs, sessionID, e)
+	f.owner = applyOwner(f.owner, sessionID, p.agentName, e)
+	if e.Phase == pipeline.SessionRequest && e.At.After(f.lastRequestAt) {
+		f.lastRequestAt = e.At
+	}
 }
 
 // promptContext is the fold's CTX figure, nil when nothing can be said.
@@ -239,16 +251,10 @@ func (f *SummaryFold) Summary(sessionID string) SessionSummary {
 		Saturated:     f.cost.Saturated || f.avoided.Saturated,
 		PromptContext: f.promptContext(),
 		InferenceHost: f.inferenceHost,
-	}
-	if sessionID != DefaultSessionID && !strings.HasPrefix(sessionID, PendingPrefix) {
-		// Recognised first, then unrecognised, by agentOfLocked's rule and for its reasons — the
-		// store's own preference for the owner that CLAIMED the session is the one difference this
-		// type's godoc names, and it is not reachable from here.
-		if l := firstAgentLabel(nil, f.agents, true); l != "" {
-			sum.Agent = l
-		} else {
-			sum.Agent = firstAgentLabel(nil, f.agents, false)
-		}
+		Procs:         procsList(f.procs),
+		procSpans:     f.procs,
+		LastRequestAt: f.lastRequestAt,
+		Agent:         pickAgent(sessionID, f.owner, nil, f.agents),
 	}
 	return sum
 }
@@ -274,6 +280,11 @@ type foldJSON struct {
 	// THERE "" MEANS UNKNOWN, NOT "NO INFERENCE TRAFFIC": that history may hold plenty, folded by
 	// a Cortex that did not record where it went. The session's next inference request sets it.
 	InferenceHost string `json:"inferenceHost,omitempty"`
+	// Procs, Owner and LastRequestAt are absent from a session.json written before they
+	// existed: that history names no process, links nothing, and its agent is picked by label.
+	Procs         map[string]ProcSpan `json:"procs,omitempty"`
+	Owner         string              `json:"owner,omitempty"`
+	LastRequestAt time.Time           `json:"lastRequestAt,omitzero"`
 }
 
 // foldAgent is one sessionAgent in session.json.
@@ -304,6 +315,9 @@ func (f *SummaryFold) MarshalJSON() ([]byte, error) {
 		Units:            f.units,
 		PromptContext:    f.promptContext(),
 		InferenceHost:    f.inferenceHost,
+		Procs:            f.procs,
+		Owner:            f.owner,
+		LastRequestAt:    f.lastRequestAt,
 	}
 	for _, a := range f.agents {
 		j.Agents = append(j.Agents, foldAgent{Name: a.name, Label: a.label, Unknown: !a.known})
@@ -327,6 +341,9 @@ func (f *SummaryFold) UnmarshalJSON(b []byte) error {
 		title:           j.Title,
 		restoredContext: j.PromptContext,
 		inferenceHost:   j.InferenceHost,
+		procs:           j.Procs,
+		owner:           pipeline.CanonicalAgent(j.Owner),
+		lastRequestAt:   j.LastRequestAt,
 	}
 	if j.TitleRank != nil {
 		f.titleRank = *j.TitleRank
diff --git a/core/session/history.go b/core/session/history.go
index ea517fc1..eae12f4f 100644
--- a/core/session/history.go
+++ b/core/session/history.go
@@ -191,4 +191,13 @@ func (s *Store) withPriorLocked(sum *SessionSummary, id string, sess *entry) {
 	if sum.InferenceHost == "" && f.inferenceHost != "" {
 		sum.InferenceHost, sum.InferenceHostFromHistory = f.inferenceHost, true
 	}
+	// The processes that named the session before this entry still named it, so a resumed
+	// session links calls a process made before the restart.
+	if len(f.procs) > 0 {
+		sum.procSpans = mergeProcs(f.procs, sum.procSpans)
+		sum.Procs = procsList(sum.procSpans)
+	}
+	if f.lastRequestAt.After(sum.LastRequestAt) {
+		sum.LastRequestAt = f.lastRequestAt
+	}
 }
diff --git a/core/session/store.go b/core/session/store.go
index 750d8a9c..680d92c6 100644
--- a/core/session/store.go
+++ b/core/session/store.go
@@ -166,6 +166,13 @@ type entry struct {
 	// A LATEST VALUE, NOT A SUM, so in context's class above: a trim has nothing to subtract, and
 	// the request that set it is the newest of its kind, which is the last a trim would take.
 	inferenceHost string
+
+	// procs is each process that NAMED this session through its own header, with when it first
+	// and last did (applyProcs); owner is the agent whose header named it first (applyOwner).
+	// Neither is shed on trim. lastRequestAt is the newest request's time, for Link.
+	procs         map[string]ProcSpan
+	owner         string
+	lastRequestAt time.Time
 }
 
 // MaxSessionIDLen is the longest session ID the store keeps intact; longer ids
@@ -570,6 +577,11 @@ func (s *Store) appendLocked(sessionID string, b *Bucket, event pipeline.Session
 	if h := inferenceHostOf(&event); h != "" {
 		sess.inferenceHost = h
 	}
+	sess.procs = applyProcs(sess.procs, sessionID, &event)
+	sess.owner = applyOwner(sess.owner, sessionID, agentName, &event)
+	if event.Phase == pipeline.SessionRequest && event.At.After(sess.lastRequestAt) {
+		sess.lastRequestAt = event.At
+	}
 	if b == nil {
 		sess.UpdatedAt = now
 		s.activeID = sessionID
@@ -1031,6 +1043,22 @@ type SessionSummary struct {
 	// omitempty on a bool drops exactly the false the wire needs; absent then means "resident",
 	// so a client reading a proxy that predates the archive sees every row as it always did.
 	Resident *bool `json:"resident,omitempty"`
+
+	// Procs are the processes that named this session through their own session header,
+	// "<pid>.<start>", sorted. Absent when none did, and on a summary from before the field.
+	Procs []string `json:"procs,omitempty"`
+	// LastRequestAt is the time of the newest request this session holds, zero when it holds
+	// none. On a pending bucket it is what Link compares with a process's claims.
+	LastRequestAt time.Time `json:"lastRequestAt,omitzero"`
+	// LinkedTo, on a pending bucket, is the sessions its process went on to name, in the order
+	// it named them; LinkedAll says every request in the bucket falls before one of them. Set
+	// by Link when the list is read, never stored. agentop folds a fully linked bucket into
+	// those sessions and lists a partly linked one as its own row.
+	LinkedTo  []string `json:"linkedTo,omitempty"`
+	LinkedAll bool     `json:"linkedAll,omitempty"`
+
+	// procSpans is Procs with each process's span, for Link; not on the wire.
+	procSpans map[string]ProcSpan
 }
 
 // sessionAgent is one client's first label in a session.
@@ -1150,6 +1178,9 @@ func (s *Store) ListSessions() []SessionSummary {
 			PromptContext: sess.context.Publish(),
 			// Read, not computed, like Title: Append folds it.
 			InferenceHost: sess.inferenceHost,
+			Procs:         procsList(sess.procs),
+			procSpans:     sess.procs,
+			LastRequestAt: sess.lastRequestAt,
 		}
 		s.withPriorLocked(&sum, id, sess)
 		out = append(out, sum)
diff --git a/core/sessionapi/summary_test.go b/core/sessionapi/summary_test.go
index 17de745e..c21d568f 100644
--- a/core/sessionapi/summary_test.go
+++ b/core/sessionapi/summary_test.go
@@ -83,6 +83,8 @@ func fullEvent() pipeline.SessionEvent {
 		// Set only on a Claude Code subagent's requests; the parent only on a nested one's.
 		Subagent:       "a1a2e36cb5946f31c",
 		ParentSubagent: "a0974b0b8591dc76b",
+		Proc:           "100.5",
+		SessionHeader:  "X-Claude-Code-Session-Id",
 	}
 }
 
@@ -167,6 +169,11 @@ func TestSummarizeEvent_DropsPayloadsKeepsTimelineFields(t *testing.T) {
 		t.Errorf("parentSubagent = %q, want %q — the tree of agents is built from it",
 			got.ParentSubagent, full.ParentSubagent)
 	}
+	// proc is kept: agentop merges a session's linked process rows into its timeline by it.
+	if got.Proc != full.Proc || got.SessionHeader != full.SessionHeader {
+		t.Errorf("proc %q sessionHeader %q, want %q %q — the linked-row fold reads them",
+			got.Proc, got.SessionHeader, full.Proc, full.SessionHeader)
+	}
 	if got.Direction != full.Direction || got.Phase != full.Phase {
 		t.Error("direction/phase did not survive")
 	}
@@ -360,7 +367,9 @@ func TestSummarizeEvent_ShapeIsGuarded(t *testing.T) {
 		// 27 since Subagent, which is TIMELINE data: it says which agent made the row. A
 		// scalar capped at maxClientLen, so the struct copy keeps it; asserted above.
 		// 28 since ParentSubagent, TIMELINE data for the same reason; asserted above.
-		{"SessionEvent", reflect.TypeOf(pipeline.SessionEvent{}), 28},
+		// 30 since Proc and SessionHeader, TIMELINE data: agentop folds a session's linked
+		// process rows by them. Asserted above.
+		{"SessionEvent", reflect.TypeOf(pipeline.SessionEvent{}), 30},
 		// 25 since AgentRole, which is TIMELINE data: agentop's CONTEXT gauge reads it on every
 		// row the timeline serves. It needs no assertion of its own in the projection test
 		// beyond the equality one there — a scalar survives the struct copy, unlike the two
```

- [ ] **Step 10: Run the tests, vet and format**

Run: `cd core && gofmt -l session pipeline sessionapi && go vet ./session ./pipeline ./sessionapi && go test -race ./session/... ./pipeline ./sessionapi`
Expected: no gofmt output; all `ok`.

- [ ] **Step 11: Commit**

```bash
git add core/pipeline core/session core/sessionapi/summary_test.go
git commit -s -m "feat: Record which processes named each session

Events gain proc and sessionHeader, and a session's fold keeps, per process
that named it through a header, its first and last such request. The archive
persists both. Nothing fills the fields yet.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 3: Switch over — the forward proxy resolves through `core/attribution`; adoption goes

The behaviour change. The store loses `Claim`, `ClaimProcess`, `SessionForClient`,
`SessionForProcess`, `TouchProcess`, `ChainSession`, `ProcessHints`, `IsAgentProcess`, the
`owners`/`adopted`/`procs` maps and `SessionSummary.Adopted`. The forward proxy builds one
`Attributor` lazily and registers it as a store recorder. Every row it records carries
`proc`, and the request row the header that named its session.

Reference implementation: commit `4f6f93ca` on `design/link-not-adopt` (validated; see
"Validation" at the end). Apply it in this order:

**Files:**
- Delete: `core/session/affinity.go`, `core/session/process.go`, `core/session/affinity_test.go`, `core/session/process_test.go`
- Modify: `core/session/pending.go` (gains `PendingPrefix`, `PendingSessionID`, `Rekeyer`, new `Evicter`)
- Modify: `core/session/store.go` (drop the maps, `followAdoptedLocked`, `agentOfLocked`, `adoptedByLocked`, `Adopted`; `ListSessions` uses `pickAgent(id, sess.owner, nil, sess.agents)`; `cleanupLocked`/`evictOldestLocked` call `evictedLocked`; `AddRecorder` takes the lock)
- Modify: `core/session/history.go` (`Clear` drops the maps; `withPriorLocked` uses `pickAgent` with the earlier fold's owner first)
- Create: `core/session/helpers_test.go` (`ev`, `procTestClock`, `newProcStore`, `recordIn`, `rekeyRecorder`, `claimedBy`)
- Modify tests: `agent_test.go` (owner = first headered request's agent), `clear_test.go`, `delete_test.go`, `continuity_test.go` (`Claim` → `Append(claimedBy(...))`); delete `TestAppendTrailing_FollowsAnAdoption`, `TestAppendPair_ReturnsTheBucketBothWentInto`, `TestClaim_LeavesThePendingBucketWhenTheTargetIsArchived`, `TestDelete_AnIDMemoryDoesNotHoldKeepsItsAdoption`, `TestListSessions_NamesTheBucketsASessionAdopted`; `archive_test.go` drops the `claim` rename case
- Create: `core/pipeline/attributed.go` (`SetAttribution`, `AttributedProc`, `NamingHeader`; first call wins) and its three fields in `Context`
- Modify: `core/listener/forwardproxy/server.go`, `process.go`, `transparent.go`, `core/listener/internal/sessionevent/sessionevent.go`
- Modify: `core/plugins/inferencerouter/plugin.go` (`synthetic` = `!session.IsConversation`), `core/plugins/cpex/cmf_body.go`, `cmd/agentop/cmd_server_write.go`
- Modify: `cmd/agentop/tui/sessions_pane.go`, `agents_other.go`, `agent_scope_test.go` (drop `adoptedSessionIDs`)
- Modify forwardproxy tests: `session_affinity_test.go` (seed claims through the proxy; Bob's pre-header calls now stay in `pending:bob`), `process_test.go` (`TestProcessAttribution_AProcessAdoptsItsOwnPendingBucketFirst` → `TestProcessAttribution_RecordsTheProcessOnEveryRow`), `session.Proc` → `attribution.Proc` everywhere

- [ ] **Step 1: Write the failing black-box test**

In `core/listener/forwardproxy/process_test.go`, replace
`TestProcessAttribution_AProcessAdoptsItsOwnPendingBucketFirst` with
`TestProcessAttribution_RecordsTheProcessOnEveryRow` (reference commit). It asserts: the
process's pre-header call stays in `pending:claude-code@100.<start>` carrying `proc`; the
headered request in `s1` carries the same `proc` and `sessionHeader`; the unnamed client's
call is in `pending:claude-code` with no `proc`; `s1`'s `Procs` is `[proc]`.

- [ ] **Step 2: Run it to see it fail**

Run: `cd core && go test ./listener/forwardproxy -run RecordsTheProcessOnEveryRow`
Expected: FAIL — today the bucket is adopted into s1 and no row carries `proc`.

- [ ] **Step 3: Move the store's survivors to `pending.go`, delete the four files, strip the store**

Exactly as in the reference commit. `pickAgent` replaces `agentOfLocked`: the owner is now
the agent of the session's first request that carried a session header (`applyOwner`,
Task 2), which is what `Claim`'s first-claim-wins recorded.

- [ ] **Step 4: Pin attribution on the context and rewrite the forward proxy's resolution**

`resolvePluginSession` becomes `s.pluginSessionOf(s.attribute(h, chain))`. `hydrateSession`
calls `pctx.SetAttribution(at.Proc, at.Header)` and takes `ChainSession` from the answer.
The two paths that record before hydration (`refuseMisdirected`, `recordUnbufferedRequest`)
call `attributeForRecording`, which pins and returns `RecordID()`, falling back to
`ActiveSession()`, then `default`. `recordOutboundRequestEvent` sets
`Proc: pctx.AttributedProc(), SessionHeader: pctx.NamingHeader()`; every other row built from a
`pctx` (response, denial, tunnel open and close) sets `Proc: pctx.AttributedProc()`.
`TouchProcess` becomes `s.touch`; `IsAgentProcess`/`ProcessHints` go to `s.attributor()`.

- [ ] **Step 5: Switch the plugins and agentop off `PendingPrefix` and `Adopted`**

- [ ] **Step 6: Run everything**

Run: `cd core && gofmt -l . && go vet ./... && go test -race ./...` and
`cd cmd/agentop && GOWORK=off go vet ./... && GOWORK=off go test ./...`, and
`GOWORK=off go build ./...` in `cmd/cortex`, `cmd/cortex-envoy`, `cmd/cortex-praxis`.
Expected: all pass.

- [ ] **Step 7: Commit**

```bash
git add -A core cmd/agentop
git commit -s -m "feat: Resolve sessions through core/attribution and stop adopting buckets

The forward proxy asks an Attributor, which keeps its own claims and never
writes to the store. A pending bucket is no longer renamed into the session
its process names; every row records its process instead.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 4: `session.Link` — link buckets to sessions when the list is read

**Files:**
- Create: `core/session/link.go`, `core/session/link_test.go`
- Modify: `core/session/store.go` (`LinkedTo []PendingLink`)
- Modify: `core/sessionapi/server.go` (`handleList` calls `session.Link` after the archive's rows are added)
- Create: `core/sessionapi/link_test.go`

**Interfaces:**
- Produces: `session.PendingLink{Session string; Until time.Time}` (wire `{session, until}`), `session.Link([]SessionSummary) []SessionSummary`, `session.ParsePending(id) (agent, proc string, ok bool)`, `session.LinkedEvents(events, after, before time.Time) []pipeline.SessionEvent`

- [ ] **Step 1: Write the failing tests** — `TestLink` (fresh launch, resume, parallel runs,
  never-named, calls after the last claim, `/clear` between two claims, a claim before the
  bucket existed), `TestLink_AfterARestartTheResumedSessionStillTakesTheCalls`,
  `TestLink_BucketsWithNoProcessNeverLink`, `TestLinkedEvents_AResponseFollowsItsRequest`,
  `TestParsePending`, and `TestHandleList_LinksAPendingBucketToItsSession`. Code: reference
  commit `4f6f93ca`, files `core/session/link_test.go` and `core/sessionapi/link_test.go`.
- [ ] **Step 2: Run them to see them fail** — `cd core && go test ./session ./sessionapi -run 'Link|ParsePending'`; Expected: `undefined: Link`.
- [ ] **Step 3: Write `core/session/link.go`** — reference commit. The rule: a bucket event
  belongs to the first session its process named at or after it, read by each session's
  `ProcSpan.Last`; `LinkedAll` when the newest claim is not before the bucket's
  `LastRequestAt`; consecutive claims on one session merge into one link.
- [ ] **Step 4: Call it from `handleList`**, after `withArchivedRows`.
- [ ] **Step 5: Run** `cd core && go vet ./session ./sessionapi && go test -race ./session ./sessionapi`; Expected: `ok`.
- [ ] **Step 6: Commit** — `feat: Link a process's pending calls to the session it names`.

---

### Task 5: agentop — buckets under their agent; fold linked buckets into their session

**Files:**
- Modify: `cmd/agentop/tui/pending_bucket.go` (`sessionAgent`, the one place a row's agent is decided)
- Modify: `cmd/agentop/tui/agents_other.go`, `sessions_pane.go`, `agents_pane.go` (read `sessionAgent`; skip `LinkedAll` rows)
- Create: `cmd/agentop/tui/linked.go` (`linkedInto`, `fetchLinked`, `mergeLinked`, `eventSession`, `linkedEventsCap = 500`)
- Modify: `cmd/agentop/tui/app.go` (`snapshotCmd` merges linked events when the snapshot is the whole session)
- Modify: `cmd/agentop/tui/keys.go`, `detail_fetch.go` (a linked event's detail is fetched from its own bucket, keyed by `(session, seq)`)
- Test: `cmd/agentop/tui/pending_bucket_test.go` (`TestSessionAgent_APendingBucketBelongsToItsAgent`), `cmd/agentop/tui/linked_test.go` (`TestSessionsPane_HidesAFullyLinkedBucket`, `TestSnapshot_MergesLinkedCallsByTime`, `TestInWindow_KeepsTheLinkedCallsTheSnapshotCovers`)

- [ ] **Step 1: Write the three failing tests** (reference commit `13d064c7`).
- [ ] **Step 2: Run them to see them fail** — `cd cmd/agentop && GOWORK=off go test ./tui -run 'SessionAgent_APending|FullyLinked|MergesLinked'`.
- [ ] **Step 3: Add `sessionAgent` and route every agent read through it** (the five sites listed above). The server still sends `agent` blank on buckets; see deviation 6.
- [ ] **Step 4: Add `linked.go`; merge in `snapshotCmd`; hide `LinkedAll` rows; fetch details by origin.**
- [ ] **Step 5: Run** `cd cmd/agentop && GOWORK=off go vet ./... && GOWORK=off go test ./...`; Expected: `ok`.
- [ ] **Step 6: Commit** — `feat: Show pending calls under their agent and inside the session they belong to`.

---

### Task 6: `attribution.Lookup` — the process lookup a relay can call

**Files:**
- Create: `core/attribution/lookup.go` (`MaxChain`, `(*Attributor).Lookup(peerproc.Resolver, client, server netip.AddrPort) ([]Proc, error)`)
- Modify: `core/listener/forwardproxy/process.go` (`lookupChain` parses the addresses and calls `Lookup`; `maxChain` goes)
- Test: `core/attribution/attribution_test.go` (`fakeResolver`, `TestLookup_NamesTheClientsChain`)

- [ ] **Step 1: Write the failing test** (reference commit `c1ec58f9`).
- [ ] **Step 2: Run it to see it fail** — `cd core && go test ./attribution -run Lookup`; Expected: `a.Lookup undefined`.
- [ ] **Step 3: Write `lookup.go`; make `lookupChain` call it.**
- [ ] **Step 4: Run** `cd core && go test ./attribution ./listener/forwardproxy`; Expected: `ok`.
- [ ] **Step 5: Commit** — `refactor: Move the client-process lookup into core/attribution`.

---

### Task 7: The wire contract

**Files:**
- Create: `core/attribution/wire.go` (`HeaderAgent`, `HeaderProc`, `HeaderSession`, `HeaderSource`, `HeaderNamedBy`, `HeaderChain`; `Strip`, `Encode`, `Decode`)
- Modify: `core/listener/forwardproxy/server.go` (`attribution.Strip(r.Header)` first in `serveOutbound` and `handleConnect`)
- Test: `core/attribution/wire_test.go` (`TestWire_RoundTrips`, `TestWire_DecodeRefusesWhatItCannotTrust`, `TestWire_StripRemovesOnlyAttribution`), `core/listener/forwardproxy/process_test.go` (`TestForwardProxy_StripsCortexAttributionHeaders`)

Header names differ from the spec's four: `X-Cortex-Session-Header` and
`X-Cortex-Chain-Session` are added, because `Attribution` has six fields and a relay must
carry all of them.

- [ ] **Step 1: Write the four failing tests** (reference commit `c1ec58f9`).
- [ ] **Step 2: Run them to see them fail.**
- [ ] **Step 3: Write `wire.go`; strip in the two handlers.** `Decode` is not called anywhere: no trusted hop exists yet.
- [ ] **Step 4: Run** `cd core && go vet ./attribution ./listener/forwardproxy && go test -race ./attribution ./listener/forwardproxy`; Expected: `ok`.
- [ ] **Step 5: Commit** — `feat: Define the attribution wire contract and strip it from clients`.

---

### Task 8: Docs

**Files:** `CLAUDE.md` (the `/v1/sessions` row), `docs/session-api.md`, `cmd/agentop/README.md`, `docs/agents/opencode.md`, `docs/agents/codex.md`.

- [ ] **Step 1:** Replace every description of adoption with the link, list `procs`,
  `lastRequestAt`, `linkedTo`, `linkedAll` (and drop `adopted`), and move pending buckets
  from "listed under Other" to "listed under their agent, hidden when fully linked". Text:
  reference commit `c1ec58f9`.
- [ ] **Step 2:** `grep -rn -i 'adopt' CLAUDE.md docs/*.md docs/agents cmd/agentop/README.md` — only the unrelated "enable adopts as Cortex's" and "a running proxy to adopt" remain.
- [ ] **Step 3: Commit** — `docs: Describe linked pending buckets`.

---

## Validation (done while writing this plan)

Every task was implemented on `design/link-not-adopt` as four `wip:` commits
(`3b7d1b04`, `4f6f93ca`, `13d064c7`, `c1ec58f9`, plus the `inWindow` fix after the plan commit) to check that the plan's code compiles and
its order works. Results, 2026-10-28:

- `cd core && go test -race ./...` — pass. `cd cmd/agentop && GOWORK=off go test ./...` — pass.
  `cmd/cortex`, `cmd/cortex-envoy`, `cmd/cortex-praxis` build and vet.
- **Live, on a scratch proxy** (`:47710`–`47714`, `HOME` under the job's tmp dir, full profile,
  archive on, the user's `:47600` proxy untouched):
  1. `claude -p` → `pending:claude-code@17543…` (12 events, the MCP handshake) `linkedTo: [d03430f4]`, `linkedAll: true`.
  2. `claude -p --resume d03430f4` → a second bucket, also linked; the session's `procs` lists both processes. (Today this is the "target already holds events" refusal.)
  3. Proxy restarted, `--resume` again → the third bucket links; the two earlier ones, now archive-only (`resident: false`), still link.
  4. Scratch agentop: the sessions list shows only `d03430f4`; opening it shows all three launches' MCP handshakes inside the timeline at their own times.

The diff is +2,668 −1,857 across 61 files (including the spec and this plan).

The four `wip:` commits are a validation artefact, not the delivery: executing this plan means
rebuilding the branch as the seven commits above (reset to the spec commit, apply each task's
part of the reference diff, test, commit).
