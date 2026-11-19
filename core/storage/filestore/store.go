// Package filestore is a storage.Store kept in memory and saved to one file, for a
// single process that needs its plugins' state to outlive it: the laptop proxy,
// where a restart is routine and there is no redis.
//
// It answers like the redis driver: a missing key reads as "", Set replaces a key's
// TTL and Incr keeps it, Expire with a TTL of zero or less deletes the key, and a
// key used as the other type — a string as a hash, or a hash as a string — is an
// error.
//
// Every change is held in memory and saved within one flush interval, and Close
// saves what is left, so a process that dies without closing the store loses at
// most that interval. The file is replaced whole, by rename, so it is never read
// half-written. One that does not parse is set aside as <path>.corrupt and the store
// starts empty: what it holds is worth keeping, not worth failing a start over.
//
// It is not shared between processes: two opening one file overwrite each other's
// saves.
package filestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rossoctl/cortex/core/storage"
)

func init() {
	storage.Register("file", func(url string) (storage.Store, error) {
		return Open(strings.TrimPrefix(url, "file://"))
	})
}

var _ storage.Store = (*Store)(nil)

// flushEvery is how long a change can wait in memory before it is saved.
const flushEvery = 2 * time.Second

var (
	errClosed    = errors.New("filestore: store is closed")
	errWrongType = errors.New("filestore: key holds the wrong type of value")
)

// entry is one key: a hash when Hash is non-nil, else the string Value. A hash
// always holds a field, as in redis, so a nil and an empty one never need telling
// apart.
type entry struct {
	Value string            `json:"v,omitempty"`
	Hash  map[string]string `json:"h,omitempty"`
	// Expires is the Unix time in nanoseconds the key expires at, 0 for never.
	Expires int64 `json:"exp,omitempty"`
}

// snapshot is the file's format.
type snapshot struct {
	Version int               `json:"version"`
	Entries map[string]*entry `json:"entries"`
}

// Store is the file-backed storage.Store.
type Store struct {
	path  string
	now   func() time.Time
	every time.Duration
	// tick, when set, paces the saves in place of a ticker at every.
	tick <-chan time.Time

	mu      sync.Mutex
	entries map[string]*entry
	dirty   bool
	closed  bool

	stop chan struct{}
	done chan struct{}
}

type option func(*Store)

func withClock(now func() time.Time) option { return func(s *Store) { s.now = now } }
func withFlushEvery(d time.Duration) option { return func(s *Store) { s.every = d } }
func withTick(c <-chan time.Time) option    { return func(s *Store) { s.tick = c } }

// Open loads the store saved at path, or starts an empty one when there is none,
// creating path's directory if it is missing.
func Open(path string) (*Store, error) { return openWith(path) }

func openWith(path string, opts ...option) (*Store, error) {
	s := &Store{path: path, now: time.Now, every: flushEvery, entries: make(map[string]*entry),
		stop: make(chan struct{}), done: make(chan struct{})}
	for _, o := range opts {
		o(s)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("filestore: %w", err)
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	go s.flushLoop()
	return s, nil
}

func (s *Store) load() error {
	b, err := os.ReadFile(s.path) //nolint:gosec // the path is the operator's
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("filestore: %w", err)
	}
	var snap snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		aside := s.path + ".corrupt"
		slog.Warn("filestore: the saved store does not parse, so it is set aside and the store starts empty",
			"path", s.path, "setAside", aside, "error", err)
		if rerr := os.Rename(s.path, aside); rerr != nil {
			return fmt.Errorf("filestore: setting aside %s: %w", s.path, rerr)
		}
		return nil
	}
	now := s.now().UnixNano()
	for k, e := range snap.Entries {
		if e != nil && (e.Expires == 0 || e.Expires > now) {
			s.entries[k] = e
		}
	}
	return nil
}

// live is key's entry, or nil when there is none or it has expired. An expired
// entry is deleted. Called with mu held.
func (s *Store) live(key string) *entry {
	e, ok := s.entries[key]
	if !ok {
		return nil
	}
	if e.Expires != 0 && e.Expires <= s.now().UnixNano() {
		delete(s.entries, key)
		s.dirty = true
		return nil
	}
	return e
}

// expiry is the Expires value for a TTL from now, 0 for none.
func (s *Store) expiry(ttl time.Duration) int64 {
	if ttl <= 0 {
		return 0
	}
	return s.now().Add(ttl).UnixNano()
}

// locked runs f with mu held, after refusing a closed store.
func (s *Store) locked(f func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errClosed
	}
	return f()
}

