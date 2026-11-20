package archive

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// Review round 2, item 1: an imported session that a live request later withholds is never
// replaced by an import, also after a restart, when its Source still says imported and its
// numbering matches the disk.
func TestNoRecord_AnImportNeverReplacesAWithheldImportedSession(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock())
	if err := a.WriteSession("s1", importedEvents(4), false); err != nil {
		t.Fatalf("import: %v", err)
	}
	r := newRecorder(a)
	r.seq["s1"] = 4
	r.record("s1", flagged(synthSession(320, 1, 512))...)
	a.Close()

	a2 := openTest(t, root, newClock())
	defer a2.Close()
	if err := a2.WriteSession("s1", importedEvents(6), true); !errors.Is(err, ErrLive) {
		t.Fatalf("WriteSession(replace) on a withheld imported session = %v, want ErrLive", err)
	}
}

// Review round 2, item 3: the default bucket and a pending bucket are never withheld, even when a
// flagged event reaches the archive under their id. They hold many clients' calls.
func TestNoRecord_SharedBucketsAreNeverWithheld(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock())
	r := newRecorder(a)
	for _, id := range []string{session.DefaultSessionID, session.PendingSessionID("claude-code")} {
		r.record(id, flagged(synthSession(321, 1, 512))...)
	}
	a.Close()
	for _, id := range []string{session.DefaultSessionID, session.PendingSessionID("claude-code")} {
		if got := onDisk(t, root, id); len(got) != 3 {
			t.Fatalf("%s holds %d events, want all 3 recorded", id, len(got))
		}
	}
}

// Review round 3, item 1: praxis mode. The store mark for a hopped request (MarkNoRecord) is
// dropped when the store evicts the entry, and the gateway's ingested rows carry no flag. The
// archive must already hold its own mark by then, so those rows are withheld.
func TestNoRecord_HopMarkSurvivesEviction(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock())
	st := session.New(time.Hour, 0, 1) // one session: the next one evicts the first
	defer st.Close()
	st.AddRecorder(a)
	evs := synthSession(330, 2, 512)
	for _, e := range evs[:3] {
		st.Append("s1", e) // recorded before the opt-out
	}
	st.MarkNoRecord("s1") // a hopped request with X-Cortex-Record: off
	st.Append("s2", synthSession(331, 1, 512)[0])
	for _, e := range evs[3:] {
		st.Append("s1", e) // the gateway's rows, ingested without the flag
	}
	settle(a)
	a.Close()
	if got := onDisk(t, root, "s1"); len(got) != 3 {
		t.Fatalf("s1 holds %d events, want the 3 recorded before the mark", len(got))
	}
}

// gateWriter holds a's writer before its next op and returns an idempotent release, also
// registered as cleanup with a's Close, so a failing test never leaves the writer gated. Each
// time the writer takes an op and waits at the gate, a.held receives a value.
func gateWriter(t *testing.T, a *Archive) (release func()) {
	t.Helper()
	gate := make(chan struct{})
	a.held = make(chan struct{}, 1)
	a.gate = gate
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(func() {
		release()
		_ = a.Close()
	})
	return release
}

// fillToReserve queues events of id until the queue is at the reserve line, with the gated
// writer holding the first one, and returns how many it recorded.
func fillToReserve(t *testing.T, a *Archive, r *recorder, id string) int {
	t.Helper()
	evs := synthSession(390, 1, 512)[:1]
	r.record(id, evs...)
	<-a.held // the writer has taken it and waits at the gate
	n := 1
	for len(a.ops) < cap(a.ops)-reserve {
		r.record(id, plain(n)...)
		n++
	}
	return n
}

// plain is one unflagged event, distinct by i.
func plain(i int) []pipeline.SessionEvent {
	e := synthSession(391, 1, 512)[0]
	e.At = e.At.Add(time.Duration(i) * time.Millisecond)
	return []pipeline.SessionEvent{e}
}

// The writer's mark is the opt-out's only state, so a flagged event of a session not marked yet is
// never lost: with the queue full, reserve included, it is kept at its place and returns at once.
func TestNoRecord_AFlaggedEventIsNeverLostWhileUnmarked(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock(), WithQueueDepth(reserve+6))
	release := gateWriter(t, a)
	r := newRecorder(a)
	evs := synthSession(332, 3, 512) // 9 events
	r.record("s1", evs[:6]...)       // to the reserve
	for len(a.ops) < cap(a.ops) {    // and the reserve too
		a.Rekeyed("filler", "filler2")
	}
	r.record("s1", flagged(evs[6:7])...) // returns with the writer still gated
	release()
	settle(a)
	r.record("s1", evs[7:]...)
	a.Close()
	if got := onDisk(t, root, "s1"); len(got) != 6 {
		t.Fatalf("s1 holds %d events, want the 6 recorded before the flag", len(got))
	}
}

