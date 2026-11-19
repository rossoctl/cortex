package archive

import (
	"log/slog"
	"path/filepath"
	"time"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session"
)

// THE WRITER OWNS THE OPT-OUT (#1923). A session is withheld when its fold is marked noRecord,
// and only the writer goroutine marks a fold, in the same FIFO order as renames, deletes, clears
// and prune, so they cannot reorder against it.
//
// AN OPT-OUT IS NEVER LOST, AND NO PRODUCER WAITS FOR IT. A flagged event of a conversation, or a
// Withhold, is queued like any op when there is room below the reserve. When there is not (the
// writer is a full queue behind, e.g. on a stalled disk), it is kept instead in optOuts, tagged
// with the position of the last op queued before it, and the writer applies it at that place: after
// every op queued before it, before every op queued after it (applyOptOuts). So it lands where it
// would have been in the queue: after a DELETE, CLEAR or rename queued earlier, and before the
// session's later rows, which the mark then withholds. The producer, which holds the session
// store's write lock, never waits, so a stalled writer never stalls other sessions' traffic.
//
// optOuts holds one entry per session id, removed when the writer applies it. It grows only while
// the queue is full, and is bounded at as many ids as the queue holds ops (a.depth). Past that the
// archive FAILS CLOSED: until a restart it records no event of a conversation, because it no longer
// knows which ones asked not to be recorded. On a single-user laptop that many opted-out sessions
// during one stall means forged session ids.

// withhold keeps session id out of the archive from now on. It runs on the writer goroutine for an
// event that asked not to be recorded (SessionEvent.NoRecord, which listeners set only for a
// request that names its own session), for an opNoRecord (Withhold), for a kept opt-out
// (applyOptOuts), and for every later event of a session already withheld, flagged or not: a
// session's header-less calls (telemetry, say) are filed to it by process or client affinity and
// carry no flag.
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
// from its next event on, for rows that arrive unflagged (praxis mode's /v1/ingest). Its opt-out
// is queued, or kept at its place when the queue is full, so it precedes those rows, because the
// store calls this before the request is hopped. It runs under the store's write lock and never
// waits.
//
// Every time, also for a session the writer has marked already: a DELETE, CLEAR or prune still
// queued may be about to remove that mark. Withholding a marked session is cheap.
func (a *Archive) Withhold(sessionID string) {
	if a.closed.Load() || !session.IsConversation(sessionID) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.ops) >= cap(a.ops)-reserve || !a.sendLocked(op{kind: opNoRecord, id: sessionID}) {
		a.optOutLocked(sessionID)
	}
}

// optOutLocked keeps the opt-out of id, which found no room in the queue, at its place: after the
// last op queued so far (a.sent). The caller holds a.mu.
//
// One entry per id. A later opt-out of the same id adds a position only when a rename, delete or
// clear was queued after the entry's last one (a.marks): that op may move or remove the mark, so
// the opt-out must apply again after it, as it would from the queue. Otherwise the mark the entry
// already applies covers it. So an entry holds at most one position per such op still queued.
func (a *Archive) optOutLocked(id string) {
	if a.stopped {
		a.dropped.Add(1)
		slog.Warn("session archive: closed before a request that asked not to be recorded reached the writer; nothing of it was written")
		return
	}
	tags, ok := a.optOuts[id]
	switch {
	case !ok:
		if len(a.optOuts) >= a.depth {
			if !a.failClosed.Swap(true) {
				slog.Error("session archive: more sessions asked not to be recorded, while the writer was behind, than it keeps track of; " +
					"recording no conversation until a restart")
			}
			a.dropped.Add(1)
			return
		}
		if a.optOuts == nil {
			a.optOuts = map[string][]uint64{}
		}
		a.optOuts[id] = []uint64{a.sent}
		a.optOutN.Add(1)
	case tags[len(tags)-1] < a.marks:
		a.optOuts[id] = append(tags, a.sent)
	}
}

// applyOptOuts withholds every kept opt-out that is due: its position is at or before a.at, the
// last positioned op the writer took, or, with all, every one (Close, once nothing more is queued).
// It runs on the writer goroutine before each op, so an opt-out applies before the first op queued
// after it, and on each tick, for one with no op behind it.
func (a *Archive) applyOptOuts(all bool) {
	if a.optOutN.Load() == 0 {
		return
	}
	var due []string
	a.mu.Lock()
	for id, tags := range a.optOuts {
		i := 0
		for i < len(tags) && (all || tags[i] <= a.at) {
			i++
		}
		if i == 0 {
			continue
		}
		due = append(due, id)
		if i == len(tags) {
			delete(a.optOuts, id)
			a.optOutN.Add(-1)
		} else {
			a.optOuts[id] = tags[i:]
		}
	}
	a.mu.Unlock()
	now := a.now()
	for _, id := range due {
		a.withhold(id, a.sessions[id], now)
	}
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
// derived from the prompt outlives them, and keeps only the mark. The summary of what the session
// held when the store's entry began (before) goes too, for the same reason: the store's list row
// merges it in. prune calls it for a marker it keeps.
func (a *Archive) markerOnly(s *sessionState) {
	if onlyMark(s.meta.Summary, s.meta.ID) && (s.before == nil || onlyMark(s.before, s.meta.ID)) {
		return // already only a marker: no rewrite on every prune
	}
	fold := session.NewSummaryFold()
	fold.MarkNoRecord()
	s.meta.Summary, s.metaDirty = fold, true
	if s.before != nil {
		s.before = fold.Clone()
	}
	a.publish(s)
}

// onlyMark reports whether f, a fold of session id, holds nothing derived from content.
func onlyMark(f *session.SummaryFold, id string) bool {
	sum := f.Summary(id)
	return sum.EventCount == 0 && sum.Title == ""
}

// keepMarker reports whether prune keeps s, a session with nothing on disk, because it marks a
// withheld session. A marker lives retainDays after the session's last event, like a segment:
// after that, the agent's own transcript has usually expired too. Open makes retainDays positive.
func keepMarker(s *sessionState, now time.Time, retainDays int) bool {
	return s.meta.Summary.NoRecord() && now.Sub(s.meta.UpdatedAt) < time.Duration(retainDays)*24*time.Hour
}
