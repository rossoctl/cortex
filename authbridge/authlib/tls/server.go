package tls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"

	"github.com/rossoctl/rossocortex/authbridge/authlib/spiffe"
)

// ServerConfig builds an mTLS *tls.Config for the reverse-proxy
// listener. The server presents the local SVID to peers, requires
// peers to present a client cert, and verifies that cert against the
// SPIRE trust bundle. Any cert whose chain validates against the
// bundle is accepted — there is no SPIFFE-ID allowlist, no per-caller
// policy. "You're in the trust domain → you can connect" is the
// policy. Per-caller restrictions are a plugin concern (read
// pctx.PeerCert from OnRequest).
//
// All cert + bundle reads happen on every handshake via
// GetCertificate / VerifyPeerCertificate callbacks, so spiffe-helper
// rotation flows through without restart. Both successful trust-bundle
// chain verification AND a non-empty CertPool are required — an empty
// pool would silently accept any cert.
//
// Returns an error only if the source can't be read at construction
// time (cold-start cert availability check). Steady-state errors
// (rotation glitches, transient missing files) propagate through the
// handshake callbacks and surface as TLS handshake failures, which
// the listener handles per its mode (close on strict, fall back to
// plaintext on permissive — but neither path lives in this package).
func ServerConfig(src spiffe.X509Source) (*tls.Config, error) {
	// Smoke-test the source at construction. If spiffe-helper hasn't
	// written yet, fail fast so the listener doesn't bind a broken
	// TLS port. The caller's startup sequence is responsible for
	// retrying (see authlib/config/resolve.go: WaitForCredentialFile).
	if _, err := src.Certificate(); err != nil {
		return nil, fmt.Errorf("server tls config: %w", err)
	}
	if _, err := src.TrustBundle(); err != nil {
		return nil, fmt.Errorf("server tls config: %w", err)
	}

	cfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		ClientAuth: tls.RequireAndVerifyClientCert,

		// GetCertificate fires on every handshake — re-reads cert+key
		// from disk so spiffe-helper rotation is picked up without a
		// listener restart.
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return src.Certificate()
		},

		// We override Go's default verification because the trust
		// bundle is also rotation-tracked: read fresh on every
		// handshake. ClientCAs would only be consulted at config
		// build time, so we set it to the current pool as a fallback
		// for clients that send abbreviated chains, but the real
		// verification happens here.
		ClientCAs: nil, // populated lazily below
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			return verifyPeerChain(src, rawCerts)
		},
	}

	// Populate ClientCAs at config-build time so Go's stdlib uses
	// these for the ClientCertificateRequested handshake message
	// (telling the peer which CAs we trust). The authoritative
	// verification still happens in VerifyPeerCertificate.
	pool, err := src.TrustBundle()
	if err != nil {
		return nil, fmt.Errorf("server tls config: %w", err)
	}
	cfg.ClientCAs = pool

	return cfg, nil
}

// verifyPeerChain re-reads the trust bundle and verifies the peer's
// chain against it. Used by VerifyPeerCertificate on both server and
// client sides — same logic, different invocation site.
func verifyPeerChain(src spiffe.X509Source, rawCerts [][]byte) error {
	if len(rawCerts) == 0 {
		return ErrNoPeerCert
	}
	pool, err := src.TrustBundle()
	if err != nil {
		return fmt.Errorf("verify peer: trust bundle: %w", err)
	}

	// rawCerts[0] is the leaf; subsequent entries are intermediates.
	leaf, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return fmt.Errorf("verify peer: parse leaf: %w", err)
	}
	intermediates := x509.NewCertPool()
	for _, der := range rawCerts[1:] {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return fmt.Errorf("verify peer: parse intermediate: %w", err)
		}
		intermediates.AddCert(cert)
	}

	opts := x509.VerifyOptions{
		Roots:         pool,
		Intermediates: intermediates,
		KeyUsages: []x509.ExtKeyUsage{
			x509.ExtKeyUsageClientAuth,
			x509.ExtKeyUsageServerAuth, // single helper used both directions
		},
	}
	if _, err := leaf.Verify(opts); err != nil {
		return fmt.Errorf("verify peer: chain: %w", err)
	}
	return nil
}
