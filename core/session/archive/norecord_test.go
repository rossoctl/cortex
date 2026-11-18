package archive

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// flagged marks every event as asking not to be recorded, as the listeners do when a request
// carries X-Cortex-Record: off.
func flagged(evs []pipeline.SessionEvent) []pipeline.SessionEvent {
	for i := range evs {
		evs[i].NoRecord = true
	}
	return evs
}

// segmentsIn lists the segment files under id's directory.
func segmentsIn(t *testing.T, root, id string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, dataDirName, dirName(id)))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl.zst") {
			out = append(out, e.Name())
		}
	}
	return out
}

// A flagged session writes no segment, only a session.json that marks it as not recorded and
// holds nothing else: no events, no title, no tokens.
func TestNoRecord_WritesOnlyAContentFreeMarker(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock())
	newRecorder(a).record("s1", flagged(synthSession(301, 2, 512))...)
	a.Close()

	if segs := segmentsIn(t, root, "s1"); len(segs) != 0 {
		t.Fatalf("segments %v, want none", segs)
	}
	m, err := readMeta(filepath.Join(root, dataDirName, dirName("s1")))
	if err != nil {
		t.Fatalf("no marker: %v", err)
	}
	sum := m.Summary.Summary("s1")
	if !sum.NoRecord || sum.EventCount != 0 || sum.Title != "" || sum.TotalTokens != 0 || len(m.Segments) != 0 {
		t.Fatalf("marker = %+v with %d segments, want noRecord and nothing else", sum, len(m.Segments))
	}
}

// Header-less events filed to a withheld session carry no flag. They are withheld too.
func TestNoRecord_LaterUnflaggedEventsAreWithheld(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock())
	r := newRecorder(a)
	evs := synthSession(302, 2, 512)
	r.record("s1", flagged(evs[:2])...)
	r.record("s1", evs[2:]...)
	a.Close()
	if segs := segmentsIn(t, root, "s1"); len(segs) != 0 {
		t.Fatalf("segments %v, want none", segs)
	}
}

// The opt-out applies from the first flagged event on and never backwards: what the archive
// already held stays (a session-id header is client-asserted, so a request naming another
// session must not erase it), and nothing after the flag is added.
func TestNoRecord_KeepsWhatCameBeforeTheFlagAndAddsNothing(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock())
	r := newRecorder(a)
	evs := synthSession(303, 2, 512)
	r.record("s1", evs[:2]...)
	r.record("s1", flagged(evs[2:3])...)
	r.record("s1", evs[3:]...)
	a.Close()
	got := onDisk(t, root, "s1")
	if len(got) != 2 || got[0].Seq != 1 || got[1].Seq != 2 {
		t.Fatalf("s1 holds %d events, want the 2 recorded before the flag", len(got))
	}
	m, err := readMeta(filepath.Join(root, dataDirName, dirName("s1")))
	if err != nil {
		t.Fatal(err)
	}
	if !m.Summary.NoRecord() {
		t.Fatal("the session is not marked after the flag")
	}
}

// A withheld session stays withheld when the store renames it, on the path that rebuilds the
// fold from segments (an entry re-created after the marker, then renamed whole).
func TestNoRecord_RenameKeepsTheMark(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock())
	r := newRecorder(a)
	r.record("s1", flagged(synthSession(309, 1, 512))...)
	settle(a)
	a.inWriter(func() { a.begin("s1", 3) })
	a.inWriter(func() { a.rename("s1", "s2", 3) })
	// Right after the rename and before any event of s2, so the in-memory mark cannot stand in
	// for a fold that lost it: the summary under the new id, and session.json on disk.
	var marked bool
	a.inWriter(func() { s := a.sessions["s2"]; marked = s != nil && s.meta.Summary.NoRecord() })
	if !marked {
		t.Fatal("the renamed session's summary is not marked")
	}
	if m, err := readMeta(filepath.Join(root, dataDirName, dirName("s2"))); err != nil || !m.Summary.NoRecord() {
		t.Fatalf("session.json under the new id: %v, marked %v", err, err == nil && m.Summary.NoRecord())
	}
	r.seq["s2"] = 3
	r.record("s2", synthSession(310, 1, 512)...)
	a.Close()
	if segs := segmentsIn(t, root, "s2"); len(segs) != 0 {
		t.Fatalf("segments %v under the new id, want none", segs)
	}
	a2 := openTest(t, root, newClock())
	defer a2.Close()
	if err := a2.WriteSession("s2", importedEvents(4), false); !errors.Is(err, ErrLive) {
		t.Fatalf("WriteSession(s2) after the rename = %v, want ErrLive", err)
	}
}

// A flagged event the writer never sees (here, while the archive is paused) still marks the
// session, so the header-less events after it are withheld.
func TestNoRecord_ADroppedFlaggedEventStillMarks(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock())
	r := newRecorder(a)
	evs := synthSession(311, 1, 512)
	a.paused.Store(true)
	r.record("s1", flagged(evs[:1])...)
	settle(a)
	a.paused.Store(false)
	r.record("s1", evs[1:]...)
	a.Close()
	if segs := segmentsIn(t, root, "s1"); len(segs) != 0 {
		t.Fatalf("segments %v, want none", segs)
	}
}

// The marker keeps the opt-out across a restart, when the store's own mark is gone: an
// unflagged event for the id is still withheld.
func TestNoRecord_SurvivesARestart(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock())
	newRecorder(a).record("s1", flagged(synthSession(304, 1, 512))...)
	a.Close()

	a2 := openTest(t, root, newClock())
	r := newRecorder(a2)
	r.seq["s1"] = a2.LastSeq("s1")
	r.record("s1", synthSession(305, 1, 512)...)
	a2.Close()
	if segs := segmentsIn(t, root, "s1"); len(segs) != 0 {
		t.Fatalf("segments %v after a restart, want none", segs)
	}
}

// An import refuses a withheld session, also after a restart: otherwise agentop import would
// bring it in from the agent's own transcript.
func TestNoRecord_AnImportRefusesTheSession(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock())
	newRecorder(a).record("s1", flagged(synthSession(306, 1, 512))...)
	a.Close()

	a2 := openTest(t, root, newClock())
	defer a2.Close()
	for _, replace := range []bool{false, true} {
		if err := a2.WriteSession("s1", importedEvents(4), replace); !errors.Is(err, ErrLive) {
			t.Fatalf("WriteSession(replace=%v) = %v, want ErrLive", replace, err)
		}
	}
}

// Another session is untouched by one that opted out.
func TestNoRecord_OtherSessionsAreRecorded(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock())
	r := newRecorder(a)
	r.record("s1", flagged(synthSession(307, 1, 512))...)
	r.record("s2", synthSession(308, 1, 512)...)
	a.Close()
	if got := onDisk(t, root, "s2"); len(got) != 3 {
		t.Fatalf("s2 holds %d events, want 3", len(got))
	}
	for _, e := range onDisk(t, root, "s2") {
		if e.NoRecord {
			t.Fatalf("a recorded event carries NoRecord: seq %d", e.Seq)
		}
	}
}
