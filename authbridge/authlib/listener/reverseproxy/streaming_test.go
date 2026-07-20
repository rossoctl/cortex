package reverseproxy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/rossocortex/authbridge/authlib/pipeline"
)

// streamingProbe is a Plugin + StreamingResponder for asserting on
// what the reverseproxy dispatches. ReadsBody=true so the listener
// takes the NeedsBody buffering path on requests. OnResponse calls
// are counted so tests can verify the framework's "pick one path"
// contract for StreamingResponder plugins.
type streamingProbe struct {
	mu              sync.Mutex
	frames          [][]byte
	lasts           []bool
	onResponseCalls int
	caps            pipeline.PluginCapabilities
}

func newStreamingProbe(writesBody bool) *streamingProbe {
	return &streamingProbe{
		caps: pipeline.PluginCapabilities{ReadsBody: true, WritesBody: writesBody},
	}
}

func (p *streamingProbe) Name() string                              { return "streaming-probe" }
func (p *streamingProbe) Capabilities() pipeline.PluginCapabilities { return p.caps }
func (p *streamingProbe) OnRequest(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *streamingProbe) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	p.mu.Lock()
	p.onResponseCalls++
	p.mu.Unlock()
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *streamingProbe) OnResponseFrame(_ context.Context, _ *pipeline.Context, frame []byte, last bool) pipeline.Action {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := make([]byte, len(frame))
	copy(cp, frame)
	p.frames = append(p.frames, cp)
	p.lasts = append(p.lasts, last)
	return pipeline.Action{Type: pipeline.Continue}
}

func (p *streamingProbe) snapshot() ([][]byte, []bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	frames := make([][]byte, len(p.frames))
	copy(frames, p.frames)
	lasts := make([]bool, len(p.lasts))
	copy(lasts, p.lasts)
	return frames, lasts
}

func (p *streamingProbe) onResponseCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.onResponseCalls
}

// TestReverseProxy_Streaming_FramesFlowThrough asserts the inbound
// streaming path: an A2A message/stream-style SSE upstream is
// forwarded frame-by-frame to the downstream client and each frame
// reaches the StreamingResponder hook. Mirrors the forwardproxy
// integration test so the contract is shared across both listeners.
func TestReverseProxy_Streaming_FramesFlowThrough(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for i := 1; i <= 3; i++ {
			fmt.Fprintf(w, "data: {\"event\":%d}\n\n", i)
			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer backend.Close()

	probe := newStreamingProbe(false)
	pipe, err := pipeline.New([]pipeline.Plugin{probe})
	if err != nil {
		t.Fatalf("New pipeline: %v", err)
	}
	srv, err := NewServer(pipeline.NewHolder(pipe), nil, backend.URL, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/stream")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if got := bytes.Count(body, []byte("data:")); got != 3 {
		t.Errorf("body has %d data: lines, want 3 — body=%q", got, body)
	}

	frames, lasts := probe.snapshot()
	if len(frames) != 4 {
		t.Fatalf("plugin saw %d calls, want 4 (3 frames + 1 final last=true) — frames=%v lasts=%v", len(frames), framesAsStrings(frames), lasts)
	}
	if !lasts[3] {
		t.Error("final call last=false, want true")
	}
}

// TestReverseProxy_Streaming_FlushesBeforeUpstreamCloses asserts that
// an in-flight client read sees the first SSE event before the
// upstream emits the second — i.e. the proxy is not buffering. Same
// regression test as forwardproxy's streaming counterpart, this time
// for the inbound path.
func TestReverseProxy_Streaming_FlushesBeforeUpstreamCloses(t *testing.T) {
	gate := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"id\":1}\n\n")
		flusher.Flush()
		<-gate
		fmt.Fprintf(w, "data: {\"id\":2}\n\n")
		flusher.Flush()
	}))
	defer backend.Close()
	defer close(gate)

	probe := newStreamingProbe(false)
	pipe, err := pipeline.New([]pipeline.Plugin{probe})
	if err != nil {
		t.Fatalf("New pipeline: %v", err)
	}
	srv, err := NewServer(pipeline.NewHolder(pipe), nil, backend.URL, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/stream")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	deadline := time.Now().Add(2 * time.Second)
	var first []byte
	for time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("ReadString: %v", err)
		}
		if bytes.HasPrefix([]byte(line), []byte("data:")) {
			first = []byte(line)
			break
		}
	}
	if first == nil {
		t.Fatal("first frame did not arrive within 2s — proxy buffering")
	}
	if !bytes.Contains(first, []byte(`"id":1`)) {
		t.Errorf("first frame = %q, missing id 1", first)
	}
}

