package sessionbudget

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/listener/forwardproxy"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

func newE2EPlugin(t *testing.T, maxTokens int64, store *memStore) *SessionBudget {
	t.Helper()
	p := New()
	cfg, _ := json.Marshal(config{
		RedisURL:         "mem://test",
		MaxTokens:        maxTokens,
		OnExceed:         "deny",
		RefreshInterval:  "30ms",
		RedisUnavailable: "fail_open",
	})
	if err := p.Configure(cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	p.store = store
	go p.refreshLoop(30 * time.Millisecond)
	t.Cleanup(func() { close(p.stopCh); <-p.stopped })
	return p
}

// newE2EPluginPause builds a plugin in on_exceed=pause mode so cold-cache
// hydrate runs on the OnRequest path (see plugin.go). The caller supplies a
// webhook URL — pass a deny-returning stub if you want breaches to reject.
func newE2EPluginPause(t *testing.T, maxTokens int64, store *memStore, webhookURL string) *SessionBudget {
	t.Helper()
	p := New()
	cfg, _ := json.Marshal(config{
		RedisURL:           "mem://test",
		MaxTokens:          maxTokens,
		OnExceed:           "pause",
		PauseWebhook:       webhookURL,
		PauseTimeout:       "2s",
		PauseTimeoutAction: "deny",
		PauseGracePeriod:   "0s",
		RefreshInterval:    "30ms",
		RedisUnavailable:   "fail_open",
	})
	if err := p.Configure(cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	p.store = store
	p.httpClient = &http.Client{Timeout: 0}
	go p.refreshLoop(30 * time.Millisecond)
	t.Cleanup(func() { close(p.stopCh); <-p.stopped })
	return p
}

func respond(p *SessionBudget, sessionID string, tokens int) {
	p.OnResponseFrame(context.Background(), makePctx(sessionID, tokens), nil, true)
}

func request(p *SessionBudget, sessionID string) pipeline.Action {
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Headers:   http.Header{},
		Session:   &pipeline.SessionView{ID: sessionID},
	}
	return p.OnRequest(context.Background(), pctx)
}

// TestE2E_HTTPRoundTrip wires session-budget into a real forward proxy.
// Under-budget requests reach the backend; the proxy is functional.
func TestE2E_HTTPRoundTrip(t *testing.T) {
	store := newMemStore()
	p := newE2EPlugin(t, 1000, store)

	pipe, err := pipeline.New([]pipeline.Plugin{p})
	if err != nil {
		t.Fatal(err)
	}
	sessions := session.New(5*time.Minute, 100, 0)
	defer sessions.Close()

	srv, err := forwardproxy.NewServer(pipeline.NewHolder(pipe), sessions, nil)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	proxyURL, _ := url.Parse(proxy.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	req, _ := http.NewRequest(http.MethodGet, backend.URL+"/v1/chat/completions", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request through proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
}

// TestE2E_AccumulateAndDeny verifies the full lifecycle: accumulate
// tokens via OnResponseFrame, then OnRequest denies with a 403.
func TestE2E_AccumulateAndDeny(t *testing.T) {
	p := newE2EPlugin(t, 150, newMemStore())

	for i := 0; i < 3; i++ {
		respond(p, "sess", 60)
	}

	action := request(p, "sess")
	if action.Type != pipeline.Reject {
		t.Fatalf("expected Reject, got %v", action.Type)
	}
	status, _, body := action.Violation.Render()
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want 403", status)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["error"] != "budget.exceeded" {
		t.Errorf("error = %v, want budget.exceeded", parsed["error"])
	}
}

// TestE2E_MultiSession verifies independent session budgets.
func TestE2E_MultiSession(t *testing.T) {
	p := newE2EPlugin(t, 100, newMemStore())

	for i := 0; i < 3; i++ {
		respond(p, "A", 40) // 120 > 100
	}
	respond(p, "B", 20) // 20 < 100

	if a := request(p, "A"); a.Type != pipeline.Reject {
		t.Fatalf("session A: expected Reject, got %v", a.Type)
	}
	if a := request(p, "B"); a.Type != pipeline.Continue {
		t.Fatalf("session B: expected Continue, got %v", a.Type)
	}
}

// TestE2E_LocalCacheEnforcesDuringOutage confirms that a populated
// cache enforces even when the backing store is unreachable.
func TestE2E_LocalCacheEnforcesDuringOutage(t *testing.T) {
	// Build without starting refreshLoop so we can swap store safely.
	p := New()
	cfg, _ := json.Marshal(config{
		RedisURL:         "mem://test",
		MaxTokens:        100,
		OnExceed:         "deny",
		RefreshInterval:  "30ms",
		RedisUnavailable: "fail_open",
	})
	if err := p.Configure(cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	p.store = &failingStore{}
	go p.refreshLoop(30 * time.Millisecond)
	t.Cleanup(func() { close(p.stopCh); <-p.stopped })

	p.mu.Lock()
	p.cache["s"] = &counters{tokens: 110, calls: 5, startedAt: time.Now()}
	p.mu.Unlock()

	if a := request(p, "s"); a.Type != pipeline.Reject {
		t.Fatalf("expected Reject from cache with store down, got %v", a.Type)
	}
}

// TestE2E_RefreshRecovery confirms that refreshCache picks up
// authoritative store values after an outage resolves.
func TestE2E_RefreshRecovery(t *testing.T) {
	inner := newMemStore()
	cs := &controllableStore{inner: inner}
	// Build without a background refreshLoop so refreshCache is invoked
	// deterministically and store swaps are race-free.
	p := New()
	cfg, _ := json.Marshal(config{
		RedisURL:         "mem://test",
		MaxTokens:        200,
		OnExceed:         "deny",
		RefreshInterval:  "30ms",
		RedisUnavailable: "fail_open",
	})
	if err := p.Configure(cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	p.store = cs

	ctx := context.Background()
	inner.HashIncr(ctx, "session-budget:s", "tokens", 180)
	inner.HashIncr(ctx, "session-budget:s", "calls", 7)
	inner.HashSetNX(ctx, "session-budget:s", "started_at", "1700000000")

	p.mu.Lock()
	p.cache["s"] = &counters{tokens: 50}
	p.mu.Unlock()

	cs.setFailing(true)
	p.refreshCache()
	p.mu.RLock()
	gotDuring := p.cache["s"].tokens
	p.mu.RUnlock()
	if gotDuring != 50 {
		t.Fatalf("during outage: tokens = %d, want 50", gotDuring)
	}

	cs.setFailing(false)
	p.refreshCache()
	p.mu.RLock()
	gotAfter := p.cache["s"].tokens
	p.mu.RUnlock()
	if gotAfter != 180 {
		t.Errorf("after recovery: tokens = %d, want 180", gotAfter)
	}
}

// TestE2E_PodRestart verifies that a fresh plugin with an empty cache
// hydrates from Redis on the first request in pause mode — no cold-cache
// overshoot for sessions already over-budget on Redis. Only pause mode
// hydrates on the request path (see plugin.go OnRequest); deny and observe
// intentionally skip on cold cache and let OnResponseFrame + the refresh
// loop populate counters, at the cost of a one-request-per-pod overshoot
// for pre-existing sessions.
func TestE2E_PodRestart(t *testing.T) {
	store := newMemStore()
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"action":"deny"}`))
	}))
	defer webhook.Close()
	p := newE2EPluginPause(t, 200, store, webhook.URL)

	ctx := context.Background()
	// Pre-seed Redis above the limit (210 > 200).
	store.HashIncr(ctx, "session-budget:s", "tokens", 210)
	store.HashIncr(ctx, "session-budget:s", "calls", 8)
	store.HashSetNX(ctx, "session-budget:s", "started_at", "1700000000")

	// Cold cache — first request hydrates from Redis, fires the webhook,
	// gets a deny, and rejects.
	if a := request(p, "s"); a.Type != pipeline.Reject {
		t.Fatalf("cold cache with over-budget Redis: expected Reject, got %v", a.Type)
	}
}

// TestE2E_HydrateSingleflight verifies concurrent cold-cache requests
// for the same session share one Redis lookup instead of stampeding.
// Uses pause mode because that's the only mode where OnRequest hydrates
// (deny/observe intentionally skip cold-cache to keep Redis off the hot path).
func TestE2E_HydrateSingleflight(t *testing.T) {
	inner := newMemStore()
	cs := &controllableStore{inner: inner, hashGetDelay: 50 * time.Millisecond}
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"action":"deny"}`))
	}))
	defer webhook.Close()
	// Build without starting refreshLoop so background HashGet calls don't
	// pollute the singleflight counter we're asserting on.
	p := New()
	cfg, _ := json.Marshal(config{
		RedisURL:           "mem://test",
		MaxTokens:          200,
		OnExceed:           "pause",
		PauseWebhook:       webhook.URL,
		PauseTimeout:       "2s",
		PauseTimeoutAction: "deny",
		PauseGracePeriod:   "0s",
		RefreshInterval:    "30ms",
		RedisUnavailable:   "fail_open",
	})
	if err := p.Configure(cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	p.store = cs
	p.httpClient = &http.Client{Timeout: 0}

	ctx := context.Background()
	inner.HashIncr(ctx, "session-budget:s", "tokens", 210)
	inner.HashIncr(ctx, "session-budget:s", "calls", 8)
	inner.HashSetNX(ctx, "session-budget:s", "started_at", "1700000000")

	// This test asserts singleflight dedup on hydrate — it counts HashGet
	// calls, not per-request outcomes. Pause-mode concurrent breaches
	// piggyback on pendingApproval (one leader gets Reject, others continue
	// via pause_pending_approval), so we don't assert per-request Reject.
	const N = 20
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_ = request(p, "s")
		}()
	}
	wg.Wait()

	// Without singleflight all N calls would hit HashGet. With it, only the
	// first flight does — later arrivals see the populated cache and skip hydrate.
	// Allow a small slack for goroutines that raced past the cache-check before
	// the first flight populated it.
	got := cs.hashGetCalls()
	if got > 3 {
		t.Errorf("HashGet called %d times for %d concurrent cold-cache requests; expected ≤3 (singleflight dedup)", got, N)
	}
}

