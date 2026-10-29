// Package egressguard implements the forward proxy's SSRF dial guard
// (#1484, MOCA MI1 §8.5): resolve a destination once, refuse addresses a
// sandbox must never reach through the proxy, and dial the address that was
// checked — never a second resolution's answer.
//
// The refused set is loopback, link-local (including the cloud metadata
// address 169.254.169.254), RFC 1918 / unique-local space, and the host's own
// interface addresses. An operator may permit prefixes within that space
// (a laptop proxy whose upstream is a local model, an in-cluster deployment)
// — but link-local, unspecified and multicast addresses are never permitted:
// 0.0.0.0 reaches loopback on every mainstream stack and 169.254.169.254 is
// the metadata service the guard exists among other things to protect.
package egressguard

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// Guard checks and dials outbound destinations. Construct with New; the zero
// value refuses nothing and dials nothing. Safe for concurrent use.
type Guard struct {
	// permit holds the operator-configured prefixes within the refused space.
	permit []netip.Prefix

	// resolve resolves a host name to addresses, once per call. A field so
	// tests can pin "resolved exactly once" against a counting fake; the
	// production value is net.DefaultResolver.LookupNetIP.
	resolve func(ctx context.Context, host string) ([]netip.Addr, error)
}

// New builds a Guard that refuses the destinations described in the package
// comment, except where a permit prefix covers them. Permit CIDRs are
// accepted even when they cover never-permitted space (an operator copying a
// broad list is not a boot failure); the never-permitted ranges simply win.
func New(permitCIDRs []string) (*Guard, error) {
	permit := make([]netip.Prefix, 0, len(permitCIDRs))
	for _, cidr := range permitCIDRs {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(cidr))
		if err != nil {
			return nil, fmt.Errorf("egressguard: permit cidr %q: %w", cidr, err)
		}
		permit = append(permit, prefix.Masked())
	}
	return &Guard{
		permit: permit,
		resolve: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
	}, nil
}

// Refusal is the typed error a refused destination produces. Host is the
// destination as the request named it (name or literal); Addr is the refused
// address the check saw. Callers render it as 403 destination_private — the
// MOCA egress contract's identifier, used verbatim so the conformance suite
// Cortex shares with moca-egress matches it without a translation table.
type Refusal struct {
	Host string
	Addr netip.Addr
}

func (r *Refusal) Error() string {
	return fmt.Sprintf("refusing to reach private destination %s (%s)", r.Host, r.Addr)
}

// CheckAddr resolves network+addr ONCE, checks the address it will dial, and
// reports that address: the literal when addr named one, else the FIRST
// resolved answer. The caller must dial the returned address, not the
// original host.
//
// The rebinding window this closes is between the CHECK and the DIAL — check
// one address, dial another — and the fix is to commit to one answer and dial
// it verbatim, never re-resolving. Later records in the same response are
// deliberately not consulted: the moca conformance case pins the shape, a
// name answering 127.0.0.1 then 10.0.0.9 with only 127.0.0.1/32 permitted
// must tunnel to 127.0.0.1, the first checked answer. What can never happen
// is the divergence the attack needs: the checked address and the dialed one
// are the same value by construction, and a second lookup has no window to
// change either.
func (g *Guard) CheckAddr(ctx context.Context, network, addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}

	var dial []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		dial = []netip.Addr{ip.Unmap()}
	} else {
		if g.resolve == nil {
			return "", fmt.Errorf("egressguard: no resolver configured")
		}
		addrs, rerr := g.resolve(ctx, host)
		if rerr != nil {
			return "", rerr
		}
		if len(addrs) == 0 {
			return "", &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		dial = addrs
	}

	// Check the first answer — the one the caller will dial. See CheckAddr's
	// doc for why the later records are not consulted.
	dialAddr := dial[0].Unmap()
	if ref := g.check(host, dialAddr); ref != nil {
		return "", ref
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return "", err
	}
	return netip.AddrPortFrom(dialAddr, uint16(p)).String(), nil
}

// DialContext is CheckAddr followed by a dial of the checked address —
// dialer-shaped, for http.Transport. Only the dial address is replaced; the
// transport's TLS ServerName and certificate verification still follow the
// hostname the request named.
func (g *Guard) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	checked, err := g.CheckAddr(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	return d.DialContext(ctx, network, checked)
}

// DialTimeout is DialContext with a fixed timeout, for the CONNECT-tunnel
// dial sites that predate contexts.
func (g *Guard) DialTimeout(ctx context.Context, network, addr string, timeout time.Duration) (net.Conn, error) {
	checked, err := g.CheckAddr(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	return net.DialTimeout(network, checked, timeout)
}

// check applies the refusal rules to one address. It returns the Refusal to
// answer the request with, or nil when the address may be dialed.
func (g *Guard) check(host string, ip netip.Addr) *Refusal {
	if ref := g.neverPermitted(host, ip); ref != nil {
		return ref
	}
	if !g.refusable(ip) {
		return nil
	}
	// Refused space, permit-able: an operator prefix covering the address
	// makes it dialable (the laptop local-model and in-cluster cases).
	for _, p := range g.permit {
		if p.Contains(ip) {
			return nil
		}
	}
	return &Refusal{Host: host, Addr: ip}
}

// neverPermitted reports the ranges no permit can open, as a Refusal when
// the address is in one. Link-local unicast carries the cloud metadata
// service (169.254.169.254 on every major cloud); unspecified reaches
// loopback on mainstream stacks; multicast is not a unicast destination.
func (g *Guard) neverPermitted(host string, ip netip.Addr) *Refusal {
	switch {
	case ip.IsLinkLocalUnicast():
		return &Refusal{Host: host, Addr: ip}
	case ip.IsUnspecified():
		return &Refusal{Host: host, Addr: ip}
	case ip.IsMulticast():
		return &Refusal{Host: host, Addr: ip}
	}
	return nil
}

// refusable reports whether ip is in the refused-but-permit-able space:
// loopback, RFC 1918 / unique-local, or one of this host's own interface
// addresses. The last set is enumerated per check — an interface address
// added after boot is guarded without a restart, and InterfaceAddrs costs
// microseconds against a dial that costs milliseconds.
func (g *Guard) refusable(ip netip.Addr) bool {
	if ip.IsLoopback() || ip.IsPrivate() {
		return true
	}
	ifaces, err := net.InterfaceAddrs()
	if err != nil {
		// Fail closed: unable to enumerate the host's addresses, treat the
		// candidate as one of them.
		return true
	}
	for _, ia := range ifaces {
		prefix, err := netip.ParsePrefix(ia.String())
		if err != nil {
			continue
		}
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}
