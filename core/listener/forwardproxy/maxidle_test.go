package forwardproxy

import (
	"net/http"
	"testing"

	"github.com/rossoctl/cortex/core/auth"
)

// Go's default MaxIdleConnsPerHost is 2, so more than two concurrent requests
// to one upstream open a fresh TCP+TLS each — on an h1-only gateway (LiteLLM
// is one; it offers no h2 in ALPN) that is a per-request handshake. The
// forward proxy sizes a per-host pool so the connection count follows the
// caller count rather than the request count.
func TestForwardProxyTransport_PoolsIdleConnectionsPerHost(t *testing.T) {
	srv, err := NewServer(outboundPipelineFromAuth(t, auth.New(auth.Config{})), nil, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	tr, ok := srv.Client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", srv.Client.Transport)
	}
	if tr.MaxIdleConnsPerHost < 100 {
		t.Errorf("MaxIdleConnsPerHost = %d, want >= 100 (default of 2 churns connections on h1 upstreams)",
			tr.MaxIdleConnsPerHost)
	}
}
