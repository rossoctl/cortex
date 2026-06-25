package openshell

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

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
