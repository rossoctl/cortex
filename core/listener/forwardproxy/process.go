package forwardproxy

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rossoctl/cortex/core/peerproc"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/tlsbridge"
)

// maxChain is how many processes a client's chain holds — the client and its parents —
// enough to reach the agent from a tool its shell ran.
const maxChain = 16

// maxHints is how many processes known to have named a session a lookup looks at first.
// On Linux each costs a scan of that process's open files.
const maxHints = 8

type connProcKey struct{}

// connProc is one client connection's process chain. It is looked up on the connection's
// first request — while the client is certainly connected, since a process that has exited
// cannot be looked up — and reused by every later request on it, including those a bridged
// CONNECT decrypts.
type connProc struct {
	once  sync.Once
	chain []session.Proc

	// mu guards listeners: which process listens behind each loopback destination this
	// connection's requests went to, for selfTraffic.
	mu        sync.Mutex
	listeners map[netip.AddrPort]listenerSeen
}

// listenerSeen is one cached ListenerOwner answer.
type listenerSeen struct {
	proc  peerproc.Proc
	found bool
	at    time.Time
}

// listenerTTL is how long a connection trusts a cached listener answer. Short, because a
// service can restart; whether the listener is an agent's is re-asked on every request,
// since a service that is not the client's child becomes one only when it first names a
// session.
const listenerTTL = 30 * time.Second

// ConnContext is the forward proxy's http.Server ConnContext. It gives each client
// connection an empty slot for its process chain and nothing else: it runs on the accept
// loop, so the lookup waits for the connection's first request.
func (s *Server) ConnContext(ctx context.Context, _ net.Conn) context.Context {
	if s.Processes == nil {
		return ctx
	}
	return context.WithValue(ctx, connProcKey{}, &connProc{})
}

func connProcOf(ctx context.Context) *connProc {
	cp, _ := ctx.Value(connProcKey{}).(*connProc)
	return cp
}

// processesOn reports whether header-less requests are filed by client process. Like
// affinityOn it needs header bucketing: a process's session is the one its header named.
func (s *Server) processesOn() bool {
	return s.Processes != nil && s.Sessions != nil && len(s.SessionIDHeaders) > 0
}

// clientChain is the process chain behind r's connection — the client first, then its
// parents — or nil when process attribution is off or the client could not be named, and
// then resolution is exactly as without it.
func (s *Server) clientChain(r *http.Request) []session.Proc {
	if !s.processesOn() {
		return nil
	}
	cp := connProcOf(r.Context())
	if cp == nil {
		return nil
	}
	cp.once.Do(func() { cp.chain = s.lookupChain(r) })
	return cp.chain
}

// bridgeProgram names the program behind r's connection for the TLS bridge's
// per-program rules, and reports false when it cannot: no process lookup here, no
// program memory on the bridge, a lookup that found nothing, or an executable whose
// path could not be read. Those clients keep the host memory's behaviour.
//
// It shares the connection's process chain with clientChain, looked up once, but not
// clientChain's precondition. Session filing needs header bucketing; whether a program
// trusts a certificate has nothing to do with headers.
func (s *Server) bridgeProgram(r *http.Request) (tlsbridge.Program, bool) {
	if s.Processes == nil || s.Sessions == nil || s.TLSBridge == nil || s.TLSBridge.Programs == nil {
		return tlsbridge.Program{}, false
	}
	cp := connProcOf(r.Context())
	if cp == nil {
		return tlsbridge.Program{}, false
	}
	cp.once.Do(func() { cp.chain = s.lookupChain(r) })
	if len(cp.chain) == 0 || cp.chain[0].Exe == "" {
		return tlsbridge.Program{}, false
	}
	p := tlsbridge.Program{Exe: cp.chain[0].Exe, PID: cp.chain[0].PID, Start: cp.chain[0].Start}
	for _, proc := range cp.chain {
		if s.Sessions.IsAgentProcess(proc) {
			p.Agent = proc.Exe
			break
		}
	}
	return p, true
}

func (s *Server) lookupChain(r *http.Request) []session.Proc {
	client, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return nil
	}
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return nil
	}
	server, err := netip.ParseAddrPort(local.String())
	if err != nil {
		return nil
	}
	owner, err := s.Processes.ConnOwner(client, server, s.Sessions.ProcessHints(maxHints)...)
	if err != nil {
		slog.Debug("forward-proxy: client process not found, filing as without process attribution",
			"client", r.RemoteAddr, "error", err)
		return nil
	}
	procs, err := s.Processes.Ancestry(owner.PID, maxChain)
	if err != nil || len(procs) == 0 {
		procs = []peerproc.Proc{owner}
	}
	chain := make([]session.Proc, len(procs))
	for i, p := range procs {
		chain[i] = session.Proc{PID: p.PID, Start: p.Start.UnixNano(), Exe: p.Exe}
	}
	return chain
}

