package forwardproxy

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/session"
)

// TestForwardProxy_SSE_StreamsWithoutResponder is the regression test for
// issue #642: a generic (e.g. MCP Streamable HTTP) upstream returns
// text/event-stream, but the outbound pipeline has NO StreamingResponder
// plugin. Before the fix, such a response fell through to an unflushed
// io.Copy, so intermittent SSE events never reached the client until the
// upstream closed the connection — the agent timed out.
//
// Unlike TestForwardProxy_MCP_SSEResponse_RecordsObserve (which uses an
// upstream that writes one frame then RETURNS, so io.Copy sees EOF
// immediately and never exposes the buffering), this upstream flushes one
// event and then holds the connection OPEN. A buffering proxy delivers
// nothing until release; a flushing proxy delivers the event at once.
func TestForwardProxy_SSE_StreamsWithoutResponder(t *testing.T) {
	release := make(chan struct{})
	closed := false
	defer func() {
		if !closed {
			close(release)
		}
	}()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream ResponseWriter lacks http.Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// event: + id: exercise byte-faithful framing — the re-framing path
		// (handleStreamingResponse) would drop a non-allowlisted event name
		// and the id: line entirely.
		io.WriteString(w, "event: message\nid: 42\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"ok\":true}}\n\n")
		f.Flush()
		<-release // hold the stream open, like a live MCP server between events
	}))
	defer upstream.Close()

	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()

	// Empty pipeline: HasStreamingResponders()==false and WritesBody()==false,
	// so serveOutbound routes to streamPassthrough — the reporter's plain-proxy
	// shape (their only outbound plugin, token-exchange, is likewise not a
	// StreamingResponder).
	pipe, err := pipeline.New(nil)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	srv, err := NewServer(pipeline.NewHolder(pipe), store, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	req, _ := http.NewRequest("POST", upstream.URL+"/mcp", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)))
	req.Header.Set("Content-Type", "application/json")
	proxyClient := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL))}}
	resp, err := proxyClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	// Read the first full SSE frame in a goroutine, WHILE the upstream is still
	// blocked on <-release. A buffering proxy delivers nothing until release, so
	// this read blocks and the select hits the timeout — that is the #642
	// regression.
	type frameResult struct {
		data []byte
		err  error
	}
	got := make(chan frameResult, 1)
	go func() {
		br := bufio.NewReader(resp.Body)
		var acc []byte
		for {
			line, err := br.ReadBytes('\n')
			acc = append(acc, line...)
			if err != nil {
				got <- frameResult{acc, err}
				return
			}
			if bytes.HasSuffix(acc, []byte("\n\n")) {
				got <- frameResult{acc, nil}
				return
			}
		}
	}()

	select {
	case fr := <-got:
		if fr.err != nil && fr.err != io.EOF {
			t.Fatalf("reading first SSE frame: %v", fr.err)
		}
		frame := string(fr.data)
		// Byte-faithful framing: event: and id: must survive verbatim.
		if !strings.Contains(frame, "event: message") {
			t.Errorf("first frame missing 'event: message' (framing not preserved): %q", frame)
		}
		if !strings.Contains(frame, "id: 42") {
			t.Errorf("first frame missing 'id: 42' (framing not preserved): %q", frame)
		}
		if !strings.Contains(frame, `data: {"jsonrpc":"2.0"`) {
			t.Errorf("first frame missing/garbled data line: %q", frame)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the first SSE event while upstream held the connection open — proxy buffered the stream (regression of #642)")
	}

	// Let the upstream handler exit cleanly.
	close(release)
	closed = true
}

// responseProbePlugin is a minimal non-StreamingResponder plugin whose
// OnResponse uses only status/headers (like opa / litellm-budgettrack). It
// records that it ran and can deny — used to prove streamPassthrough still runs
// the response-phase pipeline before streaming. When readsBody is set it also
// declares ReadsBody, so it exercises the ReadsBody-but-not-StreamingResponder
// footgun (see TestForwardProxy_SSE_ReadsBodyPluginWarnsAndStreams).
type responseProbePlugin struct {
	deny      bool
	readsBody bool
	ranResp   atomic.Bool
	// sawBodyLen is the length of pctx.ResponseBody observed in OnResponse.
	sawBodyLen atomic.Int64
}

func (p *responseProbePlugin) Name() string { return "response-probe" }
func (p *responseProbePlugin) Capabilities() pipeline.PluginCapabilities {
	// Never a StreamingResponder → streamPassthrough path. ReadsBody is opt-in
	// so most callers stay on the status/header-only shape.
	return pipeline.PluginCapabilities{ReadsBody: p.readsBody}
}
func (p *responseProbePlugin) OnRequest(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *responseProbePlugin) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	p.ranResp.Store(true)
	p.sawBodyLen.Store(int64(len(pctx.ResponseBody)))
	if p.deny {
		return pipeline.DenyStatus(403, "test.denied", "denied by response probe")
	}
	return pipeline.Action{Type: pipeline.Continue}
}