// clawgenti's probe on #1959, through the real store: with the writer stuck (a hung fsync, a
// stalled mount) and the queue at its reserve line, one opted-out request must not block any other
// session's traffic, nor itself. Its opt-out still lands, and no flagged event took the reserve.
func TestNoRecord_AStuckWriterNeverBlocksTheStore(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock(), WithQueueDepth(reserve+6))
	st := session.New(time.Hour, 0, 0)
	t.Cleanup(st.Close) // registered first, so it runs after gateWriter's release
	st.AddRecorder(a)
	release := gateWriter(t, a)
	evs := synthSession(392, 4, 512) // 12 events
	st.Append("s1", evs[0])
	<-a.held // the writer has taken it and waits at the gate
	n := 1
	for ; len(a.ops) < cap(a.ops)-reserve; n++ {
		st.Append("s1", evs[n])
	}
	fl := evs[n]
	fl.NoRecord = true
	other := synthSession(393, 1, 512)
	// Each returns with the writer still gated.
	st.Append("s1", fl)
	st.Append("s2", other[0])
	st.ListSessions()
	if q := len(a.ops); q > cap(a.ops)-reserve {
		t.Fatalf("queue at %d with the writer gated, past the reserve line %d: a flagged event took the reserve", q, cap(a.ops)-reserve)
	}
	a.Rekeyed("x1", "x2") // the reserve is still free for a rename
	if got := a.Stats().DroppedRenames; got != 0 {
		t.Fatalf("dropped %d renames: the reserve was taken", got)
	}
	release()
	settle(a)
	for _, e := range evs[n+1:] {
		st.Append("s1", e) // unflagged, after the opt-out
	}
	st.Append("s2", other[1])
	settle(a)
	a.Close()
	if got := onDisk(t, root, "s1"); len(got) != n {
		t.Fatalf("s1 holds %d events, want the %d recorded before the opt-out", len(got), n)
	}
	if got := onDisk(t, root, "s2"); len(got) != 1 {
		t.Fatalf("s2 holds %d events, want its row after the writer recovered", len(got))
	}
}

// A kept opt-out applies at its place in the queue: after a DELETE queued before it, which would
// otherwise remove its mark, and again after a DELETE queued between two opt-outs of one session
// (the entry adds a position). In both cases the unflagged rows that follow are withheld.
func TestNoRecord_AKeptOptOutKeepsItsPlaceAfterADelete(t *testing.T) {
	for _, tc := range []struct {
		name string
		ops  func(a *Archive)
	}{
		{"delete, then opt-out", func(a *Archive) {
			a.Deleted("s1")
			a.Withhold("s1")
		}},
		{"opt-out, delete, opt-out", func(a *Archive) {
			a.Withhold("s1")
			a.Deleted("s1")
			a.Withhold("s1")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			a := openTest(t, root, newClock(), WithQueueDepth(reserve+6))
			r := newRecorder(a)
			r.record("s1", flagged(synthSession(394, 1, 512))...)
			settle(a) // s1 is marked
			release := gateWriter(t, a)
			fillToReserve(t, a, r, "s0")
			tc.ops(a) // with the queue at the line: every Withhold is kept, not queued
			if a.optOutN.Load() != 1 {
				t.Fatalf("%d kept opt-outs, want 1 for s1", a.optOutN.Load())
			}
			release()
			if _, err := a.AwaitDelete("s1", 5*time.Second); err != nil {
				t.Fatalf("delete: %v", err)
			}
			settle(a)
			r.seq["s1"] = 0
			r.record("s1", synthSession(395, 1, 512)...) // unflagged, after the opt-out
			a.Close()
			if segs := segmentsIn(t, root, "s1"); len(segs) != 0 {
				t.Fatalf("segments %v, want none: the opt-out was applied before the delete", segs)
			}
		})
	}
}

