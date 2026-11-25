//go:build unix

package bootstrap

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// NotifyReady reaches a path socket, and on Linux an abstract one (@name), as systemd
// often sets it.
func TestNotifyReady(t *testing.T) {
	// Under /tmp: a socket path has a limit near 104 bytes, and t.TempDir on macOS is long.
	dir, err := os.MkdirTemp("/tmp", "notify")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	for name, sock := range map[string]string{
		"path":     filepath.Join(dir, "n.sock"),
		"abstract": fmt.Sprintf("@cortex-notify-%d", os.Getpid()),
	} {
		t.Run(name, func(t *testing.T) {
			if name == "abstract" && runtime.GOOS != "linux" {
				t.Skip("abstract sockets are Linux-only")
			}
			ln, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sock, Net: "unixgram"})
			if err != nil {
				t.Skipf("unix socket bind unavailable in this environment: %v", err)
			}
			defer ln.Close()
			t.Setenv("NOTIFY_SOCKET", sock)
			if err := NotifyReady(); err != nil {
				t.Fatalf("NotifyReady: %v", err)
			}
			buf := make([]byte, 64)
			n, err := ln.Read(buf)
			if err != nil || string(buf[:n]) != "READY=1" {
				t.Fatalf("got %q, %v; want READY=1", buf[:n], err)
			}
		})
	}
}

func TestNotifyReadyUnset(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if err := NotifyReady(); err != nil {
		t.Fatalf("NotifyReady with no NOTIFY_SOCKET: %v", err)
	}
}
