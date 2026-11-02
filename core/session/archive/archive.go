package archive

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

const (
	// dataDirName holds one directory per session. Sessions live a level below the root so that
	// clearing all history can be one rename of this directory.
	dataDirName = "data"

	// defaultQueueDepth is how far the writer may fall behind the request path before events are
	// dropped and counted. reserve slots of it are held back for renames, so a burst of events can
	// never be the reason a rename is lost.
	defaultQueueDepth = 4096
	reserve           = 64

	// defaultIdleClose and defaultMaxOpen bound the heap open writers hold — about 3 MB each.
	// The idle window is long because the cap already bounds the worst case, and every reopened
	// session re-emits its current strings into a new segment (0.16–0.34 MB measured).
	defaultIdleClose = 60 * time.Minute
	defaultMaxOpen   = 16

	// Cadences, from the writer goroutine's tick: segments flush and fsync every tick, so an
	// unclean kill loses at most about a second; session.json is rewritten every metaEvery while
	// dirty; retention runs every pruneEvery.
	defaultTickEvery = time.Second
	metaEvery        = 5 * time.Second
	pruneEvery       = time.Hour

	defaultRetentionDays = 30
	// MaxRetentionDays is the cost ledger's ceiling, for its reason: a retention large enough to
	// wrap time arithmetic must not turn into a cutoff in the future.
	MaxRetentionDays = 3650
	defaultMaxBytes  = 2 << 30
)

// Option configures an Archive.
type Option func(*Archive)

// WithClock replaces the archive's time source, for tests.
func WithClock(now func() time.Time) Option { return func(a *Archive) { a.now = now } }

// WithRetentionDays sets how long a segment is kept after its last write. Zero or negative is
// the default, 30; past MaxRetentionDays it is clamped.
func WithRetentionDays(days int) Option { return func(a *Archive) { a.retainDays = days } }

// WithMaxBytes sets the archive's size bound. Zero or negative is the default, 2 GiB.
func WithMaxBytes(n int64) Option { return func(a *Archive) { a.maxBytes = n } }

// WithQueueDepth sets the request path's queue depth, for tests.
func WithQueueDepth(n int) Option { return func(a *Archive) { a.depth = n } }

// WithIdleClose sets how long a session's segment stays open without a write.
func WithIdleClose(d time.Duration) Option { return func(a *Archive) { a.idleClose = d } }

// WithMaxOpenWriters caps how many segments are open at once.
func WithMaxOpenWriters(n int) Option { return func(a *Archive) { a.maxOpen = n } }

// Stats is what the archive reports about itself.
type Stats struct {
	Bytes         int64
	MaxBytes      int64
	RetentionDays int
	// DroppedEvents counts events that never reached disk: a full queue, or a pause after the
	// disk filled. WriteErrors counts failed writes. DroppedRenames counts renames lost to a
	// queue full even past its reserve.
	DroppedEvents  uint64
	WriteErrors    uint64
	DroppedRenames uint64
	// Paused is true after ENOSPC, until the next retention pass.
	Paused bool
}

