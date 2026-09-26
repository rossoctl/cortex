// Package shared provides a generic, process-scoped, TTL key→value store
// that plugins reach via pipeline.Context.Shared. It is intentionally
// semantics-free — feature-specific conventions (e.g. credential
// placeholders) live in their own packages and namespace their keys.
package shared

import (
	"sync"
	"time"
)

const defaultSweepInterval = time.Minute

type entry struct {
	val     any
	expires time.Time
}

// Store is a thread-safe TTL map. The zero value is not usable; call New.
type Store struct {
	mu        sync.RWMutex
	items     map[string]entry
	now       func() time.Time // injectable for tests
	stop      chan struct{}
	closeOnce sync.Once
}

// New returns an empty Store with a background janitor running. Call Close
// to stop the janitor when the store is no longer needed.
func New() *Store {
	s := &Store{items: make(map[string]entry), now: time.Now, stop: make(chan struct{})}
	go s.janitor(defaultSweepInterval)
	return s
}

// Put stores val under key with the given time-to-live.
func (s *Store) Put(key string, val any, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[key] = entry{val: val, expires: s.now().Add(ttl)}
}

// Get returns the value for key if present and unexpired. Expired entries
// are evicted lazily.
func (s *Store) Get(key string) (any, bool) {
	s.mu.RLock()
	e, ok := s.items[key]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if s.now().After(e.expires) {
		s.mu.Lock()
		// Re-check under the write lock so a concurrent Put that refreshed
		// this key isn't clobbered by a stale eviction.
		if cur, present := s.items[key]; present && s.now().After(cur.expires) {
			delete(s.items, key)
		}
		s.mu.Unlock()
		return nil, false
	}
	return e.val, true
}

// Delete removes key.
func (s *Store) Delete(key string) {
	s.mu.Lock()
	delete(s.items, key)
	s.mu.Unlock()
}

// janitor periodically reclaims expired entries until Close is called.
func (s *Store) janitor(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.sweep()
		case <-s.stop:
			return
		}
	}
}

// sweep deletes all expired entries.
func (s *Store) sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, e := range s.items {
		if now.After(e.expires) {
			delete(s.items, k)
		}
	}
}

// Close stops the background janitor. Safe to call multiple times.
func (s *Store) Close() {
	s.closeOnce.Do(func() { close(s.stop) })
}
