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
