package openshell

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	osv1 "github.com/kagenti/kagenti-extensions/authbridge/authlib/openshell/genproto/openshellv1"
)

// mockGateway is an in-process OpenShell gateway for client tests. It records
// the bearer credentials it observed so tests can assert the two-step auth
// (SA token -> IssueSandboxToken, then JWT -> GetSandboxProviderEnvironment).
type mockGateway struct {
	osv1.UnimplementedOpenShellServer

	mu             sync.Mutex
	sawSAToken     string
	sawJWT         string
	sawSandboxID   string
	issueCalls     int
	fetchCalls     int
	failFirstFetch bool // return Unauthenticated on the first fetch
}

func bearerOf(ctx context.Context) string {
	md, _ := metadata.FromIncomingContext(ctx)
	v := md.Get("authorization")
	if len(v) == 0 {
		return ""
	}
	return strings.TrimPrefix(v[0], "Bearer ")
}

func (m *mockGateway) IssueSandboxToken(ctx context.Context, _ *osv1.IssueSandboxTokenRequest) (*osv1.IssueSandboxTokenResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.issueCalls++
	m.sawSAToken = bearerOf(ctx)
	return &osv1.IssueSandboxTokenResponse{Token: "jwt-" + strconv.Itoa(m.issueCalls)}, nil
}

func (m *mockGateway) GetSandboxProviderEnvironment(ctx context.Context, req *osv1.GetSandboxProviderEnvironmentRequest) (*osv1.GetSandboxProviderEnvironmentResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fetchCalls++
	m.sawJWT = bearerOf(ctx)
	m.sawSandboxID = req.GetSandboxId()
	if m.failFirstFetch && m.fetchCalls == 1 {
		return nil, status.Error(codes.Unauthenticated, "jwt expired")
	}
	return &osv1.GetSandboxProviderEnvironmentResponse{
		Environment: map[string]string{"ANTHROPIC_AUTH_TOKEN": "sk-real"},
	}, nil
}

func startMockGateway(t *testing.T, m *mockGateway) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	osv1.RegisterOpenShellServer(srv, m)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func writeSAToken(t *testing.T, value string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(value), 0o600); err != nil {
		t.Fatalf("write SA token: %v", err)
	}
	return p
}