// selfTraffic reports whether r is an agent talking to its own service on this host: a
// plain-HTTP request to a loopback listener held by an agent's own process (see
// agentsOwnService) running the same executable as the client. OpenCode's TUI polling its
// background service is the case it exists for: about 1.3 requests a second of the agent's
// own UI, sent nowhere. Comparing executables alone would hide a node-based agent's calls to
// any local node server, which is why the listener must be an agent's.
func (s *Server) selfTraffic(r *http.Request, chain []session.Proc) bool {
	if len(chain) == 0 || chain[0].Exe == "" {
		return false
	}
	cp := connProcOf(r.Context())
	if cp == nil {
		return false
	}
	// Every listener the request could reach must qualify: "localhost" names both
	// loopbacks, and which one the dial takes is not known yet.
	var svc netip.AddrPort
	var pid int32
	found := false
	for _, dest := range loopbackDests(r) {
		l, ok := s.listenerBehind(cp, dest)
		if !ok {
			continue
		}
		if l.Exe != chain[0].Exe || !s.agentsOwnService(r, chain[0], l) {
			return false
		}
		svc, pid, found = dest, l.PID, true
	}
	if found {
		s.noteSelfTraffic(chain[0].Exe, svc, pid)
	}
	return found
}

// agentsOwnService reports whether l, a listener running the client's executable, is an
// agent's own process: one that has named a session through its own header, which an MCP or
// model server never does, or the direct child of a client that says it is a coding agent.
//
// The child half needs no history, and that is what it is for. Claims live in memory, so
// after a proxy restart a service that has not been asked to do anything yet has named
// nothing — and an idle OpenCode window's polling was recorded, a few rows a second, into
// its TUI's pending bucket until the next prompt. OpenCode starts its service as the TUI's
// child. The agent's User-Agent is required because a test suite starting a server of its
// own interpreter is the same shape, and that is the agent's work; a child cannot start
// before its parent, so a listener that does is another process's, under a reused pid.
func (s *Server) agentsOwnService(r *http.Request, client session.Proc, l peerproc.Proc) bool {
	if l.PPID == client.PID && l.Start.UnixNano() >= client.Start && affinityClient(r.Header) != "" {
		return true
	}
	return s.Sessions.IsAgentProcess(session.Proc{PID: l.PID, Start: l.Start.UnixNano()})
}

// listenerBehind is which process listens at dest, asked of the kernel at most once per
// listenerTTL on each connection. A miss is cached the same way: nothing listening, or a
// listener the lookup cannot name, is trusted for as long as a hit.
func (s *Server) listenerBehind(cp *connProc, dest netip.AddrPort) (peerproc.Proc, bool) {
	now := time.Now()
	cp.mu.Lock()
	seen, ok := cp.listeners[dest]
	cp.mu.Unlock()
	if ok && now.Sub(seen.at) < listenerTTL {
		return seen.proc, seen.found
	}
	p, err := s.Processes.ListenerOwner(dest, s.Sessions.ProcessHints(maxHints)...)
	seen = listenerSeen{proc: p, found: err == nil, at: now}
	cp.mu.Lock()
	if cp.listeners == nil {
		cp.listeners = make(map[netip.AddrPort]listenerSeen, 1)
	}
	cp.listeners[dest] = seen
	cp.mu.Unlock()
	return seen.proc, seen.found
}

// loopbackDests is where r goes when that is this host: its address when the URL names a
// loopback one, both loopbacks for "localhost" — the proxy has not dialled yet, so which
// one the name resolves to is not known — and nothing otherwise.
func loopbackDests(r *http.Request) []netip.AddrPort {
	host, port := r.URL.Hostname(), r.URL.Port()
	if port == "" {
		port = "80"
		if strings.EqualFold(r.URL.Scheme, "https") {
			port = "443"
		}
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return nil
	}
	if strings.EqualFold(host, "localhost") {
		return []netip.AddrPort{
			netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), uint16(n)),
			netip.AddrPortFrom(netip.IPv6Loopback(), uint16(n)),
		}
	}
	a, err := netip.ParseAddr(host)
	if err != nil || !a.IsLoopback() {
		return nil
	}
	return []netip.AddrPort{netip.AddrPortFrom(a.Unmap(), uint16(n))}
}

// noteSelfTraffic says once, at INFO, which program's traffic to which local service is
// not being recorded — a row that silently stops appearing is the thing nobody can debug —
// and every time at DEBUG.
func (s *Server) noteSelfTraffic(exe string, dest netip.AddrPort, pid int32) {
	if _, seen := s.selfTrafficSeen.LoadOrStore(exe+" "+dest.String(), struct{}{}); !seen {
		slog.Info("forward-proxy: not recording an agent's traffic to its own service on this host",
			"exe", exe, "service", dest.String(), "service_pid", pid)
	}
	slog.Debug("forward-proxy: agent self-traffic forwarded without recording", "exe", exe, "service", dest.String(), "service_pid", pid)
}
