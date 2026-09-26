package forwardproxy

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// getUA issues a proxied request carrying a specific User-Agent. An empty ua
// suppresses the header entirely rather than sending a blank one — net/http omits
// the line when the value is "" — which is the "no client" case the recorders have
// to keep as absence.
func getUA(t *testing.T, client *http.Client, backendURL, ua string) {
	t.Helper()
	req, err := http.NewRequest("POST", backendURL+"/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("User-Agent", ua)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	_ = resp.Body.Close()
}

// TestForwardProxy_BothPhasesCarryTheClient is the end-to-end population guard for
// the two recorders on the live request path: the request event built inline in
// serveOutbound and the response event built by recordOutboundResponseEvent.
//
// Driven through a real proxied request rather than by calling the recorders
// directly, because serveOutbound's event is constructed inside the handler and has
// no callable seam — and because this is the only assertion that proves the
// User-Agent actually survives the hop from r.Header into pctx.Headers, which is
// where ClientInfo reads it. BOTH phases are asserted: a turn produces two events,
// and attributing only one of them would halve every per-agent figure.
func TestForwardProxy_BothPhasesCarryTheClient(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	_, client, backendURL := newProbedProxy(t, store)

	getUA(t, client, backendURL, "claude-cli/2.1.14 (external, cli)")

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) < 2 {
		t.Fatalf("expected a request and a response event, got %+v", v)
	}
	var sawRequest, sawResponse bool
	for _, ev := range v.Events {
		if ev.Client == nil {
			t.Fatalf("phase %s: Client is nil; the recorder does not copy pctx.ClientInfo()", ev.Phase)
		}
		if ev.Client.Name != "claude-code" || ev.Client.Version != "2.1.14" {
			t.Errorf("phase %s: Client = %+v, want claude-code/2.1.14", ev.Phase, ev.Client)
		}
		// Raw survives the whole path, which is what makes an agent this parser does
		// not recognise still nameable in a breakdown.
		if ev.Client.Raw != "claude-cli/2.1.14 (external, cli)" {
			t.Errorf("phase %s: Raw = %q, want the verbatim UA", ev.Phase, ev.Client.Raw)
		}
		switch ev.Phase {
		case pipeline.SessionRequest:
			sawRequest = true
		case pipeline.SessionResponse:
			sawResponse = true
		}
	}
	if !sawRequest {
		t.Error("no request-phase event recorded; serveOutbound's site is unasserted")
	}
	if !sawResponse {
		t.Error("no response-phase event recorded; recordOutboundResponseEvent's site is unasserted")
	}
}

// TestForwardProxy_NoUserAgentStaysAbsent pins that absence is preserved rather
// than filled in. A recorder that substituted a placeholder would satisfy the test
// above and invent an agent for traffic that named none — which in a cost table
// reads as a real program that spent real money.
//
// DISCRIMINATING, not one-sided: both requests go through the same proxy into the same
// store, and the events have to split into named ones and absent ones. Asserting only
// "want nil" made this test pass for the wrong reason — deleting the recorders' `Client:`
// assignment outright left it green, because nil was all it checked for — so the nil it
// reports now is the absence of the HEADER rather than the absence of the wiring.
//
// The nil's Label() is deliberately NOT asserted here. Label is nil-safe by construction,
// so `ev.Client.Label() == "unknown"` restates pipeline.EventClient.Label's own contract,
// which its "nil is unknown" table row pins in that package, and it cannot fail for
// anything a recorder does or omits. What a recorder CAN get wrong is which requests carry
// a client at all, and that is what the counts below assert.
func TestForwardProxy_NoUserAgentStaysAbsent(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	_, client, backendURL := newProbedProxy(t, store)

	// The positive control goes first, so both shapes are recorded by the same server into
	// one view and the comparison is between the two REQUESTS rather than between two runs.
	getUA(t, client, backendURL, "claude-cli/2.1.14 (external, cli)")
	getUA(t, client, backendURL, "")

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) < 4 {
		t.Fatalf("expected a request and a response event from each of two requests, got %+v", v)
	}
	var named, absent int
	for _, ev := range v.Events {
		if ev.Client == nil {
			absent++
			continue
		}
		named++
		if ev.Client.Name != "claude-code" || ev.Client.Version != "2.1.14" {
			t.Errorf("phase %s: Client = %+v, want claude-code/2.1.14", ev.Phase, ev.Client)
		}
	}
	if named == 0 {
		t.Error("no event carried a client at all; the recorders do not copy pctx.ClientInfo(), so the absence here is missing WIRING and not a missing User-Agent")
	}
	if absent == 0 {
		t.Error("every event carried a client; the UA-less request was given one, and an invented agent in a cost table reads as a real program that spent real money")
	}
	if named != absent {
		t.Errorf("%d events named a client and %d did not; the two requests record the same events, so an uneven split means one phase is attributed and the other is not — which halves or doubles every per-agent figure",
			named, absent)
	}
}

