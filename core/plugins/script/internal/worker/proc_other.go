//go:build !unix

package worker

import "os/exec"

// Cortex runs scripts on a local install, which is darwin or linux. Elsewhere the
// child is killed alone.
func setProcessGroup(*exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
