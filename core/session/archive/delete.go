package archive

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// Deleted implements session.Deleter: one session's history goes, in its place in the queue —
// events queued before the delete are deleted with it, events after it start fresh. It never
// blocks; AwaitDelete reports how it went.
//
// THE NUMBERING IS KEPT UNTIL THE FILES ARE GONE, for the reason Cleared gives: a session
// re-created under the id before then numbers on from it rather than writing a second event 1
// beside files still on disk. The store's entry start is dropped at once: the entry is gone, and
// a new one sets its own through EntryStarted.
func (a *Archive) Deleted(sessionID string) {
	p := &pendingClear{done: make(chan struct{})}
	a.mu.Lock()
	p.seqs = map[string]uint64{sessionID: a.lastSeq[sessionID]}
	delete(a.started, sessionID)
	if a.deletes == nil {
		a.deletes = map[string]*pendingClear{}
	}
	a.deletes[sessionID] = p
	a.mu.Unlock()
	if a.closed.Load() {
		p.finish(ClearResult{}, errors.New("the session archive is closed"))
		return
	}
	// Into the reserve, like a rename and a clear: Record stops short of it.
	select {
	case a.ops <- op{kind: opDelete, id: sessionID, clear: p}:
	default:
		slog.Error("session archive: a delete was lost to a full queue; the session's history on disk is unchanged",
			"session", sessionID)
		p.finish(ClearResult{}, errors.New("the session archive's queue is full; nothing on disk was erased"))
	}
}

// AwaitDelete waits up to timeout for the most recent delete of sessionID to finish and reports
// it. With none asked for it reports nothing removed.
//
// The entry is dropped here, once read, not by the writer when it finishes: the API asks right
// after Deleted, and a writer that dropped it first would make a delete that worked read as one
// that found nothing.
func (a *Archive) AwaitDelete(sessionID string, timeout time.Duration) (ClearResult, error) {
	a.mu.Lock()
	p := a.deletes[sessionID]
	a.mu.Unlock()
	if p == nil {
		return ClearResult{}, nil
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-p.done:
		a.mu.Lock()
		if a.deletes[sessionID] == p {
			delete(a.deletes, sessionID)
		}
		a.mu.Unlock()
		return p.res, p.err
	case <-t.C:
		return ClearResult{}, fmt.Errorf("the session archive had not finished erasing the session after %s; it is still queued", timeout)
	}
}

// deleteSession is a delete on the writer goroutine. The session's open segment is finished
// first; then ONE rename moves its directory aside, and only after that is anything forgotten. A
// crash leaves either the session's history, untouched, or a trash directory the next Open
// deletes. A failed rename changes nothing on disk, so it forgets nothing either.
func (a *Archive) deleteSession(id string, p *pendingClear) {
	s := a.sessions[id]
	if s == nil {
		a.forgetSeqs(p.seqs)
		p.finish(ClearResult{}, nil)
		return
	}
	a.closeWriter(s)
	var n int64
	for _, seg := range s.meta.Segments {
		n += seg.Bytes
	}
	// The wall clock, not a.now: this only has to be a name no earlier trash holds.
	trash := filepath.Join(a.root, fmt.Sprintf("%s%d", trashPrefix, time.Now().UnixNano()))
	if err := os.Rename(s.dir, trash); err != nil {
		slog.Warn("session archive: could not erase a session; its history on disk is unchanged",
			"session", id, "error", err)
		p.finish(ClearResult{}, fmt.Errorf("could not move the session's history aside: %w", err))
		return
	}
	delete(a.sessions, id)
	a.unpublish(id)
	a.forgetSeqs(p.seqs)
	if err := os.RemoveAll(trash); err != nil {
		slog.Warn("session archive: erased a session, but its trash was not fully deleted; the next start deletes it",
			"dir", trash, "error", err)
	}
	a.refreshBytes()
	p.finish(ClearResult{Sessions: 1, Bytes: n}, nil)
}