// Archive is the session archive: a session.Recorder that writes every event the store appends
// to disk, a session.Rekeyer that follows the store's renames, and a session.SeqSeeder that lets
// a re-created session continue its numbering.
//
// NOTHING ON THE REQUEST PATH TOUCHES DISK. Record and Rekeyed run under the store's write lock,
// in front of live proxied traffic: they update a map and make a non-blocking send. One writer
// goroutine owns every file, every segment writer and every session.json, and the state below
// marked "writer goroutine only" is never touched from anywhere else.
type Archive struct {
	root, data string
	now        func() time.Time
	retainDays int
	maxBytes   int64
	depth      int
	idleClose  time.Duration
	maxOpen    int
	tickEvery  time.Duration

	// Test seams: gate holds the writer before each op; segHook sees each new segment writer.
	gate    chan struct{}
	segHook func(*segmentWriter)

	ops       chan op
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	closed    atomic.Bool
	closeErr  error

	// lastSeq is the highest seq recorded per session id, for SeqSeeder. Updated synchronously
	// by Record and Rekeyed, so it is right before the writer has caught up. pending is the most
	// recent clear, for AwaitClear; see clear.go.
	mu      sync.Mutex
	lastSeq map[string]uint64
	pending *pendingClear
	// deletes is the most recent delete per session id, for AwaitDelete; see delete.go. nil
	// until the first.
	deletes map[string]*pendingClear
	// started is where the store's live entry for an id began numbering, from EntryStarted, for
	// ids that held history then. The entry's first queued event carries it to the writer, and
	// its rename tells the writer which segments move.
	started map[string]entryStart

	dropped, writeErrors, droppedRenames atomic.Uint64
	bytes                                atomic.Int64
	paused                               atomic.Bool

	// index is what readers see: each session's directory, segment ranges and summary,
	// republished by the writer goroutine whenever they change. Readers decode segment files on
	// their own goroutine against this snapshot, so a page read never stalls the writer — and a
	// segment still being appended reads safely up to its last flushed block.
	idxMu sync.RWMutex
	index map[string]*indexEntry

	// Writer goroutine only.
	sessions      map[string]*sessionState
	lastMeta      time.Time
	lastPrune     time.Time
	reportedDrops uint64
	lastDropLog   time.Time
}

// entryStart is where a store entry began numbering; see Archive.started.
type entryStart struct {
	after  uint64
	queued bool
}

// sessionState is one archived session as the writer goroutine holds it.
type sessionState struct {
	dir       string
	meta      *sessionMeta // closed segments in Segments; the open one, if any, is w
	w         *segmentWriter
	lastWrite time.Time
	metaDirty bool
	// startAfter is where the store's current entry began numbering, once its first event
	// arrived, and before is the summary of what the session held then. before is never folded
	// into: the reader index shares it (see indexEntry).
	startAfter uint64
	before     *session.SummaryFold
}

type opKind uint8

const (
	opEvent opKind = iota
	opRename
	opClear
	opDelete
	opFunc
)

// op is one unit of work for the writer goroutine. Events and renames share one FIFO, so a
// rename applies in order with the events around it.
type op struct {
	kind   opKind
	id, to string
	ev     pipeline.SessionEvent
	after  uint64 // where a store entry began: on its first queued event, and on its rename
	clear  *pendingClear
	f      func()
	done   chan struct{}
}

// Open opens the archive rooted at root, creating root/data if needed. It loads every session's
// session.json, reconciles each with its files, prunes to the retention bounds — all before
// returning, so nothing is served against stale state — and starts the writer goroutine.
func Open(root string, opts ...Option) (*Archive, error) {
	a := &Archive{
		root:       root,
		data:       filepath.Join(root, dataDirName),
		now:        time.Now,
		retainDays: defaultRetentionDays,
		maxBytes:   defaultMaxBytes,
		depth:      defaultQueueDepth,
		idleClose:  defaultIdleClose,
		maxOpen:    defaultMaxOpen,
		tickEvery:  defaultTickEvery,
		lastSeq:    map[string]uint64{},
		started:    map[string]entryStart{},
		sessions:   map[string]*sessionState{},
		index:      map[string]*indexEntry{},
	}
	for _, o := range opts {
		o(a)
	}
	if a.retainDays <= 0 {
		a.retainDays = defaultRetentionDays
	}
	if a.retainDays > MaxRetentionDays {
		slog.Warn("session archive: retention_days is past the maximum; clamping",
			"requested", a.retainDays, "using", MaxRetentionDays)
		a.retainDays = MaxRetentionDays
	}
	if a.maxBytes <= 0 {
		a.maxBytes = defaultMaxBytes
	}
	if a.depth <= reserve {
		a.depth = reserve + 1
	}
	if a.maxOpen <= 0 {
		a.maxOpen = defaultMaxOpen
	}
	if a.idleClose <= 0 {
		a.idleClose = defaultIdleClose
	}
	if err := os.MkdirAll(a.data, dirMode); err != nil {
		return nil, err
	}
	removeTrash(a.root)

	loadedSessions, skipped, err := loadMetas(a.data)
	if err != nil {
		return nil, err
	}
	for _, l := range loadedSessions {
		a.adopt(l)
	}
	if skipped > 0 {
		slog.Warn("session archive: some sessions could not be read and are not served", "skipped", skipped)
	}
	now := a.now()
	a.prune(now)
	a.lastPrune, a.lastMeta = now, now
	a.refreshBytes()

	a.ops = make(chan op, a.depth)
	a.stop = make(chan struct{})
	a.done = make(chan struct{})
	go a.run()
	return a, nil
}

