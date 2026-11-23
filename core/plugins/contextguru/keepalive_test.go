package contextguru

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	cgconfig "github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dispatch"
	"github.com/rossoctl/context-guru/keepalive"
	cgstore "github.com/rossoctl/context-guru/store"
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

// Keep-alive is on by default on every install, local or not.
func TestKeepAliveOnByDefault(t *testing.T) {
	for _, local := range []bool{false, true} {
		for _, cfg := range []string{`{}`, `{"keep_alive": {}}`, `{"keep_alive": {"max_pings": 3}}`} {
			p := New()
			p.SetLocalInstall(local)
			if err := p.Configure(json.RawMessage(cfg)); err != nil {
				t.Fatal(err)
			}
			if p.keeper == nil || p.disp == nil || !p.kaPol.On() {
				t.Fatalf("local=%v %s: no keeper built, want keep-alive on by default", local, cfg)
			}
		}
	}
}

func TestKeepAliveCanBeTurnedOff(t *testing.T) {
	p := configured(t, `{"keep_alive": {"enabled": false}}`)
	if p.keeper != nil || p.disp != nil {
		t.Fatal("a keeper was built with keep_alive.enabled: false")
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
	// The dispatcher's pings carry the plugin's own per-process value, which is what OnRequest
	// and OnFinish compare against; a guessed value is ordinary traffic.
	if p.disp.sentinel == "" || p.disp.sentinel != p.sentinel {
		t.Fatalf("dispatcher sentinel %q, want the plugin's %q", p.disp.sentinel, p.sentinel)
	}
	for i := 0; i < 2; i++ {
		pctx := finishedPctx(200, pipeline.AgentRoleMain)
		pctx.Headers.Set(sentinelHeader, p.sentinel)
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
	d := newCortexDispatcher(newPingClient(true))
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

// panicStore is an engine store whose Put panics: recordBilled's write is the first thing
// OnFinish does before Record, so this stands in for any panic on that path.
type panicStore struct{ cgstore.Store }

func (panicStore) Put(string, []byte) { panic("panicStore.Put") }

// A panic in OnFinish before Record (here recordBilled, on a store that panics) is
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
	p.cgSessions.set(sessionID(a), "engine-key") // so recordBilled reaches the store
	p.store = panicStore{p.store}                // and its write panics
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

// oneHourPctx is a finished main-thread request that asks the 1-hour tier on its only breakpoint
// and was billed at it for everything it wrote.
func oneHourPctx() *pipeline.Context {
	p := finishedPctx(200, pipeline.AgentRoleMain)
	p.Body = []byte(`{"model":"claude-sonnet-5","system":[{"type":"text","text":"s",` +
		`"cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[]}`)
	p.Extensions.Inference.CacheWriteTokens = 90_000
	p.Extensions.Inference.CacheWrite1hTokens = 90_000
	return p
}

// The keeper decides a thread's tier from CacheWrite1h; with the field dropped, every thread
// billed at the 1-hour tier ran on the 5-minute schedule.
func TestRecordForCarriesTheOneHourCacheWrite(t *testing.T) {
	pctx := oneHourPctx()
	r, ok := recordFor(pctx, "s1", keepAliveConfig{Enabled: true}.policy(), nil, logEmitter{})
	if !ok {
		t.Fatal("a 1h-billed request was not recorded")
	}
	if r.Usage.CacheWrite != 90_000 || r.Usage.CacheWrite1h != 90_000 {
		t.Fatalf("usage = %+v, want CacheWrite and CacheWrite1h 90000", r.Usage)
	}
	if !keepalive.ResolveTier(r.Body, r.Usage, r.UsageOK, false) {
		t.Fatal("ResolveTier did not put a 1h-asked, 1h-billed thread on the 1-hour tier")
	}
}

// The credential-retention bound in docs/context-guru-plugin.md: a thread billed at the 1-hour
// tier holds for (MaxPings1h+1) × Idle1h, 118 minutes at the shipped defaults, never more than
// keepalive.MaxHold1h; a 5-minute thread holds for (max_pings+1) × idle_seconds, 12 minutes.
func TestKeepAliveHoldFollowsTheBilledTier(t *testing.T) {
	p := keepAlivePlugin(t)
	// StartedAt now, not finishedPctx's fixed past: the started sweep releases an entry whose hold
	// deadline has passed.
	turn := func(pctx *pipeline.Context) keepalive.EntryView {
		t.Helper()
		pctx.StartedAt = time.Now()
		invokeReq(p, pctx)
		p.OnFinish(t.Context(), pctx)
		e, _ := p.keeper.Entry(keepalive.Key{Session: sessionID(pctx)})
		return e
	}
	// Turn 0 is counted but never tracked (the engine's first-request gate).
	turn(finishedPctx(200, pipeline.AgentRoleMain))
	e := turn(finishedPctx(200, pipeline.AgentRoleMain))
	if e.TTL1h || e.Policy.Idle == 0 {
		t.Fatalf("5-minute thread: entry %+v", e)
	}
	if hold := time.Duration(e.Policy.MaxPings+1) * e.Policy.Idle; hold != 12*time.Minute {
		t.Fatalf("5-minute thread holds for %v, the docs say 12 minutes", hold)
	}

	e = turn(oneHourPctx())
	if !e.TTL1h {
		t.Fatalf("1-hour thread: entry %+v, want TTL1h", e)
	}
	hold := time.Duration(e.Policy.MaxPings+1) * e.Policy.Idle
	if hold != 118*time.Minute || hold > keepalive.MaxHold1h || keepalive.MaxHold1h != 236*time.Minute {
		t.Fatalf("1-hour thread holds for %v (MaxHold1h %v), the docs say 118 and at most 236 minutes",
			hold, keepalive.MaxHold1h)
	}
}

// destRouter redirects and, when translate is set, translates every request, as inference-router does.
type destRouter struct{ translate, upstreamHeader bool }

func (destRouter) Name() string { return "router" }
func (destRouter) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{WritesDestination: true, Description: "test"}
}
func (r destRouter) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	u, _ := url.Parse("https://openai.example")
	_ = pctx.Redirect(u)
	if r.translate {
		_ = pctx.Translate("anthropic", "openai", "/v1/chat/completions")
	}
	if r.upstreamHeader {
		_ = pctx.SetUpstreamHeaders(http.Header{"X-Target": {"1"}})
	}
	return pipeline.Action{Type: pipeline.Continue}
}
func (destRouter) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// A translated request, or one with router-set upstream headers, is not what pctx holds on the
// wire, so a ping cannot replay it: nothing is held for it.
func TestKeepAliveHoldsNothingForARequestTheRouterRewrote(t *testing.T) {
	for _, r := range []destRouter{{translate: true}, {upstreamHeader: true}} {
		p := keepAlivePlugin(t)
		pipe, err := pipeline.New([]pipeline.Plugin{r})
		if err != nil {
			t.Fatal(err)
		}
		pctx := finishedPctx(200, pipeline.AgentRoleMain)
		pctx.MarkRedirectable()
		pipe.Run(t.Context(), pctx)
		if _, _, _, tr := pctx.TranslateTarget(); tr != r.translate {
			t.Fatalf("%+v: the test router did not translate as asked", r)
		}
		invokeReq(p, pctx)
		p.OnFinish(t.Context(), pctx)
		if _, ok := p.keeper.Entry(keepalive.Key{Session: sessionID(pctx)}); ok || heldCount(p) != 0 {
			t.Fatalf("%+v: the keeper holds an entry (held creds %d) for a rewritten request", r, heldCount(p))
		}
	}
	// A plain redirect is replayable: pctx.Host and pctx.Path are where the bytes went.
	pipe, err := pipeline.New([]pipeline.Plugin{destRouter{}})
	if err != nil {
		t.Fatal(err)
	}
	pctx := finishedPctx(200, pipeline.AgentRoleMain)
	pctx.MarkRedirectable()
	pipe.Run(t.Context(), pctx)
	r, ok := recordFor(pctx, "s1", keepAliveConfig{Enabled: true}.policy(), nil, logEmitter{})
	if !pctx.Redirected() || !ok || r.Endpoint != "openai.example" {
		t.Fatalf("a redirected, untranslated request: redirected=%v recorded=%v endpoint=%q",
			pctx.Redirected(), ok, r.Endpoint)
	}
}
