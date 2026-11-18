package archive

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// trashPrefix names the directory a clear moves the archive's data into before deleting it.
const trashPrefix = ".trash-"

// ClearResult is what one clear removed from disk.
type ClearResult struct {
	Sessions int
	Bytes    int64
}

// pendingClear is a clear Cleared queued, until the writer goroutine has carried it out. res and
// err are written before done is closed and read only after.
type pendingClear struct {
	done chan struct{}
	// seqs is the archive's numbering when the clear was asked for. See Cleared.
	seqs map[string]uint64
	res  ClearResult
	err  error
}

func (p *pendingClear) finish(res ClearResult, err error) {
	p.res, p.err = res, err
	close(p.done)
}

// Cleared implements session.Clearer: every session's history goes, in its place in the queue —
// events queued before the clear are deleted with the rest, events after it start fresh. It
// never blocks; AwaitClear reports how it went.
//
// THE NUMBERING IS NOT RESET HERE, though the store asks for it right after this returns. Until
// the writer has emptied the directory, the history is still on disk — and if the rename that
// empties it fails, it stays there. A session re-created under one of its ids and numbered from 1
// would then write a second event 1 into the same directory, the collision SeqSeeder exists to
// prevent. So a re-created session carries on its old numbering, which is harmless: a seq is
// unique within a session, never dense. Only once the clear has succeeded does the writer forget
// the numbering of every id that recorded nothing since, so a session started under one of those
// later numbers from 1.
func (a *Archive) Cleared() {
	p := &pendingClear{done: make(chan struct{})}
	a.mu.Lock()
	p.seqs = maps.Clone(a.lastSeq)
	a.pending = p
	clear(a.noRecord)
	a.mu.Unlock()
	if a.closed.Load() {
		p.finish(ClearResult{}, errors.New("the session archive is closed"))
		return
	}
	// Into the reserve, like a rename: Record stops short of it, so only renames and clears can
	// fill it.
	select {
	case a.ops <- op{kind: opClear, clear: p}:
	default:
		slog.Error("session archive: a clear was lost to a full queue; history on disk is unchanged")
		p.finish(ClearResult{}, errors.New("the session archive's queue is full; nothing on disk was cleared"))
	}
}

// AwaitClear waits up to timeout for the most recent clear to finish and reports it. Clears are
// serialized by the store's lock, and each one empties everything, so the most recent subsumes
// every earlier one still queued. With no clear ever asked for it reports nothing removed.
func (a *Archive) AwaitClear(timeout time.Duration) (ClearResult, error) {
	a.mu.Lock()
	p := a.pending
	a.mu.Unlock()
	if p == nil {
		return ClearResult{}, nil
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-p.done:
		return p.res, p.err
	case <-t.C:
		return ClearResult{}, fmt.Errorf("the session archive had not finished clearing after %s; it is still queued", timeout)
	}
}

// clearAll is a clear on the writer goroutine. Every open segment is finished first, so the files
// are complete whichever way it goes; then ONE rename moves the whole data directory aside, and
// only after that is anything reset or deleted. A crash leaves either the old history, untouched,
// or a trash directory the next Open deletes — never a half-cleared archive. A failed rename
// changes nothing on disk, so it resets nothing either: the history is still served.
func (a *Archive) clearAll(p *pendingClear) {
	for _, s := range a.sessions {
		a.closeWriter(s)
	}
	a.refreshBytes()
	res := ClearResult{Sessions: len(a.sessions), Bytes: a.bytes.Load()}
	a.forget(slices.Collect(maps.Values(a.sessions)))

	// The wall clock, not a.now: this only has to be a name no earlier trash holds.
	trash := filepath.Join(a.root, fmt.Sprintf("%s%d", trashPrefix, time.Now().UnixNano()))
	if err := os.Rename(a.data, trash); err != nil {
		slog.Warn("session archive: could not clear; history on disk is unchanged", "error", err)
		p.finish(ClearResult{}, fmt.Errorf("could not move the archive aside: %w", err))
		return
	}
	a.sessions = map[string]*sessionState{}
	a.idxMu.Lock()
	a.index = map[string]*indexEntry{}
	a.idxMu.Unlock()
	a.bytes.Store(0)
	a.paused.Store(false) // whatever filled the disk is in the trash
	a.forgetSeqs(p.seqs)

	var err error
	if mkErr := os.MkdirAll(a.data, dirMode); mkErr != nil {
		// The history is gone — it is in the trash — but nothing new can be written until this
		// works: every write fails and is counted.
		err = fmt.Errorf("history cleared, but the archive could not recreate its directory: %w", mkErr)
		slog.Warn("session archive: cleared, but could not recreate the data directory", "error", mkErr)
	}
	if rmErr := os.RemoveAll(trash); rmErr != nil {
		slog.Warn("session archive: cleared, but the trash was not fully deleted; the next start deletes it",
			"dir", trash, "error", rmErr)
	}
	p.finish(res, err)
}

// forgetSeqs drops the numbering of every id in seqs that recorded nothing since seqs was taken,
// so a session started under it later numbers from 1. An id that did record keeps its numbering:
// those events are already numbered on from it.
func (a *Archive) forgetSeqs(seqs map[string]uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, n := range seqs {
		if a.lastSeq[id] == n {
			delete(a.lastSeq, id)
		}
	}
}

// removeTrash deletes what a clear moved aside and a crash kept it from deleting. Nothing reads
// a trash directory, so this is only about the disk it holds.
func removeTrash(root string) {
	ents, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range ents {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), trashPrefix) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
			slog.Warn("session archive: could not delete a cleared archive's leftovers", "dir", e.Name(), "error", err)
		}
	}
}
