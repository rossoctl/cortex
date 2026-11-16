package archive

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/model"
)

// importedEvents is n events of a synthetic session, marked as imported, with Seq unset.
func importedEvents(n int) []model.SessionEvent {
	evs := synthSession(71, (n+1)/2, 512)[:n]
	for i := range evs {
		evs[i].Seq, evs[i].SessionID = 0, ""
		evs[i].Source = model.ImportedSource("claude-code")
	}
	return evs
}

// seqs is the Seq of every archived event of id, oldest first. It ticks first, so a live
// event in an open segment is flushed and readable.
func seqs(t *testing.T, a *Archive, id string) []uint64 {
	t.Helper()
	tick(a)
	var out []uint64
	if _, _, err := a.Page(id, 0, 1000, func(e *model.SessionEvent) bool {
		out = append([]uint64{e.Seq}, out...)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func wantSeqs(t *testing.T, got []uint64, n int) {
	t.Helper()
	if len(got) != n {
		t.Fatalf("seqs %v, want 1..%d", got, n)
	}
	for i, s := range got {
		if s != uint64(i+1) {
			t.Fatalf("seqs %v, want 1..%d", got, n)
		}
	}
}

func TestWriteSession_WritesAndNumbers(t *testing.T) {
	a := openTest(t, t.TempDir(), newClock())
	defer a.Close()
	if err := a.WriteSession("s1", importedEvents(4), false); err != nil {
		t.Fatal(err)
	}
	wantSeqs(t, seqs(t, a, "s1"), 4)
	sum, ok := a.Lookup("s1")
	if !ok {
		t.Fatal("Lookup found nothing")
	}
	if sum.EventCount != 4 || sum.Source != "imported:claude-code" {
		t.Fatalf("Lookup = EventCount %d, Source %q", sum.EventCount, sum.Source)
	}
	if _, ok := a.Lookup("nope"); ok {
		t.Fatal("Lookup found a session that was never written")
	}
}

func TestWriteSession_ReplaceDeletesFirst(t *testing.T) {
	a := openTest(t, t.TempDir(), newClock())
	defer a.Close()
	if err := a.WriteSession("s1", importedEvents(4), false); err != nil {
		t.Fatal(err)
	}
	if err := a.WriteSession("s1", importedEvents(6), true); err != nil {
		t.Fatal(err)
	}
	wantSeqs(t, seqs(t, a, "s1"), 6)
	if sum, _ := a.Lookup("s1"); sum.EventCount != 6 {
		t.Fatalf("EventCount %d, want 6", sum.EventCount)
	}
}

func TestWriteSession_RefusesLiveSession(t *testing.T) {
	a := openTest(t, t.TempDir(), newClock())
	defer a.Close()
	live := synthSession(72, 1, 256)[:1]
	newRecorder(a).record("s1", live...)
	settle(a)
	err := a.WriteSession("s1", importedEvents(4), true)
	if !errors.Is(err, ErrLive) {
		t.Fatalf("err = %v, want ErrLive", err)
	}
	got := seqs(t, a, "s1")
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("live session changed: seqs %v", got)
	}
	if sum, _ := a.Lookup("s1"); sum.Source != "" || sum.EventCount != 1 {
		t.Fatalf("live summary changed: %+v", sum)
	}
}

// A live event for a new id, recorded just before the write, must win: the write never adds a
// second event 1 beside it.
func TestWriteSession_RefusesAfterLiveRecord(t *testing.T) {
	for _, gated := range []bool{false, true} {
		a := openTest(t, t.TempDir(), newClock())
		if gated {
			a.gate = make(chan struct{}) // the live event is still queued when the write is asked for
		}
		newRecorder(a).record("s1", synthSession(73, 1, 256)[:1]...)
		errc := make(chan error, 1)
		go func() { errc <- a.WriteSession("s1", importedEvents(4), false) }()
		if gated {
			close(a.gate)
		}
		err := <-errc
		if !errors.Is(err, ErrLive) && !errors.Is(err, ErrExists) {
			t.Fatalf("gated=%v: err = %v, want ErrLive or ErrExists", gated, err)
		}
		if got := seqs(t, a, "s1"); len(got) != 1 || got[0] != 1 {
			t.Fatalf("gated=%v: seqs %v, want only the live event 1", gated, got)
		}
		a.Close()
	}
}

// The store numbered a live event that the archive does not hold (dropped or still queued):
// an import must not take its seqs.
func TestWriteSession_RefusesWhenLiveEventNotOnDisk(t *testing.T) {
	a := openTest(t, t.TempDir(), newClock())
	defer a.Close()
	a.mu.Lock()
	a.lastSeq["s1"] = 1
	a.mu.Unlock()
	for _, replace := range []bool{false, true} {
		if err := a.WriteSession("s1", importedEvents(4), replace); !errors.Is(err, ErrLive) {
			t.Fatalf("replace=%v: err = %v, want ErrLive", replace, err)
		}
	}
	if got := a.LastSeq("s1"); got != 1 {
		t.Fatalf("LastSeq = %d, want 1", got)
	}
}

// A live event queued for an imported session, not yet written, must not be numbered into a
// replacement's seqs.
func TestWriteSession_RefusesReplaceWithLiveEventQueued(t *testing.T) {
	a := openTest(t, t.TempDir(), newClock())
	defer a.Close()
	if err := a.WriteSession("s1", importedEvents(4), false); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.lastSeq["s1"] = 5 // what Record does before its event reaches the writer
	a.mu.Unlock()
	if err := a.WriteSession("s1", importedEvents(6), true); !errors.Is(err, ErrLive) {
		t.Fatalf("err = %v, want ErrLive", err)
	}
}

func TestWriteSession_PlainWriteRefusesExisting(t *testing.T) {
	a := openTest(t, t.TempDir(), newClock())
	defer a.Close()
	if err := a.WriteSession("s1", importedEvents(4), false); err != nil {
		t.Fatal(err)
	}
	if err := a.WriteSession("s1", importedEvents(6), false); !errors.Is(err, ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
	wantSeqs(t, seqs(t, a, "s1"), 4)
}

func TestWriteSession_LastSeqContinues(t *testing.T) {
	a := openTest(t, t.TempDir(), newClock())
	defer a.Close()
	if err := a.WriteSession("s1", importedEvents(4), false); err != nil {
		t.Fatal(err)
	}
	if got := a.LastSeq("s1"); got != 4 {
		t.Fatalf("LastSeq = %d, want 4", got)
	}
}

func TestArchive_ImportedMarkSurvivesReopen(t *testing.T) {
	root := t.TempDir()
	clk := newClock()
	a := openTest(t, root, clk)
	if err := a.WriteSession("s1", importedEvents(4), false); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	b := openTest(t, root, clk)
	defer b.Close()
	sum, ok := b.Lookup("s1")
	if !ok || sum.Source != "imported:claude-code" || sum.EventCount != 4 {
		t.Fatalf("after reopen: ok %v, %+v", ok, sum)
	}
}

func TestWriteSession_UpdatesBytes(t *testing.T) {
	a := openTest(t, t.TempDir(), newClock())
	defer a.Close()
	before := a.Stats().Bytes
	if err := a.WriteSession("s1", importedEvents(4), false); err != nil {
		t.Fatal(err)
	}
	if after := a.Stats().Bytes; after <= before {
		t.Fatalf("Bytes %d -> %d, want growth", before, after)
	}
}

func TestWriteSession_ClosedArchive(t *testing.T) {
	a := openTest(t, t.TempDir(), newClock())
	a.Close()
	if err := a.WriteSession("s1", importedEvents(2), false); err == nil {
		t.Fatal("a closed archive accepted a write")
	}
}

// A failed disk write erases what part of the session reached disk and gives the numbering back.
func TestWriteSession_FailedWriteLeavesNothing(t *testing.T) {
	a := openTest(t, t.TempDir(), newClock())
	defer a.Close()
	a.segHook = func(w *segmentWriter) { w.out.w = failWriter{syscall.ENOSPC} }
	if err := a.WriteSession("s1", importedEvents(4), false); err == nil {
		t.Fatal("a failed write returned no error")
	}
	if _, ok := a.Lookup("s1"); ok {
		t.Fatal("Lookup found the half-written session")
	}
	if got := a.LastSeq("s1"); got != 0 {
		t.Fatalf("LastSeq = %d, want 0", got)
	}
}

// A replace that fails after the delete forgets the id: a retry is not refused as live.
func TestWriteSession_FailedReplaceForgetsID(t *testing.T) {
	a := openTest(t, t.TempDir(), newClock())
	defer a.Close()
	if err := a.WriteSession("s1", importedEvents(4), false); err != nil {
		t.Fatal(err)
	}
	a.segHook = func(w *segmentWriter) { w.out.w = failWriter{syscall.ENOSPC} }
	if err := a.WriteSession("s1", importedEvents(6), true); err == nil {
		t.Fatal("a failed replace returned no error")
	}
	if got := a.LastSeq("s1"); got != 0 {
		t.Fatalf("LastSeq = %d, want 0", got)
	}
	a.segHook = nil
	a.paused.Store(false)
	if err := a.WriteSession("s1", importedEvents(6), false); err != nil {
		t.Fatalf("retry after a failed replace: %v", err)
	}
}

// A replace on a paused archive keeps the old copy: the import's source may be gone.
func TestWriteSession_PausedReplaceKeepsOldCopy(t *testing.T) {
	a := openTest(t, t.TempDir(), newClock())
	defer a.Close()
	if err := a.WriteSession("s1", importedEvents(4), false); err != nil {
		t.Fatal(err)
	}
	a.paused.Store(true)
	if err := a.WriteSession("s1", importedEvents(6), true); err == nil {
		t.Fatal("a paused archive accepted a replace")
	}
	if sum, ok := a.Lookup("s1"); !ok || sum.EventCount != 4 {
		t.Fatalf("after a paused replace: ok %v, EventCount %d, want the old 4", ok, sum.EventCount)
	}
	if got := a.LastSeq("s1"); got != 4 {
		t.Fatalf("LastSeq = %d, want 4", got)
	}
	a.paused.Store(false)
	wantSeqs(t, seqs(t, a, "s1"), 4)
}

// An imported segment is as old as its newest event, so the size bound deletes it before a live
// session written before the import, and the usage replay of the last hours leaves it out.
func TestWriteSession_SegmentAgeIsItsEvents(t *testing.T) {
	clk := newClock()
	a := openTest(t, t.TempDir(), clk)
	defer a.Close()
	newRecorder(a).record("live", synthSession(72, 2, 512)...)
	settle(a)
	a.inWriter(func() { a.closeWriter(a.sessions["live"]) })
	clk.advance(time.Hour)
	newest := clk.now().Add(-60 * 24 * time.Hour)
	evs := importedEvents(4)
	for i := range evs {
		evs[i].At = newest.Add(time.Duration(i-len(evs)+1) * time.Second)
	}
	if err := a.WriteSession("old", evs, false); err != nil {
		t.Fatal(err)
	}
	if src := a.ReplaySources(clk.now().Add(-6 * time.Hour)); len(src.srcs) != 1 || src.srcs[0].id != "live" {
		t.Errorf("replay sources %v, want only the live segment", src.srcs)
	}
	var lastWrite time.Time
	a.inWriter(func() {
		lastWrite = a.sessions["old"].meta.Segments[0].LastWrite
		a.retainDays, a.maxBytes = 0, a.sessions["live"].meta.Segments[0].Bytes
		a.prune(clk.now())
	})
	if !lastWrite.Equal(newest) {
		t.Errorf("imported LastWrite = %v, want the newest event %v", lastWrite, newest)
	}
	if _, ok := a.Lookup("old"); ok {
		t.Error("the size bound kept the imported session")
	}
	if _, ok := a.Lookup("live"); !ok {
		t.Error("the size bound deleted the live session before the older import")
	}
}

// The session list sorts by UpdatedAt, so an import carries its events' times, also after a reopen.
func TestWriteSession_SessionTimesAreItsEvents(t *testing.T) {
	clk, dir := newClock(), t.TempDir()
	a := openTest(t, dir, clk)
	newest := clk.now().Add(-48 * time.Hour) // inside retention, so the reopen keeps it
	evs := importedEvents(4)
	for i := range evs {
		evs[i].At = newest.Add(time.Duration(i-len(evs)+1) * time.Second)
	}
	oldest := evs[0].At
	if err := a.WriteSession("old", evs, false); err != nil {
		t.Fatal(err)
	}
	check := func(when string, a *Archive) {
		t.Helper()
		for _, s := range a.Summaries() {
			if s.ID == "old" {
				if !s.CreatedAt.Equal(oldest) || !s.UpdatedAt.Equal(newest) {
					t.Errorf("%s: CreatedAt, UpdatedAt = %v, %v, want %v, %v", when, s.CreatedAt, s.UpdatedAt, oldest, newest)
				}
				return
			}
		}
		t.Errorf("%s: no summary for the import", when)
	}
	check("after the import", a)
	a.Close()
	b := openTest(t, dir, clk)
	defer b.Close()
	check("after a reopen", b)
}

// A failed write whose erase also fails keeps its numbering: the partial copy is still on disk, so
// a live event must not number into its seqs.
func TestWriteSession_FailedEraseKeepsNumbering(t *testing.T) {
	a := openTest(t, t.TempDir(), newClock())
	defer a.Close()
	a.segHook = func(w *segmentWriter) {
		w.out.w = failWriter{syscall.ENOSPC}
		if err := os.Chmod(a.data, 0o500); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { os.Chmod(a.data, 0o700) })
	if err := a.WriteSession("s1", importedEvents(4), false); err == nil {
		t.Fatal("a failed write returned no error")
	}
	if got := a.LastSeq("s1"); got != 4 {
		t.Fatalf("LastSeq = %d, want the 4 reserved", got)
	}
}