// An unflagged row of a session that gets a queue slot while its opt-out is still kept (the
// writer has made room, but not reached the opt-out's place) is withheld: the opt-out applies
// before it.
func TestNoRecord_ARowQueuedWhileTheOptOutIsKeptIsWithheld(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock(), WithQueueDepth(reserve+6))
	r := newRecorder(a)
	release := gateWriter(t, a)
	fillToReserve(t, a, r, "s0")
	r.record("s1", flagged(plain(1))...) // no room: kept
	if a.optOutN.Load() != 1 {
		t.Fatal("precondition: the opt-out is kept")
	}
	a.gate <- struct{}{} // one op through: the writer takes the next and waits again
	<-a.held
	r.record("s1", plain(2)...) // unflagged, queued behind the opt-out's place
	if a.optOutN.Load() != 1 {
		t.Fatal("the opt-out was applied before the row was queued")
	}
	release()
	a.Close()
	if segs := segmentsIn(t, root, "s1"); len(segs) != 0 {
		t.Fatalf("segments %v, want none: a row queued while the opt-out was kept was recorded", segs)
	}
}

// One entry per session, however often it opts out while the queue is full, and none left once
// the writer has applied it.
func TestNoRecord_KeptOptOutsAreOnePerSessionAndCleared(t *testing.T) {
	a := openTest(t, t.TempDir(), newClock(), WithQueueDepth(reserve+6))
	r := newRecorder(a)
	release := gateWriter(t, a)
	fillToReserve(t, a, r, "s0")
	for range 1000 {
		a.Withhold("s1")
		r.record("s1", flagged(plain(1))...)
	}
	a.mu.Lock()
	n, tags := len(a.optOuts), len(a.optOuts["s1"])
	a.mu.Unlock()
	if n != 1 || tags != 1 {
		t.Fatalf("%d entries, s1 with %d positions; want 1 and 1", n, tags)
	}
	release()
	settle(a)
	if got := a.optOutN.Load(); got != 0 {
		t.Fatalf("%d kept opt-outs after the writer caught up, want 0", got)
	}
}

// A kept opt-out still pending at Close is written as a marker, so the session stays withheld
// after a restart. A praxis Withhold is kept the same way.
func TestNoRecord_AKeptOptOutSurvivesCloseAndRestart(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock(), WithQueueDepth(reserve+6))
	r := newRecorder(a)
	release := gateWriter(t, a)
	fillToReserve(t, a, r, "s0")
	r.record("s1", flagged(plain(1))...)
	a.Withhold("s2")
	release()
	a.Close()

	a2 := openTest(t, root, newClock())
	r2 := newRecorder(a2)
	r2.seq["s1"], r2.seq["s2"] = 1, 0
	r2.record("s1", synthSession(396, 1, 512)...)
	r2.record("s2", synthSession(397, 1, 512)...)
	a2.Close()
	for _, id := range []string{"s1", "s2"} {
		if segs := segmentsIn(t, root, id); len(segs) != 0 {
			t.Fatalf("%s: segments %v after a restart, want none", id, segs)
		}
	}
}

// Past its bound on kept opt-outs the archive fails closed: no conversation is recorded until a
// restart, and Stats says so. A shared bucket still records.
func TestNoRecord_TooManyKeptOptOutsFailClosed(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock(), WithQueueDepth(reserve+6))
	r := newRecorder(a)
	release := gateWriter(t, a)
	fillToReserve(t, a, r, "s0")
	for i := range a.depth + 1 {
		a.Withhold(fmt.Sprintf("forged-%d", i))
	}
	if !a.Stats().FailedClosed {
		t.Fatal("past the bound, the archive did not fail closed")
	}
	if got := a.optOutN.Load(); got != int64(a.depth) {
		t.Fatalf("%d kept opt-outs, want the bound %d", got, a.depth)
	}
	release()
	settle(a)
	r.record("s9", synthSession(398, 1, 512)...)
	r.record(session.DefaultSessionID, synthSession(399, 1, 512)...)
	a.Close()
	if segs := segmentsIn(t, root, "s9"); len(segs) != 0 {
		t.Fatalf("segments %v for a conversation after failing closed, want none", segs)
	}
	if got := onDisk(t, root, session.DefaultSessionID); len(got) != 3 {
		t.Fatalf("default holds %d events, want its 3", len(got))
	}
}

