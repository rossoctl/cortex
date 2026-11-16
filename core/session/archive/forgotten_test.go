package archive

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// readForgotten reads every record line of root's forget file. A missing file is no records.
func readForgotten(t *testing.T, root string) []forgetRecord {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, forgetFile))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []forgetRecord
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var r forgetRecord
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatalf("forget file line %q: %v", line, err)
		}
		out = append(out, r)
	}
	return out
}

func writeForgotten(t *testing.T, root, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, forgetFile), []byte(content), fileMode); err != nil {
		t.Fatal(err)
	}
}

func recordLine(id string, from, to time.Time) string {
	return fmt.Sprintf(`{"id":%q,"from":%d,"to":%d}`+"\n", id, from.UnixMilli(), to.UnixMilli())
}

func TestForgotten_DeleteWritesRecord(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock(), WithLedgerRetentionDays(31))
	defer a.Close()
	newRecorder(a).record("s1", synthSession(1, 2, 256)...)
	settle(a)
	sum, ok := a.Lookup("s1")
	if !ok || sum.LastRequestAt.IsZero() {
		t.Fatalf("Lookup = %+v, %v; want a session with a request", sum, ok)
	}
	a.Deleted("s1")
	if _, err := a.AwaitDelete("s1", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	recs := readForgotten(t, root)
	want := forgetRecord{ID: "s1", From: sum.CreatedAt.UnixMilli(), To: sum.LastRequestAt.UnixMilli()}
	if len(recs) != 1 || recs[0] != want {
		t.Fatalf("records = %+v, want [%+v]", recs, want)
	}
	from, to, ok := a.Forgotten("s1")
	if !ok || from.UnixMilli() != want.From || to.UnixMilli() != want.To {
		t.Fatalf("Forgotten = %v, %v, %v; want %+v", from, to, ok, want)
	}
}

func TestForgotten_ClearWritesOnePerSession(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock(), WithLedgerRetentionDays(31))
	defer a.Close()
	r := newRecorder(a)
	for i, id := range []string{"a", "b", "c"} {
		r.record(id, synthSession(int64(10+i), 1, 256)...)
	}
	settle(a)
	a.Cleared()
	if _, err := a.AwaitClear(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, rec := range readForgotten(t, root) {
		ids[rec.ID] = true
	}
	if len(ids) != 3 || !ids["a"] || !ids["b"] || !ids["c"] {
		t.Fatalf("record ids = %v, want a, b and c", ids)
	}
	if ents, err := os.ReadDir(filepath.Join(root, dataDirName)); err != nil || len(ents) != 0 {
		t.Fatalf("data/ after the clear = %v, %v; want empty", ents, err)
	}
}

func TestForgotten_DayLimitWritesRecord(t *testing.T) {
	root := t.TempDir()
	clk := newClock()
	a := openTest(t, root, clk, WithLedgerRetentionDays(40))
	newRecorder(a).record("s1", synthSession(2, 1, 256)...)
	settle(a)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	clk.advance(31 * 24 * time.Hour)
	b := openTest(t, root, clk, WithLedgerRetentionDays(40))
	defer b.Close()
	if _, ok := b.Lookup("s1"); ok {
		t.Fatal("s1 survived the day limit")
	}
	if recs := readForgotten(t, root); len(recs) != 1 || recs[0].ID != "s1" {
		t.Fatalf("records = %+v, want one for s1", recs)
	}
	if _, _, ok := b.Forgotten("s1"); !ok {
		t.Fatal("Forgotten(s1) not ok")
	}
}

func TestForgotten_SizeLimitWritesRecord(t *testing.T) {
	root := t.TempDir()
	clk := newClock()
	a := openTest(t, root, clk, WithLedgerRetentionDays(31))
	newRecorder(a).record("s1", synthSession(3, 1, 256)...)
	settle(a)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	b := openTest(t, root, clk, WithLedgerRetentionDays(31), WithMaxBytes(1))
	defer b.Close()
	if _, ok := b.Lookup("s1"); ok {
		t.Fatal("s1 survived the size limit")
	}
	if recs := readForgotten(t, root); len(recs) != 1 || recs[0].ID != "s1" {
		t.Fatalf("records = %+v, want one for s1", recs)
	}
}

