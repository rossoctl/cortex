package peerproc

// Cwd reads the current working directory of pid, as the kernel resolves it (no
// symlinks). It returns ErrNotFound when pid is 0 or below, when no such process exists,
// or when the kernel will not describe it (another user's process). It returns
// ErrUnsupported on platforms with no implementation.
func Cwd(pid int32) (string, error) { return cwd(pid) }