// TestReverseProxy_BufferedDeliversLastTrueFrame: streaming-aware
// plugin sees a single OnResponseFrame call with last=true for an
// application/json response, so plugins use one code path.
func TestReverseProxy_BufferedDeliversLastTrueFrame(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"hello":"world"}`))
	}))
	defer backend.Close()

	probe := newStreamingProbe(false)
	pipe, _ := pipeline.New([]pipeline.Plugin{probe})
	srv, err := NewServer(pipeline.NewHolder(pipe), nil, backend.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/oneshot")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	frames, lasts := probe.snapshot()
	if len(frames) != 1 || !lasts[0] {
		t.Fatalf("frames=%v lasts=%v; want single last=true call", framesAsStrings(frames), lasts)
	}
	// Regression: a StreamingResponder plugin must NOT have OnResponse
	// called on the buffered application/json path — the framework
	// picks one path so the same body isn't recorded twice.
	if got := probe.onResponseCount(); got != 0 {
		t.Errorf("OnResponse called %d times; want 0 (StreamingResponder picks the OnResponseFrame path)", got)
	}
}

func framesAsStrings(frames [][]byte) []string {
	out := make([]string, len(frames))
	for i, f := range frames {
		out[i] = string(f)
	}
	return out
}

// TestReverseProxy_Streaming_PreservesEventLine is the regression
// guard for the SSE "event:" line bug: a client reading through the
// proxy must see the upstream's "event: <type>" line reproduced
// before each re-framed "data:" line, not just the bare data payload.
// The Anthropic SDK types each event from the "event:" field, so
// dropping it made downstream clients decode zero typed events even
// though the data bytes were intact.
func TestReverseProxy_Streaming_PreservesEventLine(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		flusher.Flush()
		fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n")
		flusher.Flush()
	}))
	defer backend.Close()

	p, err := pipeline.New(nil)
	if err != nil {
		t.Fatalf("New pipeline: %v", err)
	}
	srv, err := NewServer(pipeline.NewHolder(p), nil, backend.URL, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/stream")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Contains(body, []byte("event: message_start\n")) {
		t.Errorf("body missing %q — event: line not preserved; body=%q", "event: message_start\n", body)
	}
	if !bytes.Contains(body, []byte("event: content_block_delta\n")) {
		t.Errorf("body missing %q — event: line not preserved; body=%q", "event: content_block_delta\n", body)
	}
	if got := bytes.Count(body, []byte("data:")); got != 2 {
		t.Errorf("body has %d data: lines, want 2 — body=%q", got, body)
	}
}

// roundTripFunc adapts a function to http.RoundTripper, letting a test
// substitute the reverse proxy's outbound Transport with one that
// inspects the exact request the Director produced — bypassing net/http.
// Transport's own automatic Accept-Encoding negotiation, which adds
// "gzip" on the wire whenever the header is unset and would otherwise
// make an unset header indistinguishable from a deleted one at a real
// backend.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// TestReverseProxy_StripsAcceptEncoding is the regression guard for
// the gzip bug: when a plugin inspects the response body, the Director
// must strip the client's Accept-Encoding so Go's transport negotiates
// (and transparently decompresses) gzip itself. Leaving an explicit
// "Accept-Encoding: gzip" through produced a gzipped body the SSE
// re-framer read as garbage plus a Content-Encoding: gzip header over
// re-emitted plaintext. The pipeline here carries a body-inspecting
// StreamingResponder (streamingProbe, ReadsBody=true) so the strip is
// in scope.
//
// This asserts against the outbound request the Director hands to the
// Transport rather than what a real backend observes on the wire:
// net/http.Transport itself adds "Accept-Encoding: gzip" to the wire
// request whenever the header is unset (to negotiate transparent
// decompression), so a real backend sees "gzip" either way — the
// difference the fix controls is whether that header is explicit
// (caller-set, decompression left to the caller) or absent (Transport
// negotiates and decompresses transparently). Substituting the
// Transport lets the test observe that distinction directly.
func TestReverseProxy_StripsAcceptEncoding(t *testing.T) {
	var gotAcceptEncoding string
	var sawHeader bool

	p, err := pipeline.New([]pipeline.Plugin{newStreamingProbe(false)})
	if err != nil {
		t.Fatalf("New pipeline: %v", err)
	}
	srv, err := NewServer(pipeline.NewHolder(p), nil, "http://backend.invalid", nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.proxy.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotAcceptEncoding = req.Header.Get("Accept-Encoding")
		_, sawHeader = req.Header["Accept-Encoding"]
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    req,
		}, nil
	})
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	req, err := http.NewRequest("GET", proxy.URL+"/api/data", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()

	if sawHeader {
		t.Errorf("outbound request Accept-Encoding = %q, want stripped (Director did not delete it)", gotAcceptEncoding)
	}
}

// TestReverseProxy_PreservesAcceptEncoding_PassThrough is the flip side
// of the strip: when no plugin reads the response body, the proxy is a
// pure pass-through and must NOT strip Accept-Encoding — forcing
// upstream→client decompression there is a needless regression (matters
// for a remote backend or large non-streamed bodies). The pipeline is
// empty, so neither NeedsBody() nor HasStreamingResponders() holds and
// the Director leaves the caller's explicit header intact.
func TestReverseProxy_PreservesAcceptEncoding_PassThrough(t *testing.T) {
	var gotAcceptEncoding string

	p, err := pipeline.New(nil)
	if err != nil {
		t.Fatalf("New pipeline: %v", err)
	}
	srv, err := NewServer(pipeline.NewHolder(p), nil, "http://backend.invalid", nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.proxy.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotAcceptEncoding = req.Header.Get("Accept-Encoding")
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    req,
		}, nil
	})
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	req, err := http.NewRequest("GET", proxy.URL+"/api/data", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()

	if gotAcceptEncoding != "gzip" {
		t.Errorf("outbound request Accept-Encoding = %q, want %q preserved (pure pass-through must not strip)", gotAcceptEncoding, "gzip")
	}
}
