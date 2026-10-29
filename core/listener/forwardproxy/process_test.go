package forwardproxy

import (
	"bufio"
	"context"
	"encoding/pem"
	"fmt"
	"github.com/rossoctl/cortex/core/attribution"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/peerproc"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/plugintesting"
	"github.com/rossoctl/cortex/core/session"
)

// Tests for #1187's process attribution: a request with no session header is filed under
// the session of the process that sent it, looked up once per client connection.

const procClaudeUA = "claude-cli/2.1.286 (external, cli)"

// fakeProcs is a peerproc.Resolver over a made-up process table, so one test process can
// play several. A connection belongs to whichever pid the client that dialled it was made
// for (clientFor, connectAs); a listener to whichever pid listens registers.
type fakeProcs struct {
	mu          sync.Mutex
	procs       map[int32]peerproc.Proc
	byPort      map[uint16]int32
	listeners   map[uint16]int32
	listenersAt map[netip.AddrPort]int32
	lookups     int
}

func fproc(pid, ppid int32, exe string) peerproc.Proc {
	return peerproc.Proc{PID: pid, PPID: ppid, Start: time.Unix(int64(pid), 0), Exe: exe}
}

func newFakeProcs(procs ...peerproc.Proc) *fakeProcs {
	f := &fakeProcs{procs: map[int32]peerproc.Proc{}, byPort: map[uint16]int32{}, listeners: map[uint16]int32{}, listenersAt: map[netip.AddrPort]int32{}}
	for _, p := range procs {
		f.procs[p.PID] = p
	}
	return f
}

func (f *fakeProcs) ConnOwner(client, _ netip.AddrPort, _ ...int32) (peerproc.Proc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	pid, ok := f.byPort[client.Port()]
	if !ok {
		return peerproc.Proc{}, peerproc.ErrNotFound
	}
	return f.procs[pid], nil
}

func (f *fakeProcs) ListenerOwner(addr netip.AddrPort, _ ...int32) (peerproc.Proc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pid, ok := f.listenersAt[addr]
	if !ok {
		pid, ok = f.listeners[addr.Port()]
	}
	if !ok {
		return peerproc.Proc{}, peerproc.ErrNotFound
	}
	return f.procs[pid], nil
}

func (f *fakeProcs) Ancestry(pid int32, max int) ([]peerproc.Proc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []peerproc.Proc
	for pid > 1 && len(out) < max {
		p, ok := f.procs[pid]
		if !ok {
			break
		}
		out = append(out, p)
		pid = p.PPID
	}
	if len(out) == 0 {
		return nil, peerproc.ErrNotFound
	}
	return out, nil
}

// own registers c's local port as pid's.
func (f *fakeProcs) own(c net.Conn, pid int32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byPort[uint16(c.LocalAddr().(*net.TCPAddr).Port)] = pid
}

// listens registers the port of rawURL's host as pid's listener.
func (f *fakeProcs) listens(t *testing.T, rawURL string, pid int32) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listeners[uint16(portOf(u.Host))] = pid
}

// listensAt registers pid as the listener at exactly addr, ahead of any port-wide listener.
func (f *fakeProcs) listensAt(addr netip.AddrPort, pid int32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listenersAt[addr] = pid
}

func (f *fakeProcs) lookupCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookups
}

// clientFor is an HTTP client whose every connection to the proxy belongs to pid.
func (f *fakeProcs) clientFor(proxyURL string, pid int32) *http.Client {
	dialer := &net.Dialer{}
	return &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(mustParseURL(proxyURL)),
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := dialer.DialContext(ctx, network, addr)
			if err == nil {
				f.own(c, pid)
			}
			return c, err
		},
	}}
}

// newProcessProxy is a forward proxy with process attribution over procs, served by an
// http.Server carrying its ConnContext, as cortex wires it. configure, when non-nil, runs
// before it serves; backendHits counts what the backend received.
func newProcessProxy(t *testing.T, store *session.Store, procs *fakeProcs, configure func(*Server)) (proxyURL, backendURL string, backendHits *atomic.Int32) {
	t.Helper()
	backendHits = &atomic.Int32{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)
	p, err := plugintesting.BuildPipeline(nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Sessions:         store,
		Client:           http.DefaultClient,
		SessionIDHeaders: []string{session.ClaudeCodeSessionHeader, session.BobSessionHeader},
		ClientAffinity:   true,
		Processes:        procs,
	}
	if configure != nil {
		configure(srv)
	}
	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.Config.ConnContext = srv.ConnContext
	ts.Start()
	t.Cleanup(ts.Close)
	return ts.URL, backend.URL, backendHits
}

