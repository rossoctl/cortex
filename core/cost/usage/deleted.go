package usage

// Deleted implements session.Deleter: every figure answerable by sessionID goes — its own ring,
// and its row in the session breakdown of the all-sessions ring and of every agent's. The totals
// stay, as in Cleared: they name no session, and the cost ledger keeps the same figures on disk.
func (a *Aggregator) Deleted(sessionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.logReplayLocked(func(r *Aggregator) { r.Deleted(sessionID) })
	delete(a.sessions, sessionID)
	dropSessionLabel(a.all, sessionID)
	for _, ring := range a.agents {
		dropSessionLabel(ring, sessionID)
	}
}

// dropSessionLabel removes one session's row from every bucket's breakdown. The row is keyed by
// ringLabel, as Record files it.
func dropSessionLabel(ring []bucket, sessionID string) {
	label := ringLabel(sessionID)
	for i := range ring {
		delete(ring[i].bySession, label)
	}
}
