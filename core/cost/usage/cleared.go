package usage

// Cleared implements session.Clearer: every figure answerable by session id goes — each
// session's own ring, and the session breakdown of the all-sessions ring and of every agent's.
// Everything else stays, including the all-sessions and per-agent totals, which name no session.
// They are the figures the cost ledger keeps on disk too, and a clear does not touch that.
//
// pending stays as well. It is keyed by request id and holds plugin names, not content: dropping
// it would only misattribute the plugins of a request in flight across the clear.
func (a *Aggregator) Cleared() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.logReplayLocked(func(r *Aggregator) { r.Cleared() })
	clear(a.sessions)
	dropSessionLabels(a.all)
	for _, ring := range a.agents {
		dropSessionLabels(ring)
	}
}

func dropSessionLabels(ring []bucket) {
	for i := range ring {
		ring[i].bySession = nil
	}
}
