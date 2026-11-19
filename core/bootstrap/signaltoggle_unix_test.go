//go:build unix

package bootstrap

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// TestStartSignalToggle verifies a SIGUSR1 flips the level between DEBUG and
// INFO on each delivery.
func TestStartSignalToggle(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	InitLogging("test-binary")
	if LogLevel() != slog.LevelInfo {
		t.Fatalf("precondition: want INFO, got %v", LogLevel())
	}
	// The toggle logs once it has set the level; each log line marks a toggle.
	prev := slog.Default()
	toggled := make(chan struct{}, 2)
	slog.SetDefault(slog.New(notifyHandler{toggled}))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// One handler per process: a second would toggle the level back on each signal.
	startToggle.Do(StartSignalToggle)

	// INFO -> DEBUG
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGUSR1); err != nil {
		t.Fatalf("kill SIGUSR1: %v", err)
	}
	<-toggled
	if got := LogLevel(); got != slog.LevelDebug {
		t.Fatalf("after first SIGUSR1: want DEBUG, got %v", got)
	}

	// DEBUG -> INFO
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGUSR1); err != nil {
		t.Fatalf("kill SIGUSR1: %v", err)
	}
	<-toggled
	if got := LogLevel(); got != slog.LevelInfo {
		t.Fatalf("after second SIGUSR1: want INFO, got %v", got)
	}
}

var startToggle sync.Once

// notifyHandler sends on ch for every toggle record it handles.
type notifyHandler struct{ ch chan<- struct{} }

func (h notifyHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h notifyHandler) Handle(_ context.Context, r slog.Record) error {
	if strings.HasPrefix(r.Message, "log level toggled") {
		h.ch <- struct{}{}
	}
	return nil
}
func (h notifyHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h notifyHandler) WithGroup(string) slog.Handler      { return h }
