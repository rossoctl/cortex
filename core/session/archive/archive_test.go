package archive

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// openTest opens an archive on a fake clock whose real ticker effectively never fires, so a test
// drives every tick itself through tick().
func openTest(t *testing.T, root string, clk *fakeClock, opts ...Option) *Archive {
	t.Helper()
	base := []Option{WithClock(clk.now), func(a *Archive) { a.tickEvery = time.Hour }}
	a, err := Open(root, append(base, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 10, 1, 9, 0, 0, 0, time.Local)} }

// tick runs one writer tick, synchronously.
func tick(a *Archive) { a.inWriter(a.tick) }

// settle waits until the writer has applied everything queued so far. Record only queues, and
// the writer stamps what it writes with its own clock, so a test that moves the clock must
// settle first or the events are written at the later time.
func settle(a *Archive) { a.inWriter(func() {}) }

// recorder numbers events per session the way the store does and hands them to the archive.
type recorder struct {
	a   *Archive
	seq map[string]uint64
}

func newRecorder(a *Archive) *recorder { return &recorder{a: a, seq: map[string]uint64{}} }

func (r *recorder) record(id string, evs ...pipeline.SessionEvent) {
	for _, e := range evs {
		r.seq[id]++
		e.Seq, e.SessionID = r.seq[id], id
		r.a.Record(id, &e)
	}
}

// onDisk reads every event the archive holds for id, through session.json and the reader.
func onDisk(t *testing.T, root, id string) []pipeline.SessionEvent {
	t.Helper()
	dir := filepath.Join(root, dataDirName, dirName(id))
	m, err := readMeta(dir)
	if err != nil {
		t.Fatalf("readMeta(%s): %v", id, err)
	}
	var out []pipeline.SessionEvent
	for _, s := range m.Segments {
		if _, err := readSegment(filepath.Join(dir, s.File), func(e *pipeline.SessionEvent) bool {
			out = append(out, *e)
			return true
		}); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func TestArchive_WritesEventsAndTheyReadBack(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock())
	evs := synthSession(41, 5, 2048)
	newRecorder(a).record("s1", evs...)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	got := onDisk(t, root, "s1")
	if len(got) != len(evs) {
		t.Fatalf("%d events on disk, want %d", len(got), len(evs))
	}
	for i := range got {
		want := evs[i]
		want.Seq, want.SessionID = uint64(i+1), "s1"
		if w, g := onceThroughJSON(t, want), mustJSON(t, got[i]); w != g {
			t.Fatalf("event %d differs", i)
		}
	}
	m, _ := readMeta(filepath.Join(root, dataDirName, dirName("s1")))
	if n := m.Summary.Summary("s1").EventCount; n != len(evs) {
		t.Fatalf("summary counts %d, want %d", n, len(evs))
	}
}

// LastSeq is what the store numbers a re-created session after. It must be right the moment
// Record returns — before the writer has caught up — and still right after a restart.
func TestArchive_LastSeqIsSynchronousAndSurvivesARestart(t *testing.T) {
	root := t.TempDir()
	clk := newClock()
	a := openTest(t, root, clk)
	a.gate = make(chan struct{}) // the writer applies nothing yet
	newRecorder(a).record("s1", synthSession(42, 2, 1024)...)
	if got := a.LastSeq("s1"); got != 6 {
		t.Fatalf("LastSeq before the writer ran = %d, want 6", got)
	}
	close(a.gate)
	a.Close()

	b := openTest(t, root, clk)
	defer b.Close()
	if got := b.LastSeq("s1"); got != 6 {
		t.Fatalf("LastSeq after a restart = %d, want 6", got)
	}
}

// A process never appends to a segment a previous one wrote.
func TestArchive_ANewProcessOpensANewSegment(t *testing.T) {
	root := t.TempDir()
	clk := newClock()
	a := openTest(t, root, clk)
	r := newRecorder(a)
	r.record("s1", synthSession(43, 1, 1024)...)
	a.Close()
	clk.advance(time.Second)
	b := openTest(t, root, clk)
	r.a = b
	r.record("s1", synthSession(44, 1, 1024)...)
	b.Close()

	m, err := readMeta(filepath.Join(root, dataDirName, dirName("s1")))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Segments) != 2 || m.Segments[0].LastSeq != 3 || m.Segments[1].FirstSeq != 4 {
		t.Fatalf("segments = %+v, want two: 1-3 and 4-6", m.Segments)
	}
	if got := onDisk(t, root, "s1"); len(got) != 6 {
		t.Fatalf("%d events on disk, want 6", len(got))
	}
}

