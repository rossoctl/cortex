package egressguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"testing"
)

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("ParseAddr(%q): %v", s, err)
	}
	return a
}

// stubResolver answers each host from a scripted table and counts lookups, so
// the resolve-once property is asserted rather than assumed.
type stubResolver struct {
	answers map[string][]netip.Addr
	lookups int
}

func (s *stubResolver) resolve(_ context.Context, host string) ([]netip.Addr, error) {
	s.lookups++
	if a, ok := s.answers[host]; ok {
		return a, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func newStubGuard(t *testing.T, permit []string, answers map[string][]netip.Addr) *Guard {
	t.Helper()
	g, err := New(permit)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	g.resolve = (&stubResolver{answers: answers}).resolve
	return g
}

// TestCheckAddr_RefusedRanges pins the refusal set: loopback, link-local
// (metadata), RFC 1918, unique-local, unspecified and multicast are all
// refused with no permit configured. A public address passes.
func TestCheckAddr_RefusedRanges(t *testing.T) {
	g, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, c := range []struct {
		addr  string
		refIP string
	}{
		{"127.0.0.1:443", "127.0.0.1"},
		{"[::1]:443", "::1"},
		{"169.254.169.254:80", "169.254.169.254"}, // cloud metadata
		{"[fe80::1]:443", "fe80::1"},
		{"10.0.0.9:8080", "10.0.0.9"},
		{"172.16.5.4:80", "172.16.5.4"},
		{"192.168.1.10:80", "192.168.1.10"},
		{"[fc00::9]:443", "fc00::9"},              // unique-local
		{"100.100.100.200:80", "100.100.100.200"}, // Alibaba metadata, in RFC 6598 CGNAT
		{"0.0.0.0:80", "0.0.0.0"},                 // unspecified reaches loopback
		{"224.0.0.1:80", "224.0.0.1"},
	} {
		_, err := g.CheckAddr(context.Background(), "tcp", c.addr)
		var ref *Refusal
		if !errors.As(err, &ref) {
			t.Errorf("CheckAddr(%q) err = %v, want a Refusal", c.addr, err)
			continue
		}
		if ref.Addr != mustAddr(t, c.refIP) {
			t.Errorf("CheckAddr(%q) refused addr = %s, want %s", c.addr, ref.Addr, c.refIP)
		}
	}

	// A public literal passes and is its own dial target.
	checked, err := g.CheckAddr(context.Background(), "tcp", "203.0.113.7:443")
	if err != nil {
		t.Fatalf("public literal: %v", err)
	}
	if checked != "203.0.113.7:443" {
		t.Errorf("public literal dial addr = %q, want it verbatim", checked)
	}
}

// TestCheckAddr_NeverPermitted pins the ranges no permit can open: the moca
// conformance case metadata_address_403_even_if_permitted permits
// 169.254.0.0/16 and 0.0.0.0/0 and the metadata address is still refused.
func TestCheckAddr_NeverPermitted(t *testing.T) {
	g, err := New([]string{"169.254.0.0/16", "0.0.0.0/0", "::/0"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, addr := range []string{
		"169.254.169.254:80",
		"169.254.0.9:80",
		"[fe80::9]:443",
		"[fd00:ec2::254]:80", // AWS metadata, IPv6 form — ULA, so a fd00::/8 permit must not open it
		"0.0.0.0:80",
		"[ff02::1]:443",
	} {
		if _, err := g.CheckAddr(context.Background(), "tcp", addr); err == nil {
			t.Errorf("CheckAddr(%q) with blanket permits = nil, want a Refusal", addr)
		}
	}
}

// TestCheckAddr_PermitOpensPrivateSpace pins the permit-able half: loopback
// and RFC 1918 become dialable when a configured prefix covers them, which is
// the moca dns_rebinding case's EnvPermit (127.0.0.1/32) and the in-cluster /
// laptop-local-model deployments.
func TestCheckAddr_PermitOpensPrivateSpace(t *testing.T) {
	g, err := New([]string{"127.0.0.1/32", "10.0.0.0/8"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, addr := range []string{"127.0.0.1:8080", "10.1.2.3:443"} {
		if _, err := g.CheckAddr(context.Background(), "tcp", addr); err != nil {
			t.Errorf("CheckAddr(%q) with covering permit: %v", addr, err)
		}
	}
	// Uncovered private space stays refused: the permit is a prefix, not a toggle.
	if _, err := g.CheckAddr(context.Background(), "tcp", "192.168.1.4:80"); err == nil {
		t.Error("CheckAddr(192.168.1.4) outside the permits = nil, want a Refusal")
	}
}

// TestCheckAddr_ResolvesOnceAndDialsFirstChecked is the DNS-rebinding case:
// the resolver is consulted exactly once, a mixed public/private answer is
// refused outright (fail closed), and a clean answer dials the FIRST address.
func TestCheckAddr_ResolvesOnceAndDialsFirstChecked(t *testing.T) {
	g, err := New([]string{"127.0.0.1/32"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stub := &stubResolver{answers: map[string][]netip.Addr{
		"rebind.test": {mustAddr(t, "127.0.0.1"), mustAddr(t, "10.0.0.9")},
	}}
	g.resolve = stub.resolve

	checked, err := g.CheckAddr(context.Background(), "tcp", "rebind.test:9443")
	if err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if checked != "127.0.0.1:9443" {
		t.Errorf("dial addr = %q, want the first checked answer 127.0.0.1:9443", checked)
	}
	if stub.lookups != 1 {
		t.Errorf("resolver ran %d times, want exactly 1", stub.lookups)
	}
}

// TestCheckAddr_FirstAnswerRules: the FIRST record decides. When it is a
// refused address the name is refused even though later records are public —
// the reverse of the conformance case, and the shape an attacker controls by
// record order.
func TestCheckAddr_FirstAnswerRules(t *testing.T) {
	g, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	g.resolve = (&stubResolver{answers: map[string][]netip.Addr{
		"mixed.test": {mustAddr(t, "127.0.0.1"), mustAddr(t, "203.0.113.7")},
	}}).resolve
	if _, err := g.CheckAddr(context.Background(), "tcp", "mixed.test:443"); err == nil {
		t.Error("first answer refused, second public = nil, want a Refusal")
	}
}

// TestCheckAddr_MappedIPv6: a v4-mapped v6 literal is checked as its v4
// half, so ::ffff:127.0.0.1 cannot slip past the loopback rule.
func TestCheckAddr_MappedIPv6(t *testing.T) {
	g, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := g.CheckAddr(context.Background(), "tcp", "[::ffff:127.0.0.1]:443"); err == nil {
		t.Error("v4-mapped loopback = nil, want a Refusal")
	}
}

// TestCheckAddr_NAT64CheckedByEmbeddedV4: a well-known-prefix address is
// judged by the IPv4 it embeds — the loopback one is refused, a public one
// passes — and the v6 form is what comes back as the dial target, since on a
// NAT64 host the bare v4 may not route.
func TestCheckAddr_NAT64CheckedByEmbeddedV4(t *testing.T) {
	g, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := g.CheckAddr(context.Background(), "tcp", "[64:ff9b::7f00:1]:443"); err == nil {
		t.Error("NAT64-wrapped 127.0.0.1 = nil, want a Refusal")
	}
	if _, err := g.CheckAddr(context.Background(), "tcp", "[64:ff9b::a9fe:a9fe]:80"); err == nil {
		t.Error("NAT64-wrapped 169.254.169.254 = nil, want a Refusal")
	}
	checked, err := g.CheckAddr(context.Background(), "tcp", "[64:ff9b::102:304]:443")
	if err != nil {
		t.Fatalf("NAT64-wrapped 1.2.3.4: %v", err)
	}
	if checked != "[64:ff9b::102:304]:443" {
		t.Errorf("NAT64 dial addr = %q, want the v6 form back", checked)
	}
}

// TestCheckAddr_CGNATPermittable: RFC 6598 space is refused by default but,
// like RFC 1918, a covering permit opens it — an operator whose carrier
// network legitimately sits there should not need 0.0.0.0/0 to say so.
func TestCheckAddr_CGNATPermittable(t *testing.T) {
	g, err := New([]string{"100.64.0.0/10"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := g.CheckAddr(context.Background(), "tcp", "100.100.100.200:80"); err != nil {
		t.Errorf("CGNAT with a covering permit: %v", err)
	}
}

// TestCheckAddr_ServiceNamePort: a CONNECT target may name its port as a
// service ("example.com:https"), which net.DialTimeout resolved itself before
// the guard existed — LookupPort keeps that working — and an unparseable port
// is rejected BEFORE the resolve, so it costs no DNS lookup.
func TestCheckAddr_ServiceNamePort(t *testing.T) {
	g, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stub := &stubResolver{answers: map[string][]netip.Addr{
		"dst.test": {mustAddr(t, "203.0.113.7")},
	}}
	g.resolve = stub.resolve

	checked, err := g.CheckAddr(context.Background(), "tcp", "dst.test:https")
	if err != nil {
		t.Fatalf("service-name port: %v", err)
	}
	if checked != "203.0.113.7:443" {
		t.Errorf("dial addr = %q, want 203.0.113.7:443", checked)
	}

	stub.lookups = 0
	if _, err := g.CheckAddr(context.Background(), "tcp", "dst.test:notaport"); err == nil {
		t.Error("unparseable port = nil, want an error")
	}
	if stub.lookups != 0 {
		t.Errorf("resolver ran %d times on a bad port, want 0 — the port is checked first", stub.lookups)
	}
}

// ifaceAddr stands in for a net.InterfaceAddrs entry, whose String() is
// "ip/mask".
type ifaceAddr string

func (a ifaceAddr) Network() string { return "ip+net" }
func (a ifaceAddr) String() string  { return string(a) }

// TestHostOwns pins the #1539 review fix: the host's own interface ADDRESS is
// refused, its on-link SUBNET is not. net.InterfaceAddrs hands back ip/mask
// pairs, and testing the prefix instead of the address would refuse every LAN
// neighbour — and, on a cloud VM with a public address on a large on-link
// prefix, ordinary public upstreams.
func TestHostOwns(t *testing.T) {
	addrs := []net.Addr{ifaceAddr("192.168.1.167/24"), ifaceAddr("9.67.123.175/18")}
	for _, own := range []string{"192.168.1.167", "9.67.123.175"} {
		if !hostOwns(mustAddr(t, own), addrs) {
			t.Errorf("hostOwns(%s) = false, want true — the interface's own address", own)
		}
	}
	for _, neighbour := range []string{"192.168.1.1", "9.67.64.1", "9.67.127.254"} {
		if hostOwns(mustAddr(t, neighbour), addrs) {
			t.Errorf("hostOwns(%s) = true, want false — a subnet neighbour, not this host", neighbour)
		}
	}
}

// A ZONED IPv6 LITERAL must not walk past the host-owned-address rule.
// netip.Addr equality includes the zone, and net.InterfaceAddrs reports the
// host's addresses unzoned, so `[2001:db8::5%eth0]` compared unequal to the
// `2001:db8::5` the host actually owns — writing a zone the host itself would
// ignore was enough to reach its own address. Confirmed against the code before
// the fix: hostOwns(bare) true, hostOwns(zoned) false (#1539 review).
//
// hostOwns is the whole exposure, which is why this test is here and not only in
// the CheckAddr table: loopback, link-local, private and CGNAT are all RANGE
// predicates, and a zone does not move an address out of a range. Equality is
// the one place a zone bites.
// Driven through CheckAddr, not hostOwns directly: a test that strips the zone
// itself before calling hostOwns asserts the fix rather than the path, and
// passes against the unfixed code. This one fails before the fix (verified) and
// passes after, because CheckAddr is where the stripping has to happen.
func TestCheckAddr_ZonedV6DoesNotEscapeHostOwned(t *testing.T) {
	// A host whose own address is a GLOBAL IPv6 one — deliberately not loopback,
	// link-local or ULA, so hostOwns is the only rule that can refuse it and the
	// test cannot pass for an unrelated reason.
	g, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	g.ifaceAddrs = func() ([]net.Addr, error) {
		return []net.Addr{ifaceAddr("2001:db8::5/64")}, nil
	}

	// The premise: unzoned, the host's own address is refused.
	if _, cerr := g.CheckAddr(context.Background(), "tcp", "[2001:db8::5]:443"); cerr == nil {
		t.Fatal("CheckAddr([2001:db8::5]:443) = nil, want a refusal — the premise of this test")
	}
	// The hole: the same address with a zone the host itself would ignore.
	got, cerr := g.CheckAddr(context.Background(), "tcp", "[2001:db8::5%eth0]:443")
	if cerr == nil {
		t.Errorf("CheckAddr([2001:db8::5%%eth0]:443) = %q with no refusal; a written zone reached the host's own address", got)
	}
}

// The zone must survive into the DIALED address even though the check ignores
// it: on a link-scoped destination Go needs the zone to pick an interface, so
// stripping it for the dial would break a legitimate request rather than
// refusing an illegitimate one. A permitted zoned address is the only way to
// observe this, since every refused one returns "".
func TestCheckAddr_ZoneKeptOnTheDialedAddress(t *testing.T) {
	// fe80::/10 is never-permitted, so use a ULA with a covering permit: ULA is
	// refused-but-permit-able, which is the state that lets a zoned address
	// through to the return value.
	g, err := New([]string{"fd00::/8"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	g.ifaceAddrs = func() ([]net.Addr, error) { return nil, nil }

	got, cerr := g.CheckAddr(context.Background(), "tcp", "[fd00::5%eth0]:443")
	if cerr != nil {
		t.Fatalf("CheckAddr on a permitted ULA = %v, want it allowed", cerr)
	}
	if want := "[fd00::5%eth0]:443"; got != want {
		t.Errorf("dialed address = %q, want %q — the zone is dropped for the check, not for the dial", got, want)
	}
}

// The same hole through the full CheckAddr path, for the ranges a zone never
// moved: a zoned loopback and link-local stay refused. Pins that stripping the
// zone for the check did not loosen anything that was already closed.
//
// The two NAT64 rows are the ones a zone DID move, and they are here because
// the stripping and the NAT64 decode are one fix rather than two:
// nat64Prefix.Contains is false for a zoned address, so before the zone was
// stripped the embedded-v4 check never ran on one. Measured against the code
// with only the decode's input reverted to the zoned address:
// [64:ff9b::7f00:1%eth0] and [64:ff9b::a9fe:a9fe%eth0] were both ALLOWED —
// loopback and the cloud metadata service, the latter never-permitted — while
// their unzoned forms were refused. Every other test in egressguard and
// forwardproxy stayed green, so without these two rows the decode's input is
// unpinned and a later edit could silently reopen it (#1539 review).
func TestCheckAddr_ZonedV6StillRefused(t *testing.T) {
	g, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, addr := range []string{
		"[::1%lo0]:443",
		"[fe80::1%eth0]:443",
		// NAT64 well-known prefix, zoned: judged by the IPv4 each embeds.
		"[64:ff9b::7f00:1%eth0]:80",    // 127.0.0.1 — loopback
		"[64:ff9b::a9fe:a9fe%eth0]:80", // 169.254.169.254 — metadata, never-permitted
	} {
		if _, cerr := g.CheckAddr(context.Background(), "tcp", addr); cerr == nil {
			t.Errorf("CheckAddr(%s) = nil, want a refusal", addr)
		}
	}
}

// TestRefusalErrorsAs: the typed error must be reachable through the wrapping
// net/http puts around dial errors (*url.Error), which is how serveOutbound
// detects it.
func TestRefusalErrorsAs(t *testing.T) {
	g, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, cerr := g.CheckAddr(context.Background(), "tcp", "127.0.0.1:443")
	var ref *Refusal
	if !errors.As(cerr, &ref) {
		t.Fatalf("errors.As on the raw error failed: %v", cerr)
	}
	wrapped := fmt.Errorf(`Get "http://127.0.0.1:443/x": %w`, cerr)
	if !errors.As(wrapped, &ref) {
		t.Fatalf("errors.As through a wrapping error failed: %v", wrapped)
	}
	if ref.Host != "127.0.0.1" || ref.Addr != mustAddr(t, "127.0.0.1") {
		t.Errorf("Refusal = %s (%s), want host and addr from the check", ref.Host, ref.Addr)
	}
}

// TestNew_RejectsBadPermit: a malformed permit CIDR is a construction error,
// not a silently ignored entry.
func TestNew_RejectsBadPermit(t *testing.T) {
	if _, err := New([]string{"127.0.0.1/99"}); err == nil {
		t.Error("New with an invalid mask = nil, want an error")
	}
	if _, err := New([]string{"not-a-cidr"}); err == nil {
		t.Error("New with a non-CIDR = nil, want an error")
	}
}

// TestDialContext_DialsCheckedAddress: the dialer-shaped entry point dials
// the address CheckAddr returned. A loopback listener stands in for the
// destination, permitted so the guard lets the dial through — what is pinned
// is that the connection lands on the stub resolver's FIRST answer, not on
// whatever a second resolution might have said.
func TestDialContext_DialsCheckedAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback listen unavailable: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	g, err := New([]string{"127.0.0.1/32"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	first := netip.MustParseAddr("127.0.0.1")
	g.resolve = (&stubResolver{answers: map[string][]netip.Addr{
		"dst.test": {first, netip.MustParseAddr("127.0.0.2")},
	}}).resolve

	conn, derr := g.DialContext(context.Background(), "tcp", fmt.Sprintf("dst.test:%d", port))
	if derr != nil {
		t.Fatalf("DialContext: %v", derr)
	}
	defer conn.Close()
	if got := conn.RemoteAddr().String(); got != fmt.Sprintf("127.0.0.1:%d", port) {
		t.Errorf("dialed %s, want the first checked address 127.0.0.1:%d", got, port)
	}
}
