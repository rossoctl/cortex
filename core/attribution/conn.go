package attribution

import (
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/rossoctl/cortex/core/peerproc"
	"github.com/rossoctl/cortex/core/tlsbridge"
)

// ConnKey names one client connection by its two ends, as the accepting listener sees
// them. A key with an invalid end names no connection.
type ConnKey struct{ Client, Server netip.AddrPort }

// ConnKeyOf is r's connection: r.RemoteAddr and the http.LocalAddrContextKey address.
// ok is false when either is missing or does not parse.
func ConnKeyOf(r *http.Request) (key ConnKey, ok bool) {
	client, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return ConnKey{}, false
	}
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return ConnKey{}, false
	}
	server, err := netip.ParseAddrPort(local.String())
	if err != nil {
		return ConnKey{}, false
	}
	return ConnKey{Client: client, Server: server}, true
}

// Memo is one client connection's attribution state: its process chain, looked up once,
// and which process listens behind each loopback destination its requests went to. The
// zero Memo is ready to use. Share one *Memo between a CONNECT and the requests a bridged
// tunnel decrypts. Do not copy a Memo.
type Memo struct {
	once  sync.Once
	chain []Proc

	// projectOnce and project hold the connection's project label, for Project.
	projectOnce sync.Once
	project     string

	// mu guards listeners: which process listens behind each loopback destination this
	// connection's requests went to, for SelfTraffic.
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

// Chain is the process chain behind key, the client first. The first call on m looks it
// up, while the client is connected. Every later call on m returns the same chain. A key
// with an invalid end, or a failed lookup, keeps nil. err is the lookup's error, given only
// to the call that made the lookup, so the caller logs a failure once per connection.
// m must not be nil.
func (a *Attributor) Chain(res peerproc.Resolver, key ConnKey, m *Memo) ([]Proc, error) {
	var err error
	m.once.Do(func() {
		if !key.Client.IsValid() || !key.Server.IsValid() {
			return
		}
		m.chain, err = a.Lookup(res, key.Client, key.Server)
	})
	return m.chain, err
}

// Project is the connection's project label: the first call on m runs compute, and every
// later call on m returns that value. A keep-alive connection keeps its first label: a
// process that changes its cwd, or a later request with a different Attribution.Proc, keeps
// the old label until the client reconnects. m must not be nil.
func (m *Memo) Project(compute func() string) string {
	m.projectOnce.Do(func() { m.project = compute() })
	return m.project
}

// Program names chain's client program for the TLS bridge's per-program rules: the
// client's executable, its pid and start, and the executable of the nearest agent process
// in chain. ok is false when chain is empty or the client's executable is unknown.
func (a *Attributor) Program(chain []Proc) (p tlsbridge.Program, ok bool) {
	if len(chain) == 0 || chain[0].Exe == "" {
		return tlsbridge.Program{}, false
	}
	p = tlsbridge.Program{Exe: chain[0].Exe, PID: chain[0].PID, Start: chain[0].Start}
	for _, proc := range chain {
		if a.IsAgentProcess(proc) {
			p.Agent = proc.Exe
			break
		}
	}
	return p, true
}

// SelfTraffic reports whether a plain-HTTP request from chain's client to dests (every
// loopback listener the request can reach) is an agent talking to its own service on this
// host. Every listener found must run the client's executable and be an agent's own
// process. h is the client's request headers (its User-Agent). svc and pid name the last
// qualifying listener, for the caller's log. m is the client connection's Memo, and must
// not be nil.
func (a *Attributor) SelfTraffic(res peerproc.Resolver, chain []Proc, dests []netip.AddrPort, h http.Header, m *Memo) (svc netip.AddrPort, pid int32, ok bool) {
	if len(chain) == 0 || chain[0].Exe == "" {
		return netip.AddrPort{}, 0, false
	}
	// Every listener the request could reach must qualify: "localhost" names both
	// loopbacks, and which one the dial takes is not known yet.
	found := false
	for _, dest := range dests {
		l, ok := a.listenerBehind(res, m, dest)
		if !ok {
			continue
		}
		if l.Exe != chain[0].Exe || !a.agentsOwnService(h, chain[0], l) {
			return netip.AddrPort{}, 0, false
		}
		svc, pid, found = dest, l.PID, true
	}
	return svc, pid, found
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
func (a *Attributor) agentsOwnService(h http.Header, client Proc, l peerproc.Proc) bool {
	if l.PPID == client.PID && l.Start.UnixNano() >= client.Start && AgentOf(h) != "" {
		return true
	}
	return a.IsAgentProcess(Proc{PID: l.PID, Start: l.Start.UnixNano()})
}

// listenerBehind is which process listens at dest, asked of the kernel at most once per
// listenerTTL on each connection. A miss is cached the same way: nothing listening, or a
// listener the lookup cannot name, is trusted for as long as a hit.
func (a *Attributor) listenerBehind(res peerproc.Resolver, m *Memo, dest netip.AddrPort) (peerproc.Proc, bool) {
	now := a.now()
	m.mu.Lock()
	seen, ok := m.listeners[dest]
	m.mu.Unlock()
	if ok && now.Sub(seen.at) < listenerTTL {
		return seen.proc, seen.found
	}
	p, err := res.ListenerOwner(dest, a.ProcessHints(maxHints)...)
	seen = listenerSeen{proc: p, found: err == nil, at: now}
	m.mu.Lock()
	if m.listeners == nil {
		m.listeners = make(map[netip.AddrPort]listenerSeen, 1)
	}
	m.listeners[dest] = seen
	m.mu.Unlock()
	return seen.proc, seen.found
}