// Record runs in front of live traffic: with the writer stalled it must drop and count rather
// than wait, and it may only fill the queue up to the reserve.
func TestRecord_NeverBlocksAndCountsDropsWhenFull(t *testing.T) {
	a := openTest(t, t.TempDir(), newClock(), WithQueueDepth(reserve+6))
	a.gate = make(chan struct{})
	evs := synthSession(45, 34, 512) // 102 events
	done := make(chan struct{})
	go func() {
		newRecorder(a).record("s1", evs...)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Record blocked with the writer stalled")
	}
	// The writer took one op and is held at the gate; six more fit below the reserve.
	if got, want := a.Stats().DroppedEvents, uint64(len(evs)-7); got != want {
		t.Fatalf("dropped %d, want %d", got, want)
	}
	close(a.gate)
	a.Close()
}

// The reserve is for renames: with events filling the queue to it, a rename still lands.
func TestRekeyed_LandsInTheReserveWhenEventsFillTheQueue(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock(), WithQueueDepth(reserve+6))
	a.gate = make(chan struct{})
	newRecorder(a).record("pending:x", synthSession(46, 34, 512)...) // 102 events: more than the whole queue
	a.Rekeyed("pending:x", "s1")
	if got := a.Stats().DroppedRenames; got != 0 {
		t.Fatalf("dropped %d renames, want 0", got)
	}
	close(a.gate)
	a.Close()
	if _, err := readMeta(filepath.Join(root, dataDirName, dirName("s1"))); err != nil {
		t.Fatalf("the renamed session is not under its new id: %v", err)
	}
}

// A rename applies in order with the events around it. After an adoption the pending id is
// re-created at once: what was queued before the rename belongs to the adopted session, and
// what comes after starts a new one under the freed id.
func TestRekeyed_RenamesTheDirectoryInQueueOrder(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock())
	r := newRecorder(a)
	r.record("pending:x", synthSession(47, 1, 512)...) // 3 events
	a.Rekeyed("pending:x", "s1")
	// The numbering moves with the rename at once, before the writer applies it: the store asks
	// LastSeq under its lock right after renaming, and must see the history under its new id.
	if a.LastSeq("s1") != 3 || a.LastSeq("pending:x") != 0 {
		t.Fatalf("LastSeq after the rename: s1=%d pending:x=%d, want 3 and 0", a.LastSeq("s1"), a.LastSeq("pending:x"))
	}
	r.seq["pending:x"] = 0
	r.record("pending:x", synthSession(48, 2, 512)...) // 6 events, a new bucket
	a.Close()

	if got := onDisk(t, root, "s1"); len(got) != 3 {
		t.Fatalf("s1 holds %d events, want the 3 recorded before the rename", len(got))
	}
	if got := onDisk(t, root, "pending:x"); len(got) != 6 {
		t.Fatalf("pending:x holds %d events, want the 6 recorded after", len(got))
	}
}

// The store refuses a rename into a target with history, and the archive does not merge two
// histories either if one reaches it.
func TestRekeyed_SkipsWhenTheTargetHasHistory(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock())
	r := newRecorder(a)
	r.record("a", synthSession(49, 1, 512)...)
	r.record("b", synthSession(50, 1, 512)...)
	a.Rekeyed("a", "b")
	a.Close()
	if len(onDisk(t, root, "a")) != 3 || len(onDisk(t, root, "b")) != 3 {
		t.Fatal("a rename onto an existing history merged them")
	}
}

