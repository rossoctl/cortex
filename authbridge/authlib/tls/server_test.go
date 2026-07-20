package tls_test

import (
	cryptotls "crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/rossoctl/rossocortex/authbridge/authlib/spiffe"
	authtls "github.com/rossoctl/rossocortex/authbridge/authlib/tls"
)

// fakeSource lets tests drive Certificate / TrustBundle results
// independently. Mirrors the X509Source interface; rotation tests use
// it to swap data mid-flight without touching the filesystem.
type fakeSource struct {
	cert      *cryptotls.Certificate
	bundle    *x509.CertPool
	certErr   error
	bundleErr error
}

func (s *fakeSource) Certificate() (*cryptotls.Certificate, error) {
	return s.cert, s.certErr
}
func (s *fakeSource) TrustBundle() (*x509.CertPool, error) {
	return s.bundle, s.bundleErr
}

// helper: turn the test-helper SVID PEMs into a *fakeSource and a
// matching *cryptotls.Certificate that any client can present.
func sourceFromSVID(t *testing.T, certPEM, keyPEM, bundlePEM []byte) *fakeSource {
	t.Helper()
	cert, err := cryptotls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(bundlePEM) {
		t.Fatalf("AppendCertsFromPEM failed")
	}
	return &fakeSource{cert: &cert, bundle: pool}
}

// Construction smoke: a source with valid cert + bundle yields a
// usable *tls.Config.
func TestServerConfig_Construction(t *testing.T) {
	certPEM, keyPEM, bundlePEM := generateSVIDForTest(t, "spiffe://test/server")
	src := sourceFromSVID(t, certPEM, keyPEM, bundlePEM)

	cfg, err := authtls.ServerConfig(src)
	if err != nil {
		t.Fatalf("ServerConfig: %v", err)
	}
	if cfg.MinVersion != cryptotls.VersionTLS13 {
		t.Errorf("MinVersion = %d, want TLS 1.3", cfg.MinVersion)
	}
	if cfg.ClientAuth != cryptotls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth = %v, want RequireAndVerifyClientCert", cfg.ClientAuth)
	}
	if cfg.GetCertificate == nil {
		t.Error("GetCertificate not wired")
	}
	if cfg.VerifyPeerCertificate == nil {
		t.Error("VerifyPeerCertificate not wired")
	}
}

// Construction returns the source error on cold-start (cert / bundle
// not yet written) so the listener fails fast and the caller's
// retry / wait logic kicks in.
func TestServerConfig_FailsOnUnreadableSource(t *testing.T) {
	src := &fakeSource{certErr: errSourceUnready{}}
	_, err := authtls.ServerConfig(src)
	if err == nil {
		t.Fatal("ServerConfig: expected error on unready source")
	}
}

// End-to-end mTLS handshake: server presents SVID-A, client presents
// SVID-B, both signed by the same CA → handshake succeeds and the
// server can extract the client's SPIFFE URI from its cert.
//
// We use raw tls.Listen / tls.Dial rather than httptest because
// httptest.StartTLS injects its own self-signed cert into the TLS
// config under some Go versions, which would fight our GetCertificate
// hook. Direct tls.Listen reflects how the real listener integrates.
func TestServerConfig_MTLSHandshake_Success(t *testing.T) {
	caKey, caCert, caPEM := generateCAForTest(t)
	srvCertPEM, srvKeyPEM := signLeafForTest(t, caKey, caCert, "spiffe://test/server")
	cliCertPEM, cliKeyPEM := signLeafForTest(t, caKey, caCert, "spiffe://test/client-a")

	srvSrc := sourceFromSVID(t, srvCertPEM, srvKeyPEM, caPEM)
	cliSrc := sourceFromSVID(t, cliCertPEM, cliKeyPEM, caPEM)

	srvCfg, err := authtls.ServerConfig(srvSrc)
	if err != nil {
		t.Fatalf("ServerConfig: %v", err)
	}
	cliCfg, err := authtls.ClientConfig(cliSrc)
	if err != nil {
		t.Fatalf("ClientConfig: %v", err)
	}

	listener, err := cryptotls.Listen("tcp", "127.0.0.1:0", srvCfg)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	defer listener.Close()

	var observedPeer atomic.Value
	observedPeer.Store("")

	srvErr := make(chan error, 1)
	go func() {
		srv := &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
					observedPeer.Store(authtls.PeerSPIFFEID(r.TLS.PeerCertificates[0]))
				}
				w.WriteHeader(http.StatusOK)
			}),
		}
		srvErr <- srv.Serve(listener)
	}()

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: cliCfg,
			DialContext:     (&net.Dialer{}).DialContext,
		},
	}
	resp, err := client.Get("https://" + listener.Addr().String() + "/")
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}
	if got := observedPeer.Load().(string); got != "spiffe://test/client-a" {
		t.Errorf("observed peer = %q, want spiffe://test/client-a", got)
	}

	// Tear the server down explicitly so srvErr returns and we don't
	// leak a goroutine. The Close above doesn't propagate to srv.Serve;
	// we use the listener's close as the signal.
	_ = listener.Close()
	<-srvErr // drain
}

