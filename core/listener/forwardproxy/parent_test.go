package forwardproxy

import (
	"bufio"
	"crypto/tls"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// Tests for #1991: a headless agent run that another session starts carries that session
// and its project on each row.

// parentProjects is a counting ProjectOf: 100 is the parent, 400 the child.
type parentProjects struct {
	mu    sync.Mutex
	calls map[int32]int
}

func (f *parentProjects) of(pid int32) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[int32]int{}
	}
	f.calls[pid]++
	switch pid {
	case 100:
		return "~/parent"
	case 400:
		return "~/child"
	}
	return "~/other"
}

func (f *parentProjects) count(pid int32) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[pid]
}

// parentTree is a parent claude (100), two shells, a child claude (400) and a tool on the
// parent (500).
func parentTree() *fakeProcs {
	return newFakeProcs(
		fproc(100, 50, "/bin/claude"),
		fproc(200, 100, "/bin/bash"),
		fproc(300, 200, "/bin/bash"),
		fproc(400, 300, "/bin/claude"),
		fproc(500, 100, "/usr/bin/git"),
	)
}

// parentScene starts the proxy over parentTree and sends the parent's first request on s1.
func parentScene(t *testing.T, configure func(*Server)) (store *session.Store, procs *fakeProcs, fp *parentProjects, proxyURL, backendURL string) {
	t.Helper()
	procs = parentTree()
	store = session.New(0, 0, 0)
	t.Cleanup(store.Close)
	fp = &parentProjects{}
	proxyURL, backendURL, _ = newProcessProxy(t, store, procs, func(s *Server) {
		s.ProjectOf = fp.of
		if configure != nil {
			configure(s)
		}
	})
	sendAs(t, procs.clientFor(proxyURL, 100), backendURL+"/v1/messages", procClaudeUA, session.ClaudeCodeSessionHeader, "s1")
	return store, procs, fp, proxyURL, backendURL
}

// rowsOf is every event recorded under id, failing when there is none.
func rowsOf(t *testing.T, store *session.Store, id string) []pipeline.SessionEvent {
	t.Helper()
	v := store.View(id)
	if v == nil || len(v.Events) == 0 {
		t.Fatalf("no rows under %q", id)
	}
	return v.Events
}

func wantParent(t *testing.T, rows []pipeline.SessionEvent, session, project string) {
	t.Helper()
	for _, e := range rows {
		if e.ParentSession != session || e.ParentProject != project {
			t.Errorf("%s row (tunnel %v): parent = %q, %q; want %q, %q", e.Phase, e.Tunnel, e.ParentSession, e.ParentProject, session, project)
		}
	}
}

func TestParent_ChildRowsCarryTheParent(t *testing.T) {
	store, procs, _, proxyURL, backendURL := parentScene(t, nil)
	sendAs(t, procs.clientFor(proxyURL, 400), backendURL+"/v1/messages", procClaudeUA, session.ClaudeCodeSessionHeader, "c1")
	rows := rowsOf(t, store, "c1")
	phases := map[pipeline.SessionPhase]bool{}
	for _, e := range rows {
		phases[e.Phase] = true
		if e.Project != "~/child" {
			t.Errorf("%s row Project = %q, want ~/child", e.Phase, e.Project)
		}
	}
	if !phases[pipeline.SessionRequest] || !phases[pipeline.SessionResponse] {
		t.Errorf("phases = %v, want a request and a response row", phases)
	}
	wantParent(t, rows, "s1", "~/parent")
	wantParent(t, rowsOf(t, store, "s1"), "", "")
}

// Review Focus 2: the child's rows before it names a session go in its pending bucket and
// carry the parent.
func TestParent_PendingRowsCarryTheParent(t *testing.T) {
	store, procs, _, proxyURL, backendURL := parentScene(t, nil)
	sendAs(t, procs.clientFor(proxyURL, 400), backendURL+"/v1/messages", procClaudeUA, "", "")
	var pending []pipeline.SessionEvent
	for _, s := range store.ListSessions() {
		if strings.HasPrefix(s.ID, session.PendingPrefix) {
			pending = append(pending, rowsOf(t, store, s.ID)...)
		}
	}
	if len(pending) == 0 {
		t.Fatal("no pending rows")
	}
	wantParent(t, pending, "s1", "~/parent")
}

func TestParent_TunnelRowsCarryTheParent(t *testing.T) {
	store, procs, _, proxyURL, backendURL := parentScene(t, nil)
	sendAs(t, procs.clientFor(proxyURL, 400), backendURL+"/v1/messages", procClaudeUA, session.ClaudeCodeSessionHeader, "c1")
	raw, _, resp := connectAs(t, procs, proxyURL, strings.TrimPrefix(backendURL, "http://"), 400)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT answered %d", resp.StatusCode)
	}
	_ = raw.Close()
	var tunnel []pipeline.SessionEvent
	eventually(t, func() bool {
		tunnel = nil
		for _, e := range allEvents(t, store) {
			if e.Tunnel {
				tunnel = append(tunnel, e)
			}
		}
		return len(tunnel) > 0
	}, "the tunnel's rows")
	wantParent(t, tunnel, "s1", "~/parent")
}