func sendAs(t *testing.T, c *http.Client, rawURL, ua, header, sid string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if header != "" {
		req.Header.Set(header, sid)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", rawURL, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// connectAs is sendConnect from pid: the connection's local port is registered to pid
// before the CONNECT is written.
func connectAs(t *testing.T, procs *fakeProcs, proxyURL, target string, pid int32) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	raw, err := net.Dial("tcp", strings.TrimPrefix(proxyURL, "http://"))
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	procs.own(raw, pid)
	_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintf(raw, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	br := bufio.NewReader(raw)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	return raw, br, resp
}

// recordedPaths is the path of every request event recorded under id, in order.
func recordedPaths(store *session.Store, id string) []string {
	v := store.View(id)
	if v == nil {
		return nil
	}
	var out []string
	for _, e := range v.Events {
		if e.Phase == pipeline.SessionRequest && !e.Tunnel {
			out = append(out, e.HTTPPath)
		}
	}
	return out
}

func eventually(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The case this exists for. Client affinity alone files the tool's call by User-Agent —
// curl is no agent, so ActiveSession() — and files the unrelated script there too.
func TestProcessAttribution_AToolJoinsItsAgentsSessionAndAStrangerDoesNot(t *testing.T) {
	procs := newFakeProcs(
		fproc(100, 50, "/bin/claude"), fproc(200, 100, "/bin/bash"), fproc(300, 200, "/usr/bin/curl"),
		fproc(700, 60, "/usr/bin/python3"),
	)
	store := session.New(0, 0, 0)
	defer store.Close()
	proxyURL, backendURL, _ := newProcessProxy(t, store, procs, nil)

	sendAs(t, procs.clientFor(proxyURL, 100), backendURL+"/v1/messages", procClaudeUA, session.ClaudeCodeSessionHeader, "s1")
	sendAs(t, procs.clientFor(proxyURL, 300), backendURL+"/tool", "curl/8.7.1", "", "")
	sendAs(t, procs.clientFor(proxyURL, 700), backendURL+"/elsewhere", "python-requests/2.32", "", "")

	if got := strings.Join(recordedPaths(store, "s1"), ","); got != "/v1/messages,/tool" {
		t.Errorf("s1 = %s, want the agent's request and its tool's", got)
	}
	if got := strings.Join(recordedPaths(store, session.DefaultSessionID), ","); got != "/elsewhere" {
		t.Errorf("default = %s, want the request from a process of no agent", got)
	}
}

// After /clear, a tool still running in the old session keeps writing there. The agent's
// own header-less calls, and a tool started after the move, go to the new session.
func TestProcessAttribution_AToolInTheOldSessionDoesNotPullTheAgentBack(t *testing.T) {
	procs := newFakeProcs(
		fproc(100, 50, "/bin/claude"), fproc(200, 100, "/bin/bash"),
		fproc(300, 200, "/usr/bin/gh"), fproc(301, 200, "/usr/bin/curl"),
	)
	store := session.New(0, 0, 0)
	defer store.Close()
	proxyURL, backendURL, _ := newProcessProxy(t, store, procs, nil)
	claude, watcher := procs.clientFor(proxyURL, 100), procs.clientFor(proxyURL, 300)

	sendAs(t, claude, backendURL+"/v1/messages", procClaudeUA, session.ClaudeCodeSessionHeader, "s1")
	sendAs(t, watcher, backendURL+"/watch", "gh/2.80", "", "")
	sendAs(t, claude, backendURL+"/v1/messages", procClaudeUA, session.ClaudeCodeSessionHeader, "s2")
	sendAs(t, watcher, backendURL+"/watch", "gh/2.80", "", "")
	sendAs(t, claude, backendURL+"/webfetch", procClaudeUA, "", "")
	sendAs(t, procs.clientFor(proxyURL, 301), backendURL+"/tool", "curl/8.7.1", "", "")

	if got := strings.Join(recordedPaths(store, "s1"), ","); got != "/v1/messages,/watch,/watch" {
		t.Errorf("s1 = %s, want the first request and both of the watcher's", got)
	}
	if got := strings.Join(recordedPaths(store, "s2"), ","); got != "/v1/messages,/webfetch,/tool" {
		t.Errorf("s2 = %s, want the agent's requests after /clear and the new tool's", got)
	}
}

// A service serving two sessions: a's request, then b's, then a's response, whose tool
// runs next. The tool joins a, the session that answered last.
func TestProcessAttribution_AServicesToolJoinsTheSessionThatAnsweredLast(t *testing.T) {
	procs := newFakeProcs(fproc(500, 1, opencodeExe), fproc(600, 500, "/usr/bin/curl"))
	store := session.New(0, 0, 0)
	defer store.Close()
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/a" {
			<-release
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	proxyURL, _, _ := newProcessProxy(t, store, procs, func(s *Server) {
		s.SessionIDHeaders = append(s.SessionIDHeaders, "X-Opencode-Session-Id")
	})
	svc := procs.clientFor(proxyURL, 500)

	done := make(chan struct{})
	go func() {
		defer close(done)
		sendAs(t, svc, backend.URL+"/a", "opencode/latest/2.0.21/cli", "X-Opencode-Session-Id", "ses_a")
	}()
	eventually(t, func() bool { return len(recordedPaths(store, "ses_a")) == 1 }, "a's request")
	sendAs(t, svc, backend.URL+"/b", "opencode/latest/2.0.21/cli", "X-Opencode-Session-Id", "ses_b")
	unblock()
	<-done
	sendAs(t, procs.clientFor(proxyURL, 600), backend.URL+"/tool", "curl/8.7.1", "", "")

	if got := strings.Join(recordedPaths(store, "ses_a"), ","); got != "/a,/tool" {
		t.Errorf("ses_a = %s, want a's request and the tool's", got)
	}
}

// A client the lookup cannot name falls back to client affinity, exactly as without it.
func TestProcessAttribution_FallsBackWhenTheClientCannotBeLookedUp(t *testing.T) {
	store := session.New(0, 0, 0)
	defer store.Close()
	proxyURL, backendURL, _ := newProcessProxy(t, store, newFakeProcs(), nil)
	plain := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxyURL))}}

	sendAs(t, plain, backendURL+"/admin/v1/profile", "bob-shell/2.0.5", "", "")
	if store.View(session.PendingSessionID("bob")) == nil {
		t.Error("an unknown client's Bob call did not reach client affinity's pending:bob")
	}
}

