//go:build unix

package vmcontrol

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"slices"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const replyTimeout = 10 * time.Second

// Server accepts control connections on a Unix socket and drives a Handler.
type Server struct {
	ln      *net.UnixListener
	allowed []int
	h       Handler

	// hmu serialises every bindings change together with the Handler call that
	// matches it. Without it, dropConn could clear a VM's binding, release mu,
	// and call Unregister after a redialled client had already re-registered
	// that VM: the handler would then refuse the replay as a duplicate, or tear
	// down the binding the new connection had just made. Lock order is hmu then
	// mu; Handler calls happen under hmu, never under mu.
	hmu sync.Mutex

	mu       sync.Mutex
	closed   bool
	conns    map[*serverConn]struct{}
	bindings map[string]*serverConn // vmID -> owning connection
	wg       sync.WaitGroup
}

type serverConn struct {
	c   *net.UnixConn
	vms map[string]struct{}
}

// NewServer listens on path (replacing a stale socket left by a crashed
// predecessor) and restricts the socket to its owner. Peers must additionally
// have a UID in allowedUIDs.
func NewServer(path string, allowedUIDs []int, h Handler) (*Server, error) {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("vm control: %s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	// Peer credentials are the real gate; the mode keeps unrelated users from
	// even connecting. The window before chmod is covered by the parent
	// directory's mode.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return &Server{
		ln: ln, allowed: slices.Clone(allowedUIDs), h: h,
		conns:    map[*serverConn]struct{}{},
		bindings: map[string]*serverConn{},
	}, nil
}

// acceptRetryMin and acceptRetryMax bound the backoff for a TEMPORARY accept
// error, the same 5ms-doubling-to-1s shape net/http.Server.Serve uses.
const (
	acceptRetryMin = 5 * time.Millisecond
	acceptRetryMax = 1 * time.Second
)

// Serve accepts connections until Close, then returns nil.
//
// A TEMPORARY accept error is retried with bounded backoff rather than returned.
// What makes this load-bearing is what the caller does with a returned error:
// startVMControl hands it to fatalf, which exits the process — so an EMFILE or
// ENFILE (plausible on a proxy holding tens of VM listeners plus their tunnels)
// or an ECONNABORTED would kill Cortex and every VM's egress with it, for a
// condition that clears on its own. net/http's own accept loop has retried
// these since forever, for the same reason. Only a permanent error is worth the
// whole flotilla (#1569 review).
func (s *Server) Serve() error {
	var delay time.Duration
	for {
		c, err := s.ln.AcceptUnix()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed || errors.Is(err, net.ErrClosed) {
				return nil
			}
			// net.Error.Temporary() is deprecated and lies about some errors, so
			// the fd-exhaustion syscall errnos are named explicitly; Temporary()
			// is still consulted for anything else that self-describes as
			// transient (ECONNABORTED arrives that way).
			if isTemporaryAccept(err) {
				if delay == 0 {
					delay = acceptRetryMin
				} else if delay *= 2; delay > acceptRetryMax {
					delay = acceptRetryMax
				}
				slog.Warn("vmcontrol: temporary accept error on the control socket; retrying",
					"error", err, "retry_in", delay)
				time.Sleep(delay)
				continue
			}
			return err
		}
		delay = 0
		sc := &serverConn{c: c, vms: map[string]struct{}{}}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = c.Close()
			continue
		}
		s.conns[sc] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go s.serveConn(sc)
	}
}

// Close stops accepting, drops every connection and waits until each
// connection's bindings have been handed to Handler.Unregister.
func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	conns := make([]*serverConn, 0, len(s.conns))
	for sc := range s.conns {
		conns = append(conns, sc)
	}
	s.mu.Unlock()
	err := s.ln.Close()
	for _, sc := range conns {
		_ = sc.c.Close()
	}
	s.wg.Wait()
	return err
}

func (s *Server) serveConn(sc *serverConn) {
	defer s.wg.Done()
	defer s.dropConn(sc)

	uid, err := peerUID(sc.c)
	if err != nil || !slices.Contains(s.allowed, uid) {
		return // closed without a reply: do not tell an unauthorised peer anything
	}
	for {
		body, fds, err := readFrame(sc.c)
		if err != nil {
			return
		}
		reply := s.handle(sc, body, fds)
		_ = sc.c.SetWriteDeadline(time.Now().Add(replyTimeout))
		if err := writeFrame(sc.c, marshalControlReply(reply), -1); err != nil {
			return
		}
	}
}

func (s *Server) handle(sc *serverConn, body []byte, fds []int) *ControlReply {
	req, err := decodeControlRequest(body)
	if err != nil {
		closeFDs(fds)
		return refusal(CodeBadRequest, "malformed request")
	}
	switch {
	case req.Register != nil:
		return s.register(sc, req.Register, fds)
	case req.Unregister != nil:
		closeFDs(fds)
		vm := req.Unregister.VmID
		s.hmu.Lock()
		defer s.hmu.Unlock()
		s.release(vm)
		if err := s.h.Unregister(vm); err != nil {
			return handlerRefusal(err)
		}
		return &ControlReply{Ok: true}
	default:
		closeFDs(fds)
		return refusal(CodeBadRequest, "empty request")
	}
}

