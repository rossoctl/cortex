package placeholderresolve

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/credinject"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// withMap returns a plugin whose resolver is an inline map, to exercise
// OnRequest/rewrite without a real credential source (the production sources
// are gateway/secret_dir; the map is a test seam).
func withMap(t *testing.T, mappings map[string]string) *PlaceholderResolve {
	t.Helper()
	p := New()
	p.cfg.Prefix = defaultPrefix
	p.resolver = credinject.MapResolver(mappings)
	return p
}

func configureJSON(t *testing.T, cfg map[string]any) (*PlaceholderResolve, error) {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	p := New()
	return p, p.Configure(raw)
}

func TestConfigureSecretDirDefaults(t *testing.T) {
	p, err := configureJSON(t, map[string]any{"source": "secret_dir", "secret_dir": t.TempDir()})
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if p.cfg.Prefix != defaultPrefix {
		t.Errorf("prefix default = %q, want %q", p.cfg.Prefix, defaultPrefix)
	}
}

func TestConfigureRequiresSource(t *testing.T) {
	if _, err := configureJSON(t, map[string]any{"secret_dir": "/tmp"}); err == nil {
		t.Fatal("expected error: source is required")
	}
}

func TestConfigureRejectsUnknownSource(t *testing.T) {
	// `env` was removed as a source (it let the agent read the proxy's env);
	// an unknown source must be rejected, not silently honored.
	if _, err := configureJSON(t, map[string]any{"source": "env"}); err == nil {
		t.Fatal("expected error for unknown source 'env'")
	}
}

func TestConfigureGatewayRequiresBlock(t *testing.T) {
	if _, err := configureJSON(t, map[string]any{"source": "gateway"}); err == nil {
		t.Fatal("expected error: gateway source requires a gateway block")
	}
}

func TestConfigureGatewayDefaults(t *testing.T) {
	// Loopback http endpoint so Dial succeeds without certs; we only assert
	// that the gateway sub-defaults are applied.
	p, err := configureJSON(t, map[string]any{
		"source": "gateway",
		"gateway": map[string]any{
			"endpoint":   "http://127.0.0.1:8080",
			"sandbox_id": "sb-1",
		},
	})
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if p.cfg.Gateway.SATokenPath != defaultSATokenPath {
		t.Errorf("gateway sa_token_path default not applied: %+v", p.cfg.Gateway)
	}
	_ = p.Shutdown(context.Background())
}

func TestOnRequest(t *testing.T) {
	mappings := map[string]string{
		"ANTHROPIC_AUTH_TOKEN": "sk-real-token",
		"BAD":                  "evil\r\nInjected: 1",
	}

	tests := []struct {
		name       string
		authIn     string
		wantReject bool
		wantAuth   string
	}{
		{"resolves placeholder bearer", "Bearer openshell:resolve:env:ANTHROPIC_AUTH_TOKEN", false, "Bearer sk-real-token"},
		{"non-placeholder passthrough", "Bearer already-a-real-token", false, "Bearer already-a-real-token"},
		{"unknown key fails closed", "Bearer openshell:resolve:env:UNKNOWN", true, ""},
		{"invalid resolved value fails closed", "Bearer openshell:resolve:env:BAD", true, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := withMap(t, mappings)
			pctx := &pipeline.Context{Direction: pipeline.Outbound, Headers: http.Header{}}
			pctx.Headers.Set("Authorization", tc.authIn)

			act := p.OnRequest(context.Background(), pctx)

			if tc.wantReject {
				if act.Type != pipeline.Reject {
					t.Fatalf("expected Reject, got %v", act.Type)
				}
				if act.Violation == nil || act.Violation.Code != "auth.unauthorized" {
					t.Errorf("expected auth.unauthorized violation, got %+v", act.Violation)
				}
				if got := pctx.Headers.Get("Authorization"); got == "Bearer sk-real-token" {
					t.Errorf("rejected request unexpectedly carries a resolved secret")
				}
				return
			}

			if act.Type != pipeline.Continue {
				t.Fatalf("expected Continue, got %v (violation %+v)", act.Type, act.Violation)
			}
			if got := pctx.Headers.Get("Authorization"); got != tc.wantAuth {
				t.Errorf("Authorization = %q, want %q", got, tc.wantAuth)
			}
		})
	}
}

func TestRewriteMultiplePlaceholders(t *testing.T) {
	p := withMap(t, map[string]string{"A": "1", "B": "2"})
	got, found, ok := p.rewrite(context.Background(),
		"openshell:resolve:env:A and openshell:resolve:env:B")
	if !found || !ok {
		t.Fatalf("found=%v ok=%v, want both true", found, ok)
	}
	if want := "1 and 2"; got != want {
		t.Errorf("rewrite = %q, want %q", got, want)
	}
}

func TestRewritePrefixWithoutValidKey(t *testing.T) {
	// A prefix not followed by a valid env key is left literal (no match).
	p := withMap(t, map[string]string{})
	got, found, ok := p.rewrite(context.Background(), "openshell:resolve:env:123")
	if found || !ok {
		t.Fatalf("found=%v ok=%v, want found=false ok=true", found, ok)
	}
	if got != "openshell:resolve:env:123" {
		t.Errorf("rewrite = %q, want the input unchanged", got)
	}
}

func TestSecretDirResolvesEndToEnd(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ANTHROPIC_AUTH_TOKEN"), []byte("sk-real\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := configureJSON(t, map[string]any{"source": "secret_dir", "secret_dir": dir})
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	pctx := &pipeline.Context{Direction: pipeline.Outbound, Headers: http.Header{}}
	pctx.Headers.Set("Authorization", "Bearer openshell:resolve:env:ANTHROPIC_AUTH_TOKEN")
	if act := p.OnRequest(context.Background(), pctx); act.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", act.Type)
	}
	if got := pctx.Headers.Get("Authorization"); got != "Bearer sk-real" {
		t.Errorf("Authorization = %q, want Bearer sk-real", got)
	}
}

func TestUnconfiguredFailsClosed(t *testing.T) {
	p := New() // never Configured
	pctx := &pipeline.Context{Direction: pipeline.Outbound, Headers: http.Header{}}
	pctx.Headers.Set("Authorization", "Bearer openshell:resolve:env:FOO")
	if act := p.OnRequest(context.Background(), pctx); act.Type != pipeline.Reject {
		t.Fatalf("unconfigured plugin must reject, got %v", act.Type)
	}
}
