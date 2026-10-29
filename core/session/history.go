package session

import (
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// SeqSeeder is optionally implemented by a Recorder that keeps a session's events for longer
// than the store does — the session archive. When the store creates an entry it asks every
// SeqSeeder, under its write lock, for the last seq it holds under that id, and numbers the
// entry's events after the highest answer.
//
// Without it a session re-created under the same id — after max_sessions evicted it, a ttl
// expired it, or the proxy restarted — numbers its events from 1 again, so one session
// would carry two events numbered 1: one on disk, one in memory. A pager walking ?before=
// across that point would loop.
//
// LastSeq is called under the store's write lock, in front of live traffic: it must be a
// memory lookup, and must not block or call back into the store.
type SeqSeeder interface {
	LastSeq(sessionID string) uint64
}

// archivedSeqLocked is the highest seq any SeqSeeder holds for id, 0 when none does. s.mu held.
func (s *Store) archivedSeqLocked(id string) uint64 {
	var last uint64
	for _, r := range s.recorders {
		if sd, ok := r.(SeqSeeder); ok {
			if n := sd.LastSeq(id); n > last {
				last = n
			}
		}
	}
	return last
}

// EntryStarter is optionally implemented by a SeqSeeder. The store calls EntryStarted under its
// write lock each time it creates an entry for sessionID, with the seq it numbers that entry's
// events after. A rename of the entry carries the events numbered after it; those at or below
// it were recorded under sessionID before this entry existed, and stay there. Like LastSeq, it
// must be a memory update.
type EntryStarter interface {
	EntryStarted(sessionID string, after uint64)
}

// entryStartedLocked tells every EntryStarter that an entry for id numbers after `after`. s.mu held.
func (s *Store) entryStartedLocked(id string, after uint64) {
	for _, r := range s.recorders {
		if es, ok := r.(EntryStarter); ok {
			es.EntryStarted(id, after)
		}
	}
}

// Clearer is optionally implemented by a Recorder that keeps state keyed by session id: the
// usage aggregator's per-session figures, the archive's numbering and files. Clear tells each one,
// under the store's write lock and after the store has dropped every session, so no append can
// land between the two halves and be kept by one side only.
//
// Cleared runs in front of live traffic, like Record: it must not block. Work that takes time —
// the archive's file deletion — is queued for the Clearer's own goroutine.
type Clearer interface {
	Cleared()
}

// Clear removes every session and the active-session mark,
// tells every Clearer, and reports how many sessions it removed.
// The next append to any id starts a new session, numbered from 1 once the archive has reset.
//
// Subscribers stay attached: a clear erases history, it does not end the stream.
func (s *Store) Clear() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.sessions)
	s.sessions = make(map[string]*entry)
	s.activeID = ""
	// nil, as a store nothing has claimed from holds them; every writer allocates on first use.
	for _, r := range s.recorders {
		if c, ok := r.(Clearer); ok {
			c.Cleared()
		}
	}
	return n
}

// Prior is a session's history from before a store entry began: what the session archive folded
// under the id up to the seq the entry numbers after.
type Prior struct {
	// Fold is the earlier history's figures. SHARED AND IMMUTABLE: the keeper hands out the copy it
	// published, so a caller reads it and never Adds to it.
	Fold *SummaryFold
	// CreatedAt is when the keeper first saw the session; zero when it does not know.
	CreatedAt time.Time
}

// PriorKeeper is optionally implemented by a Recorder that keeps a session's history across the
// store's restarts and evictions — the session archive. For every entry numbered after earlier
// history (a SeqSeeder answered above zero when the entry was created), ListSessions asks for that
// history and reports the session's row as the two together, so that — with session.max_events
// unset, the default — a restart, a ttl or max_sessions changes nothing a list shows. With a cap
// the entry's own figures shed what a trim drops while the archive's fold keeps it, so a restart
// can raise them.
//
// ok is false when the keeper cannot say EXACTLY what came before, and the row then shows the
// entry's own figures. That undercount is the safe side: the keeper records every event the store
// appends, so anything but the history from before the entry would count the entry twice.
//
// Prior is called under the store's READ lock, on agentop's two-second poll: it must be a memory
// lookup, must not block, and must not call back into the store.
type PriorKeeper interface {
	Prior(sessionID string, after uint64) (Prior, bool)
}

