package forwardproxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"

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

// flushRecorder records the body written so far at each flush that carries body
// bytes, and whether any write went unflushed.
type flushRecorder struct {
	header    http.Header
	body      []byte
	flushed   []string
	unflushed bool
	first     chan struct{} // closed at the first flush of body bytes
}

func (r *flushRecorder) Header() http.Header { return r.header }
func (r *flushRecorder) WriteHeader(int)     {}
func (r *flushRecorder) Write(b []byte) (int, error) {
	r.body = append(r.body, b...)
	r.unflushed = true
	return len(b), nil
}
func (r *flushRecorder) Flush() {
	if len(r.body) == 0 {
		return
	}
	if len(r.flushed) == 0 {
		close(r.first)
	}
	r.flushed = append(r.flushed, string(r.body))
	r.unflushed = false
}

// A stream that is not SSE reaches the client as it arrives: the upstream sends
// its second line only after the client was flushed the first. The upstream is
// an in-memory pipe in a synctest bubble, so a proxy that waits for the end of
// the stream deadlocks the bubble and fails at once.
func TestForwardProxy_RelaysUnparsedStreamAsItArrives(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const line1, line2 = "{\"n\":1}\n", "{\"n\":2}\n"
		rec := &flushRecorder{header: http.Header{}, first: make(chan struct{})}
		pr, pw := io.Pipe()
		go func() {
			_, _ = io.WriteString(pw, line1)
			<-rec.first
			_, _ = io.WriteString(pw, line2)
			_ = pw.Close()
		}()
		srv := parserServer(t)
		srv.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode:    http.StatusOK,
				Header:        http.Header{"Content-Type": {"application/x-ndjson"}},
				Body:          pr,
				ContentLength: -1,
				Request:       r,
			}, nil
		})}

		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://ollama.test/api/chat", nil))
		if len(rec.flushed) == 0 || rec.flushed[0] != line1 || string(rec.body) != line1+line2 || rec.unflushed {
			t.Errorf("flushed %q of body %q (unflushed tail: %v), want %q flushed first and every write flushed",
				rec.flushed, rec.body, rec.unflushed, line1)
		}
	})
}
