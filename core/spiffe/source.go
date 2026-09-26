// Package spiffe provides framework-shared SPIFFE credential helpers.
// Its consumers are the mTLS layer in core/tlsconfig with the proxy-sidecar
// listeners (X509Source), the token-exchange plugin (JWTSource), and the
// plugin framework, which injects *Provider into any plugin implementing
// ProviderConsumer; future LLM-judges or audit plugins that need workload
// identity can layer on top.
//
// Both interfaces are declared here rather than inside the plugin that
// consumes them, so the framework Provider can hand them out without
// importing plugin-internal code.
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
