package forwardproxy

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

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// streamingProbe is a minimal Plugin + StreamingResponder used to
// observe how the proxy dispatches frames. It records every frame
// it sees along with the last flag, so tests can assert on order
// and finalization. ReadsBody=true so the listener takes the
// NeedsBody path on requests. OnResponse calls are also counted so
// tests can verify the framework's "pick one path" contract for
// StreamingResponder plugins (OnResponse must NOT be called when
// OnResponseFrame is the dispatch path).
type streamingProbe struct {
	mu              sync.Mutex
	frames          [][]byte
	lasts           []bool
	onResponseCalls int
	caps            pipeline.PluginCapabilities
}

func newStreamingProbe(writesBody bool) *streamingProbe {
	return &streamingProbe{
		caps: pipeline.PluginCapabilities{
			ReadsBody:  true,
			WritesBody: writesBody,
		},
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

// TestForwardProxy_Streaming_FramesFlowThrough asserts that an upstream
// emitting SSE frames over time has those frames flushed downstream as
// they arrive — no full-body buffering, no 30s timeout firing, and
// each frame reaches the StreamingResponder plugin with last=false.
// A final last=true call is made at end-of-stream.
func TestForwardProxy_Streaming_FramesFlowThrough(t *testing.T) {
	// Upstream sends 3 SSE frames with small idle gaps. We use real
	// timers (not mock clocks) because the proxy reads off net/http
	// internals; the gaps are tens of milliseconds, well under any
	// timeout, and far longer than the per-frame work.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for i := 1; i <= 3; i++ {
			fmt.Fprintf(w, "data: {\"id\":%d}\n\n", i)
			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer upstream.Close()

	probe := newStreamingProbe(false)
	pipe, err := pipeline.New([]pipeline.Plugin{probe})
	if err != nil {
		t.Fatalf("New pipeline: %v", err)
	}
	srv, err := NewServer(pipeline.NewHolder(pipe), nil, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	proxyClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(mustParseURL(proxy.URL)),
		},
	}
	req, _ := http.NewRequest("GET", upstream.URL+"/stream", nil)
	resp, err := proxyClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	// Body should contain 3 SSE events.
	if got := bytes.Count(body, []byte("data:")); got != 3 {
		t.Errorf("body has %d data: lines, want 3 — body=%q", got, body)
	}

	frames, lasts := probe.snapshot()
	// We expect 3 frames + 1 last=true call.
	if len(frames) != 4 {
		t.Fatalf("plugin saw %d calls, want 4 (3 frames + 1 final) — frames=%v lasts=%v", len(frames), framesAsStrings(frames), lasts)
	}
	for i := 0; i < 3; i++ {
		if lasts[i] {
			t.Errorf("frame %d last=true, want false", i)
		}
		if !bytes.Contains(frames[i], []byte(fmt.Sprintf(`"id":%d`, i+1))) {
			t.Errorf("frame %d = %q, missing id %d", i, frames[i], i+1)
		}
	}
	if !lasts[3] {
		t.Error("final call last=false, want true")
	}
}

// TestForwardProxy_Streaming_WritesBodyFallsBackToBuffered asserts the
// safety guard: a pipeline with a WritesBody plugin can't take the
// streaming path (the plugin can't rewrite a body we've already
// started forwarding). The proxy logs a warning and falls back to
// buffered, so the response is delivered correctly even though it
// loses the streaming property.
func TestForwardProxy_Streaming_WritesBodyFallsBackToBuffered(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"id\":1}\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	probe := newStreamingProbe(true) // WritesBody=true → buffered fallback
	pipe, err := pipeline.New([]pipeline.Plugin{probe})
	if err != nil {
		t.Fatalf("New pipeline: %v", err)
	}
	srv, err := NewServer(pipeline.NewHolder(pipe), nil, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	proxyClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(mustParseURL(proxy.URL)),
		},
	}
	req, _ := http.NewRequest("GET", upstream.URL+"/stream", nil)
	resp, err := proxyClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte(`{"id":1}`)) {
		t.Errorf("body did not contain expected payload: %q", body)
	}
	// Buffered path: streaming-aware plugins still see one last=true
	// frame carrying the whole body. Sanity-check.
	_, lasts := probe.snapshot()
	if len(lasts) == 0 || !lasts[len(lasts)-1] {
		t.Errorf("last call lasts = %v; expected final last=true on buffered fallback", lasts)
	}
}