// Failed closed, the archive records no conversation, but a flagged event that finds room still
// writes its mark: the fail-closed state is lost at a restart, and the mark is not.
func TestNoRecord_AnOptOutAfterFailingClosedIsStillMarked(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock(), WithQueueDepth(reserve+6))
	r := newRecorder(a)
	release := gateWriter(t, a)
	fillToReserve(t, a, r, "s0")
	for i := range a.depth + 1 {
		a.Withhold(fmt.Sprintf("forged-%d", i))
	}
	if !a.Stats().FailedClosed {
		t.Fatal("precondition: the archive failed closed")
	}
	release()
	settle(a)
	r.record("s7", flagged(plain(1))...) // room again: the opt-out is admitted
	settle(a)
	a.Close()

	a2 := openTest(t, root, newClock())
	r2 := newRecorder(a2)
	r2.seq["s7"] = 1
	r2.record("s7", synthSession(400, 1, 512)...) // unflagged, after the restart
	a2.Close()
	if segs := segmentsIn(t, root, "s7"); len(segs) != 0 {
		t.Fatalf("segments %v after a restart, want none: the opt-out sent while failed closed was lost", segs)
	}
}

// A DELETE still queued when a praxis Withhold arrives: the session was marked, but the delete is
// about to remove the mark, so Withhold must not take "already marked" for granted. The rows
// ingested after it are withheld.
func TestNoRecord_WithholdWhileADeleteIsQueued(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock())
	r := newRecorder(a)
	r.record("s1", flagged(synthSession(340, 1, 512))...)
	settle(a) // marked and published
	a.gate = make(chan struct{})
	a.Deleted("s1")                              // queued, not applied yet
	a.Withhold("s1")                             // a hopped request withholds s1 again
	r.record("s1", synthSession(341, 1, 512)...) // the gateway's rows, unflagged
	close(a.gate)
	if _, err := a.AwaitDelete("s1", 5*time.Second); err != nil {
		t.Fatalf("delete: %v", err)
	}
	a.Close()
	if segs := segmentsIn(t, root, "s1"); len(segs) != 0 {
		t.Fatalf("segments %v, want none: the rows after the delete were archived", segs)
	}
}

// The same for a flagged event behind a queued CLEAR: it is delivered, and the session is marked
// again after the clear.
func TestNoRecord_FlaggedEventWhileAClearIsQueued(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock())
	r := newRecorder(a)
	r.record("s1", flagged(synthSession(342, 1, 512))...)
	settle(a)
	a.gate = make(chan struct{})
	a.Cleared()
	evs := synthSession(343, 1, 512)
	r.record("s1", flagged(evs[:1])...)
	r.record("s1", evs[1:]...)
	close(a.gate)
	if _, err := a.AwaitClear(5 * time.Second); err != nil {
		t.Fatalf("clear: %v", err)
	}
	a.Close()
	if segs := segmentsIn(t, root, "s1"); len(segs) != 0 {
		t.Fatalf("segments %v, want none: the opt-out after the clear was lost", segs)
	}
}

// Flagged events never go into the reserve: it stays for renames, deletes and clears, which a
// burst of flagged events must not crowd out. Past the reserve line they are kept, and the
// producer returns.
func TestNoRecord_FlaggedEventsLeaveTheReserveAlone(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock(), WithQueueDepth(reserve+6))
	release := gateWriter(t, a)
	r := newRecorder(a)
	fillToReserve(t, a, r, "s1")
	// More than the reserve holds: if flagged events could use the reserve, they would fill it.
	evs := make([]pipeline.SessionEvent, reserve+2)
	for i := range evs {
		evs[i].NoRecord = true
	}
	r.record("s2", evs...) // returns with the writer still gated
	if q := len(a.ops); q > cap(a.ops)-reserve {
		t.Fatalf("queue at %d with the writer gated, past the reserve line %d", q, cap(a.ops)-reserve)
	}
	a.Rekeyed("s1", "s3") // with the writer still gated: must still find a reserve slot
	if got := a.Stats().DroppedRenames; got != 0 {
		t.Fatalf("dropped %d renames: flagged events took the reserve", got)
	}
	release()
	a.Close()
}

// Review round 3: when retention leaves a withheld session with nothing, the summary of what it
// held when the store's entry began (before) goes too, so the store's list row shows no expired
// title or counts.
func TestNoRecord_MarkerOnlyDropsThePriorSummary(t *testing.T) {
	a := openTest(t, t.TempDir(), newClock())
	defer a.Close()
	r := newRecorder(a)
	r.record("s1", synthSession(346, 1, 512)...)
	settle(a)
	a.inWriter(func() {
		s := a.sessions["s1"]
		s.before = s.meta.Summary.Clone() // as begin leaves it for a new store entry
		s.meta.Summary.MarkNoRecord()
		a.markerOnly(s)
		if !onlyMark(s.before, "s1") || !onlyMark(s.meta.Summary, "s1") {
			t.Errorf("after markerOnly: before %+v, fold %+v; want only the mark",
				s.before.Summary("s1"), s.meta.Summary.Summary("s1"))
		}
	})
}

