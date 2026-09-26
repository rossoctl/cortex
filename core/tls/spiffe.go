// Package tls builds *crypto/tls.Config values for authbridge's
// reverse-proxy and forward-proxy listeners using a SPIRE X.509 SVID
// source. Mode-aware: callers pass "permissive" or "strict" to drive
// the listener's fallback behavior; this package itself only models
// the cert / verification side.
//
// The package deliberately does not depend on go-spiffe — peer-SPIFFE
// extraction and trust-bundle verification are <40 LOC of crypto/x509
// stdlib code, and the rest of core is dependency-light by design.
package tls

import (
	"crypto/x509"
	"errors"
	"net/url"
)

// PeerSPIFFEID extracts the SPIFFE URI from a verified peer
// certificate's URI SAN list. Returns the empty string if the cert
// has no SPIFFE URI (workloads outside SPIRE) or has multiple — a
// SPIFFE-conformant cert has exactly one.
//
// Only schemes equal to "spiffe" are considered; an https:// URI in
// the SAN list (allowed by RFC 5280) is ignored.
func PeerSPIFFEID(cert *x509.Certificate) string {
	if cert == nil {
		return ""
	}
	var found *url.URL
	for _, u := range cert.URIs {
		if u != nil && u.Scheme == "spiffe" {
			if found != nil {
				// Two SPIFFE URIs in the same cert — non-conformant;
				// don't pick one arbitrarily.
				return ""
			}
			found = u
		}
	}
	if found == nil {
		return ""
	}
	return found.String()
}

// ErrNoPeerCert is returned by VerifyPeerSPIFFE-style helpers when a
// peer presented no certificate. Callers can errors.Is against it to
// distinguish "untrusted peer" from "no peer at all" (the latter
// usually means the listener was misconfigured to not request a
// client cert).
var ErrNoPeerCert = errors.New("no peer certificate presented")
