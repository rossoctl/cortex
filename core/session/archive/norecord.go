package archive

import (
	"log/slog"
	"path/filepath"
	"time"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session"
)

// withhold keeps session id out of the archive from now on (#1923). It runs on the writer
// goroutine for an event that asked not to be recorded (SessionEvent.NoRecord, which listeners
// set only for a request that names its own session), and for every later event of a session
// already withheld, flagged or not: a session's header-less calls (telemetry, say) are filed to
// it by process or client affinity and carry no flag.
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
// opt-out after a restart, when the store's own mark is gone, and what makes an import refuse id
// (WriteSession returns ErrLive for an id the archive holds that is not imported).
//
// noRecord ids are also remembered in a.noRecord, under a.mu, by RecordCapture: an event dropped
// before the writer saw it (queue full, archive paused) must not leave the session unmarked.
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
	s.meta.UpdatedAt, s.lastWrite = now, now
	a.writeMeta(s)
	a.publish(s)
}

// Withhold implements session.Withholder: the store tells the archive that sessionID is withheld
// from its next event on, for rows that arrive unflagged (praxis mode's /v1/ingest). It runs under
// the store's write lock, and never blocks.
func (a *Archive) Withhold(sessionID string) {
	if a.closed.Load() {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.markLocked(sessionID, a.lastSeq[sessionID]+1)
}

// markLocked marks sessionID withheld from Seq from on, in memory at once (a.noRecord), and on
// disk through an opNoRecord for the session's first mark, so a restart before its next event
// keeps it. a.mu held. The op takes the queue's normal capacity, never the reserve that renames,
// deletes and clears rely on: when the queue is full the mark stays in memory, and the session's
// next event writes it (withhold).
func (a *Archive) markLocked(sessionID string, from uint64) {
	if !session.IsConversation(sessionID) {
		return
	}
	old, ok := a.noRecord[sessionID]
	if !ok || from < old {
		a.noRecord[sessionID] = from
	}
	if ok {
		return
	}
	// No session id in these lines: it comes from a client's request header (CodeQL
	// go/clear-text-logging), and the line is about the queue, not the session.
	if len(a.ops) >= cap(a.ops)-reserve {
		slog.Warn("session archive: the queue is full; a session that asked not to be recorded is withheld in memory until its next event")
		return
	}
	select {
	case a.ops <- op{kind: opNoRecord, id: sessionID}:
	default:
		slog.Warn("session archive: the queue is full; a session that asked not to be recorded is withheld in memory until its next event")
	}
}

// withheld reports whether e, an event of id, is to be withheld: it asks it, or the session was
// marked on disk, or a flagged event the writer may not have seen yet came at or before it.
// Events numbered before that flagged one are still written: the opt-out never reaches back.
func (a *Archive) withheld(id string, s *sessionState, e *model.SessionEvent) bool {
	// Never a shared bucket: the store already clears the flag there, and the archive does
	// not trust that alone (session.IsConversation).
	if !session.IsConversation(id) {
		return false
	}
	if e.NoRecord || (s != nil && s.meta.Summary.NoRecord()) {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	from, ok := a.noRecord[id]
	return ok && e.Seq >= from
}

// keepMarker reports whether prune keeps s, a session with nothing on disk, because it marks a
// withheld session. A marker lives retainDays after the session's last event, like a segment:
// after that, the agent's own transcript has usually expired too. Open makes retainDays positive.
func keepMarker(s *sessionState, now time.Time, retainDays int) bool {
	return s.meta.Summary.NoRecord() && now.Sub(s.meta.UpdatedAt) < time.Duration(retainDays)*24*time.Hour
}
