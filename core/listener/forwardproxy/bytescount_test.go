package forwardproxy

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// bytesOf drives one request through a forward proxy built on the given plugins
// and returns the byte counts the request and response rows reported (#1309).
//
// Returned as the two rows rather than four numbers because the split is the
// contract: a request row counts what was forwarded and nothing else, a response
// row counts what came back and nothing else, and a listener that put both on one
// row would read as a round trip that never happened.
func bytesOf(t *testing.T, plugins []pipeline.Plugin, req *http.Request, upstream http.HandlerFunc) (reqRow, respRow pipeline.SessionEvent) {
	t.Helper()
	p, err := pipeline.New(plugins)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	backend := httptest.NewServer(upstream)
	defer backend.Close()

	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()

	srv := &Server{OutboundPipeline: pipeline.NewHolder(p), Sessions: store, Client: http.DefaultClient}
	proxy := httptest.NewServer(srv.Handler())
	defer proxy.Close()

	// The caller builds the request against a placeholder host so it can be
	// written before the backend exists; retarget it here.
	target := backend.URL + req.URL.Path
	retargeted, err := http.NewRequest(req.Method, target, req.Body)
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	retargeted.Header = req.Header
	retargeted.ContentLength = req.ContentLength

	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL))}}
	resp, err := client.Do(retargeted)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	// Drained, not just closed: the streamed arms count as the relay reads, so an
	// undrained body would under-report by whatever was still in flight.
	drainAndClose(t, resp)

	// The row is recorded after the relay (io.Copy or the streaming arms' loop),
	// which can finish after the client has drained and returned — wait for it
	// rather than racing the recording.
	waitForResponseEvent(t, store)

	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) != 2 {
		t.Fatalf("want a request row and a response row, got %+v", v)
	}
	return v.Events[0], v.Events[1]
}

func drainAndClose(t *testing.T, resp *http.Response) {
	t.Helper()
	buf := make([]byte, 4096)
	for {
		if _, err := resp.Body.Read(buf); err != nil {
			break
		}
	}
	resp.Body.Close()
}

// TestBytes_BufferedPairCountsEachDirectionOnItsOwnRow is the forward proxy's own
// anchor for the figures the parity suite compares across listeners: with a body
// plugin in the chain, both directions are buffered and both counts are exact.
func TestBytes_BufferedPairCountsEachDirectionOnItsOwnRow(t *testing.T) {
	const reqBody = `{"method":"tools/call","id":1}`
	const respBody = `{"result":"ok"}`

	req, _ := http.NewRequest("POST", "http://placeholder/mcp", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")

	reqRow, respRow := bytesOf(t, []pipeline.Plugin{&bodyRecorderPlugin{}}, req,
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, respBody)
		})

	if reqRow.BytesUp != int64(len(reqBody)) || reqRow.BytesDown != 0 {
		t.Errorf("request row = ↑%d ↓%d, want ↑%d ↓0", reqRow.BytesUp, reqRow.BytesDown, len(reqBody))
	}
	if respRow.BytesDown != int64(len(respBody)) || respRow.BytesUp != 0 {
		t.Errorf("response row = ↑%d ↓%d, want ↑0 ↓%d", respRow.BytesUp, respRow.BytesDown, len(respBody))
	}
}

// TestBytes_PassthroughSSECountsTheFramingToo covers streamPassthrough, the arm an
// event stream takes when no plugin is a StreamingResponder — relayed
// byte-for-byte, never buffered, and reached by no parity fixture, every one of
// which puts a streaming responder in the chain.
//
// The figure is the WIRE length, framing included, which is the whole reason the
// count is taken off the upstream body rather than inside each arm: this arm has
// no frames to add up, and the arm that does sees them with `data: ` and the
// blank-line separators already stripped. See listener/internal/bodycount.
func TestBytes_PassthroughSSECountsTheFramingToo(t *testing.T) {
	const wire = "data: {\"type\":\"message_start\"}\n\ndata: [DONE]\n\n"

	req, _ := http.NewRequest("GET", "http://placeholder/events", nil)

	_, respRow := bytesOf(t, []pipeline.Plugin{}, req,
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, wire)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		})

	if respRow.BytesDown != int64(len(wire)) {
		t.Errorf("response row ↓%d, want ↓%d (the framing counts: %q)", respRow.BytesDown, len(wire), wire)
	}
}

