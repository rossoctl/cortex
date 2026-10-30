//go:build unix

package vmcontrol

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"
)

// These tests drive the control server the way vmpool's client does: one
// framed request, one framed reply, the Register fd riding the sendmsg. They
// are the in-tree stand-ins for moca's own control tests (same semantics,
// different repo — the module boundary is why they live here at all).

// testClient is a minimal control client: synchronous round trips, no
// reconnect logic. Calls are sequential; each sends one frame and reads one.
type testClient struct {
	t    *testing.T
	conn *net.UnixConn
}

func dialControl(t *testing.T, path string) *testClient {
	t.Helper()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	tc := &testClient{t: t, conn: c.(*net.UnixConn)}
	t.Cleanup(func() { _ = tc.conn.Close() })
	return tc
}

func (tc *testClient) call(req *ControlRequest, fd int) *ControlReply {
	tc.t.Helper()
	if err := writeFrame(tc.conn, marshalControlRequest(req), fd); err != nil {
		tc.t.Fatal(err)
	}
	body, fds, err := readFrame(tc.conn)
	if err != nil {
		tc.t.Fatal(err)
	}
	closeFDs(fds) // replies never carry fds
	rep, err := decodeControlReply(body)
	if err != nil {
		tc.t.Fatal(err)
	}
	return rep
}

func (tc *testClient) register(vm, sid string, grant []byte, f *os.File) *ControlReply {
	tc.t.Helper()
	fd := -1
	if f != nil {
		fd = int(f.Fd())
	}
	return tc.call(&ControlRequest{Register: &Register{VmID: vm, Sid: sid, Grant: grant}}, fd)
}

// handover registers vm the way vmpool does: ln.File() dups the fd, the
// SCM_RIGHTS send duplicates it again into the server, and once the reply is
// in the test closes both of its own copies. From then on the handler's
// listener is the only thing keeping the socket alive — which is what the
// refuses-new-dials assertions below depend on, and what moca's harness does.
func (tc *testClient) handover(vm, sid string, ln *net.UnixListener) *ControlReply {
	tc.t.Helper()
	f, err := ln.File()
	if err != nil {
		tc.t.Fatal(err)
	}
	rep := tc.register(vm, sid, nil, f)
	_ = f.Close()
	_ = ln.Close()
	return rep
}

func (tc *testClient) unregister(vm string) *ControlReply {
	tc.t.Helper()
	return tc.call(&ControlRequest{Unregister: &Unregister{VmID: vm}}, -1)
}

// recordingHandler is a Handler that records calls, for the semantics that are
// the server's to enforce. It is called from the server's goroutines, so
// everything it touches is locked, and it owns its listeners the way the real
// handler does — closing them on Unregister — so a socket really does stop
// accepting once its binding is gone.
type recordingHandler struct {
	mu           sync.Mutex
	registered   []string
	unregistered []string
	lns          map[string]net.Listener
}

func newRecordingHandler() *recordingHandler {
	return &recordingHandler{lns: map[string]net.Listener{}}
}

func (h *recordingHandler) Register(vmID, _ string, _ []byte, ln net.Listener) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.registered = append(h.registered, vmID)
	h.lns[vmID] = ln
	return nil
}

func (h *recordingHandler) Unregister(vmID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.unregistered = append(h.unregistered, vmID)
	if ln := h.lns[vmID]; ln != nil {
		_ = ln.Close()
		delete(h.lns, vmID)
	}
	return nil
}

func (h *recordingHandler) calls() (registered, unregistered []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.registered), slices.Clone(h.unregistered)
}

// requireUnixBind skips the test when this environment refuses to bind Unix
// sockets — the macOS seatbelt sandbox does, and no flag inside the session
// lifts it. CI's Linux runners bind fine. The control channel is Unix-only by
// construction (SCM_RIGHTS, peer credentials), so there is no fallback path to
// test through; the registry's behavior is covered separately over TCP.
func requireUnixBind(t *testing.T) {
	t.Helper()
	path := filepath.Join(shortDir(t, "bindprobe", len("/probe.sock")), "probe.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Skipf("unix socket bind unavailable in this environment: %v", err)
	}
	_ = ln.Close()
}