// syncBuffer is a goroutine-safe io.Writer for capturing slog output written
// from the proxy's request-handler goroutine while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// sseProxy wires an SSE upstream + a forward proxy with the given pipeline and
// returns a client bound to the proxy. The upstream writes one event and
// returns (closes) — enough to exercise the response-phase decision.
func sseProxy(t *testing.T, plugins []pipeline.Plugin) (*http.Client, string) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "event: message\nid: 7\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(upstream.Close)

	store := session.New(5*time.Minute, 100, 0)
	t.Cleanup(store.Close)
	pipe, err := pipeline.New(plugins)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	srv, err := NewServer(pipeline.NewHolder(pipe), store, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(proxy.Close)

	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL))}}
	return client, upstream.URL + "/mcp"
}

// TestForwardProxy_SSE_ResponsePhaseDenyShortCircuits proves the fix keeps
// response-phase enforcement on streamed responses: a plugin that denies in
// OnResponse must short-circuit BEFORE any SSE byte is written.
func TestForwardProxy_SSE_ResponsePhaseDenyShortCircuits(t *testing.T) {
	probe := &responseProbePlugin{deny: true}
	client, url := sseProxy(t, []pipeline.Plugin{probe})

	req, _ := http.NewRequest("POST", url, bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if !probe.ranResp.Load() {
		t.Error("OnResponse did not run on the SSE response (RunResponse skipped)")
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (response-phase deny not honored)", resp.StatusCode)
	}
	if strings.Contains(string(body), "event: message") {
		t.Errorf("SSE body was forwarded despite the deny: %q", body)
	}
}

// TestForwardProxy_SSE_HeaderOnResponseRuns proves a non-denying, header-level
// OnResponse (e.g. cost accounting) still fires on an SSE response and the
// stream is delivered.
func TestForwardProxy_SSE_HeaderOnResponseRuns(t *testing.T) {
	probe := &responseProbePlugin{deny: false}
	client, url := sseProxy(t, []pipeline.Plugin{probe})

	req, _ := http.NewRequest("POST", url, bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if !probe.ranResp.Load() {
		t.Error("OnResponse did not run on the SSE response")
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "event: message") || !strings.Contains(string(body), "id: 7") {
		t.Errorf("SSE body not delivered verbatim: %q", body)
	}
}

// TestForwardProxy_SSE_ReadsBodyPluginWarnsAndStreams covers the ReadsBody
// footgun raised in review: a plugin that declares ReadsBody but is NOT a
// StreamingResponder falls into streamPassthrough. The proxy must NOT buffer to
// feed it a body (that would reintroduce the #642 timeout on a live stream), so
// the plugin's OnResponse sees an empty pctx.ResponseBody. The stream must still
// be delivered verbatim, and the misconfiguration must be surfaced via a warning
// rather than silently starving the plugin of the body.
func TestForwardProxy_SSE_ReadsBodyPluginWarnsAndStreams(t *testing.T) {
	// Capture slog output. The warning is emitted from the proxy's handler
	// goroutine, so use a mutex-guarded buffer to stay race-clean; tests in this
	// package don't run in parallel, so swapping the default logger is safe.
	logBuf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	probe := &responseProbePlugin{readsBody: true}
	client, url := sseProxy(t, []pipeline.Plugin{probe})

	req, _ := http.NewRequest("POST", url, bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if !probe.ranResp.Load() {
		t.Error("OnResponse did not run on the SSE response")
	}
	// The plugin declared ReadsBody, but the stream is passed through unbuffered,
	// so it sees no body — the documented limitation this warning surfaces.
	if n := probe.sawBodyLen.Load(); n != 0 {
		t.Errorf("ReadsBody plugin saw %d body bytes, want 0 (stream is not buffered for it)", n)
	}
	// The stream must still be delivered verbatim — no regression to the #642 timeout.
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "event: message") || !strings.Contains(string(body), "id: 7") {
		t.Errorf("SSE body not delivered verbatim: %q", body)
	}
	// The misconfiguration must be logged, not silent.
	if got := logBuf.String(); !strings.Contains(got, "ReadsBody plugin that is not a StreamingResponder") {
		t.Errorf("expected a warning about the ReadsBody+SSE misconfiguration, got logs: %q", got)
	}
}
