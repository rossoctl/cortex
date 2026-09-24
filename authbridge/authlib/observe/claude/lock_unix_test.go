//go:build unix

package claude

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// metadataLockPath is where lockMetadata puts its sibling lock file.
//
// Duplicated from lock_unix.go on purpose: a test that derived the path by calling the
// product code could not catch the path moving, which is the thing that would silently
// stop the lock from serialising anything.
func metadataLockPath(t *testing.T, path string) string {
	t.Helper()
	return filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".lock")
}

// holdMetadataLock takes the lock from the test process and releases it on cleanup.
//
// Flock is per-open-file-description, not per-process, so a second descriptor here does
// contend with the product's — which is what makes an in-process test of this possible at
// all, without the subprocess machinery TestHarvest_ConcurrentRunsLoseNothing needs.
func holdMetadataLock(t *testing.T, path string) {
	t.Helper()
	lockPath := metadataLockPath(t, path)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		t.Fatalf("could not take the lock the test depends on holding: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	})
}

// shortenLockTimeout shrinks the deadline for one test and restores it after.
//
// The deadline's production VALUE is a judgement about slow machines (see lockTimeout); what
// these tests check is the behaviour at it — that expiry happens, is reported, and returns no
// unlock func. That behaviour is identical at 40ms, and waiting the real 30s twice would add a
// minute to the package to re-learn nothing.
func shortenLockTimeout(t *testing.T) {
	t.Helper()
	was := lockTimeout
	lockTimeout = 40 * time.Millisecond
	t.Cleanup(func() { lockTimeout = was })
}

// A held lock is waited for, then given up on — not waited for forever. This is the
// wedged-holder case: before the timeout, `abctl observe` queued every harvest behind a
// lock nobody would release and showed no titles at all, indefinitely.
func TestLockMetadata_TimesOutAndReportsIt(t *testing.T) {
	shortenLockTimeout(t)
	path := filepath.Join(t.TempDir(), "session-metadata.json")
	holdMetadataLock(t, path)

	start := time.Now()
	unlock, err := lockMetadata(path)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrLockTimeout) {
		if unlock != nil {
			unlock()
		}
		t.Fatalf("err = %v, want it to wrap ErrLockTimeout", err)
	}
	// Nil, not a no-op: Harvest's `if lerr == nil` gate is what keeps this from being
	// called, and a non-nil func here would make that gate look optional.
	if unlock != nil {
		t.Error("unlock is non-nil alongside an error")
	}
	if elapsed < lockTimeout {
		t.Errorf("gave up after %s, before the %s deadline", elapsed, lockTimeout)
	}
	// Generous: this asserts it is bounded at all, not that the sleep is precise.
	if elapsed > 4*lockTimeout {
		t.Errorf("took %s, far past the %s deadline", elapsed, lockTimeout)
	}
}

// An uncontended harvest does not claim a timeout. Guards the field against being set
// unconditionally, which would make it useless as a signal.
func TestHarvest_UncontendedDoesNotReportALockTimeout(t *testing.T) {
	metadataHome(t)
	cfg := filepath.Join(t.TempDir(), "claude")
	writeSessionTranscript(t, filepath.Join(cfg, "projects", "-p"), "s1.jsonl",
		`{"type":"ai-title","aiTitle":"t"}`)

	res, err := Harvest(Options{ConfigDir: cfg, Merge: true})
	if err != nil {
		t.Fatalf("Harvest: %v", err)
	}
	if res.LockTimedOut {
		t.Error("LockTimedOut is true with nothing holding the lock")
	}
}

// An uncontended lock is still taken, and released, at once.
func TestLockMetadata_UncontendedIsFast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session-metadata.json")

	start := time.Now()
	unlock, err := lockMetadata(path)
	if err != nil {
		t.Fatalf("lockMetadata: %v", err)
	}
	if unlock == nil {
		t.Fatal("unlock is nil with no error")
	}
	if elapsed := time.Since(start); elapsed >= lockTimeout {
		t.Errorf("uncontended acquisition took %s, want well under %s", elapsed, lockTimeout)
	}
	unlock()

	// Released, so it can be taken again. Without this the test would pass against a
	// release that does nothing.
	again, err := lockMetadata(path)
	if err != nil {
		t.Fatalf("second lockMetadata after release: %v", err)
	}
	again()
}

