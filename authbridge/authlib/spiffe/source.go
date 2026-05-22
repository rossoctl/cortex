// Package spiffe provides framework-shared SPIFFE credential helpers.
// Today the only consumer is the mTLS layer in authlib/tls and the
// proxy-sidecar listeners; future LLM-judges or audit plugins that
// need workload identity can layer on top.
//
// Compare with authlib/plugins/tokenexchange/spiffe/, which holds the
// JWT-SVID source used exclusively by token-exchange. That one stays
// plugin-internal because only token-exchange consumes it; this
// package is framework-shared because mTLS spans every listener.
package spiffe

import (
	"crypto/tls"
	"crypto/x509"
)

// X509Source produces the local X.509-SVID + trust bundle on demand.
// Implementations are responsible for hot-rotation handling — callers
// invoke Certificate / TrustBundle on every TLS handshake.
type X509Source interface {
	// Certificate returns the local SVID (cert + private key) for use
	// in tls.Config.GetCertificate / GetClientCertificate.
	Certificate() (*tls.Certificate, error)

	// TrustBundle returns the SPIRE trust bundle for verifying peer
	// certificates. Must be reloaded by the caller on every handshake
	// to pick up bundle rotation.
	TrustBundle() (*x509.CertPool, error)
}