// adopt takes one loaded session into the archive at startup: reconciled with its files, its
// directory named for the id its session.json holds, and its last seq known to the store.
func (a *Archive) adopt(l loaded) {
	changed, err := reconcile(l.dir, l.meta)
	if err != nil {
		slog.Warn("session archive: could not reconcile a session with its files", "dir", l.dir, "error", err)
	}
	// A crash between rewriting session.json for a rename and renaming the directory leaves the
	// file naming the new id under the old directory. Finish the rename.
	if want := filepath.Join(a.data, dirName(l.meta.ID)); l.dir != want {
		if _, err := os.Stat(want); errors.Is(err, os.ErrNotExist) && os.Rename(l.dir, want) == nil {
			l.dir = want
		}
	}
	if prev, dup := a.sessions[l.meta.ID]; dup {
		slog.Warn("session archive: two directories claim one session; keeping the longer history",
			"session", l.meta.ID, "dirs", []string{prev.dir, l.dir})
		if prev.meta.lastSeq() >= l.meta.lastSeq() {
			return
		}
	}
	s := &sessionState{dir: l.dir, meta: l.meta}
	a.sessions[l.meta.ID] = s
	a.lastSeq[l.meta.ID] = l.meta.lastSeq()
	if changed {
		a.writeMeta(s)
	}
	a.publish(s)
}

// Record implements session.Recorder. It never blocks and never touches disk; see Archive.
func (a *Archive) Record(sessionID string, e *pipeline.SessionEvent) {
	if a.closed.Load() {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if e.Seq > a.lastSeq[sessionID] {
		a.lastSeq[sessionID] = e.Seq
	}
	// Every send happens under the store's write lock, so this check and the send below cannot
	// race another producer; the writer only ever makes room.
	if len(a.ops) >= cap(a.ops)-reserve {
		a.dropped.Add(1)
		return
	}
	o := op{kind: opEvent, id: sessionID, ev: *e}
	st, starting := a.started[sessionID]
	if starting && !st.queued {
		o.after = st.after
	}
	select {
	case a.ops <- o:
		if starting && !st.queued {
			st.queued = true
			a.started[sessionID] = st
		}
	default:
		a.dropped.Add(1)
	}
}

// EntryStarted implements session.EntryStarter. A memory update: it runs under the store's
// write lock.
func (a *Archive) EntryStarted(sessionID string, after uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if after == 0 {
		delete(a.started, sessionID)
		return
	}
	a.started[sessionID] = entryStart{after: after}
}

// Rekeyed implements session.Rekeyer: the session's history follows its new id. The rename
// goes through the same queue as events, into the reserve the events may not use.
func (a *Archive) Rekeyed(oldID, newID string) {
	if a.closed.Load() {
		return
	}
	a.mu.Lock()
	st := a.started[oldID]
	delete(a.started, oldID)
	delete(a.started, newID)
	if n, ok := a.lastSeq[oldID]; ok {
		delete(a.lastSeq, oldID)
		if st.after > 0 {
			a.lastSeq[oldID] = st.after
		}
		a.lastSeq[newID] = max(a.lastSeq[newID], n)
	}
	a.mu.Unlock()
	select {
	case a.ops <- op{kind: opRename, id: oldID, to: newID, after: st.after}:
	default:
		a.droppedRenames.Add(1)
		slog.Error("session archive: a rename was lost to a full queue; the session's history stays under its old id",
			"from", oldID, "to", newID)
	}
}

// LastSeq implements session.SeqSeeder. A memory lookup: it runs under the store's write lock.
func (a *Archive) LastSeq(sessionID string) uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastSeq[sessionID]
}