func TestProcessAttribution_LooksEachConnectionUpOnce(t *testing.T) {
	procs := newFakeProcs(fproc(100, 50, "/bin/claude"))
	store := session.New(0, 0, 0)
	defer store.Close()
	proxyURL, backendURL, _ := newProcessProxy(t, store, procs, nil)
	claude := procs.clientFor(proxyURL, 100)
	for i := 0; i < 3; i++ {
		sendAs(t, claude, backendURL+"/v1/messages", procClaudeUA, session.ClaudeCodeSessionHeader, "s1")
	}
	if n := procs.lookupCount(); n != 1 {
		t.Errorf("three requests on one connection took %d lookups, want 1", n)
	}
}

// An opaque tunnel — git to GitHub, which cannot be bridged — has no decrypted request to
// take a session from; its process names it.
//
// Bob speaks last, so ActiveSession() is task-1 and two agents are active: without the
// process lookup git's tunnel would land in task-1, and the stranger's would not land in
// default.
func TestProcessAttribution_AnOpaqueTunnelJoinsItsProcessSession(t *testing.T) {
	// Off, the CONNECT pin is the process answer alone, and only appendTunnelOpen reading
	// it under processesOn keeps git's tunnel out of ActiveSession()'s task-1.
	for _, tc := range []struct {
		name      string
		configure func(*Server)
	}{
		{name: "client affinity on"},
		{name: "client affinity off", configure: func(s *Server) { s.ClientAffinity = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			procs := newFakeProcs(
				fproc(100, 50, "/bin/claude"), fproc(200, 100, "/bin/bash"), fproc(310, 200, "/usr/bin/git"),
				fproc(800, 60, "/usr/bin/node"), fproc(700, 60, "/usr/bin/python3"),
			)
			store := session.New(0, 0, 0)
			defer store.Close()
			proxyURL, backendURL, _ := newProcessProxy(t, store, procs, tc.configure)
			sendAs(t, procs.clientFor(proxyURL, 100), backendURL+"/v1/messages", procClaudeUA, session.ClaudeCodeSessionHeader, "s1")
			sendAs(t, procs.clientFor(proxyURL, 800), backendURL+"/inference", "bob-shell/2.0.5", session.BobSessionHeader, "task-1")

			raw, br, resp := connectAs(t, procs, proxyURL, pingPongOrigin(t), 310)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("CONNECT status %d", resp.StatusCode)
			}
			pingPong(t, raw, br)
			eventually(t, func() bool { _, closes := tunnelRows(store, "s1"); return len(closes) == 1 }, "the git tunnel's close row in s1")
			onePair(t, store, "s1")

			// A process of no agent while agents are active: its tunnel goes to default.
			raw, br, resp = connectAs(t, procs, proxyURL, pingPongOrigin(t), 700)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("CONNECT status %d", resp.StatusCode)
			}
			pingPong(t, raw, br)
			eventually(t, func() bool {
				_, closes := tunnelRows(store, session.DefaultSessionID)
				return len(closes) == 1
			}, "the stranger's tunnel close row in default")
			onePair(t, store, session.DefaultSessionID)
			if v := store.View(session.DefaultSessionID); len(v.Events) != 2 {
				t.Errorf("default holds %d event(s), want only the stranger's tunnel pair", len(v.Events))
			}
			onePair(t, store, "s1")
			if opens, closes := tunnelRows(store, "task-1"); len(opens)+len(closes) != 0 {
				t.Errorf("task-1 holds %d tunnel row(s); neither tunnel is Bob's", len(opens)+len(closes))
			}
		})
	}
}