// controllableStore delegates to inner memStore but can be toggled to fail,
// counts HashGet calls, and optionally injects latency into HashGet.
type controllableStore struct {
	inner        *memStore
	failing      bool
	hashGetDelay time.Duration
	mu           sync.Mutex
	hashGets     int
}

func (c *controllableStore) setFailing(v bool) { c.mu.Lock(); c.failing = v; c.mu.Unlock() }
func (c *controllableStore) isFailing() bool   { c.mu.Lock(); defer c.mu.Unlock(); return c.failing }
func (c *controllableStore) hashGetCalls() int { c.mu.Lock(); defer c.mu.Unlock(); return c.hashGets }
func (c *controllableStore) err() error        { return context.DeadlineExceeded }

func (c *controllableStore) Get(ctx context.Context, key string) (string, error) {
	if c.isFailing() {
		return "", c.err()
	}
	return c.inner.Get(ctx, key)
}
func (c *controllableStore) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	if c.isFailing() {
		return c.err()
	}
	return c.inner.Set(ctx, key, value, ttl)
}
func (c *controllableStore) Incr(ctx context.Context, key string, delta int64) (int64, error) {
	if c.isFailing() {
		return 0, c.err()
	}
	return c.inner.Incr(ctx, key, delta)
}
func (c *controllableStore) HashIncr(ctx context.Context, key, field string, delta int64) (int64, error) {
	if c.isFailing() {
		return 0, c.err()
	}
	return c.inner.HashIncr(ctx, key, field, delta)
}
func (c *controllableStore) HashGet(ctx context.Context, key string) (map[string]string, error) {
	c.mu.Lock()
	c.hashGets++
	delay := c.hashGetDelay
	failing := c.failing
	c.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if failing {
		return nil, c.err()
	}
	return c.inner.HashGet(ctx, key)
}
func (c *controllableStore) HashSetNX(ctx context.Context, key, field, value string) (bool, error) {
	if c.isFailing() {
		return false, c.err()
	}
	return c.inner.HashSetNX(ctx, key, field, value)
}
func (c *controllableStore) Expire(ctx context.Context, key string, ttl time.Duration) error {
	if c.isFailing() {
		return c.err()
	}
	return c.inner.Expire(ctx, key, ttl)
}
func (c *controllableStore) Close() error { return nil }

