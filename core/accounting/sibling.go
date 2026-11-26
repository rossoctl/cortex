package accounting

import (
	"slices"
	"sync"
	"time"

	"github.com/rossoctl/accountant"
)

// siblingsCap and siblingsTrimTo bound siblingStore over all sessions, the same way threadBilling
// and the library's own Trackers are bounded (10,000 threads, trimmed to 9,000 by age).
// siblingThreadsCap bounds the threads of one session, so one busy session cannot push out the
// others: a session past it drops its oldest thread, which is past any TTL by then.
const (
	siblingsCap       = threadBillingCap
	siblingsTrimTo    = threadBillingTrimTo
	siblingThreadsCap = 64
)

// siblingStore keeps, for each (session, thread), the last forwarded body and its breakpoints, and
// when the request was made. It gives EntryFacts.SharedWarm: the prefix of a request that another
// thread of the session kept warm inside the TTL. A keep-alive rescue must not credit that prefix
// (accountant R54, rossoctl/cortex#2138). It holds the library's Body (hashes and sizes only, R2),
// never content. The library keeps the same Body as the thread's previous request, so the two
// mostly share memory.
//
// It is not saved in the snapshot: after a restart, no sibling is known until each thread sends
// again. A missed sibling overstates the rescue, which is what happened before this store.
type siblingStore struct {
	mu       sync.Mutex
	sessions map[string]map[string]siblingEntry
	n        int // the entries over all sessions
}

type siblingEntry struct {
	at  time.Time
	fwd accountant.Body
	bps []accountant.Breakpoint
}

// sharedWarm records this request as thread's last in session, and, when want is true, returns
// the last position of fwd that a sibling kept warm, or nil when none did.
//
// The host chooses the siblings: another thread of the session whose last request was inside ttl
// before at, and not after it. accountant.SharedWarmFrom decides where the shared prefix ends. It
// compares bodies, so it runs outside the lock, and only when want is true: the library reads
// SharedWarm only on a request that carries a keep-alive ping's facts.
//
// at is the request's own time (RequestMeta.At), not the time this runs: records end on their
// own goroutines, in any order. A sibling whose record has not ended yet is missed, and the
// rescue is then overstated, as before this store. A sibling that got an error (a 429) did not
// warm the cache, but it still lowers the rescue: a doubtful figure may only lower a saving (R54).
func (s *siblingStore) sharedWarm(session, thread string, at time.Time, ttl time.Duration,
	fwd accountant.Body, bps []accountant.Breakpoint, want bool) *accountant.Position {
	if session == "" {
		return nil
	}
	var sibs []accountant.Sibling
	s.mu.Lock()
	threads := s.sessions[session]
	if want {
		for id, e := range threads {
			if id == thread || e.at.After(at) || at.Sub(e.at) > ttl {
				continue
			}
			sibs = append(sibs, accountant.Sibling{Body: e.fwd, Breakpoints: e.bps})
		}
	}
	s.putLocked(session, thread, siblingEntry{at: at, fwd: fwd, bps: bps})
	s.mu.Unlock()
	if len(sibs) == 0 {
		return nil
	}
	return accountant.SharedWarmFrom(fwd, sibs)
}

// putLocked keeps e as thread's last request, unless the thread already has a later one, and
// trims the store past its bounds.
func (s *siblingStore) putLocked(session, thread string, e siblingEntry) {
	if s.sessions == nil {
		s.sessions = map[string]map[string]siblingEntry{}
	}
	threads := s.sessions[session]
	if threads == nil {
		threads = map[string]siblingEntry{}
		s.sessions[session] = threads
	}
	old, ok := threads[thread]
	if ok && old.at.After(e.at) {
		return // a later request of the thread ended first
	}
	if !ok {
		if len(threads) >= siblingThreadsCap {
			oldest := ""
			for id, t := range threads {
				if oldest == "" || t.at.Before(threads[oldest].at) {
					oldest = id
				}
			}
			delete(threads, oldest)
			s.n--
		}
		s.n++
	}
	threads[thread] = e
	if s.n > siblingsCap {
		s.trimLocked()
	}
}

// trimLocked drops the oldest entries down to siblingsTrimTo, so the sort runs once for each
// tenth of the cap.
func (s *siblingStore) trimLocked() {
	type aged struct {
		session, thread string
		at              time.Time
	}
	all := make([]aged, 0, s.n)
	for sess, threads := range s.sessions {
		for id, e := range threads {
			all = append(all, aged{sess, id, e.at})
		}
	}
	slices.SortFunc(all, func(a, b aged) int { return a.at.Compare(b.at) })
	for _, a := range all[:len(all)-siblingsTrimTo] {
		threads := s.sessions[a.session]
		delete(threads, a.thread)
		if len(threads) == 0 {
			delete(s.sessions, a.session)
		}
	}
	s.n = siblingsTrimTo
}

// len returns the entries over all sessions. Tests only.
func (s *siblingStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}
