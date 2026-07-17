package forwardproxy

import (
	"bufio"
	"bytes"
	cryptotls "crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

// Captured (iptables-REDIRECTed) connections carry no CONNECT line, so the
// destination hostname must be recovered from the connection's own first bytes:
// the TLS ClientHello SNI for HTTPS, or the HTTP Host header for plaintext HTTP.
// This gives policy parity with the explicit-proxy path (which reads r.Host).
// The recovered name is used ONLY as the policy key (pctx.Host); the dial target
// stays the SO_ORIGINAL_DST IP. See HandleTransparentConn.

const (
	// sniffBufSize bounds how much of the leading bytes we buffer to find the
	// SNI / Host header. Real ClientHellos and request header blocks fit well
	// within this; anything larger falls back to the IP.
	sniffBufSize = 8192
	// sniffTimeout bounds the peek so a client that connects but sends nothing
	// (or a server-first protocol) can't pin a goroutine. Only relevant on the
	// sniffed ports, where the client speaks first, so it is rarely hit.
	sniffTimeout = 5 * time.Second
)

// errSniffDone aborts the throwaway TLS handshake once we have the SNI.
var errSniffDone = errors.New("forwardproxy: sni sniff complete")

// shouldSniff reports whether dst's port is one where the client speaks first
// with an HTTP/TLS preamble we can parse. Gating on these ports avoids adding
// peek latency to non-HTTP, often server-first protocols (SSH:22, SMTP:25, ...)
// that we would only ever blind-tunnel anyway.
func shouldSniff(dst string) bool {
	_, port, err := net.SplitHostPort(dst)
	if err != nil {
		return false
	}
	switch port {
	case "80", "443", "8080", "8443":
		return true
	default:
		return false
	}
}

// sniffHost peeks the leading bytes of conn to recover the destination hostname
// (TLS SNI or HTTP Host header) without consuming them: it returns the hostname
// (without port; "" if none could be recovered) and a net.Conn that replays the
// peeked bytes so the downstream tunnel still forwards the handshake/request
// verbatim. A read deadline bounds the peek and is cleared before returning.
func sniffHost(conn net.Conn) (string, net.Conn) {
	br := bufio.NewReaderSize(conn, sniffBufSize)
	wrapped := &peekedConn{Conn: conn, r: br}

	_ = conn.SetReadDeadline(time.Now().Add(sniffTimeout))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()

	first, err := br.Peek(1)
	if err != nil || len(first) == 0 {
		return "", wrapped
	}
	switch {
	case first[0] == 0x16: // TLS handshake record
		return stripPort(sniffTLSSNI(br)), wrapped
	case first[0] >= 'A' && first[0] <= 'Z': // ASCII HTTP method
		return stripPort(sniffHTTPHost(br)), wrapped
	default:
		return "", wrapped
	}
}

// sniffTLSSNI peeks the first TLS record (the ClientHello) and extracts the SNI.
func sniffTLSSNI(br *bufio.Reader) string {
	hdr, err := br.Peek(5)
	if err != nil || len(hdr) < 5 {
		return ""
	}
	end := 5 + (int(hdr[3])<<8 | int(hdr[4]))
	if end > br.Size() {
		end = br.Size()
	}
	full, _ := br.Peek(end) // best effort: parse whatever is buffered
	return extractSNI(full)
}

// extractSNI parses the SNI out of a buffered ClientHello by driving a throwaway
// server-side handshake over the bytes and capturing ServerName in the config
// callback, then aborting. Leans on crypto/tls's hardened parser rather than a
// hand-rolled one. Returns "" if the bytes are not a parseable ClientHello.
func extractSNI(clientHello []byte) string {
	var sni string
	_ = cryptotls.Server(readOnlyConn{r: bytes.NewReader(clientHello)}, &cryptotls.Config{
		GetConfigForClient: func(chi *cryptotls.ClientHelloInfo) (*cryptotls.Config, error) {
			sni = chi.ServerName
			return nil, errSniffDone
		},
	}).Handshake()
	return sni
}

// sniffHTTPHost peeks the request header block and returns the Host header.
func sniffHTTPHost(br *bufio.Reader) string {
	end := br.Buffered()
	if end < 1 {
		end = 1
	}
	for {
		buf, err := br.Peek(end)
		if i := bytes.Index(buf, []byte("\r\n\r\n")); i >= 0 {
			return parseHTTPHost(buf[:i+4])
		}
		if err != nil || end >= br.Size() {
			return parseHTTPHost(buf) // best effort on what we have
		}
		end++
	}
}

func parseHTTPHost(headerBytes []byte) string {
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(headerBytes)))
	if err != nil {
		return ""
	}
	return req.Host
}

// stripPort drops a trailing :port if present (HTTP Host headers may carry one;
// SNI never does). The real port comes from the SO_ORIGINAL_DST destination.
func stripPort(host string) string {
	if host == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// peekedConn is a net.Conn whose Read replays bytes buffered by a bufio.Reader
// during sniffing, then continues from the underlying conn. Writes, Close,
// deadlines, and addresses delegate to the embedded conn.
type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// Peek returns the next n buffered bytes without consuming them. Used by the
// TLS-bridge classify step on the CONNECT and transparent paths.
func (c *peekedConn) Peek(n int) ([]byte, error) { return c.r.Peek(n) }

// readOnlyConn adapts a byte buffer to net.Conn for crypto/tls's server-side
// parser. Reads come from the buffer; writes are discarded (the throwaway
// handshake never needs to send), and the parser aborts via errSniffDone before
// any write would matter.
type readOnlyConn struct{ r io.Reader }

func (c readOnlyConn) Read(p []byte) (int, error)       { return c.r.Read(p) }
func (c readOnlyConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c readOnlyConn) Close() error                     { return nil }
func (c readOnlyConn) LocalAddr() net.Addr              { return nil }
func (c readOnlyConn) RemoteAddr() net.Addr             { return nil }
func (c readOnlyConn) SetDeadline(time.Time) error      { return nil }
func (c readOnlyConn) SetReadDeadline(time.Time) error  { return nil }
func (c readOnlyConn) SetWriteDeadline(time.Time) error { return nil }
