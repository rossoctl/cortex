package placeholderresolve

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// configure builds and configures a plugin with the given inline mappings.
func configure(t *testing.T, mappings map[string]string) *PlaceholderResolve {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"mappings": mappings})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	p := New()
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	return p
}

func TestConfigureDefaults(t *testing.T) {
	p := configure(t, map[string]string{"FOO": "x"})
	if p.cfg.Prefix != defaultPrefix {
		t.Errorf("prefix default = %q, want %q", p.cfg.Prefix, defaultPrefix)
	}
	if len(p.cfg.Headers) != 1 || p.cfg.Headers[0] != "Authorization" {
		t.Errorf("headers default = %v, want [Authorization]", p.cfg.Headers)
	}
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
		wantAuth   string // expected Authorization after OnRequest (when not rejected)
	}{
		{
			name:     "resolves placeholder bearer",
			authIn:   "Bearer openshell:resolve:env:ANTHROPIC_AUTH_TOKEN",
			wantAuth: "Bearer sk-real-token",
		},
		{
			name:     "non-placeholder passthrough",
			authIn:   "Bearer already-a-real-token",
			wantAuth: "Bearer already-a-real-token",
		},
		{
			name:       "unknown key fails closed",
			authIn:     "Bearer openshell:resolve:env:UNKNOWN",
			wantReject: true,
		},
		{
			name:       "invalid resolved value fails closed",
			authIn:     "Bearer openshell:resolve:env:BAD",
			wantReject: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := configure(t, mappings)
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
				// The placeholder must never be forwarded — on a reject the
				// caller drops the request, so the resolved value is moot,
				// but assert we did not leak a real secret either.
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
	p := configure(t, map[string]string{"A": "1", "B": "2"})
	got, found, ok := p.rewrite(context.Background(),
		"openshell:resolve:env:A and openshell:resolve:env:B")
	if !found || !ok {
		t.Fatalf("found=%v ok=%v, want both true", found, ok)
	}
	if want := "1 and 2"; got != want {
		t.Errorf("rewrite = %q, want %q", got, want)
	}
}

func TestUnconfiguredFailsClosed(t *testing.T) {
	p := New() // never Configured
	pctx := &pipeline.Context{Direction: pipeline.Outbound, Headers: http.Header{}}
	pctx.Headers.Set("Authorization", "Bearer openshell:resolve:env:FOO")
	act := p.OnRequest(context.Background(), pctx)
	if act.Type != pipeline.Reject {
		t.Fatalf("unconfigured plugin must reject, got %v", act.Type)
	}
}
