package spiffe

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net/url"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

// fakeX509SVIDFetcher is a hand-rolled fake that satisfies the package
// level x509SVIDFetcher interface. We can't use go-spiffe's internal
// fakeworkloadapi package — it lives under internal/test and is not
// importable from outside the SDK — so we operate one layer above the
// gRPC fake by mocking the methods workloadX509 actually consumes.
type fakeX509SVIDFetcher struct {
	svid    *x509svid.SVID
	svidErr error

	bundle    *x509bundle.Bundle
	bundleErr error

	closed bool
}

func (f *fakeX509SVIDFetcher) GetX509SVID() (*x509svid.SVID, error) {
	if f.closed {
		return nil, errors.New("source is closed")
	}
	if f.svidErr != nil {
		return nil, f.svidErr
	}
	return f.svid, nil
}

func (f *fakeX509SVIDFetcher) GetX509BundleForTrustDomain(_ spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	if f.closed {
		return nil, errors.New("source is closed")
	}
	if f.bundleErr != nil {
		return nil, f.bundleErr
	}
	return f.bundle, nil
}

// generateSVIDAndBundle issues a self-signed leaf certificate with the
// supplied SPIFFE ID as a URI SAN and returns it together with a single
// authority bundle. Good enough for adapter unit tests; real chain
// validation lives in higher-level integration tests.
func generateSVIDAndBundle(t *testing.T, id spiffeid.ID) (*x509svid.SVID, *x509bundle.Bundle) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{id.URL()},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}

	svid := &x509svid.SVID{
		ID:           id,
		Certificates: []*x509.Certificate{cert},
		PrivateKey:   priv,
	}
	bundle := x509bundle.FromX509Authorities(id.TrustDomain(), []*x509.Certificate{cert})
	return svid, bundle
}

func TestWorkloadX509_Certificate(t *testing.T) {
	td := spiffeid.RequireTrustDomainFromString("example.org")
	id := spiffeid.RequireFromString("spiffe://example.org/workload")
	svid, bundle := generateSVIDAndBundle(t, id)

	src := &workloadX509{
		sdk: &fakeX509SVIDFetcher{svid: svid, bundle: bundle},
		td:  td,
	}

	cert, err := src.Certificate()
	if err != nil {
		t.Fatalf("Certificate: %v", err)
	}
	if cert == nil || len(cert.Certificate) == 0 {
		t.Fatal("Certificate returned empty tls.Certificate")
	}
	if cert.Leaf == nil {
		t.Fatal("Certificate did not populate Leaf")
	}
	if cert.PrivateKey == nil {
		t.Fatal("Certificate did not carry the private key")
	}

	pool, err := src.TrustBundle()
	if err != nil {
		t.Fatalf("TrustBundle: %v", err)
	}
	if pool == nil {
		t.Fatal("TrustBundle returned nil pool")
	}
	// We added exactly one authority. CertPool exposes Subjects() (deprecated
	// but stable) which is good enough to confirm the pool isn't empty.
	if len(pool.Subjects()) == 0 { //nolint:staticcheck // SA1019: Subjects is the simplest way to confirm pool non-empty in tests
		t.Fatal("TrustBundle returned empty pool")
	}
}

func TestWorkloadX509_Certificate_AfterClose(t *testing.T) {
	td := spiffeid.RequireTrustDomainFromString("example.org")
	fake := &fakeX509SVIDFetcher{closed: true}
	src := &workloadX509{sdk: fake, td: td}

	if _, err := src.Certificate(); err == nil {
		t.Fatal("Certificate returned nil error after source closed")
	}
}

func TestWorkloadX509_TrustBundle_EmptyBundle(t *testing.T) {
	td := spiffeid.RequireTrustDomainFromString("example.org")
	emptyBundle := x509bundle.New(td)
	fake := &fakeX509SVIDFetcher{bundle: emptyBundle}
	src := &workloadX509{sdk: fake, td: td}

	_, err := src.TrustBundle()
	if err == nil {
		t.Fatal("TrustBundle returned nil error on empty bundle")
	}
	want := "workloadX509: trust bundle is empty"
	if err.Error() != want {
		t.Fatalf("TrustBundle error = %q, want %q", err.Error(), want)
	}
}
