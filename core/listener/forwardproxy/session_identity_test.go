package forwardproxy

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// identityProbePlugin records the session identity the pipeline handed it, which
// is what every session-aware plugin keys on (sessionbudget's Redis counters,
// contextguru's compaction state, sparc). Capturing it in a plugin rather than
// asserting on the store is deliberate: the store is the recording side, and the
// whole point of these tests is that the two sides agree.
type identityProbePlugin struct {
	sawNil    []bool
	sawID     []string
	sawEvents []int
}

func (p *identityProbePlugin) Name() string { return "identity-probe" }
func (p *identityProbePlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (p *identityProbePlugin) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	if pctx.Session == nil {
		p.sawNil = append(p.sawNil, true)
		p.sawID = append(p.sawID, "")
		p.sawEvents = append(p.sawEvents, -1)
		return pipeline.Action{Type: pipeline.Continue}
	}
	p.sawNil = append(p.sawNil, false)
	p.sawID = append(p.sawID, pctx.Session.ID)
	p.sawEvents = append(p.sawEvents, len(pctx.Session.Events))
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *identityProbePlugin) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// newProbedProxy wires a forward proxy whose only plugin is the identity probe,
// with header bucketing on, and returns the probe plus a client that proxies
// through it.
func newProbedProxy(t *testing.T, store *session.Store) (*identityProbePlugin, *http.Client, string) {
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
		SessionIDHeaders: []string{session.ClaudeCodeSessionHeader},
	}
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(proxy.Close)

	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL))}}
	return probe, client, backend.URL
}

