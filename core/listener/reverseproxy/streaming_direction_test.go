package reverseproxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// newResponseWritingProbe is a probe shaped like sparc / cpex: it rewrites the
// upstream response, so it genuinely cannot stream.
func newResponseWritingProbe() *streamingProbe {
	return &streamingProbe{
		caps: pipeline.PluginCapabilities{
			ReadsBody:          true,
			WritesResponseBody: true,
		},
	}
}

func sseBackend(t *testing.T, frames int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for i := 1; i <= frames; i++ {
			fmt.Fprintf(w, "data: {\"event\":%d}\n\n", i)
			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
}

func serveWith(t *testing.T, p *streamingProbe, backendURL string) *httptest.Server {
	t.Helper()
	pipe, err := pipeline.New([]pipeline.Plugin{p})
	if err != nil {
		t.Fatalf("New pipeline: %v", err)
	}
	srv, err := NewServer(pipeline.NewHolder(pipe), nil, backendURL, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return httptest.NewServer(srv.Handler())
}

// TestReverseProxy_Streaming_RequestOnlyWriterKeepsStreaming mirrors the
// forward-proxy case: a plugin that rewrites only the request must not cost
// the inbound listener incremental SSE relay.
func TestReverseProxy_Streaming_RequestOnlyWriterKeepsStreaming(t *testing.T) {
	backend := sseBackend(t, 3)
	defer backend.Close()

	probe := newStreamingProbe(true) // WritesRequestBody only
	proxy := serveWith(t, probe, backend.URL)
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
	if got := bytes.Count(body, []byte("data:")); got != 3 {
		t.Errorf("body has %d data: lines, want 3 — body=%q", got, body)
	}

	frames, lasts := probe.snapshot()
	if len(frames) != 4 {
		t.Fatalf("plugin saw %d calls, want 4 (3 frames + final) — a request-only writer must keep streaming; lasts=%v", len(frames), lasts)
	}
	if !lasts[3] {
		t.Error("final call last=false, want true")
	}
}

// TestReverseProxy_Streaming_WritesResponseBodyFallsBackToBuffered asserts the
// safety guard still holds for the direction that actually needs it: a
// response mutator forfeits streaming and receives one buffered delivery.
func TestReverseProxy_Streaming_WritesResponseBodyFallsBackToBuffered(t *testing.T) {
	backend := sseBackend(t, 3)
	defer backend.Close()

	probe := newResponseWritingProbe()
	proxy := serveWith(t, probe, backend.URL)
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
	// The buffered path still delivers every byte, just not incrementally.
	if got := bytes.Count(body, []byte("data:")); got != 3 {
		t.Errorf("body has %d data: lines, want 3 — body=%q", got, body)
	}

	frames, lasts := probe.snapshot()
	if len(frames) != 1 {
		t.Fatalf("plugin saw %d calls, want exactly 1 on the buffered path — lasts=%v", len(frames), lasts)
	}
	if !lasts[0] {
		t.Errorf("buffered delivery lasts = %v, want [true]", lasts)
	}
}
