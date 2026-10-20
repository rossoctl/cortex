package contextguru

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	cgconfig "github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dispatch"
	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/cortex/core/pipeline"
)

func finishedPctx(status int, role pipeline.AgentRole) *pipeline.Context {
	p := inferencePctx("/v1/messages", []byte(`{"model":"claude-sonnet-5","messages":[]}`))
	p.StartedAt = time.Unix(1_700_000_000, 0)
	p.StatusCode = status
	p.Headers = http.Header{"Authorization": {"Bearer caller"}, "Anthropic-Version": {"2023-06-01"}}
	p.Extensions.Inference = &pipeline.InferenceExtension{Model: "claude-sonnet-5",
		InputTokens: 3, CacheReadTokens: 48_000, OutputTokens: 120,
		PresentKinds: 1 | 2 | 4 | 8, AgentRole: role}
	return p
}

// heldCount reads the dispatcher's holds under its lock.
func heldCount(p *ContextGuru) int {
	p.disp.mu.Lock()
	defer p.disp.mu.Unlock()
	return len(p.disp.held)
}

// keepAlivePlugin is a configured, started plugin with keep-alive on and the prefix floor out of
// the way; Shutdown runs at cleanup.
func keepAlivePlugin(t *testing.T) *ContextGuru {
	t.Helper()
	p := configured(t, `{"keep_alive": {"enabled": true, "min_prefix_tokens": 1}}`)
	if err := p.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Shutdown(t.Context()) })
	return p
}

// finishTurns drives n finished main-thread requests through OnRequest and OnFinish.
func finishTurns(t *testing.T, p *ContextGuru, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		pctx := finishedPctx(200, pipeline.AgentRoleMain)
		invokeReq(p, pctx)
		p.OnFinish(t.Context(), pctx)
	}
}

func TestRecordForMapsAFinishedRequest(t *testing.T) {
	pol := keepAliveConfig{Enabled: true}.policy()
	r, ok := recordFor(finishedPctx(200, pipeline.AgentRoleMain), "s1", pol, func() dispatch.Ref { return "" }, logEmitter{})
	if !ok {
		t.Fatal("a finished main-thread request was not recorded")
	}
	if r.Key.Session != "s1" || r.Key.Thread != "" || r.Status != 200 || r.Provider != bschemas.Anthropic {
		t.Fatalf("record = %+v", r)
	}
	if r.Usage.CacheRead != 48_000 || r.Usage.FreshInput != 3 || r.Usage.Output != 120 || !r.UsageOK {
		t.Fatalf("usage = %+v ok=%v", r.Usage, r.UsageOK)
	}
	if r.Route != "/v1/messages" || r.Endpoint != "ollama" || r.Hold == nil {
		t.Fatalf("route/endpoint/hold = %q %q %v", r.Route, r.Endpoint, r.Hold != nil)
	}
	if !r.StartedAt.Equal(time.Unix(1_700_000_000, 0)) || len(r.Body) == 0 || r.Emitter == nil {
		t.Fatalf("startedAt/body/emitter = %v %d %v", r.StartedAt, len(r.Body), r.Emitter)
	}
}

func TestRecordForUsageNotOKWithoutCacheTiers(t *testing.T) {
	pctx := finishedPctx(200, pipeline.AgentRoleMain)
	pctx.Extensions.Inference.PresentKinds = 1 | 8 // input + output only
	r, ok := recordFor(pctx, "s1", keepAliveConfig{Enabled: true}.policy(), nil, logEmitter{})
	if !ok || r.UsageOK {
		t.Fatalf("ok=%v usageOK=%v; want recorded with UsageOK false", ok, r.UsageOK)
	}
}

func TestRecordForSkipsSubagentsAndMissingInputs(t *testing.T) {
	pol := keepAliveConfig{Enabled: true}.policy()
	if _, ok := recordFor(finishedPctx(200, pipeline.AgentRoleSubagent), "s1", pol, nil, logEmitter{}); ok {
		t.Error("a subagent request was recorded under the main thread's key")
	}
	if _, ok := recordFor(finishedPctx(200, pipeline.AgentRoleMain), "", pol, nil, logEmitter{}); ok {
		t.Error("a request with no session was recorded")
	}
	p := finishedPctx(200, pipeline.AgentRoleMain)
	p.Extensions.Inference = nil
	if _, ok := recordFor(p, "s1", pol, nil, logEmitter{}); ok {
		t.Error("a request with no inference parse was recorded")
	}
}

