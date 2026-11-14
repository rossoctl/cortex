package tlsbridge

import (
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"time"
)

// handshakeTimeout bounds the server-side TLS handshake against the agent so a
// stalled/malicious client cannot pin the serving goroutine indefinitely.
const handshakeTimeout = 10 * time.Second

// Terminator wraps a sniffed client conn as a tls.Server, using the Minter to
// forge a per-SNI leaf. ALPN offers h2 + http/1.1.
type Terminator struct {
	minter *Minter
}

func NewTerminator(m *Minter) *Terminator { return &Terminator{minter: m} }

// ErrSNIMismatch is Terminate refusing a ClientHello whose SNI names a host other
// than the one it was given.
var ErrSNIMismatch = errors.New("tls-bridge: SNI does not name the CONNECT host")

// Terminate completes the server-side TLS handshake against the agent. host is
// the dialed name/IP, used to mint when the ClientHello carries no SNI. With
// exactSNI, a ClientHello whose SNI names another host fails with ErrSNIMismatch.
func (t *Terminator) Terminate(client net.Conn, host string, exactSNI bool) (*tls.Conn, error) {
	cfg := &tls.Config{
		NextProtos: []string{"h2", "http/1.1"},
		GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if exactSNI && chi.ServerName != "" && !sameHost(chi.ServerName, host) {
				return nil, ErrSNIMismatch
			}
			if chi.ServerName != "" {
				return t.minter.GetCertificateForHost(chi.ServerName)
			}
			return t.minter.GetCertificateForHost(host)
		},
	}
	conn := tls.Server(client, cfg)
	// Bound the handshake; clear the deadline on success so it does not leak
	// into the served-connection (keep-alive) lifetime.
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	if err := conn.Handshake(); err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// sameHost compares two host names as DNS does: case-insensitively, with a
// trailing dot ignored. The comparison is textual: an IP never matches a name.
func sameHost(a, b string) bool {
	return strings.EqualFold(strings.TrimSuffix(a, "."), strings.TrimSuffix(b, "."))
}
