package tlsbridge

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUpstreamClient_InjectedRootVerifies(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})

	// With the origin's CA injected → verifies.
	good, err := NewUpstreamClient(caPEM, false)
	if err != nil {
		t.Fatalf("NewUpstreamClient: %v", err)
	}
	resp, err := good.Get(srv.URL)
	if err != nil {
		t.Fatalf("expected success with injected root, got %v", err)
	}
	_ = resp.Body.Close()

	// Without it (system roots only) → the self-signed httptest cert is rejected.
	bare, _ := NewUpstreamClient(nil, false)
	if _, err := bare.Get(srv.URL); err == nil {
		t.Errorf("expected verification failure with system roots only")
	}

	// insecure=true → self-signed origin is accepted without any injected root.
	insecure, _ := NewUpstreamClient(nil, true)
	if _, err := insecure.Get(srv.URL); err != nil {
		t.Errorf("expected success with insecure=true, got %v", err)
	}
}

// Go's default MaxIdleConnsPerHost is 2. On an h1-only upstream (LiteLLM is one;
// it offers no h2 in ALPN) more than two concurrent callers then each open a
// fresh TCP+TLS per request, costing a handshake every time. The transport is
// sized to hold a per-host pool, so the caller count sets the connection count.
func TestUpstreamClient_PoolsIdleConnectionsPerHost(t *testing.T) {
	client, err := NewUpstreamClient(nil, true)
	if err != nil {
		t.Fatalf("NewUpstreamClient: %v", err)
	}
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", client.Transport)
	}
	if tr.MaxIdleConnsPerHost < 100 {
		t.Errorf("MaxIdleConnsPerHost = %d, want >= 100 (default of 2 churns connections on h1 upstreams)",
			tr.MaxIdleConnsPerHost)
	}
}
