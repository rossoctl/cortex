package forwardproxy

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/inferenceparser"
	"github.com/rossoctl/cortex/core/session"
)

// anthropicSSEBody is a minimal but complete Anthropic Messages stream: the two
// usage-bearing events (message_start carries input_tokens, message_delta the
// cumulative output_tokens) plus one text delta, framed the way the real API
// frames them — a typed `event:` line before each `data:`.
const anthropicSSEBody = "event: message_start\n" +
	`data: {"type":"message_start","message":{"usage":{"input_tokens":31,"output_tokens":1}}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":12}}` + "\n\n"

// TestForwardProxy_GzippedSSE_ParsesTokens is the regression test for the
// gzipped-SSE blind spot: an upstream that returns text/event-stream WITH
// Content-Encoding: gzip.
//
// net/http auto-decompresses gzip only when its transport added
// Accept-Encoding itself. The proxy used to forward the caller's
// Accept-Encoding verbatim, which suppressed that and handed resp.Body raw
// compressed bytes. sseframe then found no "data:" lines in the binary and
// returned io.EOF on its first ReadFrame — so the proxy relayed ZERO frames
// downstream, and inference-parser finalized on empty state: every token
// count read as absent rather than wrong, and the client saw a stream that
// opened and died ("Streaming response ended before any complete data was
// received"). Both failures were silent; nothing logged an error.
//
// The client here sets Accept-Encoding explicitly, which is what a real agent
// does, so this fails against the pre-fix listener.
func TestForwardProxy_GzippedSSE_ParsesTokens(t *testing.T) {
	var sawAcceptEncoding string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAcceptEncoding = r.Header.Get("Accept-Encoding")

		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := io.WriteString(zw, anthropicSSEBody); err != nil {
			t.Errorf("gzip write: %v", err)
			return
		}
		if err := zw.Close(); err != nil {
			t.Errorf("gzip close: %v", err)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(buf.Bytes())
	}))
	defer upstream.Close()

	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()

	// The real inference-parser: a StreamingResponder, so serveOutbound routes
	// to handleStreamingResponse and each frame reaches OnResponseFrame.
	pipe, err := pipeline.New([]pipeline.Plugin{inferenceparser.NewInferenceParser()})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	srv, err := NewServer(pipeline.NewHolder(pipe), store, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	body := `{"model":"claude-sonnet-5","stream":true,` +
		`"messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequest("POST", upstream.URL+"/v1/messages", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// The header at the heart of the bug: an explicit Accept-Encoding is what
	// stops net/http from transparently decompressing for us.
	req.Header.Set("Accept-Encoding", "gzip")

	proxyClient := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL))}}
	resp, err := proxyClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	relayed, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read relayed body: %v", err)
	}

	// The upstream still sees Accept-Encoding: gzip — but now it is the
	// TRANSPORT's own header, added because we deleted the client's. That is
	// the distinction the fix turns on: net/http decompresses transparently
	// only for the Accept-Encoding it set itself, so the header being present
	// is expected, and the plaintext body below is what proves ownership
	// moved. Asserting on this string alone cannot separate the two cases.
	if sawAcceptEncoding == "" {
		t.Error("upstream saw no Accept-Encoding; expected the transport's own")
	}

	// The client must receive real SSE, not compressed bytes or nothing at all.
	// Zero relayed bytes is the exact pre-fix symptom.
	if len(relayed) == 0 {
		t.Fatal("proxy relayed zero bytes downstream — the client sees a stream that opens and dies")
	}
	// Assert the typed `event:` line, not just the type name: "message_start"
	// also appears inside the data payload, so matching the bare name passes
	// whether or not the event line survived. This form additionally guards a
	// separately-observed regression — the sseframe decoder surfaces only data
	// payloads, and Anthropic's client cannot finalize a stream without a typed
	// event line before each data line, so dropping it makes the client fall
	// back to a non-streaming retry and doubles the upstream call.
	if !bytes.Contains(relayed, []byte("event: message_start\n")) {
		t.Errorf("relayed body is missing the typed SSE event line; got %q", truncate(relayed))
	}
	// gzip's magic number: the relayed body must be plaintext, and the headers
	// must not advertise an encoding the bytes no longer carry.
	if len(relayed) >= 2 && relayed[0] == 0x1f && relayed[1] == 0x8b {
		t.Error("relayed body is still gzip-compressed")
	}
	if ce := resp.Header.Get("Content-Encoding"); ce != "" {
		t.Errorf("Content-Encoding = %q on a plaintext relayed body; want empty", ce)
	}

	// The whole point: usage from message_start + message_delta reached the
	// parser and landed on the session's response event.
	ev := lastInferenceResponse(t, store)
	if ev == nil {
		t.Fatal("no outbound SessionResponse event carrying inference telemetry")
	}
	if ev.PromptTokens != 31 {
		t.Errorf("PromptTokens = %d, want 31 (input_tokens from message_start)", ev.PromptTokens)
	}
	if ev.CompletionTokens != 12 {
		t.Errorf("CompletionTokens = %d, want 12 (cumulative output_tokens from message_delta)", ev.CompletionTokens)
	}
	if ev.FinishReason != "end_turn" {
		t.Errorf("FinishReason = %q, want end_turn", ev.FinishReason)
	}
	if ev.Completion != "hi" {
		t.Errorf("Completion = %q, want \"hi\"", ev.Completion)
	}

	// The session summary's TotalTokens is what abctl renders in its TOKENS
	// column, so assert the number an operator actually reads — not just the
	// parsed extension behind it.
	var total int
	for _, sum := range store.ListSessions() {
		total += sum.TotalTokens
	}
	if total != 43 {
		t.Errorf("session TotalTokens = %d, want 43 (31 prompt + 12 completion) — this is abctl's TOKENS column", total)
	}
}

// lastInferenceResponse returns the inference extension from the most recent
// outbound SessionResponse event that carries one, or nil.
func lastInferenceResponse(t *testing.T, store *session.Store) *pipeline.InferenceExtension {
	t.Helper()
	var found *pipeline.InferenceExtension
	for _, s := range store.ListSessions() {
		v := store.View(s.ID)
		if v == nil {
			continue
		}
		for i := range v.Events {
			e := v.Events[i]
			if e.Direction == pipeline.Outbound && e.Phase == pipeline.SessionResponse && e.Inference != nil {
				found = e.Inference
			}
		}
	}
	return found
}

func truncate(b []byte) string {
	const max = 200
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}
