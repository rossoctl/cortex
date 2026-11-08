package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// startUnsupervised is install.sh's start_unsupervised: cortex --local --supervise
// with output appended to proxy.log and its pid in proxy.pid, then up to ten
// one-second health checks. It runs in its own session, so a Ctrl-C at setup's
// terminal cannot reach a proxy meant to outlive setup; rollback stops it on
// purpose instead. healthy is false when the proxy is alive but has not answered
// yet, which install.sh also treats as started. A proxy that exits is a stepError
// with no detail: the caller adds the log lines this start wrote.
func startUnsupervised(bin, cortexDir, healthURL string) (pid int, healthy bool, err error) {
	if err := os.MkdirAll(cortexDir, 0o700); err != nil {
		return 0, false, err
	}
	logPath := filepath.Join(cortexDir, "proxy.log")
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec
	if err != nil {
		return 0, false, err
	}
	cmd := exec.Command(bin, "--local", "--supervise") //nolint:gosec // the installed cortex
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	_ = logf.Close()
	if err != nil {
		return 0, false, err
	}
	pid = cmd.Process.Pid
	pidFile := filepath.Join(cortexDir, proxyPIDFile)
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait() // reap it, so no zombie answers kill -0 as alive
		return 0, false, err
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	for i := 0; i < 10; i++ {
		select {
		case <-exited:
			_ = os.Remove(pidFile)
			return 0, false, stepError{reason: "the proxy exited immediately"}
		default:
		}
		if healthURL != "" && waitHealthy(healthURL, time.Second) {
			return pid, true, nil
		}
		if healthURL == "" {
			time.Sleep(time.Second)
		}
	}
	return pid, false, nil
}