// The archive against a real store: appends with trimming, renames and adoptions racing on
// many goroutines. Run under -race; it checks the copy Record hands the writer is safe.
func TestRecord_RaceAgainstStoreRekeyAndTrim(t *testing.T) {
	a := openTest(t, t.TempDir(), newClock())
	st := session.New(0, 5, 0)
	defer st.Close()
	st.AddRecorder(a)
	evs := synthSession(51, 10, 2048)
	var wg sync.WaitGroup
	for g := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i, e := range evs {
				id := []string{session.DefaultSessionID, "s1", "pending:bob-shell", "s2"}[(g+i)%4]
				st.Append(id, e)
				if i%7 == 0 {
					st.Rekey(session.DefaultSessionID, "ctx-race")
				}
			}
		}()
	}
	wg.Wait()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestArchive_ClosesAWriterIdleSixtyMinutes(t *testing.T) {
	clk := newClock()
	a := openTest(t, t.TempDir(), clk)
	defer a.Close()
	newRecorder(a).record("s1", synthSession(52, 1, 512)...)
	settle(a)
	open := func() (n int) {
		a.inWriter(func() {
			for _, s := range a.sessions {
				if s.w != nil {
					n++
				}
			}
		})
		return n
	}
	clk.advance(59 * time.Minute)
	tick(a)
	if open() != 1 {
		t.Fatal("the writer closed before sixty idle minutes")
	}
	clk.advance(time.Minute)
	tick(a)
	if open() != 0 {
		t.Fatal("the writer is still open after sixty idle minutes")
	}
}

// The cap bounds open writers' heap: past it, the least recently written closes.
func TestArchive_CapsOpenWriters(t *testing.T) {
	clk := newClock()
	a := openTest(t, t.TempDir(), clk, WithMaxOpenWriters(2))
	defer a.Close()
	r := newRecorder(a)
	for _, id := range []string{"s1", "s2", "s3"} {
		r.record(id, synthSession(53, 1, 512)[0])
		settle(a)
		clk.advance(time.Second)
	}
	var open []string
	a.inWriter(func() {
		for id, s := range a.sessions {
			if s.w != nil {
				open = append(open, id)
			}
		}
	})
	if len(open) != 2 || slicesContains(open, "s1") {
		t.Fatalf("open writers %v, want s2 and s3", open)
	}
}

func slicesContains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// A tick flushes, so a segment still open is readable up to it — what an unclean kill leaves.
func TestArchive_ATickMakesTheOpenSegmentReadable(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock())
	defer a.Close()
	newRecorder(a).record("s1", synthSession(54, 2, 1024)...)
	tick(a)
	var path string
	a.inWriter(func() {
		s := a.sessions["s1"]
		path = filepath.Join(s.dir, s.w.info().File)
	})
	got, _ := readAll(t, path)
	if len(got) != 6 {
		t.Fatalf("%d events readable from the open segment, want 6", len(got))
	}
}

// session.json exists from a session's first event and follows it while dirty.
func TestArchive_SessionJSONFollowsTheSession(t *testing.T) {
	root := t.TempDir()
	clk := newClock()
	a := openTest(t, root, clk)
	defer a.Close()
	r := newRecorder(a)
	r.record("s1", synthSession(55, 1, 512)[0])
	tick(a)
	dir := filepath.Join(root, dataDirName, dirName("s1"))
	if _, err := readMeta(dir); err != nil {
		t.Fatalf("no session.json after the first event: %v", err)
	}
	r.record("s1", synthSession(56, 3, 512)...)
	clk.advance(metaEvery)
	tick(a)
	m, err := readMeta(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n := m.Summary.Summary("s1").EventCount; n != 10 || m.lastSeq() != 10 {
		t.Fatalf("session.json shows %d events, last seq %d; want 10 and 10", n, m.lastSeq())
	}
}

// An orderly stop loses nothing queued: every event lands, every frame ends.
func TestClose_DrainsFinishesFramesAndWritesSummaries(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock())
	evs := synthSession(57, 20, 1024)
	newRecorder(a).record("s1", evs...)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, dataDirName, dirName("s1"))
	m, _ := readMeta(dir)
	for _, s := range m.Segments {
		if _, truncated := readAll(t, filepath.Join(dir, s.File)); truncated {
			t.Fatalf("segment %s did not end its frame", s.File)
		}
	}
	if n := m.Summary.Summary("s1").EventCount; n != len(evs) {
		t.Fatalf("summary counts %d, want %d", n, len(evs))
	}
}