// TestForwardProxy_UnrecognisedAgentIsStillNameable is the reason Raw is kept
// alongside Name. A coding agent this parser has never heard of must appear in the
// breakdown under its own User-Agent the day someone runs it, not pool with
// untagged traffic until a parser update ships.
func TestForwardProxy_UnrecognisedAgentIsStillNameable(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	_, client, backendURL := newProbedProxy(t, store)

	getUA(t, client, backendURL, "SomeNewAgent/9.9")

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) == 0 {
		t.Fatalf("expected events, got %+v", v)
	}
	ev := v.Events[0]
	if ev.Client == nil {
		t.Fatal("Client is nil; a User-Agent was sent, so this is unrecognised rather than absent")
	}
	if ev.Client.Name != "" {
		t.Errorf("Name = %q, want empty: the parser must not guess a canonical name", ev.Client.Name)
	}
	if got := ev.Client.Label(); got != "SomeNewAgent/9.9" {
		t.Errorf("Label() = %q, want the raw UA — never %q, which is reserved for absence", got, "unknown")
	}
}

// TestRecordOutboundReject_CarriesTheClient covers the third forwardproxy site.
// Called directly: a denial is a terminal event on a path that needs a rejecting
// plugin to reach, and the recorder is the unit under test.
func TestRecordOutboundReject_CarriesTheClient(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store}

	h := http.Header{}
	h.Set("User-Agent", "claude-cli/2.1.14")
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Method:    "POST",
		Host:      "api.anthropic.com",
		Path:      "/v1/messages",
		Headers:   h,
		// The recorder skips a denial that carries no invocations — a content-free
		// SessionDenied would be noise without attribution — so the fixture has to
		// supply one to reach the construction site at all.
		Extensions: pipeline.Extensions{
			Invocations: &pipeline.Invocations{
				Outbound: []pipeline.Invocation{{
					Plugin: "rate-limit", Phase: pipeline.InvocationPhaseRequest,
					Action: pipeline.ActionDeny, Reason: "over budget",
				}},
			},
		},
	}
	s.recordOutboundReject(pctx, pipeline.Action{Type: pipeline.Reject}, session.DefaultSessionID)

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 1 {
		t.Fatalf("expected 1 denial event, got %+v", v)
	}
	if got := v.Events[0].Client.Label(); got != "claude-code/2.1.14" {
		t.Errorf("Client.Label() = %q, want claude-code/2.1.14", got)
	}
}

// TestRecordTunnelOpened_CarriesTheClient covers the transparent.go site.
//
// A tunnel-open is the one recorder whose client may legitimately be absent even
// from a real agent: a transparently redirected connection has no HTTP request to
// read a header from. Asserted with the header present because the proxied-CONNECT
// half of the same function does have one, and an unattributed tunnel is the
// weaker of the two claims to pin.
func TestRecordTunnelOpened_CarriesTheClient(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store}

	h := http.Header{}
	h.Set("User-Agent", "claude-cli/2.1.14")
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Method:    "CONNECT",
		Host:      "api.anthropic.com:443",
		Headers:   h,
	}
	s.recordTunnelOpened(pctx, pipeline.TunnelPassthroughHost)

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 1 {
		t.Fatalf("expected 1 tunnel-open event, got %+v", v)
	}
	if got := v.Events[0].Client.Label(); got != "claude-code/2.1.14" {
		t.Errorf("Client.Label() = %q, want claude-code/2.1.14", got)
	}
}

