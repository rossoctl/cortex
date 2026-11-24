//go:build !windows

package main

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// Process helpers for tests, for Unix. killpid_windows_test.go has the Windows half (#1520).

// killPID sends sig to pid (to the group led by -pid when pid is negative).
func killPID(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }

// groupAttr starts a child as the leader of a process group of its own.
func groupAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

// sessionAttr starts a child in a session of its own.
func sessionAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

// mkfifo makes a named pipe at path.
func mkfifo(path string, mode uint32) error { return syscall.Mkfifo(path, mode) }

// getsid is pid's session id.
func getsid(pid int) (int, error) { return unix.Getsid(pid) }