// ConnContext runs on the accept loop, so it must not look the client up: a connection
// that sends nothing costs no lookup.
func TestProcessAttribution_AConnectionWithNoRequestIsNotLookedUp(t *testing.T) {
	procs := newFakeProcs(fproc(100, 50, "/bin/claude"))
	store := session.New(0, 0, 0)
	defer store.Close()
	proxyURL, _, _ := newProcessProxy(t, store, procs, nil)
	raw, err := net.Dial("tcp", strings.TrimPrefix(proxyURL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	procs.own(raw, 100)
	time.Sleep(50 * time.Millisecond) // let the server accept it and run ConnContext
	_ = raw.Close()
	if n := procs.lookupCount(); n != 0 {
		t.Errorf("%d lookups for a connection that sent no request, want 0", n)
	}
}

// The requests a bridged CONNECT decrypts come from the CONNECT's client; they reuse its
// lookup rather than making their own, which the inner server could not anyway. Bob is
// active too, so a request with no process answer would land in default, not in s1.
func TestProcessAttribution_ABridgedTunnelsRequestsUseItsProcess(t *testing.T) {
	procs := newFakeProcs(
		fproc(100, 50, "/bin/claude"), fproc(200, 100, "/bin/bash"), fproc(300, 200, "/usr/bin/curl"),
		fproc(800, 60, "/usr/bin/node"),
	)
	store := session.New(0, 0, 0)
	defer store.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	t.Cleanup(origin.Close)
	originCA := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: origin.Certificate().Raw})
	u, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	engine := bridgeEngine(t, portOf(u.Host), originCA)
	proxyURL, backendURL, _ := newProcessProxy(t, store, procs, func(s *Server) { s.TLSBridge = engine })
	sendAs(t, procs.clientFor(proxyURL, 100), backendURL+"/v1/messages", procClaudeUA, session.ClaudeCodeSessionHeader, "s1")
	sendAs(t, procs.clientFor(proxyURL, 800), backendURL+"/inference", "bob-shell/2.0.5", session.BobSessionHeader, "task-1")

	before := procs.lookupCount()
	raw, br, _ := connectAs(t, procs, proxyURL, u.Host, 300)
	tc := bridgedTLS(t, raw, br, u.Host, engine.CAPEM)
	for _, path := range []string{"/a", "/b"} {
		req, _ := http.NewRequest(http.MethodGet, "https://"+u.Host+path, nil)
		req.Header.Set("User-Agent", "curl/8.7.1")
		bridgedRoundTrip(t, tc, req)
	}
	_ = tc.Close()

	if n := procs.lookupCount() - before; n != 1 {
		t.Errorf("the CONNECT and its two requests took %d lookups, want 1", n)
	}
	eventually(t, func() bool { return len(recordedPaths(store, "s1")) == 3 }, "both decrypted requests in s1")
	if got := strings.Join(recordedPaths(store, "s1"), ","); got != "/v1/messages,/a,/b" {
		t.Errorf("s1 = %s, want the agent's request and the curl tunnel's two", got)
	}
}

const opencodeExe = "/Users/x/.opencode/bin/opencode"

// countPath is how many request events with path were recorded, in any session.
func countPath(store *session.Store, path string) int {
	n := 0
	for _, sum := range store.ListSessions() {
		for _, p := range recordedPaths(store, sum.ID) {
			if p == path {
				n++
			}
		}
	}
	return n
}

// OpenCode's TUI polls its own background service through the proxy. Once the service has
// named a session — so it is an agent's own process — and runs the TUI's executable, that
// polling is forwarded without a row.
func TestSelfTraffic_AnAgentPollingItsOwnServiceIsForwardedButNotRecorded(t *testing.T) {
	procs := newFakeProcs(fproc(500, 1, opencodeExe), fproc(510, 60, opencodeExe))
	store := session.New(0, 0, 0)
	defer store.Close()
	proxyURL, backendURL, hits := newProcessProxy(t, store, procs, func(s *Server) {
		s.SessionIDHeaders = append(s.SessionIDHeaders, "X-Opencode-Session-Id")
	})
	procs.listens(t, backendURL, 500) // the service listens where the TUI polls
	tui, svc := procs.clientFor(proxyURL, 510), procs.clientFor(proxyURL, 500)

	sendAs(t, tui, backendURL+"/api/info", "opencode/latest/2.0.21/cli", "", "")
	sendAs(t, svc, backendURL+"/zen/v1/chat/completions", "opencode/latest/2.0.21/cli", "X-Opencode-Session-Id", "ses_1")
	sendAs(t, tui, backendURL+"/api/info", "opencode/latest/2.0.21/cli", "", "")

	if got := hits.Load(); got != 3 {
		t.Errorf("the backend received %d requests, want all 3 forwarded", got)
	}
	if n := countPath(store, "/api/info"); n != 1 {
		t.Errorf("%d /api/info rows recorded; want only the one before the service named a session", n)
	}
	if got := strings.Join(recordedPaths(store, "ses_1"), ","); got != "/zen/v1/chat/completions" {
		t.Errorf("ses_1 = %s, want the service's inference request", got)
	}
}

