package archive

import (
	"errors"
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

// The writer's mark is the opt-out's only state, so a flagged event of a session not marked yet is
// never dropped: with the queue full, reserve included, it waits for the writer instead.
func TestNoRecord_AFlaggedEventIsNeverDroppedWhileUnmarked(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock(), WithQueueDepth(reserve+6))
	a.gate = make(chan struct{})
	r := newRecorder(a)
	evs := synthSession(332, 3, 512) // 9 events
	r.record("s1", evs[:6]...)       // to the reserve
	for len(a.ops) < cap(a.ops) {    // and the reserve too
		a.Rekeyed("filler", "filler2")
	}
	dropped := a.Stats().DroppedEvents
	done := make(chan struct{})
	go func() {
		r.record("s1", flagged(evs[6:7])...)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("a flagged event of an unmarked session returned with the queue full: it was dropped")
	case <-time.After(200 * time.Millisecond):
	}
	close(a.gate)
	<-done
	if got := a.Stats().DroppedEvents; got != dropped {
		t.Fatalf("dropped events %d → %d, want the flagged one delivered", dropped, got)
	}
	settle(a)
	r.record("s1", evs[7:]...)
	a.Close()
	if got := onDisk(t, root, "s1"); len(got) != 6 {
		t.Fatalf("s1 holds %d events, want the 6 recorded before the flag", len(got))
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

// Flagged events wait below the reserve, never in it: the reserve stays for renames, deletes and
// clears, which a burst of flagged events must not crowd out.
func TestNoRecord_FlaggedEventsLeaveTheReserveAlone(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock(), WithQueueDepth(reserve+6))
	a.gate = make(chan struct{})
	r := newRecorder(a)
	r.record("s1", synthSession(344, 2, 512)[:6]...) // up to the reserve
	// More than the reserve holds, counting the slot the gated writer has already taken off
	// the queue: if flagged events could use the reserve, they would fill it.
	evs := make([]pipeline.SessionEvent, reserve+2)
	for i := range evs {
		evs[i].NoRecord = true
	}
	waiting := make(chan struct{})
	var once sync.Once
	a.deliverWait = func() { once.Do(func() { close(waiting) }) }
	done := make(chan struct{})
	go func() {
		r.record("s2", evs...)
		close(done)
	}()
	select {
	case <-waiting: // a flagged event reached the wait for room
	case <-done:
		// Every flagged event was queued without waiting: they took the reserve.
	case <-time.After(10 * time.Second):
		t.Fatal("the flagged producer never reached deliver's wait")
	}
	a.Rekeyed("s1", "s3") // with the writer still gated: must still find a reserve slot
	if got := a.Stats().DroppedRenames; got != 0 {
		t.Fatalf("dropped %d renames: flagged events took the reserve", got)
	}
	close(a.gate)
	<-done
	a.Close()
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

// Review: an op that reaches deliver after the writer stopped reading the queue for Close is
// refused and counted, never queued where nothing reads it.
func TestNoRecord_DeliverRefusesAfterTheWriterStopped(t *testing.T) {
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
	if n := len(a.ops); n != 0 {
		t.Fatalf("%d ops queued after the writer stopped, want none", n)
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
