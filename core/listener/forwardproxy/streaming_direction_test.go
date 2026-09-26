package forwardproxy

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

// TestForwardProxy_Streaming_RequestOnlyWriterKeepsStreaming is the point of
// the directional split. Before it, any plugin declaring the single WritesBody
// flag forfeited incremental SSE relay — including a plugin that only ever
// rewrites the *request*, for bytes it never touches. tool-prune and
// context-guru are exactly that shape.
//
// The assertion is the frame count: the streaming path delivers one call per
// frame plus a final last=true (4 for 3 frames), where the buffered path
// delivers a single last=true call. A regression that reattached the fallback
// to the request flag would collapse this to 1.
func TestForwardProxy_Streaming_RequestOnlyWriterKeepsStreaming(t *testing.T) {
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

	probe := newStreamingProbe(true) // WritesRequestBody only
	if probe.caps.WritesResponseBody {
		t.Fatal("probe must not declare WritesResponseBody for this test")
	}
	pipe, err := pipeline.New([]pipeline.Plugin{probe})
	if err != nil {
		t.Fatalf("New pipeline: %v", err)
	}
	if !pipe.WritesRequestBody() {
		t.Fatal("pipeline should report WritesRequestBody")
	}
	if pipe.WritesResponseBody() {
		t.Fatal("pipeline must NOT report WritesResponseBody")
	}

	srv, err := NewServer(pipeline.NewHolder(pipe), nil, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	proxyClient := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL))},
	}
	req, _ := http.NewRequest("GET", upstream.URL+"/stream", nil)
	resp, err := proxyClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
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
	for i := 0; i < 3; i++ {
		if lasts[i] {
			t.Errorf("frame %d last=true, want false", i)
		}
	}
	if !lasts[3] {
		t.Error("final call last=false, want true")
	}
}
