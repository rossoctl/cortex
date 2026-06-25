package placeholderresolve

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"

	osv1 "github.com/kagenti/kagenti-extensions/authbridge/authlib/openshell/genproto/openshellv1"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// e2eMockGateway is a minimal in-process OpenShell gateway: it mints a fixed
// JWT and returns one resolved credential.
type e2eMockGateway struct {
	osv1.UnimplementedOpenShellServer
}

func (e2eMockGateway) IssueSandboxToken(context.Context, *osv1.IssueSandboxTokenRequest) (*osv1.IssueSandboxTokenResponse, error) {
	return &osv1.IssueSandboxTokenResponse{Token: "jwt-1"}, nil
}

func (e2eMockGateway) GetSandboxProviderEnvironment(context.Context, *osv1.GetSandboxProviderEnvironmentRequest) (*osv1.GetSandboxProviderEnvironmentResponse, error) {
	return &osv1.GetSandboxProviderEnvironmentResponse{
		Environment: map[string]string{"ANTHROPIC_AUTH_TOKEN": "sk-real"},
	}, nil
}

// TestOnRequestGatewayResolver drives the whole gateway path end-to-end:
// Configure(gateway block) -> Init (background warm-up) -> Ready -> OnRequest
// resolves the placeholder against the gateway's provider environment.
func TestOnRequestGatewayResolver(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	osv1.RegisterOpenShellServer(srv, e2eMockGateway{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	saPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(saPath, []byte("sa-token"), 0o600); err != nil {
		t.Fatalf("write SA token: %v", err)
	}

	raw, _ := json.Marshal(map[string]any{
		"gateway": map[string]any{
			"endpoint":      "http://" + lis.Addr().String(),
			"sandbox_id":    "sb-123",
			"sa_token_path": saPath,
		},
	})

	p := New()
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := p.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	// The gateway resolver primes its cache asynchronously.
	deadline := time.Now().Add(3 * time.Second)
	for !p.Ready() {
		if time.Now().After(deadline) {
			t.Fatal("plugin never became Ready (gateway resolver did not prime)")
		}
		time.Sleep(20 * time.Millisecond)
	}

	pctx := &pipeline.Context{Direction: pipeline.Outbound, Headers: http.Header{}}
	pctx.Headers.Set("Authorization", "Bearer openshell:resolve:env:ANTHROPIC_AUTH_TOKEN")

	act := p.OnRequest(context.Background(), pctx)
	if act.Type != pipeline.Continue {
		t.Fatalf("OnRequest = %v (violation %+v), want Continue", act.Type, act.Violation)
	}
	if got := pctx.Headers.Get("Authorization"); got != "Bearer sk-real" {
		t.Errorf("Authorization = %q, want Bearer sk-real", got)
	}
}
