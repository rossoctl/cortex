package forwardproxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// postAs issues a proxied request carrying the given X-Claude-Code-Agent-Id and
// X-Claude-Code-Parent-Agent-Id, sending neither header whose value is empty.
func postAs(t *testing.T, client *http.Client, backendURL, id, parent string) {
	t.Helper()
	req, err := http.NewRequest("POST", backendURL+"/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if id != "" {
		// Lowercase, as Claude Code sends it: the name reaching pctx.Headers canonicalized
		// is part of what this asserts.
		req.Header.Set("x-claude-code-agent-id", id)
	}
	if parent != "" {
		req.Header.Set("x-claude-code-parent-agent-id", parent)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	_ = resp.Body.Close()
}

// TestForwardProxy_BothPhasesCarryTheSubagent is the end-to-end population guard for the
// two recorders on the live request path, for the reason
// TestForwardProxy_BothPhasesCarryTheClient gives: serveOutbound's event has no callable
// seam, and only a real request proves the header survives the hop into pctx.Headers.
//
// DISCRIMINATING, like TestForwardProxy_NoUserAgentStaysAbsent: a subagent's request and
// a main-conversation one go through the same proxy into the same store, and the events
// have to split evenly between them. A recorder that never copied the field would leave
// every event empty, and one that invented it would leave none empty; both fail here.
func TestForwardProxy_BothPhasesCarryTheSubagent(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	_, client, backendURL := newProbedProxy(t, store)

	// A nested subagent, so the parent header is exercised on both live sites too.
	postAs(t, client, backendURL, "a1a2e36cb5946f31c", "a0974b0b8591dc76b")
	postAs(t, client, backendURL, "", "")

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) < 4 {
		t.Fatalf("expected a request and a response event from each of two requests, got %+v", v)
	}
	named := map[pipeline.SessionPhase]int{}
	var absent int
	for _, ev := range v.Events {
		switch ev.Subagent {
		case "":
			absent++
			if ev.ParentSubagent != "" {
				t.Errorf("phase %s: ParentSubagent = %q on a main-conversation row, want empty", ev.Phase, ev.ParentSubagent)
			}
		case "a1a2e36cb5946f31c":
			named[ev.Phase]++
			if ev.ParentSubagent != "a0974b0b8591dc76b" {
				t.Errorf("phase %s: ParentSubagent = %q, want a0974b0b8591dc76b; the recorder does not copy pctx.ParentSubagent()",
					ev.Phase, ev.ParentSubagent)
			}
		default:
			t.Errorf("phase %s: Subagent = %q, want a1a2e36cb5946f31c or empty", ev.Phase, ev.Subagent)
		}
	}
	if named[pipeline.SessionRequest] != 1 {
		t.Errorf("%d request events named the subagent, want 1; serveOutbound's site does not copy pctx.Subagent()",
			named[pipeline.SessionRequest])
	}
	if named[pipeline.SessionResponse] != 1 {
		t.Errorf("%d response events named the subagent, want 1; recordOutboundResponseEvent does not copy pctx.Subagent()",
			named[pipeline.SessionResponse])
	}
	if absent != 2 {
		t.Errorf("%d events named no subagent, want 2: the main-conversation request's two", absent)
	}
}

// TestRecordTunnel_CarriesTheSubagent covers the two transparent.go sites, called
// directly as TestRecordTunnelOpened_CarriesTheClient calls the first. A proxied CONNECT
// does have headers, so a subagent's tunnel row can name it.
func TestRecordTunnel_CarriesTheSubagent(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store}

	h := http.Header{}
	h.Set(pipeline.ClaudeCodeAgentIDHeader, "a1a2e36cb5946f31c")
	h.Set(pipeline.ClaudeCodeParentAgentIDHeader, "a0974b0b8591dc76b")
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Method:    "CONNECT",
		Host:      "api.anthropic.com:443",
		Headers:   h,
	}
	b := s.recordTunnelOpened(pctx, pipeline.TunnelPassthroughHost)
	s.recordTunnelClosed(pctx, b, pipeline.TunnelPassthroughHost, 200, nil, 10, 20)

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 2 {
		t.Fatalf("expected a tunnel open and close, got %+v", v)
	}
	for _, ev := range v.Events {
		if ev.Subagent != "a1a2e36cb5946f31c" || ev.ParentSubagent != "a0974b0b8591dc76b" {
			t.Errorf("phase %s: Subagent/ParentSubagent = %q/%q, want a1a2e36cb5946f31c/a0974b0b8591dc76b",
				ev.Phase, ev.Subagent, ev.ParentSubagent)
		}
	}
}

