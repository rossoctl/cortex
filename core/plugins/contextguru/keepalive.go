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

// cachePolicy resolves the keep-alive Policy from cache, the engine's own `cache:` block --
// parsed by cgconfig.LoadBytes from `engine:` in Configure, which has already run
// config.applyStrategy against the raw YAML, so cache.strategy's keys (if the operator named
// one) are already filled in by the time this runs.
//
// There used to be a second, plugin-level `keep_alive:` override here, predating
// `engine: cache:` support. It is gone: every one of its knobs (idle_seconds, max_pings,
// max_usd_per_ping, min_prefix_tokens) is a cache.keepalive_* key on config.CacheConfig already
// -- keeping both was keeping two copies of the same decision. An operator who still sends a
// `keep_alive:` block gets a WARN naming the deprecated key (Configure) and nothing else: the
// key is accepted and ignored, not refused, so an old document keeps loading.
//
// Configure defaults cache.strategy to "auto" itself when the operator's engine: YAML has no
// cache: key at all (see injectDefaultCacheStrategy), which is what keeps keep-alive "on by
// default on every install" true without this function carrying its own default.
func cachePolicy(cache cgconfig.CacheConfig) keepalive.Policy {
	resolved := cache.Resolved()
	return keepalive.Policy{
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
