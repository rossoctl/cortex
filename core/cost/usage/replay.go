package usage

import (
	"encoding/json"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/pipeline"
)

// BeginReplay starts a replay that runs alongside live traffic, and returns the private aggregator
// to feed it with Replay — configured as a is, written by nothing else. FinishReplay merges it
// into a, or AbandonReplay drops it.
//
// A PRIVATE RING, NOT a ITSELF, because live traffic writes a meanwhile. Replay's guard and the
// time-ordered feed that keeps eviction and request pairing right both assume they are the only
// writer, and the private ring keeps them so.
//
// Until the replay ends, a logs every erasure and rename it takes — Cleared, Deleted, Rekeyed — for
// FinishReplay to apply to the private ring first. Without that, a session the user erased while
// the replay was reading would come back with the merge.
func (a *Aggregator) BeginReplay() *Aggregator {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.replaying, a.replayOps = true, nil
	return New(WithClock(a.now), WithMaxSessions(a.maxSess), WithPricing(a.rates))
}

// FinishReplay merges r, the aggregator BeginReplay returned, into a and ends the replay.
//
// It first does to r what a did while the replay ran, in the order a did it. Then each ring
// merges slot by slot as mergeRing does for a rename: the same minute sums, the newer minute wins
// a slot, so a replayed minute that aged out meanwhile cannot overwrite a live one. Session rings
// beyond the cap go coldest first, which is a replayed one before any session live traffic has
// touched. r's unpaired requests are dropped: their process is gone, and no response will come.
func (a *Aggregator) FinishReplay(r *Aggregator) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, op := range a.replayOps {
		op(r)
	}
	a.replaying, a.replayOps = false, nil
	mergeRing(a.all, r.all)
	for name, ring := range r.agents {
		if into, ok := a.agents[name]; ok {
			mergeRing(into, ring)
		} else {
			a.agents[name] = ring
		}
	}
	for id, ring := range r.sessions {
		into, ok := a.sessions[id]
		if !ok {
			a.sessions[id] = ring
			continue
		}
		mergeRing(into.buckets, ring.buckets)
		if ring.lastSeen.After(into.lastSeen) {
			into.lastSeen = ring.lastSeen
		}
	}
	for len(a.sessions) > a.maxSess {
		a.evictColdestLocked()
	}
}

// AbandonReplay ends a replay without merging anything.
func (a *Aggregator) AbandonReplay() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.replaying, a.replayOps = false, nil
}

// logReplayLocked records op for FinishReplay while a replay runs. Caller holds mu.
func (a *Aggregator) logReplayLocked(op func(*Aggregator)) {
	if a.replaying {
		a.replayOps = append(a.replayOps, op)
	}
}

// Replay records an event from before this process started — the session archive's, at startup —
// and reports whether it was in range. It is Record with one guard: an event whose minute is in the
// future, or not among the newest NumBuckets-1 minutes, is dropped.
//
// THE GUARD IS WHAT MAKES OLD EVENTS SAFE TO FEED. A slot is a minute modulo NumBuckets, and a
// write to a slot holding a different minute resets it (see Aggregator): an event exactly
// NumBuckets minutes old shares its slot with the current minute and would erase it. Record never
// meets one, because live events are current; a replay is made of nothing else.
//
// ONE MINUTE OF MARGIN, so a minute boundary passing between this check and Record's lock cannot
// carry an event over the edge. A check outside the lock is enough because nothing else writes the
// ring a replay feeds: it is the private one BeginReplay returns.
//
// Feed events in ascending At order. Record pairs a request with the response after it, and
// reclaims the least recently written session ring at the cap, so that order is what makes the
// replayed ring the one the old process had.
func (a *Aggregator) Replay(sessionID string, e *pipeline.SessionEvent) bool {
	if e == nil || e.At.IsZero() {
		return false
	}
	now := a.now().Truncate(BucketWidth)
	m := e.At.Truncate(BucketWidth)
	if m.After(now) || !m.After(now.Add(-(NumBuckets-1)*BucketWidth)) {
		return false
	}
	a.Record(sessionID, e)
	return true
}

// ReplayCopy is e reduced to what Record reads, for a caller that holds many events before
// replaying them: an archived inference event carries its whole conversation, and six hours of
// those would otherwise sit in memory at once to be sorted. A copy costs about 1KB, however long
// the conversation was.
//
// A COPY WITH THE HEAVY FIELDS CLEARED, NOT A COPY OF THE FIELDS RECORD READS, so a field Record
// starts reading later is kept without anyone remembering to add it here. What is cleared is what
// Record has no use for: the protocol payloads, the conversation, the tool manifest, the completion
// and its tool calls, and every plugin entry but the cost record. TestReplayCopy_RecordsWhatTheEventDoes
// holds that.
func ReplayCopy(e *pipeline.SessionEvent) pipeline.SessionEvent {
	c := *e
	c.A2A, c.MCP = nil, nil
	if e.Inference != nil {
		inf := *e.Inference
		inf.Messages, inf.Tools, inf.ToolChoice, inf.ToolCalls, inf.Completion = nil, nil, nil, nil, ""
		inf.ToolResults = nil
		c.Inference = &inf
	}
	c.Plugins = nil
	for _, k := range [...]string{event.Key, event.PluginName} {
		if raw, ok := e.Plugins[k]; ok {
			if c.Plugins == nil {
				c.Plugins = make(map[string]json.RawMessage, 1)
			}
			c.Plugins[k] = raw
		}
	}
	return c
}