func TestClientFetchEnvironment(t *testing.T) {
	m := &mockGateway{}
	addr := startMockGateway(t, m)
	c, err := Dial(Config{
		Endpoint:    "http://" + addr,
		SATokenPath: writeSAToken(t, "sa-token-abc"),
		SandboxID:   "sb-123",
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	env, err := c.FetchEnvironment(context.Background())
	if err != nil {
		t.Fatalf("FetchEnvironment: %v", err)
	}
	if got := env.Values["ANTHROPIC_AUTH_TOKEN"]; got != "sk-real" {
		t.Errorf("env[ANTHROPIC_AUTH_TOKEN] = %q, want sk-real", got)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sawSAToken != "sa-token-abc" {
		t.Errorf("IssueSandboxToken saw bearer %q, want the SA token", m.sawSAToken)
	}
	if m.sawJWT != "jwt-1" {
		t.Errorf("GetSandboxProviderEnvironment saw bearer %q, want the minted jwt-1", m.sawJWT)
	}
	if m.sawSandboxID != "sb-123" {
		t.Errorf("saw sandbox_id %q, want sb-123", m.sawSandboxID)
	}
}

func TestClientReMintOnUnauthenticated(t *testing.T) {
	m := &mockGateway{failFirstFetch: true}
	addr := startMockGateway(t, m)
	c, err := Dial(Config{
		Endpoint:    "http://" + addr,
		SATokenPath: writeSAToken(t, "sa-token-abc"),
		SandboxID:   "sb-123",
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	env, err := c.FetchEnvironment(context.Background())
	if err != nil {
		t.Fatalf("FetchEnvironment: %v", err)
	}
	if env.Values["ANTHROPIC_AUTH_TOKEN"] != "sk-real" {
		t.Error("expected sk-real after re-mint")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.issueCalls != 2 {
		t.Errorf("issueCalls = %d, want 2 (initial mint + re-mint after Unauthenticated)", m.issueCalls)
	}
}

func TestDialRefusesPlaintextNonLoopback(t *testing.T) {
	_, err := Dial(Config{
		Endpoint:    "http://gw.example.com:8080",
		SATokenPath: writeSAToken(t, "sa"),
		SandboxID:   "sb",
	})
	if err == nil {
		t.Fatal("expected fail-closed error for non-loopback plaintext")
	}
	if !strings.Contains(err.Error(), "refusing plaintext") {
		t.Errorf("error = %v, want a refusing-plaintext error", err)
	}
}

func TestDialInsecureOptIn(t *testing.T) {
	c, err := Dial(Config{
		Endpoint:    "http://gw.example.com:8080",
		SATokenPath: writeSAToken(t, "sa"),
		SandboxID:   "sb",
		Insecure:    true,
	})
	if err != nil {
		t.Fatalf("Insecure opt-in should dial plaintext: %v", err)
	}
	_ = c.Close()
}

func TestDialLoopbackPlaintextAllowed(t *testing.T) {
	for _, ep := range []string{"http://127.0.0.1:8080", "http://localhost:8080", "http://[::1]:8080"} {
		c, err := Dial(Config{Endpoint: ep, SATokenPath: writeSAToken(t, "sa"), SandboxID: "sb"})
		if err != nil {
			t.Errorf("loopback %q should dial without Insecure: %v", ep, err)
			continue
		}
		_ = c.Close()
	}
}

func TestDialHTTPSUsesMTLS(t *testing.T) {
	dir := writeMTLSCertDir(t)
	c, err := Dial(Config{
		Endpoint:    "https://gw.example.com:8080",
		MTLSCert:    filepath.Join(dir, "tls.crt"),
		MTLSKey:     filepath.Join(dir, "tls.key"),
		MTLSCA:      filepath.Join(dir, "ca.crt"),
		SATokenPath: writeSAToken(t, "sa"),
		SandboxID:   "sb",
	})
	if err != nil {
		t.Fatalf("https dial with cert dir: %v", err)
	}
	_ = c.Close()
}

func TestDialHTTPSRequiresMTLS(t *testing.T) {
	_, err := Dial(Config{
		Endpoint:    "https://gw.example.com:8080",
		SATokenPath: writeSAToken(t, "sa"),
		SandboxID:   "sb",
	})
	if err == nil {
		t.Fatal("https without mtls_cert/key/ca should fail validation")
	}
}

func TestMTLSConfig(t *testing.T) {
	dir := writeMTLSCertDir(t)
	cfg, err := mtlsConfig(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), filepath.Join(dir, "ca.crt"), "gw.example.com")
	if err != nil {
		t.Fatalf("mtlsConfig: %v", err)
	}
	if cfg.ServerName != "gw.example.com" {
		t.Errorf("ServerName = %q, want gw.example.com", cfg.ServerName)
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("client certs = %d, want 1", len(cfg.Certificates))
	}
}

func TestHostOnly(t *testing.T) {
	for in, want := range map[string]string{
		"host:443":       "host",
		"host":           "host",
		"127.0.0.1:8080": "127.0.0.1",
		"[::1]:8080":     "::1",
	} {
		if got := hostOnly(in); got != want {
			t.Errorf("hostOnly(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for _, h := range []string{"127.0.0.1:8080", "localhost:443", "[::1]:80", "127.0.0.1"} {
		if !isLoopbackHost(h) {
			t.Errorf("isLoopbackHost(%q) = false, want true", h)
		}
	}
	for _, h := range []string{"gw.example.com:8080", "10.0.0.5:443", "8.8.8.8"} {
		if isLoopbackHost(h) {
			t.Errorf("isLoopbackHost(%q) = true, want false", h)
		}
	}
}

// writeMTLSCertDir generates a self-signed CA cert + key and writes it as
// ca.crt / tls.crt / tls.key in a temp dir, for exercising the mTLS branch.
func writeMTLSCertDir(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	dir := t.TempDir()
	for name, data := range map[string][]byte{"ca.crt": certPEM, "tls.crt": certPEM, "tls.key": keyPEM} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}
