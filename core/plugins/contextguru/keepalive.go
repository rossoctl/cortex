package contextguru

import (
	"time"

	cgcomponents "github.com/rossoctl/context-guru/components"
	cgconfig "github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dispatch"
	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/cortex/core/internal/parsercommon"
	"github.com/rossoctl/cortex/core/pipeline"
)

// keepAliveConfig is the plugin's `keep_alive:` block — one static policy, no strategies and no
// per-session overrides (spec §10, Phase 1 table: "simpler than the standalone proxy by design").
// Every value is a knob on a decision the engine makes; none of them is a decision. A zero value
// takes the engine's own shipped default (config.DefaultKeepAlive*).
type keepAliveConfig struct {
	Enabled         bool    `json:"enabled" description:"Ping an idle session's prompt cache before it expires. Each ping is billed to the caller's own credential." default:"true"`
	IdleSeconds     int     `json:"idle_seconds" description:"Idle time before the first ping, from the previous request's start." default:"240"`
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

// agentThreadPrefix marks a keeper thread that one named agent owns. It is the core's own form —
// context-guru's thread resolver names a header thread "a:<agent id>", and keepalive.IsSubagentThread
// tests for this prefix to give such a thread its subagent floors and ping plans. The core exports
// the test but not a constructor, so the prefix is spelled here; TestRecordForKeysAnAgentByItsOwnThread
// fails if the two ever disagree.
const agentThreadPrefix = "a:"

// keeperKey is the keeper entry a request belongs to: one per thread, not per session (engine
// #423), because each thread of a session has its own provider cache entry. A request from a named
// Claude Code agent (pctx.Subagent: a subagent's, or an agent-team teammate's) is its agent's own
// thread; every other request is the session's main thread, Thread "".
//
// ok is false for a request marked as a subagent's that names no agent: it is not the main thread,
// and no key tells it apart from the session's other subagents, so it has no entry of its own.
//
// pctx.Subagent reads headers only, so OnRequest and OnFinish get the same key, also when
// Extensions.Inference is not parsed yet at OnRequest.
func keeperKey(pctx *pipeline.Context, sid string) (keepalive.Key, bool) {
	if agent := pctx.Subagent(); agent != "" {
		return keepalive.Key{Session: sid, Thread: agentThreadPrefix + agent}, true
	}
	if isSubagent(pctx) {
		return keepalive.Key{}, false
	}
	return keepalive.Key{Session: sid}, true
}

// recordFor maps a finished request to what the keeper needs. Mapping only: whether the request
// is worth tracking is the keeper's call. The one filter here is about identity, not policy — a
// request with no thread of its own (keeperKey) is not recorded, because recording it would
// replace the main thread's entry.
//
// em reports each ping the entry produces; OnFinish passes a pingEmitter, which also records its cost.
func recordFor(pctx *pipeline.Context, sid string, pol keepalive.Policy, hold func() dispatch.Ref,
	em cgcomponents.Emitter) (keepalive.Record, bool) {
	inf := pctx.Extensions.Inference
	if sid == "" || inf == nil {
		return keepalive.Record{}, false
	}
	key, ok := keeperKey(pctx, sid)
	if !ok {
		return keepalive.Record{}, false
	}
	provider := providerFor(pctx.Path)
	// Cred is the caller's own credential as an opaque per-process id (credID), so a refusal marks
	// that credential dead instead of tripping the route for every caller of the endpoint.
	return keepalive.Record{
		Key:       key,
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