// OpenCode's service is the TUI's own child. That needs no session history, so the polling
// is not recorded from the first request — after a proxy restart an idle window's service
// has named nothing, and every poll used to land in the TUI's pending bucket.
func TestSelfTraffic_AnAgentsOwnChildServiceIsNotRecordedBeforeItNamesASession(t *testing.T) {
	procs := newFakeProcs(fproc(510, 60, opencodeExe), fproc(520, 510, opencodeExe))
	store := session.New(0, 0, 0)
	defer store.Close()
	proxyURL, backendURL, hits := newProcessProxy(t, store, procs, func(s *Server) {
		s.SessionIDHeaders = append(s.SessionIDHeaders, "X-Opencode-Session-Id")
	})
	procs.listens(t, backendURL, 520)
	tui := procs.clientFor(proxyURL, 510)

	sendAs(t, tui, backendURL+"/api/info", "opencode/latest/2.0.21/cli", "", "")
	sendAs(t, tui, backendURL+"/api/session", "opencode/latest/2.0.21/cli", "", "")

	if got := hits.Load(); got != 2 {
		t.Errorf("the backend received %d requests, want both forwarded", got)
	}
	if n := countPath(store, "/api/info") + countPath(store, "/api/session"); n != 0 {
		t.Errorf("%d rows recorded for the TUI polling its own child service, want 0", n)
	}
}

// A child running another program is a server the agent started — an MCP server, a dev
// server — and its traffic is the agent's work, so it stays recorded.
func TestSelfTraffic_AnAgentsChildRunningAnotherExecutableIsRecorded(t *testing.T) {
	procs := newFakeProcs(fproc(510, 60, opencodeExe), fproc(520, 510, "/usr/bin/node"))
	store := session.New(0, 0, 0)
	defer store.Close()
	proxyURL, backendURL, _ := newProcessProxy(t, store, procs, func(s *Server) {
		s.SessionIDHeaders = append(s.SessionIDHeaders, "X-Opencode-Session-Id")
	})
	procs.listens(t, backendURL, 520)
	sendAs(t, procs.clientFor(proxyURL, 510), backendURL+"/mcp", "opencode/latest/2.0.21/cli", "", "")
	if n := countPath(store, "/mcp"); n != 1 {
		t.Errorf("the agent's call to a node child: %d rows, want 1", n)
	}
}

// The child rule is for a coding agent and its own service. A test suite the agent runs
// starting a server of its own interpreter and calling it is the agent's work: python and
// python, parent and child, still recorded.
func TestSelfTraffic_AProgramOfNoAgentCallingItsOwnChildIsRecorded(t *testing.T) {
	procs := newFakeProcs(fproc(510, 60, "/usr/bin/python3"), fproc(520, 510, "/usr/bin/python3"))
	store := session.New(0, 0, 0)
	defer store.Close()
	proxyURL, backendURL, _ := newProcessProxy(t, store, procs, nil)
	procs.listens(t, backendURL, 520)
	sendAs(t, procs.clientFor(proxyURL, 510), backendURL+"/health", "python-requests/2.32.3", "", "")
	if n := countPath(store, "/health"); n != 1 {
		t.Errorf("a test suite calling the server it started: %d rows, want 1", n)
	}
}

// A child cannot start before its parent. A listener whose parent pid is the client's but
// which predates it was another process's child, under a pid since reused.
func TestSelfTraffic_AListenerOlderThanTheClientIsNotItsChild(t *testing.T) {
	procs := newFakeProcs(fproc(510, 60, opencodeExe),
		peerproc.Proc{PID: 520, PPID: 510, Start: time.Unix(400, 0), Exe: opencodeExe})
	store := session.New(0, 0, 0)
	defer store.Close()
	proxyURL, backendURL, _ := newProcessProxy(t, store, procs, func(s *Server) {
		s.SessionIDHeaders = append(s.SessionIDHeaders, "X-Opencode-Session-Id")
	})
	procs.listens(t, backendURL, 520)
	sendAs(t, procs.clientFor(proxyURL, 510), backendURL+"/api/info", "opencode/latest/2.0.21/cli", "", "")
	if n := countPath(store, "/api/info"); n != 1 {
		t.Errorf("a listener older than the client: %d rows, want 1", n)
	}
}

// Bob runs under node, and so does the local MCP server it calls. Same executable — but the
// server never names a session, so it is no agent's and its traffic stays visible.
func TestSelfTraffic_ANodeAgentsCallsToALocalNodeServerAreRecorded(t *testing.T) {
	procs := newFakeProcs(fproc(900, 60, "/usr/bin/node"), fproc(910, 60, "/usr/bin/node"))
	store := session.New(0, 0, 0)
	defer store.Close()
	proxyURL, backendURL, _ := newProcessProxy(t, store, procs, nil)
	procs.listens(t, backendURL, 910)
	bob := procs.clientFor(proxyURL, 900)

	sendAs(t, bob, backendURL+"/inference", "bob-shell/2.0.5", session.BobSessionHeader, "task-1")
	sendAs(t, bob, backendURL+"/mcp", "bob-shell/2.0.5", "", "")
	if got := strings.Join(recordedPaths(store, "task-1"), ","); got != "/inference,/mcp" {
		t.Errorf("task-1 = %s, want the MCP call recorded beside the inference", got)
	}
}

