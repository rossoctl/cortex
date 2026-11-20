//go:build !windows

package main

import (
	"errors"
	"os"
	"syscall"
)

// The process and file primitives agentop needs, for Unix. proc_windows.go has the Windows
// half (#1520).

// detached starts a child in a session of its own, so it outlives agentop's terminal.
func detached() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

// processAlive reports whether pid names a live process (kill -0).
func processAlive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

// terminate asks pid to stop (SIGTERM).
func terminate(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }

// terminateGroup asks the process group led by pid to stop. A group already gone is not an
// error.
func terminateGroup(pid int) error {
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// lockFile takes an exclusive lock on f without waiting. held is true when another process
// has it.
func lockFile(f *os.File) (held bool, err error) {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

// writable reports whether this process may create files in dir.
func writable(dir string) error {
	const wOK = 0x2
	return syscall.Access(dir, wOK)
}