func TestPolicyDefaults(t *testing.T) {
	p := keepAliveConfig{Enabled: true}.policy()
	if !p.On() || p.Idle != time.Duration(cgconfig.DefaultKeepAliveIdle)*time.Second ||
		p.MaxPings != cgconfig.DefaultKeepAliveMaxPings ||
		p.MaxUSDPerPing != cgconfig.DefaultKeepAliveMaxUSDPerPing ||
		p.MinPrefixTokens != cgconfig.DefaultKeepAliveMinPrefix {
		t.Fatalf("policy = %+v", p)
	}
	if (keepAliveConfig{}).policy().On() {
		t.Fatal("keep-alive on with enabled: false")
	}
	o := keepAliveConfig{Enabled: true, IdleSeconds: 60, MaxPings: 5, MaxUSDPerPing: 0.1,
		MinPrefixTokens: 1000}.policy()
	if o.Idle != time.Minute || o.MaxPings != 5 || o.MaxUSDPerPing != 0.1 || o.MinPrefixTokens != 1000 {
		t.Fatalf("overrides not applied: %+v", o)
	}
}

func TestKeepAliveOffByDefault(t *testing.T) {
	p := configured(t, `{}`)
	if p.keeper != nil || p.disp != nil {
		t.Fatal("a keeper was built with no keep_alive block")
	}
	// The lifecycle hooks are no-ops with keep-alive off.
	if err := p.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	pctx := finishedPctx(200, pipeline.AgentRoleMain)
	invokeReq(p, pctx)
	p.OnFinish(t.Context(), pctx)
	if err := p.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestOnFinishHoldsTheCredentialOnlyForATrackedRequest(t *testing.T) {
	p := keepAlivePlugin(t)
	// Turn 0 is counted but never tracked (the engine's first-request gate).
	finishTurns(t, p, 2)
	if n := p.keeper.Stats().Live; n != 1 {
		t.Fatalf("live entries = %d, want 1", n)
	}
	if n := heldCount(p); n != 1 {
		t.Fatalf("held credentials = %d, want 1", n)
	}
	// A denied request on the same session retires nothing new and holds nothing.
	bad := finishedPctx(401, pipeline.AgentRoleMain)
	invokeReq(p, bad) // Arrive clears the tracked entry
	p.OnFinish(t.Context(), bad)
	if n := heldCount(p); n != 0 {
		t.Fatalf("held credentials after a 401 = %d, want 0", n)
	}
}

func TestSubagentRequestDoesNotClearTheMainThread(t *testing.T) {
	p := keepAlivePlugin(t)
	finishTurns(t, p, 2)
	sub := finishedPctx(200, pipeline.AgentRoleSubagent)
	invokeReq(p, sub)
	p.OnFinish(t.Context(), sub)
	if n := p.keeper.Stats().Live; n != 1 {
		t.Fatalf("live entries after a subagent request = %d, want the main thread's 1", n)
	}
	if n := heldCount(p); n != 1 {
		t.Fatalf("held credentials = %d, want 1", n)
	}
}

func TestSentinelRequestIsNeverRecorded(t *testing.T) {
	p := keepAlivePlugin(t)
	for i := 0; i < 2; i++ {
		pctx := finishedPctx(200, pipeline.AgentRoleMain)
		pctx.Headers.Set(sentinelHeader, "1")
		invokeReq(p, pctx)
		p.OnFinish(t.Context(), pctx)
	}
	if n := p.keeper.Stats().Live; n != 0 {
		t.Fatalf("a sentinel request was tracked: %d live", n)
	}
}

func TestShutdownReleasesHeldCredentials(t *testing.T) {
	p := configured(t, `{"keep_alive": {"enabled": true, "min_prefix_tokens": 1}}`)
	if err := p.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	finishTurns(t, p, 2)
	if n := heldCount(p); n != 1 {
		t.Fatalf("held credentials before Shutdown = %d, want 1", n)
	}
	if err := p.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := heldCount(p); n != 0 {
		t.Fatalf("held credentials after Shutdown = %d, want 0", n)
	}
}

func TestConfigSchemaListsKeepAlive(t *testing.T) {
	for _, f := range New().ConfigSchema() {
		if f.Name == "keep_alive" {
			return
		}
	}
	t.Fatal("ConfigSchema has no keep_alive entry")
}

// TestASkippedSubagentRequestDoesNotPauseTheMainThread covers a request that looks like ordinary
// traffic when OnRequest runs — Extensions.Inference is still nil, as it would be if this plugin
// runs before inference-parser's own OnRequest in the chain — so isSubagent is false there and
// Arrive opens an arrival on the session's (threadless) key. By OnFinish, inference-parser has
// since run and the request turns out to be a subagent's, which recordFor skips: no Record, and
// before this fix, no Depart either. That leaked arrival pins the main thread's OWN entry
// in-flight (same Key{Session: sid}, no Thread — #423's per-thread keying collapses to one key
// when nothing ever sets Thread), so the keeper's sweep would never ping it again.
func TestASkippedSubagentRequestDoesNotPauseTheMainThread(t *testing.T) {
	p := keepAlivePlugin(t)

	sub := inferencePctx("/v1/messages", []byte(`{"model":"claude-sonnet-5","messages":[]}`))
	sub.StartedAt = time.Unix(1_700_000_000, 0)
	sub.StatusCode = 200
	sub.Headers = http.Header{"Authorization": {"Bearer caller"}, "Anthropic-Version": {"2023-06-01"}}
	invokeReq(p, sub) // Extensions.Inference is nil: isSubagent is false, so Arrive fires here.
	sub.Extensions.Inference = &pipeline.InferenceExtension{Model: "claude-sonnet-5",
		InputTokens: 3, CacheReadTokens: 48_000, OutputTokens: 120,
		PresentKinds: 1 | 2 | 4 | 8, AgentRole: pipeline.AgentRoleSubagent}
	p.OnFinish(t.Context(), sub) // recordFor now sees the subagent role and skips.

	// The main thread's own ordinary turns, after the subagent request. Turn 0 is counted but
	// never tracked (the engine's first-request gate); turn 1 establishes the live entry.
	finishTurns(t, p, 2)
	if n := p.keeper.Stats().Live; n != 1 {
		t.Fatalf("live entries after the main thread's turns = %d, want 1", n)
	}

	if n := p.keeper.Sweep(time.Unix(1_700_000_000, 0).Add(281 * time.Second)); n != 1 {
		t.Fatalf("sweep sent %d pings, want 1 — the skipped subagent request's undeparted "+
			"arrival pinned the main thread's entry in-flight", n)
	}
}

// TestARequestWithNoInferenceParseDoesNotPauseTheMainThread is
// TestASkippedSubagentRequestDoesNotPauseTheMainThread's sibling for recordFor's other skip:
// Extensions.Inference stays nil all the way to OnFinish (no parser ever populated it, e.g. a
// gated path that isn't valid inference traffic). Arrive still opens on it in OnRequest, and
// without a Depart on this path too the leaked arrival would pin the main thread's entry the
// same way.
func TestARequestWithNoInferenceParseDoesNotPauseTheMainThread(t *testing.T) {
	p := keepAlivePlugin(t)

	noParse := inferencePctx("/v1/messages", []byte(`{"model":"claude-sonnet-5","messages":[]}`))
	noParse.StartedAt = time.Unix(1_700_000_000, 0)
	noParse.StatusCode = 200
	noParse.Headers = http.Header{"Authorization": {"Bearer caller"}, "Anthropic-Version": {"2023-06-01"}}
	invokeReq(p, noParse)            // Arrive fires: no Extensions.Inference, so isSubagent is false.
	p.OnFinish(t.Context(), noParse) // recordFor skips: still no Extensions.Inference.

	finishTurns(t, p, 2)
	if n := p.keeper.Stats().Live; n != 1 {
		t.Fatalf("live entries after the main thread's turns = %d, want 1", n)
	}

	if n := p.keeper.Sweep(time.Unix(1_700_000_000, 0).Add(281 * time.Second)); n != 1 {
		t.Fatalf("sweep sent %d pings, want 1 — the no-inference-parse request's undeparted "+
			"arrival pinned the main thread's entry in-flight", n)
	}
}

func TestRequestFinishingAfterShutdownHoldsNothing(t *testing.T) {
	p := configured(t, `{"keep_alive": {"enabled": true, "min_prefix_tokens": 1}}`)
	if err := p.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	finishTurns(t, p, 2)
	if n := heldCount(p); n != 1 {
		t.Fatalf("held credentials before Shutdown = %d, want 1", n)
	}
	if err := p.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Requests still draining on the old pipeline after a hot-reload. Two of them, because
	// Keeper.Shutdown resets the turn counter and a lone turn 0 is never tracked anyway.
	finishTurns(t, p, 2)
	if n := heldCount(p); n != 0 {
		t.Fatalf("held credentials after a post-Shutdown finish = %d, want 0", n)
	}
	if n := p.keeper.Stats().Live; n != 0 {
		t.Fatalf("live entries after a post-Shutdown finish = %d, want 0", n)
	}
}

// credID is an opaque per-process id for the caller's credential (Record.Cred): distinct keys get
// distinct, non-empty ids; the same key the same id; no auth header "". It never carries the key.
func TestCredIDIsPerCredential(t *testing.T) {
	a1 := credID(http.Header{"Authorization": {"Bearer sk-caller-a"}})
	a2 := credID(http.Header{"Authorization": {"Bearer sk-caller-a"}, "Anthropic-Version": {"2023-06-01"}})
	b := credID(http.Header{"Authorization": {"Bearer sk-caller-b"}})
	x := credID(http.Header{"X-Api-Key": {"sk-caller-a"}})
	if a1 == "" || b == "" || x == "" {
		t.Fatalf("an auth header gave an empty id: a=%q b=%q x=%q", a1, b, x)
	}
	if a1 != a2 {
		t.Errorf("the same key gave two ids (%q, %q): a non-auth header must not count", a1, a2)
	}
	if a1 == b || a1 == x {
		t.Errorf("different credentials share an id: a=%q b=%q x-api-key=%q", a1, b, x)
	}
	if strings.Contains(a1, "sk-caller") || len(a1) != 32 {
		t.Errorf("id %q is not a 16-byte hex digest", a1)
	}
	if got := credID(http.Header{"Anthropic-Version": {"2023-06-01"}}); got != "" {
		t.Errorf("no auth header gave %q, want \"\"", got)
	}
	if got := credID(nil); got != "" {
		t.Errorf("nil headers gave %q, want \"\"", got)
	}
	r, _ := recordFor(finishedPctx(200, pipeline.AgentRoleMain), "s1", keepAliveConfig{Enabled: true}.policy(),
		func() dispatch.Ref { return "" }, logEmitter{})
	if r.Cred != credID(http.Header{"Authorization": {"Bearer caller"}}) {
		t.Errorf("recordFor set Cred %q, want the caller's credential id", r.Cred)
	}
}

// One caller's revoked key must not stop keep-alive for another caller of the same host, path and
// model. Cortex pings with the caller's own credential, so a 401 is that credential's fault: with
// Cred "" the keeper fed it to the route breaker, whose key every caller shares, and three of
// them switched the route off for everyone for an hour.
func TestOneCallersRefusedKeyDoesNotStopAnotherCallersPings(t *testing.T) {
	var mu sync.Mutex
	pingsB := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer sk-revoked-a" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		pingsB++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":1,"cache_read_input_tokens":48000,"output_tokens":1}}`))
	}))
	defer srv.Close()
	d := newCortexDispatcher(newPingClient())
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	k := keepalive.New(keepalive.Options{Dispatcher: d, Now: func() time.Time { return now },
		Go: func(f func()) { f() }})
	t.Cleanup(k.Shutdown)
	pol := keepalive.Policy{KeepAlive: true, Idle: 280 * time.Second, MaxPings: 2,
		MaxUSDPerPing: 0.25, MinPrefixTokens: 20000}
	host := stripScheme(srv.URL)
	// hold records two turns of one session (a session's first request is never held), through
	// recordFor, so Cred is computed exactly as OnFinish computes it.
	hold := func(sid, auth string) {
		for i := 0; i < 2; i++ {
			pctx := finishedPctx(200, pipeline.AgentRoleMain)
			pctx.Scheme, pctx.Host = "http", host
			pctx.StartedAt = now
			pctx.Headers = http.Header{"Authorization": {auth}}
			hdr := pctx.Headers
			r, ok := recordFor(pctx, sid, pol, func() dispatch.Ref { return d.hold("http", host, hdr) }, logEmitter{})
			if !ok {
				t.Fatal("setup: recordFor refused a main-thread request")
			}
			k.Record(r)
		}
	}
	for i := 0; i < 3; i++ { // three refusals on caller A's key, each its own session
		hold("a"+strconv.Itoa(i), "Bearer sk-revoked-a")
		now = now.Add(281 * time.Second)
		k.Sweep(now)
	}
	if s := k.Stats(); s.RouteTrips != 0 {
		t.Fatalf("caller A's refused key tripped the shared route (%d trips)", s.RouteTrips)
	}
	hold("b", "Bearer sk-caller-b")
	if _, ok := k.Entry(keepalive.Key{Session: "b"}); !ok {
		t.Fatal("caller B's session was refused after caller A's key was revoked")
	}
	now = now.Add(281 * time.Second)
	k.Sweep(now)
	mu.Lock()
	defer mu.Unlock()
	if pingsB != 1 {
		t.Errorf("caller B got %d pings, want 1", pingsB)
	}
}

// A panic in OnFinish before Record (here recordBilled, on a corrupt session-key entry) is
// recovered by the pipeline's dispatchFinish; the request's arrival must still be departed, or the
// thread reads as in flight until a restart and is never pinged again.
func TestAPanicInOnFinishStillDeparts(t *testing.T) {
	p := keepAlivePlugin(t)
	finishTurns(t, p, 1) // turn 0: counted, never held
	a := finishedPctx(200, pipeline.AgentRoleMain)
	invokeReq(p, a)      // request A arrives
	finishTurns(t, p, 1) // request B, overlapping, finishes and holds
	if n := p.keeper.Stats().Live; n != 1 {
		t.Fatalf("setup: %d live entries, want 1", n)
	}
	p.cgSessions.Store(sessionID(a), 42) // recordBilled's key.(string) panics on this
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("setup: OnFinish did not panic")
			}
		}()
		p.OnFinish(t.Context(), a)
	}()
	if n := p.keeper.Sweep(time.Unix(1_700_000_000, 0).Add(281 * time.Second)); n != 1 {
		t.Fatalf("sweep sent %d pings, want 1 — A's arrival leaked when its OnFinish panicked", n)
	}
}

// OnFinish departs exactly once on the path that reaches Record (Record departs for itself): a
// second Depart would release an OVERLAPPING request's count and let its thread be pinged while
// that request is still open.
func TestARecordedFinishDoesNotReleaseAnOverlappingRequest(t *testing.T) {
	p := keepAlivePlugin(t)
	finishTurns(t, p, 1) // turn 0: counted, never held
	a := finishedPctx(200, pipeline.AgentRoleMain)
	b := finishedPctx(200, pipeline.AgentRoleMain)
	invokeReq(p, a)
	invokeReq(p, b)
	p.OnFinish(t.Context(), a) // A finishes and holds; B is still open
	if n := p.keeper.Stats().Live; n != 1 {
		t.Fatalf("setup: %d live entries, want 1", n)
	}
	if n := p.keeper.Sweep(time.Unix(1_700_000_000, 0).Add(281 * time.Second)); n != 0 {
		t.Fatalf("sweep sent %d pings while B was still open, want 0 — A departed twice", n)
	}
}
