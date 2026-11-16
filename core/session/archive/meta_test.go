package archive

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

func newMeta(id string) *sessionMeta {
	return &sessionMeta{ID: id, CreatedAt: time.Now().UTC().Round(0), UpdatedAt: time.Now().UTC().Round(0), Summary: session.NewSummaryFold()}
}

func TestMeta_RoundTrips(t *testing.T) {
	dir := filepath.Join(t.TempDir(), dirName("s1"))
	m := newMeta("s1")
	evs := synthSession(31, 2, 1024)
	for i := range evs {
		m.Summary.Add("s1", &evs[i])
	}
	m.Segments = []SegmentInfo{{File: "20261001-090000.jsonl.zst", FirstSeq: 1, LastSeq: 6, Events: 6, Bytes: 1234, LastWrite: time.Now().UTC().Round(0)}}
	if err := writeMeta(dir, m); err != nil {
		t.Fatal(err)
	}
	got, err := readMeta(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.V != formatVersion || got.ID != "s1" || len(got.Segments) != 1 || got.Segments[0] != m.Segments[0] ||
		got.Summary.Summary("s1").EventCount != 6 {
		t.Fatalf("read back %+v", got)
	}
}

// A crash between writing the temporary file and renaming it leaves the old session.json whole.
func TestMeta_ACrashedWriteLeavesTheOldFileWhole(t *testing.T) {
	dir := filepath.Join(t.TempDir(), dirName("s1"))
	if err := writeMeta(dir, newMeta("s1")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, metaTmp), []byte(`{"v":1,"id":"tor`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readMeta(dir)
	if err != nil || got.ID != "s1" {
		t.Fatalf("readMeta = %+v, %v", got, err)
	}
	if err := writeMeta(dir, newMeta("s1")); err != nil {
		t.Fatalf("a leftover temporary file blocked the next write: %v", err)
	}
}

func TestMeta_ModeIs0600(t *testing.T) {
	skipWithoutUnixModes(t)
	dir := filepath.Join(t.TempDir(), dirName("s1"))
	if err := writeMeta(dir, newMeta("s1")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, metaFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, err %v", fi.Mode().Perm(), err)
	}
}

// One unreadable session must not cost the others: it is skipped and counted.
func TestLoadMetas_SkipsUnreadableAndIgnoresStrayFiles(t *testing.T) {
	data := t.TempDir()
	if err := writeMeta(filepath.Join(data, dirName("good")), newMeta("good")); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(data, dirName("bad"))
	os.MkdirAll(bad, 0o700)
	os.WriteFile(filepath.Join(bad, metaFile), []byte("not json"), 0o600)
	os.WriteFile(filepath.Join(data, "stray.txt"), []byte("x"), 0o600)

	got, skipped, err := loadMetas(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].meta.ID != "good" || skipped != 1 {
		t.Fatalf("loaded %d (skipped %d), want 1 good and 1 skipped", len(got), skipped)
	}
}

// writeSegmentIn writes evs as one closed segment in dir and returns its info.
func writeSegmentIn(t *testing.T, dir string, opened time.Time, evs []pipeline.SessionEvent) SegmentInfo {
	t.Helper()
	w, err := createSegment(dir, opened)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if err := w.append(e, opened); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.close(); err != nil {
		t.Fatal(err)
	}
	return w.info()
}

// A crash can leave a segment session.json never heard of — opened after the last write of the
// file. Reconciling finds it, reads its seq range, and folds its events into the summary.
func TestReconcile_AddsASegmentMissingFromTheMeta(t *testing.T) {
	dir := filepath.Join(t.TempDir(), dirName("synth"))
	evs := synthSession(32, 4, 1024)
	known := writeSegmentIn(t, dir, time.Date(2026, 10, 1, 9, 0, 0, 0, time.Local), evs[:6])
	m := newMeta("synth")
	m.Segments = []SegmentInfo{known}
	for i := range evs[:6] {
		m.Summary.Add("synth", &evs[i])
	}
	writeSegmentIn(t, dir, time.Date(2026, 10, 1, 10, 0, 0, 0, time.Local), evs[6:])

	changed, err := reconcile(dir, m)
	if err != nil || !changed {
		t.Fatalf("reconcile changed=%v err=%v, want a change", changed, err)
	}
	if len(m.Segments) != 2 || m.Segments[1].FirstSeq != 7 || m.Segments[1].LastSeq != 12 {
		t.Fatalf("segments = %+v", m.Segments)
	}
	if n := m.Summary.Summary("synth").EventCount; n != 12 {
		t.Fatalf("summary counts %d events, want 12", n)
	}
}

// The segment open at a crash grew after session.json last recorded it. Its seq range must be
// read from the file, or the store would number new events into ones already on disk.
func TestReconcile_RescansAStaleLastSegmentAndFoldsOnlyWhatItMissed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), dirName("synth"))
	evs := synthSession(33, 4, 1024)
	full := writeSegmentIn(t, dir, time.Date(2026, 10, 1, 9, 0, 0, 0, time.Local), evs)
	m := newMeta("synth")
	stale := full
	stale.LastSeq, stale.Events, stale.Bytes = 6, 6, full.Bytes/2
	m.Segments = []SegmentInfo{stale}
	for i := range evs[:6] {
		m.Summary.Add("synth", &evs[i])
	}

	changed, err := reconcile(dir, m)
	if err != nil || !changed {
		t.Fatalf("reconcile changed=%v err=%v, want a change", changed, err)
	}
	if got := m.Segments[0]; got.LastSeq != 12 || got.Events != 12 || got.Bytes != full.Bytes {
		t.Fatalf("segment = %+v, want LastSeq 12, 12 events, %d bytes", got, full.Bytes)
	}
	if n := m.Summary.Summary("synth").EventCount; n != 12 {
		t.Fatalf("summary counts %d events, want 12 (6 folded twice would be 18)", n)
	}
}

func TestReconcile_LeavesAnAccurateMetaAlone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), dirName("synth"))
	evs := synthSession(34, 2, 1024)
	info := writeSegmentIn(t, dir, time.Date(2026, 10, 1, 9, 0, 0, 0, time.Local), evs)
	m := newMeta("synth")
	m.Segments = []SegmentInfo{info}
	if changed, err := reconcile(dir, m); err != nil || changed {
		t.Fatalf("reconcile changed=%v err=%v, want no change", changed, err)
	}
}
