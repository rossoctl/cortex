package extproc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/staticinject"
)

// plaintextGateServer builds an ext_proc server whose outbound pipeline holds
// a static-inject configured with a credential for api.example.com — the #1481
// shape, driven here through Envoy's own message wire so what is pinned is the
// LISTENER's plumbing: the :scheme pseudo-header reaching pctx.Scheme, which
// the plugin's plaintext gate reads.
//
// The plugin-level behaviour is pinned in staticinject's own tests and the
// forward proxy's leg end to end; without this file the extproc leg was
// verified only by reading the source, so a dropped :scheme read would stop
// the envoy-sidecar shape of the gate firing while every one of those stayed
// green — exactly the silent regression a listener-wiring test exists to catch.
func plaintextGateServer(t *testing.T) *Server {
	t.Helper()
	inject := staticinject.New()
	cfg := `{
		"source": "mappings",
		"mappings": {"api.example.com": "REAL"},
		"key_by": "host",
		"placeholder": "PLACEHOLDER"
	}`
	if err := inject.Configure(json.RawMessage(cfg)); err != nil {
		t.Fatalf("static-inject Configure: %v", err)
	}
	p, err := pipeline.New([]pipeline.Plugin{inject})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	return &Server{OutboundPipeline: pipeline.NewHolder(p)}
}

// TestExtProc_Outbound_PlaintextCredentialRefusedOnSchemeHeader: a request
// whose :scheme pseudo-header says http, to a host the pipeline holds a
// credential for, is refused 403 plaintext_credential — the same conformance
// case TestForwardProxy_PlaintextCredentialRefused pins on the proxy-sidecar
// leg, asserted here on Envoy's message wire so the two deployment shapes are
// both pinned against the same regression.
func TestExtProc_Outbound_PlaintextCredentialRefusedOnSchemeHeader(t *testing.T) {
	srv := plaintextGateServer(t)

	stream := &mockStream{ctx: context.Background(), requests: []*extprocv3.ProcessingRequest{
		{
			Request: &extprocv3.ProcessingRequest_RequestHeaders{
				RequestHeaders: &extprocv3.HttpHeaders{Headers: makeHeaders(
					"x-authbridge-direction", "outbound",
					":method", "GET",
					":scheme", "http",
					":path", "/x",
					":authority", "api.example.com",
					"authorization", "Bearer PLACEHOLDER",
				)},
			},
		},
	}}
	_ = srv.Process(stream)

	if len(stream.responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.responses))
	}
	ir := stream.responses[0].GetImmediateResponse()
	if ir == nil {
		t.Fatalf("expected ImmediateResponse (deny), got %+v", stream.responses[0])
	}
	if ir.Status.Code != 403 {
		t.Errorf("status = %d, want 403", ir.Status.Code)
	}
	if !strings.Contains(string(ir.Body), "plaintext_credential") {
		t.Errorf("body = %q, want the plaintext_credential error code", ir.Body)
	}
}

// TestExtProc_Outbound_PlaintextGatePassesSchemeHTTPS: the same request with
// :scheme https injects exactly as before the gate existed — the SetHeaders
// mutation carries the credential — so this leg also pins that the gate does
// not over-fire on the envoy path.
func TestExtProc_Outbound_PlaintextGatePassesSchemeHTTPS(t *testing.T) {
	srv := plaintextGateServer(t)

	stream := &mockStream{ctx: context.Background(), requests: []*extprocv3.ProcessingRequest{
		{
			Request: &extprocv3.ProcessingRequest_RequestHeaders{
				RequestHeaders: &extprocv3.HttpHeaders{Headers: makeHeaders(
					"x-authbridge-direction", "outbound",
					":method", "GET",
					":scheme", "https",
					":path", "/x",
					":authority", "api.example.com",
					"authorization", "Bearer PLACEHOLDER",
				)},
			},
		},
	}}
	_ = srv.Process(stream)

	if len(stream.responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.responses))
	}
	resp := stream.responses[0]
	if ir := resp.GetImmediateResponse(); ir != nil {
		t.Fatalf("https request was refused: status %d body %q", ir.Status.Code, ir.Body)
	}
	// setHeaderValue is placeholder_test.go's accessor for the allow path's
	// header mutation; the injected credential must be on it.
	got, ok := setHeaderValue(resp, "authorization")
	if !ok {
		t.Fatalf("expected SetHeaders mutation for authorization, got %+v", resp)
	}
	if got != "Bearer REAL" {
		t.Errorf("authorization = %q, want %q (injected on https as before the gate)", got, "Bearer REAL")
	}
}
