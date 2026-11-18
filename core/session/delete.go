package session

// Deleter is optionally implemented by a Recorder that keeps state keyed by session id: the
// usage aggregator's per-session figures, the archive's numbering and files. Delete tells each
// one, under the store's write lock and after the store has dropped the session, so no append
// can land between the two halves and be kept by one side only.
//
// Deleted runs in front of live traffic, like Record: it must not block. Work that takes time —
// the archive's file deletion — is queued for the Deleter's own goroutine.
type Deleter interface {
	Deleted(sessionID string)
}

// Delete removes one session — its entry, owner and adoption records, and the active-session
// mark if it holds it — tells every Deleter, and reports whether memory held it.
//
// THE DELETERS ARE TOLD EVEN WHEN MEMORY DID NOT HOLD THE ID. The archive keeps sessions the
// store has evicted, and startup replays the archive into the usage ring, so either can hold a
// session alone; a delete that skipped them would leave it listed.
//
// Memory is touched only when it holds the id. An adopted pending id has no entry but still has
// its adoption, which redirects the response in flight at adoption time (followAdoptedLocked);
// dropping it would let that response re-create the pending bucket as a stray row.
//
// Process claims stay, as eviction leaves them: resolution ignores a claim on an id that is not
// live (see newestClaimLocked). The next append to the id starts a new session.
func (s *Store) Delete(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, held := s.sessions[sessionID]
	if held {
		s.removeLocked(sessionID)
	}
	for _, r := range s.recorders {
		if d, ok := r.(Deleter); ok {
			d.Deleted(sessionID)
		}
	}
	return held
}
