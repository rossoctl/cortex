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
		{"[fc00::9]:443", "fc00::9"}, // unique-local
		{"0.0.0.0:80", "0.0.0.0"},    // unspecified reaches loopback
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