// Another program calling an agent's service — curl, agentop — is not the agent talking to
// itself, and is recorded.
func TestSelfTraffic_AnotherProgramCallingTheServiceIsRecorded(t *testing.T) {
	procs := newFakeProcs(fproc(500, 1, opencodeExe), fproc(520, 60, "/usr/bin/curl"))
	store := session.New(0, 0, 0)
	defer store.Close()
	proxyURL, backendURL, _ := newProcessProxy(t, store, procs, func(s *Server) {
		s.SessionIDHeaders = append(s.SessionIDHeaders, "X-Opencode-Session-Id")
	})
	procs.listens(t, backendURL, 500)
	sendAs(t, procs.clientFor(proxyURL, 500), backendURL+"/zen/v1/chat/completions", "", "X-Opencode-Session-Id", "ses_1")
	sendAs(t, procs.clientFor(proxyURL, 520), backendURL+"/api/info", "curl/8.7.1", "", "")
	if n := countPath(store, "/api/info"); n != 1 {
		t.Errorf("curl's call to the service: %d rows, want 1", n)
	}
}

// loopbackDests is the scope of the self-traffic rule: only a request to this host can be
// an agent talking to its own service. A remote host on the port an agent's service holds
// is not — and a real lookup would answer it, since a wildcard listener matches every
// address on its port.
func TestLoopbackDests(t *testing.T) {
	v4, v6 := netip.MustParseAddr("127.0.0.1"), netip.IPv6Loopback()
	for _, tc := range []struct {
		url  string
		want []netip.AddrPort
	}{
		{"http://127.0.0.1:8080/x", []netip.AddrPort{netip.AddrPortFrom(v4, 8080)}},
		{"http://127.0.0.5:8080/x", []netip.AddrPort{netip.AddrPortFrom(netip.MustParseAddr("127.0.0.5"), 8080)}},
		{"http://[::1]:8080/x", []netip.AddrPort{netip.AddrPortFrom(v6, 8080)}},
		{"http://[::ffff:127.0.0.1]:8080/x", []netip.AddrPort{netip.AddrPortFrom(v4, 8080)}},
		{"http://LOCALHOST:8080/x", []netip.AddrPort{netip.AddrPortFrom(v4, 8080), netip.AddrPortFrom(v6, 8080)}},
		{"http://127.0.0.1/x", []netip.AddrPort{netip.AddrPortFrom(v4, 80)}},
		{"https://127.0.0.1/x", []netip.AddrPort{netip.AddrPortFrom(v4, 443)}},
		{"http://10.0.0.1:8080/x", nil},
		{"http://example.com:8080/x", nil},
		{"http://127.0.0.1:99999/x", nil},
		{"/x", nil}, // origin-form: no host to judge
	} {
		u, err := url.Parse(tc.url)
		if err != nil {
			t.Fatal(err)
		}
		if got := loopbackDests(&http.Request{URL: u}); !slices.Equal(got, tc.want) {
			t.Errorf("loopbackDests(%s) = %v, want %v", tc.url, got, tc.want)
		}
	}
}

// A process whose executable could not be read is no evidence of anything: two of them do
// not run "the same" program, so the TUI's polling stays recorded.
func TestSelfTraffic_AnUnknownExecutableIsNeverSelfTraffic(t *testing.T) {
	procs := newFakeProcs(fproc(500, 1, ""), fproc(510, 60, ""))
	store := session.New(0, 0, 0)
	defer store.Close()
	proxyURL, backendURL, _ := newProcessProxy(t, store, procs, func(s *Server) {
		s.SessionIDHeaders = append(s.SessionIDHeaders, "X-Opencode-Session-Id")
	})
	procs.listens(t, backendURL, 500)
	tui, svc := procs.clientFor(proxyURL, 510), procs.clientFor(proxyURL, 500)

	sendAs(t, svc, backendURL+"/zen/v1/chat/completions", "opencode/latest/2.0.21/cli", "X-Opencode-Session-Id", "ses_1")
	sendAs(t, tui, backendURL+"/api/info", "opencode/latest/2.0.21/cli", "", "")
	if n := countPath(store, "/api/info"); n != 1 {
		t.Errorf("%d /api/info rows recorded, want 1: an unknown executable matched another", n)
	}
}

