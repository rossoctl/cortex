package vmcontrol

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rossoctl/cortex/core/listener/forwardproxy"
	"github.com/rossoctl/cortex/core/pipeline"
)

// Limits bounds what one VM, and one session, may consume.
type Limits struct {
	// MaxConnsPerVM bounds one VM's concurrent connections. A connection past
	// it is refused as it is accepted, before any of it is read: answered 429
	// conn_cap, or closed unanswered while maxCapAnswersPerVM answers are in
	// flight. A slot frees when its connection closes.
	MaxConnsPerVM int
	// MaxRequestsPerSession bounds how many requests one session sends,
	// summed across all of its VMs. Each plain request, each CONNECT and each
	// request decrypted inside a bridged tunnel counts one. A request past it
	// is answered 429 request_cap, goes no further, and its connection is
	// closed. The count lives while the session has a registered VM and for
	// sessionIdle after its last one unregisters, so a VM registered again in
	// that window continues it.
	MaxRequestsPerSession int
}

// The default limits are moca-egress's, so a deployment moving from it keeps
// its bounds.
const (
	DefaultMaxConnsPerVM         = 32
	DefaultMaxRequestsPerSession = 10000
)

// Bounds on answering connections over the cap. They are safety bounds, not
// policy, so they are not configurable.
const (
	// maxCapAnswersPerVM is how many conn_cap answers one VM may have in
	// flight. Past it a refused connection is closed with no answer, so a
	// guest in a tight loop cannot grow the shared process's fds.
	maxCapAnswersPerVM = 8
	// capDrainBytes bounds the read after an answer, which lets a guest that
	// already sent its request read the 429 rather than a reset.
	capDrainBytes = 64 << 10
	// capAnswerTimeout bounds how long one answer may hold its connection.
	capAnswerTimeout = time.Second
)

// capEventEvery is how many conn_cap refusals one audit event may stand for,
// between admissions. moca-egress coalesces per second instead.
const capEventEvery = 64

// sessionIdle is how long a session's request count outlives its last VM;
// sweepEvery is how often Register looks for counts past it. Both are
// moca-egress's.
const (
	sessionIdle = time.Hour
	sweepEvery  = time.Minute
)

// Registry is the control channel's Handler: it binds each handed-over
// listener to the forward proxy, one binding per VM, and tears bindings down
// on Unregister. One forwardproxy.Server — one pipeline, one session store —
// serves every VM; which listener accepted the connection is the identity
// (#1477), carried into the pipeline through the request context.
type Registry struct {
	fp     *forwardproxy.Server
	sink   AuditSink
	limits Limits

	now func() time.Time

	mu        sync.Mutex
	vms       map[string]*vmBinding
	sessions  map[string]*sessionRequests
	lastSweep time.Time
}

// NewRegistry returns a Registry serving every registered VM's traffic
// through fp, emitting audit events to sink. A field of limits that is not
// positive takes its default.
func NewRegistry(fp *forwardproxy.Server, sink AuditSink, limits Limits) *Registry {
	if limits.MaxConnsPerVM <= 0 {
		limits.MaxConnsPerVM = DefaultMaxConnsPerVM
	}
	if limits.MaxRequestsPerSession <= 0 {
		limits.MaxRequestsPerSession = DefaultMaxRequestsPerSession
	}
	return &Registry{
		fp: fp, sink: sink, limits: limits, now: time.Now,
		vms: map[string]*vmBinding{}, sessions: map[string]*sessionRequests{},
	}
}

// Limits returns the limits in force, defaults applied.
func (r *Registry) Limits() Limits { return r.limits }

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
	r.mu.Lock()
	b := newVMBinding(r.fp, r.sink, vmID, sessionID, ln, r.limits.MaxConnsPerVM, r.acquire(sessionID))
	old := r.vms[vmID]
	r.vms[vmID] = b
	if old != nil {
		r.release(old.sid)
	}
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
// connection — hijacked tunnels and conn_cap answers included, which
// http.Server.Close would leave open — has been closed, and each in-flight
// request's wrapper has emitted its aborted audit line. A conn_cap event for
// a refusal already answered may still follow.
func (r *Registry) Unregister(vmID string) error {
	r.mu.Lock()
	b := r.vms[vmID]
	delete(r.vms, vmID)
	if b != nil {
		r.release(b.sid)
	}
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
	r.sessions = map[string]*sessionRequests{}
	r.mu.Unlock()
	for _, b := range vms {
		b.close()
	}
}

// acquire returns sid's request count for one more registered VM, first
// dropping the counts whose sessions have had no VM for sessionIdle. Caller
// holds r.mu.
func (r *Registry) acquire(sid string) *sessionRequests {
	if now := r.now(); now.Sub(r.lastSweep) >= sweepEvery {
		r.lastSweep = now
		for id, s := range r.sessions {
			if s.vms == 0 && now.Sub(s.idleSince) > sessionIdle {
				delete(r.sessions, id)
			}
		}
	}
	s := r.sessions[sid]
	if s == nil {
		s = &sessionRequests{max: int64(r.limits.MaxRequestsPerSession)}
		r.sessions[sid] = s
	}
	s.vms++
	return s
}