// Review Focus 1: a tool on the parent session is the parent's own work, not a child run.
func TestParent_AToolOnTheParentSessionCarriesNone(t *testing.T) {
	store, procs, _, proxyURL, backendURL := parentScene(t, nil)
	sendAs(t, procs.clientFor(proxyURL, 500), backendURL+"/tool", "git/2.0", "", "")
	rows := rowsOf(t, store, "s1")
	if len(rows) < 4 {
		t.Fatalf("s1 has %d rows, want the tool's rows filed there too", len(rows))
	}
	wantParent(t, rows, "", "")
}

func TestParent_TrustedPathCarriesNone(t *testing.T) {
	trusted := false
	store, procs, fp, proxyURL, backendURL := parentScene(t, func(s *Server) {
		s.TrustedSessionID = func(*http.Request) string {
			if trusted {
				return "vm-1"
			}
			return ""
		}
	})
	trusted = true
	sendAs(t, procs.clientFor(proxyURL, 400), backendURL+"/v1/messages", procClaudeUA, session.ClaudeCodeSessionHeader, "c1")
	wantParent(t, rowsOf(t, store, "vm-1"), "", "")
	if n := fp.count(400); n != 0 {
		t.Errorf("ProjectOf(400) calls = %d, want none", n)
	}
}

// Review Focus 3: a keep-alive connection asks for the parent's project once.
func TestParent_ProjectOncePerConnection(t *testing.T) {
	store, procs, fp, proxyURL, backendURL := parentScene(t, nil)
	if n := fp.count(100); n != 1 {
		t.Fatalf("ProjectOf(100) calls after the parent's request = %d, want 1", n)
	}
	child := procs.clientFor(proxyURL, 400)
	for i := 0; i < 2; i++ {
		sendAs(t, child, backendURL+"/v1/messages", procClaudeUA, session.ClaudeCodeSessionHeader, "c1")
	}
	wantParent(t, rowsOf(t, store, "c1"), "s1", "~/parent")
	// The child's connection asks for the parent's project once.
	if n := fp.count(100); n != 2 {
		t.Errorf("ProjectOf(100) calls = %d, want 2: one more for the child's connection", n)
	}
}

// Decision 2: a refusal never reaches hydration, and its row carries the parent too.
func TestParent_RefusalRowCarriesTheParent(t *testing.T) {
	procs := newFakeProcs(
		freshProc(100, 50, exeClaude),
		freshProc(200, 100, "/bin/bash"),
		freshProc(400, 200, exeClaude),
	)
	store := session.New(0, 0, 0)
	defer store.Close()
	fp := &parentProjects{}
	sc := newBridgeScene(t, store, procs, func(s *Server) { s.ProjectOf = fp.of })
	sendAs(t, procs.clientFor(sc.proxyURL, 100), sc.backendURL+"/v1/messages", procClaudeUA, session.ClaudeCodeSessionHeader, "s1")
	sendAs(t, procs.clientFor(sc.proxyURL, 400), sc.backendURL+"/v1/messages", procClaudeUA, session.ClaudeCodeSessionHeader, "c1")
	raw, _, cresp := connectAs(t, procs, sc.proxyURL, sc.target, 400)
	if cresp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT answered %d", cresp.StatusCode)
	}
	tc := tls.Client(raw, &tls.Config{ServerName: "127.0.0.1", RootCAs: sc.trusting})
	defer func() { _ = tc.Close() }()
	req, _ := http.NewRequest(http.MethodGet, "https://"+sc.target+"/v1/messages", nil)
	req.Host = "support.example"
	if err := req.Write(tc); err != nil {
		t.Fatalf("write bridged request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tc), req)
	if err != nil {
		t.Fatalf("read bridged response: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("status %d, want 421", resp.StatusCode)
	}
	var refused []pipeline.SessionEvent
	eventually(t, func() bool {
		refused = nil
		for _, e := range allEvents(t, store) {
			// The refusal row names the tunnel's host, not the Host it refused.
			if e.Phase == pipeline.SessionRequest && !e.Tunnel && e.Host == sc.target {
				refused = append(refused, e)
			}
		}
		return len(refused) > 0
	}, "the refusal's request row")
	wantParent(t, refused, "s1", "~/parent")
}

// A WebSocket turn carries the parent that the handshake pinned.
func TestParent_WebSocketTurnCarriesTheHandshakeParent(t *testing.T) {
	hs := &pipeline.Context{Direction: pipeline.Outbound, Host: "chatgpt.com:443", Path: "/backend-api/codex/responses"}
	hs.SetParent("s1", "~/parent")
	r, err := http.NewRequest(http.MethodGet, "https://chatgpt.com/backend-api/codex/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	ws := &wsSocket{s: &Server{}, hs: hs, r: r}
	ev := ws.turnRequestEvent(ws.newTurnContext([]byte(`{"type":"response.create"}`)))
	wantParent(t, []pipeline.SessionEvent{ev}, "s1", "~/parent")
}