// get issues a proxied request, optionally carrying a client session header.
func get(t *testing.T, client *http.Client, backendURL, sid string) {
	t.Helper()
	req, _ := http.NewRequest("POST", backendURL+"/v1/messages", strings.NewReader(`{}`))
	if sid != "" {
		req.Header.Set(session.ClaudeCodeSessionHeader, sid)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	_ = resp.Body.Close()
}

// TestPluginIdentity_FollowsClientSessionNotActiveSession is the core of #984.
// Event recording already resolves the client's session header, but plugins were
// handed ActiveSession() — a single global "most recently updated" id. So with
// two coding-agent sessions in flight, telemetry bucketed correctly while every
// plugin attributed both to whichever spoke last. Two sequential requests are
// enough to show it: after the first, ActiveSession() points at session A, so the
// second must still be told it is session B.
func TestPluginIdentity_FollowsClientSessionNotActiveSession(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	probe, client, backendURL := newProbedProxy(t, store)

	const sidA = "511754a6-63e2-47df-bb22-706dc165c344"
	const sidB = "f4019b9f-3550-442f-9bc7-229809a3b486"

	get(t, client, backendURL, sidA)
	if got := store.ActiveSession(); got != sidA {
		t.Fatalf("precondition: ActiveSession() = %q, want %q", got, sidA)
	}
	get(t, client, backendURL, sidB)

	if len(probe.sawID) != 2 {
		t.Fatalf("probe ran %d times, want 2", len(probe.sawID))
	}
	if probe.sawID[0] != sidA {
		t.Errorf("first request: plugin saw session %q, want %q", probe.sawID[0], sidA)
	}
	if probe.sawID[1] != sidB {
		t.Errorf("second request: plugin saw session %q, want %q — ActiveSession() was %q, which is the bug",
			probe.sawID[1], sidB, sidA)
	}
}

// TestPluginIdentity_AgreesWithRecordedBucket pins the invariant that makes the
// two sides one identity rather than two rules that happen to coincide: whatever
// the plugin was told is where the event was filed.
func TestPluginIdentity_AgreesWithRecordedBucket(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	probe, client, backendURL := newProbedProxy(t, store)

	const sid = "511754a6-63e2-47df-bb22-706dc165c344"
	get(t, client, backendURL, sid)

	if len(probe.sawID) != 1 {
		t.Fatalf("probe ran %d times, want 1", len(probe.sawID))
	}
	pluginSaw := probe.sawID[0]
	v := store.View(pluginSaw)
	if v == nil {
		t.Fatalf("plugin was told session %q but no such bucket was recorded", pluginSaw)
	}
	if len(v.Events) == 0 {
		t.Fatalf("bucket %q has no events; the plugin and the recorder disagree", pluginSaw)
	}
}

// TestPluginIdentity_FirstTurnHasIdentityBeforeAnyEvent covers the ordering trap.
// Hydration runs BEFORE recording, so on a session's first request the bucket
// does not exist yet and Store.View returns nil. Plugins must still be told which
// session this is — sessionbudget skips enforcement entirely on an empty id, so a
// nil view on turn one would let every new session's first call through
// unmetered.
func TestPluginIdentity_FirstTurnHasIdentityBeforeAnyEvent(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	probe, client, backendURL := newProbedProxy(t, store)

	const sid = "511754a6-63e2-47df-bb22-706dc165c344"
	get(t, client, backendURL, sid)

	if len(probe.sawID) != 1 {
		t.Fatalf("probe ran %d times, want 1", len(probe.sawID))
	}
	if probe.sawNil[0] {
		t.Fatalf("plugin saw a nil session on the first turn; it must know the id before any event exists")
	}
	if probe.sawID[0] != sid {
		t.Errorf("plugin saw %q, want %q", probe.sawID[0], sid)
	}
	if probe.sawEvents[0] != 0 {
		t.Errorf("first-turn view has %d events, want 0 — an empty view is the truthful answer",
			probe.sawEvents[0])
	}
}

// TestPluginIdentity_NoHeaderKeepsActiveSession is a characterization test, not a
// driven one. It pins the fallback that makes this change safe in-cluster: an A2A
// agent sends no coding-agent header, so plugins must still receive the session
// ActiveSession() names — the inbound turn that caused the outbound call. IBAC
// and sparc depend on that correlation.
func TestPluginIdentity_NoHeaderKeepsActiveSession(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	store.Append("conv-A", pipeline.SessionEvent{
		At:        time.Now(),
		Direction: pipeline.Inbound,
		Phase:     pipeline.SessionRequest,
		A2A:       &pipeline.A2AExtension{Method: "message/send", SessionID: "conv-A"},
	})
	probe, client, backendURL := newProbedProxy(t, store)

	get(t, client, backendURL, "") // no session header

	if len(probe.sawID) != 1 {
		t.Fatalf("probe ran %d times, want 1", len(probe.sawID))
	}
	if probe.sawID[0] != "conv-A" {
		t.Errorf("plugin saw %q, want %q (A2A correlation must survive)", probe.sawID[0], "conv-A")
	}
	if probe.sawEvents[0] != 1 {
		t.Errorf("view has %d events, want 1 (the inbound A2A turn)", probe.sawEvents[0])
	}
}

// TestPluginIdentity_NoSessionAtAllStaysNil pins the other half of the fallback:
// with no header AND nothing active, plugins keep receiving nil rather than being
// handed the default bucket. Handing them "default" would silently satisfy
// sessionbudget's DefaultSessionFallback — an opt-in config — and start enforcing
// budgets where today it deliberately skips.
func TestPluginIdentity_NoSessionAtAllStaysNil(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	probe, client, backendURL := newProbedProxy(t, store)

	get(t, client, backendURL, "")

	if len(probe.sawNil) != 1 {
		t.Fatalf("probe ran %d times, want 1", len(probe.sawNil))
	}
	if !probe.sawNil[0] {
		t.Errorf("plugin saw session %q, want nil (no header, nothing active)", probe.sawID[0])
	}
}

// flipAndRejectPlugin reproduces the interleaving that makes re-resolution
// unsafe: it appends an event under an unrelated session (flipping the global
// ActiveSession, exactly as a health probe under "default" does) and then
// rejects. Any recorder that re-resolves after the pipeline ran will file the
// denial under the interloper.
type flipAndRejectPlugin struct {
	store  *session.Store
	flipTo string
}

func (p *flipAndRejectPlugin) Name() string { return "flip-and-reject" }
func (p *flipAndRejectPlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (p *flipAndRejectPlugin) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	p.store.Append(p.flipTo, pipeline.SessionEvent{
		At:        time.Now(),
		Direction: pipeline.Inbound,
		Phase:     pipeline.SessionRequest,
		Host:      "health-probe.example",
	})
	return pctx.DenyAndRecord("blocked for test", "test.blocked", "blocked for test")
}
func (p *flipAndRejectPlugin) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// TestRejectPath_RecordsUnderTheIdentityThePluginSaw closes the gap the accept
// path already has. A denial must land in the session that made the request —
// the same identity the plugin that produced the denial was given. Re-resolving
// after the pipeline ran reads ActiveSession() at a later moment, and a guardrail
// judge call is seconds long, so any interleaving traffic in that window flips it
// and the operator sees a block filed against an unrelated session.
//
// Deliberately the NO-HEADER case: with a header the two resolutions agree by
// luck, so only header-less traffic (the in-cluster A2A shape) can show the bug.
func TestRejectPath_RecordsUnderTheIdentityThePluginSaw(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()

	// The agent's inbound A2A turn: this is the session that owns the request.
	store.Append("conv-A", pipeline.SessionEvent{
		At:        time.Now(),
		Direction: pipeline.Inbound,
		Phase:     pipeline.SessionRequest,
		A2A:       &pipeline.A2AExtension{Method: "message/send", SessionID: "conv-A"},
	})

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("backend reached; request should have been rejected")
	}))
	defer backend.Close()

	p, err := pipeline.New([]pipeline.Plugin{&flipAndRejectPlugin{store: store, flipTo: session.DefaultSessionID}})
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Sessions:         store,
		Client:           http.DefaultClient,
		SessionIDHeaders: []string{session.ClaudeCodeSessionHeader},
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL))}}

	get(t, client, backend.URL, "") // no session header

	countDenied := func(id string) int {
		v := store.View(id)
		if v == nil {
			return 0
		}
		n := 0
		for _, e := range v.Events {
			if e.Phase == pipeline.SessionDenied {
				n++
			}
		}
		return n
	}
	if got := countDenied("conv-A"); got != 1 {
		t.Errorf("denial recorded in conv-A: got %d, want 1", got)
	}
	if got := countDenied(session.DefaultSessionID); got != 0 {
		t.Errorf("denial leaked into the session that flipped ActiveSession mid-pipeline: got %d, want 0", got)
	}
}

