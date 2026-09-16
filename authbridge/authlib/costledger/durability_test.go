package costledger

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestReadDay_ACrashFragmentSwallowsTheNextRowAndTheCountSaysOne pins the corrected
// torn-write claim, because the claim it replaces was reassuring and false.
//
// appendBytes fences a torn write with a newline so the fragment cannot swallow the next
// append — and that only runs while THIS PROCESS IS STILL ALIVE. On power loss or SIGKILL
// nothing runs: the file keeps an unterminated fragment, the next process appends onto it,
// and the scanner reads the two as ONE undecodable line. Two rows are missing and the
// caveat for that day says one.
//
// So SkippedLines is a FLOOR on rows lost. That is worth a test rather than a comment,
// because the number reaches an operator through /v1/usage's degraded block and the
// difference between "at least one row" and "exactly one row" is the difference between
// looking further and stopping.
func TestReadDay_ACrashFragmentSwallowsTheNextRowAndTheCountSaysOne(t *testing.T) {
	dir := t.TempDir()
	day := time.Date(2026, 9, 13, 9, 0, 0, 0, testZone)
	s, err := newStore(dir, 30, testZone)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}

	// A day file as a crash leaves it: one complete row, then a row cut off mid-write with
	// no trailing newline and no fence, because the process that was writing it died.
	intact := line(day, "gw", "m", 1, 10, 5, 100)
	fragment := `{"at":"` + day.Add(time.Minute).Format(time.RFC3339Nano) + `","endpoint":"gw","costMic`
	if werr := os.WriteFile(s.path(day), []byte(intact+"\n"+fragment), fileMode); werr != nil {
		t.Fatalf("write: %v", werr)
	}

	// A later process appends one ordinary row, which lands on the fragment's line.
	next := Row{At: day.Add(2 * time.Minute), Endpoint: "gw", Model: "m"}
	next.CostMicros, next.Requests = 300, 1
	if _, aerr := s.append([]Row{next}); aerr != nil {
		t.Fatalf("append: %v", aerr)
	}

	rows, issues, rerr := s.readDay(dayOf(day))
	if rerr != nil {
		t.Fatalf("readDay: %v", rerr)
	}
	if len(rows) != 1 || rows[0].CostMicros != 100 {
		t.Fatalf("read %d rows (%+v), want only the one complete row that preceded the "+
			"fragment", len(rows), rows)
	}
	// THE POINT: two rows are gone — the fragment and next — and the count says one. Stated
	// as a RANGE so the test asserts the guarantee rather than the arithmetic of this
	// fixture: the count may under-report, and must never over-report.
	//
	// IT USED TO BE UNREACHABLE, and that is worth recording because the shape is so
	// plausible. A `skippedLines != 1` Fatalf sat above the inequality and hard-pinned the
	// very value it compared, so `rowsLost <= issues.skippedLines` was `2 <= 1` — dead code
	// under a comment explaining what it was for. Anything that made the count exact would
	// have tripped the Fatalf first, with a message about a fixture detail instead of the one
	// below about the documentation that would then be wrong. Both bounds are now live.
	const rowsLost = 2
	if issues.skippedLines < 1 {
		t.Fatalf("skippedLines = %d over a day file with a torn line, want at least 1: the "+
			"fence stopped counting, so a short total now reports as a clean one",
			issues.skippedLines)
	}
	if issues.skippedLines >= rowsLost {
		t.Errorf("this fixture lost %d rows and reported %d skipped lines; the assertion this "+
			"test exists for is that the count is a FLOOR — if it is now exact, the fence must "+
			"have started covering the crash case, and the docs in appendBytes and Caveats "+
			"that call it a floor need correcting too", rowsLost, issues.skippedLines)
	}
}

// TestSyncDir_MakesANewDayFileNameDurableOnThisPlatform is the guard for the directory
// fsync writeLines now does.
//
// Two ways it could go wrong and neither would show up in any other test. A filesystem
// that refuses fsync on a directory would make the FIRST append of every day return an
// error — reported by Flush and Close, and logged at Warn once a day, over rows that are
// perfectly readable. And a syncDir that quietly did nothing would leave the hole it was
// added to close.
func TestSyncDir_MakesANewDayFileNameDurableOnThisPlatform(t *testing.T) {
	dir := t.TempDir()
	if err := syncDir(dir); err != nil {
		t.Errorf("syncDir(%s) = %v; a platform that cannot fsync a directory would make the "+
			"first append of every day report a durability error over readable rows", dir, err)
	}
	if err := syncDir(filepath.Join(dir, "does-not-exist")); err == nil {
		t.Error("syncDir on a missing directory returned nil; a failure to make the name " +
			"durable has to be reported, since that is the whole reason the call exists")
	}

	// And the path it is called from: creating a day file reports no error, and appending
	// to the one that now exists reports none either.
	day := time.Date(2026, 9, 13, 9, 0, 0, 0, testZone)
	s, err := newStore(dir, 30, testZone)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	for i, when := range []time.Time{day, day.Add(time.Minute)} {
		lost, aerr := s.append([]Row{{At: when, Endpoint: "gw"}})
		if aerr != nil || lost != 0 {
			t.Fatalf("append %d: lost %d rows, err %v", i, lost, aerr)
		}
	}
	if _, serr := os.Stat(s.path(day)); serr != nil {
		t.Errorf("day file missing after two appends: %v", serr)
	}
}

