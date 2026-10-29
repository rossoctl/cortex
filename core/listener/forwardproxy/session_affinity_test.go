package forwardproxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// TestClientAffinity_InClusterResolutionIsUnchanged is a characterization test: it pins
// the in-cluster answers with the knob OFF and asserts the SAME answers with it ON.
//
// In-cluster, an A2A agent's outbound calls carry no coding-agent header and no
// recognised User-Agent, and ActiveSession() is what correlates them with the inbound
// turn that caused them — IBAC and sparc depend on it. ClientAffinity must not reach
// that case at all: every row here is traffic from no known coding agent.
func TestClientAffinity_InClusterResolutionIsUnchanged(t *testing.T) {
	cases := []struct {
		name       string
		inbound    bool   // seed an inbound A2A turn under conv-A
		ua         string // "" keeps Go's default User-Agent
		header     string // X-Claude-Code-Session-Id, "" for none
		nilHeaders bool   // the transparent path: no request to read
		wantPlugin string
		wantRecord string
	}{
		{name: "A2A turn, Go client", inbound: true, wantPlugin: "conv-A", wantRecord: "conv-A"},
		{name: "A2A turn, agent library UA", inbound: true, ua: "python-httpx/0.27.0", wantPlugin: "conv-A", wantRecord: "conv-A"},
		{name: "A2A turn, transparent", inbound: true, nilHeaders: true, wantPlugin: "conv-A", wantRecord: "conv-A"},
		{name: "nothing active", wantPlugin: "", wantRecord: session.DefaultSessionID},
		{name: "header wins over the A2A turn", inbound: true, header: "hdr-1", wantPlugin: "hdr-1", wantRecord: "hdr-1"},
	}
	for _, affinity := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				store := session.New(5*time.Minute, 100, 0)
				defer store.Close()
				if tc.inbound {
					store.Append("conv-A", pipeline.SessionEvent{
						At:        time.Now(),
						Direction: pipeline.Inbound,
						Phase:     pipeline.SessionRequest,
						A2A:       &pipeline.A2AExtension{Method: "message/send", SessionID: "conv-A"},
					})
				}
				s := &Server{
					Sessions:         store,
					SessionIDHeaders: []string{session.ClaudeCodeSessionHeader},
					ClientAffinity:   affinity,
				}
				var h http.Header
				if !tc.nilHeaders {
					h = http.Header{}
					if tc.ua != "" {
						h.Set("User-Agent", tc.ua)
					}
					if tc.header != "" {
						h.Set(session.ClaudeCodeSessionHeader, tc.header)
					}
				}
				if got := s.resolvePluginSessionID(h, nil); got != tc.wantPlugin {
					t.Errorf("affinity=%v: plugin identity = %q, want %q", affinity, got, tc.wantPlugin)
				}
				if got := s.resolveOutboundSessionID(h, nil); got != tc.wantRecord {
					t.Errorf("affinity=%v: recording bucket = %q, want %q", affinity, got, tc.wantRecord)
				}
			})
		}
	}
}

// TestClientAffinity_InClusterPluginIdentityThroughTheHandler drives the same A2A case
// through the real handler with the knob ON, so a resolver that agreed in isolation but
// was bypassed on the request path would still fail here.
func TestClientAffinity_InClusterPluginIdentityThroughTheHandler(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	store.Append("conv-A", pipeline.SessionEvent{
		At:        time.Now(),
		Direction: pipeline.Inbound,
		Phase:     pipeline.SessionRequest,
		A2A:       &pipeline.A2AExtension{Method: "message/send", SessionID: "conv-A"},
	})
	probe, client, backendURL := newAffinityProxy(t, store)

	get(t, client, backendURL, "")

	if len(probe.sawID) != 1 || probe.sawID[0] != "conv-A" {
		t.Fatalf("plugin saw %v, want [conv-A] (A2A correlation must survive client_affinity)", probe.sawID)
	}
	if v := store.View("conv-A"); v == nil || len(v.Events) != 3 {
		n := -1
		if v != nil {
			n = len(v.Events)
		}
		t.Fatalf("conv-A holds %d events, want 3 (inbound turn, outbound request, its response)", n)
	}
}

// newAffinityProxy is newProbedProxy with ClientAffinity on.
func newAffinityProxy(t *testing.T, store *session.Store) (*identityProbePlugin, *http.Client, string) {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)
	probe := &identityProbePlugin{}
	p, err := pipeline.New([]pipeline.Plugin{probe})
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Sessions:         store,
		Client:           http.DefaultClient,
		SessionIDHeaders: []string{session.ClaudeCodeSessionHeader, session.BobSessionHeader},
		ClientAffinity:   true,
	}
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(proxy.Close)
	return probe, &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL))}}, backend.URL
}