// TestRecordTunnelOpened_TransparentRedirectHasNoClient pins the accurate negative:
// a transparently redirected connection carries no HTTP headers at all, so its
// tunnel row genuinely has no client. Without this test someone "fixing" the
// missing attribution would be fixing a correct answer — the same asymmetry
// TestRecordTunnelOpened_EmptyPathIsAccurate pins for HTTPPath.
func TestRecordTunnelOpened_TransparentRedirectHasNoClient(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{Sessions: store}

	// Mirrors HandleTransparentConn's context: synthetic CONNECT, no Headers.
	s.recordTunnelOpened(&pipeline.Context{
		Direction: pipeline.Outbound,
		Method:    "CONNECT",
		Host:      "api.anthropic.com:443",
	}, pipeline.TunnelPassthroughHost)

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 1 {
		t.Fatalf("expected 1 tunnel-open event, got %+v", v)
	}
	if ev := v.Events[0]; ev.Client != nil {
		t.Errorf("Client = %+v, want nil: opaque redirected bytes carry no headers", ev.Client)
	}
}

// uaRewritePlugin rewrites the User-Agent on pctx.Headers, which is what every
// header-mutating plugin does and what the forwarded request is rebuilt from (see
// serveOutbound's header-sync block). It is the plugin that does not exist yet: nothing in
// the tree rewrites this header today, which is exactly why the ordering below has to be
// pinned by a test rather than left to hold by luck.
//
// set == "" DELETES the header instead, which is the invented-agent direction of the same
// defect run backwards.
type uaRewritePlugin struct{ set string }

func (p *uaRewritePlugin) Name() string { return "ua-rewrite" }
func (p *uaRewritePlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (p *uaRewritePlugin) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	if p.set == "" {
		pctx.Headers.Del("User-Agent")
	} else {
		pctx.Headers.Set("User-Agent", p.set)
	}
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *uaRewritePlugin) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// newProxyWithPlugin is newProbedProxy with a caller-supplied plugin, for the tests that
// need the pipeline to MUTATE the request rather than observe it.
func newProxyWithPlugin(t *testing.T, store *session.Store, plug pipeline.Plugin) (*http.Client, string) {
	t.Helper()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)

	p, err := pipeline.New([]pipeline.Plugin{plug})
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Sessions:         store,
		Client:           http.DefaultClient,
	}
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(proxy.Close)

	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL))}}
	return client, backend.URL
}

// TestForwardProxy_ClientIsResolvedAtConstructionNotOnFirstUse.
//
// Context.ClientInfo memoizes on FIRST CALL, and every call site in this file is an
// event-construction site downstream of the pipeline. pctx.Headers is a clone that plugins
// write to, so without the pin at construction the attribution of a request is decided by
// whichever recording site asks first, AFTER the pipeline has had its way with the header.
//
// This test distinguishes the two: the plugin rewrites User-Agent to an agent that never
// made the call, and both events must still name the one that did. It fails if the pin is
// removed and equally if it is merely MOVED after OutboundPipeline.Run — which is the whole
// difference between "resolved once" and "resolved before anything can change it".
//
// Latent today (no plugin in the tree touches this header) and asserted anyway, because the
// failure mode is a silent one: spend re-filed under another program's name, with nothing in
// the event to say it happened.
func TestForwardProxy_ClientIsResolvedAtConstructionNotOnFirstUse(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	client, backendURL := newProxyWithPlugin(t, store, &uaRewritePlugin{set: "impostor/9.9"})

	getUA(t, client, backendURL, "claude-cli/2.1.14 (external, cli)")

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) < 2 {
		t.Fatalf("expected a request and a response event, got %+v", v)
	}
	for _, ev := range v.Events {
		if ev.Client == nil {
			t.Fatalf("phase %s: Client is nil", ev.Phase)
		}
		if ev.Client.Raw != "claude-cli/2.1.14 (external, cli)" {
			t.Errorf("phase %s: Client.Raw = %q, want the User-Agent the CLIENT sent; a plugin "+
				"rewrote the header and the attribution followed it, so this request's spend is "+
				"filed under a program that never made it", ev.Phase, ev.Client.Raw)
		}
		if ev.Client.Name != "claude-code" {
			t.Errorf("phase %s: Client.Name = %q, want claude-code", ev.Phase, ev.Client.Name)
		}
	}
	// Both events agree. Without the pin the two sites parse the same mutated header and
	// agree on the WRONG answer, so this is the weaker half — kept because a future change
	// that pins only one phase would halve every per-agent figure.
	if a, b := v.Events[0].Client.Raw, v.Events[1].Client.Raw; a != b {
		t.Errorf("request and response disagree about the client (%q vs %q); the answer depends "+
			"on which site asked first", a, b)
	}
}

