package session

import "sync/atomic"

// TrackStream records n as the live byte counter of the open response with
// requestID. The writer of n must use atomic operations. Call untrack when the
// response ends. untrack removes the entry only if it still holds n, so a
// newer TrackStream with the same requestID stays.
func (s *Store) TrackStream(requestID string, n *int64) (untrack func()) {
	s.streamMu.Lock()
	if s.streams == nil {
		s.streams = make(map[string]*int64)
	}
	s.streams[requestID] = n
	s.streamMu.Unlock()
	return func() {
		s.streamMu.Lock()
		if s.streams[requestID] == n {
			delete(s.streams, requestID)
		}
		s.streamMu.Unlock()
	}
}

// StreamBytes returns the live byte count of the open response with
// requestID. It returns false when no open response has that ID.
func (s *Store) StreamBytes(requestID string) (int64, bool) {
	s.streamMu.Lock()
	n, ok := s.streams[requestID]
	s.streamMu.Unlock()
	if !ok {
		return 0, false
	}
	return atomic.LoadInt64(n), true
}
