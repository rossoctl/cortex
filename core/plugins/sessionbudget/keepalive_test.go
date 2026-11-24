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

// subscribe wires p to h as Init does, without Init's Redis connection.
func subscribe(t *testing.T, p *SessionBudget) *accounting.Host {
	t.Helper()
	h := accounting.New(accounting.Options{})
	t.Cleanup(func() { _ = h.Close() })
	p.SetAccounting(h)
	p.unsubPing = p.acct.AddPingListener(p.onPing)
	return h
}

func waitWrites(t *testing.T, p *SessionBudget, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.RLock()
		c := p.cache[id]
		done := c != nil && c.pendingWrites == 0
		p.mu.RUnlock()
		if done {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("accumulate did not finish")
}

// A keep-alive ping counts against its session like a response: its tokens by kind, and a call,
// locally and in Redis.
func TestKeepAlivePing_CountsTokensAndACall(t *testing.T) {
	p := newTestPlugin(1_000_000_000, 0, 0)
	h := subscribe(t, p)
	if _, ok := h.Ping("sess-1", nil, "api.anthropic.com", ping("sess-1"), accounting.PingInfo{Status: 200}); !ok {
		t.Fatal("ping refused")
	}
	waitWrites(t, p, "sess-1")

	p.mu.RLock()
	c := *p.cache["sess-1"]
	p.mu.RUnlock()
	if c.tokens != 1835 || c.inputTokens != 10 || c.cacheReadTokens != 1800 || c.cacheWriteTokens != 20 ||
		c.outputTokens != 5 || c.calls != 1 {
		t.Errorf("counters = %+v, want 1835 tokens (10/1800/20/5) and 1 call", c)
	}
	fields, _ := p.store.HashGet(context.Background(), p.redisKey("sess-1"))
	if fields["tokens"] != "1835" || fields["calls"] != "1" {
		t.Errorf("redis = %v, want tokens 1835, calls 1", fields)
	}
}

// The ping's tokens count toward the limit, so the session's next request is what stops.
func TestKeepAlivePing_NextRequestHitsTheLimit(t *testing.T) {
	p := newTestPlugin(1000, 0, 0)
	h := subscribe(t, p)
	if _, ok := h.Ping("sess-1", nil, "api.anthropic.com", ping("sess-1"), accounting.PingInfo{Status: 200}); !ok {
		t.Fatal("ping refused")
	}
	waitWrites(t, p, "sess-1")
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
