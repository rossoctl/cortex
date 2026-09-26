package spiffe

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

// x509SVIDFetcher captures just the two methods workloadX509 needs from
// *workloadapi.X509Source. Defining it here lets tests substitute a hand
// rolled fake without depending on go-spiffe's internal fakeworkloadapi
// package (which is not importable from outside the SDK).
type x509SVIDFetcher interface {
	GetX509SVID() (*x509svid.SVID, error)
	GetX509BundleForTrustDomain(trustDomain spiffeid.TrustDomain) (*x509bundle.Bundle, error)
}

// workloadX509 adapts a *workloadapi.X509Source to the framework
// X509Source interface. Trust domain captured at construction;
// TrustBundle returns the bundle for the workload's own domain only
// (see TrustBundle for the federation caveat).
type workloadX509 struct {
	sdk x509SVIDFetcher
	td  spiffeid.TrustDomain
}

// Compile-time assertion that workloadX509 satisfies X509Source. Also
// keeps the type "used" from the linter's point of view while the
// constructor is still waiting for its Provider caller (plan task T5).
var _ X509Source = (*workloadX509)(nil)

// newWorkloadX509 wraps a go-spiffe X509Source so it satisfies the local
// X509Source interface. The td argument fixes which trust domain bundle
// is returned by TrustBundle — typically the workload's own domain.
//
// Wired in by the upcoming Provider type (see plan task T5); not called
// by any caller yet, hence the nolint:unused on the constructor (the
// var _ X509Source assertion above already verifies the interface
// contract at build time).
//
//nolint:unused // wired in by Provider in plan task T5
func newWorkloadX509(sdk *workloadapi.X509Source, td spiffeid.TrustDomain) *workloadX509 {
	return &workloadX509{sdk: sdk, td: td}
}

// Certificate returns the latest X.509-SVID as a *tls.Certificate suitable
// for tls.Config.GetCertificate / GetClientCertificate. Callers invoke
// this on every handshake to pick up rotation.
func (w *workloadX509) Certificate() (*tls.Certificate, error) {
	svid, err := w.sdk.GetX509SVID()
	if err != nil {
		return nil, fmt.Errorf("workloadX509: GetX509SVID: %w", err)
	}
	if svid == nil || len(svid.Certificates) == 0 {
		return nil, errors.New("workloadX509: SVID has no certificates")
	}
	raw := make([][]byte, 0, len(svid.Certificates))
	for _, c := range svid.Certificates {
		raw = append(raw, c.Raw)
	}
	return &tls.Certificate{
		Certificate: raw,
		PrivateKey:  svid.PrivateKey,
		Leaf:        svid.Certificates[0],
	}, nil
}

// TrustBundle returns the X.509 bundle for the workload's own trust
// domain only, as an *x509.CertPool. Federated peers (SVIDs from other
// trust domains) are NOT validated by this source: GetX509BundleForTrustDomain
// returns just the requested domain's authorities, not federated peers'.
// Accepting federated SVIDs would require a bundle source that spans
// every federated domain (e.g. a *x509bundle.Set). Future work in this
// package may add that variant; for now the proxy-sidecar mTLS path is
// single-domain. Empty pool is treated as an error: a TLS handshake with
// no roots would silently accept any cert, defeating the point of mTLS.
func (w *workloadX509) TrustBundle() (*x509.CertPool, error) {
	bundle, err := w.sdk.GetX509BundleForTrustDomain(w.td)
	if err != nil {
		return nil, fmt.Errorf("workloadX509: GetX509BundleForTrustDomain(%s): %w", w.td, err)
	}
	authorities := bundle.X509Authorities()
	if len(authorities) == 0 {
		return nil, errors.New("workloadX509: trust bundle is empty")
	}
	pool := x509.NewCertPool()
	for _, c := range authorities {
		pool.AddCert(c)
	}
	return pool, nil
}