// A request a bridged tunnel decrypted is what its tunnel's open row waits for
// (tunnelLog.recordWith); skipping it would strand that open. So a bridged request is
// never self-traffic, even to an agent's own service.
func TestSelfTraffic_ABridgedRequestIsRecorded(t *testing.T) {
	procs := newFakeProcs(fproc(500, 1, opencodeExe), fproc(510, 60, opencodeExe))
	store := session.New(0, 0, 0)
	defer store.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	t.Cleanup(origin.Close)
	originCA := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: origin.Certificate().Raw})
	u, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	engine := bridgeEngine(t, portOf(u.Host), originCA)
	proxyURL, backendURL, _ := newProcessProxy(t, store, procs, func(s *Server) {
		s.TLSBridge = engine
		s.SessionIDHeaders = append(s.SessionIDHeaders, "X-Opencode-Session-Id")
	})
	procs.listens(t, origin.URL, 500) // the service holds the port the tunnel goes to
	sendAs(t, procs.clientFor(proxyURL, 500), backendURL+"/zen/v1/chat/completions", "", "X-Opencode-Session-Id", "ses_1")

	raw, br, _ := connectAs(t, procs, proxyURL, u.Host, 510)
	tc := bridgedTLS(t, raw, br, u.Host, engine.CAPEM)
	req, _ := http.NewRequest(http.MethodGet, "https://"+u.Host+"/api/info", nil)
	bridgedRoundTrip(t, tc, req)
	_ = tc.Close()

	eventually(t, func() bool { return countPath(store, "/api/info") == 1 }, "the bridged request's row")
}

// Self-traffic is relayed as it arrives. OpenCode's TUI follows its service's event stream
// through the proxy; buffered, the first event would wait for 4KB more to follow it.
func TestSelfTraffic_AnEventStreamIsRelayedAsItArrives(t *testing.T) {
	procs := newFakeProcs(fproc(500, 1, opencodeExe), fproc(510, 60, opencodeExe))
	store := session.New(0, 0, 0)
	defer store.Close()
	release := make(chan struct{})
	events := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"server.connected\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(events.Close)
	t.Cleanup(func() { close(release) })
	proxyURL, backendURL, _ := newProcessProxy(t, store, procs, func(s *Server) {
		s.SessionIDHeaders = append(s.SessionIDHeaders, "X-Opencode-Session-Id")
	})
	procs.listens(t, events.URL, 500)
	sendAs(t, procs.clientFor(proxyURL, 500), backendURL+"/zen/v1/chat/completions", "", "X-Opencode-Session-Id", "ses_1")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, events.URL+"/api/event", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := procs.clientFor(proxyURL, 510).Do(req)
	if err != nil {
		t.Fatalf("the event stream's headers never arrived: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || !strings.Contains(line, "server.connected") {
		t.Fatalf("first event = %q, %v; want it relayed while the stream is still open", line, err)
	}
	if n := countPath(store, "/api/event"); n != 0 {
		t.Errorf("%d /api/event rows; an agent's own event stream must not be recorded", n)
	}
}

// A process's calls before its first header stay in its own pending bucket, carrying the
// process, and its headered request carries the same process and the header that named the
// session: the two facts session.Link joins on. A call the lookup could not name goes to the
// agent-wide bucket and carries no process, so nothing can link it.
func TestProcessAttribution_RecordsTheProcessOnEveryRow(t *testing.T) {
	procs := newFakeProcs(fproc(100, 50, "/bin/claude"))
	store := session.New(0, 0, 0)
	defer store.Close()
	proxyURL, backendURL, _ := newProcessProxy(t, store, procs, nil)
	unnamed := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxyURL))}}
	claude := procs.clientFor(proxyURL, 100)

	sendAs(t, unnamed, backendURL+"/guess", procClaudeUA, "", "")
	sendAs(t, claude, backendURL+"/own", procClaudeUA, "", "")
	sendAs(t, claude, backendURL+"/v1/messages", procClaudeUA, session.ClaudeCodeSessionHeader, "s1")

	proc := attribution.Proc{PID: 100, Start: time.Unix(100, 0).UnixNano()}.Key()
	own := store.View(session.PendingID("claude-code", proc))
	if own == nil || len(own.Events) != 2 || own.Events[0].HTTPPath != "/own" {
		t.Fatalf("the process's bucket = %+v, want /own and its response", own)
	}
	for _, e := range own.Events {
		if e.Proc != proc || e.SessionHeader != "" {
			t.Errorf("%s %s: proc %q header %q, want %q and none", e.Phase, e.HTTPPath, e.Proc, e.SessionHeader, proc)
		}
	}
	s1 := store.View("s1")
	if s1 == nil || s1.Events[0].Proc != proc || s1.Events[0].SessionHeader != session.ClaudeCodeSessionHeader {
		t.Fatalf("s1's request = %+v, want proc %q named by the Claude header", s1, proc)
	}
	var guess *pipeline.SessionView
	if guess = store.View(session.PendingSessionID("claude-code")); guess == nil || guess.Events[0].Proc != "" {
		t.Errorf("the unnamed client's bucket = %+v, want /guess with no process", guess)
	}
	for _, sum := range store.ListSessions() {
		if sum.ID == "s1" && (len(sum.Procs) != 1 || sum.Procs[0] != proc) {
			t.Errorf("s1's Procs = %v, want [%s]", sum.Procs, proc)
		}
	}
}