// Stats reports the archive's size, bounds and losses.
func (a *Archive) Stats() Stats {
	return Stats{
		Bytes:          a.bytes.Load(),
		MaxBytes:       a.maxBytes,
		RetentionDays:  a.retainDays,
		DroppedEvents:  a.dropped.Load(),
		WriteErrors:    a.writeErrors.Load(),
		DroppedRenames: a.droppedRenames.Load(),
		Paused:         a.paused.Load(),
	}
}

// Root is the directory the archive was opened at.
func (a *Archive) Root() string { return a.root }

// RetentionDays is the retention in effect, after defaulting and clamping.
func (a *Archive) RetentionDays() int { return a.retainDays }

// Close drains the queue, finishes and fsyncs every open segment, writes every changed
// session.json and stops the writer. An orderly stop loses nothing that was queued.
func (a *Archive) Close() error {
	a.closeOnce.Do(func() {
		a.closed.Store(true)
		close(a.stop)
	})
	<-a.done
	return a.closeErr
}

// inWriter runs f on the writer goroutine and waits for it — how tests read and drive state the
// writer goroutine owns without racing it.
func (a *Archive) inWriter(f func()) {
	done := make(chan struct{})
	a.ops <- op{kind: opFunc, f: f, done: done}
	<-done
}

func (a *Archive) run() {
	defer close(a.done)
	t := time.NewTicker(a.tickEvery)
	defer t.Stop()
	for {
		select {
		case o := <-a.ops:
			a.take(o)
		case <-t.C:
			a.tick()
		case <-a.stop:
			for drained := false; !drained; {
				select {
				case o := <-a.ops:
					a.take(o)
				default:
					drained = true
				}
			}
			a.closeErr = a.shutdown()
			return
		}
	}
}

func (a *Archive) take(o op) {
	if a.gate != nil {
		<-a.gate
	}
	switch o.kind {
	case opEvent:
		if o.after > 0 {
			a.begin(o.id, o.after)
		}
		a.write(o.id, &o.ev)
	case opRename:
		a.rename(o.id, o.to, o.after)
	case opClear:
		a.clearAll(o.clear)
	case opDelete:
		a.deleteSession(o.id, o.clear)
	case opFunc:
		o.f()
		close(o.done)
	}
}

// begin marks where a new store entry for id began numbering: its events start a segment of
// their own.
func (a *Archive) begin(id string, after uint64) {
	s := a.sessions[id]
	if s == nil {
		return
	}
	a.closeWriter(s)
	if s.meta.lastSeq() > after {
		s.startAfter, s.before = 0, nil
	} else {
		s.startAfter, s.before = after, s.meta.Summary.Clone()
	}
	// Published at once rather than with the event's write: a paused or failing write publishes
	// nothing, and the store's list asks for before from the moment its entry exists.
	a.publish(s)
}