// After a crash, session.json can be behind the files. Open must catch up before it seeds
// anything: a stale LastSeq would number new events into ones already on disk.
func TestOpen_ReconcilesACrashedSession(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, dataDirName, dirName("s1"))
	evs := synthSession(58, 4, 1024)
	for i := range evs {
		evs[i].SessionID = "s1"
	}
	info := writeSegmentIn(t, dir, time.Date(2026, 10, 1, 9, 0, 0, 0, time.Local), evs)
	m := newMeta("s1")
	stale := info
	stale.LastSeq, stale.Events, stale.Bytes = 3, 3, 10
	m.Segments = []SegmentInfo{stale}
	if err := writeMeta(dir, m); err != nil {
		t.Fatal(err)
	}

	a := openTest(t, root, newClock())
	defer a.Close()
	if got := a.LastSeq("s1"); got != 12 {
		t.Fatalf("LastSeq after reconcile = %d, want 12", got)
	}
}

// A crash between rewriting session.json for a rename and moving the directory is finished at
// the next start.
func TestOpen_FinishesAnInterruptedRename(t *testing.T) {
	root := t.TempDir()
	oldDir := filepath.Join(root, dataDirName, dirName("pending:x"))
	info := writeSegmentIn(t, oldDir, time.Date(2026, 10, 1, 9, 0, 0, 0, time.Local), synthSession(59, 1, 512))
	m := newMeta("s1") // session.json already names the new id
	m.Segments = []SegmentInfo{info}
	if err := writeMeta(oldDir, m); err != nil {
		t.Fatal(err)
	}
	a := openTest(t, root, newClock())
	defer a.Close()
	if _, err := os.Stat(filepath.Join(root, dataDirName, dirName("s1"))); err != nil {
		t.Fatalf("the directory was not moved to its session's id: %v", err)
	}
	if a.LastSeq("s1") != 3 {
		t.Fatalf("LastSeq(s1) = %d, want 3", a.LastSeq("s1"))
	}
}

// Retention runs before Open returns, so nothing is served from expired history.
func TestOpen_PrunesBeforeReturning(t *testing.T) {
	root := t.TempDir()
	clk := newClock()
	dir := filepath.Join(root, dataDirName, dirName("old"))
	info := writeSegmentIn(t, dir, clk.now().AddDate(0, 0, -40), synthSession(60, 1, 512))
	info.LastWrite = clk.now().AddDate(0, 0, -40)
	m := newMeta("old")
	m.Segments = []SegmentInfo{info}
	if err := writeMeta(dir, m); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(filepath.Join(dir, info.File), info.LastWrite, info.LastWrite)

	a := openTest(t, root, clk)
	defer a.Close()
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a session past retention survived Open: %v", err)
	}
	if a.LastSeq("old") != 0 {
		t.Fatal("a pruned session still seeds numbering")
	}
}