// "localhost" names both loopbacks, and the proxy has not dialled yet, so every listener
// the request could reach must qualify. An agent's service on 127.0.0.1 does not make a
// request self-traffic while another program holds the same port on ::1.
func TestSelfTraffic_LocalhostNeedsEveryLoopbackListenerToQualify(t *testing.T) {
	procs := newFakeProcs(fproc(500, 1, opencodeExe), fproc(510, 60, opencodeExe), fproc(600, 1, "/usr/bin/node"))
	store := session.New(0, 0, 0)
	defer store.Close()
	proxyURL, backendURL, _ := newProcessProxy(t, store, procs, func(s *Server) {
		s.SessionIDHeaders = append(s.SessionIDHeaders, "X-Opencode-Session-Id")
	})
	u, err := url.Parse(backendURL)
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(portOf(u.Host))
	procs.listensAt(netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port), 500)
	procs.listensAt(netip.AddrPortFrom(netip.IPv6Loopback(), port), 600)
	sendAs(t, procs.clientFor(proxyURL, 500), backendURL+"/zen/v1/chat/completions", "", "X-Opencode-Session-Id", "ses_1")

	sendAs(t, procs.clientFor(proxyURL, 510), fmt.Sprintf("http://localhost:%d/api/info", port), "opencode/latest/2.0.21/cli", "", "")
	if n := countPath(store, "/api/info"); n != 1 {
		t.Errorf("%d /api/info rows, want 1: ::1 is another program's, which the request may reach", n)
	}
}

// OpenCode's service names its sessions only inside bridged TLS, since its inference goes to
// an HTTPS endpoint. That claim belongs to the CONNECT's client, so it is what makes the
// service an agent's own process, and its TUI's polling self-traffic.
func TestSelfTraffic_AServiceThatNamesItsSessionInsideATunnelIsAnAgents(t *testing.T) {
	procs := newFakeProcs(fproc(500, 1, opencodeExe), fproc(510, 60, opencodeExe))
	store := session.New(0, 0, 0)
	defer store.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	t.Cleanup(origin.Close)
	originCA := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: origin.Certificate().Raw})
	u, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	engine := bridgeEngine(t, portOf(u.Host), originCA)
	proxyURL, backendURL, _ := newProcessProxy(t, store, procs, func(s *Server) {
		s.TLSBridge = engine
		s.SessionIDHeaders = append(s.SessionIDHeaders, "X-Opencode-Session-Id")
	})
	procs.listens(t, backendURL, 500) // the service's own API, where its TUI polls

	raw, br, _ := connectAs(t, procs, proxyURL, u.Host, 500)
	tc := bridgedTLS(t, raw, br, u.Host, engine.CAPEM)
	req, _ := http.NewRequest(http.MethodPost, "https://"+u.Host+"/zen/v1/chat/completions", nil)
	req.Header.Set("X-Opencode-Session-Id", "ses_1")
	bridgedRoundTrip(t, tc, req)
	_ = tc.Close()
	eventually(t, func() bool { return len(recordedPaths(store, "ses_1")) == 1 }, "the service's inference in ses_1")

	sendAs(t, procs.clientFor(proxyURL, 510), backendURL+"/api/info", "opencode/latest/2.0.21/cli", "", "")
	if n := countPath(store, "/api/info"); n != 0 {
		t.Errorf("%d /api/info rows: a claim made inside a tunnel did not make the service an agent's", n)
	}
}

// A client that sends attribution headers chooses nothing: they never reach the upstream, and
// the row lands where the proxy's own resolution puts it.
func TestForwardProxy_StripsCortexAttributionHeaders(t *testing.T) {
	var saw http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { saw = r.Header.Clone() }))
	defer upstream.Close()
	store := session.New(0, 0, 0)
	defer store.Close()
	p, _ := pipeline.New(nil)
	srv := &Server{OutboundPipeline: pipeline.NewHolder(p), Sessions: store, Client: http.DefaultClient,
		SessionIDHeaders: []string{session.ClaudeCodeSessionHeader}, ClientAffinity: true}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()
	via := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL))}}

	req, _ := http.NewRequest(http.MethodGet, upstream.URL, nil)
	req.Header.Set("User-Agent", procClaudeUA)
	req.Header.Set(session.ClaudeCodeSessionHeader, "real")
	req.Header.Set(attribution.HeaderSession, "forged")
	req.Header.Set(attribution.HeaderProc, "1.1")
	req.Header.Set(attribution.HeaderSource, string(pipeline.SessionSourceHeader))
	resp, err := via.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	for _, k := range []string{attribution.HeaderSession, attribution.HeaderProc, attribution.HeaderSource} {
		if v := saw.Get(k); v != "" {
			t.Errorf("upstream saw %s: %q", k, v)
		}
	}
	if store.View("forged") != nil || store.View("real") == nil {
		t.Errorf("the row went to the forged session; want it under real")
	}
	if e := store.View("real").Events[0]; e.Proc != "" {
		t.Errorf("the row carries proc %q from the forged header; no lookup ran", e.Proc)
	}
}
