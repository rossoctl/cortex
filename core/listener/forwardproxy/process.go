package forwardproxy

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/rossoctl/cortex/core/attribution"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/tlsbridge"
)

type connProcKey struct{}

// ConnContext is the forward proxy's http.Server ConnContext. It gives each client
// connection an empty slot for its process chain and nothing else: it runs on the accept
// loop, so the lookup waits for the connection's first request.
func (s *Server) ConnContext(ctx context.Context, _ net.Conn) context.Context {
	if s.Processes == nil {
		return ctx
	}
	return context.WithValue(ctx, connProcKey{}, &attribution.Memo{})
}

func connProcOf(ctx context.Context) *attribution.Memo {
	m, _ := ctx.Value(connProcKey{}).(*attribution.Memo)
	return m
}

// processesOn reports whether header-less requests are filed by client process. Like
// affinityOn it needs header bucketing: a process's session is the one its header named.
func (s *Server) processesOn() bool {
	return s.Processes != nil && s.Sessions != nil && len(s.SessionIDHeaders) > 0
}

// clientChain is the process chain behind r's connection — the client first, then its
// parents — or nil when process attribution is off or the client could not be named, and
// then resolution is exactly as without it.
func (s *Server) clientChain(r *http.Request) []attribution.Proc {
	if !s.processesOn() {
		return nil
	}
	m := connProcOf(r.Context())
	if m == nil {
		return nil
	}
	return s.memoChain(r, m)
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
	m := connProcOf(r.Context())
	if m == nil {
		return tlsbridge.Program{}, false
	}
	return s.attributor().Program(s.memoChain(r, m))
}

// memoChain is the process chain behind r's connection, looked up once per connection
// through m. It logs a failed lookup once, at DEBUG.
func (s *Server) memoChain(r *http.Request, m *attribution.Memo) []attribution.Proc {
	key, _ := attribution.ConnKeyOf(r)
	chain, err := s.attributor().Chain(s.Processes, key, m)
	if err != nil {
		slog.Debug("forward-proxy: client process not found, filing as without process attribution",
			"client", r.RemoteAddr, "error", err)
	}
	return chain
}

// selfTraffic reports whether r is an agent talking to its own service on this host: a
// plain-HTTP request to a loopback listener held by an agent's own process (see
// attribution's SelfTraffic) running the same executable as the client. OpenCode's TUI
// polling its background service is the case it exists for: about 1.3 requests a second
// of the agent's own UI, sent nowhere. Comparing executables alone would hide a node-based agent's calls to
// any local node server, which is why the listener must be an agent's.
func (s *Server) selfTraffic(r *http.Request, chain []attribution.Proc) bool {
	m := connProcOf(r.Context())
	if m == nil {
		return false
	}
	svc, pid, ok := s.attributor().SelfTraffic(s.Processes, m, chain, loopbackDests(r), r.Header)
	if ok {
		s.noteSelfTraffic(chain[0].Exe, svc, pid)
	}
	return ok
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

// RestoreClaims puts back the process claims in sums, the session archive's summaries, so a
// process that named a session before a restart still answers with it before it names it
// again. A no-op unless process attribution is on. Call it before the proxy serves.
func (s *Server) RestoreClaims(sums []session.SessionSummary) {
	if !s.processesOn() {
		return
	}
	a := s.attributor()
	for _, sum := range sums {
		for p, sp := range sum.ProcSpans() {
			a.Restore(sum.ID, p, sp.Last)
		}
	}
}
