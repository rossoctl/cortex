package main

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/peerproc"
)

// A taken port has to be classifiable from the error alone, because that is all the
// bind site has: reserveListeners closes what it bound before returning. errors.Is
// reaching EADDRINUSE through two wrappers is what tells this condition apart from a
// privileged port or an address no interface has, and the address is what names the
// holder — it exists nowhere else, net.Listen having put it only in its message.
func TestReserveListeners_TakenPortCarriesTheAddressAndTheCause(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	addr := held.Addr().String()

	_, err = reserveListeners([]config.PlannedListener{{Name: "forward-proxy", Addr: addr}})
	var le *listenError
	switch {
	case !errors.As(err, &le):
		t.Fatalf("err = %v (%T), want a *listenError", err, err)
	case le.addr != addr:
		t.Errorf("addr = %q, want %q — the holder cannot be named without it", le.addr, addr)
	case le.name != "forward-proxy":
		t.Errorf("name = %q, want forward-proxy", le.name)
	case !errors.Is(err, syscall.EADDRINUSE):
		t.Errorf("errors.Is(err, EADDRINUSE) = false for %v; every other bind failure would be reported as this one", err)
	}
}

// The kernel knows who holds a socket this process could not bind, and peerproc reads
// it without lsof, ss or root. The holder here is this test binary, which is the one
// pid the test can assert: a wrong answer that happens to be a real process is exactly
// the failure a message naming a pid must not have.
func TestHolderOf_NamesThisProcess(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	h, err := holderOf(ln.Addr().String())
	if errors.Is(err, peerproc.ErrUnsupported) {
		t.Skipf("peerproc cannot read this host's tables: %v", err)
	}
	if err != nil {
		t.Fatalf("holderOf(%s) = %v", ln.Addr(), err)
	}
	if int(h.pid) != os.Getpid() {
		t.Errorf("holder pid = %d, want this process (%d)", h.pid, os.Getpid())
	}
	// Exe is "" when the kernel withholds it, which holderFields omits rather than
	// renders. Where it is readable it must be this binary and not a guess — compared
	// through EvalSymlinks because macOS reports the path the kernel resolved
	// (/private/var/…) where os.Executable keeps the one it was started from (/var/…).
	if h.exe != "" {
		self, err := os.Executable()
		if err != nil {
			t.Skipf("os.Executable: %v", err)
		}
		if resolved(t, h.exe) != resolved(t, self) {
			t.Errorf("holder exe = %q, want %q", h.exe, self)
		}
	}
}

func resolved(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return p
	}
	return r
}

// holderFields omits what is unknown. A zero pid renders as a real one, and a parent
// of 1 or of itself names the init process or nonsense — in a line whose whole purpose
// is telling the operator which process to stop.
func TestHolderFields_OmitsWhatIsNotKnown(t *testing.T) {
	has := func(f []any, key string) bool {
		for i := 0; i < len(f)-1; i += 2 {
			if f[i] == key {
				return true
			}
		}
		return false
	}
	full := holderFields(holder{pid: 42, ppid: 7, exe: "/usr/local/bin/cortex"})
	for _, k := range []string{"holder_pid", "holder", "holder_parent_pid"} {
		if !has(full, k) {
			t.Errorf("%v is missing %s", full, k)
		}
	}
	bare := holderFields(holder{pid: 42, ppid: 1})
	if has(bare, "holder") {
		t.Errorf("%v names an executable the kernel would not say", bare)
	}
	if has(bare, "holder_parent_pid") {
		t.Errorf("%v names init as the parent to stop", bare)
	}
	if self := holderFields(holder{pid: 42, ppid: 42}); has(self, "holder_parent_pid") {
		t.Errorf("%v names the holder as its own parent", self)
	}
}

func TestProbeAddr(t *testing.T) {
	cases := []struct {
		name, addr, want string
	}{
		// A wildcard bind has no single address to ask about, and loopback is the most
		// permissive probe: peerproc ranks an exact bind first, then a wildcard of the
		// same family, so this finds a holder of 0.0.0.0, of [::] or of loopback.
		{"a wildcard v4 bind asks about loopback", "0.0.0.0:47600", "127.0.0.1:47600"},
		{"a bare port is a wildcard bind", ":47600", "127.0.0.1:47600"},
		{"a wildcard v6 bind asks about v6 loopback", "[::]:47600", "[::1]:47600"},
		{"a literal address is itself", "127.0.0.1:47600", "127.0.0.1:47600"},
		{"a v4-mapped v6 literal is unmapped", "[::ffff:127.0.0.1]:47600", "127.0.0.1:47600"},
		{"a zone is dropped", "[fe80::1%lo0]:47600", "[fe80::1]:47600"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := probeAddr(tc.addr)
			if err != nil {
				t.Fatalf("probeAddr(%q) = %v", tc.addr, err)
			}
			if got != netip.MustParseAddrPort(tc.want) {
				t.Errorf("probeAddr(%q) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
	// Nothing here is resolved or guessed. A config that named a host would still get
	// the socket, the address and the lsof line; inventing an address to probe would
	// risk naming the holder of someone else's port.
	for _, addr := range []string{"localhost:47600", "127.0.0.1", "127.0.0.1:0", "127.0.0.1:http", ""} {
		if ap, err := probeAddr(addr); err == nil {
			t.Errorf("probeAddr(%q) = %v, want an error rather than a probe of something else", addr, ap)
		}
	}
}

// portOf is interpolated into an lsof line a human runs, so it must never render an
// unparsable address as a plausible port.
func TestPortOf(t *testing.T) {
	if got := portOf("127.0.0.1:47600"); got != "47600" {
		t.Errorf("portOf = %q, want 47600", got)
	}
	if got := portOf("47600"); got != "<port>" {
		t.Errorf("portOf(%q) = %q, want the placeholder", "47600", got)
	}
}

// The exit status is the whole protocol between the child and its supervisor, and it
// has to stay distinct from the 1 every other startup failure leaves with.
func TestExitPortInUse_IsNotAnOrdinaryFailure(t *testing.T) {
	if exitPortInUse == 0 || exitPortInUse == 1 || exitPortInUse == 2 {
		t.Errorf("exitPortInUse = %d, which a crash or a usage error also leaves", exitPortInUse)
	}
}
