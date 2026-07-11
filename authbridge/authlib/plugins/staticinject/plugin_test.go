package staticinject

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

func TestSwapsPlaceholderForRealToken(t *testing.T) {
	p := New()
	cfg := `{
		"source": "mappings",
		"mappings": {"api.example.com": "REAL"},
		"key_by": "host"
	}`
	if err := p.Configure(json.RawMessage(cfg)); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	pctx := &pipeline.Context{
		Host: "api.example.com",
		Headers: http.Header{
			"Authorization": []string{"Bearer PLACEHOLDER"},
		},
	}

	action := p.OnRequest(context.Background(), pctx)

	if action.Type != pipeline.Continue {
		t.Fatalf("OnRequest() action.Type = %v, want %v (violation: %+v)", action.Type, pipeline.Continue, action.Violation)
	}
	if got := pctx.Headers.Get("Authorization"); got != "Bearer REAL" {
		t.Errorf("Authorization header = %q, want %q", got, "Bearer REAL")
	}
}

func TestDenyOnMissingKey(t *testing.T) {
	p := New()
	cfg := `{
		"source": "mappings",
		"mappings": {"other.example.com": "REAL"},
		"key_by": "host"
	}`
	if err := p.Configure(json.RawMessage(cfg)); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	pctx := &pipeline.Context{
		Host: "api.example.com",
		Headers: http.Header{
			"Authorization": []string{"Bearer PLACEHOLDER"},
		},
	}

	action := p.OnRequest(context.Background(), pctx)

	if action.Type != pipeline.Reject {
		t.Fatalf("OnRequest() action.Type = %v, want %v (Reject)", action.Type, pipeline.Reject)
	}
	if got := pctx.Headers.Get("Authorization"); got != "Bearer PLACEHOLDER" {
		t.Errorf("Authorization header = %q, want unmodified %q", got, "Bearer PLACEHOLDER")
	}
}

func TestDenyOnMissingAuth(t *testing.T) {
	p := New()
	cfg := `{
		"source": "mappings",
		"mappings": {"api.example.com": "REAL"},
		"key_by": "host"
	}`
	if err := p.Configure(json.RawMessage(cfg)); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	pctx := &pipeline.Context{
		Host:    "api.example.com",
		Headers: http.Header{},
	}

	action := p.OnRequest(context.Background(), pctx)

	if action.Type != pipeline.Reject {
		t.Fatalf("OnRequest() action.Type = %v, want %v (Reject)", action.Type, pipeline.Reject)
	}
}

func TestDenyOnPlaceholderMismatch(t *testing.T) {
	p := New()
	cfg := `{
		"source": "mappings",
		"mappings": {"api.example.com": "REAL"},
		"key_by": "host",
		"placeholder": "EXPECTED_PLACEHOLDER"
	}`
	if err := p.Configure(json.RawMessage(cfg)); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	pctx := &pipeline.Context{
		Host: "api.example.com",
		Headers: http.Header{
			"Authorization": []string{"Bearer SOMETHING_ELSE"},
		},
	}

	action := p.OnRequest(context.Background(), pctx)

	if action.Type != pipeline.Reject {
		t.Fatalf("OnRequest() action.Type = %v, want %v (Reject)", action.Type, pipeline.Reject)
	}
	if got := pctx.Headers.Get("Authorization"); got != "Bearer SOMETHING_ELSE" {
		t.Errorf("Authorization header = %q, want unmodified %q", got, "Bearer SOMETHING_ELSE")
	}
}

func TestKeyByStatic(t *testing.T) {
	p := New()
	cfg := `{
		"source": "mappings",
		"mappings": {"X": "REAL"},
		"key_by": "static",
		"key": "X"
	}`
	if err := p.Configure(json.RawMessage(cfg)); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	pctx := &pipeline.Context{
		Host: "api.example.com",
		Headers: http.Header{
			"Authorization": []string{"Bearer PLACEHOLDER"},
		},
	}

	action := p.OnRequest(context.Background(), pctx)

	if action.Type != pipeline.Continue {
		t.Fatalf("OnRequest() action.Type = %v, want %v (violation: %+v)", action.Type, pipeline.Continue, action.Violation)
	}
	if got := pctx.Headers.Get("Authorization"); got != "Bearer REAL" {
		t.Errorf("Authorization header = %q, want %q", got, "Bearer REAL")
	}
}

