//go:build unix && include_driver_moca

package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/rossoctl/cortex/core/config"
)

// The built-in config is offered only with moca's control socket set, needs the allow list
// and the host's addresses, and binds loopback only. claimed() makes main serve exactly
// this plan.
func TestHostEgressBuiltinConfig(t *testing.T) {
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	addrs := net.InterfaceAddrs
	if _, ok, err := builtinConfig(getenv, addrs); ok || err != nil {
		t.Fatalf("got ok=%v, %v with no control socket; want no built-in config", ok, err)
	}
	env[envControlSock] = "/run/c.sock"
	if _, _, err := builtinConfig(getenv, addrs); err == nil || !strings.Contains(err.Error(), envEgressAllow) {
		t.Fatalf("got %v with no allow list; want an error naming %s", err, envEgressAllow)
	}
	env[envEgressAllow] = "api.github.com:443"
	noAddrs := func() ([]net.Addr, error) { return nil, errors.New("netlink refused") }
	if _, _, err := builtinConfig(getenv, noAddrs); err == nil {
		t.Fatal("started with the host's addresses unreadable")
	}
	b, ok, err := builtinConfig(getenv, addrs)
	if !ok || err != nil {
		t.Fatalf("got ok=%v, %v; want the built-in config", ok, err)
	}
	cfg, err := config.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	config.ApplyPreset(cfg)
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	plan, err := config.ListenerPlan(cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range plan {
		if host, _, err := net.SplitHostPort(l.Addr); err != nil || host != "127.0.0.1" {
			t.Errorf("%s binds %q, want 127.0.0.1", l.Name, l.Addr)
		}
	}
}

// Started as moca starts its egress proxy: no arguments, and an environment of the
// control socket's variables and the allow list alone, with no PATH and no HOME. It tells
// systemd it is ready, accepts on the control socket, and on SIGTERM stops at once,
// without a drain, and exits 0.
func TestHostEgressStartsFromEnvironment(t *testing.T) {
	// Under /tmp: a socket path has a limit near 104 bytes, and t.TempDir on macOS is long.
	dir, err := os.MkdirTemp("/tmp", "hostegress")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	notifyPath, ctrlPath := filepath.Join(dir, "n.sock"), filepath.Join(dir, "c.sock")
	notify, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: notifyPath, Net: "unixgram"})
	if err != nil {
		// The macOS seatbelt sandbox refuses Unix binds; CI's runners bind fine.
		t.Skipf("unix socket bind unavailable in this environment: %v", err)
	}
	defer notify.Close()

	c := startChildEnv(t, os.Args[0], []string{
		runMainEnv + "=main",
		envControlSock + "=" + ctrlPath,
		envControlUIDs + "=" + strconv.Itoa(os.Getuid()),
		envEgressAllow + "=api.github.com:443",
		"NOTIFY_SOCKET=" + notifyPath,
	})
	ready := make(chan string, 1)
	go func() {
		buf := make([]byte, 64)
		n, _ := notify.Read(buf)
		ready <- string(buf[:n])
	}()
	select {
	case msg := <-ready:
		if msg != "READY=1" {
			t.Fatalf("notify socket got %q, want READY=1", msg)
		}
	case err := <-c.done:
		c.done <- err
		t.Fatalf("the child exited (%v) before it was ready:\n%s", err, c.out.String())
	}

	// A READY sent before the control socket binds fails this dial on most runs, not all.
	ctrl, err := net.Dial("unix", ctrlPath)
	if err != nil {
		t.Fatalf("control socket: %v", err)
	}
	ctrl.Close()

	if err := syscall.Kill(c.cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := c.wait(); err != nil {
		t.Fatalf("exit after SIGTERM: %v", err)
	}
	if out := c.out.String(); !strings.Contains(out, "drain=false") {
		t.Errorf("the shutdown is not logged as drain=false:\n%s", out)
	}
}
