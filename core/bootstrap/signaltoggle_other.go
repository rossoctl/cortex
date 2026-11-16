//go:build !unix

package bootstrap

// StartSignalToggle is a no-op on non-unix platforms (Windows, js, wasip1),
// where SIGUSR1 does not exist. The log level stays at what LOG_LEVEL set.
func StartSignalToggle() {}
