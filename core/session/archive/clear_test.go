package archive

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

func trashIn(t *testing.T, root string) []string {
	t.Helper()
	ents, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), trashPrefix) {
			out = append(out, e.Name())
		}
	}
	return out
}

// A clear takes effect at its place in the queue: events queued before it are deleted with the
// rest of the history, and events after it start a fresh one. The trash is gone when it returns.
func TestClear_EventsQueuedBeforeAreGoneAndAfterStartFresh(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock())
	defer a.Close()
	r := newRecorder(a)
	r.record("old", synthSession(81, 2, 512)...)
	settle(a)
	tick(a) // old is on disk
	a.gate = make(chan struct{})
	r.record("s1", synthSession(82, 1, 512)...) // queued, not yet written, when the clear comes
	a.Cleared()
	r.record("s1", synthSession(83, 1, 512)...) // after the clear: numbered on from 4
	close(a.gate)

	res, err := a.AwaitClear(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.Sessions != 2 || res.Bytes <= 0 {
		t.Fatalf("ClearResult = %+v, want 2 sessions and their bytes", res)
	}
	settle(a)
	tick(a)
	if _, err := os.Stat(filepath.Join(root, dataDirName, dirName("old"))); !os.IsNotExist(err) {
		t.Fatalf("old survived the clear: %v", err)
	}
	evs := onDisk(t, root, "s1")
	if len(evs) != 3 || evs[0].Seq != 4 {
		t.Fatalf("s1 after the clear holds %d events from seq %d, want the 3 recorded after it, from 4",
			len(evs), firstSeq(evs))
	}
	rows := a.Summaries()
	if len(rows) != 1 || rows[0].ID != "s1" || rows[0].EventCount != 3 {
		t.Fatalf("Summaries after the clear = %+v, want s1 alone with 3 events", rows)
	}
	if tr := trashIn(t, root); len(tr) != 0 {
		t.Fatalf("trash left behind: %v", tr)
	}
}

// The numbering the store reads is forgotten for every id that recorded nothing after the
// clear, so a session started under it later numbers from 1 — and kept for one that did, whose
// events after the clear are already numbered on from it.
func TestClear_ForgetsTheNumberingOfEverySessionNotRecordedSince(t *testing.T) {
	a := openTest(t, t.TempDir(), newClock())
	defer a.Close()
	r := newRecorder(a)
	r.record("quiet", synthSession(84, 1, 512)...)
	r.record("busy", synthSession(85, 1, 512)...)
	a.Cleared()
	if a.LastSeq("quiet") != 3 {
		t.Fatalf("LastSeq(quiet) = %d before the clear ran, want 3: the disk still holds it", a.LastSeq("quiet"))
	}
	r.record("busy", synthSession(86, 1, 512)...)
	if _, err := a.AwaitClear(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	if n := a.LastSeq("quiet"); n != 0 {
		t.Fatalf("LastSeq(quiet) = %d after the clear, want 0", n)
	}
	if n := a.LastSeq("busy"); n != 6 {
		t.Fatalf("LastSeq(busy) = %d after the clear, want 6: it recorded 4..6 after it", n)
	}
}

// A clear is never the op a full queue drops: like a rename, it goes into the reserve.
func TestClear_UsesTheReserveWhenTheQueueIsFull(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock(), WithQueueDepth(reserve+6))
	defer a.Close()
	a.gate = make(chan struct{})
	newRecorder(a).record("s1", synthSession(87, 34, 512)...) // 102 events: more than the whole queue
	a.Cleared()
	close(a.gate)
	if _, err := a.AwaitClear(5 * time.Second); err != nil {
		t.Fatalf("a clear behind a full queue failed: %v", err)
	}
	if rows := a.Summaries(); len(rows) != 0 {
		t.Fatalf("Summaries after the clear = %+v", rows)
	}
}

// A crash between the rename and the delete leaves the history in a trash directory nothing
// reads. The next Open finishes the delete.
func TestOpen_DeletesLeftoverTrash(t *testing.T) {
	root := t.TempDir()
	trash := filepath.Join(root, trashPrefix+"1700000000000000000")
	if err := os.MkdirAll(filepath.Join(trash, "abc"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trash, "abc", metaFile), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := openTest(t, root, newClock())
	defer a.Close()
	if tr := trashIn(t, root); len(tr) != 0 {
		t.Fatalf("Open left trash behind: %v", tr)
	}
}

// When the rename that empties the archive fails, nothing on disk changed and nothing is reset:
// the history is still served, the clear reports the failure, and the session's numbering runs
// on rather than restarting under events already on disk.
func TestClear_AFailedRenameKeepsEverythingAndReportsIt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permission this test relies on")
	}
	root := t.TempDir()
	a := openTest(t, root, newClock())
	defer a.Close()
	r := newRecorder(a)
	r.record("s1", synthSession(88, 1, 512)...)
	settle(a)
	release := blockRenames(t, root, filepath.Join(root, dataDirName, dirName("s1")))

	a.Cleared()
	if _, err := a.AwaitClear(5 * time.Second); err == nil {
		t.Fatal("a clear whose rename failed reported success")
	}
	release()
	if rows := a.Summaries(); len(rows) != 1 || rows[0].ID != "s1" {
		t.Fatalf("Summaries after a failed clear = %+v, want s1 kept", rows)
	}
	if n := a.LastSeq("s1"); n != 3 {
		t.Fatalf("LastSeq(s1) after a failed clear = %d, want 3", n)
	}
	r.record("s1", synthSession(89, 1, 512)...)
	a.Close() // writes the session.json naming the segment opened since
	if evs := onDisk(t, root, "s1"); len(evs) != 6 || evs[5].Seq != 6 {
		t.Fatalf("s1 holds %d events after a failed clear, want 6 numbered 1..6", len(evs))
	}
}

func firstSeq(evs []pipeline.SessionEvent) uint64 {
	if len(evs) == 0 {
		return 0
	}
	return evs[0].Seq
}