// pinHijackPlugin writes pctx.OutboundSessionID, which is an exported field a
// plugin can reach.
type pinHijackPlugin struct{ hijackTo string }

func (p *pinHijackPlugin) Name() string { return "pin-hijack" }
func (p *pinHijackPlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (p *pinHijackPlugin) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	pctx.OutboundSessionID = p.hijackTo
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *pinHijackPlugin) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// TestPinIsNotPluginWritable guards the hazard created by resolving the identity
// before the pipeline runs: OutboundSessionID is exported, so a plugin that
// writes it could re-file both the request and its paired response event.
// resolveOutboundSessionID's own contract forbids exactly that — a plugin must
// not silently re-file telemetry, and it would fail invisibly because "" means
// "fall back", never an error. The listener's resolution must win.
func TestPinIsNotPluginWritable(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	p, err := pipeline.New([]pipeline.Plugin{&pinHijackPlugin{hijackTo: "attacker-chosen"}})
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Sessions:         store,
		Client:           http.DefaultClient,
		SessionIDHeaders: []string{session.ClaudeCodeSessionHeader},
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL))}}

	const sid = "511754a6-63e2-47df-bb22-706dc165c344"
	get(t, client, backend.URL, sid)

	if v := store.View("attacker-chosen"); v != nil && len(v.Events) > 0 {
		t.Errorf("a plugin re-filed %d events by writing OutboundSessionID", len(v.Events))
	}
	v := store.View(sid)
	if v == nil || len(v.Events) != 2 {
		t.Fatalf("expected request+response under %s, got %+v", sid, v)
	}
}

// TestSessionViewFor_NilStoreDoesNotPanic holds sessionViewFor to the convention
// resolveOutboundSessionID states thirty lines above it — safe to call with a nil
// store, so a future caller that forgets the guard gets a usable answer rather
// than a panic. Both current callers guard, so this is latent by design.
func TestSessionViewFor_NilStoreDoesNotPanic(t *testing.T) {
	s := &Server{} // Sessions nil: session tracking disabled
	v := s.sessionViewFor("sess-1")
	if v == nil || v.ID != "sess-1" {
		t.Fatalf("sessionViewFor with a nil store = %+v, want a view identifying sess-1", v)
	}
}

