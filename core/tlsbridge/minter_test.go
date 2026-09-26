package tlsbridge

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"strings"
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
	m, _ := newTestMinter(t)
	c2, err := m.GetCertificateForHost("10.0.0.5")
	if err != nil {
		t.Fatalf("GetCertificateForHost(ip): %v", err)
	}
	leaf, _ := x509.ParseCertificate(c2.Certificate[0])
	// IP SANs verify via IPAddresses, not DNSName; the explicit SAN check below is authoritative.
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

// TestMinter_ReMintsWallClockExpiredLeaf exercises the NotAfter backstop: if a
// cached leaf is ever wall-clock-expired while the cache deadline still reads
// fresh, Get must gate on the leaf's real NotAfter and re-mint rather than
// serve a cert the client rejects. (The primary suspend fix — the wall-clock
// cache deadline — is guarded by TestMinter_CacheDeadlineIsWallClock.)
func TestMinter_ReMintsWallClockExpiredLeaf(t *testing.T) {
	m, _ := newTestMinter(t) // LeafTTL=time.Hour, so the cache deadline stays "fresh"
	c1, err := m.GetCertificateForHost("h.example.com")
	if err != nil {
		t.Fatalf("first mint: %v", err)
	}
	// mint() must populate Leaf so the cache can gate on real validity.
	m.mu.Lock()
	e := m.items["h.example.com"].Value.(*cacheEntry)
	if e.cert.Leaf == nil {
		m.mu.Unlock()
		t.Fatal("minted cert has no Leaf populated")
	}
	// Simulate the leaf having aged past its NotAfter while the monotonic cache
	// deadline did not advance (host was suspended).
	e.cert.Leaf.NotAfter = time.Now().Add(-time.Minute)
	m.mu.Unlock()

	c2, err := m.GetCertificateForHost("h.example.com")
	if err != nil {
		t.Fatalf("second mint: %v", err)
	}
	if &c1.Certificate[0][0] == &c2.Certificate[0][0] {
		t.Fatal("served a wall-clock-expired cached leaf; expected a re-mint")
	}
	if c2.Leaf == nil || !time.Now().Before(c2.Leaf.NotAfter) {
		t.Fatal("re-minted leaf is not valid")
	}
}

// TestMinter_CacheDeadlineIsWallClock guards the actual suspend fix: the cache
// freshness deadline must carry NO monotonic clock reading, so the freshness
// check falls back to the wall clock and expires correctly across a host
// suspend (where the monotonic clock freezes but wall time advances). A
// monotonic-carrying deadline is exactly what made the cache keep serving a
// wall-clock-expired leaf. time.Time's == compares the monotonic reading too,
// so a deadline that still carried one would not equal its .Round(0) form.
func TestMinter_CacheDeadlineIsWallClock(t *testing.T) {
	m, _ := newTestMinter(t)
	if _, err := m.GetCertificateForHost("h.example.com"); err != nil {
		t.Fatalf("mint: %v", err)
	}
	m.mu.Lock()
	exp := m.items["h.example.com"].Value.(*cacheEntry).expires
	m.mu.Unlock()
	if exp != exp.Round(0) {
		t.Errorf("cache deadline carries a monotonic clock reading; must be wall-clock (.Round(0)) to survive suspend")
	}
}

// TestMint_WarnsWhenIssuerIsExpiring covers the gap between renewal being evaluated
// at STARTUP and a proxy that simply stays up. EnsureFileSource cannot help a process
// that was already running when the window opened — under launchd or systemd that is
// the normal case — so it would keep signing with an aging CA, and once past NotAfter
// every client rejects the chain. The minter is the one path that must run for any of
// this to matter, and it already holds the issuer.
func TestMint_WarnsWhenIssuerIsExpiring(t *testing.T) {
	cases := []struct {
		name     string
		notAfter time.Time
		wantWarn bool
	}{
		{"healthy CA", time.Now().Add(200 * 24 * time.Hour), false},
		{"inside the renewal window", time.Now().Add(caRenewBefore / 2), true},
		{"already expired", time.Now().Add(-time.Hour), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logbuf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			m := NewMinter(&fixedExpiryCASource{t: t, notAfter: tc.notAfter}, MinterOpts{})
			if _, err := m.GetCertificateForHost("example.com"); err != nil {
				t.Fatalf("GetCertificateForHost: %v", err)
			}

			got := logbuf.String()
			if warned := strings.Contains(got, "signing CA is expiring"); warned != tc.wantWarn {
				t.Errorf("warned=%v, want %v; log:\n%s", warned, tc.wantWarn, got)
			}
			if tc.wantWarn && !strings.Contains(got, "ca_not_after=") {
				t.Errorf("warning did not name the deadline, which is what makes it actionable:\n%s", got)
			}
		})
	}
}

// TestMint_IssuerWarningIsOncePerProcess: mint runs per cache miss, so an unthrottled
// warning would print on every handshake to a new host and bury the log it belongs in.
func TestMint_IssuerWarningIsOncePerProcess(t *testing.T) {
	var logbuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	m := NewMinter(&fixedExpiryCASource{t: t, notAfter: time.Now().Add(-time.Hour)}, MinterOpts{})
	for _, h := range []string{"a.example.com", "b.example.com", "c.example.com"} {
		if _, err := m.GetCertificateForHost(h); err != nil {
			t.Fatalf("GetCertificateForHost(%s): %v", h, err)
		}
	}
	if n := strings.Count(logbuf.String(), "signing CA is expiring"); n != 1 {
		t.Errorf("warning printed %d times across 3 mints, want exactly 1", n)
	}
}

// fixedExpiryCASource is a CASource whose CA carries a caller-chosen NotAfter, so a
// test can present an aging issuer without waiting a year.
type fixedExpiryCASource struct {
	t        *testing.T
	notAfter time.Time
	cert     *x509.Certificate
	key      crypto.Signer
}

func (f *fixedExpiryCASource) Issuer() (*x509.Certificate, crypto.Signer) {
	if f.cert == nil {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			f.t.Fatalf("generate key: %v", err)
		}
		tmpl := &x509.Certificate{
			SerialNumber:          big.NewInt(2),
			Subject:               pkix.Name{CommonName: "authbridge-tls-bridge-ca"},
			NotBefore:             time.Now().Add(-365 * 24 * time.Hour),
			NotAfter:              f.notAfter,
			IsCA:                  true,
			KeyUsage:              x509.KeyUsageCertSign,
			BasicConstraintsValid: true,
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
		if err != nil {
			f.t.Fatalf("create CA: %v", err)
		}
		crt, err := x509.ParseCertificate(der)
		if err != nil {
			f.t.Fatalf("parse CA: %v", err)
		}
		f.cert, f.key = crt, key
	}
	return f.cert, f.key
}

func (f *fixedExpiryCASource) CACertPEM() []byte {
	crt, _ := f.Issuer()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: crt.Raw})
}
