package session

import (
	"slices"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// linkFixture is a store on a hand-moved clock and the time it started at.
func linkFixture() (*Store, *procTestClock, time.Time) {
	s, clk := newProcStore()
	return s, clk, clk.t
}

// call records a request from proc under id at the clock's time, and its response a moment
// later. named marks the request as having named id through a header.
func call(s *Store, clk *procTestClock, id, proc, reqID string, named bool) {
	clk.advance(time.Second)
	req := pipeline.SessionEvent{At: clk.now(), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Proc: proc, RequestID: reqID}
	if named {
		req.SessionHeader = ClaudeCodeSessionHeader
	}
	s.Append(id, req)
	resp := req
	resp.Phase, resp.SessionHeader, resp.At = pipeline.SessionResponse, "", clk.now().Add(100*time.Millisecond)
	s.Append(id, resp)
}

func linked(t *testing.T, s *Store, id string) SessionSummary {
	t.Helper()
	for _, sum := range Link(s.ListSessions()) {
		if sum.ID == id {
			return sum
		}
	}
	t.Fatalf("no row %s", id)
	return SessionSummary{}
}

func linkIDs(l []PendingLink) []string {
	var out []string
	for _, x := range l {
		out = append(out, x.Session)
	}
	return out
}

func TestLink(t *testing.T) {
	const p = "100.5"
	bucket := PendingID("claude-code", p)
	for _, tc := range []struct {
		name    string
		record  func(s *Store, clk *procTestClock)
		wantTo  []string
		wantAll bool
	}{
		{"a fresh launch: the handshake, then the first prompt", func(s *Store, clk *procTestClock) {
			call(s, clk, bucket, p, "mcp-init", false)
			call(s, clk, "s1", p, "turn-1", true)
		}, []string{"s1"}, true},

		{"a resume into a session that already holds events", func(s *Store, clk *procTestClock) {
			call(s, clk, "s1", "99.1", "yesterday", true) // another process, days ago
			call(s, clk, bucket, p, "mcp-init", false)
			call(s, clk, "s1", p, "turn-1", true)
		}, []string{"s1"}, true},

		{"parallel runs naming one session both link", func(s *Store, clk *procTestClock) {
			call(s, clk, bucket, p, "a-init", false)
			call(s, clk, PendingID("claude-code", "101.5"), "101.5", "b-init", false)
			call(s, clk, "s1", p, "a-turn", true)
			call(s, clk, "s1", "101.5", "b-turn", true)
		}, []string{"s1"}, true},

		{"a process that never names a session links nothing", func(s *Store, clk *procTestClock) {
			call(s, clk, bucket, p, "probe-1", false)
			call(s, clk, "s1", "200.1", "someone-else", true)
		}, nil, false},

		{"calls after the last claim stay in the bucket", func(s *Store, clk *procTestClock) {
			call(s, clk, bucket, p, "mcp-init", false)
			call(s, clk, "s1", p, "turn-1", true)
			call(s, clk, bucket, p, "mcp-reconnect", false)
		}, []string{"s1"}, false},

		{"/clear: calls before s1 go to s1, calls between go to s2", func(s *Store, clk *procTestClock) {
			call(s, clk, bucket, p, "init", false)
			call(s, clk, "s1", p, "turn-1", true)
			call(s, clk, bucket, p, "between", false)
			call(s, clk, "s2", p, "turn-2", true)
		}, []string{"s1", "s2"}, true},

		{"one service naming several sessions: start-up calls go to the first it named after them", func(s *Store, clk *procTestClock) {
			call(s, clk, bucket, p, "startup", false)
			call(s, clk, "A", p, "a-1", true)
			call(s, clk, "B", p, "b-1", true)
			call(s, clk, "B", p, "b-2", true)
			call(s, clk, "A", p, "a-2", true)
		}, []string{"A"}, true},

		{"/clear after the handshake: the later session owns none of the bucket's calls", func(s *Store, clk *procTestClock) {
			call(s, clk, bucket, p, "init", false)
			call(s, clk, "s1", p, "turn-1", true)
			call(s, clk, "s2", p, "turn-2", true)
		}, []string{"s1"}, true},

		{"shortcut: a session named between two bucket calls links though it owns neither", func(s *Store, clk *procTestClock) {
			call(s, clk, bucket, p, "startup", false)
			call(s, clk, "A", p, "a-1", true)
			call(s, clk, "B", p, "b-1", true)
			call(s, clk, bucket, p, "late", false)
			call(s, clk, "A", p, "a-2", true)
		}, []string{"A", "B"}, true},

		{"a claim before the bucket existed does not take it", func(s *Store, clk *procTestClock) {
			call(s, clk, "s1", p, "turn-1", true)
			call(s, clk, bucket, p, "later", false)
		}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, clk, _ := linkFixture()
			defer s.Close()
			tc.record(s, clk)
			got := linked(t, s, bucket)
			if !slices.Equal(linkIDs(got.LinkedTo), tc.wantTo) || got.LinkedAll != tc.wantAll {
				t.Errorf("LinkedTo %v LinkedAll %v, want %v %v", linkIDs(got.LinkedTo), got.LinkedAll, tc.wantTo, tc.wantAll)
			}
		})
	}
}

