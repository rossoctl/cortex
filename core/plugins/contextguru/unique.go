package contextguru

import (
	"crypto/sha256"
	"sync"
)

// maxSeenKeys bounds seenKeys. The engine's dash.Recorder uses the same cap for the same set.
// Each entry is a 16-byte hash, so the set stays under ~10 MB.
const maxSeenKeys = 200_000

// seenKeys remembers which content each session has already had removed, so a saving can be
// split into content removed for the first time and content removed again. The engine re-applies
// a reduction on every turn the content is re-sent; by then the original sits in the provider's
// cached prefix, and settle must price it at the cache-read rate, not as fresh input.
//
// The rule is the engine's dash.Recorder.MarkUnique, so Cortex and the context-guru proxy agree:
// keys are the content-derived Report.CacheKeys, the unique part is saved*newKeys/len(keys), and a
// component with no keys counts as unique. Scoped by session rather than by tenant, because a
// provider cache is per conversation: content first seen in another session is new in this one.
//
// Cleared when full, as MarkUnique is. After a clear, a repeat counts once more as unique; that
// over-states one turn and does not accumulate. A hot-reload builds a new plugin and starts empty
// for the same reason and with the same bound.
type seenKeys struct {
	mu   sync.Mutex
	keys map[[16]byte]struct{}
}

func newSeenKeys() *seenKeys { return &seenKeys{keys: map[[16]byte]struct{}{}} }

// markUnique records keys for (session, component) and returns the part of saved they did not
// already cover.
func (s *seenKeys) markUnique(session, component string, keys []string, saved int) int {
	if saved <= 0 {
		return 0
	}
	if len(keys) == 0 {
		return saved
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.keys) > maxSeenKeys {
		s.keys = map[[16]byte]struct{}{}
	}
	fresh := 0
	for _, k := range keys {
		h := sha256.Sum256([]byte(session + "\x00" + component + "\x00" + k))
		var id [16]byte
		copy(id[:], h[:16])
		if _, ok := s.keys[id]; !ok {
			s.keys[id] = struct{}{}
			fresh++
		}
	}
	return saved * fresh / len(keys)
}

// forSession binds markUnique to one session, in the shape savingEventFrom takes. Nil on a nil
// set, which savingEventFrom reads as "every saving is new".
func (s *seenKeys) forSession(session string) uniqueFunc {
	if s == nil {
		return nil
	}
	return func(component string, keys []string, saved int) int {
		return s.markUnique(session, component, keys, saved)
	}
}
