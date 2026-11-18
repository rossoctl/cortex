package archive

import (
	"errors"
	"testing"
	"time"

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
