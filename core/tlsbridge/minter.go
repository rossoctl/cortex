package tlsbridge

import (
	"container/list"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"sync"
	"time"
)

type MinterOpts struct {
	CacheMax int           // max cached leaves (LRU); <=0 → 1024
	LeafTTL  time.Duration // leaf validity AND cache TTL; <=0 → 24h
}

// Minter mints per-host leaf certs signed by a CASource, cached LRU+TTL by host.
type Minter struct {
	src     CASource
	max     int
	ttl     time.Duration
	leafKey *ecdsa.PrivateKey // one key reused across leaves (cheaper; key is not the secret here)

	mu    sync.Mutex
	ll    *list.List               // MRU front
	items map[string]*list.Element // host -> element(*cacheEntry)

	// issuerWarnOnce keeps the CA-expiry warning to one line per process. mint is
	// on the request path, so an unthrottled warning would print per handshake.
	issuerWarnOnce sync.Once
}

// warnIfIssuerExpiring says so out loud when the signing CA is inside its renewal
// window or already past it.
//
// EnsureFileSource only evaluates renewal at STARTUP, so a long-lived proxy — the
// normal case under launchd or systemd — can run straight through the window and out
// the far side, signing leaves from an expired CA that every client rejects. That is
// precisely the state this whole change set exists to prevent, reached by simply
// staying up. Checking here costs nothing: mint already holds the issuer, and this is
// the one code path that must run for any of it to matter.
//
// A warning rather than a re-mint: replacing the CA under a running proxy would
// invalidate every client's trust anchor mid-session, with no restart to explain it.
// Restarting is the operator's call, so name the deadline and let them pick when.
func (m *Minter) warnIfIssuerExpiring(ca *x509.Certificate) {
	if ca == nil || time.Now().Add(caRenewBefore).Before(ca.NotAfter) {
		return
	}
	m.issuerWarnOnce.Do(func() {
		expired := time.Now().After(ca.NotAfter)
		slog.Warn("tls-bridge: the signing CA is expiring and this proxy has been up too long to renew it",
			"ca_not_after", ca.NotAfter.Local().Format(time.RFC3339),
			"already_expired", expired,
			"ca_fingerprint", FingerprintSHA256(ca),
			"why", "renewal is evaluated at startup only, so a proxy that stays up past the "+
				"renewal window keeps signing with the old CA until it is restarted",
			"fix", "restart the proxy to mint a replacement, then restart the clients that trust it")
	})
}

type cacheEntry struct {
	host    string
	cert    *tls.Certificate
	expires time.Time
}

// renewBefore is the gap between the cache deadline (now+ttl, when Get
// re-mints) and the leaf's NotAfter (now+ttl+renewBefore). It gives a
// connection that grabbed the leaf just before re-mint ample remaining
// validity, and a window for Get's NotAfter backstop to act.
const renewBefore = time.Hour

func NewMinter(src CASource, o MinterOpts) *Minter {
	if o.CacheMax <= 0 {
		o.CacheMax = 1024
	}
	if o.LeafTTL <= 0 {
		o.LeafTTL = 24 * time.Hour
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		// P-256 keygen from crypto/rand effectively never fails; if it does, fail
		// fast at construction rather than nil-deref later in mint().
		panic(fmt.Errorf("tlsbridge: generate leaf key: %w", err))
	}
	return &Minter{
		src: src, max: o.CacheMax, ttl: o.LeafTTL, leafKey: key,
		ll: list.New(), items: make(map[string]*list.Element),
	}
}

// GetCertificate satisfies tls.Config.GetCertificate via the SNI server name.
func (m *Minter) GetCertificate(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
	host := chi.ServerName
	if host == "" {
		return nil, fmt.Errorf("tlsbridge: no SNI; caller must use GetCertificateForHost with the dialed IP")
	}
	return m.GetCertificateForHost(host)
}

func (m *Minter) GetCertificateForHost(host string) (*tls.Certificate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.items[host]; ok {
		e := el.Value.(*cacheEntry)
		// Gate freshness on WALL-clock deadlines, not the process monotonic
		// clock. Across a host suspend / VM pause the monotonic clock freezes
		// while wall time — and the leaf's x509 validity — keeps advancing, so
		// a monotonic deadline would keep serving a leaf the client already
		// rejects as expired. e.expires is monotonic-stripped (.Round(0) below);
		// the Leaf.NotAfter check is the backstop tied to the cert's real
		// validity, the only value the client actually verifies.
		now := time.Now()
		if now.Before(e.expires) && e.cert.Leaf != nil && now.Before(e.cert.Leaf.NotAfter) {
			m.ll.MoveToFront(el)
			return e.cert, nil
		}
		m.ll.Remove(el)
		delete(m.items, host)
	}
	cert, err := m.mint(host)
	if err != nil {
		return nil, err
	}
	// .Round(0) strips the monotonic reading so the deadline is a pure wall-clock
	// time; comparisons against time.Now() then fall back to the wall clock and
	// survive suspend (see the cache-hit gate above).
	el := m.ll.PushFront(&cacheEntry{host: host, cert: cert, expires: time.Now().Add(m.ttl).Round(0)})
	m.items[host] = el
	for m.ll.Len() > m.max {
		back := m.ll.Back()
		m.ll.Remove(back)
		delete(m.items, back.Value.(*cacheEntry).host)
	}
	return cert, nil
}

func (m *Minter) mint(host string) (*tls.Certificate, error) {
	caCert, caKey := m.src.Issuer()
	m.warnIfIssuerExpiring(caCert)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("tlsbridge: serial for %s: %w", host, err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Minute),
		// Leaf validity outlasts the cache deadline (now+ttl) by renewBefore so
		// a cached leaf is always re-minted before it can serve past expiry.
		NotAfter:    time.Now().Add(m.ttl + renewBefore),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, m.leafKey.Public(), caKey)
	if err != nil {
		return nil, fmt.Errorf("tlsbridge: mint leaf for %s: %w", host, err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("tlsbridge: parse minted leaf for %s: %w", host, err)
	}
	return &tls.Certificate{
		Certificate: [][]byte{der, caCert.Raw},
		PrivateKey:  m.leafKey,
		// Populate Leaf so Get can gate on the cert's real wall-clock NotAfter
		// (and the TLS stack avoids re-parsing on each handshake).
		Leaf: leaf,
	}, nil
}