// write appends one event to its session's open segment, opening one if needed.
func (a *Archive) write(id string, e *pipeline.SessionEvent) {
	if a.paused.Load() {
		a.dropped.Add(1)
		return
	}
	now := a.now()
	s := a.sessions[id]
	fresh := s == nil
	if fresh {
		s = &sessionState{
			dir:  filepath.Join(a.data, dirName(id)),
			meta: &sessionMeta{ID: id, CreatedAt: now, UpdatedAt: now, Summary: session.NewSummaryFold()},
		}
		a.sessions[id] = s
	}
	if s.w == nil {
		a.makeRoomForWriter()
		w, err := createSegment(s.dir, now)
		if err != nil {
			a.writeFailed(s, err)
			return
		}
		if a.segHook != nil {
			a.segHook(w)
		}
		s.w = w
	}
	if err := s.w.append(*e, now); err != nil {
		a.writeFailed(s, err)
		return
	}
	s.meta.Summary.Add(id, e)
	s.meta.UpdatedAt, s.lastWrite, s.metaDirty = now, now, true
	a.publish(s)
	// Rotate on size, not only on idleness: a session busy all day kept ONE
	// segment growing without limit (idleClose never fires, planPrune never
	// deletes an open segment, and maxBytes could stay exceeded), and because
	// Page and Event decode a segment from its first event, every backward
	// page through a 20k-event segment decompressed most of it (#1566).
	// After the append, so the event that crosses the cap is IN the segment
	// being closed — no event is ever lost or duplicated by a rotation.
	if info := s.w.info(); info.Events >= maxSegmentEvents || info.Bytes >= maxSegmentBytes {
		a.closeWriter(s)
	}
	// A session's session.json exists from its first event, so a crash before the first
	// metaEvery still leaves a directory reconcile can read.
	if fresh {
		a.writeMeta(s)
	}
}

// maxSegmentEvents and maxSegmentBytes cap one segment's size. Events is the
// binding cap on a chatty session (a 500-event page is the read unit, so
// 10k events bounds a full Earlier sweep to two segments); bytes is the
// backstop for large events.
const (
	maxSegmentEvents = 10_000
	maxSegmentBytes  = 128 << 20
)

// makeRoomForWriter closes the least recently written segment when the cap is reached.
func (a *Archive) makeRoomForWriter() {
	var open []*sessionState
	for _, s := range a.sessions {
		if s.w != nil {
			open = append(open, s)
		}
	}
	if len(open) < a.maxOpen {
		return
	}
	oldest := slices.MinFunc(open, func(x, y *sessionState) int { return x.lastWrite.Compare(y.lastWrite) })
	a.closeWriter(oldest)
}

// closeWriter finishes s's open segment and moves it into the closed list.
func (a *Archive) closeWriter(s *sessionState) {
	if s.w == nil {
		return
	}
	err := s.w.close()
	a.retire(s)
	if err != nil {
		a.writeErrors.Add(1)
		slog.Warn("session archive: closing a segment failed", "session", s.meta.ID, "error", err)
	}
}

// retire moves s's writer, open or broken, into the closed list — or deletes its file when it
// never held an event.
func (a *Archive) retire(s *sessionState) {
	info := s.w.info()
	if info.Events > 0 {
		s.meta.Segments = append(s.meta.Segments, info)
	} else {
		os.Remove(filepath.Join(s.dir, info.File))
	}
	s.w = nil
	s.metaDirty = true
	a.publish(s)
}

// writeFailed counts a failed write, closes the segment it hit, and pauses the archive when the
// disk is full. The session's next event opens a fresh segment.
func (a *Archive) writeFailed(s *sessionState, err error) {
	a.writeErrors.Add(1)
	slog.Warn("session archive: write failed; closing the segment", "session", s.meta.ID, "error", err)
	if s.w != nil {
		s.w.close()
		a.retire(s)
	}
	if errors.Is(err, syscall.ENOSPC) && !a.paused.Swap(true) {
		slog.Warn("session archive: the disk is full; pausing until the next retention pass",
			"effect", "events are not archived until then; live sessions are unaffected")
	}
}