// TestBytes_UnbufferedResponseReportsTheFullCount pins the arm this file used
// to excuse: a plain response that no plugin asked to buffer and that is not an
// event stream is relayed by an io.Copy at the bottom of serveOutbound, which
// runs AFTER the row used to be recorded — so this arm reported zero. The row is
// now recorded below that copy, and the count the wrapper finishes during it is
// the figure the row carries.
//
// Still counted at the wire, not guessed from Content-Length, which is -1 on
// anything chunked and deleted outright on every streaming arm. reverseproxy's
// unbuffered relay defers its row to the end of its copy the same way
// (recordOnEOFBody).
func TestBytes_UnbufferedResponseReportsTheFullCount(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://placeholder/plain", nil)

	_, respRow := bytesOf(t, []pipeline.Plugin{}, req,
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"never":"buffered"}`)
		})

	if respRow.BytesDown != int64(len(`{"never":"buffered"}`)) {
		t.Errorf("response row ↓%d, want ↓%d — the row is recorded after the copy, so the count is complete when the figure is read",
			respRow.BytesDown, len(`{"never":"buffered"}`))
	}
}

// TestStream_CountsArePublishedWhileTheResponseIsOpen pins the forward proxy's
// half of the live tok/s estimate (#1861): the store holds the open inference
// response's byte counter while the body streams, so the Timeline can read it,
// and the entry is gone once the relay ends.
func TestStream_CountsArePublishedWhileTheResponseIsOpen(t *testing.T) {
	const first = "data: {\"type\":\"message_start\"}\n\n"
	const second = "data: {\"type\":\"message_stop\"}\n\n"

	more := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(more) }) }

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, first)
		w.(http.Flusher).Flush()
		<-more
		_, _ = io.WriteString(w, second)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(backend.Close)
	// backend.Close waits on the handler, which waits on more. Cleanups run in
	// reverse, so release runs before backend.Close, and a failed assertion does
	// not hang the test.
	t.Cleanup(release)

	p, err := pipeline.New([]pipeline.Plugin{inferenceTag{}})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	srv := &Server{OutboundPipeline: pipeline.NewHolder(p), Sessions: store, Client: http.DefaultClient}
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(proxy.Close)

	// A timeout, so a proxy that buffers the response again fails the read below
	// instead of blocking until the package's test timeout.
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy.URL))}}
	resp, err := client.Get(backend.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	// Both lines of the first frame: the whole block reached the client, so the
	// relay has read it and the count already includes it.
	for i := 0; i < 2; i++ {
		if _, err := br.ReadString('\n'); err != nil {
			t.Fatalf("first frame, line %d: %v", i+1, err)
		}
	}

	id := requestIDOf(t, store)
	if n, ok := store.StreamBytes(id); !ok || n < int64(len(first)) {
		t.Fatalf("StreamBytes(%q) = (%d, %v) while the response streams, want at least the %d bytes sent",
			id, n, ok, len(first))
	}

	release()
	if _, err := io.Copy(io.Discard, br); err != nil {
		t.Fatalf("drain: %v", err)
	}
	waitForResponseEvent(t, store)
	// The untrack runs as the serving goroutine returns, just after the row.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := store.StreamBytes(id); !ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("StreamBytes(%q) still answers after the relay ended", id)
		}
		time.Sleep(time.Millisecond)
	}
}

// requestIDOf waits for the request row and returns its ID.
func requestIDOf(t *testing.T, store *session.Store) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if v := store.View(session.DefaultSessionID); v != nil && len(v.Events) > 0 && v.Events[0].RequestID != "" {
			return v.Events[0].RequestID
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no request event recorded")
	return ""
}