// TestTransparentPath_RecordsUnderTheIdentityThePluginSaw extends the
// one-identity rule to the third path. An opaque redirected connection has no
// client-asserted id — but it does have an identity: ActiveSession() at the
// moment the connection was gated. Resolving it once to hydrate and then
// re-resolving to record reintroduces the flip window the other two paths were
// just fixed for, reduced to ActiveSession@T0 versus ActiveSession@T1.
//
// A non-HTTP/TLS dst port with no bridge configured keeps shouldSniff false, so
// the handler never reads from the conn and the rejection returns immediately.
func TestTransparentPath_RecordsUnderTheIdentityThePluginSaw(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	store.Append("conv-A", pipeline.SessionEvent{
		At:        time.Now(),
		Direction: pipeline.Inbound,
		Phase:     pipeline.SessionRequest,
		A2A:       &pipeline.A2AExtension{Method: "message/send", SessionID: "conv-A"},
	})

	p, err := pipeline.New([]pipeline.Plugin{&flipAndRejectPlugin{store: store, flipTo: session.DefaultSessionID}})
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Sessions:         store,
		Client:           http.DefaultClient,
	}

	clientSide, serverSide := net.Pipe()
	defer func() { _ = clientSide.Close() }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.HandleTransparentConn(serverSide, "10.0.0.1:9999")
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("HandleTransparentConn did not return; it should reject before dialing")
	}

	countDenied := func(id string) int {
		v := store.View(id)
		if v == nil {
			return 0
		}
		n := 0
		for _, e := range v.Events {
			if e.Phase == pipeline.SessionDenied {
				n++
			}
		}
		return n
	}
	if got := countDenied("conv-A"); got != 1 {
		t.Errorf("denial recorded in conv-A: got %d, want 1", got)
	}
	if got := countDenied(session.DefaultSessionID); got != 0 {
		t.Errorf("denial leaked into the session that flipped ActiveSession mid-pipeline: got %d, want 0", got)
	}
}

// TestTransparentPath_HydratesTheIdentityItGatesOn drives the transparent path
// end to end and asserts the plugin gating the tunnel is told which session the
// connection belongs to. Characterization for the hydration half — View(aid)
// already produced this — but it is the only test that exercises
// HandleTransparentConn's session wiring, and it pins the identity the denial
// path now carries forward.
//
// The probe is paired with a rejecting plugin so the handler returns at the
// reject branch instead of dialing an unreachable address. That makes the test
// deterministic AND race-free: closing done happens-after the pipeline ran, so
// reading the probe's observations needs no polling and no lock.
func TestTransparentPath_HydratesTheIdentityItGatesOn(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	store.Append("conv-A", pipeline.SessionEvent{
		At:        time.Now(),
		Direction: pipeline.Inbound,
		Phase:     pipeline.SessionRequest,
		A2A:       &pipeline.A2AExtension{Method: "message/send", SessionID: "conv-A"},
	})

	probe := &identityProbePlugin{}
	p, err := pipeline.New([]pipeline.Plugin{probe, &denyingPlugin{}})
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Sessions:         store,
		Client:           http.DefaultClient,
	}

	clientSide, serverSide := net.Pipe()
	defer func() { _ = clientSide.Close() }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.HandleTransparentConn(serverSide, "10.0.0.1:9999")
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("HandleTransparentConn did not return; it should reject before dialing")
	}

	if len(probe.sawID) != 1 {
		t.Fatalf("probe ran %d times on the transparent path, want 1", len(probe.sawID))
	}
	if probe.sawID[0] != "conv-A" {
		t.Errorf("plugin saw %q, want conv-A", probe.sawID[0])
	}
}

// TestSessionViewFor_IsTheOnlyHydrationPath guards the invariant behind the
// previous test: every listener path must hydrate through sessionViewFor, which
// is the single place that guarantees a usable view. A raw Sessions.View() call
// can hand plugins nil when a bucket expires between ActiveSession() and View()
// — the two take separate locks — which is the gap this consolidates away.
func TestSessionViewFor_IsTheOnlyHydrationPath(t *testing.T) {
	srcs, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range srcs {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, "pctx.Session = ") && !strings.Contains(line, "sessionViewFor") {
				t.Errorf("%s:%d hydrates pctx.Session without sessionViewFor: %s",
					f, i+1, strings.TrimSpace(line))
			}
		}
	}
}
