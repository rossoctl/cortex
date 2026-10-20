package contextguru

import (
	"time"

	cgcomponents "github.com/rossoctl/context-guru/components"
	cgconfig "github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dispatch"
	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/internal/parsercommon"
)

// keepAliveConfig is the plugin's `keep_alive:` block — one static policy, no strategies and no
// per-session overrides (spec §10, Phase 1 table: "simpler than the standalone proxy by design").
// Every value is a knob on a decision the engine makes; none of them is a decision. A zero value
// takes the engine's own shipped default (config.DefaultKeepAlive*).
type keepAliveConfig struct {
	Enabled         bool    `json:"enabled" description:"Ping an idle session's prompt cache before it expires. Each ping is billed to the caller's own credential."`
	IdleSeconds     int     `json:"idle_seconds" description:"Idle time before the first ping, from the previous request's start." default:"270"`
	MaxPings        int     `json:"max_pings" description:"Most pings in one idle span." default:"2"`
	MaxUSDPerPing   float64 `json:"max_usd_per_ping" description:"Refuse a ping whose projected cost exceeds this (USD)." default:"0.50"`
	MinPrefixTokens int     `json:"min_prefix_tokens" description:"Billed prefix a session must reach before it is pinged." default:"80000"`
}

func (c keepAliveConfig) policy() keepalive.Policy {
	pol := keepalive.Policy{
		KeepAlive:       c.Enabled,
		Idle:            time.Duration(cgconfig.DefaultKeepAliveIdle) * time.Second,
		MaxPings:        cgconfig.DefaultKeepAliveMaxPings,
		MaxUSDPerPing:   cgconfig.DefaultKeepAliveMaxUSDPerPing,
		MinPrefixTokens: cgconfig.DefaultKeepAliveMinPrefix,
	}
	if c.IdleSeconds > 0 {
		pol.Idle = time.Duration(c.IdleSeconds) * time.Second
	}
	if c.MaxPings > 0 {
		pol.MaxPings = c.MaxPings
	}
	if c.MaxUSDPerPing > 0 {
		pol.MaxUSDPerPing = c.MaxUSDPerPing
	}
	if c.MinPrefixTokens > 0 {
		pol.MinPrefixTokens = c.MinPrefixTokens
	}
	return pol
}

// recordFor maps a finished request to what the keeper needs. Mapping only: whether the request
// is worth tracking is the keeper's call. The one filter here is about identity, not policy —
// Cortex knows a request is a subagent's but not WHICH subagent, so recording it would replace the
// main thread's entry (engine #423). Subagent caches are therefore not kept warm.
//
// em reports each ping the entry produces; OnFinish passes a pingEmitter, which also records its cost.
func recordFor(pctx *pipeline.Context, sid string, pol keepalive.Policy, hold func() dispatch.Ref,
	em cgcomponents.Emitter) (keepalive.Record, bool) {
	inf := pctx.Extensions.Inference
	if sid == "" || inf == nil || inf.AgentRole == pipeline.AgentRoleSubagent {
		return keepalive.Record{}, false
	}
	provider, _ := dialectFor(pctx.Path)
	// Cred is the caller's own credential as an opaque per-process id (credID), so a refusal marks
	// that credential dead instead of tripping the route for every caller of the endpoint.
	return keepalive.Record{
		Key:       keepalive.Key{Session: sid},
		Cred:      credID(pctx.Headers),
		StartedAt: pctx.StartedAt,
		Body:      pctx.Body,
		Provider:  provider,
		Endpoint:  pctx.Host,
		Route:     pctx.Path,
		Status:    pctx.StatusCode,
		Usage: keepalive.Usage{FreshInput: int64(inf.InputTokens), CacheRead: int64(inf.CacheReadTokens),
			CacheWrite: int64(inf.CacheWriteTokens), Output: int64(inf.OutputTokens)},
		UsageOK: parsercommon.Kind(inf.PresentKinds)&(parsercommon.KindCacheRead|parsercommon.KindCacheWrite) != 0,
		Policy:  pol,
		Agent:   pctx.Headers.Get("User-Agent"),
		Header:  pctx.Headers,
		Emitter: em,
		Hold:    hold,
	}, true
}
