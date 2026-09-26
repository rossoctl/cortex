package sessionbudget

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// newSplitPlugin builds a plugin with one per-kind limit set and no
// aggregate token limit, so a test can isolate the per-kind path from
// the existing MaxTokens enforcement.
func newSplitPlugin(t *testing.T, field string, limit int64) *SessionBudget {
	t.Helper()
	p := New()
	cfg := fmt.Sprintf(`{
		"redis_url": "mem://test",
		%q: %d,
		"refresh_interval": "100ms"
	}`, field, limit)
	if err := p.Configure(json.RawMessage(cfg)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	p.store = newMemStore()
	return p
}

// makePctxSplit is makePctx with the per-kind fields populated. The
// aggregate TotalTokens is left as the sum of the sub-kinds so the
// existing MaxTokens plumbing keeps seeing the same value it did
// before this feature landed.
func makePctxSplit(sessionID string, input, cacheRead, cacheWrite, output, reasoning int) *pipeline.Context {
	total := input + cacheRead + cacheWrite + output
	return &pipeline.Context{
		Direction: pipeline.Outbound,
		Headers:   http.Header{},
		Session:   &pipeline.SessionView{ID: sessionID},
		Extensions: pipeline.Extensions{
			Inference: &pipeline.InferenceExtension{
				TotalTokens:      total,
				InputTokens:      input,
				CacheReadTokens:  cacheRead,
				CacheWriteTokens: cacheWrite,
				OutputTokens:     output,
				ReasoningTokens:  reasoning,
			},
		},
	}
}

// TestOnResponseFrame_AccumulatesSplit confirms every sub-counter
// increments from a single response frame. This is the base contract
// the per-kind limits build on — if a sub-counter is stuck at zero,
// its limit can never trip.
func TestOnResponseFrame_AccumulatesSplit(t *testing.T) {
	p := newTestPlugin(0, 1, 0)
	pctx := makePctxSplit("sess-1", 100, 50, 25, 200, 75)

	if act := p.OnResponseFrame(context.Background(), pctx, nil, true); act.Type != pipeline.Continue {
		t.Fatalf("frame: got %v want Continue", act.Type)
	}

	p.mu.RLock()
	c := p.cache["sess-1"]
	p.mu.RUnlock()
	if c == nil {
		t.Fatal("no cache entry")
	}

	checks := []struct {
		name string
		got  int64
		want int64
	}{
		{"tokens", c.tokens, 375},
		{"inputTokens", c.inputTokens, 100},
		{"cacheReadTokens", c.cacheReadTokens, 50},
		{"cacheWriteTokens", c.cacheWriteTokens, 25},
		{"outputTokens", c.outputTokens, 200},
		{"reasoningTokens", c.reasoningTokens, 75},
	}
	for _, ch := range checks {
		if ch.got != ch.want {
			t.Errorf("%s = %d, want %d", ch.name, ch.got, ch.want)
		}
	}
}

// TestOnRequest_RejectsAtSplitLimit runs each per-kind limit through
// the same at-limit assertion so a regression in any one branch of
// evaluate() surfaces as its own failing row.
func TestOnRequest_RejectsAtSplitLimit(t *testing.T) {
	cases := []struct {
		name     string
		field    string
		limit    int64
		seed     func(*counters)
		wantWord string
	}{
		{"input", "max_input_tokens", 100, func(c *counters) { c.inputTokens = 100 }, "input"},
		{"cache_read", "max_cache_read_tokens", 100, func(c *counters) { c.cacheReadTokens = 100 }, "cache-read"},
		{"cache_write", "max_cache_write_tokens", 100, func(c *counters) { c.cacheWriteTokens = 100 }, "cache-write"},
		{"output", "max_output_tokens", 100, func(c *counters) { c.outputTokens = 100 }, "output"},
		{"reasoning", "max_reasoning_tokens", 100, func(c *counters) { c.reasoningTokens = 100 }, "reasoning"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newSplitPlugin(t, tc.field, tc.limit)
			c := &counters{}
			tc.seed(c)
			p.mu.Lock()
			p.cache["sess-1"] = c
			p.mu.Unlock()

			action := p.OnRequest(context.Background(), makePctx("sess-1", 0))
			if action.Type != pipeline.Reject {
				t.Fatalf("expected Reject, got %v", action.Type)
			}
		})
	}
}

// TestEvaluate_AggregateStillEnforced pins that MaxTokens keeps
// working unchanged when only the aggregate limit is set — the
// per-kind fields are optional additions, not a replacement.
func TestEvaluate_AggregateStillEnforced(t *testing.T) {
	p := newTestPlugin(500, 0, 0)
	p.mu.Lock()
	p.cache["sess-1"] = &counters{tokens: 500, inputTokens: 200, outputTokens: 300}
	p.mu.Unlock()

	action := p.OnRequest(context.Background(), makePctx("sess-1", 0))
	if action.Type != pipeline.Reject {
		t.Fatalf("aggregate limit: expected Reject, got %v", action.Type)
	}
}

// TestHydrate_LegacyKeyHasNoSplitFields covers the migration
// scenario: a Redis hash written before per-kind counters existed
// only carries `tokens`, `calls`, `started_at`. Hydrating it must
// succeed and set every per-kind counter to zero — no ParseInt
// error leaks out, no field goes to a garbage value.
func TestHydrate_LegacyKeyHasNoSplitFields(t *testing.T) {
	fields := map[string]string{
		"tokens":     "1234",
		"calls":      "7",
		"started_at": "1700000000",
	}
	c := parseCountersFromFields(fields)
	if c.tokens != 1234 || c.calls != 7 {
		t.Fatalf("aggregate parse: tokens=%d calls=%d", c.tokens, c.calls)
	}
	if c.inputTokens != 0 || c.cacheReadTokens != 0 || c.cacheWriteTokens != 0 ||
		c.outputTokens != 0 || c.reasoningTokens != 0 {
		t.Errorf("per-kind counters not zero on legacy key: %+v", c)
	}
	if c.startedAt.IsZero() {
		t.Error("startedAt lost")
	}
}
