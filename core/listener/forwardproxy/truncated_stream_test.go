package forwardproxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/listener/internal/sseframe"
	"github.com/rossoctl/cortex/core/listener/skiphost"
	"github.com/rossoctl/cortex/core/pipeline"
)

// abortingStream answers 200, writes one flushed chunk, then resets the TCP
// connection with SO_LINGER=0 — an upstream that breaks a streamed response
// partway. contentType picks the arm the proxy takes: text/event-stream hits
// the SSE relays, anything else the unbuffered io.Copy.
func abortingStream(contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"n\":1}\n\n")
		w.(http.Flusher).Flush()
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.(*net.TCPConn).SetLinger(0)
		_ = conn.Close()
	}
}

// readViaProxy fetches target through the proxy and returns the error the
// CLIENT saw while reading the body — the thing #1543 is about: a truncated
// upstream stream must reach the client as an error, not a clean end of body.
func readViaProxy(t *testing.T, proxy *httptest.Server, target string) error {
	t.Helper()
	c := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(mustParseURL(proxy.URL)),
		},
	}
	resp, err := c.Get(target)
	if err != nil {
		// A failure to even get headers is a client-visible error too —
		// the plain-body arm's reset can land before the client reads the
		// flushed headers. What must NOT happen is a clean GET plus a
		// clean body read off a truncated stream.
		return err
	}
	defer resp.Body.Close()
	_, readErr := io.ReadAll(resp.Body)
	return readErr
}

// TestForwardProxy_TruncatedStream_ReachesClientAsError: every relay arm the
// forward proxy takes for an open-ended (chunked) body — the SSE relay with a
// streaming responder, the SSE passthrough with none, and the plain unbuffered
// copy — used to return normally when the upstream broke mid-body, so net/http
// wrote the terminating chunk and the client read a COMPLETE 200 off a cut-off
// stream (#1543). Each arm must instead abort the client's connection, and the
// response row must carry the failure instead of a clean 200.
func TestForwardProxy_TruncatedStream_ReachesClientAsError(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		plugins     []pipeline.Plugin
	}{
		{name: "sse passthrough, no plugins", contentType: "text/event-stream"},
		{name: "sse relay, streaming responder", contentType: "text/event-stream", plugins: []pipeline.Plugin{newStreamingProbe(false)}},
		{name: "plain body, unbuffered copy", contentType: "application/json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(abortingStream(tc.contentType))
			defer up.Close()
			proxy, store := failureProxy(t, nil, tc.plugins...)

			if readErr := readViaProxy(t, proxy, up.URL); readErr == nil {
				t.Error("truncated upstream stream reached the client as a clean end of body")
			}

			row := onlyResponseEvent(t, store)
			if row.Error == nil {
				t.Fatalf("response row for a truncated stream carries no error (status %d) — agentop hides the failure", row.StatusCode)
			}
			if row.Error.Kind != "upstream_error" {
				t.Errorf("row error kind = %q, want upstream_error", row.Error.Kind)
			}
			if row.Error.Code != "200" {
				t.Errorf("row error code = %q, want the upstream's 200 as error.code", row.Error.Code)
			}
		})
	}
}

// The control: an upstream that ends its stream cleanly produces a clean read
// AND a row with no error. Pins that the fix above did not start aborting
// healthy streams.
func TestForwardProxy_CleanStreamStillReadsClean(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"n\":1}\n\n")
	}))
	defer up.Close()
	proxy, store := failureProxy(t, nil, newStreamingProbe(false))

	if readErr := readViaProxy(t, proxy, up.URL); readErr != nil {
		t.Errorf("clean upstream stream reached the client as an error: %v", readErr)
	}
	row := onlyResponseEvent(t, store)
	if row.Error != nil {
		t.Errorf("clean stream's row carries an error: %+v", row.Error)
	}
}

// skip_hosts promises no pipeline and no session row — but not a lying
// connection: a truncated skipped stream must still abort the client's
// connection, while recording nothing.
func TestForwardProxy_TruncatedSkippedStreamAbortsButRecordsNothing(t *testing.T) {
	up := httptest.NewServer(abortingStream("text/event-stream"))
	defer up.Close()
	skip := skiphostMustMatch(t, up.URL)
	proxy, store := failureProxy(t, skip)

	if readErr := readViaProxy(t, proxy, up.URL); readErr == nil {
		t.Error("truncated skipped stream reached the client as a clean end of body")
	}
	for _, ev := range allEvents(t, store) {
		if ev.Phase == pipeline.SessionResponse {
			t.Errorf("skip_hosts recorded a response row: %+v", ev)
		}
	}
}

