package vmcontrol

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rossoctl/cortex/core/listener/forwardproxy"
	"github.com/rossoctl/cortex/core/session"
)

// vmBinding is one registered VM: its listener, the http.Server accepting on
// it, and the state Unregister needs to abort it.
type vmBinding struct {
	vmID  string
	sid   string
	inner http.Handler // the forward proxy's own handler
	sink  AuditSink
	ln    *trackedListener
	srv   *http.Server

	// mu guards aborted and the live-set join below it.
	mu      sync.Mutex
	aborted bool
	live    sync.WaitGroup // in-flight request wrappers
}

func newVMBinding(fp *forwardproxy.Server, sink AuditSink, vmID, sid string, ln net.Listener) *vmBinding {
	b := &vmBinding{
		vmID: vmID, sid: sid,
		inner: fp.Handler(), sink: sink,
		ln: newTrackedListener(ln),
	}
	// ReadHeaderTimeout matches bootstrap's, which every other http.Server in
	// the binary gets. Without it a guest can open connections and drip headers
	// forever, each one holding a goroutine and an fd until Unregister — and the
	// fds are the scarcer half, since one proxy holds a listener per VM plus
	// their tunnels. Per-VM caps are #1485's; this is the one-line floor (also
	// gosec G112) and it costs the conformance cases nothing (#1569 review).
	b.srv = &http.Server{Handler: b, ReadHeaderTimeout: 10 * time.Second}
	return b
}

// requestBinding is what a VM listener stamps into the request context.
type requestBinding struct{ vmID, sid string }

// bindingCtxKey is a NAMED type, like the forward proxy's connProcKey and the
// transparent listener's origDstKey. An untyped `var bindingCtxKey struct{}`
// would be a zero-size value whose type is plain struct{}, so every package
// doing the same thing shares one context key: a foreign reader would see this
// VM's binding, and a foreign writer would overwrite it with a value this
// package would then type-assert. Naming the type makes the key unique to
// vmcontrol (and satisfies SA1029) (#1569 review).
type bindingCtxKey struct{}

// SessionIDFromRequest returns the session id the VM listener resolved for r
// — from which handed-over socket its connection arrived — or "" when r did
// not come through one. It is the forward proxy's TrustedSessionID hook: the
// answer is the listener's own, so nothing the client sent participates.
func SessionIDFromRequest(r *http.Request) string {
	if b, ok := r.Context().Value(bindingCtxKey{}).(requestBinding); ok {
		return b.sid
	}
	return ""
}

// AdoptBridgedRequest is the forward proxy's AdoptBridgedRequest hook: it
// carries a VM's binding from the CONNECT onto each request that tunnel
// decrypts, and strips the guest's identity headers from them.
//
// It is needed because a bridged request never passes through vmBinding.
// ServeHTTP: tlsbridge.ServeConn parses the decrypted bytes into fresh
// requests inside its own http.Server, so the context the wrapper stamped on
// the CONNECT is not theirs and the headers it stripped were a different
// header set. Without this, SessionIDFromRequest answers "" on a decrypted
// request, hydrateSession falls through to header resolution, and a guest
// sending X-Claude-Code-Session-Id files its traffic in any session it likes
// — the trusted-session seam inverted, and the guest headers reach the origin
// too. Pinned by TestBridgedRequestKeepsTheVMBinding (#1569 review).
//
// Returns r unchanged when the CONNECT carried no binding, so a bridged
// tunnel on a non-VM listener is untouched.
func AdoptBridgedRequest(connect, r *http.Request) *http.Request {
	b, ok := connect.Context().Value(bindingCtxKey{}).(requestBinding)
	if !ok {
		return r
	}
	stripGuestHeaders(r)
	return r.WithContext(context.WithValue(r.Context(), bindingCtxKey{}, b))
}