// TestWriteLines_SyncsTheDirectoryOncePerNewDayFile pins the directory fsync, which is
// otherwise unobservable: an fsync has no userspace effect, so without this a future edit
// could drop it and every other test in this package would still pass.
//
// TWO PROPERTIES, and the second is why the existence check exists. The name of a NEW day
// file lives in the parent directory and is not covered by the file's own fsync, so power
// loss could otherwise leave the day's first append synced and the file absent. Every later
// append that day needs nothing, and paying an extra fsync per minute for it would be a
// cost on the writer goroutine with no durability to show for it.
func TestWriteLines_SyncsTheDirectoryOncePerNewDayFile(t *testing.T) {
	dir := t.TempDir()
	var synced []string
	real := syncNewDayFileName
	syncNewDayFileName = func(d string) error {
		synced = append(synced, d)
		return real(d)
	}
	t.Cleanup(func() { syncNewDayFileName = real })

	day := time.Date(2026, 9, 13, 9, 0, 0, 0, testZone)
	path := filepath.Join(dir, "2026-09-13.jsonl")
	for i := 0; i < 2; i++ {
		if lost, err := writeLines(path, []Row{{At: day.Add(time.Duration(i) * time.Minute)}}); err != nil || lost != 0 {
			t.Fatalf("writeLines %d: lost %d, err %v", i, lost, err)
		}
	}
	if len(synced) != 1 || synced[0] != dir {
		t.Errorf("directory syncs = %v, want exactly one of %s: the first append of a day has "+
			"to make the file's NAME durable, and the appends after it must not pay for it again",
			synced, dir)
	}
}

// TestWriteLines_ADirectorySyncFailureIsNotALostRow pins which of the two signals a
// directory-sync failure travels on.
//
// The rows ARE in the file and every reader will see them; what cannot be claimed is that
// the file survives power loss. Writer.write already separates those cases — "zero lost
// with a non-nil error is possible ... and is deliberately not counted as a drop" — and
// counting this as a lost row would make Dropped(), the one exported completeness signal,
// cry wolf over readable spend.
func TestWriteLines_ADirectorySyncFailureIsNotALostRow(t *testing.T) {
	dir := t.TempDir()
	real := syncNewDayFileName
	syncNewDayFileName = func(string) error { return errUnsyncableDir }
	t.Cleanup(func() { syncNewDayFileName = real })

	day := time.Date(2026, 9, 13, 9, 0, 0, 0, testZone)
	path := filepath.Join(dir, "2026-09-13.jsonl")
	lost, err := writeLines(path, []Row{{At: day, Endpoint: "gw"}})
	if !errors.Is(err, errUnsyncableDir) {
		t.Errorf("err = %v, want the directory sync failure reported; a durability claim that "+
			"cannot be made has to reach Flush and Close", err)
	}
	if lost != 0 {
		t.Errorf("lost = %d, want 0: the row is in the file and readable", lost)
	}
	rows, issues, rerr := readRowsFrom(t, path)
	if rerr != nil || len(rows) != 1 || issues.skippedLines != 0 {
		t.Errorf("day file holds %d rows (%+v, err %v), want the one row that was written",
			len(rows), issues, rerr)
	}
}

// errUnsyncableDir stands in for a filesystem that will not fsync a directory.
var errUnsyncableDir = errors.New("test: directory sync refused")

// readRowsFrom decodes a day file by path, for the tests that write one directly.
func readRowsFrom(t *testing.T, path string) ([]Row, dayIssues, error) {
	t.Helper()
	s, err := newStore(filepath.Dir(path), 30, testZone)
	if err != nil {
		return nil, dayIssues{}, err
	}
	base := filepath.Base(path)
	day, ok := s.dayFromName(base)
	if !ok {
		t.Fatalf("dayFromName(%q) refused a name this test wrote", base)
	}
	return s.readDay(day)
}