func TestDenyOnUnsafeCredentialValue(t *testing.T) {
	p := New()
	cfg := `{
		"source": "mappings",
		"mappings": {"api.example.com": "evil\r\nX-Injected: 1"},
		"key_by": "host"
	}`
	if err := p.Configure(json.RawMessage(cfg)); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	pctx := &pipeline.Context{
		Host: "api.example.com",
		Headers: http.Header{
			"Authorization": []string{"Bearer PLACEHOLDER"},
		},
	}

	action := p.OnRequest(context.Background(), pctx)

	if action.Type != pipeline.Reject {
		t.Fatalf("OnRequest() action.Type = %v, want %v (Reject, CWE-113 unsafe value)", action.Type, pipeline.Reject)
	}
	if got := pctx.Headers.Get("Authorization"); got != "Bearer PLACEHOLDER" {
		t.Errorf("Authorization header = %q, want unmodified %q", got, "Bearer PLACEHOLDER")
	}
	if got := pctx.Headers.Get("Authorization"); strings.Contains(got, "X-Injected") {
		t.Errorf("Authorization header = %q, must not contain injected value", got)
	}
}

// =============================================================================
// Name / Capabilities / ConfigSchema / Configure sanity checks
// =============================================================================

func TestName(t *testing.T) {
	p := New()
	if got := p.Name(); got != "static-inject" {
		t.Errorf("Name() = %q, want %q", got, "static-inject")
	}
}

func TestConfigure_Invalid(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		wantErr string
	}{
		{"missing source", `{}`, "source"},
		{"unknown source", `{"source": "bogus"}`, "source"},
		{"secret_dir missing dir", `{"source": "secret_dir"}`, "secret_dir"},
		{"mappings missing map", `{"source": "mappings"}`, "mappings"},
		{"bad key_by", `{"source": "mappings", "mappings": {"a": "b"}, "key_by": "bogus"}`, "key_by"},
		{"key_by static missing key", `{"source": "mappings", "mappings": {"a": "b"}, "key_by": "static"}`, "key"},
		{"unknown field", `{"source": "mappings", "mappings": {"a": "b"}, "unknown_field": 1}`, "static-inject config"},
		{"invalid json", `{invalid}`, "static-inject config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := New()
			err := p.Configure(json.RawMessage(tt.config))
			if err == nil {
				t.Fatalf("Configure() error = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Configure() error = %q, want error containing %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestConfigure_FailurePreservesPreviousState(t *testing.T) {
	p := New()
	// First, a valid configure so the plugin has a committed cfg+resolver.
	if err := p.Configure(json.RawMessage(`{"source":"mappings","mappings":{"api.example.com":"REAL"},"key_by":"host"}`)); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	// Now reconfigure the SAME plugin with an invalid payload; it must fail
	// and must NOT clobber the previously committed cfg/resolver.
	if err := p.Configure(json.RawMessage(`{"source":"bogus"}`)); err == nil {
		t.Fatalf("Configure() error = nil, want error")
	}

	// The plugin must still behave exactly as it did under the first
	// (valid) configuration, proving Configure only commits state after
	// successful validation.
	pctx := &pipeline.Context{
		Host:    "api.example.com",
		Headers: http.Header{"Authorization": []string{"Bearer PLACEHOLDER"}},
	}
	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("OnRequest() after failed reconfigure action.Type = %v, want %v (violation: %+v)", action.Type, pipeline.Continue, action.Violation)
	}
	if got := pctx.Headers.Get("Authorization"); got != "Bearer REAL" {
		t.Errorf("Authorization header = %q, want %q (previous config preserved)", got, "Bearer REAL")
	}
}
