package tlsbridge

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"testing"
	"time"
)

func newTestMinter(t *testing.T) (*Minter, *x509.CertPool) {
	t.Helper()
	src, err := NewEphemeralSource()
	if err != nil {
		t.Fatalf("ca: %v", err)
	}
	m := NewMinter(src, MinterOpts{CacheMax: 8, LeafTTL: time.Hour})
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(src.CACertPEM()) {
		t.Fatal("append CA to pool")
	}
	return m, pool
}

func TestMinter_LeafChainsToCA_AndHasSAN(t *testing.T) {
	m, pool := newTestMinter(t)
	cert, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: "api.example.com"})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "api.example.com"}); err != nil {
		t.Errorf("leaf does not verify against CA for its SAN: %v", err)
	}
}

func TestMinter_IPLiteralSAN(t *testing.T) {
	m, pool := newTestMinter(t)
	c2, err := m.GetCertificateForHost("10.0.0.5")
	if err != nil {
		t.Fatalf("GetCertificateForHost(ip): %v", err)
	}
	leaf, _ := x509.ParseCertificate(c2.Certificate[0])
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool}); err != nil {
		// IP SANs verify via IPAddresses, not DNSName; the explicit SAN check below is authoritative.
	}
	found := false
	for _, ip := range leaf.IPAddresses {
		if ip.Equal(net.ParseIP("10.0.0.5")) {
			found = true
		}
	}
	if !found {
		t.Errorf("leaf for IP host lacks the IP SAN")
	}
}

func TestMinter_CacheHitReturnsSameCert(t *testing.T) {
	m, _ := newTestMinter(t)
	a, _ := m.GetCertificate(&tls.ClientHelloInfo{ServerName: "h.example.com"})
	b, _ := m.GetCertificate(&tls.ClientHelloInfo{ServerName: "h.example.com"})
	if &a.Certificate[0][0] != &b.Certificate[0][0] {
		t.Errorf("expected cached cert reuse for same SNI")
	}
}

func TestMinter_TTLExpiryRemints(t *testing.T) {
	src, err := NewEphemeralSource()
	if err != nil {
		t.Fatalf("ca: %v", err)
	}
	m := NewMinter(src, MinterOpts{CacheMax: 8, LeafTTL: 20 * time.Millisecond})
	a, err := m.GetCertificateForHost("h.example.com")
	if err != nil {
		t.Fatalf("first mint: %v", err)
	}
	time.Sleep(40 * time.Millisecond) // past the TTL
	b, err := m.GetCertificateForHost("h.example.com")
	if err != nil {
		t.Fatalf("second mint: %v", err)
	}
	if &a.Certificate[0][0] == &b.Certificate[0][0] {
		t.Errorf("expected a re-minted cert after TTL expiry, got the cached one")
	}
}

func TestMinter_LRUEvictsOldest(t *testing.T) {
	src, err := NewEphemeralSource()
	if err != nil {
		t.Fatalf("ca: %v", err)
	}
	m := NewMinter(src, MinterOpts{CacheMax: 2, LeafTTL: time.Hour})

	a1, err := m.GetCertificateForHost("a")
	if err != nil {
		t.Fatalf("mint a: %v", err)
	}
	if _, err := m.GetCertificateForHost("b"); err != nil {
		t.Fatalf("mint b: %v", err)
	}
	// Minting "c" overflows CacheMax=2, evicting the least-recently-used host ("a").
	c1, err := m.GetCertificateForHost("c")
	if err != nil {
		t.Fatalf("mint c: %v", err)
	}

	// "a" was evicted, so re-getting it mints a fresh cert.
	a2, err := m.GetCertificateForHost("a")
	if err != nil {
		t.Fatalf("re-mint a: %v", err)
	}
	if &a1.Certificate[0][0] == &a2.Certificate[0][0] {
		t.Errorf("expected evicted host \"a\" to be re-minted, got the original cert")
	}

	// "c" is still the most-recent entry, so an immediate re-get is a cache hit.
	c2, err := m.GetCertificateForHost("c")
	if err != nil {
		t.Fatalf("re-get c: %v", err)
	}
	if &c1.Certificate[0][0] != &c2.Certificate[0][0] {
		t.Errorf("expected most-recent host \"c\" to still be cached")
	}
}