// TestRecordWebSocketClose_CarriesTheProcAndSubagent covers the WebSocket close row. It
// must name the process and the subagent as the tunnel close row does. A hand-built row
// once dropped all three fields.
func TestRecordWebSocketClose_CarriesTheProcAndSubagent(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store}

	h := http.Header{}
	h.Set(pipeline.ClaudeCodeAgentIDHeader, "a1a2e36cb5946f31c")
	h.Set(pipeline.ClaudeCodeParentAgentIDHeader, "a0974b0b8591dc76b")
	hs := &pipeline.Context{
		Direction: pipeline.Outbound,
		Method:    http.MethodGet,
		Host:      "chatgpt.com:443",
		Path:      "/backend-api/codex/responses",
		Headers:   h,
		// An old handshake start. The close row must time the socket from the 101.
		StartedAt: time.Now().Add(-time.Hour),
	}
	hs.SetAttribution("codex", "", "")
	ws := &wsSocket{s: s, hs: hs, sid: "ws-sess", start: time.Now()}
	ws.bytesUp.Store(10)
	ws.bytesDown.Store(20)
	ws.recordClose()

	v := store.View("ws-sess")
	if v == nil || len(v.Events) != 1 {
		t.Fatalf("expected one close row, got %+v", v)
	}
	ev := v.Events[0]
	if ev.Proc != "codex" {
		t.Errorf("Proc = %q, want codex", ev.Proc)
	}
	if ev.Subagent != "a1a2e36cb5946f31c" || ev.ParentSubagent != "a0974b0b8591dc76b" {
		t.Errorf("Subagent/ParentSubagent = %q/%q, want a1a2e36cb5946f31c/a0974b0b8591dc76b",
			ev.Subagent, ev.ParentSubagent)
	}
	if ev.Phase != pipeline.SessionResponse || !ev.Tunnel || ev.TunnelReason != pipeline.TunnelWebSocket ||
		ev.StatusCode != http.StatusSwitchingProtocols || ev.BytesUp != 10 || ev.BytesDown != 20 {
		t.Errorf("close row shape changed: %+v", ev)
	}
	if ev.Duration <= 0 || ev.Duration >= time.Minute {
		t.Errorf("Duration = %v, want the time since the 101, not since the handshake", ev.Duration)
	}
}

// TestForwardProxy_BobChildFilesInRoot is the end-to-end guard for rules 1 and 2 of
// #1803: a Bob child's rows file in its root's session and name the child, while a
// child with an unusable parent id, or one that names itself, stays a root in its own
// session.
func TestForwardProxy_BobChildFilesInRoot(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	p, err := pipeline.New([]pipeline.Plugin{&identityProbePlugin{}})
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Sessions:         store,
		Client:           http.DefaultClient,
		SessionIDHeaders: []string{session.BobSessionHeader},
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL))}}

	post := func(task, parent string) {
		t.Helper()
		req, _ := http.NewRequest("POST", backend.URL+"/v1/messages", strings.NewReader(`{}`))
		req.Header.Set(pipeline.BobTaskIDHeader, task)
		if parent != "" {
			req.Header.Set(pipeline.BobParentTaskIDHeader, parent)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		_ = resp.Body.Close()
	}
	post("r1", "")
	post("c1", "r1")
	post("c2", strings.Repeat("r", session.MaxSessionIDLen+44)) // over the limit: not a claim
	post("c3", "c3")

	// rows counts each session's rows by (Subagent, SubagentKind, phase).
	type key struct {
		sub, kind string
		phase     pipeline.SessionPhase
	}
	rows := func(id string) map[key]int {
		v := store.View(id)
		if v == nil {
			t.Fatalf("no session %q", id)
		}
		out := map[key]int{}
		for _, ev := range v.Events {
			out[key{ev.Subagent, ev.SubagentKind, ev.Phase}]++
		}
		return out
	}
	req, resp := pipeline.SessionRequest, pipeline.SessionResponse
	want := map[string]map[key]int{
		"r1": {{"", "", req}: 1, {"", "", resp}: 1, {"c1", pipeline.SubagentKindSubagent, req}: 1, {"c1", pipeline.SubagentKindSubagent, resp}: 1},
		"c2": {{"", "", req}: 1, {"", "", resp}: 1},
		"c3": {{"", "", req}: 1, {"", "", resp}: 1},
	}
	for id, w := range want {
		got := rows(id)
		if len(got) != len(w) {
			t.Errorf("session %s: rows %v, want %v", id, got, w)
			continue
		}
		for k, n := range w {
			if got[k] != n {
				t.Errorf("session %s: rows %v, want %v", id, got, w)
				break
			}
		}
	}
	if v := store.View("c1"); v != nil {
		t.Errorf("session c1 exists with %d events; a child must file in its root", len(v.Events))
	}
}
