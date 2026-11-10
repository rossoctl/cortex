package main

import (
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/rossoctl/cortex/core/peerproc"
)

// A bind refused because something else already holds the address is a condition, not a
// crash, and it is the startup failure a user is most likely to meet by accident: a local
// install's ports are pinned (47600-47604), so a second install on the same machine cannot
// start at all. All that was ever said about it was net.Listen's own line —
//
//	forward-proxy listen: listen tcp 127.0.0.1:47600: bind: address already in use
//
// which names the address and nothing about who holds it. Worse, the companion symptom is
// a self-signed-certificate error in the agent, because requests reach whichever install
// won the port and are bridged with *its* CA; every generated CA shares one subject, so
// only the fingerprint differs and the ports are the only place the clash is visible.
//
// So this file answers the question that message raises — which process, and what to do
// about it — and exits with a status the supervisor can tell apart from a crash. See
// supervise.go for the other half: the restart loop that used to repeat the same
// unexplained line every 30 seconds for as long as the machine was up.
//
// install.sh refuses to install over a foreign proxy on the forward port, which covers the
// install-time case; this is the runtime one, which no install-time check can reach — the
// other install may be started, or stopped, long after this one was set up.

// exitPortInUse is the status this process leaves with when it could not bind because
// something already holds the address, as opposed to 1 for every other startup failure.
// The supervisor reads it to tell "nothing will fix this but freeing the address" from a
// crash worth restarting at speed. Nothing outside this package depends on the number.
const exitPortInUse = 3

// cortexBinaries are the basenames this proxy has shipped under, and its praxis-mode
// gateway, which holds praxis.listen. The Cortex remedy — stop
// the other install — is only right for these: a sidecar that cannot bind :8080 in a pod
// has met something entirely unrelated, and must not be sent looking for a second laptop
// install it does not have.
var cortexBinaries = map[string]bool{"cortex": true, "authbridge-proxy": true, "cortex-gateway": true}

// listenError is a socket that could not be bound: which one, and at what address. The
// address is the part a caller cannot otherwise recover — it lives only inside
// net.Listen's message — and naming the holder needs it.
type listenError struct {
	name string // the plan's name for the socket, e.g. "forward-proxy"
	addr string // as configured, e.g. "127.0.0.1:47600"
	err  error  // net.Listen's, which wraps syscall.EADDRINUSE when that was the cause
}

// Error keeps net.Listen's wording, which the operator has already learned to search for.
func (e *listenError) Error() string { return fmt.Sprintf("%s listen: %v", e.name, e.err) }

func (e *listenError) Unwrap() error { return e.err }

// holder is the process holding an address this proxy wanted.
type holder struct {
	pid  int32
	ppid int32
	exe  string // absolute path, or "" when the kernel would not say
}

// reportListenFailure says why no socket could be bound and ends the process. It does not
// return. log.Fatalf rather than fatalf is right here and only here — see main's bind
// site: nothing on disk is open yet for fatalf to flush.
func reportListenFailure(err error) {
	var le *listenError
	if !errors.As(err, &le) || !errors.Is(err, syscall.EADDRINUSE) {
		// A privileged port, an address no interface has, a plan the config made
		// impossible: nothing here can say more about it than the error already does.
		log.Fatalf("%v", err)
	}

	// cause is net.Listen's own message, kept verbatim on every branch below: it is the
	// wording operators have learned to search for ("address already in use"), and the
	// sentences here deliberately do not all repeat it.
	f := []any{"listener", le.name, "address", le.addr, "cause", le.err}
	h, herr := holderOf(le.addr)
	switch {
	case herr != nil:
		f = append(f, "lookup_error", herr,
			"fix", "name it with `lsof -nP -iTCP:"+portOf(le.addr)+" -sTCP:LISTEN`, or `ss -ltnp` on Linux, then stop it or give "+le.name+" another address in the config")
		slog.Error("cannot start: this address is already in use, and the process holding it could not be named", f...)

	case cortexBinaries[filepath.Base(h.exe)]:
		f = append(f, holderFields(h)...)
		f = append(f,
			"reason", "a local install's ports are pinned, so two Cortex installs on one machine cannot both bind them",
			"fix", "stop the other one — `agentop service stop` as the user who owns "+h.exe+", or uninstall it — and this one starts by itself once the address is free")
		slog.Error("cannot start: another Cortex already holds this address", f...)

	default:
		f = append(f, holderFields(h)...)
		f = append(f, "fix", "stop that process, or give "+le.name+" another address in the config")
		slog.Error("cannot start: another process already holds this address", f...)
	}
	os.Exit(exitPortInUse)
}

// holderFields names the holder for the log. The parent is worth a field when it is some
// other process: a Cortex runs under a supervisor that starts another child the moment
// this one is killed, so the pid that has to be stopped is usually not the pid on the
// socket. Each field is omitted rather than sent empty — an unknown value must not render
// as a real one.
func holderFields(h holder) []any {
	f := []any{"holder_pid", h.pid}
	if h.exe != "" {
		f = append(f, "holder", h.exe)
	}
	if h.ppid > 1 && h.ppid != h.pid {
		f = append(f, "holder_parent_pid", h.ppid)
	}
	return f
}

// holderOf names the process listening on a configured address, from the kernel's own
// tables — no lsof, no ss, no root. peerproc already ranks candidates the way the kernel
// resolves a bind, including the IPv4-against-[::] case and its macOS IPV6_V6ONLY caveat,
// so there is nothing to mirror from install.sh's shell probes here.
func holderOf(addr string) (holder, error) {
	ap, err := probeAddr(addr)
	if err != nil {
		return holder{}, err
	}
	procs, err := peerproc.New()
	if err != nil {
		// Once more, as main does for process attribution: the self-test checks this
		// process's parent, which a supervisor can change under it at startup.
		procs, err = peerproc.New()
	}
	if err != nil {
		return holder{}, err
	}
	p, err := procs.ListenerOwner(ap)
	if err != nil {
		return holder{}, err
	}
	return holder{pid: p.PID, ppid: p.PPID, exe: p.Exe}, nil
}

// probeAddr turns a configured listen address into the one address to ask about.
//
// A wildcard bind has no single address, so it asks about loopback of the same family:
// peerproc ranks an exact bind ahead of a wildcard listener, so loopback is the most
// permissive probe there is — it finds a holder on 0.0.0.0, on [::], or on loopback
// itself, which between them is every way a wildcard bind gets refused.
//
// A hostname is not resolved. A listen address is a literal in every config this ships
// with, and a DNS lookup on the failure path would buy nothing: without a holder the
// message still names the socket, the address and how to find the process by hand.
func probeAddr(addr string) (netip.AddrPort, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("parsing %q: %w", addr, err)
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return netip.AddrPort{}, fmt.Errorf("%q names no fixed port", addr)
	}
	loopback4 := netip.AddrFrom4([4]byte{127, 0, 0, 1})
	ip := loopback4
	if host != "" {
		if ip, err = netip.ParseAddr(host); err != nil {
			return netip.AddrPort{}, fmt.Errorf("%q is not a literal address", host)
		}
		ip = ip.Unmap().WithZone("")
		if ip.IsUnspecified() {
			if ip.Is4() {
				ip = loopback4
			} else {
				ip = netip.IPv6Loopback()
			}
		}
	}
	return netip.AddrPortFrom(ip, uint16(n)), nil
}

// portOf is the port of a configured address for a command the message suggests, or a
// placeholder when the address is not one this can split. It is only ever interpolated
// into prose a human runs.
func portOf(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil && port != "" {
		return port
	}
	return "<port>"
}
