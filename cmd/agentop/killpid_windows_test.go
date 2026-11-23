//go:build windows

package main

import (
	"errors"
	"syscall"
)

// The Windows half of killpid_unix_test.go. Windows has no signals to send to another process,
// no process groups of the Unix kind and no sessions. The tests that use these need a Unix
// service or shell and do not run on Windows; these keep them compiling there (#1520).

var errNoUnixProcess = errors.New("not on Windows")

func killPID(int, syscall.Signal) error { return errNoUnixProcess }

func groupAttr() *syscall.SysProcAttr { return nil }

func sessionAttr() *syscall.SysProcAttr { return nil }

func getsid(int) (int, error) { return 0, errNoUnixProcess }

func mkfifo(string, uint32) error { return errNoUnixProcess }