// TestForwardProxy_APluginCannotInventAClient is the same defect run backwards, and the
// direction that costs money on a cost table: a plugin that DELETES the header cannot turn a
// named agent into untagged traffic. Pinned separately because a "resolve later" bug that
// happens to preserve names could still lose them, and vice versa.
func TestForwardProxy_APluginCannotInventAClient(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	client, backendURL := newProxyWithPlugin(t, store, &uaRewritePlugin{set: ""})

	getUA(t, client, backendURL, "claude-cli/2.1.14 (external, cli)")

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) < 2 {
		t.Fatalf("expected a request and a response event, got %+v", v)
	}
	for _, ev := range v.Events {
		if ev.Client == nil {
			t.Fatalf("phase %s: Client is nil — a plugin deleted the header and the agent that "+
				"actually made this call became untagged traffic", ev.Phase)
		}
		if ev.Client.Name != "claude-code" {
			t.Errorf("phase %s: Client = %+v, want claude-code/2.1.14", ev.Phase, ev.Client)
		}
	}
}

// TestHandleTransparentConn_APluginCannotInventAClient covers the third construction site in
// this package, and the direction that matters there: a transparently redirected connection
// has NO HTTP request, so its Context is built with empty headers and the honest answer is
// "no client".
//
// Empty headers are still WRITABLE. Without the pin at construction, ClientInfo's memo fills
// at recordTunnelOpened — after the gate pipeline has run — so a plugin that set a User-Agent
// on those headers would give the tunnel row an agent that never existed. An invented agent
// in a cost table reads as a real program that spent real money, which is the claim
// TestForwardProxy_NoUserAgentStaysAbsent makes for the HTTP path.
func TestHandleTransparentConn_APluginCannotInventAClient(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen upstream: %v", err)
	}
	defer func() { _ = upstream.Close() }()
	go func() {
		c, err := upstream.Accept()
		if err != nil {
			return
		}
		// Held open only long enough for the tunnel to be recorded; the test closes
		// the agent side, which tears both down.
		time.Sleep(500 * time.Millisecond)
		_ = c.Close()
	}()

	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	p, err := pipeline.New([]pipeline.Plugin{&uaRewritePlugin{set: "impostor/9.9"}})
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{OutboundPipeline: pipeline.NewHolder(p), Sessions: store}

	agentSide, proxySide := net.Pipe()
	go srv.HandleTransparentConn(proxySide, upstream.Addr().String())
	defer func() { _ = agentSide.Close() }()

	// The event lands after the gate runs and the tunnel opens, on another goroutine.
	var v *pipeline.SessionView
	for i := 0; i < 100; i++ {
		if v = store.View(session.DefaultSessionID); v != nil && len(v.Events) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if v == nil || len(v.Events) == 0 {
		t.Fatalf("no tunnel-open event recorded, so nothing here is asserted: %+v", v)
	}
	if ev := v.Events[0]; ev.Client != nil {
		t.Errorf("Client = %+v, want nil: a plugin wrote a User-Agent into a redirected "+
			"connection's empty headers and the tunnel row took it as the calling agent",
			ev.Client)
	}
}