// rename follows the store's rename of oldID to newID: session.json is rewritten with the new id
// first, then the directory moved. A crash between the two leaves a session.json naming the new
// id under the old directory, which adopt finishes at the next start.
func (a *Archive) rename(oldID, newID string, after uint64) {
	s := a.sessions[oldID]
	if s == nil {
		return
	}
	newDir := filepath.Join(a.data, dirName(newID))
	if _, taken := a.sessions[newID]; taken {
		slog.Warn("session archive: rename target already has history; keeping it under the old id",
			"from", oldID, "to", newID)
		return
	}
	if _, err := os.Stat(newDir); err == nil {
		slog.Warn("session archive: rename target directory exists; keeping history under the old id",
			"from", oldID, "to", newID)
		return
	}
	if after > 0 && a.renameEntry(s, oldID, newID, newDir, after) {
		return
	}
	s.meta.ID = newID
	a.writeMeta(s)
	if err := os.Rename(s.dir, newDir); err != nil {
		slog.Warn("session archive: could not move a renamed session's directory; it will be moved at the next start",
			"from", oldID, "to", newID, "error", err)
	} else {
		s.dir = newDir
	}
	delete(a.sessions, oldID)
	a.sessions[newID] = s
	a.unpublish(oldID)
	a.publish(s)
}

// renameEntry renames the segments of oldID that a store entry numbering after `after` wrote,
// and leaves the rest under oldID. It reports false when every segment is the entry's, for
// rename to move the directory whole.
func (a *Archive) renameEntry(s *sessionState, oldID, newID, newDir string, after uint64) bool {
	segs := slices.Clone(s.meta.Segments)
	if s.w != nil {
		segs = append(segs, s.w.info())
	}
	isEntry := func(seg SegmentInfo) bool { return seg.Events > 0 && seg.FirstSeq > after }
	entry := 0
	for _, seg := range segs {
		if isEntry(seg) {
			entry++
		}
	}
	startAfter, before := s.startAfter, s.before
	s.startAfter, s.before = 0, nil
	if entry == len(segs) {
		if startAfter == after {
			a.closeWriter(s)
			s.meta.Summary, _ = foldSegments(s.dir, s.meta.Segments, oldID)
		}
		return false
	}
	if entry == 0 {
		return true
	}
	if startAfter != after {
		slog.Warn("session archive: cannot tell a renamed session's events from earlier ones; keeping them under the old id",
			"from", oldID, "to", newID)
		return true
	}

	a.closeWriter(s)
	var keep, move []SegmentInfo
	for _, seg := range s.meta.Segments {
		if isEntry(seg) {
			move = append(move, seg)
		} else {
			keep = append(keep, seg)
		}
	}
	slices.SortFunc(move, func(x, y SegmentInfo) int { return cmp.Compare(x.FirstSeq, y.FirstSeq) })
	since, createdAt := foldSegments(s.dir, move, oldID)
	if createdAt.IsZero() {
		createdAt = a.now()
	}
	ns := &sessionState{
		dir:       newDir,
		meta:      &sessionMeta{ID: newID, CreatedAt: createdAt, UpdatedAt: latestWrite(move), Summary: since, Segments: move},
		lastWrite: s.lastWrite,
	}
	// session.json first: a crash before a segment moves leaves it listed where it is not, which
	// reconcile drops, rather than a directory loadMetas cannot read.
	if err := writeMeta(newDir, ns.meta); err != nil {
		a.writeErrors.Add(1)
		slog.Warn("session archive: could not write a renamed session's session.json; keeping its history under the old id",
			"from", oldID, "to", newID, "error", err)
		os.RemoveAll(newDir)
		return true
	}
	ns.meta.Segments = nil
	for _, seg := range move {
		if err := os.Rename(filepath.Join(s.dir, seg.File), filepath.Join(newDir, seg.File)); err != nil {
			a.writeErrors.Add(1)
			slog.Warn("session archive: could not move a renamed session's segment; it stays under the old id",
				"from", oldID, "to", newID, "file", seg.File, "error", err)
			keep = append(keep, seg)
			ns.metaDirty = true
			continue
		}
		ns.meta.Segments = append(ns.meta.Segments, seg)
	}
	// A CLONE: before was published, and the old id's live fold is Added to from here on.
	s.meta.Segments, s.meta.Summary, s.meta.UpdatedAt = keep, before.Clone(), latestWrite(keep)
	a.writeMeta(s)
	if ns.metaDirty {
		a.writeMeta(ns)
	}
	a.sessions[newID] = ns
	a.publish(s)
	a.publish(ns)
	return true
}