func (s *Server) register(sc *serverConn, r *Register, fds []int) *ControlReply {
	if len(fds) == 0 {
		return refusal(CodeBadRequest, "register carries no fd")
	}
	closeFDs(fds[1:]) // only the first fd is meaningful
	ln, err := listenerFromFD(fds[0])
	if err != nil {
		return refusal(CodeBadRequest, err.Error())
	}
	vm := r.VmID
	if vm == "" {
		_ = ln.Close()
		return refusal(CodeBadRequest, "empty vm_id")
	}

	s.hmu.Lock()
	defer s.hmu.Unlock()
	s.mu.Lock()
	owner := s.bindings[vm]
	if owner == sc {
		s.mu.Unlock()
		_ = ln.Close()
		return refusal(CodeDuplicateVM, "vm already registered on this connection")
	}
	// A different connection owning vm is a client that redialled before we
	// noticed its old connection die; the new registration supersedes the stale
	// one.
	s.bindings[vm] = sc
	sc.vms[vm] = struct{}{}
	s.mu.Unlock()
	if owner != nil {
		s.mu.Lock()
		delete(owner.vms, vm)
		s.mu.Unlock()
		_ = s.h.Unregister(vm)
	}

	if err := s.h.Register(vm, r.Sid, r.Grant, ln); err != nil {
		_ = ln.Close()
		s.release(vm)
		return handlerRefusal(err)
	}
	return &ControlReply{Ok: true}
}

// release forgets vm's ownership record.
func (s *Server) release(vm string) {
	s.mu.Lock()
	if o := s.bindings[vm]; o != nil {
		delete(o.vms, vm)
		delete(s.bindings, vm)
	}
	s.mu.Unlock()
}

// dropConn runs when a control connection ends: bindings are owned by the
// connection that registered them, so a vmpool crash or restart cannot leave
// the proxy serving VMs nobody manages.
func (s *Server) dropConn(sc *serverConn) {
	_ = sc.c.Close()
	s.hmu.Lock()
	defer s.hmu.Unlock()
	s.mu.Lock()
	delete(s.conns, sc)
	owned := make([]string, 0, len(sc.vms))
	for vm := range sc.vms {
		if s.bindings[vm] == sc {
			delete(s.bindings, vm)
			owned = append(owned, vm)
		}
	}
	sc.vms = nil
	s.mu.Unlock()
	for _, vm := range owned {
		_ = s.h.Unregister(vm)
	}
}

// listenerFromFD takes ownership of fd, which must be a listening socket.
func listenerFromFD(fd int) (net.Listener, error) {
	acc, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ACCEPTCONN)
	switch {
	case errors.Is(err, unix.ENOPROTOOPT):
		// darwin does not report SO_ACCEPTCONN for AF_UNIX sockets (Linux, the
		// deployment target, does). Probing with accept(2) would steal a queued
		// connection, so development builds accept any stream socket here and a
		// non-listener fails at the handler's first Accept instead.
		if typ, terr := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TYPE); terr != nil || typ != unix.SOCK_STREAM {
			_ = unix.Close(fd)
			return nil, errors.New("fd is not a stream socket")
		}
	case err != nil:
		_ = unix.Close(fd)
		return nil, fmt.Errorf("fd is not a socket: %w", err)
	case acc == 0:
		_ = unix.Close(fd)
		return nil, errors.New("fd is not a listening socket")
	}
	f := os.NewFile(uintptr(fd), "vm-egress-listener")
	defer f.Close() // FileListener dups
	ln, err := net.FileListener(f)
	if err != nil {
		return nil, fmt.Errorf("fd is not a usable listener: %w", err)
	}
	return ln, nil
}

func refusal(code, msg string) *ControlReply {
	return &ControlReply{Ok: false, Code: code, Message: msg}
}

func handlerRefusal(err error) *ControlReply {
	var re *RefusalError
	if errors.As(err, &re) {
		return refusal(re.Code, re.Message)
	}
	return refusal(CodeHandlerError, err.Error())
}

// isTemporaryAccept reports whether an accept error is worth retrying.
//
// EMFILE/ENFILE are the ones that matter here — the process or the system is out
// of descriptors, which a proxy holding a listener per VM plus their tunnels can
// reach, and which clears as soon as something closes. ECONNABORTED is the peer
// giving up mid-handshake. Anything else that still implements the deprecated
// Temporary() and says yes is taken at its word; everything else is permanent.
func isTemporaryAccept(err error) bool {
	if errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) ||
		errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EINTR) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return ne.Temporary() //nolint:staticcheck // SA1019: no replacement for the accept-retry decision; the errnos above carry the cases that matter.
	}
	return false
}