// History is the events a session recorded on disk, the session archive's, for a plugin that
// needs the events from before this process: a store entry starts empty after a restart.
type History interface {
	// Earlier calls fn, newest first, for the events recorded under sessionID numbered below
	// before, every one when before is 0, until fn returns false or none are left. It reads
	// from disk.
	Earlier(sessionID string, before uint64, fn func(*pipeline.SessionEvent) bool) error
}

// HistoryConsumer is implemented by plugins that read a session's archived events.
// plugins.BuildWithDeps injects the process's History before Configure runs; a process with no
// session archive passes nothing.
type HistoryConsumer interface {
	SetHistory(History)
}

// priorLocked is the first PriorKeeper's answer for id's entry numbered after `after`. s.mu held.
func (s *Store) priorLocked(id string, after uint64) (Prior, bool) {
	for _, r := range s.recorders {
		if pk, ok := r.(PriorKeeper); ok {
			if p, ok := pk.Prior(id, after); ok && p.Fold != nil {
				return p, true
			}
		}
	}
	return Prior{}, false
}

// withPriorLocked adds to sum — sess's row as ListSessions built it — the history id held before
// sess began. Each figure merges by the rule that folds it, so the row is what a store that never
// lost the session would report. s.mu held.
func (s *Store) withPriorLocked(sum *SessionSummary, id string, sess *entry) {
	if sess.after == 0 {
		return
	}
	p, ok := s.priorLocked(id, sess.after)
	if !ok {
		return
	}
	f := p.Fold
	sum.EventCount += f.events
	sum.TotalTokens += f.tokens
	// CostSum's addition for the sums, OR for the flags: a total that clamped on either side is a
	// bound, whichever side it was.
	cost, avoided := sess.cost, sess.avoided
	cost.Add(f.cost.Micros)
	avoided.Add(f.avoided.Micros)
	cost.Saturated = cost.Saturated || f.cost.Saturated
	avoided.Saturated = avoided.Saturated || f.avoided.Saturated
	sum.CostMicros, sum.AvoidedMicros = cost.Micros, avoided.Micros
	sum.Saturated = cost.Saturated || avoided.Saturated
	// THE UNIT COUNTS, NOT THE RENDERED LISTS: currenciesOf renders dollars-only as nil, so a union
	// of two rendered lists would drop "USD" from a session priced in dollars on one side only.
	if len(f.units) > 0 {
		units := make(map[string]int, len(f.units)+len(sess.units))
		for u, n := range sess.units {
			units[u] += n
		}
		for u, n := range f.units {
			units[u] += n
		}
		sum.Currencies = currenciesOf(units)
	}
	// The earlier history's title as the one held and the entry's as the candidate. applyTitle
	// then gives what folding both in order would, because the entry's title is itself the first
	// at its best rank, or its latest /rename.
	_, sum.Title = applyTitle(f.titleRank, f.title, sess.titleRank, sess.Title)
	owner := sess.owner
	if f.owner != "" {
		owner = f.owner
	}
	sum.Agent = pickAgent(id, owner, f.agents, sess.agents)
	if !p.CreatedAt.IsZero() && p.CreatedAt.Before(sum.CreatedAt) {
		sum.CreatedAt = p.CreatedAt
	}
	sum.PromptContext = pipeline.MergePromptContext(f.promptContext(), sum.PromptContext)
	// The entry's own when it has one: everything it holds came after the history. When it has
	// none the row says so, since a request the entry holds is what a router's pin is made by.
	if sum.InferenceHost == "" && f.inferenceHost != "" {
		sum.InferenceHost, sum.InferenceHostFromHistory = f.inferenceHost, true
	}
	// The processes that named the session before this entry still named it, so a resumed
	// session links calls a process made before the restart.
	if len(f.procs) > 0 {
		sum.procSpans = mergeProcs(f.procs, sum.procSpans)
		sum.Procs = procsList(sum.procSpans)
	}
	if f.lastRequestAt.After(sum.LastRequestAt) {
		sum.LastRequestAt = f.lastRequestAt
	}
}
