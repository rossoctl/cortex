package sessionbudget

import (
	"context"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/accounting"
	"github.com/rossoctl/cortex/core/pipeline"
)

func ping(session string) accountant.Ping {
	t0 := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	return accountant.Ping{
		Session: session, Model: "claude-sonnet-4-5", Host: "api.anthropic.com",
		Usage:  accountant.CallUsage{Usage: accountant.Usage{Input: 10, CacheRead: 1800, CacheWrite: 20}, Output: 5},
		At:     t0.Add(4 * time.Minute),
		Window: t0,
	}
}

// subscribe wires p to a new host as Init does, without Init's Redis connection.
func subscribe(t *testing.T, p *SessionBudget) *accounting.Host {
	t.Helper()
	h := accounting.New(accounting.Options{})
	t.Cleanup(func() { _ = h.Close() })
	subscribeTo(h, p)
	return h
}

func subscribeTo(h *accounting.Host, p *SessionBudget) {
	p.SetAccounting(h)
	p.unsubPing = p.acct.AddKeyedPingListener(p.pingKey(), p.onPing)
}

func calls(p *SessionBudget, id string) int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if c := p.cache[id]; c != nil {
		return c.calls
	}
	return 0
}

// A reload starts the new instance before it stops the old one, which can drain for minutes, and
// both write the same Redis counters. Only the new instance counts a ping meanwhile; if the reload
// fails and the new instance is shut down, the old one counts again.
func TestKeepAlivePing_AReloadCountsAPingOnce(t *testing.T) {
	h := accounting.New(accounting.Options{})
	t.Cleanup(func() { _ = h.Close() })
	old, next := newTestPlugin(1_000_000_000, 0, 0), newTestPlugin(1_000_000_000, 0, 0)
	subscribeTo(h, old)
	subscribeTo(h, next)
	if _, ok := h.Ping("sess-1", nil, "api.anthropic.com", ping("sess-1"), accounting.PingInfo{Status: 200}); !ok {
		t.Fatal("ping refused")
	}
	if o, n := calls(old, "sess-1"), calls(next, "sess-1"); o != 0 || n != 1 {
		t.Errorf("during the drain: old counted %d, new %d; want 0 and 1", o, n)
	}
	close(next.stopped) // no refresh loop was started
	if err := next.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.Ping("sess-1", nil, "api.anthropic.com", ping("sess-1"), accounting.PingInfo{Status: 200}); !ok {
		t.Fatal("ping refused")
	}
	if o, n := calls(old, "sess-1"), calls(next, "sess-1"); o != 1 || n != 1 {
		t.Errorf("after the new instance stopped: old counted %d, new %d; want 1 and 1", o, n)
	}
}

// A keep-alive ping counts against its session like a response: its tokens by kind, and a call.
// The local counters move before Ping returns (count updates them before it starts the Redis
// write), so no wait is needed. The same delta goes to accumulate, whose Redis write
// TestAccumulate_WritesToStore covers.
func TestKeepAlivePing_CountsTokensAndACall(t *testing.T) {
	p := newTestPlugin(1_000_000_000, 0, 0)
	h := subscribe(t, p)
	if _, ok := h.Ping("sess-1", nil, "api.anthropic.com", ping("sess-1"), accounting.PingInfo{Status: 200}); !ok {
		t.Fatal("ping refused")
	}
	p.mu.RLock()
	c := *p.cache["sess-1"]
	p.mu.RUnlock()
	if c.tokens != 1835 || c.inputTokens != 10 || c.cacheReadTokens != 1800 || c.cacheWriteTokens != 20 ||
		c.outputTokens != 5 || c.calls != 1 {
		t.Errorf("counters = %+v, want 1835 tokens (10/1800/20/5) and 1 call", c)
	}
}

// The ping's tokens count toward the limit, so the session's next request is what stops.
func TestKeepAlivePing_NextRequestHitsTheLimit(t *testing.T) {
	p := newTestPlugin(1000, 0, 0)
	h := subscribe(t, p)
	if _, ok := h.Ping("sess-1", nil, "api.anthropic.com", ping("sess-1"), accounting.PingInfo{Status: 200}); !ok {
		t.Fatal("ping refused")
	}
	if a := p.OnRequest(context.Background(), makePctx("sess-1", 0)); a.Type == pipeline.Continue {
		t.Error("a request over the token limit after a ping was let through")
	}
}

// Shutdown unsubscribes: the instance a reload replaces counts no more pings.
func TestKeepAlivePing_ShutdownUnsubscribes(t *testing.T) {
	p := newTestPlugin(1_000_000_000, 0, 0)
	h := subscribe(t, p)
	close(p.stopped) // no refresh loop was started
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.Ping("sess-1", nil, "api.anthropic.com", ping("sess-1"), accounting.PingInfo{Status: 200})
	p.mu.RLock()
	_, ok := p.cache["sess-1"]
	p.mu.RUnlock()
	if ok {
		t.Error("a ping after Shutdown was counted")
	}
}

// A ping with no session is not counted (and the default bucket is not used for it).
func TestKeepAlivePing_NoSession(t *testing.T) {
	p := newTestPluginWithFallback(1_000_000_000, 0, 0, true)
	p.onPing(accounting.PingSpend{Usage: ping("").Usage})
	p.mu.RLock()
	n := len(p.cache)
	p.mu.RUnlock()
	if n != 0 {
		t.Errorf("%d sessions counted, want 0", n)
	}
}
