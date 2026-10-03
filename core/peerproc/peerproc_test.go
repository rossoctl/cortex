//go:build darwin || linux

package peerproc

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"syscall"
	"testing"
	"time"
)

// TestMain doubles as the two child processes the tests run. With PEERPROC_TEST_DIAL
// set, the test binary dials that address, holds the connection until the other end
// closes it, and exits. With PEERPROC_TEST_LISTEN set, it listens on that address,
// writes "ready" to stdout, and holds the listener until its stdin reaches EOF.
func TestMain(m *testing.M) {
	if addr := os.Getenv("PEERPROC_TEST_DIAL"); addr != "" {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			os.Exit(2)
		}
		_, _ = c.Read(make([]byte, 1))
		os.Exit(0)
	}
	if addr := os.Getenv("PEERPROC_TEST_LISTEN"); addr != "" {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			os.Exit(2)
		}
		fmt.Println("ready")
		_, _ = io.Copy(io.Discard, os.Stdin)
		_ = ln.Close()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func newResolver(t testing.TB) Resolver {
	t.Helper()
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func listen(t testing.TB, network, addr string) net.Listener {
	t.Helper()
	ln, err := net.Listen(network, addr)
	if err != nil {
		t.Fatalf("listen %s %s: %v", network, addr, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

func acceptOne(t testing.TB, ln net.Listener) net.Conn {
	t.Helper()
	_ = ln.(*net.TCPListener).SetDeadline(time.Now().Add(10 * time.Second))
	c, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func dial(t testing.TB, network, addr string) net.Conn {
	t.Helper()
	c, err := net.Dial(network, addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// ends is an accepted connection's client and server address, the two ConnOwner takes.
func ends(c net.Conn) (client, server netip.AddrPort) {
	return c.RemoteAddr().(*net.TCPAddr).AddrPort(), c.LocalAddr().(*net.TCPAddr).AddrPort()
}

func sameFile(a, b string) bool {
	ea, err1 := filepath.EvalSymlinks(a)
	eb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ea == eb
}

func TestNew_SelfTestPasses(t *testing.T) {
	newResolver(t)
}

// A connection this process dialled is this process's.
func TestConnOwner_NamesThisProcess(t *testing.T) {
	r := newResolver(t)
	ln := listen(t, "tcp", "127.0.0.1:0")
	dial(t, "tcp", ln.Addr().String())
	client, server := ends(acceptOne(t, ln))

	p, err := r.ConnOwner(client, server)
	if err != nil {
		t.Fatalf("ConnOwner: %v", err)
	}
	if p.PID != int32(os.Getpid()) {
		t.Errorf("pid %d, want %d", p.PID, os.Getpid())
	}
	if p.PPID != int32(os.Getppid()) {
		t.Errorf("ppid %d, want %d", p.PPID, os.Getppid())
	}
	if p.Start.IsZero() || p.Start.After(time.Now()) {
		t.Errorf("start %v is not a time in the past", p.Start)
	}
}

// The case this package exists for: another process dials us, and the lookup names it,
// its executable, and its parent — this test process, which spawned it.
func TestConnOwner_NamesAChildProcessAndItsAncestry(t *testing.T) {
	r := newResolver(t)
	ln := listen(t, "tcp", "127.0.0.1:0")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), "PEERPROC_TEST_DIAL="+ln.Addr().String())
	before := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	client, server := ends(acceptOne(t, ln))
	child := int32(cmd.Process.Pid)

	p, err := r.ConnOwner(client, server)
	if err != nil {
		t.Fatalf("ConnOwner: %v", err)
	}
	if p.PID != child {
		t.Fatalf("pid %d, want the child %d", p.PID, child)
	}
	if p.PPID != int32(os.Getpid()) {
		t.Errorf("child's ppid %d, want this process %d", p.PPID, os.Getpid())
	}
	// Linux's start time is ticks after boot plus a whole-second boot time, so it can read
	// up to a second early; 5s either side still catches a missing boot time or a wrong
	// tick rate, which are off by years or by a factor.
	if p.Start.Before(before.Add(-5*time.Second)) || p.Start.After(after.Add(5*time.Second)) {
		t.Errorf("child's Start %v, want within 5s of its launch (%v .. %v)", p.Start, before, after)
	}
	if !sameFile(p.Exe, self) {
		t.Errorf("child's exe %q, want %q", p.Exe, self)
	}

	chain, err := r.Ancestry(child, 4)
	if err != nil {
		t.Fatalf("Ancestry: %v", err)
	}
	if len(chain) < 2 || chain[0].PID != child || chain[1].PID != int32(os.Getpid()) {
		t.Fatalf("ancestry %+v, want the child then this process", chain)
	}
	if !chain[0].Start.Equal(p.Start) {
		t.Errorf("Start differs between two lookups of one process: %v and %v", p.Start, chain[0].Start)
	}

	// Naming the child as a hint gives the same answer; Linux looks there first.
	if h, err := r.ConnOwner(client, server, child); err != nil || h.PID != child {
		t.Errorf("ConnOwner with the child as hint = %+v, %v", h, err)
	}
}

func TestListenerOwner_NamesTheListener(t *testing.T) {
	r := newResolver(t)
	ln := listen(t, "tcp", "127.0.0.1:0")

	p, err := r.ListenerOwner(ln.Addr().(*net.TCPAddr).AddrPort())
	if err != nil {
		t.Fatalf("ListenerOwner: %v", err)
	}
	if p.PID != int32(os.Getpid()) {
		t.Errorf("pid %d, want %d", p.PID, os.Getpid())
	}
}

// A socket bound to a port but not listening on it takes no connections, so it is no
// one's listener. macOS keeps it in the same table as the listeners, with no foreign
// port either; only its SO_ACCEPTCONN flag tells them apart.
func TestListenerOwner_IgnoresABoundSocketThatIsNotListening(t *testing.T) {
	r := newResolver(t)
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	syscall.CloseOnExec(fd)
	t.Cleanup(func() { _ = syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	addr := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(sa.(*syscall.SockaddrInet4).Port))

	if p, err := r.ListenerOwner(addr); !errors.Is(err, ErrNotFound) {
		t.Errorf("ListenerOwner(%s) over a bound, non-listening socket = %+v, %v; want ErrNotFound", addr, p, err)
	}
}

// Nobody holds this pair, and that is ErrNotFound — not a failure of the lookup itself.
func TestConnOwner_UnknownConnectionIsNotFound(t *testing.T) {
	r := newResolver(t)
	_, err := r.ConnOwner(netip.MustParseAddrPort("127.0.0.1:1"), netip.MustParseAddrPort("127.0.0.1:2"))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// A connection whose client end has been closed is held by no process, though the kernel
// keeps it in its table a while.
func TestConnOwner_ClosedConnectionIsNotFound(t *testing.T) {
	r := newResolver(t)
	for name, closeServer := range map[string]bool{"client closed": false, "both ends closed": true} {
		t.Run(name, func(t *testing.T) {
			ln := listen(t, "tcp", "127.0.0.1:0")
			c := dial(t, "tcp", ln.Addr().String())
			s := acceptOne(t, ln)
			client, server := ends(s)
			_ = c.Close()
			if closeServer {
				_, _ = io.Copy(io.Discard, s)
				_ = s.Close()
			}

			if p, err := r.ConnOwner(client, server); !errors.Is(err, ErrNotFound) {
				t.Errorf("ConnOwner = %+v, %v; want ErrNotFound", p, err)
			}
		})
	}
}

func TestListenerOwner_UnknownPortIsNotFound(t *testing.T) {
	r := newResolver(t)
	ln := listen(t, "tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close() // the port is free again

	_, err := r.ListenerOwner(netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(port)))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// IPv6 is best-effort, but where the host has ::1 both lookups must work over it.
func TestIPv6Loopback(t *testing.T) {
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	r := newResolver(t)
	dial(t, "tcp6", ln.Addr().String())
	client, server := ends(acceptOne(t, ln))

	if p, err := r.ConnOwner(client, server); err != nil || p.PID != int32(os.Getpid()) {
		t.Errorf("ConnOwner over ::1 = %+v, %v", p, err)
	}
	if p, err := r.ListenerOwner(server); err != nil || p.PID != int32(os.Getpid()) {
		t.Errorf("ListenerOwner over ::1 = %+v, %v", p, err)
	}
}

// Ancestry walks parent links and stops before PID 1; every link is the one before
// it's parent.
func TestAncestry_WalksParentsAndStopsBelowInit(t *testing.T) {
	r := newResolver(t)
	chain, err := r.Ancestry(int32(os.Getpid()), 64)
	if err != nil {
		t.Fatalf("Ancestry: %v", err)
	}
	if chain[0].PID != int32(os.Getpid()) {
		t.Fatalf("chain starts at %d, want this process", chain[0].PID)
	}
	for i, p := range chain {
		if p.PID <= 1 {
			t.Errorf("chain[%d] is pid %d; the walk must stop before PID 1", i, p.PID)
		}
		if i > 0 && p.PID != chain[i-1].PPID {
			t.Errorf("chain[%d] is pid %d, want chain[%d]'s parent %d", i, p.PID, i-1, chain[i-1].PPID)
		}
	}
}

func TestAncestry_RespectsMax(t *testing.T) {
	r := newResolver(t)
	chain, err := r.Ancestry(int32(os.Getpid()), 1)
	if err != nil || len(chain) != 1 {
		t.Errorf("Ancestry(self, 1) = %d entries, %v; want exactly 1", len(chain), err)
	}
}

func TestAncestry_UnknownPIDIsNotFound(t *testing.T) {
	r := newResolver(t)
	// Above both kernels' PID ceilings: macOS stops at 99999, Linux at 2^22.
	if _, err := r.Ancestry(1<<30, 4); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	// The walk stops before PID 1, so 1 and below have no ancestry at all.
	for _, pid := range []int32{1, 0, -1} {
		if _, err := r.Ancestry(pid, 4); !errors.Is(err, ErrNotFound) {
			t.Errorf("Ancestry(%d): err = %v, want ErrNotFound", pid, err)
		}
	}
}

// The per-connection cost the proxy will pay on a connection's first request.
func BenchmarkConnOwner(b *testing.B) {
	r := newResolver(b)
	ln := listen(b, "tcp", "127.0.0.1:0")
	dial(b, "tcp", ln.Addr().String())
	client, server := ends(acceptOne(b, ln))
	self := int32(os.Getpid())
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.ConnOwner(client, server, self); err != nil {
			b.Fatal(err)
		}
	}
}

// Exe is something two processes can be compared by: absolute and symlink-resolved, or
// "" when unknown — never the relative or symlinked spelling a process was started by.
func TestConnOwner_ExeIsResolvedOrUnknown(t *testing.T) {
	r := newResolver(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(self)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	link := filepath.Join(dir, "via-link")
	if err := os.Symlink(self, link); err != nil {
		t.Fatal(err)
	}
	moved, other := filepath.Join(dir, "moved-link"), filepath.Join(dir, "other")
	if err := os.Symlink(self, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		cmd   func() *exec.Cmd
		after func(t *testing.T) // runs once the child has dialled
	}{
		"through a symlink": {
			cmd: func() *exec.Cmd { return exec.Command(link) },
		},
		"through a symlink retargeted after it started": {
			cmd: func() *exec.Cmd { return exec.Command(moved) },
			after: func(t *testing.T) {
				if err := os.Remove(moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, moved); err != nil {
					t.Fatal(err)
				}
			},
		},
		"by a relative path": {
			cmd: func() *exec.Cmd {
				c := exec.Command("./" + filepath.Base(self))
				c.Dir = filepath.Dir(self)
				return c
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ln := listen(t, "tcp", "127.0.0.1:0")
			cmd := tc.cmd()
			cmd.Env = append(os.Environ(), "PEERPROC_TEST_DIAL="+ln.Addr().String())
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
			client, server := ends(acceptOne(t, ln))
			if tc.after != nil {
				tc.after(t)
			}

			p, err := r.ConnOwner(client, server)
			if err != nil || p.PID != int32(cmd.Process.Pid) {
				t.Fatalf("ConnOwner = %+v, %v; want the child %d", p, err, cmd.Process.Pid)
			}
			if p.Exe != real {
				t.Errorf("Exe %q; want %q", p.Exe, real)
			}
		})
	}
}

// Environ reads a child's environment back from the kernel. The child is this test binary
// re-run as TestEnvironHelperProcess, not a system binary, which macOS can refuse to show.
func TestEnviron_ReadsAChildsEnvironment(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("Environ is unsupported on " + runtime.GOOS)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestEnvironHelperProcess$")
	cmd.Env = append(os.Environ(), "PEERPROC_ENVIRON_HELPER=1", "PEERPROC_MARKER=a=b c")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close(); _ = cmd.Wait() }()
	// Start returns before the kernel has finished the exec: Linux closes the child's
	// close-on-exec descriptors, which is what Start waits on, before it records where the
	// new image's environment lies, and /proc/<pid>/environ reads empty until it does.
	// The child's own output is the first sign that the exec is done.
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("child said %q, %v; want \"ready\"", line, err)
	}

	env, err := Environ(int32(cmd.Process.Pid))
	if err != nil {
		t.Fatalf("Environ(child): %v", err)
	}
	if !slices.Contains(env, "PEERPROC_MARKER=a=b c") {
		t.Errorf("child's environment lacks the marker; got %d entries", len(env))
	}
}

// TestEnvironHelperProcess is the child TestEnviron_ReadsAChildsEnvironment reads: it writes
// "ready" to stdout and waits for its stdin to close, so it is alive while the test reads it.
func TestEnvironHelperProcess(t *testing.T) {
	if os.Getenv("PEERPROC_ENVIRON_HELPER") != "1" {
		return
	}
	fmt.Println("ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}
