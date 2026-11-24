package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// On Windows, the subcommands that install, run or replace Cortex refuse with a message naming
// #1520; every other subcommand, and every subcommand elsewhere, is let through.
func TestRefuseUnsupported(t *testing.T) {
	for _, cmd := range []string{"service", "setup", "uninstall", "update"} {
		var stderr bytes.Buffer
		if !refuseUnsupported("windows", cmd, &stderr) || !strings.Contains(stderr.String(), "#1520") {
			t.Errorf("windows %s: not refused (stderr %q)", cmd, stderr.String())
		}
		if refuseUnsupported("linux", cmd, &stderr) || refuseUnsupported("darwin", cmd, &stderr) {
			t.Errorf("%s refused off Windows", cmd)
		}
	}
	for _, cmd := range []string{"observe", "exec", "cost", "configure", "doctor", "export"} {
		if refuseUnsupported("windows", cmd, &bytes.Buffer{}) {
			t.Errorf("windows %s refused", cmd)
		}
	}
}

// processAlive is true for this process, and false for a child that has exited and for pids
// that name nothing.
func TestProcessAlive(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Fatal("this process is not alive")
	}
	if processAlive(0) || processAlive(-1) {
		t.Fatal("pid 0 or -1 is alive")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// The test binary itself, asked to run no test: it starts and exits at once on every OS.
	cmd := exec.Command(exe, "-test.run=^$")
	// Run returns once the child has exited and been reaped, so it is gone at once.
	if err := cmd.Run(); err != nil {
		t.Fatalf("run the child: %v", err)
	}
	if pid := cmd.Process.Pid; processAlive(pid) {
		t.Fatalf("pid %d of an exited child is still alive", pid)
	}
}

// lockFile: the first lock is taken, a second one on the same file is held.
func TestLockFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "update.lock")
	open := func() *os.File {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return f
	}
	if held, err := lockFile(open()); held || err != nil {
		t.Fatalf("first lock: held %v, err %v", held, err)
	}
	if held, err := lockFile(open()); !held || err != nil {
		t.Fatalf("second lock: held %v, err %v; want held", held, err)
	}
}

// writable: a temp dir is, a missing dir is not, and nothing is left behind.
func TestWritable(t *testing.T) {
	dir := t.TempDir()
	if err := writable(dir); err != nil {
		t.Fatalf("writable(%s) = %v", dir, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("writable left %d entries in %s", len(entries), dir)
	}
	if err := writable(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing dir is writable")
	}
	// A file (setup checks the shell profile this way): writable, and left unchanged.
	f := filepath.Join(dir, ".zshrc")
	if err := os.WriteFile(f, []byte("export A=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writable(f); err != nil {
		t.Fatalf("writable(%s) = %v for a writable file", f, err)
	}
	if b, _ := os.ReadFile(f); string(b) != "export A=1\n" {
		t.Fatalf("writable changed the file: %q", b)
	}
}

// On Windows, stopping a process refuses rather than killing it: there is no SIGTERM, and a
// killed proxy skips its flushes.
func TestTerminateRefusesOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the Windows half")
	}
	if err := terminate(os.Getpid()); err == nil {
		t.Fatal("terminate did not refuse")
	}
	if err := terminateGroup(os.Getpid()); err == nil {
		t.Fatal("terminateGroup did not refuse")
	}
}
