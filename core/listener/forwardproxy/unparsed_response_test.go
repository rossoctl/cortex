package forwardproxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/inferenceparser"
)

// parserServer is a forward proxy running the inference parser, the body reader
// every local install has.
func parserServer(t *testing.T) *Server {
	t.Helper()
	pipe, err := pipeline.New([]pipeline.Plugin{inferenceparser.NewInferenceParser()})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	srv, err := NewServer(pipeline.NewHolder(pipe), nil, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv
}

// A response over the buffer limit relays whole when no parser claimed the
// request, and gets a 502 when one did.
func TestForwardProxy_BuffersOnlyParsedResponses(t *testing.T) {
	huge := bytes.Repeat([]byte("x"), maxBodySize+1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(huge)
	}))
	t.Cleanup(upstream.Close)
	proxy := httptest.NewServer(parserServer(t).Handler())
	t.Cleanup(proxy.Close)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL))}}

	resp, err := client.Get(upstream.URL + "/node.tar.xz")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || err != nil || n != int64(len(huge)) {
		t.Errorf("download: status %d, %d of %d bytes, err %v; want 200 and the whole body", resp.StatusCode, n, len(huge), err)
	}

	resp, err = client.Post(upstream.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("inference response: status %d, want 502", resp.StatusCode)
	}
}

// flushRecorder records the order of body writes and flushes.
type flushRecorder struct {
	header http.Header
	events []string
}

func (r *flushRecorder) Header() http.Header { return r.header }
func (r *flushRecorder) WriteHeader(int)     {}
func (r *flushRecorder) Write(b []byte) (int, error) {
	r.events = append(r.events, "write")
	return len(b), nil
}
func (r *flushRecorder) Flush() { r.events = append(r.events, "flush") }

// A stream that is not SSE is flushed to the client as it is relayed.
func TestForwardProxy_RelaysUnparsedStreamAsItArrives(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, "{\"n\":1}\n")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "{\"n\":2}\n")
	}))
	t.Cleanup(upstream.Close)

	rec := &flushRecorder{header: http.Header{}}
	parserServer(t).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, upstream.URL+"/api/chat", nil))
	if n := len(rec.events); n < 2 || rec.events[n-2] != "write" || rec.events[n-1] != "flush" {
		t.Errorf("events = %v, want each body write flushed", rec.events)
	}
}
