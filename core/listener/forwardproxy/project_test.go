package forwardproxy

import (
	"bufio"
	"crypto/tls"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// Tests for #1815: each forward-proxy request records the project of its agent process.

// fakeProjects is a counting ProjectOf that returns "~/repo" for every pid.
type fakeProjects struct {
	mu   sync.Mutex
	pids []int32
}

func (f *fakeProjects) of(pid int32) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pids = append(f.pids, pid)
	return "~/repo"
}

func (f *fakeProjects) calls() []int32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.pids)
}

// requestProjects is the Project of every request event recorded under id.
func requestProjects(store *session.Store, id string) []string {
	v := store.View(id)
	if v == nil {
		return nil
	}
	var out []string
	for _, e := range v.Events {
		if e.Phase == pipeline.SessionRequest && !e.Tunnel {
			out = append(out, e.Project)
		}
	}
	return out
}

func TestProject_OncePerConnection(t *testing.T) {
	procs := newFakeProcs(fproc(100, 50, "/bin/claude"))
	store := session.New(0, 0, 0)
	defer store.Close()
	fp := &fakeProjects{}
	proxyURL, backendURL, _ := newProcessProxy(t, store, procs, func(s *Server) { s.ProjectOf = fp.of })
	claude := procs.clientFor(proxyURL, 100)
	for i := 0; i < 2; i++ {
		sendAs(t, claude, backendURL+"/v1/messages", procClaudeUA, session.ClaudeCodeSessionHeader, "s1")
	}
	if got := fp.calls(); len(got) != 1 || got[0] != 100 {
		t.Errorf("ProjectOf calls = %v, want one call for pid 100", got)
	}
	if got := requestProjects(store, "s1"); !slices.Equal(got, []string{"~/repo", "~/repo"}) {
		t.Errorf("request projects = %q, want ~/repo twice", got)
	}
}

func TestProject_NilChainCarriesEmpty(t *testing.T) {
	store := session.New(0, 0, 0)
	defer store.Close()
	fp := &fakeProjects{}
	proxyURL, backendURL, _ := newProcessProxy(t, store, newFakeProcs(), func(s *Server) { s.ProjectOf = fp.of })
	plain := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxyURL))}}
	sendAs(t, plain, backendURL+"/v1/messages", procClaudeUA, session.ClaudeCodeSessionHeader, "s1")
	if got := requestProjects(store, "s1"); !slices.Equal(got, []string{""}) {
		t.Errorf("request projects = %q, want one empty", got)
	}
	if got := fp.calls(); len(got) != 0 {
		t.Errorf("ProjectOf calls = %v, want none", got)
	}
}

func TestProject_TrustedPathCarriesEmpty(t *testing.T) {
	procs := newFakeProcs(fproc(100, 50, "/bin/claude"))
	store := session.New(0, 0, 0)
	defer store.Close()
	fp := &fakeProjects{}
	proxyURL, backendURL, _ := newProcessProxy(t, store, procs, func(s *Server) {
		s.ProjectOf = fp.of
		s.TrustedSessionID = func(*http.Request) string { return "vm-1" }
	})
	sendAs(t, procs.clientFor(proxyURL, 100), backendURL+"/v1/messages", procClaudeUA, "", "")
	if got := requestProjects(store, "vm-1"); !slices.Equal(got, []string{""}) {
		t.Errorf("request projects = %q, want one empty", got)
	}
	if got := fp.calls(); len(got) != 0 {
		t.Errorf("ProjectOf calls = %v, want none", got)
	}
}

// A worker below its launcher: Attribution.Proc names the launcher, so the project is
// the launcher's cwd, not the worker's.
func TestProject_AgentRootPid(t *testing.T) {
	procs := newFakeProcs(fproc(100, 50, "/bin/claude"), fproc(110, 100, "/bin/claude"))
	store := session.New(0, 0, 0)
	defer store.Close()
	fp := &fakeProjects{}
	proxyURL, backendURL, _ := newProcessProxy(t, store, procs, func(s *Server) { s.ProjectOf = fp.of })
	sendAs(t, procs.clientFor(proxyURL, 100), backendURL+"/v1/messages", procClaudeUA, session.ClaudeCodeSessionHeader, "s1")
	sendAs(t, procs.clientFor(proxyURL, 110), backendURL+"/v1/messages", procClaudeUA, session.ClaudeCodeSessionHeader, "s1")
	if got := fp.calls(); !slices.Equal(got, []int32{100, 100}) {
		t.Errorf("ProjectOf calls = %v, want the launcher's pid 100 for both connections", got)
	}
}

// Review Focus 4: a refusal never reaches hydration, so attributeForRecording pins the
// project. A misdirected request from pid 100 carries the label; the same request on a
// trusted (VM) connection carries "".
func TestProject_RefusalRow(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trusted string
		want    string
	}{
		{"agent process", "", "~/repo"},
		{"trusted session", "vm-1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			procs := newFakeProcs(freshProc(100, 1, exeClaude))
			store := session.New(0, 0, 0)
			defer store.Close()
			fp := &fakeProjects{}
			sc := newBridgeScene(t, store, procs, func(s *Server) {
				s.ProjectOf = fp.of
				if tc.trusted != "" {
					s.TrustedSessionID = func(*http.Request) string { return tc.trusted }
				}
			})
			raw, _, cresp := connectAs(t, procs, sc.proxyURL, sc.target, 100)
			if cresp.StatusCode != http.StatusOK {
				t.Fatalf("CONNECT answered %d", cresp.StatusCode)
			}
			tc2 := tls.Client(raw, &tls.Config{ServerName: "127.0.0.1", RootCAs: sc.trusting})
			defer func() { _ = tc2.Close() }()
			req, _ := http.NewRequest(http.MethodGet, "https://"+sc.target+"/v1/messages", nil)
			req.Host = "support.example"
			if err := req.Write(tc2); err != nil {
				t.Fatalf("write bridged request: %v", err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(tc2), req)
			if err != nil {
				t.Fatalf("read bridged response: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusMisdirectedRequest {
				t.Fatalf("status %d, want 421", resp.StatusCode)
			}
			// The refusal records after it answers, so wait for its row.
			var got []string
			eventually(t, func() bool {
				got = nil
				for _, e := range allEvents(t, store) {
					if e.Phase == pipeline.SessionRequest && !e.Tunnel {
						got = append(got, e.Project)
					}
				}
				return len(got) > 0
			}, "the refusal's request row")
			if !slices.Equal(got, []string{tc.want}) {
				t.Errorf("refusal row projects = %q, want [%q]", got, tc.want)
			}
		})
	}
}

// A WebSocket turn is a request of the handshake's process, so its row carries the
// project that the handshake pinned. Codex Responses over WebSocket records only turn rows.
func TestWebSocketTurn_CarriesTheHandshakeProject(t *testing.T) {
	hs := &pipeline.Context{Direction: pipeline.Outbound, Host: "chatgpt.com:443", Path: "/backend-api/codex/responses"}
	hs.SetProject("~/repo")
	r, err := http.NewRequest(http.MethodGet, "https://chatgpt.com/backend-api/codex/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	ws := &wsSocket{s: &Server{}, hs: hs, r: r}
	ev := ws.turnRequestEvent(ws.newTurnContext([]byte(`{"type":"response.create"}`)))
	if ev.Project != "~/repo" {
		t.Errorf("turn row Project = %q, want the handshake's ~/repo", ev.Project)
	}
}
