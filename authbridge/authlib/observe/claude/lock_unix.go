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
// THE TWO COSTS ARE NOT SYMMETRIC, which is what sets the size. Too long only delays a
// harvest that was going to be wrong anyway — the wedged case this bound exists for, where
// no wait of any length succeeds. Too short corrupts a HEALTHY run: expiry means proceeding
// unlocked, and an unlocked harvest can have its whole contribution erased by another run's
// rename. So the deadline has to sit far above the slowest legitimate wait, and the penalty
// for overshooting is measured in seconds of a background goroutine nobody is watching.
//
// SIZED AGAINST A QUEUE, not a single scan, because the wait is the queue ahead of you and
// not the holder alone. That was the sizing error in the first version of this: 2s was chosen
// as "many times one scan" from a ~3ms one-file measurement, and it fell over as soon as
// several harvests contended on a slow machine. Six concurrent harvests of a padded tree took
// ~190ms serialized on a developer laptop and blew straight past 2s on a shared CI runner —
// the package's own concurrency test failed, with five of six children losing every entry.
// A runner is not an exotic environment; it is the slowest machine this code routinely runs on
// and therefore the one that sets the number.
//
// A VAR RATHER THAN A CONST only so the tests can shrink it: waiting the real deadline twice
// would add a minute to the package for no extra coverage, and a test that asserts the logic at
// 40ms asserts exactly the same logic. Nothing outside the tests assigns it.
//
// 30s is deliberately far past any queue this file can produce. `abctl observe` harvests one
// tree per tick, `read-claude-sessions` is one process, and the realistic worst case is a
// handful of viewers plus a manual run — a queue of seconds, not minutes, even derated for a
// loaded runner. What 30s buys is that reaching it means no wait would have worked.
var lockTimeout = 30 * time.Second

// lockPoll is how often acquisition is retried inside lockTimeout.
//
// Polled rather than blocking because the two are exclusive in this API: syscall.Flock
// either blocks forever (LOCK_EX) or returns at once (LOCK_NB), and there is no
// deadline variant. 20ms adds at most 20ms to an uncontended handoff, and costs 1500
// cheap syscalls across a full 30s timeout — paid only by a harvest that is already
// losing, since a lock that frees up is acquired on the next tick.
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
// What the bound costs, and why the deadline is generous rather than tight: past it two harvests
// interleave, and the loser's entries can be erased wholesale by the winner's rename. That is the
// very lost update this lock exists to prevent, so expiry must mean "no wait would have worked"
// and never "this machine is slow today". recoverConcurrentEntries is a weaker backstop than it
// looks — a SINGLE re-read after saving, which by its own documentation cannot converge when more
// than one rename lands inside its window, i.e. exactly the many-writer case a short deadline
// creates. It does not cover for a deadline that fires under ordinary contention.
//
// So: unlocked is strictly better than wedged, and strictly worse than locked — which makes the
// bound worth having and worth sizing so that only the wedged case ever reaches it. See
// lockTimeout for the measurements that set the number.
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
