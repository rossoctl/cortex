package sessionapi

import (
	"sync"
	"time"

	"github.com/rossoctl/cortex/core/model"
)

// eraseMarkTTL is how long an erase mark is kept: longer than a line can wait in the
// gateway's emitter queue.
const eraseMarkTTL = 10 * time.Minute

// eraseMarks remembers recent erases, so a line queued before an erase cannot bring the
// erased session back (praxis mode only). It is a store recorder: Deleted and Cleared run
// under the store's write lock, so they take only mu and never call the store.
type eraseMarks struct {
	mu        sync.Mutex
	now       func() time.Time
	ids       map[string]time.Time
	lastClear time.Time
}

func newEraseMarks() *eraseMarks {
	return &eraseMarks{now: time.Now, ids: map[string]time.Time{}}
}

func (m *eraseMarks) Record(string, *model.SessionEvent) {}

func (m *eraseMarks) Deleted(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for id, at := range m.ids {
		if now.Sub(at) > eraseMarkTTL {
			delete(m.ids, id)
		}
	}
	m.ids[sessionID] = now
}

func (m *eraseMarks) Cleared() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastClear = m.now()
	clear(m.ids) // the clear's mark is later than each of them
}

// erasedAfter reports whether sid was erased, by a delete or a clear, after startedAt and
// within the last eraseMarkTTL.
func (m *eraseMarks) erasedAfter(sid string, startedAt time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for _, at := range []time.Time{m.ids[sid], m.lastClear} {
		if !at.IsZero() && now.Sub(at) <= eraseMarkTTL && at.After(startedAt) {
			return true
		}
	}
	return false
}

// setClock replaces the clock, for tests.
func (m *eraseMarks) setClock(now func() time.Time) {
	m.mu.Lock()
	m.now = now
	m.mu.Unlock()
}
