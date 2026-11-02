package vmcontrol

import (
	"net"
	"sync"

	"github.com/rossoctl/cortex/core/listener/forwardproxy"
)

// Registry is the control channel's Handler: it binds each handed-over
// listener to the forward proxy, one binding per VM, and tears bindings down
// on Unregister. One forwardproxy.Server — one pipeline, one session store —
// serves every VM; which listener accepted the connection is the identity
// (#1477), carried into the pipeline through the request context.
type Registry struct {
	fp   *forwardproxy.Server
	sink AuditSink

	mu  sync.Mutex
	vms map[string]*vmBinding
}

// NewRegistry returns a Registry serving every registered VM's traffic
// through fp, emitting audit events to sink.
func NewRegistry(fp *forwardproxy.Server, sink AuditSink) *Registry {
	return &Registry{fp: fp, sink: sink, vms: map[string]*vmBinding{}}
}

// Register binds vmID's listener. The Registry owns ln from here on.
//
// A non-empty grant is refused: this slice has no grants (tenancy is decided
// once at boot, by MOCA_TENANCY in cmd/cortex), and refusing here rather than
// ignoring the grant keeps a vmpool that thinks it sent one from running a VM
// with weaker isolation than it configured.
func (r *Registry) Register(vmID, sessionID string, grant []byte, ln net.Listener) error {
	if len(grant) != 0 {
		return &RefusalError{Code: CodeBadRequest, Message: "grants are not supported in this slice (MI1 S2)"}
	}
	b := newVMBinding(r.fp, r.sink, vmID, sessionID, ln)
	r.mu.Lock()
	old := r.vms[vmID]
	r.vms[vmID] = b
	r.mu.Unlock()
	// The control server already refuses a duplicate on the same connection
	// and supersedes across connections; this covers a Register that reaches
	// the Registry by any other path.
	if old != nil {
		old.close()
	}
	go func() { _ = b.srv.Serve(b.ln) }() // returns ErrServerClosed in close
	return nil
}

// Unregister drops vmID's binding. An unknown vmID is not an error.
//
// By the time it returns, the listener refuses new dials, every live
// connection — hijacked tunnels included, which http.Server.Close would leave
// open — has been closed, and each in-flight request's wrapper has emitted its
// aborted audit line.
func (r *Registry) Unregister(vmID string) error {
	r.mu.Lock()
	b := r.vms[vmID]
	delete(r.vms, vmID)
	r.mu.Unlock()
	if b != nil {
		b.close()
	}
	return nil
}

// Close unregisters everything (process shutdown).
func (r *Registry) Close() {
	r.mu.Lock()
	vms := r.vms
	r.vms = map[string]*vmBinding{}
	r.mu.Unlock()
	for _, b := range vms {
		b.close()
	}
}

// trackedListener records every accepted connection so Unregister can close
// them all. http.Server.Close closes only the conns it still owns; a hijacked
// CONNECT tunnel is the handler's, and an aborted VM's tunnel must not outlive
// its Unregister.
type trackedListener struct {
	inner net.Listener

	mu    sync.Mutex
	conns map[*trackedConn]struct{} // nil once closed
}

func newTrackedListener(inner net.Listener) *trackedListener {
	return &trackedListener{inner: inner, conns: map[*trackedConn]struct{}{}}
}

func (l *trackedListener) Accept() (net.Conn, error) {
	c, err := l.inner.Accept()
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	if l.conns == nil {
		l.mu.Unlock()
		_ = c.Close()
		return nil, net.ErrClosed
	}
	tc := &trackedConn{Conn: c, ln: l}
	l.conns[tc] = struct{}{}
	l.mu.Unlock()
	return tc, nil
}

// Close is the net.Listener method: it stops accepting but leaves live
// connections alone, matching http.Server's use of it. Unregister goes through
// closeAll instead.
func (l *trackedListener) Close() error {
	return l.inner.Close()
}

func (l *trackedListener) Addr() net.Addr { return l.inner.Addr() }

// closeAll closes the listener and every tracked connection.
func (l *trackedListener) closeAll() {
	_ = l.inner.Close()
	l.mu.Lock()
	conns := l.conns
	l.conns = nil
	l.mu.Unlock()
	for c := range conns {
		_ = c.Close()
	}
}

func (l *trackedListener) remove(c *trackedConn) {
	l.mu.Lock()
	if l.conns != nil {
		delete(l.conns, c)
	}
	l.mu.Unlock()
}

// trackedConn is an accepted connection that removes itself from its listener
// on Close.
type trackedConn struct {
	net.Conn
	ln   *trackedListener
	once sync.Once
}

func (c *trackedConn) Close() error {
	c.once.Do(func() { c.ln.remove(c) })
	return c.Conn.Close()
}
