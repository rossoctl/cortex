//go:build unix

package claude

import (
	"os"
	"path/filepath"
	"syscall"
)

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
// Blocking, with no timeout. A harvest holds this for the length of one scan, and the failure mode
// of a timeout here is the lost update this exists to prevent; a caller that cannot wait should not
// be harvesting. A crashed holder releases on process exit, since the kernel owns the lock.
//
// Errors are returned rather than swallowed so the caller can proceed UNLOCKED instead of refusing
// to harvest: a filesystem that cannot flock (some network mounts) should still get titles, on the
// same best-effort footing as before this existed.
func lockMetadata(path string) (func(), error) {
	lockPath := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // path derived from the metadata path
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		// Unlock before close, though closing the descriptor would release it anyway: being
		// explicit keeps the pairing readable, and the lock file is deliberately left in place —
		// removing it races another process that has it open.
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