// A process names a session, the proxy restarts and loses the claim, the process makes calls
// into a new bucket, then names the session again: the session's span for it reaches past the
// new bucket's calls, so they link. The archive's fold is what carries the span across.
func TestLink_AfterARestartTheResumedSessionStillTakesTheCalls(t *testing.T) {
	const p = "100.5"
	t0 := time.Unix(1_700_000_000, 0)
	earlier := NewSummaryFold()
	e := named(p, t0)
	earlier.Add("s1", &e)
	clk := &procTestClock{t: t0.Add(time.Hour)}
	s := New(0, 0, 0, WithClock(clk.now))
	defer s.Close()
	s.AddRecorder(&priorKeeper{last: map[string]uint64{"s1": 1}, prior: map[string]Prior{"s1": {Fold: earlier}}})
	bucket := PendingID("claude-code", p)
	call(s, clk, bucket, p, "after-restart", false)
	call(s, clk, "s1", p, "turn-2", true)
	if got := linked(t, s, bucket); !slices.Equal(linkIDs(got.LinkedTo), []string{"s1"}) || !got.LinkedAll {
		t.Errorf("LinkedTo %v LinkedAll %v, want [s1] true", linkIDs(got.LinkedTo), got.LinkedAll)
	}
}

// The agent-wide bucket and the default bucket carry no process, so nothing links them.
func TestLink_BucketsWithNoProcessNeverLink(t *testing.T) {
	s, clk, _ := linkFixture()
	defer s.Close()
	call(s, clk, PendingSessionID("claude-code"), "", "guess", false)
	call(s, clk, DefaultSessionID, "", "curl", false)
	call(s, clk, "s1", "100.5", "turn", true)
	for _, id := range []string{PendingSessionID("claude-code"), DefaultSessionID} {
		if got := linked(t, s, id); got.LinkedTo != nil {
			t.Errorf("%s LinkedTo %v, want none", id, got.LinkedTo)
		}
	}
}

// LinkedEvents gives each link its window, and a response goes with its request even when it
// arrived after the claim — the MCP stream that closes five minutes later.
func TestLinkedEvents_AResponseFollowsItsRequest(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	ev := func(ph pipeline.SessionPhase, id string, s int) pipeline.SessionEvent {
		return pipeline.SessionEvent{At: at(s), Phase: ph, RequestID: id}
	}
	events := []pipeline.SessionEvent{
		ev(pipeline.SessionRequest, "stream", 1),
		ev(pipeline.SessionRequest, "between", 20),
		ev(pipeline.SessionResponse, "between", 21),
		ev(pipeline.SessionResponse, "stream", 300), // closes long after both claims
		ev(pipeline.SessionRequest, "after", 400),
	}
	links := []PendingLink{{Session: "s1", First: at(10), Last: at(10)}, {Session: "s2", First: at(30), Last: at(30)}}
	first := LinkedEvents(events, links, "s1")
	if got := ids(first); !slices.Equal(got, []string{"stream", "stream"}) {
		t.Errorf("first link = %v, want the stream's request and its late response", got)
	}
	second := LinkedEvents(events, links, "s2")
	if got := ids(second); !slices.Equal(got, []string{"between", "between"}) {
		t.Errorf("second link = %v, want the call between the claims", got)
	}
}

func ids(events []pipeline.SessionEvent) []string {
	var out []string
	for _, e := range events {
		out = append(out, e.RequestID)
	}
	return out
}

func TestParsePending(t *testing.T) {
	for _, tc := range []struct {
		id, agent, proc string
		ok              bool
	}{
		{"pending:claude-code@100.5", "claude-code", "100.5", true},
		{"pending:bob", "bob", "", true},
		{"pending:", "", "", false},
		{DefaultSessionID, "", "", false},
		{"s1", "", "", false},
	} {
		agent, proc, ok := ParsePending(tc.id)
		if agent != tc.agent || proc != tc.proc || ok != tc.ok {
			t.Errorf("ParsePending(%q) = %q %q %v", tc.id, agent, proc, ok)
		}
	}
}

// OwnerAt is the session a call made at t belongs to: the first session its process named at
// or after t. A session the process was still naming at t (its span covers t) wins over one it
// started naming later; between two spans that both cover t, the one begun more recently.
func TestOwnerAt(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	links := []PendingLink{
		{Session: "A", First: at(10), Last: at(1000)},
		{Session: "B", First: at(500), Last: at(600)},
	}
	for _, tc := range []struct {
		t    int
		want string
	}{
		{1, "A"},   // before both: A is named first after it
		{300, "A"}, // inside A's span only
		{550, "B"}, // inside both: B began more recently
		{800, "A"}, // after B's last, inside A's
		{1001, ""}, // after every claim
	} {
		if got := OwnerAt(links, at(tc.t)); got != tc.want {
			t.Errorf("OwnerAt(%d) = %q, want %q", tc.t, got, tc.want)
		}
	}
}

// A Bob worker's calls before the launcher spoke wait in the worker's own bucket; the naming
// request is attributed to the launcher (the agent root) but carries the worker too, so that
// bucket links as well — what adoption did for PendingProcessID(chain[0]).
func TestLink_AWorkersOwnBucketLinksThroughItsLaunchersClaim(t *testing.T) {
	s, clk, _ := linkFixture()
	defer s.Close()
	const launcher, worker = "820.1", "821.1"
	call(s, clk, PendingID("bob", worker), worker, "probe", false)
	clk.advance(time.Second)
	e := named(launcher, clk.now())
	e.ClientProc = worker
	s.Append("task-1", e)
	got := linked(t, s, PendingID("bob", worker))
	if !slices.Equal(linkIDs(got.LinkedTo), []string{"task-1"}) || !got.LinkedAll {
		t.Errorf("worker bucket LinkedTo %v LinkedAll %v, want [task-1] true", linkIDs(got.LinkedTo), got.LinkedAll)
	}
}