// skiphostMustMatch builds a matcher covering the host of rawURL.
func skiphostMustMatch(t *testing.T, rawURL string) *skiphost.Matcher {
	t.Helper()
	u := mustParseURL(rawURL)
	m, err := skiphost.New([]string{u.Hostname()})
	if err != nil {
		t.Fatalf("skiphost.New: %v", err)
	}
	return m
}

// TestClassifyBrokenStream_ProxySideCuts: two of the read errors a relay arm
// sees are the proxy's own cuts, not the upstream's — the idle read cutoff
// and an SSE frame over the frame cap. Both must record proxy_error, the
// kind reserved for "the one that is ours", so an operator reading the row
// looks at the proxy rather than the network; abort stays true either way,
// because the client's stream is cut and must not read as complete.
// pctx.StatusCode must go back to zero on every classified cut, so the
// deferred RunFinish reports OutcomeError rather than an allow off the sent
// 200 — the same contract the buffered path's zero holds.
func TestClassifyBrokenStream_ProxySideCuts(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusOK}
	req, err := http.NewRequest(http.MethodGet, "http://up.example/stream", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	for _, tc := range []struct {
		name string
		err  error
		kind string
	}{
		{name: "idle cutoff", err: errIdleCut, kind: "proxy_error"},
		{name: "oversized frame", err: sseframe.ErrFrameTooLarge, kind: "proxy_error"},
		{name: "upstream reset", err: errors.New("read tcp 10.0.0.1:443->10.0.0.2:5000: read: connection reset by peer"), kind: "upstream_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pctx := &pipeline.Context{StatusCode: http.StatusOK}
			fail, abort := classifyBrokenStream(pctx, req, resp, tc.err)
			if fail.Kind != tc.kind {
				t.Errorf("kind = %q, want %q", fail.Kind, tc.kind)
			}
			if fail.Code != "200" {
				t.Errorf("error.code = %q, want the sent 200", fail.Code)
			}
			if !abort {
				t.Error("abort = false, want the client connection reset")
			}
			if pctx.StatusCode != 0 {
				t.Errorf("pctx.StatusCode = %d, want 0 so the outcome reports an error, not an allow off the sent 200", pctx.StatusCode)
			}
		})
	}

	// The client hung up first: nobody left to abort for.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pctx := &pipeline.Context{StatusCode: http.StatusOK}
	fail, abort := classifyBrokenStream(pctx, req.WithContext(ctx), resp, context.Canceled)
	if abort {
		t.Error("abort = true on a client hangup")
	}
	if fail.Kind != "client_canceled" {
		t.Errorf("kind = %q, want client_canceled", fail.Kind)
	}
	if pctx.StatusCode != 0 {
		t.Errorf("pctx.StatusCode = %d, want 0 on a client hangup too, matching the buffered path", pctx.StatusCode)
	}
}

// TestIdleReader_CutSurfacesSentinel: when the idle timer — not the upstream —
// closes the body, the read error must be errIdleCut, the sentinel
// classifyBrokenStream keys on to record proxy_error. A read that succeeds
// before the timer must come back untouched.
func TestIdleReader_CutSurfacesSentinel(t *testing.T) {
	pr, pw := net.Pipe()
	defer func() { _ = pw.Close() }()
	defer func() { _ = pr.Close() }()

	// Healthy read: data arrives, the timer never fires, no sentinel.
	go func() { _, _ = pw.Write([]byte("data")) }()
	healthy := idleReader(pr, time.Minute)
	n, err := healthy.Read(make([]byte, 4))
	if err != nil || n != 4 {
		t.Fatalf("healthy read = %d, %v; want 4, nil", n, err)
	}

	// Idle cut: nothing arrives, the timer closes the body.
	idle := idleReader(pr, 20*time.Millisecond)
	if _, err := idle.Read(make([]byte, 4)); !errors.Is(err, errIdleCut) {
		t.Errorf("read error after the idle cutoff = %v, want errIdleCut", err)
	}
}
