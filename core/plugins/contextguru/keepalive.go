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

// keepAliveConfig is the plugin's `keep_alive:` block — a legacy static override, kept for
// back-compat with an install that tunes keep-alive here instead of under `engine: cache:`.
// Every value is a knob on a decision the engine makes; none of them is a decision. A zero value
// takes the engine's own shipped default (config.DefaultKeepAlive*), or the strategy's resolved
// value when `engine: cache: strategy` is set (policy, below).
type keepAliveConfig struct {
	Enabled         bool    `json:"enabled" description:"Ping an idle session's prompt cache before it expires. Each ping is billed to the caller's own credential." default:"true"`
	IdleSeconds     int     `json:"idle_seconds" description:"Idle time before the first ping, from the previous request's start." default:"240"`
	MaxPings        int     `json:"max_pings" description:"Most pings in one idle span." default:"2"`
	MaxUSDPerPing   float64 `json:"max_usd_per_ping" description:"Refuse a ping whose projected cost exceeds this (USD)." default:"0.50"`
	MinPrefixTokens int     `json:"min_prefix_tokens" description:"Billed prefix a session must reach before it is pinged." default:"80000"`
}

// policy resolves the keep-alive Policy from cache, the engine's own `cache:` block (parsed by
// cgconfig.LoadBytes from `engine:` — see Configure), and c, this plugin's legacy `keep_alive:`
// override.
//
// Without `cache.strategy` set, nothing here changes from before: c alone decides every field,
// as policy() always did. This is deliberate — an install that has never heard of `engine: cache:`
// keeps behaving exactly as it does today.
//
// With `cache.strategy` set, cache.Resolved() (named strategies: none, auto, 1-ping, 2-ping,
// 1-hour-head, 1-hour-on-wait, 1-hour-always, 30-min-ping — see context-guru's
// docs/how-to/cache-strategies.md) decides every field, INCLUDING whether keep-alive runs at all
// ("none" sets no cache key, so cache.KeepAlive is false). The one override kept across both
// paths is c.Enabled == false: an explicit `keep_alive: {enabled: false}` always wins, as a kill
// switch independent of the chosen strategy. Mixing the two tuning surfaces (a strategy AND
// c's other knobs) is not supported — set one or the other, not both — because resolving
// per-field precedence between two plugin-level overrides would be a local policy decision, not
// a translation of what the engine already resolved.
func (c keepAliveConfig) policy(cache cgconfig.CacheConfig) keepalive.Policy {
	if cache.Strategy == "" {
		// WaitPlan is left false (the zero value) here on purpose, matching the doc above: with no
		// `cache.strategy` set, this branch must be byte-for-byte what policy() always returned.
		// Turning the per-wait plan on changes real scheduling, not just a tunable — #858's "a
		// thread with no tool call yet is not pinged" would stop every reply that has not yet
		// called a tool, which is also every fixture in this package's own tests. That default
		// change is opt-in, through a named strategy (the branch below), not bundled into the
		// legacy keep_alive block's existing meaning.
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
	resolved := cache.Resolved()
	pol := keepalive.Policy{
		KeepAlive:            resolved.KeepAlive,
		Idle:                 time.Duration(resolved.KeepAliveIdleSeconds) * time.Second,
		OpenAIIdle:           time.Duration(resolved.KeepAliveOpenAIIdleSeconds) * time.Second,
		MaxPings:             resolved.KeepAliveMaxPings,
		Idle1h:               time.Duration(resolved.KeepAliveIdleSeconds1h) * time.Second,
		MaxPings1h:           resolved.KeepAliveMaxPings1h,
		MaxUSDPerPing:        resolved.KeepAliveMaxUSDPerPing,
		MaxBodyBytes:         resolved.KeepAliveMaxBodyBytes,
		MinPrefixTokens:      resolved.KeepAliveMinPrefixTokens,
		HeadTTL1h:            resolved.HeadTTL1h,
		HeadTTLMinBytes:      resolved.HeadTTLMinBytes,
		WaitPlan:             resolved.WaitPlanOn(),
		OneHourOnWait:        resolved.KeepAliveOneHourOnWait,
		OneHourRebuildTokens: resolved.KeepAliveOneHourRebuildTokens,
		OneHourAlways:        resolved.KeepAliveOneHourAlways,
		ToolWaitMaxPings:     resolved.KeepAliveToolWaitMaxPings,
		ToolWaitMinPrefix:    resolved.KeepAliveToolWaitMinPrefixTokens,
		BackgroundExtraPings: resolved.KeepAliveBackgroundExtraPings,
		HumanExtraPings:      resolved.KeepAliveHumanExtraPings,
	}
	if !c.Enabled {
		pol.KeepAlive = false
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

// replayable reports whether pctx's body, path and headers are the bytes that went upstream.
func replayable(pctx *pipeline.Context) bool {
	if _, _, _, translated := pctx.TranslateTarget(); translated {
		return false
	}
	return len(pctx.UpstreamHeaders()) == 0
}

// recordFor maps a finished request to what the keeper needs. Mapping only: whether the request
// is worth tracking is the keeper's call. The filters here are about identity and replay, not
// policy:
//   - a request with no thread of its own (keeperKey) is not recorded, because recording it would
//     replace the main thread's entry.
//   - a request that a router translated, or gave upstream headers to, is not recorded. The
//     listener converts the body, rewrites the path and sets those headers on the wire only, so
//     pctx.Body, pctx.Path and pctx.Headers are not what went upstream. A ping built from them
//     would send the client's protocol to the target's host with the target's credential, and
//     its refusal would mark that credential dead.
//
// em reports each ping the entry produces; OnFinish passes a pingEmitter, which also records its cost.
func recordFor(pctx *pipeline.Context, sid string, pol keepalive.Policy, hold func() dispatch.Ref,
	em cgcomponents.Emitter) (keepalive.Record, bool) {
	inf := pctx.Extensions.Inference
	if sid == "" || inf == nil || !replayable(pctx) {
		return keepalive.Record{}, false
	}
	key, ok := keeperKey(pctx, sid)
	if !ok {
		return keepalive.Record{}, false
	}
	provider := providerFor(pctx.Path)
	// toolUseNames is every tool the response called, in call order — read only so the keeper's
	// WaitPlan (Policy.WaitPlan) can classify what the thread is about to wait on: its own tool
	// result (own_tool), a terminal tool (done), or neither (human/background/teammate/unknown).
	// ToolUseIncomplete is always false here: that flag exists for a host that assembles
	// ToolCalls from a spliced sniff window of its own streaming buffer, which does not describe
	// this parser — inferenceparser fully assembles ToolCalls (streaming or not) before this
	// plugin's OnFinish runs.
	//
	// OpenTasks is left nil: a background-task/teammate wait (Claude Code's own markers in the
	// transcript) is classified by context-guru's internal/waitcause scanner, which the core does
	// not export. Until it does, such a wait here falls back to WaitingFor's "unknown" cause
	// rather than "background"/"teammate" — a known gap, not a silent approximation.
	var toolUseNames []string
	for _, tc := range inf.ToolCalls {
		toolUseNames = append(toolUseNames, tc.Name)
	}
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
			CacheWrite: int64(inf.CacheWriteTokens), CacheWrite1h: int64(inf.CacheWrite1hTokens),
			Output: int64(inf.OutputTokens), StopReason: inf.FinishReason},
		UsageOK:           parsercommon.Kind(inf.PresentKinds)&(parsercommon.KindCacheRead|parsercommon.KindCacheWrite) != 0,
		ToolUseNames:      toolUseNames,
		ToolUseIncomplete: false,
		Policy:            pol,
		Agent:             pctx.Headers.Get("User-Agent"),
		Header:            pctx.Headers,
		Emitter:           em,
		Hold:              hold,
	}, true
}