func TestArchive_PrunesHourlyAndRemovesEmptiedSessions(t *testing.T) {
	root := t.TempDir()
	clk := newClock()
	a := openTest(t, root, clk, WithRetentionDays(30))
	defer a.Close()
	newRecorder(a).record("s1", synthSession(61, 1, 512)...)
	settle(a)
	clk.advance(61 * time.Minute) // idle close, then retention is due
	tick(a)
	clk.advance(31 * 24 * time.Hour)
	tick(a)
	if _, err := os.Stat(filepath.Join(root, dataDirName, dirName("s1"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an expired session survived the hourly pass: %v", err)
	}
	if a.Stats().Bytes != 0 {
		t.Fatalf("Stats().Bytes = %d after everything expired", a.Stats().Bytes)
	}
}

type failWriter struct{ err error }

func (f failWriter) Write([]byte) (int, error) { return 0, f.err }

// A write error is counted and closes the segment it hit; the session's next event opens a new
// one rather than going on into a broken stream.
func TestArchive_AWriteErrorIsCountedAndTheNextEventOpensANewSegment(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock())
	defer a.Close()
	first := true
	a.segHook = func(w *segmentWriter) {
		if first {
			w.out.w, first = failWriter{errors.New("injected")}, false
		}
	}
	r := newRecorder(a)
	r.record("s1", synthSession(62, 1, 512)...)
	tick(a) // the flush hits the failing writer
	if a.Stats().WriteErrors != 1 {
		t.Fatalf("WriteErrors = %d, want 1", a.Stats().WriteErrors)
	}
	r.record("s1", synthSession(63, 1, 512)...)
	tick(a)
	var segs int
	a.inWriter(func() {
		s := a.sessions["s1"]
		segs = len(s.meta.Segments)
		if s.w != nil {
			segs++
		}
	})
	if segs != 2 {
		t.Fatalf("%d segments, want the broken one and a fresh one", segs)
	}
}

// A full disk pauses the archive instead of failing every event against it, and the next
// retention pass — which may have freed space — resumes it.
func TestArchive_ENOSPCPausesUntilTheNextRetentionPass(t *testing.T) {
	clk := newClock()
	a := openTest(t, t.TempDir(), clk)
	defer a.Close()
	first := true
	a.segHook = func(w *segmentWriter) {
		if first {
			w.out.w, first = failWriter{syscall.ENOSPC}, false
		}
	}
	r := newRecorder(a)
	r.record("s1", synthSession(64, 1, 512)...)
	tick(a)
	if !a.Stats().Paused {
		t.Fatal("ENOSPC did not pause the archive")
	}
	before := a.Stats().DroppedEvents
	r.record("s1", synthSession(65, 1, 512)...)
	tick(a)
	if got := a.Stats().DroppedEvents - before; got != 3 {
		t.Fatalf("%d events dropped while paused, want 3", got)
	}
	clk.advance(pruneEvery)
	tick(a)
	if a.Stats().Paused {
		t.Fatal("the retention pass did not resume the archive")
	}
}

func TestStats_ReportsBytesAndBounds(t *testing.T) {
	a := openTest(t, t.TempDir(), newClock(), WithRetentionDays(9999), WithMaxBytes(12345))
	defer a.Close()
	newRecorder(a).record("s1", synthSession(66, 2, 1024)...)
	tick(a)
	st := a.Stats()
	if st.Bytes <= 0 || st.MaxBytes != 12345 || st.RetentionDays != MaxRetentionDays {
		t.Fatalf("Stats = %+v", st)
	}
}

// A rename carries the renamed entry's events, and what an earlier entry recorded under the
// same id stays under it: the store renames one entry, never the id's whole history. After
// each of the two ways an id's entry is re-created.
func TestRekeyed_MovesOnlyTheRenamedEntrysEvents(t *testing.T) {
	renames := []struct {
		name, from string
		rename     func(st *session.Store, to string)
	}{
		{"rekey", session.DefaultSessionID, func(st *session.Store, to string) { st.Rekey(session.DefaultSessionID, to) }},
	}
	earlier, entry := synthSession(60, 1, 512), synthSession(61, 1, 512) // 3 events each
	for _, rn := range renames {
		check := func(t *testing.T, root string, a *Archive, st *session.Store) {
			t.Helper()
			st.Append(rn.from, entry[0])
			rn.rename(st, "Y")
			st.Append("Y", entry[1])
			if got := a.LastSeq(rn.from); got != 3 {
				t.Errorf("LastSeq(%s) after the rename = %d, want 3: its earlier history stays", rn.from, got)
			}
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
			wantHistory(t, root, "Y", 4, 5)
			wantHistory(t, root, rn.from, 1, 2, 3)
			b := openTest(t, root, newClock())
			defer b.Close()
			if b.LastSeq("Y") != 5 || b.LastSeq(rn.from) != 3 {
				t.Fatalf("LastSeq after reopening: Y=%d %s=%d, want 5 and 3", b.LastSeq("Y"), rn.from, b.LastSeq(rn.from))
			}
		}
		t.Run(rn.name+"/after a restart", func(t *testing.T) {
			root := t.TempDir()
			a, st := restarted(t, root, newClock(), rn.from, earlier)
			defer st.Close()
			check(t, root, a, st)
		})
		t.Run(rn.name+"/after an eviction", func(t *testing.T) {
			root := t.TempDir()
			a := openTest(t, root, newClock())
			st := session.New(0, 0, 1) // one resident session: the next one evicts it
			defer st.Close()
			st.AddRecorder(a)
			for _, e := range earlier {
				st.Append(rn.from, e)
			}
			st.Append("other", synthSession(62, 1, 512)[0]) // evicts rn.from; its segment stays open
			check(t, root, a, st)
		})
	}
}

// wantHistory checks that id's archived events are numbered want, in order, and that its
// session.json summary is the fold of exactly those events.
func wantHistory(t *testing.T, root, id string, want ...uint64) {
	t.Helper()
	evs := onDisk(t, root, id)
	var got []uint64
	own := session.NewSummaryFold()
	for i := range evs {
		got = append(got, evs[i].Seq)
		own.Add(evs[i].SessionID, &evs[i])
	}
	if !slices.Equal(got, want) {
		t.Fatalf("%s holds seqs %v, want %v", id, got, want)
	}
	m, err := readMeta(filepath.Join(root, dataDirName, dirName(id)))
	if err != nil {
		t.Fatal(err)
	}
	if g, w := m.Summary.Summary(id), own.Summary(id); g.EventCount != w.EventCount || g.Title != w.Title {
		t.Fatalf("%s summary = %d events titled %q, want %d titled %q", id, g.EventCount, g.Title, w.EventCount, w.Title)
	}
}

// restarted archives events under id with one store and archive, closes both, and returns a
// reopened archive with a fresh store recording into it: a proxy restart.
func restarted(t *testing.T, root string, clk *fakeClock, id string, events []pipeline.SessionEvent, opts ...Option) (*Archive, *session.Store) {
	t.Helper()
	a := openTest(t, root, clk)
	st := session.New(0, 0, 0)
	st.AddRecorder(a)
	for _, e := range events {
		st.Append(id, e)
	}
	a.Close()
	st.Close()
	b := openTest(t, root, clk, opts...)
	st = session.New(0, 0, 0)
	st.AddRecorder(b)
	return b, st
}

// What a rename moves is decided by the events of the entry that reached disk, wherever they
// landed: an entry whose first event never reached the writer, one that reached nothing, and an
// entry renamed a second time. Each starts after a restart over 3 earlier events under default.
func TestRekeyed_MovesWhatTheEntryWrote(t *testing.T) {
	from := session.DefaultSessionID
	entry := synthSession(64, 1, 512)
	cases := []struct {
		name string
		opts []Option
		// run appends the entry's events and renames it, ending with the entry under to.
		run  func(a *Archive, st *session.Store) (to string)
		want []uint64 // to's seqs; nil for no history
	}{
		{"the first event is dropped by a full queue", []Option{WithQueueDepth(reserve + 3)},
			func(a *Archive, st *session.Store) string {
				a.gate = make(chan struct{})
				for _, e := range synthSession(65, 2, 512) {
					st.Append("filler", e) // more than the queue holds short of its reserve
				}
				st.Append(from, entry[0])
				close(a.gate)
				settle(a)
				st.Append(from, entry[1])
				st.Rekey(from, "Y")
				return "Y"
			}, []uint64{5}},
		{"the first event is dropped by a paused writer", nil,
			func(a *Archive, st *session.Store) string {
				a.paused.Store(true)
				st.Append(from, entry[0])
				settle(a)
				a.paused.Store(false)
				st.Append(from, entry[1])
				st.Rekey(from, "Y")
				return "Y"
			}, []uint64{5}},
		{"every event is dropped", nil,
			func(a *Archive, st *session.Store) string {
				a.paused.Store(true)
				st.Append(from, entry[0])
				st.Rekey(from, "Y")
				settle(a)
				return "Y"
			}, nil},
		{"the entry is renamed twice", nil,
			func(a *Archive, st *session.Store) string {
				st.Append(from, entry[0])
				st.Rekey(from, "Y")
				st.Rekey("Y", "Z")
				st.Append("Z", entry[1])
				return "Z"
			}, []uint64{4, 5}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			a, st := restarted(t, root, newClock(), from, synthSession(63, 1, 512), c.opts...)
			defer st.Close()
			to := c.run(a, st)
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
			wantHistory(t, root, from, 1, 2, 3)
			if c.want == nil {
				if _, err := os.Stat(filepath.Join(root, dataDirName, dirName(to))); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("%s has a directory, want none: %v", to, err)
				}
				return
			}
			wantHistory(t, root, to, c.want...)
		})
	}
}

