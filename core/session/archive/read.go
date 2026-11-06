package archive

import (
	"cmp"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// PageInfo is what a page says about the archived session as a whole.
type PageInfo struct {
	// TotalEvents is how many events the archive holds for the session.
	TotalEvents int
	// OldestSeq is the first seq of its oldest retained segment.
	OldestSeq uint64
}

// entry is a reader's snapshot of one session from the index.
func (a *Archive) entry(id string) (indexEntry, bool) {
	a.idxMu.RLock()
	defer a.idxMu.RUnlock()
	e, ok := a.index[id]
	if !ok {
		return indexEntry{}, false
	}
	return *e, true
}

// Page calls fn, newest first, for the archived events of id numbered below before — every
// event when before is 0 — until limit events or fn returns false. ok is false when the archive
// holds nothing under id.
//
// RUNS ON THE CALLER'S GOROUTINE against the index snapshot, never on the writer's: decoding a
// segment takes a fraction of a second, and the writer must not queue behind it. The open
// segment reads up to its last flush, at most a tick behind; anything newer is still in memory
// for a resident session.
//
// Memory is bounded by limit: each segment is scanned forward keeping only its newest qualifying
// events, so a page never holds a whole segment of events.
func (a *Archive) Page(id string, before uint64, limit int, fn func(*pipeline.SessionEvent) bool) (PageInfo, bool, error) {
	e, ok := a.entry(id)
	if !ok || len(e.segments) == 0 {
		return PageInfo{}, false, nil
	}
	info := PageInfo{TotalEvents: e.summary.EventCount, OldestSeq: e.segments[0].FirstSeq}
	remaining := limit
	for i := len(e.segments) - 1; i >= 0 && remaining > 0; i-- {
		seg := e.segments[i]
		if before > 0 && seg.FirstSeq >= before {
			continue
		}
		newest := newLastN(remaining)
		_, err := readSegment(filepath.Join(e.dir, seg.File), func(ev *pipeline.SessionEvent) bool {
			if before > 0 && ev.Seq >= before {
				return false // a segment's seqs only rise: nothing after this qualifies
			}
			newest.push(ev)
			return true
		})
		if errors.Is(err, fs.ErrNotExist) {
			continue // pruned or renamed since the snapshot; the rest of the page still stands
		}
		if err != nil {
			return info, true, err
		}
		evs := newest.items()
		for j := len(evs) - 1; j >= 0; j-- {
			evs[j].SessionID = id
			if !fn(evs[j]) {
				return info, true, nil
			}
			remaining--
		}
	}
	return info, true, nil
}

// Event returns the archived event of id numbered seq. ok is false when the archive does not hold
// it — never a neighbour in its place.
func (a *Archive) Event(id string, seq uint64) (*pipeline.SessionEvent, bool, error) {
	path, ok := a.segmentOf(id, seq)
	if !ok {
		return nil, false, nil
	}
	var found *pipeline.SessionEvent
	_, err := readSegment(path, func(ev *pipeline.SessionEvent) bool {
		if ev.Seq == seq {
			found = ev
		}
		return ev.Seq < seq
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil || found == nil {
		return nil, false, err
	}
	found.SessionID = id
	return found, true, nil
}

// Capture returns the request the archived event of id numbered seq captured, rebuilt byte for
// byte and checked against its hash. ok is false when the archive does not hold the event or the
// event has no capture: it is not a request, or it was written before lossless capture. err is
// set when the capture is there but cannot be rebuilt exactly.
func (a *Archive) Capture(id string, seq uint64) (*pipeline.RequestCapture, bool, error) {
	path, ok := a.segmentOf(id, seq)
	if !ok {
		return nil, false, nil
	}
	c, err := readCapture(path, seq)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil || c == nil {
		return nil, false, err
	}
	return c, true, nil
}

// segmentOf is the path of the segment of id whose range holds seq, newest first.
func (a *Archive) segmentOf(id string, seq uint64) (string, bool) {
	e, ok := a.entry(id)
	if !ok {
		return "", false
	}
	for i := len(e.segments) - 1; i >= 0; i-- {
		seg := e.segments[i]
		open := e.open && i == len(e.segments)-1
		if seq < seg.FirstSeq || (!open && seq > seg.LastSeq) {
			continue
		}
		return filepath.Join(e.dir, seg.File), true
	}
	return "", false
}

// Summaries returns every archived session's summary, most recently updated first: the same
// figures ListSessions reports for a resident session, from the archive's fold. Resident and
// the live-only fields are left for the caller.
func (a *Archive) Summaries() []session.SessionSummary {
	a.idxMu.RLock()
	out := make([]session.SessionSummary, 0, len(a.index))
	for _, e := range a.index {
		out = append(out, e.summary)
	}
	a.idxMu.RUnlock()
	slices.SortFunc(out, func(x, y session.SessionSummary) int {
		return cmp.Compare(y.UpdatedAt.UnixNano(), x.UpdatedAt.UnixNano())
	})
	return out
}

// Prior implements session.PriorKeeper: the fold of what id held before the store entry
// numbering after `after` began. ok only when the writer has marked that entry's start, because
// exactly then is before the history up to `after` and nothing of the entry's: begin runs in queue
// order, after every earlier event and before any of the entry's. Until the writer takes the
// entry's first event, and wherever it could mark none, the answer is false, and the store shows
// the entry's own figures — an undercount, never a double count.
//
// A memory lookup under idxMu, safe under the store's read lock because idxMu is a LEAF lock: it
// may be taken while the store's read lock is held, as here, but nothing is ever acquired while
// idxMu is held, so no lock cycle can pass through it.
func (a *Archive) Prior(sessionID string, after uint64) (session.Prior, bool) {
	a.idxMu.RLock()
	e := a.index[sessionID]
	a.idxMu.RUnlock()
	if e == nil || e.before == nil || e.startAfter != after {
		return session.Prior{}, false
	}
	return session.Prior{Fold: e.before, CreatedAt: e.summary.CreatedAt}, true
}

// lastN keeps the newest n events pushed into it.
type lastN struct {
	buf  []*pipeline.SessionEvent
	next int
	full bool
}

func newLastN(n int) *lastN { return &lastN{buf: make([]*pipeline.SessionEvent, n)} }

func (l *lastN) push(e *pipeline.SessionEvent) {
	if len(l.buf) == 0 {
		return
	}
	l.buf[l.next] = e
	l.next = (l.next + 1) % len(l.buf)
	if l.next == 0 {
		l.full = true
	}
}

// items returns what it holds, oldest first.
func (l *lastN) items() []*pipeline.SessionEvent {
	if !l.full {
		return slices.Clone(l.buf[:l.next])
	}
	return append(slices.Clone(l.buf[l.next:]), l.buf[:l.next]...)
}

var _ session.History = (*Archive)(nil)

// earlierPage is how many events Earlier asks Page for at a time, which bounds the events one
// read holds.
var earlierPage = 500

// Earlier implements session.History: Page after Page, each below the oldest event the last
// one returned, until fn returns false or the session's oldest archived event.
func (a *Archive) Earlier(id string, before uint64, fn func(*pipeline.SessionEvent) bool) error {
	for {
		n, last, stopped := 0, uint64(0), false
		_, ok, err := a.Page(id, before, earlierPage, func(e *pipeline.SessionEvent) bool {
			n, last = n+1, e.Seq
			stopped = !fn(e)
			return !stopped
		})
		// last <= 1: nothing is numbered below it, and a before of 0 would start over.
		if err != nil || !ok || stopped || n < earlierPage || last <= 1 {
			return err
		}
		before = last
	}
}
