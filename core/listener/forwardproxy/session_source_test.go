package forwardproxy

import (
	"github.com/rossoctl/cortex/core/attribution"
	"net/http"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// The handler records on pctx how it resolved the session it hands plugins.
func TestSessionSourceThroughTheHandler(t *testing.T) {
	const claudeUA = "claude-cli/2.1.284 (external, cli)"
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	probe, client, backendURL := newAffinityProxy(t, store)

	send := func(ua, header, sid string) {
		t.Helper()
		req, _ := http.NewRequest("POST", backendURL+"/x", nil)
		req.Header.Set("User-Agent", ua)
		if header != "" {
			req.Header.Set(header, sid)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	send(claudeUA, "", "")                                      // pending:claude-code
	send(claudeUA, session.ClaudeCodeSessionHeader, "claude-1") // header
	send(claudeUA, "", "")                                      // affinity
	send("curl/8.7.1", "", "")                                  // active
	send("curl/8.7.1", session.SessionIDHeader, "spoofed")      // not a configured header: active

	want := []struct {
		id     string
		source pipeline.SessionSource
		header string
	}{
		{session.PendingSessionID("claude-code"), pipeline.SessionSourcePending, ""},
		{"claude-1", pipeline.SessionSourceHeader, session.ClaudeCodeSessionHeader},
		{"claude-1", pipeline.SessionSourceAffinity, ""},
		{"claude-1", pipeline.SessionSourceActive, ""},
		{"claude-1", pipeline.SessionSourceActive, ""},
	}
	if len(probe.sawSource) != len(want) {
		t.Fatalf("plugin saw %d sessions (%v), want %d", len(probe.sawSource), probe.sawID, len(want))
	}
	for i, w := range want {
		if probe.sawID[i] != w.id || probe.sawSource[i] != w.source || probe.sawHeader[i] != w.header {
			t.Errorf("request %d: session %q via %q/%q, want %q via %q/%q",
				i, probe.sawID[i], probe.sawSource[i], probe.sawHeader[i], w.id, w.source, w.header)
		}
	}
}

// TrustedSessionID, when it answers, ends the search: the session is resolved
// from how the connection arrived, and a header naming another session — or
// the process chain, or affinity — must not override it. vmcontrol's registry
// tests drive the same seam end to end; this pins the resolution itself.
func TestTrustedSessionIDOverridesEveryOtherSource(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{
		Sessions:         store,
		SessionIDHeaders: []string{session.ClaudeCodeSessionHeader},
		TrustedSessionID: func(r *http.Request) string {
			if r.Header.Get("X-Which-VM") == "vm-a" {
				return "trusted-sid"
			}
			return ""
		},
	}
	agent := attribution.Proc{PID: 100, Start: 1}
	store.Append("trusted-sid", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest})

	h := http.Header{}
	h.Set(session.ClaudeCodeSessionHeader, "header-sid")
	h.Set("X-Which-VM", "vm-a")
	pctx := &pipeline.Context{}
	if sid := s.hydrateSession(pctx, s.trustedSessionOf(&http.Request{Header: h}), h, []attribution.Proc{agent}); sid != "trusted-sid" {
		t.Fatalf("session %q, want the trusted trusted-sid", sid)
	}
	if pctx.Session == nil || pctx.Session.ID != "trusted-sid" {
		t.Fatalf("pctx.Session = %+v, want trusted-sid", pctx.Session)
	}
	if pctx.SessionSource != pipeline.SessionSourceConnection || pctx.SessionHeader != "" {
		t.Errorf("source %q/%q, want connection with no header named", pctx.SessionSource, pctx.SessionHeader)
	}

	// A request the hook does not claim falls through to the old resolution
	// unchanged.
	h.Del("X-Which-VM")
	pctx = &pipeline.Context{}
	if sid := s.hydrateSession(pctx, s.trustedSessionOf(&http.Request{Header: h}), h, nil); sid != "header-sid" {
		t.Fatalf("unclaimed request: session %q, want header-sid", sid)
	}
	if pctx.SessionSource != pipeline.SessionSourceHeader {
		t.Errorf("unclaimed request: source %q, want header", pctx.SessionSource)
	}
}

// A process that never sent a session header takes the session its ancestor
// named, and that is recorded as a process resolution.
func TestSessionSourceProcessChain(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{
		Sessions:         store,
		SessionIDHeaders: []string{session.ClaudeCodeSessionHeader},
		Processes:        newFakeProcs(),
	}
	agent := attribution.Proc{PID: 100, Start: 1}
	tool := attribution.Proc{PID: 200, Start: 2}

	h := http.Header{"User-Agent": {procClaudeUA}}
	h.Set(session.ClaudeCodeSessionHeader, "claude-1")
	store.Append("claude-1", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest})
	if sid, how := s.resolvePluginSession(h, []attribution.Proc{agent}); sid != "claude-1" || how.source != pipeline.SessionSourceHeader {
		t.Fatalf("agent: %q via %q, want claude-1 via header", sid, how.source)
	}
	pctx := &pipeline.Context{}
	if sid := s.hydrateSession(pctx, "", http.Header{}, []attribution.Proc{tool, agent}); sid != "claude-1" {
		t.Fatalf("tool: session %q, want claude-1", sid)
	}
	if pctx.SessionSource != pipeline.SessionSourceProcess || pctx.SessionHeader != "" {
		t.Fatalf("tool: source %q/%q, want process", pctx.SessionSource, pctx.SessionHeader)
	}
}

// A child of the agent that names a session of its own — by header, or later from
// the claim that header left — still carries the agent's session as ChainSession, so
// a policy keyed on it cannot be escaped. The agent's own requests carry none.
func TestChainSessionOverridesAChildsOwnSession(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{
		Sessions:         store,
		SessionIDHeaders: []string{session.ClaudeCodeSessionHeader},
		Processes:        newFakeProcs(),
	}
	agent := attribution.Proc{PID: 100, Start: 1}
	shell := attribution.Proc{PID: 150, Start: 2}
	child := attribution.Proc{PID: 200, Start: 3}

	named := func(sid string) http.Header {
		h := http.Header{"User-Agent": {procClaudeUA}}
		h.Set(session.ClaudeCodeSessionHeader, sid)
		return h
	}
	pctx := &pipeline.Context{}
	hAgent := named("claude-1")
	s.hydrateSession(pctx, s.trustedSessionOf(&http.Request{Header: hAgent}), hAgent, []attribution.Proc{agent, shell})
	store.Append("claude-1", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest})
	if pctx.ChainSession != "" {
		t.Errorf("agent: ChainSession %q, want none", pctx.ChainSession)
	}

	pctx = &pipeline.Context{}
	hChild := named("fresh")
	sid := s.hydrateSession(pctx, s.trustedSessionOf(&http.Request{Header: hChild}), hChild, []attribution.Proc{child, shell, agent})
	store.Append("fresh", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest})
	if sid != "fresh" || pctx.SessionSource != pipeline.SessionSourceHeader {
		t.Fatalf("child: recorded under %q via %q, want fresh via header", sid, pctx.SessionSource)
	}
	if pctx.ChainSession != "claude-1" {
		t.Errorf("child with its own header: ChainSession %q, want claude-1", pctx.ChainSession)
	}

	pctx = &pipeline.Context{}
	if sid := s.hydrateSession(pctx, "", http.Header{}, []attribution.Proc{child, shell, agent}); sid != "fresh" {
		t.Fatalf("child without header: %q, want its own claim fresh", sid)
	}
	if pctx.ChainSession != "claude-1" {
		t.Errorf("child without header: ChainSession %q, want claude-1", pctx.ChainSession)
	}
}