// A client whose cert is signed by a CA NOT in the server's trust
// bundle must be rejected at the handshake. This is the core mTLS
// guarantee — the trust bundle IS the policy.
func TestServerConfig_RejectsUntrustedClient(t *testing.T) {
	srvCAKey, srvCACert, srvCAPEM := generateCAForTest(t)
	srvCertPEM, srvKeyPEM := signLeafForTest(t, srvCAKey, srvCACert, "spiffe://test/server")
	srvSrc := sourceFromSVID(t, srvCertPEM, srvKeyPEM, srvCAPEM)

	// Different CA — not in server's bundle.
	otherCAKey, otherCACert, _ := generateCAForTest(t)
	cliCertPEM, cliKeyPEM := signLeafForTest(t, otherCAKey, otherCACert, "spiffe://test/intruder")
	// Client trusts the server's CA so the server-side cert verifies
	// fine; the failure mode we want is the server rejecting THIS
	// client (not the client rejecting the server's cert).
	cliSrc := sourceFromSVID(t, cliCertPEM, cliKeyPEM, srvCAPEM)

	srvCfg, err := authtls.ServerConfig(srvSrc)
	if err != nil {
		t.Fatalf("ServerConfig: %v", err)
	}
	cliCfg, err := authtls.ClientConfig(cliSrc)
	if err != nil {
		t.Fatalf("ClientConfig: %v", err)
	}

	listener, err := cryptotls.Listen("tcp", "127.0.0.1:0", srvCfg)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	defer listener.Close()

	srvErr := make(chan error, 1)
	go func() {
		srv := &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		}
		srvErr <- srv.Serve(listener)
	}()

	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: cliCfg},
	}
	_, err = client.Get("https://" + listener.Addr().String() + "/")
	if err == nil {
		t.Fatal("expected handshake failure with untrusted client cert")
	}

	_ = listener.Close()
	<-srvErr
}

// An expired client cert must be rejected at the handshake. We don't
// rely solely on x509.Verify's transitive enforcement — locking the
// behavior here means a future refactor of verifyPeerChain can't
// accidentally drop expiry checking without the test catching it.
func TestServerConfig_RejectsExpiredClient(t *testing.T) {
	caKey, caCert, caPEM := generateCAForTest(t)
	srvCertPEM, srvKeyPEM := signLeafForTest(t, caKey, caCert, "spiffe://test/server")
	srvSrc := sourceFromSVID(t, srvCertPEM, srvKeyPEM, caPEM)

	expiredCertPEM, expiredKeyPEM := signLeafExpiredForTest(t, caKey, caCert, "spiffe://test/expired-client")
	cliSrc := sourceFromSVID(t, expiredCertPEM, expiredKeyPEM, caPEM)

	srvCfg, err := authtls.ServerConfig(srvSrc)
	if err != nil {
		t.Fatalf("ServerConfig: %v", err)
	}
	cliCfg, err := authtls.ClientConfig(cliSrc)
	if err != nil {
		t.Fatalf("ClientConfig: %v", err)
	}

	listener, err := cryptotls.Listen("tcp", "127.0.0.1:0", srvCfg)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	defer listener.Close()

	srvErr := make(chan error, 1)
	go func() {
		srv := &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		}
		srvErr <- srv.Serve(listener)
	}()

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: cliCfg}}
	_, err = client.Get("https://" + listener.Addr().String() + "/")
	if err == nil {
		t.Fatal("expected handshake failure with expired client cert")
	}

	_ = listener.Close()
	<-srvErr
}

// PeerSPIFFEID extracts the URI cleanly across the cases that matter:
// a normal SPIFFE URI; nil cert; cert with no URI SAN.
func TestPeerSPIFFEID(t *testing.T) {
	caKey, caCert, _ := generateCAForTest(t)
	certPEM, _ := signLeafForTest(t, caKey, caCert, "spiffe://test/example")

	parsed := parseLeafForTest(t, certPEM)
	if got := authtls.PeerSPIFFEID(parsed); got != "spiffe://test/example" {
		t.Errorf("PeerSPIFFEID = %q, want spiffe://test/example", got)
	}

	if got := authtls.PeerSPIFFEID(nil); got != "" {
		t.Errorf("PeerSPIFFEID(nil) = %q, want empty", got)
	}

	// Cert with no URI SAN at all.
	bareCertPEM, _ := signLeafForTestNoURI(t, caKey, caCert)
	bareParsed := parseLeafForTest(t, bareCertPEM)
	if got := authtls.PeerSPIFFEID(bareParsed); got != "" {
		t.Errorf("PeerSPIFFEID(no-URI cert) = %q, want empty", got)
	}
}

// A cert with two SPIFFE URIs is non-conformant. PeerSPIFFEID must
// return "" rather than picking one arbitrarily — picking the first
// would give an attacker who can attach a second SPIFFE URI to a
// peer cert a way to spoof identity.
func TestPeerSPIFFEID_RejectsMultiURI(t *testing.T) {
	caKey, caCert, _ := generateCAForTest(t)
	certPEM, _ := signLeafMultiURIForTest(t, caKey, caCert,
		"spiffe://test/legit", "spiffe://test/spoofed")

	parsed := parseLeafForTest(t, certPEM)
	if got := authtls.PeerSPIFFEID(parsed); got != "" {
		t.Errorf("PeerSPIFFEID(multi-URI cert) = %q, want empty (non-conformant cert)", got)
	}
}

// ErrNoPeerCert is reachable via errors.Is — locks the public sentinel.
func TestErrNoPeerCert_Sentinel(t *testing.T) {
	if !errors.Is(authtls.ErrNoPeerCert, authtls.ErrNoPeerCert) {
		t.Fatal("ErrNoPeerCert doesn't satisfy errors.Is against itself — sentinel broken")
	}
}

// errSourceUnready stands in for the cold-start "spiffe-helper hasn't
// written the file yet" case in fakeSource.
type errSourceUnready struct{}

func (errSourceUnready) Error() string { return "fake: source not ready yet" }

// We need fakeSource to satisfy the X509Source interface even when
// Certificate / TrustBundle return nil. Static check keeps the
// interface contract honest.
var _ spiffe.X509Source = (*fakeSource)(nil)