// TestE2E_PauseMode covers the full lifecycle for both webhook outcomes.
// The 'approve' row also verifies the request body carries session_id;
// the 'deny' row also verifies the 403 response schema.
func TestE2E_PauseMode(t *testing.T) {
	tests := []struct {
		name     string
		response string
		want     pipeline.ActionType
	}{
		{"approve", `{"action":"approve"}`, pipeline.Continue},
		{"deny", `{"action":"deny"}`, pipeline.Reject},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req map[string]any
				_ = json.NewDecoder(r.Body).Decode(&req)
				if req["session_id"] != "sess" {
					t.Errorf("webhook got session_id=%v, want sess", req["session_id"])
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tt.response))
			}))
			defer srv.Close()

			p := New()
			cfg, _ := json.Marshal(config{
				RedisURL:           "mem://test",
				MaxCalls:           3,
				OnExceed:           "pause",
				PauseWebhook:       srv.URL,
				PauseTimeout:       "5s",
				PauseTimeoutAction: "deny",
				RefreshInterval:    "30ms",
				RedisUnavailable:   "fail_open",
			})
			if err := p.Configure(cfg); err != nil {
				t.Fatalf("Configure: %v", err)
			}
			p.store = newMemStore()
			p.httpClient = &http.Client{}
			go p.refreshLoop(30 * time.Millisecond)
			t.Cleanup(func() { close(p.stopCh); <-p.stopped })

			p.mu.Lock()
			p.cache["sess"] = &counters{tokens: 100, calls: 3, startedAt: time.Now()}
			p.mu.Unlock()

			action := request(p, "sess")
			if action.Type != tt.want {
				t.Fatalf("action = %v, want %v", action.Type, tt.want)
			}
			if tt.want == pipeline.Reject {
				status, _, body := action.Violation.Render()
				if status != http.StatusForbidden {
					t.Errorf("status = %d, want 403", status)
				}
				var parsed map[string]any
				_ = json.Unmarshal(body, &parsed)
				if parsed["error"] != "budget.exceeded" {
					t.Errorf("error = %v, want budget.exceeded", parsed["error"])
				}
			}
		})
	}
}

