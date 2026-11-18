package archive

import (
	"path/filepath"
	"time"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session"
)

// THE WRITER OWNS THE OPT-OUT (#1923). A session is withheld when its fold is marked noRecord,
// and only the writer goroutine marks a fold, in the same FIFO as renames, deletes, clears and
// prune, so they cannot reorder against it. There is no other copy of the state: RecordCapture
// never drops a flagged event of a session the writer has not marked yet, and Withhold never
// drops its op (deliver), so nothing has to be remembered elsewhere until the writer gets to it.

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
	if a.closed.Load() || !session.IsConversation(sessionID) || a.markedInIndex(sessionID) {
		return
	}
	a.deliver(op{kind: opNoRecord, id: sessionID})
}

// deliver queues o and, when the queue is full, waits for the writer rather than drop it. Only
// for the ops an opt-out depends on. The caller holds the store's write lock and must not hold
// a.mu: the writer takes a.mu, and it never takes the store's lock, so it always makes room. It
// gives up only when the archive closes.
func (a *Archive) deliver(o op) {
	select {
	case a.ops <- o:
		return
	default:
	}
	select {
	case a.ops <- o:
	case <-a.done:
		a.releaseCapture(o.cap)
	}
}

// markedInIndex reports whether the writer has published id as withheld. A false answer can be
// stale (a mark not published yet), which only makes the caller deliver an op it could have
// dropped.
func (a *Archive) markedInIndex(id string) bool {
	a.idxMu.RLock()
	defer a.idxMu.RUnlock()
	e := a.index[id]
	return e != nil && e.summary.NoRecord
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

// keepMarker reports whether prune keeps s, a session with nothing on disk, because it marks a
// withheld session. A marker lives retainDays after the session's last event, like a segment:
// after that, the agent's own transcript has usually expired too. Open makes retainDays positive.
func keepMarker(s *sessionState, now time.Time, retainDays int) bool {
	return s.meta.Summary.NoRecord() && now.Sub(s.meta.UpdatedAt) < time.Duration(retainDays)*24*time.Hour
}
