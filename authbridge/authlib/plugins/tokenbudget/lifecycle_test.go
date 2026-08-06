package tokenbudget

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

func TestFullLifecycle_RefreshFromRedis(t *testing.T) {
	store := newMemStore()
	p := New()
	cfg := `{"redis_url":"mem://test","max_tokens":500,"refresh_interval":"50ms","session_ttl_seconds":3600}`
	if err := p.Configure(json.RawMessage(cfg)); err != nil {
		t.Fatal(err)
	}
	p.store = store

	ctx := context.Background()
	store.HashIncr(ctx, "token-budget:remote-sess", "tokens", 450)
	store.HashIncr(ctx, "token-budget:remote-sess", "calls", 10)
	store.HashSetNX(ctx, "token-budget:remote-sess", "started_at", "1700000000")

	// Seed cache so refreshCache picks it up.
	p.mu.Lock()
	p.cache["remote-sess"] = &counters{}
	p.mu.Unlock()

	p.refreshCache()

	p.mu.RLock()
	c := p.cache["remote-sess"]
	p.mu.RUnlock()
	if c == nil {
		t.Fatal("expected cache entry after refresh")
	}
	if c.tokens != 450 {
		t.Errorf("tokens = %d, want 450", c.tokens)
	}
	if c.calls != 10 {
		t.Errorf("calls = %d, want 10", c.calls)
	}
	if c.startedAt.Unix() != 1700000000 {
		t.Errorf("startedAt = %v, want 1700000000", c.startedAt.Unix())
	}
}

func TestFullLifecycle_DurationEnforcement(t *testing.T) {
	p := newTestPlugin(0, 0, 1)
	p.store = newMemStore()

	ctx := context.Background()
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Headers:   http.Header{},
		Session:   &pipeline.SessionView{ID: "dur-sess"},
		Extensions: pipeline.Extensions{
			Inference: &pipeline.InferenceExtension{TotalTokens: 10},
		},
	}
	p.OnResponseFrame(ctx, pctx, nil, true)

	// Backdate startedAt past the 1s limit.
	p.mu.Lock()
	p.cache["dur-sess"].startedAt = time.Now().Add(-2 * time.Second)
	p.mu.Unlock()

	action := p.OnRequest(ctx, &pipeline.Context{
		Direction: pipeline.Outbound,
		Headers:   http.Header{},
		Session:   &pipeline.SessionView{ID: "dur-sess"},
	})
	if action.Type != pipeline.Reject {
		t.Fatalf("expected Reject after duration exceeded, got %v", action.Type)
	}
}
