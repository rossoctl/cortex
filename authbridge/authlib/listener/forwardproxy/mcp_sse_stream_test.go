package forwardproxy

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