// guestStrippedHeaders are the client-asserted identity headers a VM guest
// cannot be allowed to speak. Session headers would otherwise be honored by
// the proxy's own header bucketing; Proxy-Authorization reaches nothing that
// authenticates a VM (the forward proxy deletes it too, but the guest's copy
// must not survive even to the pipeline). X-Moca-* is stripped by prefix below.
var guestStrippedHeaders = []string{
	"Proxy-Authorization",
	session.ClaudeCodeSessionHeader,
	session.BobSessionHeader,
	session.SessionIDHeader,
	session.CodexSessionHeader,
}

// stripGuestHeaders removes everything from r that could move the request to
// another session or assert an identity the connection did not establish.
func stripGuestHeaders(r *http.Request) {
	for _, name := range guestStrippedHeaders {
		r.Header.Del(name)
	}
	for name := range r.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-moca-") {
			r.Header.Del(name)
		}
	}
}

// ServeHTTP is the per-VM wrapper around the forward proxy's handler: it
// strips guest-asserted identity headers, carries the binding in the request
// context (the trusted-session seam), and emits exactly one terminal audit
// line per request when the inner handler returns.
func (b *vmBinding) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	stripGuestHeaders(r)

	// Join the live set under mu, or refuse: an Add racing Unregister's Wait
	// could be missed and hang nothing but its own accounting. Past this gate,
	// Unregister's closeAll is what wakes the inner handler (its connections
	// are closed under it), and live.Wait does not return until the aborted
	// line below has been emitted.
	b.mu.Lock()
	if b.aborted {
		b.mu.Unlock()
		// The binding is gone and no request ran — but a bare return is not how
		// to say so. net/http would send an empty 200 OK on a connection that is
		// still open in the window between close() setting aborted and closeAll()
		// closing the conns, so the guest reads "success" for a request that
		// never left, and the one-terminal-line-per-request promise has a hole
		// exactly where the audit matters. 503 with Connection: close says it to
		// the guest, and the line says it to the operator (#1569 review).
		w.Header().Set("Connection", "close")
		http.Error(w, "egress binding closed", http.StatusServiceUnavailable)
		b.emit(r, http.StatusServiceUnavailable)
		return
	}
	b.live.Add(1)
	b.mu.Unlock()
	defer b.live.Done()

	r = r.WithContext(context.WithValue(r.Context(), bindingCtxKey{}, requestBinding{b.vmID, b.sid}))
	sw := newStatusWriter(w)
	b.inner.ServeHTTP(sw, r)
	b.emit(r, sw.status)
}

// emit writes the request's one terminal audit line. A CONNECT that was
// established reports 200: the proxy writes that status itself on the raw
// connection after hijacking, where statusWriter cannot see it.
func (b *vmBinding) emit(r *http.Request, status int) {
	e := Event{
		VMID: b.vmID, SID: b.sid,
		Host: r.Host, Method: r.Method,
		Status: status,
	}
	b.mu.Lock()
	aborted := b.aborted
	b.mu.Unlock()
	switch {
	case aborted:
		e.Decision = DecisionAborted
	case r.Method == http.MethodConnect:
		e.Decision = DecisionTunnel
	default:
		e.Decision = DecisionForward
		e.Path = RedactPath(r.URL)
	}
	b.sink.Emit(e)
}

func (b *vmBinding) close() {
	b.mu.Lock()
	b.aborted = true
	b.mu.Unlock()
	b.ln.closeAll() // wakes every in-flight handler, hijacked tunnels included
	b.live.Wait()   // the aborted lines are emitted before Unregister returns
	_ = b.srv.Close()
}

// statusWriter captures the status the client saw. It forwards Hijack — an
// opaque CONNECT tunnel is the one request shape whose bytes never pass
// through it — and Flush, so streaming responses keep streaming.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func newStatusWriter(w http.ResponseWriter) *statusWriter {
	return &statusWriter{ResponseWriter: w, status: http.StatusOK}
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("vmcontrol: inner ResponseWriter does not support Hijack")
	}
	return h.Hijack()
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
