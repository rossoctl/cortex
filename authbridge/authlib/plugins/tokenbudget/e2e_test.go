package tokenbudget

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

	"github.com/rossoctl/cortex/authbridge/authlib/listener/forwardproxy"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
)

func newE2EPlugin(t *testing.T, maxTokens int64, store *memStore) *TokenBudget {
	t.Helper()
	p := New()
	cfg, _ := json.Marshal(config{
		RedisURL:        "mem://test",
		MaxTokens:       maxTokens,
		RefreshInterval: "30ms",
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

func respond(p *TokenBudget, sessionID string, tokens int) {
	p.OnResponseFrame(context.Background(), makePctx(sessionID, tokens), nil, true)
}

func request(p *TokenBudget, sessionID string) pipeline.Action {
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Headers:   http.Header{},
		Session:   &pipeline.SessionView{ID: sessionID},
	}
	return p.OnRequest(context.Background(), pctx)
}

// TestE2E_HTTPRoundTrip wires token-budget into a real forward proxy.
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
	p := newE2EPlugin(t, 100, newMemStore())
	p.store = &failingStore{}

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
	p := newE2EPlugin(t, 200, newMemStore())
	p.store = cs

	ctx := context.Background()
	inner.HashIncr(ctx, "token-budget:s", "tokens", 180)
	inner.HashIncr(ctx, "token-budget:s", "calls", 7)
	inner.HashSetNX(ctx, "token-budget:s", "started_at", "1700000000")

	p.mu.Lock()
	p.cache["s"] = &counters{tokens: 50}
	p.mu.Unlock()

	cs.setFailing(true)
	p.refreshCache()
	p.mu.RLock()
	if p.cache["s"].tokens != 50 {
		t.Fatalf("during outage: tokens = %d, want 50", p.cache["s"].tokens)
	}
	p.mu.RUnlock()

	cs.setFailing(false)
	p.refreshCache()
	p.mu.RLock()
	if p.cache["s"].tokens != 180 {
		t.Errorf("after recovery: tokens = %d, want 180", p.cache["s"].tokens)
	}
	p.mu.RUnlock()
}

// TestE2E_PodRestart verifies that a fresh plugin with an empty cache
// resumes enforcement after refresh picks up pre-existing store counters.
func TestE2E_PodRestart(t *testing.T) {
	store := newMemStore()
	p := newE2EPlugin(t, 200, store)

	ctx := context.Background()
	store.HashIncr(ctx, "token-budget:s", "tokens", 190)
	store.HashIncr(ctx, "token-budget:s", "calls", 8)
	store.HashSetNX(ctx, "token-budget:s", "started_at", "1700000000")

	// Cold cache — first request passes (overshoot window).
	if a := request(p, "s"); a.Type != pipeline.Continue {
		t.Fatalf("cold cache: expected Continue, got %v", a.Type)
	}

	// Seed cache entry so refresh picks up this session.
	respond(p, "s", 15)

	// Wait for refresh (30ms interval, 80ms is ~2.6x margin).
	time.Sleep(80 * time.Millisecond)

	if a := request(p, "s"); a.Type != pipeline.Reject {
		t.Fatalf("after refresh: expected Reject, got %v", a.Type)
	}
}

// controllableStore delegates to inner memStore but can be toggled to fail.
type controllableStore struct {
	inner   *memStore
	failing bool
	mu      sync.Mutex
}

func (c *controllableStore) setFailing(v bool) { c.mu.Lock(); c.failing = v; c.mu.Unlock() }
func (c *controllableStore) isFailing() bool   { c.mu.Lock(); defer c.mu.Unlock(); return c.failing }
func (c *controllableStore) err() error         { return context.DeadlineExceeded }

func (c *controllableStore) Get(ctx context.Context, key string) (string, error) {
	if c.isFailing() { return "", c.err() }
	return c.inner.Get(ctx, key)
}
func (c *controllableStore) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	if c.isFailing() { return c.err() }
	return c.inner.Set(ctx, key, value, ttl)
}
func (c *controllableStore) Incr(ctx context.Context, key string, delta int64) (int64, error) {
	if c.isFailing() { return 0, c.err() }
	return c.inner.Incr(ctx, key, delta)
}
func (c *controllableStore) HashIncr(ctx context.Context, key, field string, delta int64) (int64, error) {
	if c.isFailing() { return 0, c.err() }
	return c.inner.HashIncr(ctx, key, field, delta)
}
func (c *controllableStore) HashGet(ctx context.Context, key string) (map[string]string, error) {
	if c.isFailing() { return nil, c.err() }
	return c.inner.HashGet(ctx, key)
}
func (c *controllableStore) HashSetNX(ctx context.Context, key, field, value string) (bool, error) {
	if c.isFailing() { return false, c.err() }
	return c.inner.HashSetNX(ctx, key, field, value)
}
func (c *controllableStore) Expire(ctx context.Context, key string, ttl time.Duration) error {
	if c.isFailing() { return c.err() }
	return c.inner.Expire(ctx, key, ttl)
}
func (c *controllableStore) Close() error { return nil }

// TestE2E_ShadowMode verifies that on_exceed=observe allows requests
// through even when budget is exceeded, while still accumulating.
func TestE2E_ShadowMode(t *testing.T) {
	p := New()
	cfg, _ := json.Marshal(config{
		RedisURL:        "mem://test",
		MaxTokens:       150,
		OnExceed:        "observe",
		RefreshInterval: "30ms",
		RedisUnavailable: "fail_open",
	})
	if err := p.Configure(cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	store := newMemStore()
	p.store = store
	go p.refreshLoop(30 * time.Millisecond)
	t.Cleanup(func() { close(p.stopCh); <-p.stopped })

	for i := 0; i < 3; i++ {
		respond(p, "sess", 60) // 180 total > 150 limit
	}

	// In observe mode, request should continue (not reject).
	action := request(p, "sess")
	if action.Type != pipeline.Continue {
		t.Fatalf("shadow mode: expected Continue, got %v", action.Type)
	}

	// Counters should still accumulate past the limit.
	respond(p, "sess", 20) // 200 total
	p.mu.RLock()
	c := p.cache["sess"]
	p.mu.RUnlock()
	if c.tokens != 200 {
		t.Errorf("tokens = %d, want 200 (accumulation continues in shadow mode)", c.tokens)
	}

	// Subsequent requests also continue.
	action = request(p, "sess")
	if action.Type != pipeline.Continue {
		t.Fatalf("shadow mode (2nd request): expected Continue, got %v", action.Type)
	}
}
