//go:build unix

package bootstrap

import (
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

// StartSignalToggle installs a SIGUSR1 handler that toggles the process log
// level between DEBUG and INFO, so operators can flip verbose logging on a
// running pod without a restart.
func StartSignalToggle() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGUSR1)
	go func() {
		for range sigCh {
			if logLevel.Level() == slog.LevelDebug {
				logLevel.Set(slog.LevelInfo)
				slog.Info("log level toggled to INFO (send SIGUSR1 to switch back to DEBUG)")
			} else {
				logLevel.Set(slog.LevelDebug)
				slog.Info("log level toggled to DEBUG (send SIGUSR1 to switch back to INFO)")
			}
		}
	}()
}