func TestForgotten_NoInferenceWritesNone(t *testing.T) {
	root := t.TempDir()
	clk := newClock()
	a := openTest(t, root, clk, WithLedgerRetentionDays(31))
	defer a.Close()
	newRecorder(a).record("s1", pipeline.SessionEvent{
		Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "example.com", At: clk.now()})
	settle(a)
	a.Deleted("s1")
	if _, err := a.AwaitDelete("s1", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if recs := readForgotten(t, root); len(recs) != 0 {
		t.Fatalf("records = %+v, want none", recs)
	}
	if _, _, ok := a.Forgotten("s1"); ok {
		t.Fatal("Forgotten(s1) ok for a session with no tokens or cost")
	}
}

func TestWriteSession_ReplaceWritesNoRecord(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock(), WithLedgerRetentionDays(31))
	defer a.Close()
	if err := a.WriteSession("s1", importedEvents(4), false); err != nil {
		t.Fatal(err)
	}
	if err := a.WriteSession("s1", importedEvents(6), true); err != nil {
		t.Fatal(err)
	}
	settle(a)
	if recs := readForgotten(t, root); len(recs) != 0 {
		t.Fatalf("records = %+v, want none after a replace", recs)
	}
	if _, _, ok := a.Forgotten("s1"); ok {
		t.Fatal("Forgotten(s1) ok after a replace")
	}
}

func TestForgotten_LedgerOffWritesNone(t *testing.T) {
	root := t.TempDir()
	a := openTest(t, root, newClock())
	defer a.Close()
	r := newRecorder(a)
	r.record("a", synthSession(20, 1, 256)...)
	r.record("b", synthSession(21, 1, 256)...)
	settle(a)
	a.Deleted("a")
	if _, err := a.AwaitDelete("a", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	a.Cleared()
	if _, err := a.AwaitClear(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, forgetFile)); !os.IsNotExist(err) {
		t.Fatalf("forget file with the ledger off: %v", err)
	}
}

func TestForgotten_SecondRecordMerges(t *testing.T) {
	root := t.TempDir()
	writeForgotten(t, root, `{"id":"a","from":100,"to":200}`+"\n"+`{"id":"a","from":50,"to":300}`+"\n")
	// A clock near the records' times, so that the compaction at Open does not expire them.
	clk := &fakeClock{t: time.UnixMilli(400)}
	a := openTest(t, root, clk, WithLedgerRetentionDays(31))
	defer a.Close()
	from, to, ok := a.Forgotten("a")
	if !ok || from.UnixMilli() != 50 || to.UnixMilli() != 300 {
		t.Fatalf("Forgotten = %d, %d, %v; want 50, 300, true", from.UnixMilli(), to.UnixMilli(), ok)
	}
	if n := a.Stats().ForgetRecords; n != 1 {
		t.Fatalf("ForgetRecords = %d, want 1", n)
	}
}

func TestForgotten_CorruptLineSkippedAndCounted(t *testing.T) {
	root := t.TempDir()
	clk := newClock()
	at := clk.now().Add(-time.Hour)
	writeForgotten(t, root, recordLine("good", at, at)+"not json\n"+`{"id":"x","fr`)
	a := openTest(t, root, clk, WithLedgerRetentionDays(31))
	defer a.Close()
	if n := a.Stats().BadForgetLines; n != 2 {
		t.Fatalf("BadForgetLines = %d, want 2", n)
	}
	if _, _, ok := a.Forgotten("good"); !ok {
		t.Fatal("the good record did not load")
	}
	// A record appended after the torn line starts on a line of its own.
	newRecorder(a).record("s1", synthSession(4, 1, 256)...)
	settle(a)
	a.Deleted("s1")
	if _, err := a.AwaitDelete("s1", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, forgetFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(b), "\n") || !strings.Contains(string(b), "\n"+`{"id":"s1",`) {
		t.Fatalf("forget file = %q, want the s1 record on a line of its own", b)
	}
}

func TestForgotten_ExpireAndCompact(t *testing.T) {
	root := t.TempDir()
	clk := newClock()
	now := clk.now()
	old := now.Add(-10 * 24 * time.Hour)
	live := now.Add(-time.Hour)
	writeForgotten(t, root, recordLine("old1", old, old)+recordLine("old2", old, old)+
		recordLine("old3", old, old)+recordLine("live", live, live))
	a := openTest(t, root, clk, WithLedgerRetentionDays(3))
	defer a.Close()
	if _, _, ok := a.Forgotten("live"); !ok {
		t.Fatal("the live record did not load")
	}
	for _, id := range []string{"old1", "old2", "old3"} {
		if _, _, ok := a.Forgotten(id); ok {
			t.Fatalf("expired record %s is still served", id)
		}
	}
	if recs := readForgotten(t, root); len(recs) != 1 || recs[0].ID != "live" {
		t.Fatalf("records after the compaction = %+v, want only live", recs)
	}
}

func TestForgotten_SurvivesReopen(t *testing.T) {
	root := t.TempDir()
	clk := newClock()
	a := openTest(t, root, clk, WithLedgerRetentionDays(31))
	newRecorder(a).record("s1", synthSession(5, 1, 256)...)
	settle(a)
	a.Deleted("s1")
	if _, err := a.AwaitDelete("s1", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	b := openTest(t, root, clk, WithLedgerRetentionDays(31))
	defer b.Close()
	if _, _, ok := b.Forgotten("s1"); !ok {
		t.Fatal("Forgotten(s1) not ok after a reopen")
	}
}
