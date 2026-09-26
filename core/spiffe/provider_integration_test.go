//go:build integration

package spiffe

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestProvider_BasicLifecycle exercises NewProvider against a real
// SPIRE Workload API socket. Skipped unless SPIFFE_ENDPOINT_SOCKET is
// set. Run with: SPIFFE_ENDPOINT_SOCKET=unix:///run/spire/agent.sock \
//
//	go test -tags=integration ./spiffe/... -run TestProvider_BasicLifecycle
func TestProvider_BasicLifecycle(t *testing.T) {
	socket := os.Getenv("SPIFFE_ENDPOINT_SOCKET")
	if socket == "" {
		t.Skip("SPIFFE_ENDPOINT_SOCKET not set; skipping integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dir := t.TempDir()
	p, err := NewProvider(ctx, ProviderConfig{
		SocketPath:  socket,
		MirrorFiles: true,
		MirrorDir:   dir,
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	defer p.Close()

	if p.X509Source() == nil {
		t.Error("X509Source() = nil, want non-nil")
	}
	jwtSrc, err := p.JWTSource("test-audience")
	if err != nil {
		t.Fatalf("JWTSource(\"test-audience\"): %v", err)
	}
	if jwtSrc == nil {
		t.Error("JWTSource(audience) = nil, want non-nil")
	}

	// Sanity-check the X509 source can fetch a cert (the cold-start
	// gate inside NewProvider should already have made this safe).
	cert, err := p.X509Source().Certificate()
	if err != nil {
		t.Fatalf("Certificate: %v", err)
	}
	if cert == nil || len(cert.Certificate) == 0 {
		t.Errorf("Certificate returned empty cert chain")
	}
}

// TestProvider_AgentUnreachable verifies NewProvider returns an error
// (rather than blocking forever) when the socket path doesn't exist
// and the context has a short deadline.
func TestProvider_AgentUnreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	_, err := NewProvider(ctx, ProviderConfig{
		SocketPath:  "unix:///nonexistent/spire-agent.sock",
		MirrorFiles: false,
	})
	if err == nil {
		t.Fatal("NewProvider with nonexistent socket = nil error, want non-nil")
	}
}
