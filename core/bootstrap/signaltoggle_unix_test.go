//go:build unix

package bootstrap

import (
	"log/slog"
	"syscall"
	"testing"
	"time"
)

// TestStartSignalToggle verifies a SIGUSR1 flips the level between DEBUG and
// INFO on each delivery.
func TestStartSignalToggle(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	InitLogging("test-binary")
	if LogLevel() != slog.LevelInfo {
		t.Fatalf("precondition: want INFO, got %v", LogLevel())
	}

	StartSignalToggle()

	// INFO -> DEBUG
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGUSR1); err != nil {
		t.Fatalf("kill SIGUSR1: %v", err)
	}
	waitForLevel(t, slog.LevelDebug)

	// DEBUG -> INFO
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGUSR1); err != nil {
		t.Fatalf("kill SIGUSR1: %v", err)
	}
	waitForLevel(t, slog.LevelInfo)
}

// waitForLevel polls until the process log level reaches want, since the signal
// is handled asynchronously by the goroutine StartSignalToggle launched.
func waitForLevel(t *testing.T, want slog.Level) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if LogLevel() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for level %v (still %v)", want, LogLevel())
}
