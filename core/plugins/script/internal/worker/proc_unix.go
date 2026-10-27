//go:build unix

package worker

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in a process group of its own, so a timeout can
// kill everything it started — a shell script's pipeline, a Python subprocess —
// and not only the direct child.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