// TestClientAffinity_SeparatesBobFromClaudeCode replays the order observed on a laptop
// running both agents: Bob's startup probes and task classifier before its first
// X-Task-Id, Claude Code's WebFetch with no header, and a library call from neither.
// With ActiveSession() alone, Bob's pre-header calls landed in Claude's session and the
// WebFetch in Bob's.
func TestClientAffinity_SeparatesBobFromClaudeCode(t *testing.T) {
	const (
		claudeUA   = "claude-cli/2.1.284 (external, cli)"
		webFetchUA = "Claude-User (claude-code/2.1.284; +https://support.anthropic.com/)"
		bobBareUA  = "bob-shell/2.0.5"
		bobSDKUA   = "ai-sdk/5.0.1 openai-compatible/3.0.36 bob-shell/2.0.5"
	)
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	probe, client, backendURL := newAffinityProxy(t, store)

	send := func(path, ua, header, sid string) {
		t.Helper()
		req, _ := http.NewRequest("POST", backendURL+path, nil)
		req.Header.Set("User-Agent", ua)
		if header != "" {
			req.Header.Set(header, sid)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		_ = resp.Body.Close()
	}
	send("/v1/messages", claudeUA, session.ClaudeCodeSessionHeader, "claude-1")
	send("/admin/v1/profile", bobBareUA, "", "")
	send("/inference/v1/chat/completions?classifier", bobSDKUA, "", "")
	send("/inference/v1/chat/completions", bobSDKUA, session.BobSessionHeader, "task-1")
	send("/web/fetch", webFetchUA, "", "")
	send("/api/web/domain_info", "axios/1.15.2", "", "")

	paths := func(id string) []string {
		v := store.View(id)
		if v == nil {
			return nil
		}
		var out []string
		for _, e := range v.Events {
			if e.Phase == pipeline.SessionRequest {
				out = append(out, e.HTTPPath)
			}
		}
		return out
	}
	want := map[string][]string{
		"claude-1":               {"/v1/messages", "/web/fetch"},
		"task-1":                 {"/inference/v1/chat/completions"},
		session.DefaultSessionID: {"/api/web/domain_info"},
		// Bob's calls before its first header stay in its bucket: nothing renames a row.
		// Without a process lookup they cannot be linked to task-1 either (see session.Link).
		session.PendingSessionID("bob"): {"/admin/v1/profile", "/inference/v1/chat/completions"},
	}
	for id, w := range want {
		if got := paths(id); strings.Join(got, ",") != strings.Join(w, ",") {
			t.Errorf("session %s request paths = %v, want %v", id, got, w)
		}
	}
	// The axios call is recorded under default but its PLUGINS hear no identity, the same
	// answer resolvePluginSessionID gives when nothing is known. Handing them "default"
	// would satisfy sessionbudget's opt-in DefaultSessionFallback.
	if got := probe.sawID[len(probe.sawID)-1]; got != "" {
		t.Errorf("plugins saw %q for an unknown client beside two live agents, want \"\"", got)
	}
	// Every response followed its request.
	for id := range want {
		v := store.View(id)
		var req, resp int
		for _, e := range v.Events {
			if e.Phase == pipeline.SessionRequest {
				req++
			} else {
				resp++
			}
		}
		if req != resp {
			t.Errorf("session %s holds %d requests and %d responses", id, req, resp)
		}
	}
}

// TestRecordTunnelOpened_UsesTheGatedSessionOnlyUnderAffinity pins #1187's half of this
// change: with the knob on the tunnel row joins the session its CONNECT was gated under;
// with it off, recording still reads ActiveSession() as it always has.
func TestRecordTunnelOpened_UsesTheGatedSessionOnlyUnderAffinity(t *testing.T) {
	for _, affinity := range []bool{false, true} {
		store := session.New(5*time.Minute, 100, 0)
		store.Append("active", pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Inbound, Phase: pipeline.SessionRequest})
		s := &Server{Sessions: store, ClientAffinity: affinity}
		pctx := &pipeline.Context{Direction: pipeline.Outbound, Method: "CONNECT", Host: "api.example:443", OutboundSessionID: "gated"}
		s.recordTunnelOpened(pctx, "")
		want := "active"
		if affinity {
			want = "gated"
		}
		if v := store.View(want); v == nil || v.Events[len(v.Events)-1].HTTPMethod != "CONNECT" {
			t.Errorf("affinity=%v: tunnel row not recorded under %q", affinity, want)
		}
		store.Close()
	}
}

