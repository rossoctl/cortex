// Package tokenbudget enforces per-session lifetime budgets on tokens,
// inference calls, and wall-clock duration. Must run before inference-parser
// in the declared plugin order (response path is reverse: inference-parser
// finalizes counts first, then this plugin reads them).
package tokenbudget

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins"
	"github.com/rossoctl/cortex/authbridge/authlib/storage"
)

type config struct {
	RedisURL           string `json:"redis_url" required:"true" description:"Redis/Valkey connection URL."`
	MaxTokens          int64  `json:"max_tokens" description:"Cumulative token ceiling per session. 0 = no limit."`
	MaxCalls           int64  `json:"max_calls" description:"Max inference calls per session. 0 = no limit."`
	MaxDurationSeconds int64  `json:"max_duration_seconds" description:"Wall-clock session lifetime in seconds. 0 = no limit."`
	OnExceed           string `json:"on_exceed" description:"Action on breach: deny (block) or observe (shadow — log but continue)." default:"deny" enum:"deny,observe"`
	SessionTTLSeconds  int    `json:"session_ttl_seconds" description:"Redis key TTL; should be >= max_duration_seconds." default:"7200"`
	RefreshInterval    string `json:"refresh_interval" description:"How often to sync local cache from Redis." default:"5s"`
	RedisUnavailable   string `json:"redis_unavailable" description:"Behavior when Redis is unreachable." default:"fail_open" enum:"fail_open,fail_closed"`
}

type counters struct {
	tokens    int64
	calls     int64
	startedAt time.Time
}

// TokenBudget is the plugin state. Redis provides cross-pod durability;
// the local cache provides zero-I/O enforcement on the request path.
type TokenBudget struct {
	cfg   config
	store storage.Store
	log   *slog.Logger

	mu      sync.RWMutex
	cache   map[string]*counters
	stopCh  chan struct{}
	stopped chan struct{}
}

func New() *TokenBudget {
	return &TokenBudget{
		cache:   make(map[string]*counters),
		stopCh:  make(chan struct{}),
		stopped: make(chan struct{}),
		log:     slog.Default().With("plugin", "token-budget"),
	}
}

func init() {
	plugins.RegisterPlugin("token-budget", func() pipeline.Plugin { return New() })
}

func (p *TokenBudget) Name() string { return "token-budget" }

func (p *TokenBudget) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		Description: "Enforce per-session token, call, and duration budgets via Redis.",
	}
}

func (p *TokenBudget) Configure(raw json.RawMessage) error {
	p.cfg = config{
		OnExceed:          "deny",
		SessionTTLSeconds: 7200,
		RefreshInterval:   "5s",
		RedisUnavailable:  "fail_open",
	}
	if err := json.Unmarshal(raw, &p.cfg); err != nil {
		return fmt.Errorf("token-budget config: %w", err)
	}
	if p.cfg.RedisURL == "" {
		return fmt.Errorf("token-budget: redis_url is required")
	}
	if p.cfg.MaxTokens <= 0 && p.cfg.MaxCalls <= 0 && p.cfg.MaxDurationSeconds <= 0 {
		return fmt.Errorf("token-budget: at least one limit (max_tokens, max_calls, max_duration_seconds) must be > 0")
	}
	return nil
}

func (p *TokenBudget) Init(_ context.Context) error {
	store, err := storage.Open("redis", p.cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("token-budget: redis connect: %w", err)
	}
	p.store = store

	interval, err := time.ParseDuration(p.cfg.RefreshInterval)
	if err != nil {
		interval = 5 * time.Second
	}
	go p.refreshLoop(interval)
	return nil
}

func (p *TokenBudget) Shutdown(_ context.Context) error {
	close(p.stopCh)
	<-p.stopped
	if p.store != nil {
		return p.store.Close()
	}
	return nil
}

// OnRequest evaluates cached counters against limits. No I/O.
func (p *TokenBudget) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	sessionID := p.sessionID(pctx)
	if sessionID == "" {
		return pipeline.Action{Type: pipeline.Continue}
	}

	p.mu.RLock()
	c, ok := p.cache[sessionID]
	p.mu.RUnlock()

	if !ok {
		return pipeline.Action{Type: pipeline.Continue}
	}

	if reason := p.evaluate(c); reason != "" {
		if p.cfg.OnExceed == "observe" {
			pctx.Observe("shadow_budget_exceeded")
			p.log.Warn("budget exceeded (shadow mode)",
				"session", sessionID,
				"reason", reason,
				"tokens", c.tokens,
				"calls", c.calls)
			return pipeline.Action{Type: pipeline.Continue}
		}
		return pipeline.DenyWithDetails("budget.exceeded", reason, map[string]any{
			"spent_tokens": c.tokens,
			"spent_calls":  c.calls,
			"limit_tokens": p.cfg.MaxTokens,
			"limit_calls":  p.cfg.MaxCalls,
		})
	}
	return pipeline.Action{Type: pipeline.Continue}
}

