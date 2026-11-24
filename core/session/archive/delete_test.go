package archive

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/session"
)

// The store finds Deleters by type assertion, so a drifted signature would silently stop
// erasing. This makes it a compile error instead.
var _ session.Deleter = (*Archive)(nil)

// A delete removes one session's directory, index entry and numbering, and nothing else.
func TestDelete_RemovesOneSessionAndLeavesTheOthers(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock())
	defer a.Close()
	r := newRecorder(a)
	r.record("gone", synthSession(91, 2, 512)...)
	r.record("kept", synthSession(92, 1, 512)...)
	settle(a)
	tick(a) // both are on disk

	a.Deleted("gone")
	res, err := a.AwaitDelete("gone", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if res.Sessions != 1 || res.Bytes <= 0 {
		t.Fatalf("ClearResult = %+v, want 1 session and its bytes", res)
	}
	if _, err := os.Stat(filepath.Join(root, dataDirName, dirName("gone"))); !os.IsNotExist(err) {
		t.Fatalf("gone's directory survived: %v", err)
	}
	if rows := a.Summaries(); len(rows) != 1 || rows[0].ID != "kept" {
		t.Fatalf("Summaries = %+v, want kept alone", rows)
	}
	if n := a.LastSeq("gone"); n != 0 {
		t.Fatalf("LastSeq(gone) = %d after its delete, want 0", n)
	}
	if n := a.LastSeq("kept"); n != 3 {
		t.Fatalf("LastSeq(kept) = %d, want 3 untouched", n)
	}
	if tr := trashIn(t, root); len(tr) != 0 {
		t.Fatalf("trash left behind: %v", tr)
	}
}

// A delete takes effect at its place in the queue: events queued before it go with the history,
// events after it start a fresh one — numbered on from the old last seq, which is why that
// numbering is kept while they are recorded.
func TestDelete_EventsQueuedBeforeAreGoneAndAfterStartFresh(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock())
	defer a.Close()
	r := newRecorder(a)
	r.record("s1", synthSession(93, 1, 512)...)
	settle(a)
	a.gate = make(chan struct{})
	r.record("s1", synthSession(94, 1, 512)...) // queued, not yet written, when the delete comes
	a.Deleted("s1")
	r.record("s1", synthSession(95, 1, 512)...) // after the delete: numbered on from 7
	close(a.gate)

	if _, err := a.AwaitDelete("s1", time.Hour); err != nil {
		t.Fatal(err)
	}
	settle(a)
	tick(a)
	evs := onDisk(t, root, "s1")
	if len(evs) != 3 || evs[0].Seq != 7 {
		t.Fatalf("s1 after the delete holds %d events from seq %d, want the 3 recorded after it, from 7",
			len(evs), firstSeq(evs))
	}
	if n := a.LastSeq("s1"); n != 9 {
		t.Fatalf("LastSeq(s1) = %d, want 9: events after the delete keep their numbers", n)
	}
}

// When the rename that moves the session aside fails, nothing on disk changed and nothing is
// forgotten: the history is still served, and the delete reports the failure.
func TestDelete_AFailedRenameKeepsTheHistoryAndReportsIt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permission this test relies on")
	}
	root := t.TempDir()
	a := openTest(t, root, newClock())
	defer a.Close()
	newRecorder(a).record("s1", synthSession(96, 1, 512)...)
	settle(a)
	blockRenames(t, root, filepath.Join(root, dataDirName, dirName("s1")))

	a.Deleted("s1")
	if _, err := a.AwaitDelete("s1", time.Hour); err == nil {
		t.Fatal("a delete whose rename failed reported success")
	}
	if rows := a.Summaries(); len(rows) != 1 || rows[0].ID != "s1" {
		t.Fatalf("Summaries after a failed delete = %+v, want s1 kept", rows)
	}
	if n := a.LastSeq("s1"); n != 3 {
		t.Fatalf("LastSeq(s1) after a failed delete = %d, want 3", n)
	}
}

// A delete is never the op a full queue drops: like a rename and a clear, it goes into the
// reserve.
func TestDelete_UsesTheReserveWhenTheQueueIsFull(t *testing.T) {
	a := openTest(t, t.TempDir(), newClock(), WithQueueDepth(reserve+6))
	defer a.Close()
	a.gate = make(chan struct{})
	newRecorder(a).record("s1", synthSession(97, 34, 512)...) // 102 events: more than the whole queue
	a.Deleted("s1")
	close(a.gate)
	if _, err := a.AwaitDelete("s1", time.Hour); err != nil {
		t.Fatalf("a delete behind a full queue failed: %v", err)
	}
	if rows := a.Summaries(); len(rows) != 0 {
		t.Fatalf("Summaries after the delete = %+v", rows)
	}
}

// An id the archive never held removes nothing and is not an error: the API answers 404 from it.
func TestDelete_AnIDTheArchiveNeverHeldRemovesNothing(t *testing.T) {
	a := openTest(t, t.TempDir(), newClock())
	defer a.Close()
	a.Deleted("never")
	res, err := a.AwaitDelete("never", time.Hour)
	if err != nil || res.Sessions != 0 {
		t.Fatalf("AwaitDelete(never) = %+v, %v; want nothing removed and no error", res, err)
	}
}

// AwaitDelete gives up after its timeout rather than hanging the API on a stalled writer.
func TestAwaitDelete_TimesOutWhileTheWriterIsHeld(t *testing.T) {
	a := openTest(t, t.TempDir(), newClock())
	defer a.Close()
	a.gate = make(chan struct{})
	a.Deleted("s1")
	if _, err := a.AwaitDelete("s1", time.Nanosecond); err == nil {
		t.Fatal("AwaitDelete returned no error while the delete was still queued")
	}
	close(a.gate)
}