// An entry created over no history carries everything to its rename, even when an earlier
// entry under the id began over history that retention has since deleted.
func TestRekeyed_AnEntryOverNoHistoryMovesWhole(t *testing.T) {
	root, clk := t.TempDir(), newClock()
	a := openTest(t, root, clk)
	st := session.New(0, 0, 1)
	st.AddRecorder(a)
	from := session.DefaultSessionID
	for _, e := range synthSession(66, 1, 512) {
		st.Append(from, e)
	}
	a.Close()
	st.Close()

	b := openTest(t, root, clk)
	st = session.New(0, 0, 1)
	defer st.Close()
	st.AddRecorder(b)
	st.Append(from, synthSession(67, 1, 512)[0]) // an entry over history: numbered from 4
	settle(b)
	clk.advance(time.Duration(defaultRetentionDays+1) * 24 * time.Hour)
	tick(b) // closes the idle segment and prunes the whole session
	if b.LastSeq(from) != 0 {
		t.Fatalf("LastSeq(%s) after retention = %d, want 0", from, b.LastSeq(from))
	}
	st.Append("other", synthSession(68, 1, 512)[0]) // evicts the entry
	st.Append(from, synthSession(69, 1, 512)[0])    // a new one, over nothing: numbered from 1
	st.Rekey(from, "Y")
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	wantHistory(t, root, "Y", 1)
}

