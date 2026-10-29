// Package egressguard implements the forward proxy's SSRF dial guard
// (#1484, MOCA MI1 §8.5): resolve a destination once, refuse addresses a
// sandbox must never reach through the proxy, and dial the address that was
// checked — never a second resolution's answer.
//
// The refused set is loopback, link-local (including the cloud metadata
// address 169.254.169.254), RFC 1918 / unique-local space, RFC 6598 CGNAT
// shared space (100.64.0.0/10 — Alibaba's metadata service, 100.100.100.200,
// lives there), and the host's own interface addresses — the addresses
// themselves, NOT their on-link subnets: a neighbour in the host's /24 is
// not this host. An operator may permit prefixes within that space (a laptop
// proxy whose upstream is a local model, an in-cluster deployment) — but
// link-local, unspecified, multicast and the AWS IPv6 metadata address
// (fd00:ec2::254) are never permitted: 0.0.0.0 reaches loopback on every
// mainstream stack, 169.254.169.254 is the metadata service the guard exists
// among other things to protect, and fd00:ec2::254 is that service's IPv6
// form — ULA, so it would otherwise sit in the permit-able set.
//
// A well-known-prefix NAT64 address (64:ff9b::/96) is checked by the IPv4
// address it embeds, so [64:ff9b::7f00:1] is refused as the 127.0.0.1 it
// carries; the v6 form is still what gets dialed.
package egressguard

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// Guard checks and dials outbound destinations. Construct with New; the zero
// value checks literal addresses but cannot resolve names — check runs with an
// empty permit list, so a loopback, private or metadata literal is refused and
// a public one is allowed, while a name fails with "no resolver configured".
// Safe for concurrent use.
type Guard struct {
	// permit holds the operator-configured prefixes within the refused space.
	permit []netip.Prefix

	// resolve resolves a host name to addresses, once per call. A field so
	// tests can pin "resolved exactly once" against a counting fake; the
	// production value is net.DefaultResolver.LookupNetIP.
	resolve func(ctx context.Context, host string) ([]netip.Addr, error)

	// ifaceAddrs enumerates this host's interface addresses. A field for the
	// same reason as resolve: the host-owned rule is the one refusal that
	// depends on the machine the test runs on, so a test asserting it has to
	// supply the addresses rather than hope for them. Production is
	// net.InterfaceAddrs.
	ifaceAddrs func() ([]net.Addr, error)
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
		ifaceAddrs: net.InterfaceAddrs,
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
	// The port BEFORE the resolve: net.LookupPort accepts both numbers and
	// service names ("https" — what a CONNECT request may carry, and what
	// net.DialTimeout would have resolved itself before the guard existed),
	// and a bad port now costs no DNS lookup. It also range-checks, which is
	// what the strconv.ParseUint here used to do.
	p, err := net.LookupPort(network, port)
	if err != nil {
		return "", err
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
	// The IPv6 zone is dropped for the CHECK and kept for the dial. netip.Addr
	// equality includes the zone, so `[2001:db8::5%eth0]` compared unequal to the
	// `2001:db8::5` that net.InterfaceAddrs reports and walked straight past
	// hostOwns — the host's own address, reachable by writing a zone the host
	// itself would ignore. Loopback, link-local and private were never exposed
	// this way (those are range predicates, which a zone does not perturb), so
	// hostOwns was the whole hole, and equality is exactly where a zone bites.
	// A permit is unaffected either way: Prefix.Contains is false for any zoned
	// address, so stripping the zone can only make a permit apply, never remove
	// one (#1539 review).
	checkAddr := dialAddr.WithZone("")
	// A NAT64 well-known-prefix address is judged by the IPv4 it embeds —
	// [64:ff9b::7f00:1] is 127.0.0.1 wearing a v6 hat — but the v6 form is
	// what gets dialed and returned, since on a NAT64 host the v4 may not
	// route. The mapping is arithmetic, not a lookup, so checking one form
	// and dialing the other opens no check-vs-dial window.
	if embedded, ok := nat64Embedded(checkAddr); ok {
		checkAddr = embedded
	}
	if ref := g.check(host, checkAddr); ref != nil {
		return "", ref
	}
	return netip.AddrPortFrom(dialAddr, uint16(p)).String(), nil
}

// nat64Prefix is the NAT64 well-known prefix (RFC 6052): a v6 address in it
// carries the IPv4 destination in its low 32 bits.
var nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")

// nat64Embedded returns the IPv4 address a well-known-prefix NAT64 address
// embeds, and whether ip was one at all.
func nat64Embedded(ip netip.Addr) (netip.Addr, bool) {
	if ip.Is4() || ip.Is4In6() || !nat64Prefix.Contains(ip) {
		return netip.Addr{}, false
	}
	b := ip.As16()
	return netip.AddrFrom4([4]byte(b[12:16])), true
}

// cgnat is RFC 6598 shared address space: not RFC 1918, not globally routed,
// and home to Alibaba's metadata service (100.100.100.200). Refused like
// private space, permit-able like it too.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// awsMetadataV6 is the AWS instance metadata service's IPv6 form. It is ULA,
// so without this entry it would sit in the permit-able set — and an operator
// permitting fd00::/8 (plausible for a ULA-internal upstream) would open it.
var awsMetadataV6 = netip.MustParseAddr("fd00:ec2::254")

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
// service (169.254.169.254 on every major cloud, fd00:ec2::254 on AWS's
// IPv6 path); unspecified reaches loopback on mainstream stacks; multicast
// is not a unicast destination.
func (g *Guard) neverPermitted(host string, ip netip.Addr) *Refusal {
	switch {
	case ip == awsMetadataV6:
		return &Refusal{Host: host, Addr: ip}
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
// loopback, RFC 1918 / unique-local, RFC 6598 CGNAT, or one of this host's
// own interface addresses. The last set is enumerated per check — an
// interface address added after boot is guarded without a restart, and
// InterfaceAddrs costs microseconds against a dial that costs milliseconds.
func (g *Guard) refusable(ip netip.Addr) bool {
	if ip.IsLoopback() || ip.IsPrivate() || cgnat.Contains(ip) {
		return true
	}
	enumerate := g.ifaceAddrs
	if enumerate == nil {
		// A zero-value Guard still asks the host, so the host-owned rule holds
		// for one as it does for a constructed Guard.
		enumerate = net.InterfaceAddrs
	}
	ifaces, err := enumerate()
	if err != nil {
		// Fail closed: unable to enumerate the host's addresses, treat the
		// candidate as one of them.
		return true
	}
	return hostOwns(ip, ifaces)
}

// hostOwns reports whether ip is one of the host's own interface addresses.
// The address ITSELF, not the interface's on-link subnet: net.InterfaceAddrs
// hands back ip/mask pairs, and testing the prefix would refuse every
// neighbour on the LAN — and on a cloud VM with a public address on a large
// on-link prefix, ordinary public upstreams (#1539 review).
func hostOwns(ip netip.Addr, ifaceAddrs []net.Addr) bool {
	for _, ia := range ifaceAddrs {
		prefix, err := netip.ParsePrefix(ia.String())
		if err != nil {
			continue
		}
		if prefix.Addr().Unmap() == ip {
			return true
		}
	}
	return false
}
