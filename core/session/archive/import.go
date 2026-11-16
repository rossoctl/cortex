package archive

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session"
)

// ErrLive and ErrExists are the refusals of WriteSession.
var (
	ErrLive   = errors.New("session archive: the session has live events")
	ErrExists = errors.New("session archive: the session is already archived")
)

var errClosed = errors.New("the session archive is closed")

// Lookup returns the archived summary of id from the reader index.
func (a *Archive) Lookup(id string) (session.SessionSummary, bool) {
	e, ok := a.entry(id)
	return e.summary, ok
}

// WriteSession writes events as the whole session id and returns when they are on disk
// (segment closed and fsynced, session.json written, bytes refreshed). It numbers Seq from 1 in
// order and sets SessionID. With replace, it first deletes the archived session in the same
// writer op, so no live event lands between.
//
// The refusals are decided on the writer goroutine, after every event queued before the call:
//   - ErrLive when the archive holds id without the imported mark, or when the store numbered an
//     event for id that the archive does not hold yet (a live event is queued or was dropped).
//   - ErrExists when replace is false and the archive holds id.
//
// A closed archive returns an error.
func (a *Archive) WriteSession(id string, events []model.SessionEvent, replace bool) error {
	if len(events) == 0 {
		return errors.New("session archive: no events to write")
	}
	if a.closed.Load() {
		return errClosed
	}
	var err error
	o := op{kind: opFunc, done: make(chan struct{}), f: func() { err = a.writeSession(id, events, replace) }}
	// The writer stops reading ops at Close, so each wait also watches a.done.
	select {
	case a.ops <- o:
	case <-a.done:
		return errClosed
	}
	select {
	case <-o.done:
		return err
	case <-a.done:
		return errClosed
	}
}

// writeSession is WriteSession on the writer goroutine.
func (a *Archive) writeSession(id string, events []model.SessionEvent, replace bool) error {
	s := a.sessions[id]
	// The check and the reservation are one critical section: a live Record after it numbers
	// after the import (SeqSeeder), never from 1.
	a.mu.Lock()
	old := a.lastSeq[id]
	switch {
	case s == nil && old > 0:
		a.mu.Unlock()
		return ErrLive
	case s != nil && (s.meta.Summary.Summary(id).Source == "" || old > s.meta.lastSeq()):
		// old past what is on disk is a live event queued for an imported session: a replace
		// would number its own events into that event's seq.
		a.mu.Unlock()
		return ErrLive
	case s != nil && !replace:
		a.mu.Unlock()
		return ErrExists
	}
	reserved := uint64(len(events))
	a.lastSeq[id] = reserved
	a.mu.Unlock()

	// restore gives the numbering back, unless a live event has taken it since.
	restore := func(err error) error {
		a.mu.Lock()
		if a.lastSeq[id] == reserved {
			if old == 0 {
				delete(a.lastSeq, id)
			} else {
				a.lastSeq[id] = old
			}
		}
		a.mu.Unlock()
		return err
	}
	// deleteSession gets an EMPTY seqs map, so that its forgetSeqs keeps the numbering reserved
	// above.
	del := func() error {
		p := &pendingClear{done: make(chan struct{}), seqs: map[string]uint64{}}
		a.deleteSession(id, p)
		return p.err
	}
	// fail erases what part of the session reached disk, then restores.
	fail := func(err error) error {
		if derr := del(); derr != nil {
			// The partial copy stays on disk, so its seqs stay reserved.
			return errors.Join(err, derr)
		}
		return restore(err)
	}
	// Paused is checked before the replace delete: a paused write must not erase the old copy.
	if a.paused.Load() {
		return restore(errors.New("session archive: paused after a full disk"))
	}
	if s != nil {
		// The ledger holds the old copy's turns. If the write below fails or the process stops, the
		// record keeps the next import from counting them again.
		a.forget([]*sessionState{s})
		if err := del(); err != nil {
			return restore(err)
		}
		// The old copy is gone from disk, so a failure below must not restore its numbering.
		old = 0
	}

	// write counts its failures instead of returning them; a change in the count is this
	// write's failure, since only the writer goroutine (this one) counts.
	errs := a.writeErrors.Load()
	for i := range events {
		e := events[i]
		e.Seq, e.SessionID = uint64(i+1), id
		a.write(id, &e, nil)
		if a.writeErrors.Load() != errs || a.paused.Load() {
			return fail(errors.New("session archive: the write failed; see the log"))
		}
	}
	s = a.sessions[id]
	a.closeWriter(s)
	// Retention and the usage replay judge a segment by LastWrite: for an import that is its
	// newest event, not the import time.
	for i := range s.meta.Segments {
		seg := &s.meta.Segments[i]
		if seg.FirstSeq < 1 || seg.LastSeq > uint64(len(events)) {
			continue
		}
		var t time.Time
		for _, e := range events[seg.FirstSeq-1 : seg.LastSeq] {
			if e.At.After(t) {
				t = e.At
			}
		}
		if !t.IsZero() {
			seg.LastWrite = t
			// A reconcile reads the mtime when session.json does not match the segment file.
			_ = os.Chtimes(filepath.Join(s.dir, seg.File), t, t)
		}
	}
	// The session list sorts by UpdatedAt: an import must sort by its events, not the import time.
	var first, last time.Time
	for i := range events {
		if t := events[i].At; !t.IsZero() {
			if first.IsZero() || t.Before(first) {
				first = t
			}
			if t.After(last) {
				last = t
			}
		}
	}
	if !last.IsZero() {
		s.meta.CreatedAt, s.meta.UpdatedAt = first, last
	}
	a.writeMeta(s)
	if a.writeErrors.Load() != errs {
		return fail(errors.New("session archive: the write failed; see the log"))
	}
	a.publish(s)
	a.refreshBytes()
	return nil
}
