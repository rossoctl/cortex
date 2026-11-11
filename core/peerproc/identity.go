package peerproc

// Identity names one process for its whole lifetime, the way the kernel does: a pid
// alone does not, since pids are reused. BootID is the kernel's boot identifier, so an
// identity from before a reboot never matches a process after it. The rest is the
// platform's own start record:
//
//   - macOS: UniqueID is the kernel's 64-bit unique process id, which no other process
//     is given until the next boot, and StartSeconds/StartMicroseconds the start time.
//   - Linux: StartTicks is field 22 of /proc/<pid>/stat, clock ticks after boot.
//
// The fields not used on a platform are zero. These are the values Codex records as a
// pid file's processIdentity (codex-rs/app-server-daemon/src/backend/pid_identity.rs),
// which is why they are kept raw rather than turned into a time.Time: equality with what
// Codex wrote is the point, and a conversion could only lose that.
type Identity struct {
	BootID            string
	UniqueID          uint64
	StartSeconds      uint64
	StartMicroseconds uint64
	StartTicks        uint64
	// Zombie is true when the process has exited and is waiting for its parent to
	// collect it. It still holds its pid and identity, but it runs nothing.
	Zombie bool
}

// ProcessIdentity reads pid's Identity. ErrNotFound when no such process exists or the
// kernel will not describe it; ErrUnsupported on platforms with no implementation. On an
// error the Identity may still carry the fields that were read — on macOS, the BootID and
// UniqueID of another user's process, whose start time the kernel withholds.
func ProcessIdentity(pid int32) (Identity, error) { return processIdentity(pid) }