// OnResponse is a no-op; see OnResponseFrame.
func (p *TokenBudget) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// OnResponseFrame accumulates token counts on finalization (last=true).
func (p *TokenBudget) OnResponseFrame(_ context.Context, pctx *pipeline.Context, _ []byte, last bool) pipeline.Action {
	if !last {
		return pipeline.Action{Type: pipeline.Continue}
	}

	sessionID := p.sessionID(pctx)
	if sessionID == "" {
		return pipeline.Action{Type: pipeline.Continue}
	}

	inf := pctx.Extensions.Inference
	if inf == nil || inf.TotalTokens == 0 {
		return pipeline.Action{Type: pipeline.Continue}
	}

	tokens := int64(inf.TotalTokens)

	go p.accumulate(sessionID, tokens)

	p.mu.Lock()
	c, ok := p.cache[sessionID]
	if !ok {
		c = &counters{startedAt: time.Now()}
		p.cache[sessionID] = c
	}
	c.tokens += tokens
	c.calls++
	p.mu.Unlock()

	return pipeline.Action{Type: pipeline.Continue}
}

func (p *TokenBudget) evaluate(c *counters) string {
	if p.cfg.MaxTokens > 0 && c.tokens >= p.cfg.MaxTokens {
		return fmt.Sprintf("token limit reached: %d/%d", c.tokens, p.cfg.MaxTokens)
	}
	if p.cfg.MaxCalls > 0 && c.calls >= p.cfg.MaxCalls {
		return fmt.Sprintf("call limit reached: %d/%d", c.calls, p.cfg.MaxCalls)
	}
	if p.cfg.MaxDurationSeconds > 0 && !c.startedAt.IsZero() {
		elapsed := time.Since(c.startedAt).Seconds()
		if int64(elapsed) >= p.cfg.MaxDurationSeconds {
			return fmt.Sprintf("duration limit reached: %ds/%ds", int64(elapsed), p.cfg.MaxDurationSeconds)
		}
	}
	return ""
}

// accumulate writes counters to Redis. On failure, writes are dropped (fail-open).
func (p *TokenBudget) accumulate(sessionID string, tokens int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	key := p.redisKey(sessionID)
	ttl := time.Duration(p.cfg.SessionTTLSeconds) * time.Second

	_, err := p.store.HashIncr(ctx, key, "tokens", tokens)
	if err != nil {
		p.log.Warn("redis HashIncr tokens failed", "session", sessionID, "err", err)
		return
	}

	_, _ = p.store.HashIncr(ctx, key, "calls", 1)

	set, _ := p.store.HashSetNX(ctx, key, "started_at", strconv.FormatInt(time.Now().Unix(), 10))
	if set {
		_ = p.store.Expire(ctx, key, ttl)
	}
}

func (p *TokenBudget) refreshLoop(interval time.Duration) {
	defer close(p.stopped)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.refreshCache()
		}
	}
}

// refreshCache replaces local counters with authoritative Redis values.
func (p *TokenBudget) refreshCache() {
	p.mu.RLock()
	keys := make([]string, 0, len(p.cache))
	for k := range p.cache {
		keys = append(keys, k)
	}
	p.mu.RUnlock()

	for _, sessionID := range keys {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		fields, err := p.store.HashGet(ctx, p.redisKey(sessionID))
		cancel()

		if err != nil {
			// TODO: fail_closed should deny requests when Redis is unreachable
			// and the local cache has no data. Currently both modes retain stale cache.
			if p.cfg.RedisUnavailable != "fail_open" {
				p.log.Warn("redis refresh failed", "session", sessionID, "err", err)
			}
			continue
		}

		if len(fields) == 0 {
			p.mu.Lock()
			delete(p.cache, sessionID)
			p.mu.Unlock()
			continue
		}

		tokens, _ := strconv.ParseInt(fields["tokens"], 10, 64)
		calls, _ := strconv.ParseInt(fields["calls"], 10, 64)
		var startedAt time.Time
		if ts, err := strconv.ParseInt(fields["started_at"], 10, 64); err == nil {
			startedAt = time.Unix(ts, 0)
		}

		p.mu.Lock()
		p.cache[sessionID] = &counters{tokens: tokens, calls: calls, startedAt: startedAt}
		p.mu.Unlock()
	}
}

func (p *TokenBudget) sessionID(pctx *pipeline.Context) string {
	if pctx.Session != nil && pctx.Session.ID != "" {
		return pctx.Session.ID
	}
	return ""
}

func (p *TokenBudget) redisKey(sessionID string) string {
	return "token-budget:" + sessionID
}

var (
	_ pipeline.Plugin             = (*TokenBudget)(nil)
	_ pipeline.Configurable       = (*TokenBudget)(nil)
	_ pipeline.Initializer        = (*TokenBudget)(nil)
	_ pipeline.Shutdowner         = (*TokenBudget)(nil)
	_ pipeline.StreamingResponder = (*TokenBudget)(nil)
)
