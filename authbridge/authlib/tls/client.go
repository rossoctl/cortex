package tls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"

	"github.com/rossoctl/cortex/authbridge/authlib/spiffe"
)

// ClientConfig builds an mTLS *tls.Config for the forward-proxy
// listener's outbound dialer. The client presents the local SVID,
// verifies the server cert against the SPIRE trust bundle. Like
// ServerConfig, no SPIFFE-ID pinning — the trust bundle IS the
// policy.
//
// Cert+bundle reads happen on every handshake via
// GetClientCertificate / VerifyPeerCertificate callbacks, so
// spiffe-helper rotation flows through without restart.
func ClientConfig(src spiffe.X509Source) (*tls.Config, error) {
	if _, err := src.Certificate(); err != nil {
		return nil, fmt.Errorf("client tls config: %w", err)
	}
	if _, err := src.TrustBundle(); err != nil {
		return nil, fmt.Errorf("client tls config: %w", err)
	}

	cfg := &tls.Config{
		MinVersion: tls.VersionTLS13,

		// Authority verification: we override the default with our
		// own callback so the trust bundle is re-read on every
		// handshake (same rotation reason as server side).
		//
		// InsecureSkipVerify: true here lets crypto/tls skip its
		// built-in chain check; VerifyPeerCertificate then runs ours.
		// This pattern is documented in the stdlib — the name is a
		// misnomer, since our callback still verifies the peer chain
		// against the SPIRE trust bundle, just against the
		// rotation-aware pool that re-reads on every handshake.
		// CodeQL's go/disabled-certificate-check can't see across
		// the callback into verifyPeerChain in server.go, so we
		// suppress the alert inline.
		//
		//nolint:gosec // see VerifyPeerCertificate below
		// lgtm[go/disabled-certificate-check]
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			return verifyPeerChain(src, rawCerts)
		},

		// Client cert presentation: server requests it, we hand back
		// the freshly-read SVID.
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return src.Certificate()
		},
	}

	return cfg, nil
}