// TestForwardProxy_BufferedDeliversLastTrueFrame asserts the
// application/json one-shot contract: streaming-aware plugins receive
// the buffered body as a single OnResponseFrame call with last=true,
// so the same code path handles streaming and non-streaming responses.
func TestForwardProxy_BufferedDeliversLastTrueFrame(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	probe := newStreamingProbe(false)
	pipe, err := pipeline.New([]pipeline.Plugin{probe})
	if err != nil {
		t.Fatalf("New pipeline: %v", err)
	}
	srv, err := NewServer(pipeline.NewHolder(pipe), nil, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	proxyClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(mustParseURL(proxy.URL)),
		},
	}
	req, _ := http.NewRequest("GET", upstream.URL+"/oneshot", nil)
	resp, err := proxyClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	frames, lasts := probe.snapshot()
	if len(frames) != 1 || !lasts[0] {
		t.Fatalf("frames=%v lasts=%v; want exactly one call with last=true", framesAsStrings(frames), lasts)
	}
	if !bytes.Equal(frames[0], []byte(`{"ok":true}`)) {
		t.Errorf("frame[0] = %q, want JSON body", frames[0])
	}
	// Regression: a StreamingResponder plugin must NOT have OnResponse
	// called on the buffered application/json path — the framework
	// picks one path and the buffered last=true frame is it. Otherwise
	// migrated plugins would record every JSON response twice.
	if got := probe.onResponseCount(); got != 0 {
		t.Errorf("OnResponse called %d times; want 0 (StreamingResponder picks the OnResponseFrame path)", got)
	}
}

// TestForwardProxy_Streaming_HeadersAndBodyArriveBeforeUpstreamCloses
// is the regression test for #477 — the agent should see frames
// arriving as the upstream produces them, not after the upstream
// finishes. We assert that by reading the first frame from the
// proxy's response BEFORE the upstream writes the second frame, with
// the upstream gated on a channel.
func TestForwardProxy_Streaming_HeadersAndBodyArriveBeforeUpstreamCloses(t *testing.T) {
	gate := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"id\":1}\n\n")
		flusher.Flush()
		<-gate // hold the upstream open until the test releases it
		fmt.Fprintf(w, "data: {\"id\":2}\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()
	defer close(gate)

	probe := newStreamingProbe(false)
	pipe, err := pipeline.New([]pipeline.Plugin{probe})
	if err != nil {
		t.Fatalf("New pipeline: %v", err)
	}
	srv, err := NewServer(pipeline.NewHolder(pipe), nil, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	proxyClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(mustParseURL(proxy.URL)),
		},
	}
	req, _ := http.NewRequest("GET", upstream.URL+"/stream", nil)
	resp, err := proxyClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	// Read the first SSE event — must arrive even though upstream is
	// still holding open. If buffering had been left in place, this
	// read would block until upstream closes (and a 30s timeout would
	// likely fire — that was the bug).
	br := bufio.NewReader(resp.Body)
	deadline := time.Now().Add(2 * time.Second)
	var firstFrame []byte
	for time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("ReadString: %v", err)
		}
		if bytes.HasPrefix([]byte(line), []byte("data:")) {
			firstFrame = []byte(line)
			break
		}
	}
	if firstFrame == nil {
		t.Fatal("first frame did not arrive within 2s — proxy likely buffered")
	}
	if !bytes.Contains(firstFrame, []byte(`"id":1`)) {
		t.Errorf("first frame = %q, missing id 1", firstFrame)
	}
}

func framesAsStrings(frames [][]byte) []string {
	out := make([]string, len(frames))
	for i, f := range frames {
		out[i] = string(f)
	}
	return out
}