// foldSegments reads segs back from dir and folds their events, recorded under id, in seq order.
// It also returns the first event's time.
func foldSegments(dir string, segs []SegmentInfo, id string) (*session.SummaryFold, time.Time) {
	segs = slices.Clone(segs)
	slices.SortFunc(segs, func(x, y SegmentInfo) int { return cmp.Compare(x.FirstSeq, y.FirstSeq) })
	f := session.NewSummaryFold()
	var first time.Time
	for _, seg := range segs {
		if _, err := readSegment(filepath.Join(dir, seg.File), func(e *pipeline.SessionEvent) bool {
			if first.IsZero() {
				first = e.At
			}
			f.Add(id, e)
			return true
		}); err != nil {
			slog.Warn("session archive: could not read a renamed segment back for its summary", "file", seg.File, "error", err)
		}
	}
	return f, first
}

// latestWrite is the last time any of segs was written.
func latestWrite(segs []SegmentInfo) time.Time {
	var t time.Time
	for _, seg := range segs {
		if seg.LastWrite.After(t) {
			t = seg.LastWrite
		}
	}
	return t
}

// tick is the writer goroutine's clock: flush, idle close, session.json cadence, retention.
func (a *Archive) tick() {
	now := a.now()
	for _, s := range a.sessions {
		if s.w == nil {
			continue
		}
		if err := s.w.flush(); err != nil {
			a.writeFailed(s, err)
			continue
		}
		if now.Sub(s.lastWrite) >= a.idleClose {
			a.closeWriter(s)
		}
	}
	if now.Sub(a.lastMeta) >= metaEvery {
		for _, s := range a.sessions {
			if s.metaDirty {
				a.writeMeta(s)
			}
		}
		a.lastMeta = now
	}
	if now.Sub(a.lastPrune) >= pruneEvery {
		a.prune(now)
		a.lastPrune = now
		if a.paused.Swap(false) {
			slog.Info("session archive: resuming after a full disk")
		}
	}
	if d := a.dropped.Load(); d > a.reportedDrops && now.Sub(a.lastDropLog) >= time.Minute {
		slog.Warn("session archive: events were not archived", "dropped", d-a.reportedDrops, "droppedTotal", d)
		a.reportedDrops, a.lastDropLog = d, now
	}
	a.refreshBytes()
}

// prune deletes what retention says to, and every session left with nothing on disk.
func (a *Archive) prune(now time.Time) {
	var refs []segmentRef
	byDir := map[string]*sessionState{}
	for _, s := range a.sessions {
		byDir[s.dir] = s
		for _, seg := range s.meta.Segments {
			refs = append(refs, segmentRef{SessionDir: s.dir, Info: seg})
		}
		if s.w != nil {
			refs = append(refs, segmentRef{SessionDir: s.dir, Info: s.w.info(), Open: true})
		}
	}
	lastBefore := map[*sessionState]uint64{}
	for _, r := range planPrune(refs, now, a.retainDays, a.maxBytes) {
		s := byDir[r.SessionDir]
		if _, seen := lastBefore[s]; !seen {
			lastBefore[s] = s.meta.lastSeq()
		}
		if err := os.Remove(filepath.Join(r.SessionDir, r.Info.File)); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("session archive: could not delete a segment retention expired", "file", r.Info.File, "error", err)
			continue
		}
		s.meta.Segments = slices.DeleteFunc(s.meta.Segments, func(x SegmentInfo) bool { return x.File == r.Info.File })
		s.metaDirty = true
		a.publish(s)
	}
	for id, s := range a.sessions {
		if s.w != nil || len(s.meta.Segments) > 0 {
			continue
		}
		if err := os.RemoveAll(s.dir); err != nil {
			slog.Warn("session archive: could not remove an emptied session", "dir", s.dir, "error", err)
			continue
		}
		delete(a.sessions, id)
		a.unpublish(id)
		// Forget the session's numbering only if nothing newer than what was on disk was
		// recorded since: an event still queued for it must keep its seq out of reuse.
		a.mu.Lock()
		if a.lastSeq[id] <= lastBefore[s] {
			delete(a.lastSeq, id)
		}
		a.mu.Unlock()
	}
	for _, s := range a.sessions {
		if s.metaDirty && len(lastBefore) > 0 {
			if _, touched := lastBefore[s]; touched {
				a.writeMeta(s)
			}
		}
	}
	a.refreshBytes()
}