// release drops one registered VM's hold on sid's request count. With the
// last one, the count starts its sessionIdle. Caller holds r.mu.
func (r *Registry) release(sid string) {
	if s := r.sessions[sid]; s != nil {
		if s.vms--; s.vms == 0 {
			s.idleSince = r.now()
		}
	}
}

// sessionRequests is one session's request count, shared by its VMs' bindings.
type sessionRequests struct {
	max  int64
	used atomic.Int64
	// Guarded by Registry.mu: the session's registered VMs, and when the
	// last of them left.
	vms       int
	idleSince time.Time
}

// charge counts one request and reports whether it is within the cap.
func (s *sessionRequests) charge() bool { return s.used.Add(1) <= s.max }

// trackedListener records every accepted connection so Unregister can close
// them all, and turns away a connection past maxConns as it is accepted.
// http.Server.Close closes only the conns it still owns; a hijacked CONNECT
// tunnel is the handler's, and an aborted VM's tunnel must not outlive its
// Unregister.
type trackedListener struct {
	inner    net.Listener
	maxConns int
	// refused takes each connection over the cap, with what to do about it.
	refused func(net.Conn, capRefusal)

	mu    sync.Mutex
	conns map[*trackedConn]struct{} // nil once closed
	// answering holds the conn_cap answers in flight. reported is set once a
	// refusal has been reported since the last admission, and suppressed
	// counts the refusals since the last report.
	answering  map[net.Conn]struct{}
	reported   bool
	suppressed uint64
}

// capRefusal is how one connection over the cap is turned away.
type capRefusal struct {
	answer    bool   // write the 429; otherwise close at once
	report    bool   // emit an audit event once the answer has closed
	coalesced uint64 // earlier refusals that event stands for
}

func newTrackedListener(inner net.Listener, maxConns int, refused func(net.Conn, capRefusal)) *trackedListener {
	return &trackedListener{
		inner: inner, maxConns: maxConns, refused: refused,
		conns: map[*trackedConn]struct{}{}, answering: map[net.Conn]struct{}{},
	}
}

func (l *trackedListener) Accept() (net.Conn, error) {
	for {
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
		if len(l.conns) >= l.maxConns {
			d := l.chargeCap(c)
			l.mu.Unlock()
			l.refused(c, d)
			continue
		}
		l.reported = false
		tc := &trackedConn{Conn: c, ln: l}
		l.conns[tc] = struct{}{}
		l.mu.Unlock()
		return tc, nil
	}
}

// chargeCap decides how c, over the cap, is turned away. Only an answered
// refusal reports, so the audit write rides an answer slot: the first answered
// refusal since the last admission, and every one after capEventEvery
// refusals. An answer is tracked until answerCap ends it. Caller holds l.mu.
func (l *trackedListener) chargeCap(c net.Conn) capRefusal {
	if len(l.answering) >= maxCapAnswersPerVM {
		l.suppressed++
		return capRefusal{}
	}
	l.answering[c] = struct{}{}
	d := capRefusal{answer: true}
	if l.reported && l.suppressed < capEventEvery-1 {
		l.suppressed++
		return d
	}
	d.report, d.coalesced = true, l.suppressed
	l.reported, l.suppressed = true, 0
	return d
}

// answerCap writes the conn_cap refusal naming vmID to c, closes c, then runs
// report if it is set. The write and drain take at most capAnswerTimeout, or
// end when closeAll closes c. The answer slot chargeCap took is held until
// report returns.
func (l *trackedListener) answerCap(c net.Conn, vmID string, report func()) {
	defer func() {
		l.mu.Lock()
		delete(l.answering, c)
		l.mu.Unlock()
	}()
	status, h, body := pipeline.MocaRefusal(http.StatusTooManyRequests, DecisionConnCap, vmID).Violation.Render()
	resp := &http.Response{
		StatusCode: status, ProtoMajor: 1, ProtoMinor: 1, Header: h,
		ContentLength: int64(len(body)), Body: io.NopCloser(bytes.NewReader(body)), Close: true,
	}
	_ = c.SetDeadline(time.Now().Add(capAnswerTimeout))
	_ = resp.Write(c)
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(c, capDrainBytes))
	_ = c.Close()
	if report != nil {
		report()
	}
}

// Close is the net.Listener method: it stops accepting but leaves live
// connections alone, matching http.Server's use of it. Unregister goes through
// closeAll instead.
func (l *trackedListener) Close() error {
	return l.inner.Close()
}

func (l *trackedListener) Addr() net.Addr { return l.inner.Addr() }

// closeAll closes the listener, every tracked connection and every conn_cap
// answer in flight.
func (l *trackedListener) closeAll() {
	_ = l.inner.Close()
	l.mu.Lock()
	conns := l.conns
	l.conns = nil
	var answering []net.Conn
	for c := range l.answering {
		answering = append(answering, c)
	}
	l.mu.Unlock()
	for c := range conns {
		_ = c.Close()
	}
	for _, c := range answering {
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
