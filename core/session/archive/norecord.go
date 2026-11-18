package archive

import (
	"log/slog"
	"path/filepath"
	"time"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session"
)

// THE WRITER OWNS THE OPT-OUT (#1923). A session is withheld when its fold is marked noRecord,
// and only the writer goroutine marks a fold, in the same FIFO as renames, deletes, clears and
// prune, so they cannot reorder against it. There is no other copy of the state: RecordCapture
// never drops a flagged event of a conversation, and Withhold never drops its op (deliver), so
// nothing has to be remembered elsewhere until the writer gets to it.

// withhold keeps session id out of the archive from now on. It runs on the writer goroutine for an
// event that asked not to be recorded (SessionEvent.NoRecord, which listeners set only for a
// request that names its own session), for an opNoRecord (Withhold), and for every later event of
// a session already withheld, flagged or not: a session's header-less calls (telemetry, say) are
// filed to it by process or client affinity and carry no flag.
//
// FROM NOW ON, NEVER BACKWARDS. What the archive already holds for id stays: it was recorded
// before the opt-out, and the session-id header that names id is client-asserted, so a request
// must not be able to erase another session's history by naming it. The open segment is closed,
// so nothing more is written to it, and the session is marked. Erasing is the user's explicit
// DELETE /v1/sessions/{id}.
//
// For an id the archive does not hold, withhold writes a content-free marker: a session.json with
// no segments whose summary says noRecord and nothing else. The event itself is never folded,
// because the fold reads content (the title comes from the prompt). The mark is what keeps the
// opt-out after a restart, and what makes an import refuse id (WriteSession returns ErrLive).
//
// metaDirty is set before the write, so a failed write is retried at the next metaEvery and at
// shutdown, and Close reports a marker it could not write.
func (a *Archive) withhold(id string, s *sessionState, now time.Time) {
	if s != nil && s.meta.Summary.NoRecord() {
		// Already marked. A marker's age is measured from the session's last event
		// (keepMarker), so it is renewed, and written with the next metaEvery.
		s.meta.UpdatedAt, s.metaDirty = now, true
		return
	}
	if s == nil {
		fold := session.NewSummaryFold()
		s = &sessionState{
			dir:  filepath.Join(a.data, dirName(id)),
			meta: &sessionMeta{ID: id, CreatedAt: now, Summary: fold},
		}
		a.sessions[id] = s
	} else {
		a.closeWriter(s)
	}
	s.meta.Summary.MarkNoRecord()
	s.meta.UpdatedAt, s.lastWrite, s.metaDirty = now, now, true
	a.writeMeta(s)
	a.publish(s)
}

// Withhold implements session.Withholder: the store tells the archive that sessionID is withheld
// from its next event on, for rows that arrive unflagged (praxis mode's /v1/ingest). The op it
// queues is never dropped (deliver), and it precedes those rows in the FIFO, because the store
// calls this before the request is hopped. It runs under the store's write lock.
func (a *Archive) Withhold(sessionID string) {
	if a.closed.Load() || !session.IsConversation(sessionID) {
		return
	}
	// Every time, also for a session the writer has marked already: a DELETE, CLEAR or prune
	// still queued may be about to remove that mark. Withholding a marked session is cheap.
	a.deliver(op{kind: opNoRecord, id: sessionID})
}

// deliver queues o, an op an opt-out depends on, and never drops it: when the queue has no room
// below the reserve it waits for the writer. The reserve stays for renames, deletes and clears,
// which must not be crowded out by flagged events.
//
// PRIVACY OVER LATENCY, a deliberate trade: while the archive is saturated (its writer a full
// queue behind, e.g. on a stalled disk) a request of an opted-out session waits here, without
// bound if the disk never recovers. An unsaturated archive never waits.
//
// No deadlock: the caller holds the store's write lock and must not hold a.mu. The writer takes
// a.mu and never the store's lock, so it keeps draining, also while paused. Only producers that
// hold the store's lock send, so once there is room below the reserve the send cannot block, and
// the FIFO order of the store's appends holds. It gives up only when the archive closes, and
// says so.
func (a *Archive) deliver(o op) {
	for len(a.ops) >= cap(a.ops)-reserve {
		if a.deliverWait != nil {
			a.deliverWait() // tests: the producer is waiting for room
		}
		select {
		case <-a.done:
			a.undelivered(o)
			return
		case <-time.After(time.Millisecond):
		}
	}
	// The send and the writer's stop are ordered by a.mu: an op is either queued before the
	// writer's last drain, and taken by it, or refused here and counted. Non-blocking: there is
	// room below the reserve, and no other producer sends while the store's lock is held.
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		a.undelivered(o)
		return
	}
	select {
	case a.ops <- o:
		a.mu.Unlock()
	default:
		a.mu.Unlock()
		a.undelivered(o)
	}
}

// undelivered counts and reports an opt-out op lost to the archive closing. No session id: it
// comes from a client's request header.
func (a *Archive) undelivered(o op) {
	a.releaseCapture(o.cap)
	a.dropped.Add(1)
	slog.Warn("session archive: closed before a request that asked not to be recorded reached the writer; nothing of it was written")
}

// withheld reports whether e, an event of id, is to be withheld: it asks it, or the session is
// marked. Never for a shared bucket: the store already clears the flag there, and the archive
// does not trust that alone (session.IsConversation).
func (a *Archive) withheld(id string, s *sessionState, e *model.SessionEvent) bool {
	if !session.IsConversation(id) {
		return false
	}
	return e.NoRecord || (s != nil && s.meta.Summary.NoRecord())
}

// later returns the later of a and b.
func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// markerOnly reduces s, a withheld session that has nothing left on disk, to its marker: the
// fold drops what the expired segments contributed (title, counts, projects), so nothing
// derived from the prompt outlives them, and keeps only the mark. prune calls it for a marker it
// keeps.
func (a *Archive) markerOnly(s *sessionState) {
	if sum := s.meta.Summary.Summary(s.meta.ID); sum.EventCount == 0 && sum.Title == "" {
		return // already only a marker: no rewrite on every prune
	}
	fold := session.NewSummaryFold()
	fold.MarkNoRecord()
	s.meta.Summary, s.metaDirty = fold, true
	a.publish(s)
}

// keepMarker reports whether prune keeps s, a session with nothing on disk, because it marks a
// withheld session. A marker lives retainDays after the session's last event, like a segment:
// after that, the agent's own transcript has usually expired too. Open makes retainDays positive.
func keepMarker(s *sessionState, now time.Time, retainDays int) bool {
	return s.meta.Summary.NoRecord() && now.Sub(s.meta.UpdatedAt) < time.Duration(retainDays)*24*time.Hour
}