// refreshBytes recomputes the archive's size from every session's segments.
func (a *Archive) refreshBytes() {
	var total int64
	for _, s := range a.sessions {
		for _, seg := range s.meta.Segments {
			total += seg.Bytes
		}
		if s.w != nil {
			total += s.w.info().Bytes
		}
	}
	a.bytes.Store(total)
}

// writeMeta writes s's session.json: the closed segments, the open one as it stands, and the
// summary — one snapshot, as sessionMeta requires.
func (a *Archive) writeMeta(s *sessionState) {
	m := *s.meta
	m.Segments = slices.Clone(s.meta.Segments)
	if s.w != nil {
		m.Segments = append(m.Segments, s.w.info())
	}
	if err := writeMeta(s.dir, &m); err != nil {
		a.writeErrors.Add(1)
		slog.Warn("session archive: could not write session.json", "session", s.meta.ID, "error", err)
		return
	}
	s.metaDirty = false
}

// shutdown closes every writer and writes every changed session.json.
func (a *Archive) shutdown() error {
	var errs []error
	for _, s := range a.sessions {
		if s.w == nil {
			continue
		}
		if err := s.w.close(); err != nil {
			errs = append(errs, fmt.Errorf("session %s: %w", s.meta.ID, err))
		}
		a.retire(s)
	}
	for _, s := range a.sessions {
		if s.metaDirty {
			a.writeMeta(s)
		}
	}
	return errors.Join(errs...)
}

// indexEntry is one session as readers see it. Every field is a fresh copy, never mutated after
// publish, so a reader holding one needs no lock.
type indexEntry struct {
	dir string
	// segments are the closed segments, then — when open is true — the one still being written.
	segments []SegmentInfo
	open     bool
	summary  session.SessionSummary
	// startAfter and before are the writer's sessionState fields of the same names: where the
	// store's current entry for this id began numbering, and the fold of what the session held
	// then. before is SHARED with the writer rather than copied, because it is immutable: begin
	// makes it as a fresh clone, nothing folds into it, and renameEntry clones it before reusing it
	// as a live fold.
	startAfter uint64
	before     *session.SummaryFold
}

// publish replaces s's entry in the reader index. Writer goroutine only.
func (a *Archive) publish(s *sessionState) {
	e := &indexEntry{dir: s.dir, segments: slices.Clone(s.meta.Segments)}
	if s.w != nil {
		e.segments = append(e.segments, s.w.info())
		e.open = true
	}
	e.summary = s.meta.Summary.Summary(s.meta.ID)
	e.summary.CreatedAt, e.summary.UpdatedAt = s.meta.CreatedAt, s.meta.UpdatedAt
	e.startAfter, e.before = s.startAfter, s.before
	a.idxMu.Lock()
	a.index[s.meta.ID] = e
	a.idxMu.Unlock()
}

// unpublish removes id from the reader index. Writer goroutine only.
func (a *Archive) unpublish(id string) {
	a.idxMu.Lock()
	delete(a.index, id)
	a.idxMu.Unlock()
}