// TestClientAffinity_OffKeepsTodaysAttribution pins the knob's off position to the
// attribution it replaces, misfiling included: Bob's pre-header probe lands in Claude's
// session because Claude's was the one updated last. A change that turned affinity on
// regardless of the knob passes every in-cluster test — those are built to be unaffected —
// and fails this one.
func TestClientAffinity_OffKeepsTodaysAttribution(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store, SessionIDHeaders: []string{session.ClaudeCodeSessionHeader, session.BobSessionHeader}}
	store.Append("claude-1", pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest})
	h := http.Header{"User-Agent": []string{"bob-shell/2.0.5"}}
	if got := s.resolveOutboundSessionID(h, nil); got != "claude-1" {
		t.Fatalf("knob off: Bob's header-less probe = %q, want claude-1 (today's ActiveSession answer)", got)
	}
}

// TestClientAffinity_NeedsHeaderBucketing pins affinityOn's id_headers term: with header
// bucketing off no session is ever any agent's, so a known agent would collect in its
// pending bucket forever. The knob then does nothing.
func TestClientAffinity_NeedsHeaderBucketing(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store, SessionIDHeaders: []string{}, ClientAffinity: true}
	h := http.Header{"User-Agent": []string{"bob-shell/2.0.5"}}
	if got := s.resolveOutboundSessionID(h, nil); got != session.DefaultSessionID {
		t.Fatalf("id_headers: [] with client_affinity = %q, want %q (affinity inert)", got, session.DefaultSessionID)
	}
}

// TestClientAffinity_TunnelRowsKeepTodaysSessionWhenTheOwnerIsAmbiguous replays the
// review's case: tunnels with no User-Agent — most of a laptop's CONNECTs — while two
// agents are recent. Filing those under default split every bridged call from the
// request it carries. A known agent's tunnel still follows its pin.
func TestClientAffinity_TunnelRowsKeepTodaysSessionWhenTheOwnerIsAmbiguous(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = backend.Close() }()
	go func() {
		for c, err := backend.Accept(); err == nil; c, err = backend.Accept() {
			go func() { _, _ = io.Copy(io.Discard, c); _ = c.Close() }()
		}
	}()
	store := session.New(0, 100, 0)
	defer store.Close()
	p, _ := pipeline.New(nil)
	srv := &Server{OutboundPipeline: pipeline.NewHolder(p), Sessions: store, Client: http.DefaultClient,
		SessionIDHeaders: []string{session.ClaudeCodeSessionHeader, session.BobSessionHeader}, ClientAffinity: true}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()
	// Bob then Claude Code name their sessions through the proxy: ActiveSession() is claude-1.
	answers := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer answers.Close()
	via := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL))}}
	for _, c := range [][3]string{
		{"task-1", "bob-shell/2.0.5", session.BobSessionHeader},
		{"claude-1", "claude-cli/2.1.286 (external, cli)", session.ClaudeCodeSessionHeader},
	} {
		req, _ := http.NewRequest(http.MethodGet, answers.URL, nil)
		req.Header.Set("User-Agent", c[1])
		req.Header.Set(c[2], c[0])
		resp, err := via.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	waitFor := func(id string, want int) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			n := 0
			for _, e := range store.View(id).Events {
				if e.HTTPMethod == http.MethodConnect {
					n++
				}
			}
			if n >= want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s holds %d tunnel rows, want %d; default holds %v", id, n, want, store.View(session.DefaultSessionID))
			}
		}
	}
	connect := func(uaLine string) {
		t.Helper()
		c, err := net.Dial("tcp", proxy.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		_, _ = fmt.Fprintf(c, "CONNECT %[1]s HTTP/1.1\r\nHost: %[1]s\r\n%[2]s\r\n", backend.Addr(), uaLine)
		if resp, err := http.ReadResponse(bufio.NewReader(c), nil); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("CONNECT: %v %v", resp, err)
		}
	}
	connect("")
	waitFor("claude-1", 1)
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	go srv.HandleTransparentConn(server, backend.Addr().String())
	waitFor("claude-1", 2)
	connect("User-Agent: bob-shell/2.0.5\r\n")
	waitFor("task-1", 1)
	// The reject paths file through tunnelSessionID rather than the pin.
	if got, def := srv.tunnelSessionID("", nil, nil), srv.recordingSessionID("", nil, nil); got != "task-1" || def != session.DefaultSessionID {
		t.Errorf("ambiguous denial filed under %q (recordingSessionID %q), want task-1, today's ActiveSession()", got, def)
	}
}