// newTestServer starts a control server on a fresh socket, admitting this
// process's uid (overridable), and returns it with the socket path.
func newTestServer(t *testing.T, h Handler, allowedUIDs ...int) (*Server, string) {
	t.Helper()
	requireUnixBind(t)
	path := filepath.Join(shortDir(t, "ectl", len("/control.sock")), "control.sock")
	if len(allowedUIDs) == 0 {
		allowedUIDs = []int{os.Getuid()}
	}
	srv, err := NewServer(path, allowedUIDs, h)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(func() { _ = srv.Close() })
	return srv, path
}

// vmSocket binds a listener the way vmpool does, on a path the test's guests
// can dial. Unlink-on-close is off so the path survives the test closing its
// own copy after a successful handover.
func vmSocket(t *testing.T, name string) (*net.UnixListener, string) {
	t.Helper()
	requireUnixBind(t)
	path := filepath.Join(shortDir(t, "vmsock", 1+len(name)), name)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	return ln, path
}

func TestServerRegisterUnregister(t *testing.T) {
	h := newRecordingHandler()
	_, path := newTestServer(t, h)
	tc := dialControl(t, path)

	ln, _ := vmSocket(t, "vm-a.sock")
	if rep := tc.handover("vm-a", "sid-a", ln); !rep.Ok {
		t.Fatalf("register: %s: %s", rep.Code, rep.Message)
	}
	if rep := tc.unregister("vm-a"); !rep.Ok {
		t.Fatalf("unregister: %s: %s", rep.Code, rep.Message)
	}
	registered, unregistered := h.calls()
	if len(registered) != 1 || registered[0] != "vm-a" {
		t.Errorf("registered %v, want [vm-a]", registered)
	}
	if len(unregistered) != 1 || unregistered[0] != "vm-a" {
		t.Errorf("unregistered %v, want [vm-a]", unregistered)
	}
}

// An unknown uid is closed without a reply: the peer learns nothing about the
// gate, not even that there is one to fail.
func TestServerUIDGate(t *testing.T) {
	h := newRecordingHandler()
	_, path := newTestServer(t, h, os.Getuid()+1000) // nobody
	tc := dialControl(t, path)

	_ = writeFrame(tc.conn, marshalControlRequest(&ControlRequest{Register: &Register{VmID: "vm-a"}}), -1)
	if _, _, err := readFrame(tc.conn); err == nil {
		t.Fatal("unauthorized peer got a reply")
	}
	if registered, _ := h.calls(); len(registered) != 0 {
		t.Errorf("handler ran for an unauthorized peer: %v", registered)
	}
}

func TestServerRegisterWithoutFD(t *testing.T) {
	h := newRecordingHandler()
	_, path := newTestServer(t, h)
	tc := dialControl(t, path)

	rep := tc.register("vm-a", "sid-a", nil, nil)
	if rep.Ok || rep.Code != CodeBadRequest || rep.Message != "register carries no fd" {
		t.Fatalf("got %+v, want bad_request \"register carries no fd\"", rep)
	}
}

func TestServerDuplicateVMOnSameConnection(t *testing.T) {
	h := newRecordingHandler()
	_, path := newTestServer(t, h)
	tc := dialControl(t, path)

	ln, _ := vmSocket(t, "vm-a.sock")
	if rep := tc.handover("vm-a", "sid-a", ln); !rep.Ok {
		t.Fatalf("first register: %s", rep.Message)
	}
	ln2, _ := vmSocket(t, "vm-a-2.sock")
	rep := tc.handover("vm-a", "sid-a", ln2)
	if rep.Ok || rep.Code != CodeDuplicateVM {
		t.Fatalf("got %+v, want duplicate_vm", rep)
	}
}

