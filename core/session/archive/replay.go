package archive

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// ReplaySet is the segments a startup replay reads, as ReplaySources found them. Read it any time
// after: the set does not change once taken.
type ReplaySet struct {
	cutoff time.Time
	srcs   []replaySource
}

type replaySource struct {
	id, path string
	open     bool
}

// ReplaySources lists every segment that can hold an event at or after cutoff, under the id its
// session has now. A segment last written before cutoff is left out: it can hold nothing newer.
//
// TAKE IT BEFORE ANY LISTENER STARTS, and the set holds exactly what earlier processes wrote: a
// process never appends to a segment it did not open, so nothing this process records can land in
// one of these. That is what lets the read itself run alongside live traffic without counting an
// event twice.
func (a *Archive) ReplaySources(cutoff time.Time) ReplaySet {
	set := ReplaySet{cutoff: cutoff}
	a.idxMu.RLock()
	defer a.idxMu.RUnlock()
	for id, e := range a.index {
		for i, seg := range e.segments {
			if seg.Events > 0 && !seg.LastWrite.Before(cutoff) {
				set.srcs = append(set.srcs, replaySource{id, filepath.Join(e.dir, seg.File), e.open && i == len(e.segments)-1})
			}
		}
	}
	return set
}

// Read calls fn for every event in the set at or after its cutoff that is not a tunnel row, stamped
// with the id its session had when the set was taken. Order is per segment in seq order; segments
// and sessions come in any order, so a caller that needs time order sorts what it collects.
//
// Every segment is decoded whole, since segments record no event times. Each event fn receives is
// the reader's own, freshly decoded; a caller that keeps many should keep a reduced copy, not the
// event (see usage.ReplayCopy).
//
// A segment gone since the set was taken — its session erased, or the archive cleared — is skipped
// and not counted as incomplete. It returns ctx.Err() as soon as ctx is done, checked between
// events, so a deadline bounds it.
//
// shortcut: an erase during the read leaves the all-sessions and per-agent totals short by the
// segments not yet read, which an erase keeps; fix if a LAST 1H after an early erase is reported low.
func (s ReplaySet) Read(ctx context.Context, fn func(sessionID string, e *pipeline.SessionEvent)) (incomplete int, err error) {
	for _, src := range s.srcs {
		if err := ctx.Err(); err != nil {
			return incomplete, err
		}
		var stop error
		truncated, rerr := readSegment(src.path, func(ev *pipeline.SessionEvent) bool {
			if stop = ctx.Err(); stop != nil {
				return false
			}
			if ev.Tunnel || ev.At.IsZero() || ev.At.Before(s.cutoff) {
				return true
			}
			ev.SessionID = src.id
			fn(src.id, ev)
			return true
		})
		switch {
		case stop != nil:
			return incomplete, stop
		case rerr != nil && !errors.Is(rerr, fs.ErrNotExist):
			incomplete++
		case truncated && !src.open:
			incomplete++
		}
	}
	return incomplete, nil
}

// ReplaySince is ReplaySources and Read in one call, for a caller with nothing to start between
// them.
func (a *Archive) ReplaySince(ctx context.Context, cutoff time.Time, fn func(sessionID string, e *pipeline.SessionEvent)) (incomplete int, err error) {
	return a.ReplaySources(cutoff).Read(ctx, fn)
}