// THE POINT OF THE TIMEOUT, at the level the bug was reported at: a wedged holder no
// longer stops the titles from appearing. The harvest proceeds unlocked and writes.
func TestHarvest_ProceedsWhenTheLockIsHeld(t *testing.T) {
	shortenLockTimeout(t)
	metadataHome(t)
	cfg := filepath.Join(t.TempDir(), "claude")
	writeSessionTranscript(t, filepath.Join(cfg, "projects", "-p"), "s1.jsonl",
		`{"type":"ai-title","aiTitle":"t"}`)
	path, err := SessionMetadataPath()
	if err != nil {
		t.Fatal(err)
	}
	holdMetadataLock(t, path)

	res, err := Harvest(Options{ConfigDir: cfg, Merge: true})
	if err != nil {
		t.Fatalf("Harvest: %v", err)
	}
	if got := res.Meta["s1"].Title; got != "t" {
		t.Errorf("Meta[s1].Title = %q, want the harvest to have run unlocked", got)
	}
	if got := readMetadataFile(t, path)["s1"].Title; got != "t" {
		t.Errorf("on disk Title = %q, want the harvest to have written", got)
	}
	// Reported, not silent: an unlocked harvest can lose its entries to a concurrent rename, and
	// without this the only failure the lock exists to prevent has no diagnosis.
	if !res.LockTimedOut {
		t.Error("LockTimedOut is false after the harvest ran unlocked")
	}
}

// A flock failure that is NOT a timeout is recorded too, and the harvest still runs.
//
// The default: arm this covers was missing: a lock that failed for any reason other than the
// deadline left LockTimedOut false and nothing else set, so the one state where a concurrent
// rename can erase every entry a run wrote was also the one state a caller could not report.
//
// Provoked through a metadata directory that cannot be written, which makes the lock FILE
// uncreatable — a real case (read-only home, restrictive mode) and the only non-timeout lock
// failure a unit test can produce without a filesystem that refuses flock outright.
func TestHarvest_NonTimeoutLockFailureIsRecorded(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory mode this test relies on")
	}
	metadataHome(t)
	cfg := filepath.Join(t.TempDir(), "claude")
	writeSessionTranscript(t, filepath.Join(cfg, "projects", "-p"), "s1.jsonl",
		`{"type":"ai-title","aiTitle":"t"}`)
	path, err := SessionMetadataPath()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// 0o500: readable and traversable, so the harvest can still try, but no new file can be
	// created in it — which is what the lock needs.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	res, _ := Harvest(Options{ConfigDir: cfg, Merge: true})
	// The save fails too — the directory is unwritable, that is the point — so the error is not
	// asserted. What must hold is that the lock failure was RECORDED rather than swallowed.
	if res.LockFailed == "" {
		t.Error("LockFailed is empty after the lock could not be created")
	}
	// Not mislabelled as a timeout: nobody was holding it, and saying so would send a reader
	// looking for a process that does not exist.
	if res.LockTimedOut {
		t.Error("LockTimedOut is true for a failure that was not a timeout")
	}
}

// The mirror: a lock that works reports neither field. Without this the pair above would pass
// against code that set LockFailed unconditionally.
func TestHarvest_SuccessfulLockReportsNeitherFailure(t *testing.T) {
	metadataHome(t)
	cfg := filepath.Join(t.TempDir(), "claude")
	writeSessionTranscript(t, filepath.Join(cfg, "projects", "-p"), "s1.jsonl",
		`{"type":"ai-title","aiTitle":"t"}`)

	res, err := Harvest(Options{ConfigDir: cfg, Merge: true})
	if err != nil {
		t.Fatalf("Harvest: %v", err)
	}
	if res.LockFailed != "" {
		t.Errorf("LockFailed = %q with a working lock", res.LockFailed)
	}
	if res.LockTimedOut {
		t.Error("LockTimedOut is true with a working lock")
	}
}