func (s *Store) Get(_ context.Context, key string) (string, error) {
	var v string
	err := s.locked(func() error {
		e := s.live(key)
		if e == nil {
			return nil
		}
		if e.Hash != nil {
			return errWrongType
		}
		v = e.Value
		return nil
	})
	return v, err
}

func (s *Store) Set(_ context.Context, key, value string, ttl time.Duration) error {
	return s.locked(func() error {
		s.entries[key] = &entry{Value: value, Expires: s.expiry(ttl)}
		s.dirty = true
		return nil
	})
}

func (s *Store) Incr(_ context.Context, key string, delta int64) (int64, error) {
	var n int64
	err := s.locked(func() error {
		e := s.live(key)
		if e == nil {
			e = &entry{Value: "0"}
			s.entries[key] = e
		}
		if e.Hash != nil {
			return errWrongType
		}
		cur, err := strconv.ParseInt(e.Value, 10, 64)
		if err != nil {
			return fmt.Errorf("filestore: value is not an integer: %w", err)
		}
		n = cur + delta
		e.Value = strconv.FormatInt(n, 10)
		s.dirty = true
		return nil
	})
	return n, err
}

// hash is key's hash for writing, created when the key is missing. Called with mu
// held.
func (s *Store) hash(key string) (*entry, error) {
	e := s.live(key)
	if e == nil {
		e = &entry{Hash: make(map[string]string, 1)}
		s.entries[key] = e
		return e, nil
	}
	if e.Hash == nil {
		return nil, errWrongType
	}
	return e, nil
}

func (s *Store) HashIncr(_ context.Context, key, field string, delta int64) (int64, error) {
	var n int64
	err := s.locked(func() error {
		e, err := s.hash(key)
		if err != nil {
			return err
		}
		var cur int64
		if v, ok := e.Hash[field]; ok {
			if cur, err = strconv.ParseInt(v, 10, 64); err != nil {
				return fmt.Errorf("filestore: hash field is not an integer: %w", err)
			}
		}
		n = cur + delta
		e.Hash[field] = strconv.FormatInt(n, 10)
		s.dirty = true
		return nil
	})
	return n, err
}

func (s *Store) HashGet(_ context.Context, key string) (map[string]string, error) {
	out := map[string]string{}
	err := s.locked(func() error {
		e := s.live(key)
		if e == nil {
			return nil
		}
		if e.Hash == nil {
			return errWrongType
		}
		for k, v := range e.Hash {
			out[k] = v
		}
		return nil
	})
	return out, err
}

func (s *Store) HashSetNX(_ context.Context, key, field, value string) (bool, error) {
	var set bool
	err := s.locked(func() error {
		e, err := s.hash(key)
		if err != nil {
			return err
		}
		if _, ok := e.Hash[field]; ok {
			return nil
		}
		e.Hash[field] = value
		s.dirty, set = true, true
		return nil
	})
	return set, err
}

func (s *Store) Expire(_ context.Context, key string, ttl time.Duration) error {
	return s.locked(func() error {
		e := s.live(key)
		if e == nil {
			return nil
		}
		if ttl <= 0 {
			delete(s.entries, key)
		} else {
			e.Expires = s.expiry(ttl)
		}
		s.dirty = true
		return nil
	})
}

// Close saves what the store holds and stops its saving. Operations after it fail;
// a second Close does nothing.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	close(s.stop)
	<-s.done
	return s.flush()
}

func (s *Store) flushLoop() {
	defer close(s.done)
	tick := s.tick
	if tick == nil {
		t := time.NewTicker(s.every)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-s.stop:
			return
		case <-tick:
			if err := s.flush(); err != nil {
				slog.Warn("filestore: saving the store failed; the next interval retries", "path", s.path, "error", err)
			}
		}
	}
}

// flush saves the store when it has changed since the last save. The file is
// written beside its final name and renamed over it, so a reader sees the old file
// or the new one, never part of one.
func (s *Store) flush() error {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	now := s.now().UnixNano()
	snap := snapshot{Version: 1, Entries: make(map[string]*entry, len(s.entries))}
	for k, e := range s.entries {
		if e.Expires == 0 || e.Expires > now {
			c := *e
			if e.Hash != nil {
				c.Hash = make(map[string]string, len(e.Hash))
				for f, v := range e.Hash {
					c.Hash[f] = v
				}
			}
			snap.Entries[k] = &c
		}
	}
	s.dirty = false
	s.mu.Unlock()

	if err := s.write(snap); err != nil {
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
		return err
	}
	return nil
}

func (s *Store) write(snap snapshot) error {
	b, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("filestore: %w", err)
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // the path is the operator's
	if err != nil {
		return fmt.Errorf("filestore: %w", err)
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return fmt.Errorf("filestore: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("filestore: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("filestore: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("filestore: %w", err)
	}
	return nil
}
