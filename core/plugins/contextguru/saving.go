package contextguru

import (
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/cortex/core/pipeline"
)

// savingEvent is the per-request record published under "context-guru/event", so cost/settle can
// price what this request saved and attribute it to context-guru. FACTS ONLY, like tool-prune's
// event: no rates, no dollars, and no message text — the session API is unauthenticated, so
// apply.Trace.Changes (clipped before/after text) is never published.
//
// The field names are a wire contract with cost/settle, which reads them structurally rather
// than importing this package; a test there fails if one is renamed.
type savingEvent struct {
	// BytesRemoved and BodyBytesAfter mean what tool-prune's do, so settle shares its calibration.
	BytesRemoved   int `json:"bytesRemoved"`
	BodyBytesAfter int `json:"bodyBytesAfter"`
	// Projected marks a saving that was measured but not applied (on_error: observe): the bytes
	// went upstream and the money was spent. Set from this plugin's own SetBody result.
	Projected bool `json:"projected,omitempty"`
	// TokensBefore and TokensAfter are the engine's own token counts for the whole request.
	TokensBefore int `json:"tokensBefore"`
	TokensAfter  int `json:"tokensAfter"`
	// FrozenTokens is the cached prefix the engine left alone for cache safety.
	FrozenTokens int `json:"frozenTokens,omitempty"`
	// PrefixRewritten says a change landed inside the prefix the provider had cached on the
	// previous turn, so the provider re-writes that prefix: settle charges that as a penalty.
	// False when the engine did not know the boundary (not cache-aware, or the first turn): the
	// penalty is then unknown, not zero, and settle cannot charge it.
	PrefixRewritten bool `json:"prefixRewritten,omitempty"`
	// SavedGross and SavedUnique split the components' savings, in engine tokens, into all of it
	// and the part this session has not had removed before. Content removed on an earlier turn
	// sits in the provider's cached prefix by now, so without compaction it would have been a
	// cache read, not fresh input: settle prices the two parts at different rates, with the
	// fraction SavedUnique/SavedGross. Both are always sent, so settle can tell "all repeat"
	// (SavedUnique 0) from a producer that does not split.
	SavedGross  int               `json:"savedGross"`
	SavedUnique int               `json:"savedUnique"`
	Components  []componentSaving `json:"components,omitempty"`
}

// componentSaving is one engine component's contribution, in engine tokens.
type componentSaving struct {
	Component string `json:"component"`
	Saved     int    `json:"saved"`
	// Replay marks a saving made by replaying a decision taken on an earlier turn: no model
	// call and no new spend, so it must not be read as a fresh act.
	Replay bool `json:"replay,omitempty"`
	// Unique is the part of Saved this session has not had removed before (seenKeys.markUnique).
	Unique int `json:"unique"`
}

// uniqueFunc returns the part of a component's saving that is new to the session, given the
// component's content keys (seenKeys.markUnique, bound to one session).
type uniqueFunc func(component string, keys []string, saved int) int

// savingEventFrom builds the event from the body sizes and the engine's trace. applied is this
// plugin's own SetBody result. unique splits each saving into new and repeated content; nil
// counts every saving as new.
func savingEventFrom(before, after int, applied bool, tr apply.Trace, unique uniqueFunc) savingEvent {
	ev := savingEvent{
		BytesRemoved:   before - after,
		BodyBytesAfter: after,
		Projected:      !applied,
		FrozenTokens:   tr.FrozenTokens,
	}
	if tr.Run == nil {
		return ev
	}
	ev.TokensBefore, ev.TokensAfter = tr.Run.TokensBefore, tr.Run.TokensAfter
	known := tr.CacheAware && tr.MaxCachedIdx >= 0
	for _, r := range tr.Run.Components {
		if r.Skipped || r.Reverted {
			continue
		}
		if known && !ev.PrefixRewritten {
			for _, i := range r.ChangedIdx {
				if i <= tr.MaxCachedIdx {
					ev.PrefixRewritten = true
					break
				}
			}
		}
		if s := r.Saved(); s > 0 {
			u := s
			if unique != nil {
				u = unique(r.Component, r.CacheKeys, s)
			}
			ev.SavedGross += s
			ev.SavedUnique += u
			ev.Components = append(ev.Components, componentSaving{Component: r.Component, Saved: s,
				Replay: r.Replays > 0, Unique: u})
		}
	}
	return ev
}

// publishSaving records the event for this request. One writer per key: this plugin's name.
func publishSaving(pctx *pipeline.Context, ev savingEvent) {
	if pctx.Extensions.Custom == nil {
		pctx.Extensions.Custom = map[string]any{}
	}
	pctx.Extensions.Custom["context-guru"+pipeline.PluginEventSuffix] = ev
}