// Bindings are owned by the connection that registered them: when vmpool's
// connection dies, its VMs are unregistered even though nobody sent an
// Unregister — a crashed vmpool cannot leave the proxy serving VMs nobody
// manages.
func TestServerDropConnUnbinds(t *testing.T) {
	h := newRecordingHandler()
	_, path := newTestServer(t, h)
	tc := dialControl(t, path)

	ln, sockPath := vmSocket(t, "vm-a.sock")
	if rep := tc.handover("vm-a", "sid-a", ln); !rep.Ok {
		t.Fatalf("register: %s", rep.Message)
	}

	_ = tc.conn.Close() // vmpool goes away
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, unregistered := h.calls()
		if len(unregistered) > 0 || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, unregistered := h.calls()
	if len(unregistered) != 1 || unregistered[0] != "vm-a" {
		t.Fatalf("unregistered %v, want [vm-a]", unregistered)
	}
	// The socket the VM dialed refuses new connections once the binding is gone.
	if c, err := net.Dial("unix", sockPath); err == nil {
		_ = c.Close()
		t.Error("vm socket still accepts after dropConn")
	}
}

// A redialled vmpool re-registering a VM it still owns supersedes the stale
// binding from the old connection instead of being refused as a duplicate.
func TestServerRedialSupersedes(t *testing.T) {
	h := newRecordingHandler()
	_, path := newTestServer(t, h)
	tc1 := dialControl(t, path)

	ln1, sock1 := vmSocket(t, "vm-a.sock")
	if rep := tc1.handover("vm-a", "sid-a", ln1); !rep.Ok {
		t.Fatalf("first register: %s", rep.Message)
	}

	tc2 := dialControl(t, path)
	ln2, sock2 := vmSocket(t, "vm-a-new.sock")
	if rep := tc2.handover("vm-a", "sid-a", ln2); !rep.Ok {
		t.Fatalf("redial register: %s: %s", rep.Code, rep.Message)
	}

	// The old listener is unbound (its socket refuses); the new one serves.
	if c, err := net.Dial("unix", sock1); err == nil {
		_ = c.Close()
		t.Error("stale vm socket still accepts after supersede")
	}
	if c, err := net.Dial("unix", sock2); err != nil {
		t.Errorf("new vm socket refuses: %v", err)
	} else {
		_ = c.Close()
	}
	if _, unregistered := h.calls(); len(unregistered) != 1 || unregistered[0] != "vm-a" {
		t.Errorf("unregistered %v, want exactly the superseded [vm-a]", unregistered)
	}
}

// The malformed-frame test above needs the reply read inline; this keeps its
// assertion honest by checking the reply really was a refusal.
func TestServerMalformedFrameReply(t *testing.T) {
	h := newRecordingHandler()
	_, path := newTestServer(t, h)
	tc := dialControl(t, path)

	if err := writeFrame(tc.conn, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, -1); err != nil {
		t.Fatal(err)
	}
	body, fds, err := readFrame(tc.conn)
	if err != nil {
		t.Fatal(err)
	}
	closeFDs(fds)
	rep, err := decodeControlReply(body)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Ok || rep.Code != CodeBadRequest || rep.Message != "malformed request" {
		t.Fatalf("got %+v, want bad_request \"malformed request\"", rep)
	}
	// The connection survives a bad frame: the next request still answers.
	ln, _ := vmSocket(t, "vm-ok.sock")
	if rep := tc.handover("vm-ok", "sid", ln); !rep.Ok {
		t.Fatalf("register after malformed frame: %s", rep.Message)
	}
}

// TestIsTemporaryAccept: the accept loop must retry what clears on its own and
// return only what does not. A returned error reaches fatalf, which exits the
// process and takes every VM's egress with it, so an EMFILE on a proxy holding
// a listener per VM must not be fatal (#1569 review). It lives in this
// unix-tagged file because isTemporaryAccept does; an untagged home broke the
// non-unix test build with `undefined: isTemporaryAccept` (#1569 review).
func TestIsTemporaryAccept(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"EMFILE: this process is out of fds", syscall.EMFILE, true},
		{"ENFILE: the system is", syscall.ENFILE, true},
		{"ECONNABORTED: peer gave up mid-handshake", syscall.ECONNABORTED, true},
		{"EINTR", syscall.EINTR, true},
		{"wrapped EMFILE", fmt.Errorf("accept: %w", syscall.EMFILE), true},
		{"EINVAL is permanent", syscall.EINVAL, false},
		{"a plain error is permanent", errors.New("boom"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTemporaryAccept(tc.err); got != tc.want {
				t.Errorf("isTemporaryAccept(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
