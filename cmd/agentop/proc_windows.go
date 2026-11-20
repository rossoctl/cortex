//go:build windows

package main

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// The Windows half of proc_unix.go (#1520).

// errNoGracefulStop is what stopping another process returns on Windows. Windows has no
// SIGTERM: Go's Process.Signal can only kill, and a killed proxy skips its plugin stops and its
// ledger and archive flushes. Until the proxy has a stop path of its own, agentop refuses
// rather than kill it.
var errNoGracefulStop = errors.New("stopping a process gracefully is not supported on Windows yet (#1520)")

// detached starts a child in a process group of its own, with no console, so it outlives
// agentop's console window.
func detached() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS}
}

// processAlive reports whether pid names a live process: it can be opened and has not exited.
// Two differences from kill -0 on Unix. A process of another user can often be opened with
// PROCESS_QUERY_LIMITED_INFORMATION, so it reads as alive here, where Unix's EPERM reads it as
// gone. And a process that exited with code 259 reads as still running, since that is
// STILL_ACTIVE's value.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h) //nolint:errcheck // a query handle
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259 // STILL_ACTIVE
	return code == stillActive
}

// terminate refuses: see errNoGracefulStop.
func terminate(int) error { return errNoGracefulStop }

// terminateGroup refuses: see errNoGracefulStop.
func terminateGroup(int) error { return errNoGracefulStop }

// lockFile takes an exclusive lock on f without waiting (LockFileEx). held is true when another
// process has it.
func lockFile(f *os.File) (held bool, err error) {
	var ol windows.Overlapped
	err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ol)
	if err == nil {
		return false, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return true, nil
	}
	return false, err
}

// writable reports whether this process may write path: for a directory, create files in it,
// by creating and removing one; for a file, open it for appending, which writes nothing. Windows
// has no access(2), and its ACLs are what decide.
func writable(path string) error {
	if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			return err
		}
		return f.Close()
	}
	f, err := os.CreateTemp(path, ".agentop-writable-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}