// What a rename carries describes the entry alone, even when the history it began over has since
// been pruned away: the earlier events must not reach the claiming session's summary.
func TestRekeyed_AfterAPruneTheSummaryIsTheEntrys(t *testing.T) {
	from := session.DefaultSessionID
	entry := synthSession(71, 1, 512)
	cases := []struct {
		name  string
		prune func(a *Archive, clk *fakeClock)
	}{
		{"by retention", func(a *Archive, clk *fakeClock) {
			clk.advance(2 * 24 * time.Hour)
			tick(a)
		}},
		{"by the size bound", func(a *Archive, clk *fakeClock) {
			a.inWriter(func() { a.maxBytes = 1 })
			tick(a)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root, clk := t.TempDir(), newClock()
			a, st := restarted(t, root, clk, from, synthSession(70, 1, 512))
			defer st.Close()
			clk.advance(29 * 24 * time.Hour)
			st.Append(from, entry[0]) // an entry over history: numbered from 4
			settle(a)
			c.prune(a, clk)
			a.inWriter(func() {
				for _, seg := range a.sessions[from].meta.Segments {
					if seg.FirstSeq <= 3 {
						t.Errorf("segment %s holds earlier history; the prune did not take it", seg.File)
					}
				}
			})
			st.Rekey(from, "Y")
			st.Append("Y", entry[1])
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
			wantHistory(t, root, "Y", 4, 5)
		})
	}
}
