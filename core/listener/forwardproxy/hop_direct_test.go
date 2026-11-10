package forwardproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/rossoctl/cortex/core/attribution"
	"github.com/rossoctl/cortex/core/plugins/inferenceparser"
)

// With Hop nil, none of the five hop branch points in serveOutbound changes a thing:
// rows and captures as before, the client's hop headers reach the origin, Go's own
// Accept-Encoding, and the origin gets the dial.
func TestServeOutbound_DirectModeBranchPoints(t *testing.T) {
	const jsonAnswer = `{"id":"m1","type":"message","role":"assistant","model":"claude-x","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":3,"output_tokens":1}}`
	const sseAnswer = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m2\",\"model\":\"claude-x\",\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	var mu sync.Mutex
	var seen []http.Header
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		if r.URL.Query().Get("stream") == "1" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, sseAnswer)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, jsonAnswer)
	}))
	defer origin.Close()
	proxy, rec := proxyWithCaptures(t, inferenceparser.NewInferenceParser())

	hopHeaders := http.Header{
		attribution.HeaderAttribution: {"client-attr"},
		headerDest:                    {"http://client.example:1"},
		headerToken:                   {"client-token"},
		headerRequestID:               {"client-id"},
		headerProbe:                   {"1"},
	}
	for _, q := range []string{"", "?stream=1"} {
		postThrough(t, proxy, origin.URL+"/v1/messages"+q, `{"model":"claude-x","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`, hopHeaders)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("origin got %d requests, want 2", len(seen))
	}
	for i, h := range seen {
		for k, v := range hopHeaders {
			if got := h.Get(k); got != v[0] {
				t.Errorf("request %d: origin %s = %q, want the client's %q", i, k, got, v[0])
			}
		}
		if ae := h.Get("Accept-Encoding"); ae != "gzip" {
			t.Errorf("request %d: origin Accept-Encoding = %q, want Go's gzip", i, ae)
		}
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.got) != 2 || len(rec.res) != 2 {
		t.Fatalf("captures: %d request, %d response; want 2 and 2", len(rec.got), len(rec.res))
	}
	for id := range rec.got {
		if rec.res[id] == nil {
			t.Errorf("request %s has no response capture", id)
		}
	}
}
