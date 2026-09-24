//go:build unix

package claude

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// lockTimeout bounds how long a harvest waits for the metadata lock before proceeding
// without it.
//
// SIZED AGAINST A SCAN, not against a guess: a harvest holds the lock for the length of
// one transcript scan, measured at ~3ms for a one-file tree and well under a second for
// the ~180-session tree a laptop accumulates. Two seconds is therefore many times the
// longest legitimate wait, so expiry means the holder is wedged rather than busy.
//
// The cost of it being too short is a lost update, which recoverConcurrentEntries
// narrows; the cost of it being too long is the bug it exists to fix — no titles at all
// while `abctl observe` polls a lock nobody will release.
const lockTimeout = 2 * time.Second

// lockPoll is how often acquisition is retried inside lockTimeout.
//
// Polled rather than blocking because the two are exclusive in this API: syscall.Flock
// either blocks forever (LOCK_EX) or returns at once (LOCK_NB), and there is no
// deadline variant. 20ms costs at most 100 cheap syscalls across the whole timeout and
// adds at most 20ms to an uncontended handoff.
const lockPoll = 20 * time.Millisecond

// lockMetadata takes an exclusive advisory lock covering the read-modify-write of the metadata
// file, and returns the release.
//
// NEEDED, not belt-and-braces. The recovery pass below the save was the only protection and it
// does not hold: measured, two concurrent Harvests over distinct config dirs finish with 2 of 6
// sessions on disk, and six with the same. The pass is explicitly a SINGLE re-read, so it cannot
// converge when more than one rename lands inside its own window — os.Rename makes the last
// writer total, and `abctl observe` harvesting by default means two viewers at once is ordinary
// rather than exotic.
//
// syscall.Flock rather than a dependency: it is stdlib, and the alternative was promoting a
// primitive for one call site. Advisory and per-open-file-description, which is exactly the scope
// wanted — a sibling lock file next to the metadata, so the lock survives the atomic rename that
// replaces the metadata file itself. Locking the metadata file would lock an inode the rename is
// about to detach, protecting nothing.
//
// BOUNDED BY lockTimeout, then it gives up and lets the caller proceed unlocked. This used to
// block indefinitely, on the reasoning that a caller who cannot wait should not be harvesting.
// The case that reasoning did not cover is a holder that never releases — not a crash, which the
// kernel cleans up on process exit, but a process still alive and stuck. `abctl observe` harvests
// on a timer, so every later attempt queued behind the same lock and the viewer showed no titles
// at all, indefinitely, with nothing on screen to say why.
//
// What the bound costs: past the deadline two harvests can interleave, and the loser's entries can
// be dropped by the winner's rename. That is the lost update this lock exists to prevent, now
// possible again in the one case where the alternative was no titles ever.
// recoverConcurrentEntries narrows it — it re-reads after saving and takes back what another run
// wrote — but it is a single re-read, so it cannot converge against a run that renames inside its
// window. Unlocked is strictly worse than locked and strictly better than wedged.
//
// Errors are returned rather than swallowed so the caller can proceed UNLOCKED instead of refusing
// to harvest: a filesystem that cannot flock (some network mounts) should still get titles, on the
// same best-effort footing as before this existed. A timeout joins that path, reported as
// ErrLockTimeout so the two are distinguishable.
//
// The release func is nil on every error return, which the caller relies on to decide whether to
// defer it — see Harvest's `if lerr == nil` gate. Returning a no-op func alongside an error would
// make that gate look optional, and it is not.
func lockMetadata(path string) (func(), error) {
	lockPath := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // path derived from the metadata path
	if err != nil {
		return nil, err
	}
	// LOCK_NB polled to a deadline. EWOULDBLOCK (EAGAIN on Linux, where they are the same errno)
	// is the one retryable answer: it means held, not broken. Every other errno is a real failure
	// and returns at once, exactly as the blocking call used to.
	//
	// UNTESTED BRANCH, said out loud rather than left as a silent hole: no test distinguishes this
	// discrimination from "retry on every errno", because provoking a non-EWOULDBLOCK flock error
	// needs a filesystem that refuses flock outright (some network mounts) — not something a unit
	// test can conjure. A mutation that retries every errno passes the whole suite. What it would
	// cost in production is the timeout spent sleeping against an error that will never change,
	// then reported as a lock timeout rather than as the ENOLCK it was.
	deadline := time.Now().Add(lockTimeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, err
		}
		if !time.Now().Before(deadline) {
			// Closed here too. The caller proceeds unlocked and never sees this descriptor, so
			// leaking it would leak one per harvest — and `abctl observe` harvests on a timer.
			_ = f.Close()
			return nil, fmt.Errorf("%w after %s", ErrLockTimeout, lockTimeout)
		}
		time.Sleep(lockPoll)
	}
	return func() {
		// Unlock before close, though closing the descriptor would release it anyway: being
		// explicit keeps the pairing readable, and the lock file is deliberately left in place —
		// removing it races another process that has it open.
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
