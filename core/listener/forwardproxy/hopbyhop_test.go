package forwardproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// The tests in this file pin issue #1567 item 1. Hop-by-hop headers describe ONE
// connection and must not be forwarded. The four response-header copy loops used to
// relay them verbatim — requests were stripped (stripForUpstream) but responses were
// not, and a comment on one of the loops even claimed the stripping happened. Through
// the TLS bridge an HTTP/1.1 origin's Keep-Alive reaches an HTTP/2 client inside a
// HEADERS frame, which Node's http2 rejects with ERR_HTTP2_STREAM_ERROR.

// hopByHopUpstream answers every request with a body carrying the headers under test.
func hopByHopUpstream(t *testing.T, contentType string) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		// A hop-by-hop header by name...
		w.Header().Set("Keep-Alive", "timeout=5")
		// ...and one made hop-by-hop by being named in Connection.
		w.Header().Set("Connection", "X-Hop-Extra")
		w.Header().Set("X-Hop-Extra", "hop value")
		// And one that must survive.
		w.Header().Set("X-Normal", "kept")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "{}")
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

// proxyGet fetches url through a plain forward proxy (no plugins) and returns the
// response the proxy handed the client, body consumed.
func proxyGet(t *testing.T, url string) *http.Response {
	t.Helper()
	pipe, err := pipeline.New(nil)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	srv, err := NewServer(pipeline.NewHolder(pipe), nil, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(proxy.Close)
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL))},
		Timeout:   10 * time.Second,
	}
	resp, err := client.Get(url + "/data")
	if err != nil {
		t.Fatalf("GET through the proxy: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp
}

func assertNoHopByHop(t *testing.T, resp *http.Response) {
	t.Helper()
	if v := resp.Header.Get("Keep-Alive"); v != "" {
		t.Errorf("Keep-Alive = %q, want absent — hop-by-hop headers describe one connection "+
			"and must not be forwarded to the client", v)
	}
	if v := resp.Header.Get("X-Hop-Extra"); v != "" {
		t.Errorf("X-Hop-Extra = %q, want absent — a header named in Connection is hop-by-hop "+
			"for this connection, whatever its usual meaning", v)
	}
	if v := resp.Header.Get("X-Normal"); v != "kept" {
		t.Errorf("X-Normal = %q, want %q — end-to-end headers must still be forwarded", v, "kept")
	}
}

func TestForwardProxy_HopByHopResponseHeadersStripped(t *testing.T) {
	resp := proxyGet(t, hopByHopUpstream(t, "application/json").URL)
	assertNoHopByHop(t, resp)
}

// The streaming path has its own copy loop (streamPassthrough); the same rule must
// hold there. No StreamingResponder is configured, so the passthrough loop is the
// one that runs.
func TestForwardProxy_HopByHopResponseHeadersStrippedOnStream(t *testing.T) {
	resp := proxyGet(t, hopByHopUpstream(t, "text/event-stream").URL)
	assertNoHopByHop(t, resp)
}
