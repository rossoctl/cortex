//go:build darwin || linux

package peerproc

import (
	"errors"
	"os"
	"os/exec"
	"testing"
)

// ProcessIdentity against the real process table: this process has one, read twice it is
// the same, another process's differs, and a pid no process can have has none.
func TestProcessIdentity_RealProcesses(t *testing.T) {
	self, err := ProcessIdentity(int32(os.Getpid()))
	if err != nil {
		t.Fatalf("this process: %v", err)
	}
	if self.BootID == "" || self.Zombie {
		t.Errorf("this process = %+v, want a boot id and not a zombie", self)
	}
	if again, err := ProcessIdentity(int32(os.Getpid())); err != nil || again != self {
		t.Errorf("read twice: %+v then %+v, %v", self, again, err)
	}
	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	other, err := ProcessIdentity(int32(cmd.Process.Pid))
	if err != nil {
		t.Fatalf("a child: %v", err)
	}
	if other == self || other.BootID != self.BootID {
		t.Errorf("a child = %+v, this process = %+v: want the same boot, a different process", other, self)
	}
	for _, pid := range []int32{0, -1} {
		if _, err := ProcessIdentity(pid); !errors.Is(err, ErrNotFound) {
			t.Errorf("pid %d: err = %v, want ErrNotFound", pid, err)
		}
	}
}

// An exited child that has not been reaped is a zombie: it keeps its pid and identity but
// runs nothing, which is what lets a caller tell it from a live process.
func TestProcessIdentity_Zombie(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Wait() })
	pid := int32(cmd.Process.Pid)
	for i := 0; i < 200; i++ {
		id, err := ProcessIdentity(pid)
		if err != nil {
			t.Fatalf("an unreaped child: %v", err)
		}
		if id.Zombie {
			return
		}
		// Not exited yet; true exits within milliseconds.
		exec.Command("sleep", "0.01").Run()
	}
	t.Fatal("an exited, unreaped child never read as a zombie")
}
