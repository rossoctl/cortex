package runtimeutil

import (
	"log/slog"
	"syscall"
	"testing"
	"time"
)

// TestInitLogging verifies InitLogging maps LOG_LEVEL to the process level and
// defaults to INFO for unset/unknown values.
func TestInitLogging(t *testing.T) {
	cases := []struct {
		env  string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug}, // case-insensitive
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"info", slog.LevelInfo},
		{"", slog.LevelInfo},         // unset -> default
		{"nonsense", slog.LevelInfo}, // unknown -> default
	}
	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv("LOG_LEVEL", tc.env)
			InitLogging("test-binary")
			if got := LogLevel(); got != tc.want {
				t.Fatalf("LOG_LEVEL=%q: LogLevel()=%v, want %v", tc.env, got, tc.want)
			}
		})
	}
}

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
