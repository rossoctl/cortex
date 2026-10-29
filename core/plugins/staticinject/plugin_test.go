package staticinject

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
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

func TestInjectHeader_XAPIKey(t *testing.T) {
	p := New()
	cfg := `{
		"source": "mappings",
		"mappings": {"api.example.com": "REALKEY"},
		"key_by": "host",
		"inject_header": "x-api-key"
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
	if got := pctx.Headers.Get("x-api-key"); got != "REALKEY" {
		t.Errorf("x-api-key header = %q, want %q (raw value, no Bearer prefix)", got, "REALKEY")
	}
	if got := pctx.Headers.Get("Authorization"); got != "" {
		t.Errorf("Authorization header = %q, want removed (empty)", got)
	}
}

func TestInjectHeader_DefaultUnchanged(t *testing.T) {
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
		t.Errorf("Authorization header = %q, want %q (default inject_header behavior unchanged)", got, "Bearer REAL")
	}
	if _, ok := pctx.Headers["X-Api-Key"]; ok {
		t.Errorf("x-api-key header should not be set when inject_header defaults to Authorization")
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

// TestDenyOnEmptyResolvedValue locks in fail-closed behavior when the resolver
// returns an empty credential. ReadCredentialFile trims a whitespace-only secret
// file to "" (ok=true), and an inline mapping may hold "" — both must deny rather
// than forward an empty "Bearer " header.
func TestDenyOnEmptyResolvedValue(t *testing.T) {
	t.Run("empty inline mapping value", func(t *testing.T) {
		p := New()
		cfg := `{
			"source": "mappings",
			"mappings": {"api.example.com": ""},
			"key_by": "host"
		}`
		if err := p.Configure(json.RawMessage(cfg)); err != nil {
			t.Fatalf("Configure() error = %v", err)
		}

		pctx := &pipeline.Context{
			Host:    "api.example.com",
			Headers: http.Header{"Authorization": []string{"Bearer PLACEHOLDER"}},
		}
		action := p.OnRequest(context.Background(), pctx)
		if action.Type != pipeline.Reject {
			t.Fatalf("OnRequest() action.Type = %v, want %v (Reject, empty credential)", action.Type, pipeline.Reject)
		}
		if got := pctx.Headers.Get("Authorization"); got != "Bearer PLACEHOLDER" {
			t.Errorf("Authorization header = %q, want unmodified %q", got, "Bearer PLACEHOLDER")
		}
	})

	t.Run("whitespace-only secret file", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "api.example.com"), []byte("   \n"), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}

		p := New()
		cfg := `{
			"source": "secret_dir",
			"secret_dir": "` + dir + `",
			"key_by": "host"
		}`
		if err := p.Configure(json.RawMessage(cfg)); err != nil {
			t.Fatalf("Configure() error = %v", err)
		}

		pctx := &pipeline.Context{
			Host:    "api.example.com",
			Headers: http.Header{"Authorization": []string{"Bearer PLACEHOLDER"}},
		}
		action := p.OnRequest(context.Background(), pctx)
		if action.Type != pipeline.Reject {
			t.Fatalf("OnRequest() action.Type = %v, want %v (Reject, whitespace-only file)", action.Type, pipeline.Reject)
		}
		if got := pctx.Headers.Get("Authorization"); got != "Bearer PLACEHOLDER" {
			t.Errorf("Authorization header = %q, want unmodified %q", got, "Bearer PLACEHOLDER")
		}
	})
}

// =============================================================================
// Plaintext-credential gate (#1481, MOCA MI1 §8.5)
// =============================================================================

// newPlaintextGatePlugin builds a configured plugin whose resolver holds a
// credential for api.example.com and whose placeholder matches what the
// conformance-shaped requests present.
func newPlaintextGatePlugin(t *testing.T) *StaticInject {
	t.Helper()
	p := New()
	cfg := `{
		"source": "mappings",
		"mappings": {"api.example.com": "REAL"},
		"key_by": "host",
		"placeholder": "PLACEHOLDER"
	}`
	if err := p.Configure(json.RawMessage(cfg)); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}
	return p
}

// TestDenyPlaintextCredential pins the contract case: a plain-HTTP request to
// a host that has a credential is refused 403 plaintext_credential, nothing
// is injected, and the refusal is recorded as an invocation so the denial is
// visible in the session timeline.
func TestDenyPlaintextCredential(t *testing.T) {
	p := newPlaintextGatePlugin(t)

	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Scheme:    "http",
		Host:      "api.example.com",
		Headers: http.Header{
			"Authorization": []string{"Bearer PLACEHOLDER"},
		},
	}

	action := p.OnRequest(context.Background(), pctx)

	if action.Type != pipeline.Reject {
		t.Fatalf("OnRequest() action.Type = %v, want %v (Reject)", action.Type, pipeline.Reject)
	}
	if action.Violation == nil {
		t.Fatalf("OnRequest() Violation = nil, want 403 plaintext_credential")
	}
	if status, _, _ := action.Violation.Render(); status != 403 {
		t.Errorf("Violation status = %d, want 403", status)
	}
	if action.Violation.Code != "plaintext_credential" {
		t.Errorf("Violation code = %q, want %q (MOCA contract identifier, verbatim)",
			action.Violation.Code, "plaintext_credential")
	}
	if got := pctx.Headers.Get("Authorization"); got != "Bearer PLACEHOLDER" {
		t.Errorf("Authorization header = %q, want unmodified %q (nothing injected)", got, "Bearer PLACEHOLDER")
	}
	// The refusal must be attributable in the invocation timeline, the same
	// way the conformance suite reads moca-egress's decision event. Record
	// buckets by Direction, so the outbound bucket is the one to read.
	inv := pctx.Extensions.Invocations
	if inv == nil {
		t.Fatalf("Extensions.Invocations = nil, want one outbound deny record")
	}
	if len(inv.Outbound) != 1 {
		t.Fatalf("outbound invocations length = %d, want 1 (the deny record)", len(inv.Outbound))
	}
	if inv.Outbound[0].Action != pipeline.ActionDeny || inv.Outbound[0].Reason != "plaintext_credential" {
		t.Errorf("invocation = {%s %q}, want {deny plaintext_credential}",
			inv.Outbound[0].Action, inv.Outbound[0].Reason)
	}
}

// TestPlaintextGateNamesTheCredentialNotTheScheme pins the negative space of
// the contract: plain HTTP to a host WITHOUT a credential is not refused by
// this gate — it keeps its normal (401 unresolved-key) treatment, leaving
// allow-list/SSRF policy to judge the request.
func TestPlaintextGateNamesTheCredentialNotTheScheme(t *testing.T) {
	p := newPlaintextGatePlugin(t)

	pctx := &pipeline.Context{
		// Outbound explicitly: Direction's zero value is Inbound, where the gate
		// deliberately does not fire, so without this the test would pass for the
		// wrong reason — on the direction rather than on the missing credential.
		Direction: pipeline.Outbound,
		Scheme:    "http",
		Host:      "no-credential.example.com",
		Headers: http.Header{
			"Authorization": []string{"Bearer PLACEHOLDER"},
		},
	}

	action := p.OnRequest(context.Background(), pctx)

	if action.Type != pipeline.Reject {
		t.Fatalf("OnRequest() action.Type = %v, want %v (Reject)", action.Type, pipeline.Reject)
	}
	if action.Violation == nil || action.Violation.Code != "static-inject.unresolved-key" {
		t.Errorf("Violation = %+v, want code %q (unresolved key, not plaintext_credential)",
			action.Violation, "static-inject.unresolved-key")
	}
}

// TestPlaintextGateAllowsHTTPS pins that a TLS-bridged request to a
// credentialed host injects exactly as before the gate existed.
func TestPlaintextGateAllowsHTTPS(t *testing.T) {
	p := newPlaintextGatePlugin(t)

	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Scheme:    "https",
		Host:      "api.example.com",
		Headers: http.Header{
			"Authorization": []string{"Bearer PLACEHOLDER"},
		},
	}

	action := p.OnRequest(context.Background(), pctx)

	if action.Type != pipeline.Continue {
		t.Fatalf("OnRequest() action.Type = %v, want %v (violation: %+v)",
			action.Type, pipeline.Continue, action.Violation)
	}
	if got := pctx.Headers.Get("Authorization"); got != "Bearer REAL" {
		t.Errorf("Authorization header = %q, want %q", got, "Bearer REAL")
	}
}

// INBOUND + http STILL INJECTS, and this is the test that keeps MOCA's AB1
// working. On the reverse proxy pctx.Scheme is the scheme the CLIENT used to
// reach Cortex (requestScheme, reverseproxy/server.go) — "http" whenever
// the inbound listener is not TLS. The credential leaves on the Cortex→backend
// hop instead, whose scheme comes from reverse_proxy_backend and cannot be read
// from here, so an inbound refusal keyed on Scheme judges the wrong hop.
//
// AB1 is exactly that shape: static-inject in the inbound pipeline behind a
// plaintext reverse_proxy_addr ":8080", forwarding to
// reverse_proxy_backend "https://api.anthropic.com". Gating on scheme alone
// refused every LLM call through it — reproduced end to end at 403 with zero
// upstream hits before the direction check (#1537 review).
func TestPlaintextGateDoesNotFireOnInbound(t *testing.T) {
	p := newPlaintextGatePlugin(t)

	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		// The client's hop, not the credential's: the backend may well be https.
		Scheme: "http",
		Host:   "api.example.com",
		Headers: http.Header{
			"Authorization": []string{"Bearer PLACEHOLDER"},
		},
	}

	action := p.OnRequest(context.Background(), pctx)

	if action.Type != pipeline.Continue {
		t.Fatalf("OnRequest() action.Type = %v, want %v — an inbound plaintext front says nothing about the backend hop the credential takes (violation: %+v)",
			action.Type, pipeline.Continue, action.Violation)
	}
	if got := pctx.Headers.Get("Authorization"); got != "Bearer REAL" {
		t.Errorf("Authorization header = %q, want %q — the credential must still be injected", got, "Bearer REAL")
	}
}

// TestPlaintextGateIgnoresUnknownScheme pins that an empty/unknown scheme is
// NOT treated as plaintext: Context.Scheme's contract says assuming "" means
// http would silently mask missing listener plumbing, so the gate denies only
// on a scheme the listener explicitly declared plaintext.
func TestPlaintextGateIgnoresUnknownScheme(t *testing.T) {
	for _, scheme := range []string{"", "tcp"} {
		t.Run("scheme="+scheme, func(t *testing.T) {
			p := newPlaintextGatePlugin(t)

			pctx := &pipeline.Context{
				Direction: pipeline.Outbound,
				Scheme:    scheme,
				Host:      "api.example.com",
				Headers: http.Header{
					"Authorization": []string{"Bearer PLACEHOLDER"},
				},
			}

			action := p.OnRequest(context.Background(), pctx)

			if action.Type != pipeline.Continue {
				t.Fatalf("OnRequest() action.Type = %v, want %v (violation: %+v)",
					action.Type, pipeline.Continue, action.Violation)
			}
			if got := pctx.Headers.Get("Authorization"); got != "Bearer REAL" {
				t.Errorf("Authorization header = %q, want %q", got, "Bearer REAL")
			}
		})
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