// Review: when retention expires the last segment of a withheld session, the marker it keeps
// holds only the mark, not the expired history's title and counts.
func TestNoRecord_AnExpiredHistoryLeavesOnlyTheMark(t *testing.T) {
	root := t.TempDir()
	clk := newClock()
	a := openTest(t, root, clk, WithRetentionDays(1))
	r := newRecorder(a)
	evs := synthSession(345, 1, 512)
	r.record("s1", evs[:2]...) // history before the opt-out, with a title
	settle(a)
	clk.advance(30 * time.Hour)         // past retention for the segment
	r.record("s1", flagged(evs[2:])...) // the opt-out, today
	settle(a)
	a.inWriter(func() { a.prune(clk.now()) })
	a.Close()
	m, err := readMeta(filepath.Join(root, dataDirName, dirName("s1")))
	if err != nil {
		t.Fatalf("the marker is gone: %v", err)
	}
	if sum := m.Summary.Summary("s1"); !sum.NoRecord || sum.EventCount != 0 || sum.Title != "" || len(m.Segments) != 0 {
		t.Fatalf("after retention: %+v with %d segments, want only the mark", sum, len(m.Segments))
	}
}

// Review: an opt-out that reaches the archive after the writer stopped reading the queue for Close
// is refused and counted, never queued or kept where nothing reads it.
func TestNoRecord_AnOptOutIsRefusedAfterTheWriterStopped(t *testing.T) {
	a := openTest(t, t.TempDir(), newClock())
	defer a.Close()
	a.mu.Lock()
	a.stopped = true // as the writer sets it after its drain for Close
	a.mu.Unlock()
	before := a.Stats().DroppedEvents
	a.Withhold("s1")
	if got := a.Stats().DroppedEvents; got != before+1 {
		t.Fatalf("dropped %d → %d, want the op counted as undelivered", before, got)
	}
	if n := len(a.ops); n != 0 || a.optOutN.Load() != 0 {
		t.Fatalf("%d ops queued and %d opt-outs kept after the writer stopped, want none", n, a.optOutN.Load())
	}
	a.mu.Lock()
	a.stopped = false
	a.mu.Unlock()
}

// A rename moves a marked session's mark inside the rename op, so a DELETE queued after it
// removes the new id for good: no marker comes back behind the delete.
func TestNoRecord_RenameThenDeleteLeavesNothing(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock())
	r := newRecorder(a)
	r.record("s1", flagged(synthSession(333, 1, 512))...)
	settle(a)
	a.Rekeyed("s1", "s2")
	a.Deleted("s2")
	if _, err := a.AwaitDelete("s2", 5*time.Second); err != nil {
		t.Fatalf("delete: %v", err)
	}
	settle(a)
	r.seq["s2"] = 0
	r.record("s2", synthSession(334, 1, 512)...)
	a.Close()
	if got := onDisk(t, root, "s2"); len(got) != 3 {
		t.Fatalf("s2 holds %d events after DELETE, want the 3 recorded since: the mark came back", len(got))
	}
}

// A clear removes every mark with the history: the user's explicit erase. Later unflagged events
// record again.
func TestNoRecord_ClearRemovesTheMark(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock())
	r := newRecorder(a)
	r.record("s1", flagged(synthSession(335, 1, 512))...)
	settle(a)
	a.Cleared()
	if _, err := a.AwaitClear(5 * time.Second); err != nil {
		t.Fatalf("clear: %v", err)
	}
	r.seq["s1"] = 0
	r.record("s1", synthSession(336, 1, 512)...)
	a.Close()
	if got := onDisk(t, root, "s1"); len(got) != 3 {
		t.Fatalf("s1 holds %d events after CLEAR, want the 3 recorded since", len(got))
	}
}

// Review round 2, item 4: a flagged event dropped while the archive is paused still persists the
// mark, so a restart before the session's next event keeps it withheld.
func TestNoRecord_ADroppedFlagIsPersistedBeforeARestart(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock())
	a.paused.Store(true)
	newRecorder(a).record("s1", flagged(synthSession(322, 1, 512)[:1])...)
	settle(a)
	a.Close()

	a2 := openTest(t, root, newClock())
	r := newRecorder(a2)
	r.seq["s1"] = 1
	r.record("s1", synthSession(323, 1, 512)...)
	a2.Close()
	if segs := segmentsIn(t, root, "s1"); len(segs) != 0 {
		t.Fatalf("segments %v after a restart, want none", segs)
	}
}
