package reverseproxy

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestReverseProxy_TruncatedStream_ReachesClientAsError is the parity control
// for #1543/#1552: the forward proxy used to end a stream the upstream broke
// mid-body as a clean, complete 200, and the reverse proxy did not —
// httputil.ReverseProxy aborts the client connection on an upstream copy
// error. Pinned here so the two listeners cannot drift apart on exactly this
// behavior, in either direction.
func TestReverseProxy_TruncatedStream_ReachesClientAsError(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"n\":1}\n\n")
		w.(http.Flusher).Flush()
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.(*net.TCPConn).SetLinger(0)
		_ = conn.Close()
	}))
	defer backend.Close()

	proxy := serveWith(t, newStreamingProbe(false), backend.URL)
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/stream")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Error("truncated upstream stream reached the client as a clean end of body — the reverse proxy must abort, as the forward proxy now does (#1543)")
	}
}