// TestE2E_ConcurrentDenyOvershootBound pins the documented "overshoot by up to
// the in-flight count" bound. Call accounting is response-driven, so N
// concurrent OnRequest calls with the cache at (limit-1) all evaluate before
// any of them completes. In the worst case all N pass; enforcement resumes on
// the next request once responses have accumulated.
func TestE2E_ConcurrentDenyOvershootBound(t *testing.T) {
	const limit = 10
	const N = 8

	p := New()
	cfg, _ := json.Marshal(config{
		RedisURL:         "mem://test",
		MaxCalls:         limit,
		OnExceed:         "deny",
		RefreshInterval:  "30ms",
		RedisUnavailable: "fail_open",
	})
	if err := p.Configure(cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	p.store = newMemStore()
	go p.refreshLoop(30 * time.Millisecond)
	t.Cleanup(func() { close(p.stopCh); <-p.stopped })

	// Seed the cache one call below the limit. With no OnResponseFrame calls
	// interleaved into the burst, every concurrent OnRequest snapshots
	// calls=limit-1 (< limit) and MUST pass — this is deterministic under
	// the plugin's read-snapshot-then-evaluate design, no scheduling luck.
	p.mu.Lock()
	p.cache["sess"] = &counters{calls: limit - 1}
	p.mu.Unlock()

	var wg sync.WaitGroup
	results := make([]pipeline.ActionType, N)
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			results[i] = request(p, "sess").Type
		}(i)
	}
	wg.Wait()

	for i, r := range results {
		if r != pipeline.Continue {
			t.Fatalf("result[%d] = %v, want Continue (no response landed during burst, so every snapshot sees calls=%d < limit=%d)",
				i, r, limit-1, limit)
		}
	}
	// Simulate the responses that would have followed the in-flight requests.
	// Once accounting catches up, the next request must deny.
	for i := 0; i < N; i++ {
		respond(p, "sess", 0)
	}
	if a := request(p, "sess"); a.Type != pipeline.Reject {
		t.Fatalf("post-burst request: expected Reject (calls now %d > limit=%d), got %v",
			p.cache["sess"].calls, limit, a.Type)
	}
}
